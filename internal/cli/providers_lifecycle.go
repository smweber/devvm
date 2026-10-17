package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/charmbracelet/huh"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
	"github.com/smweber/devvm/internal/provider/hetzner"
	"github.com/smweber/devvm/internal/reach"
	"github.com/smweber/devvm/internal/session"
)

// The lifecycle verbs on a provider box (providers.md §3, §8): create,
// delete, start and stop. Each is the provider's API call plus exactly the
// existing machinery around it: create ends in setup's own shape (reached
// over ssh like a managed remote entry, root adoption included), delete in
// the host-side cleanup smol's delete uses, start/stop in smol's forward
// handling. Every API call here is a human's command; nothing in the
// daemon, __session, completion or a status snapshot reaches this file.
// Each call's answer is merged into the provider cache before the change
// marker is touched (the deferred TouchChanged in the verb), so the menu
// shows the new row within the debounce window without a listing.

// lifecycleClient is the provider's client for a lifecycle verb: this
// host's token, or the first-use prompt on a terminal (askProviderToken).
// Without either, the no-token error names 'devvm providers token'.
func (a *App) lifecycleClient(pn string) (provider.Provider, error) {
	cl, err := a.providerClient(pn)
	if errNoToken(err) {
		cl, err = a.askProviderToken(pn, err)
	}
	return cl, err
}

// confirmYes is askConfirm for a verb with --yes: no terminal says so.
func confirmYes(q string) (bool, error) {
	ok, err := askConfirm(q)
	if err != nil {
		return false, fmt.Errorf("%w (pass --yes to skip the confirmation)", err)
	}
	return ok, nil
}

// interruptContext is the context a long, interruptible step (create's
// setup, start's wait) runs under: cancelled on SIGINT/SIGTERM, so the ssh
// children it runs are killed (exec.CommandContext) and the command can
// say what it left behind instead of dying silently. A var so a test can
// hand in a cancelled one.
var interruptContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// askConfirm is the y/N prompt the lifecycle verbs use (confirm, on the
// controlling terminal); a var so a test can answer it. It is a package
// global, so the tests that stub it (and pickBackend, openTTY) must not
// use t.Parallel.
var askConfirm = confirm

