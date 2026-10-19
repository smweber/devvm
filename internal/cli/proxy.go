package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// The hub proxy (docs/proposals/hub.md §3, roadmap step 2). A leaf run
// against HUB/NAME is re-run on the hub as `devvm LEAF NAME …` with the
// hub-side name, and the hub's own devvm does the work against its own
// registry. The remote command line is *rebuilt* from the parsed cobra
// command — the leaf's path, the flags that were explicitly set, the
// positionals — never replayed from os.Args: the persistent --config-dir and
// --state-dir name laptop directories and must never be sent, and `exec`/`keys add`
// disable flag parsing precisely so the guest command's own arguments (a
// guest --config-dir after `exec NAME --`, say) pass through untouched.
//
// Dispatch happens in each proxied leaf's RunE (see commands.go), because
// that is the only place the parsed *cobra.Command is in hand; `resolve`
// keeps refusing hub machines, which is the guard for every leaf that is
// not proxied (ports run on this host, hubforwards.go; auth subscribes here
// and then proxies, authHubMachine). cp-in/cp-out
// dispatch in their own
// RunE too, but to the tar-over-stdio loops in cp_hub.go rather than here:
// runProxied's empty stdin cannot carry an archive.

// proxiedStateVerbs are the proxied leaves after which the local change
// marker is touched, so a `status --watch` on this host re-emits even though
// the state that changed lives on the hub. It mirrors which local verbs
// defer config.TouchChanged. delete is absent because it is not dispatched
// through proxy(): runDelete proxies it itself and already defers the touch.
var proxiedStateVerbs = map[string]bool{
	"create": true, "setup": true, "start": true, "stop": true,
}

// stdinIsTTY and stdoutIsTTY decide `ssh -t` (hub.md §4): the hub side gets
// a terminal iff the laptop has one on *both* stdin and stdout. stdin alone
// is not enough: `devvm exec h/web -- cat f > out` from an interactive shell
// would get a remote pty whose ONLCR turns every \n into \r\n and merges
// stderr into stdout — local exec never allocates one. Every prompt case
// (the huh form, a confirm, a key-comment prompt) has both. Step 4's tar
// over stdio relies on this rule: its stdout is a pipe, so no pty can
// corrupt the stream. A real isatty (termios ioctl), not a ModeCharDevice
// check: /dev/null is a character device too, and `</dev/null` is exactly
// the case that must not get -t. Vars so tests can pin either answer.
var (
	stdinIsTTY  = func() bool { return isatty.IsTerminal(os.Stdin.Fd()) }
	stdoutIsTTY = func() bool { return isatty.IsTerminal(os.Stdout.Fd()) }
)

// proxyTTY is the -t decision.
func proxyTTY() bool { return stdinIsTTY() && stdoutIsTTY() }

// hubMachineArg reports whether the leaf's first positional names a machine
// on a hub. The name is validated here so a malformed HUB/NAME fails before
// anything is dialed; a bare name (or none) is not the proxy's business.
func hubMachineArg(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	_, _, ok, err := config.SplitHubName(args[0])
	return ok, err
}

// hubOr wraps a leaf's RunE: a HUB/NAME first positional is proxied to the
// hub, anything else runs the local handler. It is the one line every
// proxied leaf adds in commands.go.
func (a *App) hubOr(local func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if ok, err := hubMachineArg(args); err != nil {
			return err
		} else if ok {
			return a.proxy(cmd, args)
		}
		return local(cmd, args)
	}
}

