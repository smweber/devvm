package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
	"github.com/smweber/devvm/internal/reach"
	"github.com/smweber/devvm/internal/session"
)

// statusGroups is the display order for `status`: one section per backend.
var statusGroups = []struct{ backend, title string }{
	{config.BackendSmol, "smol"},
	{config.BackendRemote, "remote"},
	{config.BackendHetzner, "hetzner"}, // provider servers: their own section, from the provider's cached listing
	{config.BackendHub, "hub"},         // every hub's own row followed by its machines, whatever backend the hub reports for them
}

// smolLifecycle is the state track shown in the verbose view; the live state
// is highlighted. A smol VM either exists or it doesn't (no dormant state,
// config.md §2); a remote box's state is its reach (a line of its own under
// the row), so it gets no track.
var smolLifecycle = []string{"running", "stopped"}

// The --plain fifth column (config.md §7): who owns the machine.
const (
	ownManaged   = "managed"   // devvm made it (smol: the marker matches) or shapes it (remote, managed = true)
	ownUnmanaged = "unmanaged" // a smol VM devvm did not create, or an adopted remote host
	ownShadowed  = "shadowed"  // a registry entry hiding a local smol VM, or an entry or smol VM hiding a provider server, of the same name
	ownNone      = "-"         // hubs; a hub machine whose hub did not say; a row nothing settled
)

func ownership(managed bool) string {
	if managed {
		return ownManaged
	}
	return ownUnmanaged
}

// statusRow is one machine's resolved status for rendering.
type statusRow struct {
	name        string
	backend     string
	state       string // running | stopped | reachable | unreachable | ? (an older hub may send others)
	hub         string // the hub this row belongs to (its own row included); "" for a local machine
	exists      bool
	running     bool
	mem         int // MiB (smol: smolvm's own record)
	disk        int // GiB (smol: smolvm's own record)
	cpus        int
	host        string
	fwds        fwdSummary
	saved       bool         // this host has saved forwards for it (<State>/ports/NAME): --plain's `down`
	managed     string       // the --plain fifth column: managed | unmanaged | shadowed | -
	note        string       // a remark for the human table, e.g. "this host"
	reach       *reach.Entry // a remote box's cached reach (--plain's sixth column); nil for every other row
	hubVerified string       // a hub machine's sixth column as its hub sent it (`verified=…`); "" when it sent none
	shadows     []string     // what this row hides, for the human note ("a local smol VM", "a hetzner server")
	m           *config.Machine
}

// fwdSummary is what status knows about a machine's forward daemon.
type fwdSummary struct {
	daemon bool      // a daemon answered
	state  string    // session.StateUp | session.StateReconnecting
	n      int       // forwards it owns (live or pending), any owner
	conf   int       // of those, the ones a conf owner holds: --plain's N
	since  time.Time // when state began
}

// runStatusAll lists every machine (registry ∪ live smol ∪ the hubs'
// listings), grouped by backend. verbose adds a lifecycle track, live smol
// resource sizes, and per-machine forward detail. local stops at this host.
func (a *App) runStatusAll(verbose, local bool) error {
	rows, err := a.gatherRows(statusScope{local: local, probe: !local})
	if bc, ok := asBrokenConf(err); ok {
		fmt.Fprintf(a.Stderr, "devvm: %v\n", bc.err)
		fmt.Fprintf(a.Stdout, "broken conf: %v\n", bc.err)
		return nil
	}
	if err != nil {
		return err
	}
	if !local {
		// The live round refreshed the reach and provider caches a running
		// `status --watch` reads, and its debounce re-resolves a symlinked
		// devvm.toml (kqueue may have dropped the relink): wake it. The menu
		// bar app runs this on every menu open, so that is the user's refresh.
		config.TouchChanged(a.Paths)
	}
	if !local {
		a.noteNoToken()
	}
	sources := a.providerSourceLines(local)
	if len(rows) == 0 {
		fmt.Fprintln(a.Stdout, "No machines. Create one with 'devvm create'.")
		if verbose {
			for _, l := range sources {
				fmt.Fprintln(a.Stdout, l)
			}
		}
		return nil
	}

	first := true
	for _, g := range statusGroups {
		var group []statusRow
		for _, r := range rows {
			if groupOf(r) == g.backend {
				group = append(group, r)
			}
		}
		src, isProvider := sources[g.backend]
		if len(group) == 0 && !(verbose && isProvider) {
			continue
		}
		if !first {
			fmt.Fprintln(a.Stdout)
		}
		first = false
		fmt.Fprintln(a.Stdout, g.title)
		if verbose && isProvider {
			// The listing's age and health, once per section: the rows
			// are its, however old (providers.md §5).
			fmt.Fprintf(a.Stdout, "  %s\n", src)
		}
		if len(group) == 0 {
			continue
		}
		if g.backend == config.BackendSmol {
			a.renderSmolGroup(group, verbose)
		} else {
			a.renderRemoteGroup(group, verbose)
		}
	}
	return nil
}