// runCreateProvider is `create NAME --backend PROVIDER` (§3): refuse a
// name this host or the project already holds, settle the template and the
// placement, make sure the key is in the project, say what will bill, then
// Create and hand the server to setup unchanged. Everything that can be
// refused is refused before Create: after it, the server exists and bills,
// and every failure names it and the two ways forward instead of deleting
// it (a half-shaped box may be worth inspecting; deleting it is the user's
// call).
func (a *App) runCreateProvider(s createSpec, f *config.File) error {
	pn := s.Backend
	if err := config.ValidServerName(s.Name); err != nil {
		return err
	}
	spec, ok := f.Providers[pn]
	if !ok {
		return fmt.Errorf("create --backend %s needs a [providers.%s] table in %s (location, image, and ssh_key or ssh_key_file); add it with 'devvm config edit'",
			pn, pn, f.Path)
	}
	// An entry or a local smol VM would shadow the new server for every
	// command (§2), so the name is refused while either holds it.
	if owner := localOwner(f, smolSnapshot(), s.Name); owner != "" {
		return fmt.Errorf("'%s' is already %s on this host, which would shadow a %s server of that name: pick another name",
			s.Name, owner, providerTitle(pn))
	}
	if s.Memory != 0 || s.Disk != 0 {
		return fmt.Errorf("--memory and --disk size a smol VM; a %s server's size is its server type (--server-type, or server_type in the template)", providerTitle(pn))
	}
	t, pl, err := a.gatherProviderSpec(&s, spec)
	if err != nil {
		return err
	}
	// The record every later command builds logs in as [templates.default]'s
	// user (devvm stores nothing per server, so it cannot remember another
	// one): a template that would make a different user would lock them out,
	// as checkTemplateUser refuses for a remote entry.
	if login := f.DefaultUser(); t.User != login {
		return fmt.Errorf("template %s sets user = %s, but a %s server is always reached as [templates.default]'s user (%s): devvm keeps no per-server record; use a template with user = %q, or change [templates.default]",
			t.Name, t.User, providerTitle(pn), login, login)
	}
	pubkey, err := providerKeyFile(pn, pl)
	if err != nil {
		return err
	}
	cl, err := a.lifecycleClient(pn)
	if err != nil {
		return err
	}
	// "create is not idempotent: a second create NAME finds the server
	// and refuses" (§3), managed or not: the name is the project's.
	ctx, cancel := context.WithTimeout(context.Background(), providerTimes.get)
	existing, err := cl.Get(ctx, s.Name)
	cancel()
	if err != nil {
		return fmt.Errorf("%s: looking up %q: %w", pn, s.Name, err)
	}
	_ = a.mergeProviderRow(pn, s.Name, existing) // derived data
	if existing != nil {
		next := "pick another name"
		if provider.Managed(*existing) {
			next = fmt.Sprintf("'devvm setup %s' re-runs setup on it, 'devvm delete %s' removes it", s.Name, s.Name)
		}
		return fmt.Errorf("a %s server named '%s' already exists (server %d, %s, %s): %s",
			providerTitle(pn), s.Name, existing.ID, existing.State, orDash(existing.Address), next)
	}
	key, err := a.ensureProviderKey(cl, pn, pl, pubkey)
	if err != nil {
		return err
	}

	// What bills, before it does (§8).
	nets := "IPv4 + IPv6"
	if !pl.IPv4 {
		nets = "IPv6 only"
	}
	fmt.Fprintf(a.Stdout, "Creating %s server '%s': %s in %s, image %s, %s, ssh key %s.\n",
		providerTitle(pn), s.Name, pl.ServerType, pl.Location, pl.Image, nets, key)
	fmt.Fprintf(a.Stdout, "It bills from now until 'devvm delete %s' (a stopped server still bills).\n", s.Name)

	ctx, cancel = context.WithTimeout(context.Background(), providerTimes.create)
	srv, err := cl.Create(ctx, provider.Spec{
		Name: s.Name, Type: pl.ServerType, Image: pl.Image, Location: pl.Location, SSHKey: key,
		IPv6Only: !pl.IPv4,
		Labels:   map[string]string{provider.LabelManaged: provider.LabelManagedValue, provider.LabelTemplate: t.Name},
	})
	cancel()
	if srv != nil {
		// It exists and bills from here on: in the cache at once, so
		// `status` and the menu show it whatever happens next.
		_ = a.mergeProviderRow(pn, srv.Name, srv)
	}
	next := fmt.Sprintf("run: devvm setup %s or devvm delete %s", s.Name, s.Name)
	if err != nil {
		if srv == nil {
			return createNoServerErr(pn, s.Name, err)
		}
		return fmt.Errorf("%s server '%s' was created (server %d, %s) and bills, but %v; %s",
			providerTitle(pn), srv.Name, srv.ID, orDash(srv.Address), err, next)
	}
	if srv.Address == "" {
		return fmt.Errorf("%s server '%s' was created (server %d) and bills, but has no public address yet; %s",
			providerTitle(pn), srv.Name, srv.ID, next)
	}
	// The address first, so the user can watch (or ssh in) while setup waits.
	fmt.Fprintf(a.Stdout, "Created '%s' (server %d) at %s; setting it up.\n", srv.Name, srv.ID, srv.Address)

	m := providerRecord(f, pn, *srv)
	if !m.Managed() {
		return fmt.Errorf("%s server '%s' came back without the %s=%s label, so devvm will not shape it; %s",
			providerTitle(pn), srv.Name, provider.LabelManaged, provider.LabelManagedValue, next)
	}
	b, err := providerBackendFor(m, a.Paths)
	if err != nil {
		return fmt.Errorf("%s server '%s' was created (server %d, %s) and bills, but %v; %s",
			providerTitle(pn), srv.Name, srv.ID, srv.Address, err, next)
	}
	// Hetzner reuses addresses: a pin left for this one (a server deleted
	// in the console, or on another host) is stale by construction, and
	// would fail setup's first ssh with a changed host key. One a
	// `[machines.*]` entry at the same address still uses is kept
	// (forgetHostKey), so a mismatch then names that entry.
	pinKey := knownHostsKey(m)
	holder := a.pinSharedBy(m, pinKey)
	if err := a.forgetHostKey(m); err != nil {
		fmt.Fprintf(a.Stderr, "devvm: %s: %v\n", s.Name, err)
	}
	// setup's own path from the user step on (runSetup's gates are for a
	// name it resolved; this record is the one just created): reachForSetup
	// waits for ssh and creates the user as root, shape does the rest.
	o := setupOpts{resolved: &t, fresh: true, tmplFlags: tmplFlags{Keys: s.Keys, NoPorts: !s.savesPorts()}}
	sctx, stop := interruptContext()
	defer stop()
	if err := a.shape(sctx, m, b, t, o); err != nil {
		if sctx.Err() != nil {
			return fmt.Errorf("'%s' was created (server %d at %s; it bills) but setup was interrupted; %s",
				s.Name, srv.ID, srv.Address, next)
		}
		if holder != "" && holder != "?" && hostKeyMismatch(err) {
			next = fmt.Sprintf("the pinned host key for %s is kept for the entry [machines.%s], which reaches the same address; if that pin is stale, clear it with 'ssh-keygen -R %s -f %s', then %s",
				pinKey, holder, pinKey, a.Paths.KnownHostsPath(), next)
		}
		return fmt.Errorf("'%s' was created (server %d at %s; it bills) but setup failed: %w; %s",
			s.Name, srv.ID, srv.Address, err, next)
	}
	a.printCreateNext(s.Name, pn)
	return nil
}

