// Package backend abstracts the two transports devvm drives — local smol
// microVMs and ssh hosts — behind one interface, replacing the old bash string
// dispatch ("${BACKEND}_run", m_run, ...). A provider server (hetzner) is
// reached by the ssh implementation: only its registry differs, and that is
// the cli's provider source, not a transport.
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/smweber/devvm/internal/config"
)

// DefaultUser is the unprivileged guest user devvm runs work as.
const DefaultUser = "dev"

// State is a machine's lifecycle snapshot for `devvm status`.
type State struct {
	Name    string
	Backend string
	Exists  bool
	Running bool
	Raw     string // backend-native status line, for display
}

// ExecOpts tunes a single guest command. The zero value runs a non-interactive,
// non-login command as the dev user with stdio wired to the parent process.
type ExecOpts struct {
	BatchMode bool              // SSH: disable authentication prompts (e.g. completion) and never a pty (RequestTTY=no)
	TTY       bool              // allocate a pty (interactive shell / login flows)
	Quiet     bool              // SSH: -o LogLevel=ERROR — drop ssh's own notices ("Shared connection to HOST closed." after a -t run on a ControlMaster)
	Stream    bool              // smol: use --stream (unbuffered streaming output)
	Login     bool              // wrap argv in `bash -lc` so the user's env/PATH is present
	User      string            // guest user; "" -> DefaultUser, or "root"
	Env       map[string]string // extra environment for the guest command
	Stdin     io.Reader         // default os.Stdin
	Stdout    io.Writer         // default os.Stdout
	Stderr    io.Writer         // default os.Stderr

	// ConnectTimeout (seconds) replaces SSHConnectTimeout() for this one ssh
	// call; 0 keeps the default. The hub listing uses it: a hub that is asleep
	// must cost `status` two seconds, whatever DEVVM_SSH_CONNECT_TIMEOUT says.
	ConnectTimeout int
}

func (o ExecOpts) user() string {
	if o.User == "" {
		return DefaultUser
	}
	return o.User
}

// Backend is the transport-specific surface. Lifecycle (Power*), file copy, and
// two exec styles: Run waits with stdio wired through; Spawn returns a Session
// whose stdin/stdout pipes the caller drives (the persistent agent exec).
type Backend interface {
	Kind() string // config.BackendSmol | config.BackendRemote | config.BackendHub | config.BackendHetzner
	Exists() (bool, error)
	PowerStart() error
	PowerStop() error
	PowerDelete() error
	Status() (State, error)
	Copy(hostSrc, guestDst string) error
	Run(ctx context.Context, o ExecOpts, argv ...string) error
	Spawn(ctx context.Context, o ExecOpts, argv ...string) (*Session, error)
}

// For returns the backend implementation for a resolved machine. The ssh
// backend keeps its isolated known_hosts and ControlMaster sockets in
// paths.State.
func For(m *config.Machine, paths config.Paths) (Backend, error) {
	switch m.Backend {
	case config.BackendSmol:
		return &smolBackend{m: m, paths: paths}, nil
	case config.BackendRemote, config.BackendHetzner:
		// A provider server is a remote box in every way that matters once
		// it exists (providers.md §4): same ssh, same daemon, same bridge.
		return &sshBackend{m: m, paths: paths}, nil
	case config.BackendHub:
		if m.IsHubMachine() {
			return &hubBackend{m: m, hub: &sshBackend{m: m.Hub, paths: paths}}, nil
		}
		// The hub itself is reached exactly like a remote box (its conf carries
		// the same ssh fields); the shaping verbs refuse it before they get here.
		return &sshBackend{m: m, paths: paths}, nil
	default:
		return nil, fmt.Errorf("machine %q has unsupported backend %q", m.Name, m.Backend)
	}
}

// Refresher is a backend whose record is discovered rather than configured
// (smol): Refresh asks its discovery source again and returns a fresh copy
// of the record, Managed re-settled, and whether the box runs (m is nil
// when it no longer exists, which also reads as not running). The session
// daemon calls it at start and before every reconnect dial (config.md §2,
// §9 item 3: the re-read asks the backend, never a marker file directly).
// A backend without it (ssh, hub) always counts as running, with the
// record it was built from.
type Refresher interface {
	Refresh() (m *config.Machine, running bool, err error)
}

