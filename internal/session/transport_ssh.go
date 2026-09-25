package session

import (
	"context"
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

// controlTimeout bounds every `ssh -O` against the master. A wedged master
// (socket present, process not answering — the usual post-sleep state) would
// otherwise hang each cancel/exit/check for good.
const controlTimeout = 5 * time.Second

func newSSHTransport(conn backend.SSHConn) (*sshTransport, error) {
	if _, err := exec.LookPath("ssh"); err != nil {
		return nil, fmt.Errorf("ssh is not installed on this host")
	}
	if err := os.MkdirAll(filepath.Dir(conn.ControlPath), 0o700); err != nil {
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
	// A master that died without unlinking its socket (killed, or a link drop
	// mid-cleanup) would make `-M` fall back to a plain connection with
	// "ControlSocket already exists, disabling multiplexing" — and the daemon's
	// reconnect would then find no master and spin. Only a socket nobody
	// answers is removed.
	if _, err := os.Stat(t.conn.ControlPath); err == nil && !t.masterAlive() {
		_ = os.Remove(t.conn.ControlPath)
	}
	args := append([]string{}, t.conn.Flags...)
	args = append(args,
		"-M", "-N", "-f",
		"-o", "ControlPath="+t.conn.ControlPath,
		"-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=3",
		"-o", "ExitOnForwardFailure=no",
		"-o", "BatchMode=yes", // detached: never wait on a password prompt
		t.conn.Host)
	cmd := exec.Command("ssh", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil { // -f backgrounds after auth; Run returns once it forks
		return err
	}
	// `-M` exits 0 even when it degraded to a plain connection ("ControlSocket
	// already exists, disabling multiplexing"). Without a master every `-O
	// forward` would fail; surface that as an ordinary dial error instead.
	if !t.masterAlive() {
		return fmt.Errorf("ssh master for %s did not take %s", t.conn.Host, t.conn.ControlPath)
	}
	return nil
}

// control runs an `ssh -O <op>` against the master, bounded by controlTimeout.
// ssh's stderr reaches the daemon log for forward and cancel, where it is
// the diagnosis (a remote refusal, a port in use). exit is best-effort and
// its stderr only noise: "Exit request sent." on every teardown, and
// "Control socket connect(…): No such file or directory" when the master
// is already gone. (check never passes through here: masterAlive keeps its
// stderr to itself for the same reason.)
func (t *sshTransport) control(op string, extra ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	args := []string{"-O", op, "-o", "ControlPath=" + t.conn.ControlPath}
	args = append(args, extra...)
	args = append(args, t.conn.Host)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	if op != "exit" {
		cmd.Stderr = os.Stderr
	}
	return cmd.Run()
}

func (t *sshTransport) forward(hostPort, guestPort int) (io.Closer, error) {
	// Pre-probe bindability so a conflict bumps rather than sinking the request.
	// Small race window between close and ssh binding, tolerated as the old
	// probe_free_host_port did. This IPv4 probe, not ssh's own result, is
	// what decides busy: OpenSSH reports a `localhost:` spec as bound when
	// any one of its addresses bound (browser-bridge.md §4).
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", hostPort))
	if err != nil {
		return nil, errPortBusy
	}
	ln.Close()

	// One `localhost:` spec, which OpenSSH binds on every loopback address
	// localhost resolves to (127.0.0.1 and, where the host has it, ::1,
	// best-effort), so the forward is dual-stack and there is still exactly
	// one `-O cancel` per forward.
	return t.forwardSpec(sshForwardSpec(hostPort, guestPort))
}

// forwardSpec adds one -L to the master and returns its canceller.
func (t *sshTransport) forwardSpec(spec string) (io.Closer, error) {
	if err := t.control("forward", "-L", spec); err != nil {
		return nil, fmt.Errorf("ssh -O forward %s: %w", spec, err)
	}
	return &sshForwardCloser{t: t, spec: spec}, nil
}

// forwardTo is the hub transport's -L (hubLocal): this host's port, both
// families, to 127.0.0.1:hubPort on the hub. No pre-probe here: the hub
// transport probes before it asks the hub for a port at all.
func (t *sshTransport) forwardTo(hostPort, hubPort int) (io.Closer, error) {
	return t.forwardSpec(hubForwardSpec(hostPort, hubPort))
}

// hubForwardSpec is the -L spec for a hub forward: dual-stack on this host
// like every forward, and on the hub the daemon's own listener named by
// family. 127.0.0.1, not localhost: the hub daemon binds IPv4 for sure and
// ::1 only best-effort, so a stranger on the hub's [::1]:hubPort must never
// receive this host's traffic (hub.md §7).
func hubForwardSpec(hostPort, hubPort int) string {
	return fmt.Sprintf("localhost:%d:127.0.0.1:%d", hostPort, hubPort)
}

// sshForwardSpec is the -L spec for one forward: dual-stack on the host, the
// guest's localhost (resolved by sshd in the guest) on the far side.
func sshForwardSpec(hostPort, guestPort int) string {
	return fmt.Sprintf("localhost:%d:localhost:%d", hostPort, guestPort)
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
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-O", "check",
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