// createNoServerErr is a Create that returned no server. Only an
// API-level refusal (an HTTP error answer: a conflict, a bad type, a
// missing key) means nothing was made; a transport error, a timeout or a
// cancelled context may have come after the POST was accepted, so the
// server may exist and bill, and the user is told how to find out.
func createNoServerErr(pn, name string, err error) error {
	var apiErr *hetzner.APIError
	if errors.As(err, &apiErr) || errors.Is(err, provider.ErrConflict) || errors.Is(err, provider.ErrNotFound) {
		return fmt.Errorf("%s: creating '%s': %w (the API refused it; no server was created)", pn, name, err)
	}
	return fmt.Errorf("%s: creating '%s': %w; '%s' may have been created and would bill: run 'devvm status' to list the project, then 'devvm delete %s' if it exists",
		pn, name, err, name, name)
}

// hostKeyMismatch reports whether setup failed on a pinned host key that
// no longer matches (probeSSH's fatal marks).
func hostKeyMismatch(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "REMOTE HOST IDENTIFICATION HAS CHANGED") || strings.Contains(msg, "Host key verification failed")
}

// providerBackendFor is backend.For for the record create just built, a
// var so a test can fail it after Create.
var providerBackendFor = backend.For

func orDash(s string) string {
	if s == "" {
		return "no address"
	}
	return s
}

// gatherProviderSpec settles a provider create's template and placement:
// the template (flags folded in) as for smol, the placement as template >
// [providers.NAME] > built-in (Template.Placement) under the --server-type,
// --location and --image flags. On a terminal without --yes it prompts for
// the placement a flag did not give, seeded with the resolved value, then
// the hook, repos and ports as smol's form does.
func (a *App) gatherProviderSpec(s *createSpec, spec config.ProviderSpec) (config.Template, config.Placement, error) {
	tty := openTTY()
	if tty != nil {
		defer tty.Close()
	}
	interactive := tty != nil && !s.Yes
	t, err := a.resolveTemplate(s.Template, s.tmplFlags)
	if err != nil {
		return config.Template{}, config.Placement{}, err
	}
	pl := t.Placement(spec)
	for _, f := range []struct {
		key, flag string
		dst       *string
	}{{"server type", s.ServerType, &pl.ServerType}, {"location", s.Location, &pl.Location}, {"image", s.Image, &pl.Image}} {
		if f.flag == "" {
			continue
		}
		if err := config.ValidPlacement(f.key, f.flag); err != nil {
			return config.Template{}, config.Placement{}, err
		}
		*f.dst = f.flag
	}
	if interactive {
		if err := askPlacement(tty, s, &pl); err != nil {
			return config.Template{}, config.Placement{}, err
		}
	}
	if err := resolveHook(tty, interactive, s, t); err != nil {
		return config.Template{}, config.Placement{}, err
	}
	s.User = t.User
	s.Repos, s.Ports = t.Repos, t.Ports
	s.KeysGithub, s.KeyFiles = t.AuthorizedKeysGithub, t.AuthorizedKeys
	s.Harden, s.Fail2ban = t.Harden, t.Fail2ban
	if interactive {
		if err := askExtras(tty, s); err != nil {
			return config.Template{}, config.Placement{}, err
		}
	}
	out := s.template()
	out.ServerType, out.Location, out.Image = pl.ServerType, pl.Location, pl.Image
	ipv4 := pl.IPv4
	out.IPv4 = &ipv4
	return out, pl, nil
}