// hubOrStopForwards is hubOr for the verbs that leave the VM down or gone
// on the hub (stop): once the proxied command succeeds, this host's daemon
// for HUB/NAME is stopped too, as runStop stops a local one. Left running it would find `__session` refusing the
// stopped VM and redial for good, every attempt (up to every 30s) an ssh
// master, a login shell and a `devvm __session` on the hub. `start
// HUB/NAME` brings it back (startHubMachine); `delete HUB/NAME` stops it
// in deleteHubMachine. A proxied failure (a refusal, an aborted prompt)
// leaves it alone: the VM may still be up.
func (a *App) hubOrStopForwards(local func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if ok, err := hubMachineArg(args); err != nil {
			return err
		} else if !ok {
			return local(cmd, args)
		}
		if err := a.proxy(cmd, args); err != nil {
			return err
		}
		if cl, err := session.Existing(a.Paths, args[0]); err == nil && cl.Stop() == nil {
			// Waited out, so a `start HUB/NAME` right after spawns its
			// daemon against a released master, not the old one's.
			_ = session.WaitGone(cl.Paths(), args[0], daemonGoneTimeout)
		}
		return nil
	}
}

// exitCode carries a command's own exit status back to Execute, which exits
// with it silently: a proxied command's (the hub's devvm already printed its
// own error, so a second "devvm: exit status 1" here would only double it)
// and `exec`'s guest command's (guestExit; it wrote its own stderr).
type exitCode struct{ code int }

func (e *exitCode) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// proxy runs the parsed leaf on the hub named by args[0]. Laptop-side inputs
// are resolved first (keys add, repos add): rebuilding argv preserves bytes,
// not semantics, and a key file, the default key set or the current
// directory's origin all belong to the laptop. Everything else goes across
// as typed.
func (a *App) proxy(cmd *cobra.Command, args []string) error {
	m, p, err := a.resolveProxy(args[0])
	if err != nil {
		return err
	}
	leaf := strings.Fields(cmd.CommandPath())[1:] // drop the root's own name
	if proxiedStateVerbs[leaf[0]] {
		defer config.TouchChanged(a.Paths) // wake `status --watch`
	}
	var runs [][]string
	var seed func() error
	shaping := false
	switch key := strings.Join(leaf, " "); {
	case key == "create" || key == "setup":
		argv, after, err := a.proxyShaping(cmd, args, m, key == "create")
		if err != nil {
			return err
		}
		runs, seed, shaping = append(runs, argv), after, true
	case key == "keys add":
		lines, err := laptopKeyLines(args[1:])
		if err != nil {
			return err
		}
		// One run per key line: the hub's `keys add` takes a single inline
		// key, and the line rides as one token so its comment survives
		// verbatim (the hub's isInlineKey sees the key type first).
		for _, line := range lines {
			argv, err := hubArgv(cmd, []string{args[0], line})
			if err != nil {
				return err
			}
			runs = append(runs, argv)
		}
	case key == "repos add":
		if len(args) == 1 {
			// As the local leaf: prompt with the laptop cwd's origin prefilled
			// when there is a terminal; without one the origin is the answer,
			// and no origin is an error rather than a hung prompt.
			repo, err := a.laptopRepo()
			if err != nil {
				return fmt.Errorf("repos add %s: %w", args[0], err)
			}
			args = append(args, repo)
		}
		argv, err := hubArgv(cmd, args)
		if err != nil {
			return err
		}
		runs = append(runs, argv)
	case key == "start" || key == "stop":
		// --yes is this host's (it skips the confirm a provider server
		// devvm did not make gets, and a hub machine is never one); a hub
		// older than step 3 would refuse the unknown flag.
		argv, err := hubArgvSkip(cmd, args, []string{"yes"})
		if err != nil {
			return err
		}
		runs = append(runs, argv)
	default:
		argv, err := hubArgv(cmd, args)
		if err != nil {
			return err
		}
		runs = append(runs, argv)
	}
	for _, argv := range runs {
		run := a.runProxied
		if shaping {
			run = a.runProxiedShaping
		}
		if err := run(m, p, argv); err != nil {
			return err
		}
	}
	if seed != nil {
		return seed()
	}
	return nil
}

