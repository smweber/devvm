package backend

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/smweber/devvm/internal/config"
)

// hubBackend is the Backend for a HUB/NAME record: a machine that lives in
// another host's devvm registry and is reached only through that host
// (docs/proposals/hub.md §4). It implements Backend only as far as the local
// leaves need. Proxy and ProxyInteractive are the transport the cli's
// proxied leaves ride: a `devvm` command line rebuilt on the laptop, run on
// the hub under the user's login shell. Run is the proxied `exec` for the
// non-interactive probes later steps make (cp, auth). Power* and Copy stay
// refused: the cli proxies the lifecycle verbs itself (their remote argv
// comes from the parsed command, flags included, which a bare PowerStop
// cannot carry) and cp streams a tar over the proxy (cli's cp_hub.go).
//
// What never changes: Spawn refuses. The hub's own daemon holds the one
// agent exec into the VM (the one-exec rule), so the laptop must never
// spawn a second one; laptop forwards ride a `__session` on the hub
// (roadmap step 6), not an exec of their own.
type hubBackend struct {
	m   *config.Machine // the hub-machine record (Name is HUB/NAME, Hub the hub conf)
	hub *sshBackend     // the ssh hop to the hub; the proxy runs commands over it
}

// ErrHubProxy is returned by hub-machine ops that are not implemented yet:
// laptop-side forwards (ports) land in step 6 and auth in step 8, so the
// wording names no mechanism. Copy returns it for good (see there).
var ErrHubProxy = errors.New("hub machines are not supported by this command yet")

// Proxier is the hub-machine backend's proxy surface, which the cli's
// dispatch uses without knowing the ssh details. argv is the devvm command
// line to run on the hub (`stop web`, `exec web -- …`), already rebuilt with
// the hub-side machine name.
type Proxier interface {
	// Proxy runs `devvm argv…` on the hub and waits. o.TTY decides ssh -t;
	// o.Stdin/Stdout/Stderr are wired through as for any Run.
	Proxy(ctx context.Context, o ExecOpts, argv ...string) error
	// ProxyInteractive runs the same with a terminal, for attach and shell.
	// transport is the hub conf's; see hubBackend.ProxyInteractive for why
	// only ssh is used today.
	ProxyInteractive(transport string, argv ...string) error
}

// NoSubscribeEnv is set on every proxied command line (roadmap shared
// decision 3): a proxied attach/shell/auth must hold the hub daemon but
// never subscribe to its browser events, because the laptop subscribes
// through its own relay instead (hub.md §8). It is exported as the
// contract's name; nothing reads it before step 5's session client.
const NoSubscribeEnv = "DEVVM_NO_SUBSCRIBE"

// ProxyArgv is the hub-side command line for a proxied devvm argv:
// `env DEVVM_NO_SUBSCRIBE=1 devvm argv…`. `env` rather than a bare
// `VAR=1 devvm` prefix because shellJoin single-quotes every token and a
// quoted `'VAR=1'` is a command name, not an assignment, to every POSIX
// shell; `env` reads the same either way and works under fish too.
func ProxyArgv(argv ...string) []string {
	return append([]string{"env", NoSubscribeEnv + "=1", "devvm"}, argv...)
}

// Proxy implements Proxier over the hub's ssh hop: the login-shell wrapper
// (LoginShellArgv) around ProxyArgv. Everything else about the ssh call —
// ControlMaster, ConnectTimeout, known_hosts posture, -t on o.TTY,
// BatchMode — is sshBackend.Run's, so there is one ssh argv builder. A -t
// run is also Quiet: with a pty, ssh prints "Shared connection to HOST
// closed." after the remote command exits on the ControlMaster, which is
// noise under every proxied stop/create/delete; BatchMode runs never show
// it and stay as they are.
func (b *hubBackend) Proxy(ctx context.Context, o ExecOpts, argv ...string) error {
	o.Quiet = o.TTY
	return b.hub.Run(ctx, o, LoginShellArgv(ProxyArgv(argv...)...)...)
}

// ProxyInteractive runs a proxied leaf with a terminal: plain `ssh -t` on
// the hub's ControlMaster, whatever the hub conf's transport says. mosh is
// not used for this hop yet: the wrapped command carries quotes and `$`
// that would have to survive mosh's own re-quoting of the remote command
// on the way to mosh-server, which today's mosh path never exercises (it
// only ever sends `tmux new-session -A -s dev`). A conf that asks for mosh
// gets a one-line notice rather than a silent downgrade.
func (b *hubBackend) ProxyInteractive(transport string, argv ...string) error {
	if transport == config.TransportMosh {
		fmt.Fprintf(os.Stderr, "devvm: hub '%s' is configured for mosh, but proxied sessions use ssh for now\n", b.m.Hub.Name)
	}
	return b.Proxy(context.Background(), ExecOpts{TTY: true}, argv...)
}

