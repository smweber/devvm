// Package backend abstracts the two transports devvm drives — local smol
// microVMs and ssh hosts — behind one interface, replacing the old bash string
// dispatch ("${BACKEND}_run", m_run, ...). A future cloud backend (hetzner)
// slots in as a third implementation reusing the ssh transport.
package backend

import (
	"context"
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
	TTY    bool              // allocate a pty (interactive shell / login flows)
	Stream bool              // smol: use --stream (unbuffered streaming output)
	Login  bool              // wrap argv in `bash -lc` so the user's env/PATH is present
	User   string            // guest user; "" -> DefaultUser, or "root"
	Env    map[string]string // extra environment for the guest command
	Stdin  io.Reader         // default os.Stdin
	Stdout io.Writer         // default os.Stdout
	Stderr io.Writer         // default os.Stderr
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
	Kind() string // config.BackendSmol | config.BackendRemote{Managed,Unmanaged}
	Exists() (bool, error)
	PowerStart() error
	PowerStop() error
	PowerDelete() error
	Status() (State, error)
	Copy(hostSrc, guestDst string) error
	Run(ctx context.Context, o ExecOpts, argv ...string) error
	Spawn(ctx context.Context, o ExecOpts, argv ...string) (*Session, error)
}

// For returns the backend implementation for a resolved machine. configDir is
// used by the ssh backend for its isolated known_hosts and ControlMaster paths.
func For(m *config.Machine, configDir string) (Backend, error) {
	switch m.Backend {
	case config.BackendSmol:
		return &smolBackend{m: m}, nil
	case config.BackendRemoteManaged, config.BackendRemoteUnmanaged:
		return &sshBackend{m: m, configDir: configDir}, nil
	default:
		return nil, fmt.Errorf("machine %q has unsupported backend %q", m.Name, m.Backend)
	}
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

// VNCer is the ssh-only viewer surface callers reach via type assertion.
type VNCer interface {
	VNC(tunnelUp func() error) error
}

// SSHConn carries what the session daemon needs to run a dedicated ControlMaster
// and add/remove native `-L` forwards on it.
type SSHConn struct {
	Host        string   // the ssh destination
	Flags       []string // port/identity/known_hosts options for the master
	ControlPath string   // dedicated master socket for this machine's daemon
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