// proxyShaping builds a proxied `create`/`setup HUB/NAME` (config.md §7).
//
// Saved forwards for HUB/NAME are this host's state (HUB@NAME, config.md
// §5), never the hub's: --port is always stripped and --no-ports sent, so
// the hub neither saves nor prompts for any, and the returned after seeds
// this host's file once the hub's run succeeded. It replaces the list on a
// fresh box (create, setup --adopt) and merges into it on a re-run.
//
// With --template T (other than default, which is the same as no flag),
// the template is resolved here, from this host's devvm.toml, and sent as
// the explicit template flags plus --inline-template, so the hub's own file
// never matters. Key sources are read here too (a key file and the GitHub
// lookup are this host's) and cross as lines, except for a machine this
// host knows is smol, which takes no keys. Without a template the leaf goes
// across as typed and the hub resolves its own default.
func (a *App) proxyShaping(cmd *cobra.Command, args []string, m *config.Machine, create bool) (argv []string, after func() error, err error) {
	fs := cmd.Flags()
	var ports []string
	if fs.Changed(flagPort) {
		ports, _ = fs.GetStringArray(flagPort)
	}
	adopt := false
	if fs.Lookup("adopt") != nil {
		adopt, _ = fs.GetBool("adopt")
	}
	fresh := create || adopt
	name, _ := fs.GetString(flagTemplate)
	inline := fs.Changed(flagTemplate) && name != config.DefaultTemplate

	skip := []string{flagPort, flagNoPorts}
	var extra []string
	if inline {
		var f tmplFlags
		f.User, _ = fs.GetString(flagUser)
		f.Hook, _ = fs.GetString(flagHook)
		if f.Hook == "" && fs.Lookup("bootstrap-hook") != nil {
			f.Hook, _ = fs.GetString("bootstrap-hook")
		}
		f.Memory, _ = fs.GetInt(flagMemory)
		f.Disk, _ = fs.GetInt(flagDisk)
		if fs.Changed(flagRepo) {
			f.Repos, _ = fs.GetStringArray(flagRepo)
		}
		f.Ports = ports
		f.Keys, _ = fs.GetStringArray(flagKey)
		f.Harden, _ = fs.GetBool(flagHarden)
		f.Fail2ban, _ = fs.GetBool(flagFail2ban)
		f.noteSet(fs)
		t, err := a.resolveTemplate(name, f)
		if err != nil {
			return nil, nil, err
		}
		keyLines := f.Keys
		if !a.hubMachineIsSmol(m, fs, create) {
			keyLines = append(a.templateKeyLines(t.AuthorizedKeysGithub, t.AuthorizedKeys), keyLines...)
		}
		ports = t.Ports
		// Everything the user typed except the template flags, which go
		// across resolved instead.
		skip = append(append(skip, flagTemplate, "bootstrap-hook"), templateFlagNames...)
		extra = templateArgv(t, keyLines)
	}
	// A create with no --backend says smol explicitly: the hub's own
	// devvm.toml may configure a provider, and its create would then ask
	// which backend (and could bill a server from the hub). HUB/NAME is
	// only ever a hub's smol VM (providers.md §6).
	if create && !fs.Changed("backend") {
		extra = append(extra, "--backend="+config.BackendSmol)
	}
	extra = append(extra, "--"+flagNoPorts)
	argv, err = hubArgvSkip(cmd, args, skip)
	if err != nil {
		return nil, nil, err
	}
	// The leaf and the machine come first; the added flags go after them,
	// where cobra parses them like any other flag.
	head := len(strings.Fields(cmd.CommandPath())) // leaf path + machine
	argv = append(append(slices.Clone(argv[:head]), extra...), argv[head:]...)
	after = func() error {
		if !fresh && len(ports) == 0 {
			return nil
		}
		return a.seedPorts(m, ports, fresh)
	}
	return argv, after, nil
}

