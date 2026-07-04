package session

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/smweber/devvm/internal/backend"
)

// sshTransport owns a dedicated ControlMaster for one machine and adds/removes
// native `-L` forwards on it live (ssh -O forward / -O cancel). This replaces
// autossh: resilience comes from the daemon, and native channels keep ssh's
// per-connection throughput for VNC / dev-server traffic.
type sshTransport struct {
	conn     backend.SSHConn
	deadCh   chan struct{}
	deadOnce sync.Once
	stopCh   chan struct{}
	stopOnce sync.Once
}

// checkInterval is how often the monitor probes the master (ssh -O check).
const checkInterval = 30 * time.Second

func newSSHTransport(conn backend.SSHConn) (*sshTransport, error) {
	if _, err := exec.LookPath("ssh"); err != nil {
		return nil, fmt.Errorf("ssh is not installed on this host")
	}
	if err := os.MkdirAll(filepath.Dir(conn.ControlPath), 0o755); err != nil {
		return nil, err
	}
	t := &sshTransport{conn: conn, deadCh: make(chan struct{}), stopCh: make(chan struct{})}
	if err := t.startMaster(); err != nil {
		return nil, err
	}
	go t.monitor()
	return t, nil
}

// startMaster launches a backgrounded ControlMaster (ssh -M -N -f). Keepalives
// make a dropped link kill the master; monitor() notices it's gone.
func (t *sshTransport) startMaster() error {
	args := append([]string{}, t.conn.Flags...)
	args = append(args,
		"-M", "-N", "-f",
		"-o", "ControlPath="+t.conn.ControlPath,
		"-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=3",
		"-o", "ExitOnForwardFailure=no",
		t.conn.Host)
	cmd := exec.Command("ssh", args...)
	cmd.Stderr = os.Stderr
	return cmd.Run() // -f backgrounds after auth; Run returns once it forks
}

// control runs an `ssh -O <op>` against the master.
func (t *sshTransport) control(op string, extra ...string) error {
	args := []string{"-O", op, "-o", "ControlPath=" + t.conn.ControlPath}
	args = append(args, extra...)
	args = append(args, t.conn.Host)
	cmd := exec.Command("ssh", args...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (t *sshTransport) forward(hostPort, guestPort int) (io.Closer, error) {
	// Pre-probe bindability so a conflict bumps rather than sinking the request.
	// Small race window between close and ssh binding, tolerated as the old
	// probe_free_host_port did.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort))
	if err != nil {
		return nil, errPortBusy
	}
	ln.Close()

	spec := fmt.Sprintf("127.0.0.1:%d:localhost:%d", hostPort, guestPort)
	if err := t.control("forward", "-L", spec); err != nil {
		return nil, fmt.Errorf("ssh -O forward %s: %w", spec, err)
	}
	return &sshForwardCloser{t: t, spec: spec}, nil
}

// monitor probes the master until it stops answering (link dropped, keepalives
// exhausted), then marks the transport dead so the daemon shuts down instead of
// advertising forwards that no longer exist.
func (t *sshTransport) monitor() {
	tick := time.NewTicker(checkInterval)
	defer tick.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-tick.C:
			if !t.masterAlive() {
				t.markDead()
				return
			}
		}
	}
}

// masterAlive asks the master socket directly (ssh -O check): no network
// round-trip, just "is a master still holding this ControlPath".
func (t *sshTransport) masterAlive() bool {
	cmd := exec.Command("ssh", "-O", "check",
		"-o", "ControlPath="+t.conn.ControlPath, t.conn.Host)
	return cmd.Run() == nil
}

func (t *sshTransport) markDead() {
	t.deadOnce.Do(func() { close(t.deadCh) })
}

func (t *sshTransport) dead() <-chan struct{} { return t.deadCh }

func (t *sshTransport) Close() error {
	t.stopOnce.Do(func() { close(t.stopCh) })
	// Best-effort master shutdown; ignore errors (it may already be gone).
	_ = t.control("exit")
	t.markDead()
	return nil
}

type sshForwardCloser struct {
	t    *sshTransport
	spec string
}

func (c *sshForwardCloser) Close() error {
	return c.t.control("cancel", "-L", c.spec)
}

var _ transport = (*sshTransport)(nil)
