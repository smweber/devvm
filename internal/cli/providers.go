package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
	"github.com/smweber/devvm/internal/provider/hetzner"
)

// The provider source (docs/proposals/providers.md §2, §5). A provider
// server has no entry: it is discovered from the provider's API, the last
// of resolve's ordered sources (HUB/NAME → entry → local smol → each
// configured provider), and listed by `status` from a per-provider cache
// that only a live round writes. It lives here rather than among
// backend.Discover's sources because it needs what only the cli has: the
// file's `[providers.*]` tables, this host's token, and the cache.
//
// Every API call is in this file: Get in providerLookup (resolve, only
// after every earlier source missed, and never in providerCached mode: the
// daemon, __session, completion, config rm), List in listProvider (the live round
// of a one-shot `status` or a watcher's refresh), and List in the token
// command's validation. Nothing on the snapshot path dials: a --watch
// snapshot reads the cache file and nothing else.

// providerTimes bounds an API call made for resolve. The listing in a round
// shares the round's deadline (reachTimes) instead, so a one-shot `status`
// stays bounded by one deadline. A var so tests run with short values.
var providerTimes = struct{ get time.Duration }{get: 15 * time.Second}

// providerTitle is a provider's name as its console calls it, for messages.
func providerTitle(name string) string {
	if name == config.BackendHetzner {
		return "Hetzner"
	}
	return name
}

// providerToken resolves this host's token for a provider: the env var,
// else the state-dir file (hetzner.Token, which warns about a loose mode).
func (a *App) providerToken(name string) (string, error) {
	switch name {
	case config.BackendHetzner:
		return hetzner.Token(a.Paths)
	}
	return "", fmt.Errorf("unknown provider %q", name)
}

// errNoToken reports whether err is a provider's "no token here".
func errNoToken(err error) bool { return errors.Is(err, hetzner.ErrNoToken) }

// providerAPI is the client for a provider and a token. Tests replace it
// (App.providerAPI) with the fake, so every path that would dial is counted.
func (a *App) providerAPIFor(name, token string) (provider.Provider, error) {
	if a.providerAPI != nil {
		return a.providerAPI(name, token)
	}
	switch name {
	case config.BackendHetzner:
		return hetzner.New(token), nil
	}
	return nil, fmt.Errorf("unknown provider %q", name)
}

// providerClient is the client for a configured provider, or the no-token
// error (errNoToken) when this host holds none.
func (a *App) providerClient(name string) (provider.Provider, error) {
	tok, err := a.providerToken(name)
	if err != nil {
		return nil, err
	}
	return a.providerAPIFor(name, tok)
}

// providerRecord turns an API row into the record every verb uses
// (config.NewProviderMachine): Managed is the label, settled here by the
// source that produced the row (config.md §9 item 3).
func providerRecord(f *config.File, providerName string, s provider.Server) *config.Machine {
	return config.NewProviderMachine(s.Name, providerName, f.DefaultUser(), provider.Managed(s), config.ProviderServer{
		ID: s.ID, Status: s.Status, State: s.State, Address: s.Address, Type: s.Type, Location: s.Location,
	})
}

// providerMode is how resolve's provider source may answer.
type providerMode int

const (
	// providerLive: the API (one Get per provider), its answer merged into
	// the cache. The default: a human typed the command.
	providerLive providerMode = iota
	// providerCached: the last listing only, never the API and never the
	// token. For what runs unattended or must stay instant: the forward
	// daemon (providers.md §8: it never calls the API), the hub-side
	// __session, completion, and config rm's "does the name still
	// resolve" check.
	providerCached
)

// cachedProviders switches resolve's provider source to the cache until the
// returned func restores it (defer it).
func (a *App) cachedProviders() func() {
	old := a.providerMode
	a.providerMode = providerCached
	return func() { a.providerMode = old }
}