// hubMachineIsSmol reports whether a hub machine is known to be smol, so a
// template's keys (ignored on smol) need no resolving: no GitHub fetch and
// no "ignoring keys" line on every run. A create is smol unless --backend
// says otherwise; a setup asks this host's cached listing of the hub, and a
// machine missing from it is taken as smol (what hub machines are), so only
// a cached remote row gets keys.
func (a *App) hubMachineIsSmol(m *config.Machine, fs *pflag.FlagSet, create bool) bool {
	if create {
		b, _ := fs.GetString("backend")
		return b == "" || b == config.BackendSmol
	}
	for _, row := range readHubCache(a.Paths, m.Hub.Name) {
		if row.name == m.HubMachineName() {
			return row.backend == config.BackendSmol
		}
	}
	return true
}

// runProxiedShaping is runProxied for create/setup, mapping a hub too old
// for them to one line: `setup` and the flags the laptop adds
// (--inline-template, --no-ports) are v0.1.16, and an older hub answers
// with cobra's "unknown command"/"unknown flag", or a login shell's 127.
// The hub's output is passed through as it comes (a pty run merges both
// streams, so both are watched).
func (a *App) runProxiedShaping(m *config.Machine, p backend.Proxier, argv []string) error {
	var seen bytes.Buffer
	o := proxyExecOpts(proxyTTY())
	o.Stdout, o.Stderr = io.MultiWriter(a.Stdout, &seen), io.MultiWriter(a.Stderr, &seen)
	err := p.Proxy(context.Background(), o, argv...)
	var ee *exec.ExitError
	if err != nil && errors.As(err, &ee) {
		out := seen.String()
		if ee.ExitCode() == 127 || strings.Contains(out, `unknown command "setup"`) ||
			strings.Contains(out, "unknown flag: --"+flagInline) || strings.Contains(out, "unknown flag: --"+flagNoPorts) {
			return fmt.Errorf("hub %s runs devvm older than %s (needs setup); run 'devvm update' there", m.Hub.Name, hubMinVersion)
		}
	}
	return a.exitStatus(m, err)
}

// proxyInteractive is attach/shell for a hub machine: the same proxied leaf,
// run with a terminal. --transport is validated and consumed here (it
// describes the laptop→hub hop, which is ssh for now — see
// hubBackend.ProxyInteractive) and never forwarded: on the hub the machine
// is usually smol, whose attach refuses the flag.
func (a *App) proxyInteractive(cmd *cobra.Command, args []string, transport string) error {
	m, p, err := a.resolveProxy(args[0])
	if err != nil {
		return err
	}
	tr, err := resolveTransport(m, transport)
	if err != nil {
		return err
	}
	// Subscribed here, through this host's daemon for HUB@NAME, before the
	// leaf runs on the hub with DEVVM_NO_SUBSCRIBE=1 (hub.md §8): a URL the
	// session opens lands on this host, not the hub's. A VM the hub says is
	// not running is started first (hubBootForSession), then subscribed.
	release, err := a.hubBridge(m, func() error { return a.hubBootForSession(m, p) })
	if err != nil {
		return err
	}
	defer release()
	leaf := strings.Fields(cmd.CommandPath())[1:]
	return a.exitStatus(m, p.ProxyInteractive(tr, append(leaf, m.HubMachineName())...))
}

// hubBootForSession is bootForSession for a hub machine, run when the
// hub's `__session` refused the bridge for a VM that is not running: the
// proxied `start` (the hub waits for "running" and brings its own forwards
// up), then this host's saved forwards for it, as `start HUB/NAME` does.
// Proxied rather than left to the hub-side attach, so a hub of any version
// boots it and the bridge is subscribed before the session runs.
func (a *App) hubBootForSession(m *config.Machine, p backend.Proxier) error {
	fmt.Fprintf(a.Stderr, "devvm: starting %s…\n", m.Name)
	defer config.TouchChanged(a.Paths) // wake `status --watch`
	// No terminal for it (start prompts for nothing on smol): with `-t` its
	// ssh would read what the user types ahead into the session. BatchMode
	// costs nothing: the ControlMaster the __session just used is up.
	o := proxyExecOpts(false)
	o.Stdout, o.Stderr = a.Stdout, a.Stderr
	if err := a.exitStatus(m, p.Proxy(context.Background(), o, "start", m.HubMachineName())); err != nil {
		return err
	}
	if err := a.hubForwardsAfterStart(m.Name); err != nil {
		fmt.Fprintf(a.Stderr, "devvm: saved forwards for %s not brought up (%v); 'devvm ports up %s' retries\n", m.Name, err, m.Name)
	}
	return nil
}