// askPlacement prompts for the placement fields no flag set.
func askPlacement(tty *os.File, s *createSpec, pl *config.Placement) error {
	var fields []huh.Field
	add := func(title, key, flag string, v *string) {
		if flag != "" {
			return
		}
		fields = append(fields, huh.NewInput().Title(title).Value(v).
			Validate(func(v string) error { return config.ValidPlacement(key, strings.TrimSpace(v)) }))
	}
	add("Server type", "server type", s.ServerType, &pl.ServerType)
	add("Location", "location", s.Location, &pl.Location)
	add("Image", "image", s.Image, &pl.Image)
	if len(fields) == 0 {
		return nil
	}
	if err := form(tty, fields...); err != nil {
		return err
	}
	pl.ServerType, pl.Location, pl.Image = strings.TrimSpace(pl.ServerType), strings.TrimSpace(pl.Location), strings.TrimSpace(pl.Image)
	for _, kv := range [][2]string{{"server type", pl.ServerType}, {"location", pl.Location}, {"image", pl.Image}} {
		if kv[1] == "" {
			return fmt.Errorf("a %s is required", kv[0])
		}
	}
	return nil
}

// askBackend is create's first question on a terminal when the file
// configures a provider and no --backend was given: smol or a provider.
// Without a provider table there is nothing to choose, so a plain `create`
// is exactly what it was.
func (a *App) askBackend(s *createSpec) error {
	if s.Backend != "" || s.Yes {
		return nil
	}
	f, err := config.LoadFile(a.Paths)
	if err != nil || !f.HasProviders() {
		return nil // runCreate reports a broken file itself
	}
	tty := openTTY()
	if tty == nil {
		return nil
	}
	defer tty.Close()
	choice, err := pickBackend(tty, f.ProviderNames())
	if err != nil {
		return err
	}
	s.Backend = choice
	return nil
}

// pickBackend is askBackend's select (smol first and pre-selected), a var
// so a test can answer it.
var pickBackend = func(tty *os.File, providers []string) (string, error) {
	choice := config.BackendSmol
	opts := []huh.Option[string]{huh.NewOption("smol: a local microVM", config.BackendSmol)}
	for _, pn := range providers {
		opts = append(opts, huh.NewOption(pn+": a "+providerTitle(pn)+" server (bills while it exists)", pn))
	}
	err := form(tty, huh.NewSelect[string]().Title("Backend").Options(opts...).Value(&choice))
	return choice, err
}

// providerKeyFile reads and checks ssh_key_file when the provider table
// names one, before any API call: a missing file, a private key or a line
// that is not a public key fails here, not after a server bills. With
// ssh_key (a key already in the project) it returns "". With neither, a
// server would come up with no key this host can log in with (Hetzner
// mails a root password instead), so that is refused too.
func providerKeyFile(pn string, pl config.Placement) (string, error) {
	switch {
	case pl.SSHKey != "":
		return "", nil
	case pl.SSHKeyFile == "":
		return "", fmt.Errorf("[providers.%s] names no ssh key: set ssh_key (a key already in the %s project) or ssh_key_file (a public key, uploaded once), so the new server lets this host in",
			pn, providerTitle(pn))
	}
	path := config.ExpandHome(pl.SSHKeyFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("[providers.%s] ssh_key_file: %w", pn, err)
	}
	text := string(data)
	if strings.Contains(text, "PRIVATE KEY") {
		return "", fmt.Errorf("[providers.%s] ssh_key_file %s is a private key; name the .pub file", pn, pl.SSHKeyFile)
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 && (strings.HasPrefix(f[0], "ssh-") || strings.HasPrefix(f[0], "ecdsa-") || strings.HasPrefix(f[0], "sk-")) {
			if _, err := base64.StdEncoding.DecodeString(f[1]); err == nil {
				return line, nil
			}
		}
		break
	}
	return "", fmt.Errorf("[providers.%s] ssh_key_file %s is not an ssh public key", pn, pl.SSHKeyFile)
}

