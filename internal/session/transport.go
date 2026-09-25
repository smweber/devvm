package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// errPortBusy signals that a host port could not be bound, so the allocator
// should bump to the next one.
var errPortBusy = errors.New("host port busy")

// transport is the backend-specific carrier for a machine's forwards. smol
// multiplexes them over the agent's yamux session; ssh adds native -L forwards
// to a dedicated ControlMaster; a hub machine relays through the hub's daemon
// (hubTransport). dead is closed when the underlying channel dies (VM stopped
// / ssh dropped / the hub's relay closed) so the daemon can reconnect.
type transport interface {
	// forward binds 127.0.0.1:hostPort, and [::1]:hostPort best-effort, and
	// carries connections to the guest's 127.0.0.1:guestPort. Returns
	// errPortBusy if the IPv4 port can't be bound; the IPv4 bind alone
	// decides busy-or-free (browser-bridge.md §4).
	forward(hostPort, guestPort int) (io.Closer, error)
	dead() <-chan struct{}
	Close() error
}

// teardownAware is a transport that wants to know its teardown has begun,
// before the daemon closes its forwards one by one (daemon.teardown).
type teardownAware interface {
	beginTeardown()
}

// newTransport builds the right transport for a resolved machine.
func newTransport(ctx context.Context, m *config.Machine, b backend.Backend) (transport, error) {
	switch m.Backend {
	case config.BackendSmol:
		return newSmolTransport(ctx, m, b)
	case config.BackendRemoteManaged, config.BackendRemoteUnmanaged:
		conn, ok := b.(backend.SSHConnector)
		if !ok {
			return nil, fmt.Errorf("remote backend does not expose a connector")
		}
		return newSSHTransport(conn.SSHConn())
	case config.BackendHub:
		// A machine on a hub: forwards ride one `__session` on the hub, whose
		// daemon holds the only agent exec (hub.md §7). A hub itself has no
		// forwards.
		hs, ok := b.(backend.HubSessioner)
		if !ok || !m.IsHubMachine() {
			return nil, fmt.Errorf("%s is a hub, not a machine; forwards go to HUB/NAME", m.Name)
		}
		return newHubTransport(m.Name, hs.SSHConn(), hs.SessionArgv())
	default:
		return nil, fmt.Errorf("no forward transport for backend %q", m.Backend)
	}
}

// transportLogf is where transports report what they tolerate (a failed
// ::1 bind); it lands in the daemon's log like the daemon's own lines.
var transportLogf = log.New(os.Stderr, "devvm-daemon: ", log.LstdFlags).Printf

// listenLoopback binds a forward's host side on both loopback families.
// 127.0.0.1 must bind (failure is errPortBusy, which bumps or, for an exact
// forward, refuses). [::1] is best-effort, as auth.ensureCallback always
// was: macOS resolves localhost to ::1 first, so a browser reaches the
// forward either way, but a host without an IPv6 loopback, or another
// process already on [::1]:port, only costs the v6 listener. Classifying a
// held ::1 as busy was considered and dropped (bridge §4): it needs errno
// inspection on two transports and a v4 unbind for a conflict the old code
// never hit.
func listenLoopback(port int) ([]net.Listener, error) {
	v4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, errPortBusy
	}
	lns := []net.Listener{v4}
	if v6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port)); err == nil {
		lns = append(lns, v6)
	} else {
		logV6Failure(port, err)
	}
	return lns, nil
}

// noV6Once keeps a host without an IPv6 loopback from logging on every
// bind: that is a property of the host, said once per daemon process.
var noV6Once sync.Once

// logV6Failure reports a tolerated [::1] bind failure: per port when
// something else holds [::1]:port (worth knowing: macOS sends localhost
// there first), once when the host has no usable ::1 at all.
func logV6Failure(port int, err error) {
	if errors.Is(err, syscall.EADDRINUSE) {
		transportLogf("host port %d: [::1]:%d is held by another process; forward is IPv4-only", port, port)
		return
	}
	noV6Once.Do(func() {
		transportLogf("no usable IPv6 loopback on this host; forwards are IPv4-only: %v", err)
	})
}

// listeners closes every listener of one forward as a unit.
type listeners []net.Listener

func (ls listeners) Close() error {
	var first error
	for _, l := range ls {
		if err := l.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