// providerSourceLines is each configured provider's `status -v` line
// (providerSourceLine), keyed by provider; none with --local, which skips
// providers. Read from the caches only.
func (a *App) providerSourceLines(local bool) map[string]string {
	out := map[string]string{}
	if local {
		return out
	}
	f, err := config.LoadFile(a.Paths)
	if err != nil {
		return out
	}
	now := time.Now()
	for _, pn := range f.ProviderNames() {
		out[pn] = providerSourceLine(pn, readProviderCache(a.Paths, pn), now)
	}
	return out
}

// runStatusPlain emits one tab-separated row per machine — name, backend,
// state, forward state, ownership and last contact — with no headers or grouping, so
// scripts can enumerate machines (e.g. every running smol VM) without
// scraping the human-formatted table. The token sets are the format's
// contract:
//
//	state:    running | stopped | reachable | unreachable | ?
//	forwards: up:N | reconnecting:N | down | -
//	managed:  managed | unmanaged | shadowed | -
//	verified: verified=<unix>:<how> | verified=- | -
//
// The fifth column was appended (config.md §7), so a consumer reading four
// fields still works. `unmanaged` is a smol VM devvm did not create (it
// supports status/start/stop/shell/exec only; `setup --adopt` hands it
// over) or an adopted remote host; `shadowed` is a entry hiding a
// local smol VM of the same name (the entry wins for every command); `-` is
// a hub, or a machine on a hub too old to emit the column: never
// `unmanaged`, which would be a claim nobody made. `dormant` is gone (a
// smol VM exists or it doesn't), though an older hub may still send it.
//
// N is the number of configured (`conf`-owned) forwards the daemon holds; a
// forward only a session or a ttl holds is not counted (browser-bridge.md
// §8: a login's callback would otherwise flicker the menubar badge), and
// N is never 0: with no conf-owned forward the column reads as if no daemon
// answered. `down` means ports are configured but none is up (the normal
// state of a stopped VM, so a UI should only flag it on a running machine);
// `-` means nothing is configured.
// A remote box (backend `remote`, managed or not) is `reachable` when a
// live ControlMaster answered or its last probe connected, `unreachable`
// when its last probe failed, `?` when it was never probed or cannot be (an
// alias behind ProxyJump/ProxyCommand): its reach cache, which this
// listing refreshes with a live round first (providers.md §5). The sixth
// column, appended like the fifth, is when and how that was last
// established: `verified=<unix seconds>:<how>`, how ∈ {master, probe, or
// the command that talked to the box: attach, shell, exec, setup}, or
// `verified=-` for a remote box with nothing established; a hub machine
// carries its hub's sixth column through (a newer hub's `verified=…`), and
// every other row has `-`. `?` otherwise means the backend could not be
// asked. A devvm.toml that does not parse renders no machine rows (an
// entry shadows a smol VM, and a broken file cannot say which) but the one
// row brokenConfRow, `-` everywhere but state `broken conf`, with the parse
// error on stderr and a zero exit: the menu bar app shows the state rather
// than a failed listing. A hub's broken row never becomes a HUB/NAME row
// (`-` is no valid name, so parseHubRows drops it). Consumers should treat any other token as "unknown", not fail.
// `--watch` re-emits this listing on devvm-made changes (see watchStatus); the
// format is the same, so a consumer parses one thing.
//
// Hubs add no column: a hub's own row is `HUB<TAB>hub<TAB>reachable|unreachable`,
// and a machine on it is `HUB/NAME` with the backend and state the hub
// reported (an older hub's `remote-managed`/`remote-unmanaged` read as
// `remote` plus the managed column; a hub's remote box that is also an
// entry here is not repeated, since the direct path is the right one) (`unreachable` for every row of a hub that did not answer, from
// the cache) and this host's own forwards column. A consumer that wants to
// group by hub splits column 1 on `/`. --local lists this host only.
//
// A provider server (backend `hetzner`) is a row from the provider's last
// listing (cache/provider-NAME.list, refreshed by the live round first):
// state running | stopped | starting | stopping | ? mapped from the
// provider's own status, the fifth column managed (the devvm=true label) or
// unmanaged, the sixth its last contact as for a remote box. A provider
// that cannot be listed (no token, an API error) keeps its cached rows with
// their last state; it never reads `unreachable`, which would say the box
// is down. A server whose name an entry or a local smol VM holds has no row
// (the other row reads `shadowed`). --local lists no provider.
func (a *App) runStatusPlain(local bool) error {
	rows, err := a.gatherRows(statusScope{local: local, probe: !local})
	if bc, ok := asBrokenConf(err); ok {
		fmt.Fprintf(a.Stderr, "devvm: %v\n", bc.err)
		fmt.Fprint(a.Stdout, brokenConfRow)
		return nil
	}
	if err != nil {
		return err
	}
	if !local {
		// The live round refreshed the reach and provider caches a running
		// `status --watch` reads, and its debounce re-resolves a symlinked
		// devvm.toml (kqueue may have dropped the relink): wake it. The menu
		// bar app runs this on every menu open, so that is the user's refresh.
		config.TouchChanged(a.Paths)
	}
	if !local {
		a.noteNoToken()
	}
	for _, r := range rows {
		fmt.Fprint(a.Stdout, plainLine(r))
	}
	return nil
}