// providerLookup is resolve's last source: each configured provider in
// name order, first match wins. It runs only after the entry and smol
// sources missed, so a name either of them knows never dials the API.
//
// Live (the default), it is one Get per provider with a token. A provider
// with no token is skipped and named in the returned hint for the caller's
// "unknown machine" error (so the hint shows only when nothing matched). An
// API error stops the search: a provider that cannot answer must not let
// the name read as unknown. Each answer is merged into the cache (a found
// row replaced, a vanished one dropped), so the daemon spawned right after
// finds what this command found.
//
// Cached, it reads the last listing and nothing else; a miss with a
// provider configured is an error naming the refresh, since the server may
// exist and the cache (derived data) may just be gone.
func (a *App) providerLookup(f *config.File, name string, mode providerMode) (*config.Machine, string, error) {
	if mode == providerCached {
		for _, pn := range f.ProviderNames() {
			for _, s := range readProviderCache(a.Paths, pn).servers {
				if s.Name == name {
					return providerRecord(f, pn, s), "", nil
				}
			}
		}
		if f.HasProviders() {
			return nil, "", fmt.Errorf("no cached %s server %s; run 'devvm status' to list the provider", strings.Join(f.ProviderNames(), "/"), name)
		}
		return nil, "", nil
	}
	var hints []string
	for _, pn := range f.ProviderNames() {
		cl, err := a.providerClient(pn)
		if errNoToken(err) {
			hints = append(hints, fmt.Sprintf("%s servers were not searched: no token (run 'devvm providers token %s')", providerTitle(pn), pn))
			continue
		}
		if err != nil {
			return nil, "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), providerTimes.get)
		s, err := cl.Get(ctx, name)
		cancel()
		if err != nil {
			return nil, "", fmt.Errorf("%s: looking up %q: %w", pn, name, err)
		}
		_ = a.mergeProviderRow(pn, name, s) // derived data; a failed merge is not a failed lookup
		if s != nil {
			return providerRecord(f, pn, *s), "", nil
		}
	}
	return nil, strings.Join(hints, "; "), nil
}

// mergeProviderRow puts one Get's answer into the cache: s replaces the
// row named name (appended if new), nil drops it. fetched, attempted and
// error are the last listing's and are left alone: one row is not a listing.
func (a *App) mergeProviderRow(providerName, name string, s *provider.Server) error {
	unlock, err := lockProviderCache(a.Paths, providerName)
	if err != nil {
		return err
	}
	defer unlock()
	c := readProviderCache(a.Paths, providerName)
	kept := c.servers[:0]
	found := false
	for _, o := range c.servers {
		switch {
		case o.Name != name:
			kept = append(kept, o)
		case s != nil && !found:
			kept, found = append(kept, *s), true
		}
	}
	if s != nil && !found {
		kept = append(kept, *s)
	}
	if s == nil && len(kept) == len(c.servers) {
		return nil // nothing to drop: leave the file as it is
	}
	c.servers = kept
	return writeProviderCache(a.Paths, providerName, c)
}

// lockProviderCache serialises the cache's two read-modify-write writers
// (a round's listing and a lookup's merge) across processes, so a merge
// never resurrects rows a round just replaced or loses one it just added.
func lockProviderCache(p config.Paths, name string) (func(), error) {
	if err := os.MkdirAll(p.CacheDir(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(p.CacheDir(), "provider-"+name+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil // closing drops the lock
}

// providerUnmanagedErr is the one refusal for a provider server without the
// devvm=true label (providers.md §2, §8): no verb but status, shell and
// exec touches one, and no flag promotes it.
func providerUnmanagedErr(m *config.Machine) error {
	return fmt.Errorf("'%s' is a %s server without the %s=%s label: devvm never promotes a provider server; set the %s=%s label in the %s console",
		m.Name, providerTitle(m.Backend), provider.LabelManaged, provider.LabelManagedValue,
		provider.LabelManaged, provider.LabelManagedValue, providerTitle(m.Backend))
}

// providerLifecycleErr refuses the lifecycle verbs on a provider server
// until they are built (providers.md §3, implementation step 3).
func providerLifecycleErr(m *config.Machine, verb string) error {
	return fmt.Errorf("'%s' is a %s server: %s is not supported yet (providers step 3)", m.Name, providerTitle(m.Backend), verb)
}

// localOwner names the earlier source that holds name on this host, ""
// when none does: a `[machines.NAME]` entry or a local smol VM, which
// shadow a provider server of the same name for every command
// (providers.md §2). `config check` warns with it, and step 3's `create
// --backend hetzner` refuses with it. A file that does not parse is the
// caller's to refuse first.
//
// smols is one local listing the caller took (smolSnapshot), so a check
// over many names costs one `smolvm machine ls`, not one per name.
func localOwner(f *config.File, smols smolMachines, name string) string {
	if _, ok := f.Entry(name); ok {
		return fmt.Sprintf("the entry [machines.%s] in %s", name, f.Path)
	}
	if _, ok := smols.byName[name]; ok {
		return fmt.Sprintf("the local smol VM '%s'", name)
	}
	return ""
}

// The cache. cache/provider-NAME.list is the provider's last listing, read
// by every status snapshot and by the daemon's lookup; only a live round
// writes it. Hand-readable, like the hub and reach caches: key=value header
// lines, then one tab-separated row per server.
//
//	# devvm: the last hetzner listing (derived data; safe to delete)
//	fetched=1760710000
//	attempted=1760710300
//	error=listing failed: hetzner: GET /servers: 503 ...
//	# name	id	state	status	address	type	location	labels
//	web	4711	running	running	1.2.3.4	cx23	nbg1	devvm=true,devvm-template=default
//
// fetched is when the rows were listed (Unix seconds; absent: never),
// attempted when the last round tried, error why that one failed (absent
// when it succeeded). A failed round keeps the rows and their fetched time,
// so the snapshot shows the last known state and how old it is.

// providerCache is one provider's cache file.
type providerCache struct {
	servers   []provider.Server
	fetched   time.Time
	attempted time.Time
	err       string
}

// The error kinds a round records: no token, or the listing failing.
const (
	providerErrNoToken = "no token"
	providerErrFailed  = "listing failed"
)

func providerCachePath(p config.Paths, name string) string {
	return filepath.Join(p.CacheDir(), "provider-"+name+".list")
}

func readProviderCache(p config.Paths, name string) providerCache {
	data, err := os.ReadFile(providerCachePath(p, name))
	if err != nil {
		return providerCache{}
	}
	return parseProviderCache(string(data))
}

func parseProviderCache(text string) providerCache {
	var c providerCache
	unix := func(v string) time.Time {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return time.Time{}
		}
		return time.Unix(n, 0)
	}
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "\t") {
			f := strings.Split(line, "\t")
			if len(f) < 7 || config.ValidName(f[0]) != nil {
				continue
			}
			id, _ := strconv.ParseInt(f[1], 10, 64)
			s := provider.Server{ID: id, Name: f[0], State: f[2], Status: f[3], Address: f[4], Type: f[5], Location: f[6], Labels: map[string]string{}}
			if s.State == "" {
				s.State = provider.StateUnknown
			}
			if len(f) > 7 {
				for _, kv := range strings.Split(f[7], ",") {
					if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
						s.Labels[k] = v
					}
				}
			}
			c.servers = append(c.servers, s)
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "fetched":
			c.fetched = unix(v)
		case "attempted":
			c.attempted = unix(v)
		case "error":
			c.err = v
		}
	}
	return c
}