func (b *hubBackend) Kind() string { return config.BackendHub }

// Exists is true: resolve never asks the hub whether a machine exists (the
// proxied command reports that itself), and the leaves that would act on
// the answer are refused for hub machines before they ask.
func (b *hubBackend) Exists() (bool, error) { return true, nil }

// Status reports what the laptop knows on its own, which is nothing about the
// VM's state: the merged listing (cli's hubRows) takes state and backend from
// the hub's `status --plain --local` row instead, and `status HUB/NAME`
// goes there rather than here.
func (b *hubBackend) Status() (State, error) {
	return State{
		Name:    b.m.Name,
		Backend: config.BackendHub,
		Exists:  true,
		Raw:     fmt.Sprintf("on hub %s (%s); see 'devvm status'", b.m.Hub.Name, b.m.Hub.SSHHost),
	}, nil
}

// refused names an op the Backend interface has but a hub machine does not
// serve through it (see the type comment for which ops proxy instead).
func (b *hubBackend) refused(verb string) error {
	return fmt.Errorf("%s %s: %w", verb, b.m.Name, ErrHubProxy)
}

// The lifecycle verbs are proxied by the cli from the parsed command, not
// through these: `deprovision --yes` and friends carry flags a Power* call
// cannot, and the hub's own confirm prompts run through the forwarded tty.
func (b *hubBackend) PowerStart() error  { return b.refused("start") }
func (b *hubBackend) PowerStop() error   { return b.refused("stop") }
func (b *hubBackend) PowerDelete() error { return b.refused("delete") }

// Copy is never used for hub machines: cp streams a tar over the proxy
// (cli's cp_hub.go, hub.md §6) because the hub's `cp-in` needs the archive,
// the destination and the flags, not a path pair on the hub, so the
// dispatch lives in the cli's cp leaves rather than behind this interface.
func (b *hubBackend) Copy(hostSrc, guestDst string) error {
	return fmt.Errorf("copy to %s: %w", b.m.Name, ErrHubProxy)
}

// Run is a proxied `devvm exec NAME -- argv…` (hub.md §4). The hub's exec
// already runs a login shell as the dev user, so Login is implied; root is
// asked for with a guest-side sudo, since User would otherwise sudo the
// hub-side ssh command instead. o.Env would likewise land on the hub
// process, not in the guest, so it is refused rather than misapplied. A
// smol one-shot exec has no stdin (no -i), so callers that need it use the
// tar path (cli's cp_hub.go), not Run.
func (b *hubBackend) Run(ctx context.Context, o ExecOpts, argv ...string) error {
	if len(o.Env) > 0 {
		return fmt.Errorf("exec on %s: guest env is not carried through the proxy (put it in argv)", b.m.Name)
	}
	if o.User == "root" {
		argv = append([]string{"sudo"}, argv...)
	}
	o.User, o.Login = "", false
	remote := append([]string{"exec", b.m.HubMachineName(), "--"}, argv...)
	return b.Proxy(ctx, o, remote...)
}

// Spawn always refuses; see the type comment.
func (b *hubBackend) Spawn(ctx context.Context, o ExecOpts, argv ...string) (*Session, error) {
	return nil, fmt.Errorf("%s: a hub machine never gets a laptop-owned agent exec (the hub's daemon holds the only one)", b.m.Name)
}

// Shell and Attach proxy the same leaf on the hub with a terminal
// (ProxyInteractive); the hub's own attach does the tmux work.
func (b *hubBackend) Shell(transport string) error {
	return b.ProxyInteractive(transport, "shell", b.m.HubMachineName())
}

func (b *hubBackend) Attach(transport string) error {
	return b.ProxyInteractive(transport, "attach", b.m.HubMachineName())
}

// LoginShellArgv wraps argv so it runs under the remote user's *login* shell:
// `sh -c 'exec "$SHELL" -lc "$0"' 'ARGV…'`. A plain `ssh HUB devvm …` runs in
// a non-login shell where a Homebrew or ~/.local/bin devvm is usually not on
// PATH; on the hubs only the login shell has it. The joined argv rides as $0
// of the sh script, so it is quoted once (by remoteCommand) and no quoting
// has to survive two shells. $SHELL rather than a hardcoded bash because it
// is the user's shell (sshd sets it) whose profile put devvm on PATH.
func LoginShellArgv(argv ...string) []string {
	return []string{"sh", "-c", `exec "${SHELL:-sh}" -lc "$0"`, shellJoin(argv)}
}
