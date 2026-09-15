package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/smweber/devvm/internal/config"
)

// hubBackend is the Backend for a HUB/NAME record: a machine that lives in
// another host's devvm registry and is reached only through that host
// (docs/proposals/hub.md §4). It implements Backend only as far as the local
// leaves need; every lifecycle and exec op is a proxied `devvm` run on the
// hub, which lands in roadmap step 2. Until then those ops return
// ErrHubProxy so a caller fails with a clear message rather than acting on
// the hub as if it were the machine.
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
// most proxy to the hub (step 2), but ports never does (laptop-side forwards
// land in step 6), so the wording names no mechanism.
var ErrHubProxy = errors.New("hub machines are not supported by this command yet")

func (b *hubBackend) Kind() string { return config.BackendHub }

// Exists is true: resolve never asks the hub whether a machine exists (the
// proxied command reports that itself), and the cached listing that will
// answer this properly is roadmap step 3.
func (b *hubBackend) Exists() (bool, error) { return true, nil }

// Status reports what the laptop knows on its own, which is nothing about the
// VM's state: the merged listing (roadmap step 3) fills state and backend
// from the hub's `status --plain --local` row.
func (b *hubBackend) Status() (State, error) {
	return State{
		Name:    b.m.Name,
		Backend: config.BackendHub,
		Exists:  true,
		Raw:     fmt.Sprintf("on hub %s (%s); state is not queried yet", b.m.Hub.Name, b.m.Hub.SSHHost),
	}, nil
}

// proxied is where roadmap step 2's a.proxy slots in: it will run
// `ssh [-t] HUB -- "$SHELL" -lc 'DEVVM_NO_SUBSCRIBE=1 devvm CMD NAME ARGS…'`
// over b.hub (see LoginShellArgv for the shell wrapper).
func (b *hubBackend) proxied(verb string) error {
	return fmt.Errorf("%s %s: %w", verb, b.m.Name, ErrHubProxy)
}

func (b *hubBackend) PowerStart() error  { return b.proxied("start") }
func (b *hubBackend) PowerStop() error   { return b.proxied("stop") }
func (b *hubBackend) PowerDelete() error { return b.proxied("delete") }

// Copy is never used for hub machines: cp streams a tar over the proxy
// (roadmap step 4) because the hub's `cp-in` needs the archive, not a path
// on the hub.
func (b *hubBackend) Copy(hostSrc, guestDst string) error {
	return fmt.Errorf("copy to %s: %w", b.m.Name, ErrHubProxy)
}

func (b *hubBackend) Run(ctx context.Context, o ExecOpts, argv ...string) error {
	return b.proxied("exec")
}

// Spawn always refuses; see the type comment.
func (b *hubBackend) Spawn(ctx context.Context, o ExecOpts, argv ...string) (*Session, error) {
	return nil, fmt.Errorf("%s: a hub machine never gets a laptop-owned agent exec (the hub's daemon holds the only one)", b.m.Name)
}

func (b *hubBackend) Shell(transport string) error  { return b.proxied("shell") }
func (b *hubBackend) Attach(transport string) error { return b.proxied("attach") }

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