// providerKeyName is the name an ssh_key_file key is uploaded under:
// derived from the key itself, so every host with the same file finds the
// same project key and two different keys never fight over a name (a
// comment like me@laptop is not unique across machines).
func providerKeyName(pubkey string) string {
	f := strings.Fields(pubkey)
	blob, _ := base64.StdEncoding.DecodeString(f[1])
	sum := sha256.Sum256(blob)
	return "devvm-" + hex.EncodeToString(sum[:])[:12]
}

// ensureProviderKey makes sure the key Create names is in the project
// and returns its name there: ssh_key must already exist (FindSSHKey);
// ssh_key_file's key is uploaded once under providerKeyName, or found
// under another name with the same key material.
func (a *App) ensureProviderKey(cl provider.Provider, pn string, pl config.Placement, pubkey string) (string, error) {
	ke, ok := cl.(provider.KeyEnsurer)
	if !ok {
		return "", fmt.Errorf("%s: this client cannot manage ssh keys", pn)
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerTimes.get)
	defer cancel()
	if pl.SSHKey != "" {
		k, err := ke.FindSSHKey(ctx, pl.SSHKey)
		if err != nil {
			return "", fmt.Errorf("%s: looking up ssh key %q: %w", pn, pl.SSHKey, err)
		}
		if k == nil {
			return "", fmt.Errorf("[providers.%s] ssh_key = %q: no key of that name in the %s project; upload it in the console, or set ssh_key_file instead", pn, pl.SSHKey, providerTitle(pn))
		}
		return k.Name, nil
	}
	k, err := ke.EnsureSSHKey(ctx, providerKeyName(pubkey), pubkey)
	if err != nil {
		return "", fmt.Errorf("%s: ssh key from %s: %w", pn, pl.SSHKeyFile, err)
	}
	return k.Name, nil
}

// deleteProviderBox is `delete NAME` on a managed provider server (§3):
// confirm with what is about to go (unless --yes), stop its forward
// daemon, DELETE (waited), then the host-side cleanup smol's delete does
// (forgetHostState: saved forwards, reach cache, and the pinned host key,
// since Hetzner reuses addresses) and the cache row. A server already gone
// is cleaned up locally all the same.
func (a *App) deleteProviderBox(m *config.Machine, yes bool) error {
	srv, pn := m.Server, m.Backend
	if !yes {
		ok, err := confirmYes(fmt.Sprintf("Delete %s server '%s' (%s in %s, %s, server %d) and its disk (irreversible)?",
			providerTitle(pn), m.Name, srv.Type, srv.Location, orDash(srv.Address), srv.ID))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("aborted")
		}
	}
	cl, err := a.lifecycleClient(pn)
	if err != nil {
		return err
	}
	a.reapRunFiles(m.Name) // its ssh master goes before the server does
	ctx, cancel := context.WithTimeout(context.Background(), providerTimes.action)
	err = cl.Delete(ctx, srv.ID)
	cancel()
	gone := errors.Is(err, provider.ErrNotFound)
	if err != nil && !gone {
		return fmt.Errorf("%s: deleting '%s' (server %d): %w", pn, m.Name, srv.ID, err)
	}
	// The server is gone from here on: the cache row goes and the delete
	// is reported whatever the host-side cleanup says, so a failure there
	// (returned last) never reads as the server still existing.
	_ = a.mergeProviderRow(pn, m.Name, nil) // derived data
	if cerr := a.forgetHostState(m.Name, m); cerr != nil {
		fmt.Fprintf(a.Stdout, "devvm: deleted %s server '%s', but clearing this host's state for it failed\n", providerTitle(pn), m.Name)
		return cerr
	}
	session.RemoveLock(a.Paths, m.Name)
	if gone {
		fmt.Fprintf(a.Stdout, "devvm: %s server '%s' was already gone; cleared this host's state for it\n", providerTitle(pn), m.Name)
		return nil
	}
	fmt.Fprintf(a.Stdout, "devvm: deleted %s server '%s'\n", providerTitle(pn), m.Name)
	return nil
}

