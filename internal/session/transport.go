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
	// decides busy-or-free (browser-bridge.md §4). The closer drops this one
	// forward (`ports rm`, an owner's last drop).
	forward(hostPort, guestPort int) (io.Closer, error)
	// events carries the guest's open-url requests (browser-bridge.md §1):
	// the smol agent exec's event streams, or a hub machine's events relayed
	// by its hub daemon (hubTransport, while subscribed there). nil when
	// the transport has none (ssh until its agent exec ships), which a
	// select never receives from.
	events() <-chan bridgeRequest
	dead() <-chan struct{}
	// Close drops the transport and every forward bound on it, in one go:
	// the daemon's teardown closes the transport, never its forwards one by
	// one. Each transport knows the cheap way: smol closes the listeners it
	// tracks, ssh's `-O exit` takes every -L with the master, and the hub
	// relay's close makes the hub drop every add it owned (with the -Ls on
	// the master going the ssh way). Closing N forwards one at a time
	// against a wedged link would wait out N timeouts in turn.
	Close() error
}

// subscriber is a transport that carries this daemon's subscription to a
// daemon upstream: the hub transport (roadmap step 8) subscribes on the hub
// daemon while this one has subscribers, so events from the hub's VM reach
// this host (hub.md §8). Optional; only hubTransport implements it.
//
// The daemon calls setSubscribed(true) on every local subscribe (each moves
// the subscriber to the front here, and the re-send moves this host to the
// front upstream), setSubscribed(false) when the last subscriber leaves, and
// setSubscribed(true) from restore() on a fresh transport, after its binds
// and before the daemon reports up, when it still has subscribers. Calls
// run outside d.mu, serialized, each bounded by upstreamSubscribeTimeout.
type subscriber interface {
	setSubscribed(on bool) error
}

// shimReporter is a transport that knows the guest path of the BROWSER shim
// (browser-bridge.md §5): smol's own install, or a hub machine's as its hub
// daemon reported it. ping carries it, so thin `auth` never installs
// anything itself (bridge §6). Optional; ssh has none.
type shimReporter interface {
	shimPath() string
}

// newTransport builds the right transport for a resolved machine, chosen
// by the record's predicates rather than its backend string (config.md §9
// item 1): anything reached over ssh gets native forwards, so a future
// provider backend needs no case here.
func newTransport(ctx context.Context, m *config.Machine, b backend.Backend) (transport, error) {
	switch {
	case m.IsHub():
		return nil, fmt.Errorf("%s is a hub, not a machine; forwards go to HUB/NAME", m.Name)
	case m.IsHubMachine():
		// A machine on a hub: forwards ride one `__session` on the hub, whose
		// daemon holds the only agent exec (hub.md §7).
		hs, ok := b.(backend.HubSessioner)
		if !ok {
			return nil, fmt.Errorf("%s: hub backend does not expose a session", m.Name)
		}
		return newHubTransport(m.Name, hs.SSHConn(), hs.SessionArgv())
	case m.IsRemote():
		conn, ok := b.(backend.SSHConnector)
		if !ok {
			return nil, fmt.Errorf("remote backend does not expose a connector")
		}
		return newSSHTransport(conn.SSHConn())
	default:
		return newSmolTransport(ctx, m, b)
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

// v6Reporter is a forward's closer that knows whether its [::1] side
// bound. The bridge needs it for a literal [::1] URL (bridge §4).
type v6Reporter interface {
	boundV6() bool
}

// forwardHasV6 reports whether a forward's [::1] side bound. A closer that
// cannot tell counts as dual-stack: only a known failure refuses an open.
func forwardHasV6(c io.Closer) bool {
	if r, ok := c.(v6Reporter); ok {
		return r.boundV6()
	}
	return true
}

func (ls listeners) boundV6() bool {
	for _, l := range ls {
		if a, ok := l.Addr().(*net.TCPAddr); ok && a.IP.To4() == nil {
			return true
		}
	}
	return false
}

// errTransportClosed refuses a bind on a transport that was already closed:
// its Close could no longer drop the forward.
var errTransportClosed = errors.New("transport closed")

// forwardSet is a transport's own record of the forwards it holds in this
// process (smol's listeners), so its Close drops them all. A forward's own
// closer takes it out of the set; closeAll takes whatever is left.
type forwardSet struct {
	mu     sync.Mutex
	closed bool
	open   map[*trackedForward]struct{}
}

// trackedForward is one forward in a forwardSet.
type trackedForward struct {
	set  *forwardSet
	c    io.Closer
	once sync.Once
	err  error
}

// track records c, or closes it and refuses once the set is closed.
func (s *forwardSet) track(c io.Closer) (io.Closer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return nil, errTransportClosed
	}
	if s.open == nil {
		s.open = map[*trackedForward]struct{}{}
	}
	tf := &trackedForward{set: s, c: c}
	s.open[tf] = struct{}{}
	return tf, nil
}

func (tf *trackedForward) boundV6() bool { return forwardHasV6(tf.c) }

func (tf *trackedForward) Close() error {
	tf.once.Do(func() {
		tf.set.mu.Lock()
		delete(tf.set.open, tf)
		tf.set.mu.Unlock()
		tf.err = tf.c.Close()
	})
	return tf.err
}

// closeAll closes every forward still open and refuses later ones.
func (s *forwardSet) closeAll() {
	s.mu.Lock()
	s.closed = true
	open := make([]*trackedForward, 0, len(s.open))
	for tf := range s.open {
		open = append(open, tf)
	}
	s.mu.Unlock()
	for _, tf := range open {
		tf.Close()
	}
}