// brokenConfRow is the whole --plain listing while devvm.toml does not
// parse (see runStatusPlain).
const brokenConfRow = "-\t-\tbroken conf\t-\t-\t-\n"

// brokenConfError is gatherRows' error for a devvm.toml that does not
// parse: the one failure status renders (brokenConfRow) instead of
// failing.
type brokenConfError struct{ err error }

func (e brokenConfError) Error() string { return e.err.Error() }
func (e brokenConfError) Unwrap() error { return e.err }

func asBrokenConf(err error) (brokenConfError, bool) {
	var bc brokenConfError
	ok := errors.As(err, &bc)
	return bc, ok
}

// groupOf is the status section a row renders in: a hub and every machine
// on it share the hub section, whatever backend the hub reports for them.
func groupOf(r statusRow) string {
	if r.hub != "" {
		return config.BackendHub
	}
	return r.backend
}

// plainLine is one --plain row, shared by the one-shot listing and --watch.
func plainLine(r statusRow) string {
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\n", r.name, r.backend, r.state, plainForwards(r), plainManaged(r), plainVerified(r))
}

// plainVerified renders the sixth column (see runStatusPlain).
func plainVerified(r statusRow) string {
	switch {
	case r.reach != nil:
		return r.reach.Plain()
	case r.hubVerified != "":
		return r.hubVerified
	}
	return "-"
}

// plainManaged renders the fifth column (see runStatusPlain).
func plainManaged(r statusRow) string {
	if r.managed == "" {
		return ownNone
	}
	return r.managed
}

// plainForwards renders the machine-readable forward column (see runStatusPlain).
func plainForwards(r statusRow) string {
	switch {
	case r.fwds.daemon && r.fwds.conf > 0:
		return fmt.Sprintf("%s:%d", r.fwds.state, r.fwds.conf)
	case r.saved:
		return "down"
	default:
		return "-"
	}
}

func (a *App) renderSmolGroup(rows []statusRow, verbose bool) {
	fmt.Fprintf(a.Stdout, "  %-16s %-12s %-8s %-8s %s\n", "NAME", "STATE", "MEM", "DISK", "FWDS")
	for _, r := range rows {
		fmt.Fprintf(a.Stdout, "  %-16s %-12s %-8s %-8s %s%s\n",
			r.name, r.state, memHuman(r.mem), diskHuman(r.disk), fwdsCount(r.fwds), rowNote(r))
		if verbose {
			a.renderVerboseDetail(r)
		}
	}
}

func (a *App) renderRemoteGroup(rows []statusRow, verbose bool) {
	// HOST is sized to the widest value so a long user@address does not push
	// FWDS out of line.
	hostW := 22
	for _, r := range rows {
		if len(r.host) > hostW {
			hostW = len(r.host)
		}
	}
	// providers.md §3: with -v a stopped server's state reads
	// "stopped (billing)", since it still bills (never in --plain).
	state := func(r statusRow) string {
		if verbose && r.m != nil && r.m.IsProvider() && r.state == provider.StateStopped {
			return r.state + " (billing)"
		}
		return r.state
	}
	stateW := 12
	for _, r := range rows {
		stateW = max(stateW, len(state(r)))
	}
	fmt.Fprintf(a.Stdout, "  %-16s %-*s %-*s %s\n", "NAME", stateW, "STATE", hostW, "HOST", "FWDS")
	for _, r := range rows {
		fmt.Fprintf(a.Stdout, "  %-16s %-*s %-*s %s%s\n", r.name, stateW, state(r), hostW, r.host, fwdsCount(r.fwds), rowNote(r))
		if verbose {
			a.renderVerboseDetail(r)
		}
	}
}

