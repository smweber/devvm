package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/bootstrap"
	"github.com/smweber/devvm/internal/config"
)

// `devvm setup` (config.md §3) is the single place devvm touches a box's
// contents, and the single promotion path to managed. It replaced
// `bootstrap`, whose name also meant the user's own hook script. Its order
// is fixed: ensure user → prereqs → agent + shim (+ profile.d on smol) →
// hook → repos → keys → harden → seed saved forwards. The user comes first
// because everything after it runs as that user or through its sudo;
// harden comes after keys because it refuses on an empty authorized_keys.

// tmplFlags are the template fields as flags, shared by `create` and
// `setup`. They are the explicit form the laptop sends to a hub's proxied
// create/setup (config.md §7), so the hub's own file never matters there;
// they also work directly. Each overrides the resolved template field.
type tmplFlags struct {
	User   string
	Hook   string
	Memory int
	Disk   int
	Repos  []string
	Ports  []string
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
	// [templates.*] are not consulted; unset fields take the built-ins. It
	// implies NoPorts.
	Inline bool
	// NoPorts: this host saves no forwards and asks for none. A laptop
	// proxying to its hub sends it, since the forwards for HUB/NAME are the
	// laptop's own state (HUB@NAME), seeded there.
	NoPorts bool
}

// savesPorts reports whether this run keeps saved forwards on this host.
func (v tmplFlags) savesPorts() bool { return !v.Inline && !v.NoPorts }

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
	fs.StringVar(&v.User, flagUser, "", "remote: the user setup creates and logs in as (template user; default dev)")
	fs.StringVar(&v.Hook, flagHook, "", "hook: url:<URL> [args] | cmd:<path> [args] | none (template hook)")
	fs.IntVarP(&v.Memory, flagMemory, "m", 0, "smol: VM memory in MiB (template memory)")
	fs.IntVarP(&v.Disk, flagDisk, "d", 0, "smol: VM disk in GiB (template disk; default 50)")
	fs.StringArrayVar(&v.Repos, flagRepo, nil, "repo to clone into ~/src (repeatable; template repos)")
	fs.StringArrayVar(&v.Ports, flagPort, nil, "forward to save, HOST:GUEST or PORT (repeatable; template ports)")
	fs.StringArrayVar(&v.Keys, flagKey, nil, "remote: authorized_keys line to add (repeatable)")
	fs.BoolVar(&v.Harden, flagHarden, false, "remote: harden sshd + firewall (template harden)")
	fs.BoolVar(&v.Fail2ban, flagFail2ban, false, "remote: install fail2ban with harden (template fail2ban)")
	fs.BoolVar(&v.Inline, flagInline, false, "the template flags are the whole template (sent by a laptop to its hub)")
	fs.BoolVar(&v.NoPorts, flagNoPorts, false, "save no forwards here (sent by a laptop to its hub: they are the laptop's)")
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
	if v.User != "" {
		u := v.User
		s.User = &u
	}
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
	if v.Ports != nil {
		p := slices.Clone(v.Ports)
		s.Ports = &p
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

// validateTemplate checks what config cannot: the hook spec and the port
// mappings, whose parsers live here and in bootstrap.
func validateTemplate(t config.Template) error {
	if _, err := bootstrap.ParseSpec(t.Hook); err != nil {
		return fmt.Errorf("template %s: %w", t.Name, err)
	}
	for _, p := range t.Ports {
		if _, _, err := parseMapping(p); err != nil {
			return fmt.Errorf("template %s: %w", t.Name, err)
		}
	}
	if err := config.ValidUser(t.User); err != nil {
		return fmt.Errorf("template %s: %w", t.Name, err)
	}
	return nil
}

// setupOpts is `setup`'s input.
type setupOpts struct {
	Template string
	Adopt    bool
	Yes      bool
	Host     string // remote: this run's address, never written
	tmplFlags

	// resolved is create's already-settled template (flags, prompts and
	// all); setup then resolves nothing itself.
	resolved *config.Template
	// fresh: the box just became managed (create, --adopt), so a ports
	// file a same-named predecessor left is replaced, not merged into.
	fresh bool
}

func (a *App) setupCmd() *cobra.Command {
	var o setupOpts
	c := &cobra.Command{
		Use:   "setup NAME",
		Short: "Shape a box: user, prereqs, agent, hook, repos, keys, hardening",
		Long: "Shape a box from a template ([templates.*] in devvm.toml; default without\n" +
			"--template). The one place devvm changes a box's contents, in a fixed order:\n" +
			"user, prereqs (incl. tmux, gh, mosh), agent + BROWSER shim, the template's\n" +
			"hook, repos (cloned into ~/src), keys and hardening (remote), saved forwards.\n" +
			"Safe to re-run.\n\n" +
			"A managed smol VM or a remote entry with managed = true is shaped without\n" +
			"asking. A smol VM devvm did not create needs --adopt, which asks you to type\n" +
			"its name; a remote entry with managed = false is never shaped. A remote box\n" +
			"is reached as the entry's user, else the template's (on a root-only box, the\n" +
			"user is created). Precedence: flag > --template > [templates.default] > built-in.",
		RunE: a.hubOr(func(cmd *cobra.Command, args []string) error {
			o.noteSet(cmd.Flags())
			return a.runSetup(args[0], o)
		}),
	}
	a.machineArg(c)
	f := c.Flags()
	f.StringVarP(&o.Template, flagTemplate, "t", "", "template to shape with (default: default)")
	f.BoolVar(&o.Adopt, "adopt", false, "hand a smol VM devvm did not create over to devvm (asks you to type its name)")
	f.BoolVarP(&o.Yes, "yes", "y", false, "don't prompt (never applies to --adopt)")
	f.StringVar(&o.Host, "host", "", "remote: reach the box at this address for this run only (nothing is written)")
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
	defer config.TouchChanged(a.Paths) // --adopt changes the managed column
	m, b, err := a.resolveUnmanaged(name)
	if err != nil {
		return err
	}
	if m.IsRemote() {
		if o.Adopt {
			return fmt.Errorf("--adopt applies only to a local smol VM devvm did not create; '%s' is a %s entry, whose managed line is the consent", name, m.Backend)
		}
		if !m.Managed() {
			return fmt.Errorf("'%s' is an adopted host (managed = false): devvm never shapes it; manage its keys with 'devvm keys', or set managed = true in its [machines.%s] entry ('devvm config edit') to hand it over",
				name, name)
		}
	} else {
		if o.Host != "" {
			return fmt.Errorf("--host applies only to remote boxes; '%s' is a local smol VM", name)
		}
		switch {
		case m.Managed() && o.Adopt:
			return fmt.Errorf("'%s' is already managed by devvm; re-run 'devvm setup %s' without --adopt", name, name)
		case !m.Managed() && !o.Adopt:
			return fmt.Errorf("'%s' is a smol VM devvm did not create; %s", name, backend.AdoptHint(name))
		}
	}
	if o.Adopt && o.Yes {
		return fmt.Errorf("--yes does not apply to --adopt: handing '%s' to devvm needs you to type its name", name)
	}
	var t config.Template
	if o.resolved != nil {
		t = *o.resolved
	} else if t, err = a.resolveTemplate(o.Template, o.tmplFlags); err != nil {
		return err
	}
	if err := checkTemplateUser(m, t); err != nil {
		return err
	}
	if o.Adopt {
		if err := a.confirmAdopt(m, t); err != nil {
			return err
		}
		if err := a.writeManagedMarker(name); err != nil {
			return fmt.Errorf("could not mark '%s' managed: %w", name, err)
		}
		// Re-resolve: the smol source settles Managed from the marker, and
		// only a managed record execs as dev and may install.
		if m, b, err = a.resolve(name); err != nil {
			return err
		}
		o.fresh = true
		fmt.Fprintf(a.Stderr, "devvm: '%s' is managed by devvm now\n", name)
	}
	return a.shape(context.Background(), m, b, t, o)
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

// checkTemplateUser refuses, before anything runs, a template whose user is
// not the one a remote entry with no `user` logs in as
// ([templates.default]'s): setup would create that user and pin sshd's
// AllowUsers to it, and every later command, logging in as the other one,
// would be locked out. Nothing is written for the user: the fix is the
// entry's own `user` line. An entry that names its user is the login
// whatever the template says (reachForSetup warns).
func checkTemplateUser(m *config.Machine, t config.Template) error {
	if !m.IsRemote() || m.User != "" {
		return nil
	}
	if login := loginUser(m); login != "" && login != t.User {
		return fmt.Errorf("template %s sets user = %s but %s logs in as %s; set user = %q in [machines.%s] first",
			t.Name, t.User, m.Name, login, t.User, m.Name)
	}
	return nil
}

// browserStep is the agent + shim (+ profile.d) step, a var so a test can
// stand in for agentbin's install.
var browserStep = bootstrap.Browser

// shape runs the fixed order on a box already cleared to be shaped.
func (a *App) shape(ctx context.Context, m *config.Machine, b backend.Backend, t config.Template, o setupOpts) error {
	a.warnIgnored(m, t, o)
	// 1. The user. A remote box may answer only as root (a fresh cloud VM);
	// reachForSetup waits for it and creates the user there. On smol the
	// exec enters as root and the backend is wired to dev.
	if m.IsRemote() {
		var err error
		if m, b, err = a.reachForSetup(ctx, m, t.User, o.Host); err != nil {
			return err
		}
	} else {
		if err := a.startForSetup(m, b); err != nil {
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
	// 3. Agent + BROWSER shim + /etc/profile.d/devvm.sh, on smol only (a
	// no-op on remote boxes, which wait for the ssh agent exec). Before the
	// hook, so the hook's own login shells already see BROWSER.
	if err := browserStep(ctx, b, m); err != nil {
		return err
	}
	// 4. The user's hook.
	if err := bootstrap.RunHook(ctx, b, t.Hook); err != nil {
		return err
	}
	// 5. Repos: cloneRepos skips a checkout that exists, so a re-run is
	// idempotent.
	if len(t.Repos) > 0 {
		if err := a.cloneRepos(b, t.Repos); err != nil {
			return err
		}
	}
	if m.IsRemote() {
		// 6. Keys (a source that yields nothing warns and moves on).
		if err := a.seedAuthorizedKeys(b, t.AuthorizedKeysGithub, t.AuthorizedKeys); err != nil {
			return err
		}
		if err := a.addGuestKeys(b, o.Keys); err != nil {
			return err
		}
		// 7. Hardening, after the keys it insists on.
		if t.Harden {
			if err := bootstrap.Harden(ctx, b, m, t.Fail2ban); err != nil {
				return err
			}
		}
	}
	a.noteContact(m, "setup", config.TransportSSH, nil) // every step above ran over ssh
	// 8. Saved forwards (host state, config.md §5), unless they are the
	// proxying laptop's.
	if !o.savesPorts() {
		return nil
	}
	return a.seedPorts(m, t.Ports, o.fresh)
}

// startForSetup boots a stopped smol VM the way `start` does (PowerStart,
// then wait for "running") before setup's first exec. Left to the exec,
// smolvm would boot it implicitly, behind a forward daemon that may be
// reconnecting and would start its serve exec mid-boot: the overlap of
// exec startups the one-exec rule (and attach's refusal) exists to avoid.
// After the --adopt prompt, so an unconfirmed VM is never booted.
func (a *App) startForSetup(m *config.Machine, b backend.Backend) error {
	st, err := b.Status()
	if err != nil {
		return err
	}
	if st.Running {
		return nil
	}
	fmt.Fprintf(a.Stderr, "devvm: starting %s…\n", m.Name)
	if err := b.PowerStart(); err != nil {
		return err
	}
	return requireRunningForForwards(m, b, runningPollTimeout)
}

// warnIgnored names, in one line, the template fields that do not apply to
// this box: sizes are create-time and smol-only; keys and hardening are for
// boxes reached over ssh; smol is wired to the dev user.
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
		if t.User != backend.DefaultUser {
			ignored = append(ignored, "user")
		}
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
	if len(ignored) > 0 {
		kind := "a smol VM"
		if m.IsRemote() {
			kind = "a remote box"
		}
		fmt.Fprintf(a.Stderr, "devvm: template %s: ignoring %s (not applicable to %s)\n",
			t.Name, strings.Join(ignored, ", "), kind)
	}
}

// seedPorts saves the template's forwards. On a fresh box the list
// replaces whatever file is there (a predecessor's of the same name, never
// cleared), so a recreated VM never inherits old forwards; on a re-run it
// is merged, so `ports add` work survives.
func (a *App) seedPorts(m *config.Machine, ports []string, fresh bool) error {
	if !fresh && len(ports) == 0 {
		return nil
	}
	return a.editPorts(m, func(cur []string) []string {
		if fresh {
			return slices.Clone(ports)
		}
		for _, p := range ports {
			pref, guest, err := parseMapping(p)
			if err != nil {
				continue // validateTemplate refused it already
			}
			if _, ok := findMappingIn(cur, pref, guest); !ok {
				cur = append(cur, p)
			}
		}
		return cur
	})
}

// openAdoptTTY is the terminal the adoption prompt runs on, a var so a test
// can type into it.
var openAdoptTTY = func() (io.ReadWriteCloser, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

// confirmAdopt is the handover's gate (config.md §3): print exactly what
// will happen and wait for the VM's name, typed. Deliberately harder than a
// y/n: once managed, devvm installs into the VM, and `delete` destroys it.
// No terminal, no adoption.
func (a *App) confirmAdopt(m *config.Machine, t config.Template) error {
	refuse := fmt.Errorf("setup --adopt %s needs a terminal: you confirm the handover by typing the VM's name", m.Name)
	if !stdinIsTTY() {
		return refuse
	}
	tty, err := openAdoptTTY()
	if err != nil {
		return refuse
	}
	defer tty.Close()
	fmt.Fprint(tty, adoptPlan(m.Name, t))
	fmt.Fprintf(tty, "Type the VM's name (%s) to hand it over: ", m.Name)
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("aborted: '%s' was not adopted", m.Name)
	}
	if strings.TrimSpace(line) != m.Name {
		return fmt.Errorf("aborted: '%s' was not adopted (typed %q)", m.Name, strings.TrimSpace(line))
	}
	return nil
}

// adoptPlan is what `setup --adopt` prints before asking.
func adoptPlan(name string, t config.Template) string {
	var s strings.Builder
	fmt.Fprintf(&s, "devvm setup --adopt %s hands the smol VM '%s' to devvm. Using template %s, it will:\n", name, name, t.Name)
	fmt.Fprintf(&s, "  - create the user '%s' with passwordless sudo (as root)\n", backend.DefaultUser)
	fmt.Fprintf(&s, "  - install devvm's prereqs (build tools, curl, git, rsync, tmux, gh, mosh) as root\n")
	fmt.Fprintf(&s, "  - install the devvm agent and BROWSER shim as root (and %s)\n", bootstrap.ProfilePath)
	if t.Hook == bootstrap.KindNone {
		fmt.Fprintf(&s, "  - run no hook (hook = none)\n")
	} else {
		fmt.Fprintf(&s, "  - run the hook: %s\n", t.Hook)
	}
	if len(t.Repos) > 0 {
		fmt.Fprintf(&s, "  - clone into ~/src: %s\n", strings.Join(t.Repos, ", "))
	}
	if len(t.Ports) > 0 {
		fmt.Fprintf(&s, "  - save forwards: %s\n", strings.Join(t.Ports, ", "))
	}
	fmt.Fprintf(&s, "  - treat '%s' as managed from now on: devvm installs into it, and 'devvm delete %s' destroys it\n", name, name)
	return s.String()
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
		time.Sleep(setupReachInterval)
	}
}

// withHost puts addr in place of an ssh destination's host, keeping its
// user (`setup --host`). An addr with its own user@ is taken as is.
func withHost(sshHost, addr string) string {
	if strings.Contains(addr, "@") {
		return addr
	}
	if u, _, ok := strings.Cut(sshHost, "@"); ok {
		return u + "@" + addr
	}
	return addr
}

// reachForSetup settles how setup reaches a managed remote box, and makes
// sure the user exists. The login it tries first is the record's (the
// entry's `user`, or with none [templates.default]'s; at --host's address
// for this run, if given),
// waiting a bounded time for a box that does not answer yet. If that login
// is refused it tries root@; a box that answers only as root (a fresh cloud
// VM) gets that user, with NOPASSWD sudo and root's keys (EnsureUser), and
// setup continues as it. Only setup ever tries root, and only here (remote
// + managed). Nothing is written: the entry names the box, and the user
// every later command logs in as is the entry's, else [templates.default]'s
// (config.md §1, §3).
func (a *App) reachForSetup(ctx context.Context, m *config.Machine, tmplUser, host string) (*config.Machine, backend.Backend, error) {
	cur := *m
	// The login every later command uses (the entry's user, else
	// [templates.default]'s); runSetup refused a template that would make
	// another one, so this is also the user setup creates.
	user := loginUser(m)
	if host != "" {
		cur.SSHHost = withHost(cur.SSHHost, host)
	}
	r, err := a.waitSSH(ctx, &cur)
	if err != nil {
		return nil, nil, err
	}
	var root *config.Machine
	switch r {
	case reachOK:
		b, err := backend.For(&cur, a.Paths)
		if err != nil {
			return nil, nil, err
		}
		login, err := bootstrap.LoginUser(ctx, b)
		if err != nil {
			return nil, nil, err
		}
		if login != "root" {
			if login != "" && login != tmplUser {
				// The entry's login wins (user = "ubuntu"): setup runs as it,
				// and Harden pins AllowUsers to it, not to the template's user.
				fmt.Fprintf(a.Stderr, "devvm: '%s' logs in as %s: template user %q is ignored, and hardening would pin AllowUsers to %s\n",
					m.Name, login, tmplUser, login)
			}
			return &cur, b, nil
		}
		root = &cur // the configured login is root itself
	case reachDenied:
		rm := cur
		rm.SSHHost = bootstrap.SwitchUser(cur.SSHHost, "root")
		if r2, _, err := a.probeSSH(ctx, &rm); err != nil {
			return nil, nil, err
		} else if r2 != reachOK {
			return nil, nil, fmt.Errorf("ssh to %s was refused (permission denied), and so was %s; add a key with 'devvm keys add %s' or check identity in its entry",
				cur.SSHHost, rm.SSHHost, m.Name)
		}
		root = &rm
	}
	if user == "root" {
		return nil, nil, fmt.Errorf("'%s' logs in as root (user = \"root\"): setup creates an unprivileged user with sudo; remove the entry's user, or set another", m.Name)
	}
	rb, err := backend.For(root, a.Paths)
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

func (a *App) runLockdown(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if err := requireRemote(m, "lockdown"); err != nil {
		return err
	}
	if !m.Managed() {
		ok, err := confirm(fmt.Sprintf("'%s' is an unmanaged host — really run lockdown on it?", name))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("aborted")
		}
	}
	// fail2ban is a template field now (the entry holds no shaping
	// fields): [templates.default]'s, as for a setup with no --template.
	t, err := a.resolveTemplate("", tmplFlags{})
	if err != nil {
		return err
	}
	return bootstrap.Harden(context.Background(), b, m, t.Fail2ban)
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
// every field, so nothing on the hub fills a gap. Ports are not sent: a
// hub machine's saved forwards are this host's state (HUB@NAME), seeded
// here after the proxied run.
func templateArgv(t config.Template, keyLines []string) []string {
	out := []string{"--" + flagInline, "--" + flagUser + "=" + t.User, "--" + flagHook + "=" + t.Hook}
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