// cacheField keeps a value on its one line and in its one column.
func cacheField(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "-"
	}
	return s
}

func (c providerCache) text(name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# devvm: the last %s listing (derived data; safe to delete)\n", name)
	if !c.fetched.IsZero() {
		fmt.Fprintf(&b, "fetched=%d\n", c.fetched.Unix())
	}
	if !c.attempted.IsZero() {
		fmt.Fprintf(&b, "attempted=%d\n", c.attempted.Unix())
	}
	if c.err != "" {
		fmt.Fprintf(&b, "error=%s\n", strings.Join(strings.Fields(c.err), " "))
	}
	b.WriteString("# name\tid\tstate\tstatus\taddress\ttype\tlocation\tlabels\n")
	for _, s := range c.servers {
		keys := make([]string, 0, len(s.Labels))
		for k := range s.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		labels := make([]string, len(keys))
		for i, k := range keys {
			labels[i] = k + "=" + s.Labels[k]
		}
		fmt.Fprintf(&b, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.ID, cacheField(s.State), cacheField(s.Status),
			cacheField(s.Address), cacheField(s.Type), cacheField(s.Location), strings.Join(labels, ","))
	}
	return b.String()
}

// writeProviderCache replaces the cache atomically (temp + rename, as the
// hub cache): a snapshot reading it mid-write sees one listing or the
// other. It is under cache/, which `status --watch` does not observe, so a
// round's write never wakes the watcher that ran it.
func writeProviderCache(p config.Paths, name string, c providerCache) error {
	dir := p.CacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".provider-"+name+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.WriteString(c.text(name))
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), providerCachePath(p, name))
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return werr
}