// rowNote is the human table's trailing remark: a VM devvm did not create
// (with what hands it over), an adopted remote box, an entry shadowing a
// VM, the hub entry naming this host.
func rowNote(r statusRow) string {
	switch {
	case r.note != "":
		return "  (" + r.note + ")"
	case r.hub == "" && r.managed == ownUnmanaged && r.backend == config.BackendSmol:
		return "  (unmanaged: " + backend.AdoptHint(r.name) + ")"
	case r.hub == "" && r.managed == ownUnmanaged && r.m != nil && r.m.IsProvider():
		return "  (unmanaged: no devvm=true label; status, shell and exec only)"
	case r.hub == "" && r.managed == ownUnmanaged:
		return "  (adopted: devvm never modifies it)"
	case r.managed == ownShadowed:
		what := strings.Join(r.shadows, " and ")
		if what == "" {
			what = "a local smol VM"
		}
		return "  (shadows " + what + " of the same name)"
	}
	return ""
}

// renderVerboseDetail prints the lifecycle track and sizes (local smol) and
// each live forward under a machine's row.
func (a *App) renderVerboseDetail(r statusRow) {
	if r.m != nil && r.m.IsProvider() {
		s := r.m.Server
		fmt.Fprintf(a.Stdout, "    server: %s in %s, %s (id %d, %s status %s)\n",
			dash(s.Type), dash(s.Location), dash(s.Address), s.ID, r.backend, dash(s.Status))
		if r.reach != nil {
			// Not probed: the state is the API's; this is the last time a
			// command (attach, shell, exec, setup) got through.
			fmt.Fprintf(a.Stdout, "    contact: %s\n", providerContact(*r.reach))
		}
	} else if r.reach != nil {
		fmt.Fprintf(a.Stdout, "    reach: %s\n", r.reach.Describe(time.Now()))
	}
	if r.backend == config.BackendSmol && r.exists && r.hub == "" {
		fmt.Fprintf(a.Stdout, "    lifecycle: %s\n", lifecycleTrack(smolLifecycle, r.state))
		if r.cpus > 0 {
			fmt.Fprintf(a.Stdout, "    resources: %d CPUs\n", r.cpus)
		}
	}
	if r.hub != "" && r.name != r.hub {
		// The HOST column carries the hub; the backend the hub reported for
		// the machine has nowhere else to show in the human table. A row the
		// hub did not name (this host's saved forwards or daemon only) has none.
		if r.backend == config.BackendHub {
			fmt.Fprintf(a.Stdout, "    backend: unknown (hub %s does not list it)\n", r.hub)
		} else {
			fmt.Fprintf(a.Stdout, "    backend: %s (as reported by hub %s)\n", r.backend, r.hub)
		}
	}
	if cl, err := session.Existing(a.Paths, r.name); err == nil {
		if st, err := cl.Status(); err == nil && len(st.Forwards) > 0 {
			if st.Reconnecting() {
				fmt.Fprintf(a.Stdout, "    forwards: reconnecting (since %s)\n", sinceHuman(st.Since))
			} else {
				fmt.Fprintln(a.Stdout, "    forwards:")
			}
			for _, f := range st.Forwards {
				fmt.Fprintf(a.Stdout, "      localhost:%d -> %s:%d%s\n", f.Host, r.name, f.Guest, forwardSuffix(f))
			}
		}
	}
}

// statusScope says how far a listing reaches. local stops at this host: no
// hub is read, nothing is dialed (hub.md §5: it is what a hub runs for
// *its* callers, so hubs never fan out). Otherwise every hub entry
// contributes its rows — fetched now when listings is nil, or, under
// --watch, the latest block from each held pipe.
//
// probe runs a live round first (liveRound: the remote boxes probed, every
// provider listed; the one-shot listing: a human typed it and wants fresh),
// in parallel with the hubs' listings so they all cost one deadline. Without it, as on every --watch
// snapshot, a remote row is its reach cache and nothing is dialled.
type statusScope struct {
	local    bool
	probe    bool
	hubs     []*config.Machine // the hub entries, when the caller already loaded them (--watch); nil loads them here
	listings hubListings
}

