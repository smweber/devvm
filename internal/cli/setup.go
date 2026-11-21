package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/bootstrap"
	"github.com/smweber/devvm/internal/config"
)

// `devvm setup` (config.md §3) is the single place devvm touches a box's
// contents, and only a box devvm made: a smol VM or a provider server (its
// devvm=true label), never an entry. It replaced
// `bootstrap`, whose name also meant the user's own hook script. Its order
// is fixed: ensure user → prereqs → agent + shim (+ profile.d) → keys →
// harden → hook → repos. The user comes first because everything after
// it runs as that user or through its sudo;
// harden comes after keys because it refuses on an empty authorized_keys,
// and both come before the hook and the repos, so neither user code nor
// the network can leave a billing cloud server unhardened.

// tmplFlags are the template fields as flags, shared by `create` and
// `setup`. They are the explicit form the laptop sends to a hub's proxied
// create/setup (config.md §7), so the hub's own file never matters there;
// they also work directly. Each overrides the resolved template field.
type tmplFlags struct {
	Hook   string
	Memory int
	Disk   int
	Repos  []string
	// Keys are authorized_keys lines, already resolved: the laptop turns a
	// template's authorized_keys_github/authorized_keys into lines (as
	// `keys add` inlines key lines), since the files and the network
	// lookup are the laptop's.
	Keys     []string
	Harden   bool
	Fail2ban bool
	// HardenSet/Fail2banSet: the flag was given, either value, so
	// --harden=false overrides a template's harden = true.
	HardenSet   bool
	Fail2banSet bool
	// Inline: these flags are the whole template. The hub's own
	// [templates.*] are not consulted; unset fields take the built-ins.
	Inline bool
}

// Template flag names, defined once: addTemplateFlags registers them and
// the proxy strips them before sending its own.
const (
	flagUser     = "user"
	flagHook     = "hook"
	flagMemory   = "memory"
	flagDisk     = "disk"
	flagRepo     = "repo"
	flagPort     = "port"
	flagKey      = "key"
	flagHarden   = "harden"
	flagFail2ban = "fail2ban"
	flagInline   = "inline-template"
	flagNoPorts  = "no-ports"
	flagTemplate = "template"
)

var templateFlagNames = []string{flagUser, flagHook, flagMemory, flagDisk, flagRepo,
	flagPort, flagKey, flagHarden, flagFail2ban, flagInline, flagNoPorts}

// addTemplateFlags registers the template flags on fs, hidden except the
// names in visible (create shows --memory/--disk/--hook). memory and disk
// keep create's -m/-d shorthands.
func addTemplateFlags(fs *pflag.FlagSet, v *tmplFlags, visible ...string) {
	// --user is gone with the template's user (every box devvm makes has
	// the user dev) but stays accepted, and ignored, until hubs and laptops
	// are all past this release: a laptop one release older still sends it.
	fs.String(flagUser, "", "ignored (a laptop one release older sends it to its hub)")
	fs.StringVar(&v.Hook, flagHook, "", "hook: url:<URL> [args] | cmd:<path> [args] | none (template hook)")
	fs.IntVarP(&v.Memory, flagMemory, "m", 0, "smol: VM memory in MiB (template memory)")
	fs.IntVarP(&v.Disk, flagDisk, "d", 0, "smol: VM disk in GiB (template disk; default 50)")
	fs.StringArrayVar(&v.Repos, flagRepo, nil, "repo to clone into ~/src (repeatable; template repos)")
	// --port and --no-ports are gone ('ports add' saves a forward) but stay
	// accepted, and ignored, until hubs and laptops are all past this
	// release: a laptop still sends --no-ports to its hub, and a hub one
	// release older still needs it (so it saves and asks for no forwards).
	fs.StringArray(flagPort, nil, "ignored: save a forward with 'devvm ports add'")
	fs.Bool(flagNoPorts, false, "ignored (a laptop sends it to its hub)")
	fs.StringArrayVar(&v.Keys, flagKey, nil, "remote: authorized_keys line to add (repeatable)")
	fs.BoolVar(&v.Harden, flagHarden, false, "remote: harden sshd + firewall (template harden)")
	fs.BoolVar(&v.Fail2ban, flagFail2ban, false, "remote: install fail2ban with harden (template fail2ban)")
	fs.BoolVar(&v.Inline, flagInline, false, "the template flags are the whole template (sent by a laptop to its hub)")
	for _, n := range templateFlagNames {
		if !slices.Contains(visible, n) {
			_ = fs.MarkHidden(n)
		}
	}
}