// listProvider is one provider's part of a live round: List under the
// round's deadline, then the cache. A failure (no token, the API down, the
// deadline) keeps the cached rows and records why, so the snapshot shows
// the last known state with its age and `status -v` can say what went wrong.
func (a *App) listProvider(ctx context.Context, name string) {
	cl, err := a.providerClient(name)
	var servers []provider.Server
	if err == nil {
		servers, err = cl.List(ctx)
	}
	now := time.Now()
	// Read under the lock, after the (slow) List, so a lookup's merge in
	// between is not lost.
	unlock, lerr := lockProviderCache(a.Paths, name)
	if lerr != nil {
		return
	}
	defer unlock()
	c := readProviderCache(a.Paths, name)
	switch {
	case err == nil:
		c = providerCache{servers: servers, fetched: now, attempted: now}
	case errNoToken(err):
		c.attempted, c.err = now, providerErrNoToken
	default:
		c.attempted, c.err = now, providerErrFailed+": "+err.Error()
	}
	_ = writeProviderCache(a.Paths, name, c) // derived data; a failed write is not a failed status
}

// providerRound lists every configured provider in parallel under one
// deadline (reachTimes.deadline, the probe round's and so the hub
// listing's), for a one-shot `status` and a watcher's refresh.
func (a *App) providerRound(ctx context.Context) {
	f, err := config.LoadFile(a.Paths)
	if err != nil || !f.HasProviders() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, reachTimes.deadline)
	defer cancel()
	var wg sync.WaitGroup
	for _, pn := range f.ProviderNames() {
		wg.Add(1)
		go func() { defer wg.Done(); a.listProvider(ctx, pn) }()
	}
	wg.Wait()
}

// liveRound is the §5 refresh round: probe every remote entry and list
// every provider, in parallel, each bounded by the same deadline.
// Provider servers are not probed here: their state is the API row's, and
// their last contact (the reach cache) is what the commands that talked to
// them recorded.
func (a *App) liveRound(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.reachRound(ctx, a.remoteConfs()) }()
	go func() { defer wg.Done(); a.providerRound(ctx) }()
	wg.Wait()
}

// providerSourceLine is `status -v`'s word on a provider's listing:
// "hetzner: listed 12s ago (3 servers)", "hetzner: no token (devvm
// providers token hetzner)", "hetzner: listing failed, cached 3m ago (…)".
func providerSourceLine(name string, c providerCache, now time.Time) string {
	cached := "nothing cached"
	if !c.fetched.IsZero() {
		cached = "cached " + sinceAt(c.fetched, now) + " ago"
	}
	switch {
	case c.err == providerErrNoToken:
		return fmt.Sprintf("%s: no token (devvm providers token %s), %s", name, name, cached)
	case strings.HasPrefix(c.err, providerErrFailed):
		why := strings.TrimPrefix(strings.TrimPrefix(c.err, providerErrFailed), ": ")
		line := fmt.Sprintf("%s: %s, %s", name, providerErrFailed, cached)
		if why != "" {
			line += " (" + why + ")"
		}
		return line
	case c.fetched.IsZero():
		return fmt.Sprintf("%s: never listed (a one-shot 'devvm status' lists it)", name)
	default:
		return fmt.Sprintf("%s: listed %s ago (%d servers)", name, sinceAt(c.fetched, now), len(c.servers))
	}
}

// sinceAt is a compact age: 12s, 3m, 5h, 2d.
func sinceAt(t, now time.Time) string {
	d := max(now.Sub(t), 0).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// noteNoToken is the one line a one-shot `status` prints for a configured
// provider this host has no token for (providers.md §1: nothing without a
// TTY ever prompts; it names the command instead). stderr, so a --plain
// consumer's stdout is untouched.
func (a *App) noteNoToken() {
	f, err := config.LoadFile(a.Paths)
	if err != nil || a.Stderr == nil {
		return
	}
	for _, pn := range f.ProviderNames() {
		if readProviderCache(a.Paths, pn).err == providerErrNoToken {
			fmt.Fprintf(a.Stderr, "devvm: %s: no API token on this host, so its servers are from the last listing; run 'devvm providers token %s'\n", pn, pn)
		}
	}
}

// providerCachedNames is the server names in each configured provider's
// last listing, provider by name: status' shadow marking, config check's
// collision warning and completion read it (hints, never a live call).
func (a *App) providerCachedNames(f *config.File) map[string][]string {
	out := map[string][]string{}
	for _, pn := range f.ProviderNames() {
		for _, s := range readProviderCache(a.Paths, pn).servers {
			out[pn] = append(out[pn], s.Name)
		}
	}
	return out
}