// gatherRows resolves every `[machines.*]` entry plus every local smol VM
// into a statusRow, then (unless local) each hub's block: its own row and a
// row per machine on it (hubRows). A devvm.toml that does not parse is the
// error, and no rows: see runStatusPlain.
//
// smolvm is listed once per call and every smol row is derived from that one
// listing (rather than the backend's per-machine Status probe): a snapshot is
// re-taken on every --watch event, and N+1 `smolvm machine ls` subprocesses
// per change would be the polling this command exists to avoid. The same
// listing settles each VM's ownership (the smol source's rule,
// backend.SmolRecord) and carries its sizes.
func (a *App) gatherRows(sc statusScope) ([]statusRow, error) {
	f, err := config.LoadFile(a.Paths)
	if err != nil {
		return nil, brokenConfError{err}
	}
	if sc.probe {
		probed := make(chan struct{})
		go func() { defer close(probed); a.liveRound(context.Background()) }()
		if !sc.local && sc.listings == nil {
			if sc.hubs == nil {
				sc.hubs = a.hubConfs()
			}
			sc.listings = a.fetchHubListings(context.Background(), sc.hubs)
		}
		<-probed
	}
	var rows []statusRow
	smols := smolSnapshot()
	for _, m := range f.Machines() {
		if m.IsHub() {
			continue // its row comes with its listing
		}
		rows = append(rows, a.rowFor(m, smols))
	}
	// Local smol VMs with no entry: discovered (config.md §2). One devvm
	// made reads managed and can hold forwards; any other reads unmanaged,
	// and a stray ports file for it reads `-`, since nothing may bring its
	// forwards up. One shadowed by an entry has no row of its own.
	for _, sm := range smols.list {
		if _, ok := f.Entries[sm.Name]; ok {
			continue
		}
		managed := backend.SmolRecord(a.Paths, sm).Managed()
		r := smolRow(statusRow{name: sm.Name, backend: config.BackendSmol, managed: ownership(managed)}, sm)
		r.fwds = a.forwardSummary(sm.Name)
		if managed {
			r.saved = a.hasSavedPorts(sm.Name)
		}
		rows = append(rows, r)
	}
	if sc.local {
		return rows, nil
	}
	rows = a.providerRows(f, smols, rows)
	hubs := sc.hubs
	if hubs == nil {
		hubs = a.hubConfs()
	}
	listings := sc.listings
	if listings == nil {
		listings = a.fetchHubListings(context.Background(), hubs)
	}
	for _, hub := range hubs {
		hr := a.hubRows(hub, listings[hub.Name], f.Entries)
		smols.shadow(&hr[0]) // the hub's own row: an entry like any other
		if isThisHost(hub.Name) {
			// Cosmetic only (config.md §1): the shared file names this host
			// as a hub; nothing behaves differently for it.
			hr[0].note = "this host"
		}
		rows = append(rows, hr...)
	}
	markShadowsProviders(a.providerCachedNames(f), rows)
	return rows, nil
}

// providerRows appends a row per server in each configured provider's
// cached listing (never a live call: gatherRows' probe is what refreshes
// the cache first). A server whose name an entry or a local smol VM holds
// gets no row: that one wins for every command (providers.md §2), and its
// own row says `shadowed` (markShadowsProviders).
func (a *App) providerRows(f *config.File, smols smolMachines, rows []statusRow) []statusRow {
	for _, pn := range f.ProviderNames() {
		for _, s := range readProviderCache(a.Paths, pn).servers {
			if _, ok := f.Entries[s.Name]; ok {
				continue
			}
			if _, ok := smols.byName[s.Name]; ok {
				continue
			}
			m := providerRecord(f, pn, s)
			r := statusRow{name: s.Name, backend: pn, state: s.State, host: dash(s.Address), m: m,
				managed: ownership(m.Managed()), exists: true, running: s.State == provider.StateRunning}
			if r.state == "" {
				r.state = provider.StateUnknown
			}
			e := reach.Read(a.Paths, s.Name)
			r.reach = &e
			r.fwds = a.forwardSummary(s.Name)
			if m.Managed() {
				r.saved = a.hasSavedPorts(s.Name)
			}
			rows = append(rows, r)
		}
	}
	return rows
}