// noteSet records which bool flags were given, from the parsed flag set.
func (v *tmplFlags) noteSet(fs *pflag.FlagSet) {
	v.HardenSet = fs.Changed(flagHarden)
	v.Fail2banSet = fs.Changed(flagFail2ban)
}

// layer is the flags as a template layer: a set flag (non-zero, or a given
// bool) wins over every template.
func (v tmplFlags) layer() config.TemplateSpec {
	var s config.TemplateSpec
	if v.Hook != "" {
		h := v.Hook
		s.Hook = &h
	}
	if v.Memory != 0 {
		m := v.Memory
		s.Memory = &m
	}
	if v.Disk != 0 {
		d := v.Disk
		s.Disk = &d
	}
	if v.Repos != nil {
		r := slices.Clone(v.Repos)
		s.Repos = &r
	}
	if v.Harden || v.HardenSet {
		h := v.Harden
		s.Harden = &h
	}
	if v.Fail2ban || v.Fail2banSet {
		f := v.Fail2ban
		s.Fail2ban = &f
	}
	return s
}

// resolveTemplate settles the effective template for create/setup:
//
//	flag > --template T > [templates.default] > built-in
//
// T falls back to default one level, never further. With Inline the flags
// are the whole template (a laptop resolved it already).
func (a *App) resolveTemplate(name string, f tmplFlags) (config.Template, error) {
	// `--template default` is the same as no flag.
	if name == config.DefaultTemplate {
		name = ""
	}
	label := name
	if label == "" {
		label = config.DefaultTemplate
	}
	flags := f.layer()
	if err := flags.Validate(); err != nil {
		return config.Template{}, err
	}
	if f.Inline {
		t := flags.Resolve(label)
		return t, validateTemplate(t)
	}
	named, def, err := config.TemplateLayers(a.Paths, name)
	if err != nil {
		return config.Template{}, err
	}
	chain := flags
	if name != "" {
		chain = chain.Under(named)
	}
	t := chain.Under(def).Resolve(label)
	return t, validateTemplate(t)
}

// validateTemplate checks what config cannot: the hook spec, whose parser
// lives in bootstrap.
func validateTemplate(t config.Template) error {
	if _, err := bootstrap.ParseSpec(t.Hook); err != nil {
		return fmt.Errorf("template %s: %w", t.Name, err)
	}
	return nil
}

// setupOpts is `setup`'s input.
type setupOpts struct {
	Template string
	tmplFlags

	// resolved is create's already-settled template (flags, prompts and
	// all); setup then resolves nothing itself.
	resolved *config.Template
}

func (a *App) setupCmd() *cobra.Command {
	var o setupOpts
	c := &cobra.Command{
		Use:   "setup NAME",
		Short: "Shape a box: user, prereqs, agent, keys, hardening, hook, repos",
		Long: "Shape a box from a template ([templates.*] in devvm.toml; default without\n" +
			"--template). The one place devvm changes a box's contents, in a fixed order:\n" +
			"user, prereqs (incl. tmux, gh, mosh), agent + BROWSER shim, keys and\n" +
			"hardening (a provider server), the template's hook, repos (cloned into\n" +
			"~/src). Safe to re-run. Forwards are saved with 'devvm ports add', not here.\n\n" +
			"Only a smol VM or a provider server devvm made is shaped; an entry in\n" +
			"devvm.toml (a remote box, a hub) never is. A server that answers only as\n" +
			"root gets its user created first.\n" +
			"Precedence: flag > --template > [templates.default] > built-in.",
		RunE: a.hubOr(func(cmd *cobra.Command, args []string) error {
			o.noteSet(cmd.Flags())
			if err := a.runSetup(args[0], o); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "devvm: '%s' is set up\n", args[0])
			return nil
		}),
	}
	a.machineArg(c)
	f := c.Flags()
	f.StringVarP(&o.Template, flagTemplate, "t", "", "template to shape with (default: default)")
	addTemplateFlags(f, &o.tmplFlags)
	_ = c.RegisterFlagCompletionFunc(flagTemplate, a.completeTemplates)
	return c
}