// ErrUnmanaged is what the backend says when asked to shape a local VM devvm
// did not create (config.md §2). The cli refuses such commands first, with
// the `setup --adopt` hint; this is the backstop.
var ErrUnmanaged = errors.New("this smol VM was not created by devvm (unmanaged)")

// RootCopier is a backend whose Copy leaves files owned by root (smol:
// `smolvm machine cp` writes into the overlay as root), so a caller that
// hands the file to the guest user must make it readable first. A
// capability, not a backend name (config.md §9 item 1).
type RootCopier interface {
	CopiesAsRoot() bool
}

// Downloader is a backend that can pull a guest file onto the host without
// streaming it over exec stdout (smol: `smolvm machine cp NAME:SRC DST`).
// smolvm caps a `machine exec`'s streamed stdout at 11 MiB, so cp-out stages
// its tar in the guest and pulls it with CopyOut instead. guestSrc is
// absolute and overlay-backed (not under a guest tmpfs: see smol's Copy);
// hostDst must not exist yet. Cancelling ctx kills the transfer. ssh and hub
// backends stream (no cap), so they do not implement it. A capability, not a
// backend name (config.md §9 item 1).
type Downloader interface {
	CopyOut(ctx context.Context, guestSrc, hostDst string) error
}

// AdoptHint is the one next step every refusal of an unmanaged smol VM
// names: the handover, or the removal route.
func AdoptHint(name string) string {
	return fmt.Sprintf("hand it over with 'devvm setup --adopt %s', or remove it with 'smolvm machine rm --name %s' and 'devvm create %s'", name, name, name)
}

// Interactive is the connect surface every backend implements. Shell opens a raw
// login shell; Attach joins the persistent dev tmux session (named "dev").
// transport selects "ssh" | "mosh" for remote backends and is ignored by smol
// (reached via smolvm exec). It steers only the interactive session — forwards
// (native ssh -L) and exec always use ssh.
type Interactive interface {
	Shell(transport string) error
	Attach(transport string) error
}

// SSHConn carries what the session daemon needs to run a dedicated ControlMaster
// and add/remove native `-L` forwards on it.
type SSHConn struct {
	Host        string   // the ssh destination
	Flags       []string // port/identity/known_hosts options for the master
	ControlPath string   // dedicated master socket for this machine's daemon
}

// MasterBinder is a backend reached over ssh whose execs can ride a given
// ControlMaster (sshBackend.OnMaster): the session daemon's ssh transport
// runs agentbin and the agent exec on its own master. A capability, not a
// backend name (config.md §9 item 1).
type MasterBinder interface {
	OnMaster(controlPath string) Backend
}

// SSHConnector is implemented by the ssh backend so the session package can
// manage native forwards without importing ssh internals.
type SSHConnector interface {
	SSHConn() SSHConn
}

// Session is a running guest exec the caller multiplexes over (yamux). Closing
// tears down the whole child process group.
type Session struct {
	cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.Reader
	cancel context.CancelFunc
}

// Wait blocks until the exec exits.
func (s *Session) Wait() error { return s.cmd.Wait() }

// Close kills the child (and its process group) and releases resources.
func (s *Session) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	killGroup(s.cmd)
	return nil
}

// stdioDefaults fills nil stdio with the parent's, so Run mirrors the old
// direct-exec behavior unless the caller captures.
func (o *ExecOpts) stdioDefaults() {
	if o.Stdin == nil {
		o.Stdin = os.Stdin
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
}

// term returns a safe TERM for interactive execs (matching the old guard that
// rewrote unknown/dumb to xterm-256color).
func term() string {
	t := os.Getenv("TERM")
	if t == "" || t == "unknown" || t == "dumb" {
		return "xterm-256color"
	}
	return t
}

func needCmd(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s is not installed on this host", name)
	}
	return nil
}
