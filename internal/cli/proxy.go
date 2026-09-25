package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
// positionals — never replayed from os.Args: the persistent --config-dir
// names a laptop directory and must never be sent, and `exec`/`keys add`
// disable flag parsing precisely so the guest command's own arguments (a
// guest --config-dir after `exec NAME --`, say) pass through untouched.
//
// Dispatch happens in each proxied leaf's RunE (see commands.go), because
// that is the only place the parsed *cobra.Command is in hand; `resolve`
// keeps refusing hub machines, which is the guard for every leaf that is
// not proxied (auth; ports run on this host, hubforwards.go). cp-in/cp-out
// dispatch in their own
// RunE too, but to the tar-over-stdio loops in cp_hub.go rather than here:
// runProxied's empty stdin cannot carry an archive.

// proxiedStateVerbs are the proxied leaves after which the local change
// marker is touched, so a `status --watch` on this host re-emits even though
// the state that changed lives on the hub. It mirrors which local verbs
// defer config.TouchChanged. delete is absent because it is not dispatched
// through proxy(): runDelete proxies it itself and already defers the touch.
var proxiedStateVerbs = map[string]bool{
	"create": true, "provision": true, "deprovision": true,
	"start": true, "stop": true,
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
// on the hub (stop, deprovision): once the proxied command succeeds, this
// host's daemon for HUB/NAME is stopped too, as runStop and runDeprovision
// stop a local one. Left running it would find `__session` refusing the
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
		if cl, err := session.Existing(a.ConfigDir, args[0]); err == nil && cl.Stop() == nil {
			// Waited out, so a `start HUB/NAME` right after spawns its
			// daemon against a released master, not the old one's.
			_ = session.WaitGone(a.ConfigDir, args[0], daemonGoneTimeout)
		}
		return nil
	}
}

// proxyExit carries the remote command's exit status back to Execute, which
// exits with it silently: the hub's devvm already printed its own error, so
// a second "devvm: exit status 1" here would only double it.
type proxyExit struct{ code int }

func (e *proxyExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }

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
		defer config.TouchChanged(a.ConfigDir) // wake `status --watch`
	}
	var runs [][]string
	switch strings.Join(leaf, " ") {
	case "keys add":
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
	case "repos add":
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
	default:
		argv, err := hubArgv(cmd, args)
		if err != nil {
			return err
		}
		runs = append(runs, argv)
	}
	for _, argv := range runs {
		if err := a.runProxied(m, p, argv); err != nil {
			return err
		}
	}
	return nil
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
	leaf := strings.Fields(cmd.CommandPath())[1:]
	return a.exitStatus(m, p.ProxyInteractive(tr, append(leaf, m.HubMachineName())...))
}

// resolveProxy loads the hub-machine record and its proxy surface. Like
// resolve it reads the hub conf only: whether the machine exists is the
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
// In fact the hub's Execute flattens every leaf failure to 1 — including a
// guest command's own status under `exec` (`sh -c 'exit 3'` comes back as
// 1, on a local machine too) — so today only 0, 1 and 255 are expected
// here; the pass-through is for whenever that changes. A negative code —
// ssh killed by a signal — is clamped to 1 so os.Exit never sees -1.
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
	return &proxyExit{code: code}
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
	argv = append(argv, setFlags(cmd)...)
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
// Inherited flags — the root's --config-dir — are the laptop's and skipped.
func setFlags(cmd *cobra.Command) []string {
	var out []string
	inherited := cmd.InheritedFlags()
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if f.Name == "help" || inherited.Lookup(f.Name) != nil {
			return
		}
		switch v := f.Value.(type) {
		case pflag.SliceValue:
			// One --name=item per element. A stringSlice (CSV) flag whose
			// item itself contains a comma would be re-split on the hub; no
			// proxied leaf has a slice flag today, so this is future-proofing
			// with that one caveat.
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