// completeTemplates offers the [templates.*] names in devvm.toml.
func (a *App) completeTemplates(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	ts, err := config.LoadTemplates(a.Paths)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(ts))
	for n := range ts {
		names = append(names, n)
	}
	slices.Sort(names)
	return names, cobra.ShellCompDirectiveNoFileComp
}

// runSetup gates, resolves the template, and shapes the box.
func (a *App) runSetup(name string, o setupOpts) error {
	defer config.TouchChanged(a.Paths) // setup may start a stopped smol VM
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if m.IsRemote() && !m.IsProvider() {
		// An entry (simplify.md step 12): devvm only connects to it.
		return fmt.Errorf("'%s' is an entry in devvm.toml: devvm never shapes one (it connects: attach, shell, exec, ports, cp, keys)", name)
	}
	var t config.Template
	if o.resolved != nil {
		t = *o.resolved
	} else if t, err = a.resolveTemplate(o.Template, o.tmplFlags); err != nil {
		return err
	}
	ctx, stop := interruptContext()
	defer stop()
	return a.shape(ctx, m, b, t, o)
}

// loginUser is the user part of a remote record's ssh destination ("" for
// a bare host or alias).
func loginUser(m *config.Machine) string {
	u, _, ok := strings.Cut(m.SSHHost, "@")
	if !ok {
		return ""
	}
	return u
}

// browserStep is the agent + shim (+ profile.d) step, a var so a test can
// stand in for agentbin's install.
var browserStep = bootstrap.Browser

// shape runs the fixed order on a box already cleared to be shaped. Its
// one backstop: a box devvm did not make (Managed) is refused before any
// step, since every step below installs, hardens or runs code as root
// there. runSetup and create refuse such a box first.
func (a *App) shape(ctx context.Context, m *config.Machine, b backend.Backend, t config.Template, o setupOpts) error {
	if !m.Managed() {
		return fmt.Errorf("'%s' is not a box devvm made: devvm never shapes it", m.Name)
	}
	a.warnIgnored(m, t, o)
	// 1. The user. A provider server may answer only as root (a fresh cloud
	// VM); reachForSetup waits for it and creates the user there. On smol
	// the exec enters as root and the backend is wired to dev.
	if m.IsRemote() {
		var err error
		if m, b, err = a.reachForSetup(ctx, m); err != nil {
			return err
		}
	} else {
		if _, err := a.startStopped(m, b); err != nil {
			return err
		}
		if err := bootstrap.EnsureUser(ctx, b, backend.ExecOpts{User: "root"}, backend.DefaultUser); err != nil {
			return err
		}
	}
	// 2. devvm's own prereqs, as root.
	if err := bootstrap.Prereqs(ctx, b, m); err != nil {
		return err
	}
	// 3. Agent + BROWSER shim + /etc/profile.d/devvm.sh, smol and remote
	// alike (both daemons run the agent exec). Before the hook, so the
	// hook's own login shells already see BROWSER.
	if err := browserStep(ctx, b, m); err != nil {
		return err
	}
	if m.IsRemote() { // a provider server: an entry never gets here
		// 4. Keys (a source that yields nothing warns and moves on).
		if err := a.seedAuthorizedKeys(b, t.AuthorizedKeysGithub, t.AuthorizedKeys); err != nil {
			return err
		}
		if err := a.addGuestKeys(b, o.Keys); err != nil {
			return err
		}
		// 5. Hardening, after the keys it insists on and before anything
		// that runs user code or needs the network, so a failed hook or
		// clone never leaves the box open. Its sshd reload pins AllowUsers
		// to this login; the steps below run as it, on the same master.
		if t.Harden {
			if err := bootstrap.Harden(ctx, b, m, t.Fail2ban); err != nil {
				return err
			}
		}
	}
	// 6. The user's hook.
	if err := bootstrap.RunHook(ctx, b, t.Hook); err != nil {
		return err
	}
	// 7. Repos: cloneRepos skips a checkout that exists, so a re-run is
	// idempotent. A failed clone does not end setup: every repo is tried,
	// and setup reports the failures last (setupIncomplete).
	// An interrupt during the clones is an interrupt, never "finished".
	// owner/repo specs skipped for want of gh's login are no failure.
	var incomplete error
	if len(t.Repos) > 0 {
		failed, err := a.cloneRepos(ctx, b, t.Repos)
		if err != nil {
			return fmt.Errorf("setup was interrupted while cloning: %w", err)
		}
		if len(failed) > 0 {
			incomplete = &setupIncomplete{failed: failed}
		}
	}
	a.noteContact(m, "setup", config.TransportSSH, nil) // every step above ran over ssh
	return incomplete
}