// markShadowsProviders marks every local row (an entry, a hub's own row, a
// smol VM) whose name a provider's cached listing also has: that server is
// hidden behind it, which the fifth column says rather than hides.
func markShadowsProviders(cached map[string][]string, rows []statusRow) {
	if len(cached) == 0 {
		return
	}
	for i := range rows {
		r := &rows[i]
		if strings.Contains(r.name, "/") || (r.m != nil && r.m.IsProvider()) {
			continue
		}
		for _, pn := range sortedNames(cached) {
			if slices.Contains(cached[pn], r.name) {
				r.managed = ownShadowed
				r.shadows = append(r.shadows, "a "+pn+" server")
			}
		}
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// hubRows renders one hub: its own row, then a row per machine on it. In
// the human table every row of the hub shows the hub's ssh host in HOST and
// no backend column (the section is the hub's, whatever backend the hub
// reports for a machine); `-v` prints the reported backend under the row.
// The
// machine set is the union of what the hub listed (fresh, or cached when it
// did not answer) and what this host knows on its own — saved forwards
// (ports/HUB@NAME), a live run/HUB@NAME.sock — so a laptop-side forward for a machine
// the hub no longer lists still shows, with state `?` (the hub answered and
// did not name it; `delete HUB/NAME` clears it) and backend `hub` (nothing
// reported one). State and backend otherwise come from the hub's row, the
// forwards column from this host's own daemon for HUB@NAME, never the
// hub's. With no fresh answer every row of the hub is `unreachable`,
// whatever the cache remembers: a cached `running` would be a claim nobody
// is making.
//
// local is this host's own entries: a hub's row for a remote box this host
// also has an entry for is hidden (config.md §7: the direct path is the
// right one, and the hub is not a jump host), unless this host has forwards
// for the HUB/NAME form. nil hides nothing.
func (a *App) hubRows(hub *config.Machine, l hubListing, local map[string]config.Entry) []statusRow {
	state := "reachable"
	if !l.fresh {
		state = "unreachable"
	}
	rows := []statusRow{{name: hub.Name, backend: config.BackendHub, hub: hub.Name, state: state, host: hub.SSHHost, m: hub, managed: ownNone}}
	byName := map[string]hubRow{}
	names := map[string]bool{}
	for _, r := range l.rows {
		// By name, and only a remote row against a remote entry: the
		// listing carries no host to compare, and a hub entry of that name
		// here is another thing entirely.
		if e, dup := local[r.name]; dup && e.Backend == config.BackendRemote && r.backend == config.BackendRemote {
			display := config.JoinHubName(hub.Name, r.name)
			if !a.hasSavedPorts(display) && !a.forwardSummary(display).daemon {
				continue
			}
		}
		byName[r.name], names[r.name] = r, true
	}
	for _, display := range append(savedHubPorts(a.Paths), liveHubSockets(a.Paths)...) {
		if h, n, _, _ := config.SplitHubName(display); h == hub.Name {
			names[n] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		display := config.JoinHubName(hub.Name, n)
		r := statusRow{
			name: display, hub: hub.Name, host: hub.SSHHost,
			m:     &config.Machine{Name: display, Hub: hub, Backend: config.BackendHub},
			saved: a.hasSavedPorts(display),
		}
		r.managed = ownNone
		if hr, ok := byName[n]; ok {
			// The hub's own word on ownership (its marker), passed through;
			// a hub too old for the column said nothing, which is `-`.
			r.backend, r.state, r.managed = hr.backend, hr.state, hr.managed
			// A newer hub's sixth column, passed through as the hub said
			// it (its remote boxes' last contact, from the hub's cache).
			if v, _, _ := strings.Cut(hr.rest, "\t"); strings.HasPrefix(v, "verified=") {
				r.hubVerified = v
			}
			// "dormant" only from a hub older than the dormant state's removal.
			r.exists, r.running = hr.state != "dormant", hr.state == "running"
		} else {
			r.backend, r.state = config.BackendHub, "?"
		}
		if !l.fresh {
			r.state = "unreachable"
		}
		r.fwds = a.forwardSummary(display)
		rows = append(rows, r)
	}
	return rows
}

// smolMachines is one `smolvm machine ls` taken for a whole status snapshot.
type smolMachines struct {
	available bool // smolvm is installed; otherwise every smol row is "?"
	list      []backend.SmolMachine
	byName    map[string]backend.SmolMachine
}

func smolSnapshot() smolMachines {
	s := smolMachines{available: backend.SmolAvailable(), byName: map[string]backend.SmolMachine{}}
	if !s.available {
		return s
	}
	s.list, _ = backend.SmolList()
	for _, sm := range s.list {
		s.byName[sm.Name] = sm
	}
	return s
}

// shadow marks a entry's row when a local smol VM has the same
// name (config.md §2): the entry wins for every command and the VM gets no
// row of its own, so the collision is said here rather than hidden.
func (s smolMachines) shadow(r *statusRow) {
	if _, ok := s.byName[r.name]; ok {
		r.managed = ownShadowed
		r.shadows = append(r.shadows, "a local smol VM")
	}
}

// smolRow fills a row's state and sizes from the VM's listing row: sizes are
// smolvm's own (config.md §3), for running and stopped VMs alike.
func smolRow(r statusRow, sm backend.SmolMachine) statusRow {
	r.exists, r.running = true, sm.Running()
	r.state = smolStateLabel(sm)
	r.mem, r.disk, r.cpus = sm.MemoryMiB, sm.StorageGB, sm.CPUs
	return r
}

// hasSavedPorts reports whether this host has saved forwards for name,
// which is what lets --plain say `down` for a stopped VM: the list is on
// the host, so no daemon or guest needs to answer.
func (a *App) hasSavedPorts(name string) bool {
	saved, _ := a.savedPorts(name)
	return len(saved) > 0
}

func (a *App) rowFor(m *config.Machine, smols smolMachines) statusRow {
	r := statusRow{name: m.Name, backend: m.Backend, m: m, host: m.SSHHost, saved: a.hasSavedPorts(m.Name)}
	r.managed = ownership(m.Managed())
	smols.shadow(&r)
	if _, err := backend.For(m, a.Paths); err != nil {
		r.state = "?"
		return r
	}
	// The reach cache, never a live call: this runs on every --watch
	// snapshot (gatherRows' probe is what refreshes it first).
	e := reach.Read(a.Paths, m.Name)
	r.reach = &e
	r.exists, r.running = true, e.State == reach.Reachable
	r.state = e.State
	r.fwds = a.forwardSummary(m.Name)
	return r
}

// smolStateLabel is a listed VM's state token: running, or stopped for
// anything else smolvm reports (a VM it lists exists).
func smolStateLabel(sm backend.SmolMachine) string {
	if sm.Running() {
		return "running"
	}
	return "stopped"
}

// forwardSummary asks a machine's daemon for its state and forward count; a
// zero value means no daemon answered.
func (a *App) forwardSummary(name string) fwdSummary {
	cl, err := session.Existing(a.Paths, name)
	if err != nil {
		return fwdSummary{}
	}
	st, err := cl.Status()
	if err != nil {
		return fwdSummary{}
	}
	state := st.State
	if state == "" {
		state = session.StateUp // a pre-0.1.11 daemon reports no state; it only ever ran up
	}
	return fwdSummary{daemon: true, state: state, n: len(st.Forwards), conf: st.ConfCount(), since: st.Since}
}

// runStatus is the single-machine drill-in: always detailed (state, lifecycle,
// smolvm's sizes and ownership for smol, and live forwards).
func (a *App) runStatus(name string) error {
	m, b, err := a.resolveAny(name) // a hub and a hub machine both have a status
	if err != nil {
		return err
	}
	if m.IsHubMachine() {
		return a.runStatusHubMachine(m)
	}
	if sm, ok := backend.SmolRow(b); ok {
		// The row resolve already listed: state, sizes and ownership with no
		// second `smolvm machine ls`, and no guest exec (a stopped VM has
		// sizes too).
		fmt.Fprintf(a.Stdout, "%s (%s)\n", m.Name, m.Backend)
		fmt.Fprintf(a.Stdout, "  lifecycle: %s\n", lifecycleTrack(smolLifecycle, smolStateLabel(sm)))
		fmt.Fprintf(a.Stdout, "  resources: %d CPUs, %s RAM, %s disk\n", sm.CPUs, memHuman(sm.MemoryMiB), diskHuman(sm.StorageGB))
		if m.Managed() {
			fmt.Fprintln(a.Stdout, "  managed: yes (created by devvm)")
		} else {
			fmt.Fprintf(a.Stdout, "  managed: no (not created by devvm: status, start, stop, shell and exec only; %s)\n", backend.AdoptHint(m.Name))
		}
	} else {
		// A entry: no smolvm call (a shadowed VM shows in the
		// listing's fifth column, not here).
		st, err := b.Status()
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "%s (%s)\n", m.Name, m.Backend)
		fmt.Fprintf(a.Stdout, "  state: %s\n", st.Raw)
		if m.IsProvider() {
			s := m.Server
			fmt.Fprintf(a.Stdout, "  server: %s in %s, %s\n", dash(s.Type), dash(s.Location), dash(s.Address))
			if s.State == provider.StateStopped {
				fmt.Fprintf(a.Stdout, "  billing: stopped but still billed by %s\n", providerTitle(m.Backend))
			}
			fmt.Fprintf(a.Stdout, "  contact: %s\n", providerContact(reach.Read(a.Paths, m.Name)))
		}
		switch {
		case m.IsHub():
		case m.IsProvider() && m.Managed():
			fmt.Fprintln(a.Stdout, "  managed: yes (the devvm=true label: devvm shapes it)")
		case m.IsProvider():
			fmt.Fprintf(a.Stdout, "  managed: no (no devvm=true label: status, shell and exec only; devvm never promotes a provider server)\n")
		case m.Managed():
			fmt.Fprintln(a.Stdout, "  managed: yes (devvm shapes it: setup, prereqs, hardening)")
		default:
			fmt.Fprintln(a.Stdout, "  managed: no (adopted: devvm never modifies it)")
		}
	}
	a.forwardReport(name)
	return nil
}

// runStatusHubMachine is the drill-in for HUB/NAME: one listing from its hub
// (the same call the merged listing makes, same cache, same deadline), its
// row picked out. A hub that does not answer says so on its own line rather
// than dressing a cached token up as the current state.
func (a *App) runStatusHubMachine(m *config.Machine) error {
	l := a.listHub(context.Background(), m.Hub)
	for _, r := range a.hubRows(m.Hub, l, nil) {
		if r.name != m.Name {
			continue
		}
		if r.backend == config.BackendHub { // the hub did not name it, so no backend is known
			fmt.Fprintf(a.Stdout, "%s (on hub %s, %s)\n", m.Name, m.Hub.Name, m.Hub.SSHHost)
		} else {
			fmt.Fprintf(a.Stdout, "%s (%s on hub %s, %s)\n", m.Name, r.backend, m.Hub.Name, m.Hub.SSHHost)
		}
		fmt.Fprintf(a.Stdout, "  state: %s\n", r.state)
		if !l.fresh {
			fmt.Fprintf(a.Stdout, "  hub: %s did not answer within %s; the machine is from its last listing\n", m.Hub.Name, hubTimes.deadline)
		}
		a.forwardReport(m.Name)
		return nil
	}
	if !l.fresh {
		return fmt.Errorf("hub '%s' (%s) did not answer, and its last listing does not name %s", m.Hub.Name, m.Hub.SSHHost, m.HubMachineName())
	}
	return fmt.Errorf("hub '%s' lists no machine %q; run 'devvm status'", m.Hub.Name, m.HubMachineName())
}

// lifecycleTrack renders states joined by arrows with the current one bracketed.
func lifecycleTrack(states []string, current string) string {
	parts := make([]string, len(states))
	for i, s := range states {
		if s == current {
			parts[i] = "[" + s + "]"
		} else {
			parts[i] = s
		}
	}
	return strings.Join(parts, " ─ ")
}

// memHuman renders MiB as GiB when it's a clean multiple, else MiB; "—" at zero.
func memHuman(miB int) string {
	if miB <= 0 {
		return "—"
	}
	if miB%1024 == 0 {
		return fmt.Sprintf("%d GiB", miB/1024)
	}
	return fmt.Sprintf("%d MiB", miB)
}

func diskHuman(giB int) string {
	if giB <= 0 {
		return "—"
	}
	return fmt.Sprintf("%d GiB", giB)
}

// fwdsCount renders a forward count, or "—" when none are up.
// fwdsCount is the FWDS column: a count, flagged while the daemon is between
// transports so a stuck reconnect reads differently from healthy forwards.
func fwdsCount(f fwdSummary) string {
	if f.n == 0 {
		return "—"
	}
	if f.state == session.StateReconnecting {
		return fmt.Sprintf("%d (reconnecting %s)", f.n, sinceHuman(f.since))
	}
	return strconv.Itoa(f.n)
}

// providerContact is a provider box's last-contact text: the reach cache's
// words once a command got through, else "none recorded yet" (it is never
// probed, so "not probed" would read as a gap to fill).
func providerContact(e reach.Entry) string {
	if e.Verified.IsZero() {
		return "none recorded yet"
	}
	return e.Describe(time.Now())
}