// powerProviderBox is `start`/`stop` on a provider server (§3, §8). The
// state is the API row resolve just fetched (its Get is the "Get first"):
// a server already in the asked state is a no-op that says so, rather than
// Hetzner's action error. An unmanaged server is someone's in the project,
// so the power change confirms first (--yes skips). stop mirrors smol's:
// the forward daemon is stopped before the power goes, and start brings
// the saved forwards back up. stop warns every time, on stderr: a stopped
// server still bills.
func (a *App) powerProviderBox(m *config.Machine, on, yes bool) error {
	srv, pn := m.Server, m.Backend
	verb, want := "stop", provider.StateStopped
	if on {
		verb, want = "start", provider.StateRunning
	}
	billing := func() {
		if on {
			return
		}
		// An off box answers nothing: the last contact would read as a
		// live "reachable, verified 22s ago" until the next probe.
		_ = reach.Remove(a.Paths, m.Name)
		if m.Managed() {
			fmt.Fprintf(a.Stderr, "devvm: %s is stopped but still billed; devvm delete %s frees it\n", m.Name, m.Name)
		} else {
			// delete refuses a server devvm did not make: the console is the way.
			fmt.Fprintf(a.Stderr, "devvm: %s is stopped but still billed; delete it in the %s console to stop billing\n", m.Name, providerTitle(pn))
		}
	}
	if srv.State == want {
		fmt.Fprintf(a.Stdout, "devvm: '%s' is already %s\n", m.Name, want)
		billing()
		if on {
			return a.resumeProviderForwards(m)
		}
		return nil
	}
	if !m.Managed() && !yes {
		ok, err := confirmYes(fmt.Sprintf("'%s' is a %s server devvm did not make (no %s=%s label; %s, %s): really %s it?",
			m.Name, providerTitle(pn), provider.LabelManaged, provider.LabelManagedValue, srv.State, orDash(srv.Address), verb))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("aborted")
		}
	}
	cl, err := a.lifecycleClient(pn)
	if err != nil {
		return err
	}
	if !on {
		// As smol's stop: reap the forwards first, so none is left
		// dialling a server that is going away. start brings them back.
		if dc, derr := session.Existing(a.Paths, m.Name); derr == nil {
			_ = dc.Stop()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerTimes.action)
	err = cl.Power(ctx, srv.ID, on)
	cancel()
	if err != nil {
		return fmt.Errorf("%s: %s '%s' (server %d): %w", pn, verb, m.Name, srv.ID, err)
	}
	a.setProviderState(pn, m.Name, on)
	fmt.Fprintf(a.Stdout, "devvm: '%s' is %s\n", m.Name, want)
	billing()
	if on {
		return a.resumeProviderForwards(m)
	}
	return nil
}

// resumeProviderForwards is start's forwards step for a provider box, as
// for smol. The API says running as soon as the server boots, well before
// sshd listens, and a daemon spawned then exits on its first dial; so it
// first waits for ssh as setup does (waitSSH: bounded, one "waiting" line,
// only "unreachable" retried, no root adoption: a managed box has its
// user), which costs one probe when sshd is up already. Then the saved
// forwards, from the cache: resolve's Get, merged, is the newest answer
// there is, and the daemon reads only the cache too.
func (a *App) resumeProviderForwards(m *config.Machine) error {
	if !m.Managed() {
		return nil // no forwards on a server devvm did not make, and no known user
	}
	ctx, stop := interruptContext()
	defer stop()
	switch r, err := a.waitSSH(ctx, m); {
	case err != nil:
		return fmt.Errorf("'%s' is running, but %w; 'devvm ports up %s' brings its forwards up once it answers", m.Name, err, m.Name)
	case r == reachDenied:
		return fmt.Errorf("'%s' is running, but ssh to %s was refused (permission denied)", m.Name, m.SSHHost)
	}
	a.noteContact(m, "start", config.TransportSSH, nil)
	saved, err := a.savedPorts(m.Name)
	if err != nil || len(saved) == 0 {
		return err
	}
	defer a.cachedProviders()()
	return a.tunnelUpWait(m.Name, runningPollTimeout)
}

// setProviderState records a finished power action in the cache row
// (Power waited for it, so the state is known without another call).
func (a *App) setProviderState(pn, name string, on bool) {
	unlock, err := lockProviderCache(a.Paths, pn)
	if err != nil {
		return
	}
	defer unlock()
	c := readProviderCache(a.Paths, pn)
	for i := range c.servers {
		if c.servers[i].Name != name {
			continue
		}
		if on {
			c.servers[i].State, c.servers[i].Status = provider.StateRunning, "running"
		} else {
			c.servers[i].State, c.servers[i].Status = provider.StateStopped, "off"
		}
		_ = writeProviderCache(a.Paths, pn, c) // derived data
		return
	}
}