// setupIncomplete is a setup that ran every step, but some repos did not
// clone. It is not "setup failed": every step ran, so create reports the
// box as created with a problem, and the exit status is
// exitSetupIncomplete rather than 1.
type setupIncomplete struct{ failed []string }

func (e *setupIncomplete) Error() string {
	return "setup finished, but: failed to clone " + strings.Join(e.failed, ", ")
}

// exitSetupIncomplete is the exit status of a create/setup that ended in
// setupIncomplete (failureStatus). It is how a hub's proxied create/setup
// says "finished, but" to the laptop, which passes the status on. 3: not 1
// (any failure), 127 (no devvm on the hub's PATH), 130 (^C) or 255 (ssh).
const exitSetupIncomplete = 3

// startStopped boots a stopped smol VM the way `start` does (PowerStart,
// then wait for "running") and reports whether it had to. setup calls it
// before its first exec, attach/shell before theirs (bootForSession): on
// smolvm 1.16.1 an exec into a stopped VM is refused, not a boot, and
// starting it here also keeps a forward daemon that may be reconnecting
// from starting its serve exec mid-boot.
//
// Two starts at once (two attaches, an attach right after `start &`) are
// serialized by lockStart and the second finds the VM running: smolvm
// 1.16.1 lists a VM whose start is in flight as "stopped", and a second
// `machine start` then kills the first one's boot ("Killing orphaned
// _boot-vm process"), failing that start (ROADMAP Lessons). A VM smolvm
// does report "starting" is only waited for. A PowerStart that fails is
// forgiven when the VM runs anyway (a start from outside devvm). A Status
// error is no block (the session's own exec reports what is wrong), as in
// requireRunningForForwards.
func (a *App) startStopped(m *config.Machine, b backend.Backend) (bool, error) {
	if st, err := b.Status(); err != nil || st.Running {
		return false, nil
	}
	unlock, err := lockStart(a.Paths, m.Name)
	if err != nil {
		return false, err
	}
	defer unlock()
	st, err := b.Status() // again, under the lock: the start we waited for
	if err != nil || st.Running {
		return false, nil
	}
	if st.Raw == smolStarting {
		fmt.Fprintf(a.Stderr, "devvm: waiting for %s to start…\n", m.Name)
		return true, waitRunning(m, b)
	}
	fmt.Fprintf(a.Stderr, "devvm: starting %s…\n", m.Name)
	if err := b.PowerStart(); err != nil {
		if waitRunning(m, b) == nil {
			return true, nil
		}
		return false, err
	}
	return true, waitRunning(m, b)
}

// smolStarting is smolvm's state for a VM whose boot is in flight (1.16.1
// never reports it: such a VM lists as "stopped").
const smolStarting = "starting"