// resolveProxy loads the hub-machine record and its proxy surface. Like
// resolve it reads the hub entry only: whether the machine exists is the
// proxied command's answer.
func (a *App) resolveProxy(name string) (*config.Machine, backend.Proxier, error) {
	m, b, err := a.resolveAny(name)
	if err != nil {
		return nil, nil, err
	}
	p, ok := b.(backend.Proxier)
	if !ok || !m.IsHubMachine() {
		return nil, nil, fmt.Errorf("%s is not a machine on a hub", name)
	}
	return m, p, nil
}

// runProxied runs one rebuilt devvm argv on the hub. With a terminal on
// stdin the hub gets one too (-t), so its prompts — the huh form of
// `create`, a delete's confirm, a key-comment prompt — work; without one
// nothing is piped: no proxied leaf reads stdin (prompts use /dev/tty), a
// smol one-shot exec has none (hub.md §4), and forwarding a script's stdin
// into ssh would eat it, the classic `while read` trap `ssh -n` exists for.
// BatchMode goes with it: no terminal means no way to answer a passphrase
// prompt, so fail fast rather than hang a script.
func (a *App) runProxied(m *config.Machine, p backend.Proxier, argv []string) error {
	o := proxyExecOpts(proxyTTY())
	o.Stdout, o.Stderr = a.Stdout, a.Stderr
	return a.exitStatus(m, p.Proxy(context.Background(), o, argv...))
}

func proxyExecOpts(tty bool) backend.ExecOpts {
	if tty {
		return backend.ExecOpts{TTY: true}
	}
	return backend.ExecOpts{BatchMode: true, Stdin: strings.NewReader("")}
}

// exitStatus turns the ssh exit into what the user should see: the remote
// devvm's own status, silently (it printed its error), except ssh's 255,
// which is the hop failing and gets one line naming the hub. 255 is
// conflated with "unreachable" by design: it is ssh's own code for a
// connection or protocol failure, and the hub's devvm never produces it.
// The hub's Execute flattens a leaf failure to 1, except where the leaf
// returns an exitCode: a guest command's own status under `exec` comes
// back as is (guestExit), and a guest's 255 is ssh's again here, read as
// the hub unreachable (a remote box's own 255 never crosses: guestExit
// makes it an error on the hub). A negative code —
// ssh killed by a signal — is clamped to 1 so os.Exit never sees -1.
//
// A ^C typed into a -t run is no signal on this host (ssh's terminal is
// raw): the hub's pty delivers it, and the hub devvm it kills used to come
// back as ssh's 255 for an exit-signal, misread here as the hub
// unreachable. The -t wrapper (backend.TTYLoginShellArgv) turns that death
// into 130, which passes through below silently like any status.
func (a *App) exitStatus(m *config.Machine, err error) error {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return err
	}
	code := ee.ExitCode()
	switch {
	case code == 255:
		return fmt.Errorf("cannot reach hub '%s' (%s): ssh exited 255", m.Hub.Name, m.Hub.SSHHost)
	case code < 0:
		code = 1
	}
	return &exitCode{code: code}
}

// laptopKeyLines resolves a `keys add` spec on the laptop: a file path, an
// inline key, --from-github USER, or (bare) every ~/.ssh/id_*.pub here. The
// bare form deliberately means "my keys" rather than the local leaf's
// GitHub-user prompt: on a hub machine the point is to get the laptop in.
// labelKeys runs here too, so an uncommented key is labeled with the
// laptop's hostname, not the hub's.
func laptopKeyLines(spec []string) ([]string, error) {
	lines, err := resolvePubkeys(spec)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("no ~/.ssh/id_*.pub on this host; pass a key file or --from-github USER")
	}
	return labelKeys(lines)
}

// laptopRepo settles a bare `repos add HUB/NAME`: the local leaf's prompt
// (origin prefilled) on a terminal, else the laptop cwd's origin itself.
func (a *App) laptopRepo() (string, error) {
	if stdinIsTTY() {
		return promptRepo()
	}
	origin := normalizeRepo(originRemote())
	if origin == "" {
		return "", fmt.Errorf("no REPO given and the current directory has no git origin")
	}
	return origin, nil
}

// hubArgv rebuilds the devvm argv to run on the hub from a parsed leaf:
// the leaf's path, then its first positional with HUB/ stripped, then the
// flags that were explicitly set, then the remaining positionals. A `--`
// the user typed is put back where it was (ArgsLenAtDash) so everything
// after it is byte-identical; a leaf with flag parsing disabled (exec, keys
// add) has its args verbatim already, `--` included. Only the leaf's own
// flags are walked: --config-dir and any other persistent flag of a parent
// belong to the laptop.
func hubArgv(cmd *cobra.Command, args []string) ([]string, error) {
	return hubArgvSkip(cmd, args, nil)
}

// hubArgvSkip is hubArgv leaving out the named flags.
func hubArgvSkip(cmd *cobra.Command, args []string, skip []string) ([]string, error) {
	argv := append([]string{}, strings.Fields(cmd.CommandPath())[1:]...)
	pos := append([]string{}, args...)
	if len(pos) > 0 {
		// hubMachineArg validated the reference before dispatch, so an error
		// here would be a caller passing something else; never let it
		// become an empty machine name on the wire.
		_, machine, ok, err := config.SplitHubName(pos[0])
		if err != nil {
			return nil, err
		}
		if ok {
			pos[0] = machine
		}
	}
	dash := cmd.Flags().ArgsLenAtDash()
	if dash != 0 && len(pos) > 0 {
		argv = append(argv, pos[0])
		pos = pos[1:]
		if dash > 0 {
			dash--
		}
	}
	argv = append(argv, setFlags(cmd, skip...)...)
	if dash >= 0 {
		argv = append(argv, pos[:dash]...)
		argv = append(argv, "--")
		argv = append(argv, pos[dash:]...)
	} else {
		argv = append(argv, pos...)
	}
	return argv, nil
}

// setFlags renders the leaf's explicitly set local flags as --name=value
// tokens (the `=` form so a value starting with `-` is never re-read as a
// flag). A true bool is the bare --name; a slice flag repeats. It walks
// cmd.Flags(), the set that parsed (Visit yields only what was set);
// cobra's LocalFlags() is a copy of the definitions with no parse state.
// Inherited flags — the root's --config-dir and --state-dir — are the
// laptop's and skipped.
func setFlags(cmd *cobra.Command, skip ...string) []string {
	var out []string
	inherited := cmd.InheritedFlags()
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if f.Name == "help" || inherited.Lookup(f.Name) != nil || slices.Contains(skip, f.Name) {
			return
		}
		switch v := f.Value.(type) {
		case pflag.SliceValue:
			// One --name=item per element. A stringSlice (CSV) flag whose
			// item itself contains a comma would be re-split on the hub; the
			// template flags (--repo, --port, --key) are stringArrays for
			// that reason, so a key line's options survive.
			for _, item := range v.GetSlice() {
				out = append(out, "--"+f.Name+"="+item)
			}
		default:
			if f.Value.Type() == "bool" && f.Value.String() == "true" {
				out = append(out, "--"+f.Name)
				return
			}
			out = append(out, "--"+f.Name+"="+f.Value.String())
		}
	})
	return out
}