// lockStart serializes this host's starts of one smol VM (`start`, and
// attach/shell/setup starting it first): run/.start-NAME.lock, flock'd,
// never unlinked (like the ports locks).
func lockStart(p config.Paths, name string) (unlock func(), err error) {
	if err := p.EnsureRuntimeDir(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(p.RuntimeDir(), ".start-"+config.RuntimeName(name)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock the start of %s: %w", name, err)
	}
	return func() { f.Close() }, nil // closing releases the flock
}

// waitRunning waits (runningPollTimeout) for a VM just started to report
// running: `smolvm machine start` may return while it is still starting.
func waitRunning(m *config.Machine, b backend.Backend) error {
	if requireRunningForForwards(m, b, runningPollTimeout) != nil {
		return fmt.Errorf("%s did not reach running within %s", m.Name, runningPollTimeout)
	}
	return nil
}

// warnIgnored names, in one line, the template fields that do not apply to
// this box: sizes are create-time and smol-only; keys and hardening are for
// boxes reached over ssh.
func (a *App) warnIgnored(m *config.Machine, t config.Template, o setupOpts) {
	var ignored []string
	if m.IsRemote() {
		if t.Memory != 0 {
			ignored = append(ignored, "memory")
		}
		if t.Disk != 0 {
			ignored = append(ignored, "disk")
		}
	} else {
		if len(t.AuthorizedKeysGithub) > 0 || len(t.AuthorizedKeys) > 0 || len(o.Keys) > 0 {
			ignored = append(ignored, "keys")
		}
		if t.Harden {
			ignored = append(ignored, "harden")
		}
		if t.Fail2ban {
			ignored = append(ignored, "fail2ban")
		}
	}
	if !m.IsProvider() && t.Name != config.DefaultTemplate {
		// Placement is create-time and a provider's; nothing else has one.
		// Named only when the chosen template's own table sets it: one a
		// shared [templates.default] carries for provider boxes is not a
		// mistake on every smol or remote setup.
		if named, _, err := config.TemplateLayers(a.Paths, t.Name); err == nil {
			ignored = append(ignored, named.ProviderFields()...)
		}
	}
	if len(ignored) > 0 {
		kind := "a smol VM"
		switch {
		case m.IsProvider():
			kind = "a " + providerTitle(m.Backend) + " server"
		}
		fmt.Fprintf(a.Stderr, "devvm: template %s: ignoring %s (not applicable to %s)\n",
			t.Name, strings.Join(ignored, ", "), kind)
	}
}

// setupReachWait bounds how long setup waits for a remote box's ssh to
// answer before it gives up (config.md §9 item 5): a just-created cloud VM
// takes tens of seconds to listen on 22, and cloud-init may bounce sshd.
// Only "unreachable" is retried; a permission denied answers at once.
const setupReachWait = 90 * time.Second

// Vars so a test can shrink them.
var (
	setupReachBudget   = setupReachWait
	setupReachInterval = 5 * time.Second
)

type reachResult int

const (
	reachOK reachResult = iota
	reachDenied
	reachDown
)

// probeSSH asks whether dest answers as its login. BatchMode, so a key that
// does not work is a clean "Permission denied" on stderr rather than a
// password prompt; exit 255 alone cannot tell that from a host that is down.
// Any other exit status means the host answered and ran the login's shell
// (a nologin shell exits 1): that is for LoginUser to report, not a reason
// to wait. A changed host key or an agent offering too many keys is fatal at
// once: waiting 90s would not change either answer.
func (a *App) probeSSH(ctx context.Context, m *config.Machine) (reachResult, string, error) {
	b, err := backend.For(m, a.Paths)
	if err != nil {
		return reachDown, "", err
	}
	var stderr bytes.Buffer
	err = b.Run(ctx, backend.ExecOpts{BatchMode: true, Stdin: strings.NewReader(""),
		Stdout: io.Discard, Stderr: &stderr}, "true")
	msg := strings.TrimSpace(stderr.String())
	var ee *exec.ExitError
	switch {
	case err == nil:
		return reachOK, msg, nil
	case !errors.As(err, &ee):
		return reachDown, msg, err // no ssh at all: not worth waiting for
	case ee.ExitCode() != 255:
		return reachOK, msg, nil
	case fatalSSH(msg) != "":
		return reachDown, msg, fmt.Errorf("ssh to %s: %s", m.SSHHost, fatalSSH(msg))
	case strings.Contains(msg, "Permission denied"):
		return reachDenied, msg, nil
	default:
		return reachDown, msg, nil
	}
}

// fatalSSHMarks are ssh failures no retry can fix.
var fatalSSHMarks = []string{
	"REMOTE HOST IDENTIFICATION HAS CHANGED",
	"Host key verification failed",
	"Too many authentication failures",
	// Local failures: this host's ssh or its config, never the box.
	"ControlPath too long",
	"command not found",
	"Bad configuration option",
}

// fatalSSH returns the stderr line carrying a fatal mark, or "".
func fatalSSH(msg string) string {
	for _, mark := range fatalSSHMarks {
		if i := strings.Index(msg, mark); i >= 0 {
			line := msg[i:]
			if j := strings.IndexByte(line, '\n'); j >= 0 {
				line = line[:j]
			}
			return strings.TrimSpace(strings.Trim(line, "@ "))
		}
	}
	return ""
}

// waitSSH probes until dest answers (or refuses), for at most
// setupReachBudget.
func (a *App) waitSSH(ctx context.Context, m *config.Machine) (reachResult, error) {
	deadline := time.Now().Add(setupReachBudget)
	noted := false
	for {
		r, msg, err := a.probeSSH(ctx, m)
		if err != nil {
			return r, err
		}
		if r != reachDown {
			return r, nil
		}
		if !time.Now().Add(setupReachInterval).Before(deadline) {
			last := msg
			if i := strings.LastIndex(last, "\n"); i >= 0 {
				last = last[i+1:]
			}
			return r, fmt.Errorf("cannot reach %s over ssh after %s: %s", m.SSHHost, setupReachBudget, last)
		}
		if !noted {
			fmt.Fprintf(a.Stderr, "devvm: waiting up to %s for ssh to %s...\n", setupReachBudget, m.SSHHost)
			noted = true
		}
		select {
		case <-ctx.Done(): // an interrupt: stop waiting at once
			return reachDown, ctx.Err()
		case <-time.After(setupReachInterval):
		}
	}
}

// reachForSetup settles how setup reaches a provider server devvm made, and
// makes sure the user exists. The login it tries first is the record's
// (config.DefaultUser, which every later command logs in as), waiting a
// bounded time for a box that does not answer yet. If that login
// is refused it tries root@; a box that answers only as root (a fresh cloud
// VM) gets that user, with NOPASSWD sudo and root's keys (EnsureUser), and
// setup continues as it. Only setup ever tries root, and only here. Nothing
// is written: devvm stores nothing per server (config.md §1, §3).
func (a *App) reachForSetup(ctx context.Context, m *config.Machine) (*config.Machine, backend.Backend, error) {
	cur := *m
	// The login every later command uses, and so the user setup creates.
	user := loginUser(m)
	r, err := a.waitSSH(ctx, &cur)
	if err != nil {
		return nil, nil, err
	}
	if r == reachOK {
		// The login is dev@address (config.NewProviderMachine), never root.
		b, err := backend.For(&cur, a.Paths)
		if err != nil {
			return nil, nil, err
		}
		return &cur, b, nil
	}
	// Denied: a fresh server answers only as root.
	root := cur
	root.SSHHost = bootstrap.SwitchUser(cur.SSHHost, "root")
	if r2, _, err := a.probeSSH(ctx, &root); err != nil {
		return nil, nil, err
	} else if r2 != reachOK {
		return nil, nil, fmt.Errorf("ssh to %s was refused (permission denied), and so was %s; check that the ssh key in [providers.%s] is this host's",
			cur.SSHHost, root.SSHHost, m.Backend)
	}
	rb, err := backend.For(&root, a.Paths)
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(a.Stderr, "devvm: '%s' answers only as root; creating user '%s'\n", m.Name, user)
	if err := bootstrap.EnsureUser(ctx, rb, backend.ExecOpts{}, user); err != nil {
		return nil, nil, err
	}
	um := cur
	um.SSHHost = bootstrap.SwitchUser(cur.SSHHost, user)
	// The user may have existed already, with keys of its own that are not
	// this host's (EnsureUser copies root's only into an empty file).
	if r, msg, err := a.probeSSH(ctx, &um); err != nil {
		return nil, nil, err
	} else if r == reachDenied {
		return nil, nil, fmt.Errorf("user %s exists on '%s' but your key is not in its authorized_keys (add it as root, then re-run setup)", user, m.Name)
	} else if r != reachOK {
		return nil, nil, fmt.Errorf("cannot reach %s over ssh after creating user %s: %s", um.SSHHost, user, msg)
	}
	ub, err := backend.For(&um, a.Paths)
	if err != nil {
		return nil, nil, err
	}
	return &um, ub, nil
}

// seedAuthorizedKeys pushes a template's keys at setup. A source that
// yields no keys (e.g. a keyless github user) warns and moves on rather
// than aborting the whole setup.
func (a *App) seedAuthorizedKeys(b backend.Backend, githubUsers, files []string) error {
	lines := a.templateKeyLines(githubUsers, files)
	return a.addGuestKeys(b, lines)
}

// templateKeyLines resolves a template's authorized_keys_github users and
// authorized_keys files into key lines, on this host. Shared by setup and
// the hub proxy, which sends the lines as --key.
func (a *App) templateKeyLines(githubUsers, files []string) []string {
	var lines []string
	for _, u := range githubUsers {
		keys, err := githubKeys(u)
		if err != nil {
			fmt.Fprintf(a.Stderr, "devvm: %v\n", err)
			continue
		}
		lines = append(lines, keys...)
	}
	for _, f := range files {
		keys, err := resolvePubkeys([]string{config.ExpandHome(f)})
		if err != nil {
			fmt.Fprintf(a.Stderr, "devvm: %v\n", err)
			continue
		}
		lines = append(lines, keys...)
	}
	return lines
}

// templateArgv renders a resolved template as the explicit flag set the
// laptop sends with a proxied create/setup --template (config.md §7):
// every field, so nothing on the hub fills a gap.
func templateArgv(t config.Template, keyLines []string) []string {
	out := []string{"--" + flagInline, "--" + flagHook + "=" + t.Hook}
	if t.Memory != 0 {
		out = append(out, "--"+flagMemory+"="+strconv.Itoa(t.Memory))
	}
	if t.Disk != 0 {
		out = append(out, "--"+flagDisk+"="+strconv.Itoa(t.Disk))
	}
	for _, r := range t.Repos {
		out = append(out, "--"+flagRepo+"="+r)
	}
	for _, k := range keyLines {
		out = append(out, "--"+flagKey+"="+k)
	}
	out = append(out,
		"--"+flagHarden+"="+strconv.FormatBool(t.Harden),
		"--"+flagFail2ban+"="+strconv.FormatBool(t.Fail2ban))
	return out
}

// cloneRepos clones each spec into ~/src on the guest, skipping any that already
// exist. GitHub "owner/repo" shorthand goes through gh (login token, private
// repos); everything else (https://, ssh://, git@host:path) is a plain git
// clone so non-GitHub hosts work too. Runs in a login shell so gh (Homebrew) is
// on PATH.
//
// An owner/repo spec needs gh's login, which a fresh box lacks: the script
// checks it after the already-cloned test (so a checkout there is never
// reported) and exits ghNoLoginExit without one, and such specs are
// skipped, named in one line with the fix, never counted as failed. A
// missing gh, or an API outage, is no working login too. URL specs need no
// login.
//
// It returns the repos that failed; err is only ctx's (an interrupt stops
// the loop: the repos after it are not tried).
func (a *App) cloneRepos(ctx context.Context, b backend.Backend, repos []string) (failed []string, err error) {
	// $1 is the untrusted repo spec; it's only ever a positional arg to git/gh,
	// never interpolated into the script text.
	script := `set -e
mkdir -p "$HOME/src"; cd "$HOME/src"
repo="$1"
dir="${repo##*/}"; dir="${dir%.git}"
if [ -d "$dir" ]; then echo "devvm: $dir already cloned"; exit 0; fi
case "$repo" in
  *://*|*@*:*) git clone "$repo" ;;
  */*)         gh auth status --hostname github.com >/dev/null 2>&1 || exit ` + strconv.Itoa(ghNoLoginExit) + `
               gh repo clone "$repo" ;;
  *)           echo "devvm: unrecognized repo '$repo'" >&2; exit 1 ;;
esac`
	// Clone every repo, warning-and-continuing past a failure so one bad spec (or
	// an already-in-flight gh auth hiccup) doesn't strand the repos after it. The
	// guest script already treats an existing checkout as success (exit 0), so a
	// failure here is a real one worth surfacing but not aborting on.
	//
	// `bash -c`, not -lc: Login already wraps it in a login shell (the
	// profile's PATH is inherited), and a second login shell, SHLVL=1 under
	// smolvm's exec and ssh, runs ~/.bash_logout on `exit N`; Ubuntu's
	// stock one ends in a clear_console that fails without a console, and
	// under set -e that turns every exit status into 1.
	var skipped []string
	for _, repo := range repos {
		err := b.Run(ctx, backend.ExecOpts{Login: true}, "bash", "-c", script, "_", repo)
		var ee *exec.ExitError
		switch {
		case ctx.Err() != nil:
			return failed, ctx.Err()
		case errors.As(err, &ee) && ee.ExitCode() == ghNoLoginExit:
			skipped = append(skipped, repo)
		case err != nil:
			fmt.Fprintf(a.Stderr, "devvm: failed to clone %s: %v (continuing)\n", repo, err)
			failed = append(failed, repo)
		}
	}
	if len(skipped) > 0 {
		fmt.Fprintf(a.Stderr, "devvm: skipped %s: gh in the box has no working login; run 'gh auth login' in the box, then clone them by hand\n",
			strings.Join(skipped, ", "))
	}
	return failed, nil
}

// ghNoLoginExit is the clone script's exit for an owner/repo spec whose gh
// has no working login. It passes through smolvm's exec and ssh as is, and
// collides with nothing git, gh, bash (126, 127, 128+N) or ssh (255) exit
// with.
const ghNoLoginExit = 86
