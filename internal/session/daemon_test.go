package session

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/agentrpc"
	"github.com/smweber/devvm/internal/config"
)

// fakeTransport binds real host listeners (so bind conflicts drive the bump)
// and forwards to a real guest port, standing in for smol/ssh in unit tests.
type fakeTransport struct {
	dc        chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
	binds     atomic.Int32 // forwards bound on this transport instance
}

// forward uses the real transports' listenLoopback, so the dual-stack rule
// (IPv4 decides busy, ::1 best-effort) is what the tests exercise.
func (f *fakeTransport) forward(hostPort, guestPort int) (io.Closer, error) {
	lns, err := listenLoopback(hostPort)
	if err != nil {
		return nil, err
	}
	for _, ln := range lns {
		go func(ln net.Listener) {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					up, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", guestPort))
					if err != nil {
						c.Close()
						return
					}
					agentrpc.Splice(c, up)
				}(c)
			}
		}(ln)
	}
	f.binds.Add(1)
	return listeners(lns), nil
}

func (f *fakeTransport) dead() <-chan struct{} { return f.dc }
func (f *fakeTransport) Close() error {
	f.closed.Store(true)
	return nil
}

// die simulates the link dropping (idempotent, like the real transports).
func (f *fakeTransport) die() { f.closeOnce.Do(func() { close(f.dc) }) }

func newFakeTransport() *fakeTransport { return &fakeTransport{dc: make(chan struct{})} }

func newTestDaemon(t *testing.T) *daemon {
	t.Helper()
	d := newDaemon(t.TempDir(), "t", "test", newFakeTransport(), func() (transport, error) {
		return nil, errors.New("no dial in this test")
	})
	d.logf = t.Logf
	d.minBackoff, d.maxBackoff = 5*time.Millisecond, 20*time.Millisecond
	return d
}

// runLoop runs the supervision loop in the background and returns a channel
// that closes when it exits.
func runLoop(d *daemon) <-chan struct{} {
	done := make(chan struct{})
	go func() { d.loop(); close(done) }()
	return done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("loop did not exit after %s", what)
	}
}

func portOpen(port int) bool {
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// freePort returns a currently-free loopback port (bind :0, read it, release).
// add() needs a concrete preferred port to report back; 0 is not a valid pref.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

func echoServer(t *testing.T) (addr string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { io.Copy(c, c); c.Close() }(c)
		}
	}()
	return ln.Addr().String(), ln.Addr().(*net.TCPAddr).Port
}

func TestDaemonAddRemoveList(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)

	pref := freePort(t)
	host, bumped, _, err := d.add(pref, guest, false, confOwner)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if bumped || host != pref {
		t.Errorf("free pref %d should bind as-is, got host=%d bumped=%v", pref, host, bumped)
	}

	// Data must round-trip through the forward.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", host))
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	io.WriteString(conn, "hi\n")
	buf := make([]byte, 3)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hi\n" {
		t.Fatalf("echo = %q err=%v", buf, err)
	}
	conn.Close()

	if fs := d.list(); len(fs) != 1 || fs[0].Guest != guest {
		t.Fatalf("list = %v", fs)
	}

	// Adding the same guest again is idempotent (same host port).
	host2, _, _, _ := d.add(host, guest, false, confOwner)
	if host2 != host {
		t.Errorf("re-add host = %d, want %d", host2, host)
	}

	d.remove(guest, confOwner)
	if fs := d.list(); len(fs) != 0 {
		t.Fatalf("after remove list = %v", fs)
	}
}

func TestDaemonPortBump(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)

	// Occupy a host port to force a bump.
	occ, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()
	pref := occ.Addr().(*net.TCPAddr).Port

	host, bumped, _, err := d.add(pref, guest, false, confOwner)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !bumped {
		t.Errorf("expected bump when preferred port %d is taken", pref)
	}
	if host == pref {
		t.Errorf("host %d should differ from taken pref %d", host, pref)
	}
	if host <= pref || host > pref+20 {
		t.Errorf("bumped host %d out of range (%d, %d]", host, pref, pref+20)
	}
}

func TestDaemonDispatch(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)

	if r := d.dispatch(Request{Op: OpPing}); !r.OK {
		t.Errorf("ping not ok: %+v", r)
	}
	if r := d.dispatch(Request{Op: OpAdd, Host: freePort(t), Guest: guest}); !r.OK {
		t.Errorf("add not ok: %+v", r)
	}
	if r := d.dispatch(Request{Op: OpList}); !r.OK || len(r.Forwards) != 1 || r.State != StateUp || r.Since.IsZero() {
		t.Errorf("list wrong: %+v", r)
	}
	if r := d.dispatch(Request{Op: OpRemove, Guest: guest}); !r.OK {
		t.Errorf("remove not ok: %+v", r)
	}
	if r := d.dispatch(Request{Op: "bogus"}); r.OK || r.Err == "" {
		t.Errorf("bogus op should error: %+v", r)
	}
}

func TestDaemonReconnectRestoresForwards(t *testing.T) {
	_, guest := echoServer(t)
	_, guest2 := echoServer(t)
	d := newTestDaemon(t)
	first := d.tr.(*fakeTransport)

	// Fail twice, then hand back a fresh transport.
	var attempts atomic.Int32
	second := newFakeTransport()
	d.dial = func() (transport, error) {
		if attempts.Add(1) <= 2 {
			return nil, errors.New("link still down")
		}
		return second, nil
	}

	pref, pref2 := freePort(t), freePort(t)
	host, _, _, err := d.add(pref, guest, false, confOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.add(pref2, guest2, false, confOwner); err != nil {
		t.Fatal(err)
	}
	done := runLoop(d)

	first.die()
	// onDead marks the state under the lock and then tears the forwards and
	// transport down outside it, so "reconnecting" is visible a moment before
	// the closes land; wait for the whole picture, not the first sign of it.
	waitFor(t, "outage torn down", func() bool {
		r := d.dispatch(Request{Op: OpList})
		if r.State != StateReconnecting || len(r.Forwards) != 2 || !first.closed.Load() {
			return false
		}
		for _, f := range r.Forwards {
			if !f.Pending {
				return false
			}
		}
		return true
	})

	waitFor(t, "state back up", func() bool { s, _ := d.status(); return s == StateUp })
	if got := attempts.Load(); got != 3 {
		t.Errorf("dial attempts = %d, want 3", got)
	}
	if second.binds.Load() != 2 {
		t.Errorf("forwards bound on new transport = %d, want 2", second.binds.Load())
	}
	byGuest := map[int]Forward{}
	for _, f := range d.list() {
		byGuest[f.Guest] = f
	}
	if len(byGuest) != 2 || byGuest[guest].Host != host || byGuest[guest].Pending || byGuest[guest2].Host != pref2 {
		t.Fatalf("restored forwards = %+v (want same host ports %d, %d)", byGuest, host, pref2)
	}
	// Traffic flows through the restored forward.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", host))
	if err != nil {
		t.Fatalf("dial restored forward: %v", err)
	}
	io.WriteString(conn, "hi\n")
	buf := make([]byte, 3)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hi\n" {
		t.Fatalf("echo = %q err=%v", buf, err)
	}
	conn.Close()

	d.triggerStop()
	waitDone(t, done, "stop")
}

func TestDaemonReconnectBumpsTakenPort(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	first := d.tr.(*fakeTransport)
	pref := freePort(t)
	if _, _, _, err := d.add(pref, guest, false, confOwner); err != nil {
		t.Fatal(err)
	}
	// Steal the host port while the link is down.
	var occ net.Listener
	d.dial = func() (transport, error) {
		if occ == nil {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", pref))
			if err != nil {
				return nil, err
			}
			occ = ln
		}
		return newFakeTransport(), nil
	}
	done := runLoop(d)
	first.die()
	waitFor(t, "reconnecting state", func() bool { s, _ := d.status(); return s == StateReconnecting })
	waitFor(t, "state back up", func() bool { s, _ := d.status(); return s == StateUp })
	defer occ.Close()
	fs := d.list()
	if len(fs) != 1 || fs[0].Host == pref || fs[0].Host <= pref || fs[0].Host > pref+20 {
		t.Fatalf("restored forwards = %+v, want a bumped host port above %d", fs, pref)
	}
	d.triggerStop()
	waitDone(t, done, "stop")
}

func TestDaemonAddWhileReconnecting(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	first := d.tr.(*fakeTransport)
	release := make(chan struct{})
	second := newFakeTransport()
	d.dial = func() (transport, error) {
		select {
		case <-release:
			return second, nil
		default:
			return nil, errors.New("down")
		}
	}
	done := runLoop(d)
	first.die()
	waitFor(t, "reconnecting state", func() bool { s, _ := d.status(); return s == StateReconnecting })

	pref := freePort(t)
	host, bumped, pending, err := d.add(pref, guest, false, confOwner)
	if err != nil || host != pref || bumped || !pending {
		t.Fatalf("add during outage = host %d bumped %v pending %v err %v", host, bumped, pending, err)
	}
	if portOpen(pref) {
		t.Fatal("pending forward must not be bound yet")
	}
	// Re-adding the same guest during the outage is idempotent and still pending.
	if _, _, pending, _ := d.add(pref, guest, false, confOwner); !pending {
		t.Error("re-add during outage should report pending")
	}

	close(release)
	waitFor(t, "state back up", func() bool { s, _ := d.status(); return s == StateUp })
	waitFor(t, "pending forward bound", func() bool { return portOpen(pref) })
	if fs := d.list(); len(fs) != 1 || fs[0].Pending {
		t.Fatalf("after reconnect list = %+v", fs)
	}
	d.triggerStop()
	waitDone(t, done, "stop")
}

func TestDaemonStopWhileReconnecting(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	d.minBackoff, d.maxBackoff = time.Hour, time.Hour // stop must not wait out a backoff
	first := d.tr.(*fakeTransport)
	if _, _, _, err := d.add(freePort(t), guest, false, confOwner); err != nil {
		t.Fatal(err)
	}
	done := runLoop(d)
	first.die()
	waitFor(t, "reconnecting state", func() bool { s, _ := d.status(); return s == StateReconnecting })
	d.triggerStop()
	waitDone(t, done, "stop during reconnect")
}

func TestDaemonReconnectIdlesOutWithNoForwards(t *testing.T) {
	d := newTestDaemon(t)
	d.idle = 30 * time.Millisecond
	first := d.tr.(*fakeTransport)
	done := runLoop(d)
	first.die()
	waitDone(t, done, "idle with nothing to restore")
}

func TestDaemonRemoveWhileReconnecting(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	first := d.tr.(*fakeTransport)
	if _, _, _, err := d.add(freePort(t), guest, false, confOwner); err != nil {
		t.Fatal(err)
	}
	done := runLoop(d)
	first.die()
	waitFor(t, "reconnecting state", func() bool { s, _ := d.status(); return s == StateReconnecting })
	d.remove(guest, confOwner) // closer is nil; must not panic
	if fs := d.list(); len(fs) != 0 {
		t.Fatalf("list after remove = %+v", fs)
	}
	d.triggerStop()
	waitDone(t, done, "stop")
}

// failingTransport binds nothing: every forward fails with a non-busy error,
// like `ssh -O forward` against a master that came up degraded.
type failingTransport struct {
	*fakeTransport
	fail atomic.Bool
}

func (f *failingTransport) forward(hostPort, guestPort int) (io.Closer, error) {
	if f.fail.Load() {
		return nil, errors.New("ssh -O forward: exit 255")
	}
	return f.fakeTransport.forward(hostPort, guestPort)
}

func TestDaemonRestoreFailureKeepsForwardsPendingAndRetries(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	first := d.tr.(*fakeTransport)
	pref := freePort(t)
	if _, _, _, err := d.add(pref, guest, false, confOwner); err != nil {
		t.Fatal(err)
	}
	// First dial: a transport whose binds fail. Second: a healthy one.
	bad := &failingTransport{fakeTransport: newFakeTransport()}
	bad.fail.Store(true)
	good := newFakeTransport()
	var dials atomic.Int32
	d.dial = func() (transport, error) {
		if dials.Add(1) == 1 {
			return bad, nil
		}
		return good, nil
	}
	done := runLoop(d)
	first.die()
	waitFor(t, "second dial", func() bool { return dials.Load() >= 2 })
	waitFor(t, "state back up", func() bool { s, _ := d.status(); return s == StateUp })
	if !bad.closed.Load() {
		t.Error("the transport that could not carry forwards was not closed")
	}
	fs := d.list()
	if len(fs) != 1 || fs[0].Pending || fs[0].Host != pref {
		t.Fatalf("forward after retry = %+v, want bound on %d", fs, pref)
	}
	if !portOpen(pref) {
		t.Error("restored forward is not listening")
	}
	d.triggerStop()
	waitDone(t, done, "stop")
}

func TestDaemonKickRetriesImmediately(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	d.minBackoff, d.maxBackoff = time.Hour, time.Hour
	first := d.tr.(*fakeTransport)
	if _, _, _, err := d.add(freePort(t), guest, false, confOwner); err != nil {
		t.Fatal(err)
	}
	second := newFakeTransport()
	var dials atomic.Int32
	d.dial = func() (transport, error) { dials.Add(1); return second, nil }
	done := runLoop(d)
	first.die()
	waitFor(t, "reconnecting state", func() bool { s, _ := d.status(); return s == StateReconnecting })
	if dials.Load() != 0 {
		t.Fatalf("dialed %d times before the backoff elapsed", dials.Load())
	}
	if r := d.dispatch(Request{Op: OpKick}); !r.OK {
		t.Fatalf("kick = %+v", r)
	}
	waitFor(t, "state back up after kick", func() bool { s, _ := d.status(); return s == StateUp })
	d.triggerStop()
	waitDone(t, done, "stop")
}

func TestDaemonStopInterruptsHangingDial(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	first := d.tr.(*fakeTransport)
	if _, _, _, err := d.add(freePort(t), guest, false, confOwner); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	late := newFakeTransport()
	d.dial = func() (transport, error) { <-release; return late, nil }
	done := runLoop(d)
	first.die()
	waitFor(t, "reconnecting state", func() bool { s, _ := d.status(); return s == StateReconnecting })
	time.Sleep(3 * d.minBackoff) // let the dial start and hang
	d.triggerStop()
	waitDone(t, done, "stop during a hanging dial")
	close(release)
	waitFor(t, "late transport closed", func() bool { return late.closed.Load() })
}

func TestDaemonVMNotRunningWaitsWithoutFailing(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	first := d.tr.(*fakeTransport)
	if _, _, _, err := d.add(freePort(t), guest, false, confOwner); err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	second := newFakeTransport()
	d.dial = func() (transport, error) {
		if dials.Add(1) < 3 {
			return nil, errVMNotRunning
		}
		return second, nil
	}
	done := runLoop(d)
	first.die()
	waitFor(t, "state back up once the VM runs", func() bool { s, _ := d.status(); return s == StateUp })
	d.triggerStop()
	waitDone(t, done, "stop")
}

func TestDaemonPingReportsVersion(t *testing.T) {
	d := newTestDaemon(t)
	if r := d.dispatch(Request{Op: OpPing}); r.Version != "test" || r.State != StateUp {
		t.Fatalf("ping = %+v", r)
	}
	if r := d.dispatch(Request{Op: OpList}); r.Version != "test" {
		t.Fatalf("list = %+v", r)
	}
}

// orderedTransport records when Close ran so the shutdown-ordering test can
// prove the socket outlived the transport.
type orderedTransport struct {
	*fakeTransport
	sockAtClose bool // socket still present when Close ran
	sock        string
}

func (o *orderedTransport) Close() error {
	_, err := os.Stat(o.sock)
	o.sockAtClose = err == nil
	return o.fakeTransport.Close()
}

func TestDaemonShutdownUnlinksSocketAfterTransport(t *testing.T) {
	dir := shortTempDir(t)
	if err := os.MkdirAll(config.RuntimeDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := socketPath(dir, "t")
	tr := &orderedTransport{fakeTransport: newFakeTransport(), sock: sock}
	d := newDaemon(dir, "t", "test", tr, nil)
	d.logf = t.Logf
	ln, info, err := listenControl(sock)
	if err != nil {
		t.Fatal(err)
	}
	d.ln, d.sockInfo = ln, info
	go d.serveControl()
	if err := d.shutdown(); err != nil {
		t.Fatal(err)
	}
	if !tr.closed.Load() {
		t.Fatal("transport not closed")
	}
	if !tr.sockAtClose {
		t.Error("socket was unlinked before the transport closed; WaitGone would race the old master")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket still present after shutdown: %v", err)
	}
}

// A replacement daemon that took the socket path while this one was still
// tearing down must not have its socket unlinked by our shutdown.
func TestDaemonShutdownLeavesSuccessorSocket(t *testing.T) {
	dir := shortTempDir(t)
	if err := os.MkdirAll(config.RuntimeDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := socketPath(dir, "t")
	ln, info, err := listenControl(sock)
	if err != nil {
		t.Fatal(err)
	}
	// Stand in for the successor: it clears our unanswered socket and listens
	// on its own, between our ln.Close and our unlink. Simulated up front,
	// since the ordering within shutdown is what's under test.
	ln.Close()
	os.Remove(sock)
	successor, _, err := listenControl(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.Close()
	d := newDaemon(dir, "t", "test", newFakeTransport(), nil)
	d.logf = t.Logf
	d.ln, d.sockInfo = ln, info
	if err := d.shutdown(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("successor's socket was unlinked: %v", err)
	}
}

// A transport that arrives after stop interrupted its dial must be closed
// before shutdown unlinks the socket, or the process exits with an orphaned
// exec/master that the next daemon then runs in parallel with.
func TestDaemonShutdownWaitsForLateDial(t *testing.T) {
	dir := shortTempDir(t)
	if err := os.MkdirAll(config.RuntimeDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	_, guest := echoServer(t)
	sock := socketPath(dir, "t")
	first := newFakeTransport()
	release := make(chan struct{})
	late := newFakeTransport()
	d := newDaemon(dir, "t", "test", first, func() (transport, error) { <-release; return late, nil })
	d.logf = t.Logf
	d.minBackoff, d.maxBackoff = 5*time.Millisecond, 20*time.Millisecond
	ln, info, err := listenControl(sock)
	if err != nil {
		t.Fatal(err)
	}
	d.ln, d.sockInfo = ln, info
	if _, _, _, err := d.add(freePort(t), guest, false, confOwner); err != nil {
		t.Fatal(err)
	}
	done := runLoop(d)
	first.die()
	waitFor(t, "reconnecting state", func() bool { s, _ := d.status(); return s == StateReconnecting })
	time.Sleep(3 * d.minBackoff) // the dial is now hanging on release
	d.triggerStop()
	waitDone(t, done, "stop")
	shut := make(chan struct{})
	go func() { d.shutdown(); close(shut) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-shut:
		t.Fatal("shutdown finished while the late dial was still open")
	default:
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatal("socket unlinked before the late transport was closed")
	}
	close(release)
	select {
	case <-shut:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after the late dial returned")
	}
	if !late.closed.Load() {
		t.Error("late transport not closed")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket still present after shutdown: %v", err)
	}
}

// exhaustingTransport refuses every bind until released, standing in for a
// port range that was fully taken during restore.
type exhaustingTransport struct {
	*fakeTransport
	busy atomic.Bool
}

func (e *exhaustingTransport) forward(hostPort, guestPort int) (io.Closer, error) {
	if e.busy.Load() {
		return nil, errPortBusy
	}
	return e.fakeTransport.forward(hostPort, guestPort)
}

// A forward left pending by port exhaustion during restore is re-bound by the
// next add (what `ports up` does), not reported pending forever.
func TestDaemonAddRebindsForwardPendingAfterExhaustion(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	first := d.tr.(*fakeTransport)
	pref := freePort(t)
	if _, _, _, err := d.add(pref, guest, false, confOwner); err != nil {
		t.Fatal(err)
	}
	second := &exhaustingTransport{fakeTransport: newFakeTransport()}
	second.busy.Store(true)
	d.dial = func() (transport, error) { return second, nil }
	done := runLoop(d)
	first.die()
	waitFor(t, "state up with the forward pending", func() bool {
		s, _ := d.status()
		fs := d.list()
		return s == StateUp && len(fs) == 1 && fs[0].Pending
	})
	second.busy.Store(false)
	host, _, pending, err := d.add(pref, guest, false, confOwner)
	if err != nil || pending || host != pref {
		t.Fatalf("add after exhaustion = host %d pending %v err %v; want bound on %d", host, pending, err, pref)
	}
	if !portOpen(pref) {
		t.Error("re-bound forward is not listening")
	}
	d.triggerStop()
	waitDone(t, done, "stop")
}

// add binds outside the lock; a list issued while a bind is blocked must not
// wait for it.
func TestDaemonListNotBlockedByBind(t *testing.T) {
	d := newTestDaemon(t)
	release := make(chan struct{})
	d.tr = &blockingTransport{fakeTransport: newFakeTransport(), release: release}
	added := make(chan struct{})
	go func() { d.add(freePort(t), 1, false, confOwner); close(added) }()
	time.Sleep(20 * time.Millisecond)
	listed := make(chan struct{})
	go func() { d.list(); d.status(); close(listed) }()
	select {
	case <-listed:
	case <-time.After(2 * time.Second):
		t.Fatal("list blocked behind a bind in flight")
	}
	close(release)
	<-added
}

type blockingTransport struct {
	*fakeTransport
	release chan struct{}
}

func (b *blockingTransport) forward(hostPort, guestPort int) (io.Closer, error) {
	<-b.release
	return nil, errors.New("bind failed after the wait")
}

// shutdown tears the transport down (d.tr = nil) without moving the state to
// reconnecting; a control request already in flight must land as pending,
// not dereference a nil transport.
func TestDaemonAddAfterTeardownIsPending(t *testing.T) {
	d := newTestDaemon(t)
	d.teardown()
	host, bumped, pending, err := d.add(freePort(t), 7, false, confOwner)
	if err != nil || !pending || bumped || host == 0 {
		t.Fatalf("add after teardown = host %d, bumped %v, pending %v, err %v; want pending", host, bumped, pending, err)
	}
	if f := d.forwards[7]; f == nil || f.closer != nil {
		t.Fatalf("forward not recorded as pending: %+v", f)
	}
}

// releaseTransport binds for real once released, so two adds racing for the
// same guest can be caught in the act.
type releaseTransport struct {
	*fakeTransport
	release chan struct{}
}

func (r *releaseTransport) forward(hostPort, guestPort int) (io.Closer, error) {
	<-r.release
	return r.fakeTransport.forward(hostPort, guestPort)
}

// Two concurrent adds for one guest must bind once: on ssh both closers would
// carry the same -L spec and the loser's cancel would kill the winner.
func TestDaemonConcurrentAddBindsOnce(t *testing.T) {
	d := newTestDaemon(t)
	tr := &releaseTransport{fakeTransport: newFakeTransport(), release: make(chan struct{})}
	d.tr = tr
	pref := freePort(t)
	type res struct {
		host    int
		pending bool
		err     error
	}
	results := make(chan res, 2)
	for i := 0; i < 2; i++ {
		go func() {
			h, _, p, err := d.add(pref, 1, false, confOwner)
			results <- res{h, p, err}
		}()
	}
	time.Sleep(20 * time.Millisecond) // let both reach the claim
	close(tr.release)
	var got []res
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			got = append(got, r)
		case <-time.After(2 * time.Second):
			t.Fatal("add did not return")
		}
	}
	if n := tr.binds.Load(); n != 1 {
		t.Fatalf("forward bound %d times, want 1", n)
	}
	for _, r := range got {
		if r.err != nil || r.host != pref {
			t.Fatalf("add = %+v, want host %d", r, pref)
		}
	}
	if f := d.forwards[1]; f == nil || f.closer == nil || f.binding {
		t.Fatalf("forward not settled: %+v", f)
	}
}

// shortTempDir is t.TempDir() moved under /tmp: macOS caps a unix socket path
// at 104 bytes and the default temp root (/var/folders/…/T/TestName…) blows
// it ("bind: invalid argument"). Resolved through EvalSymlinks because /tmp
// and /var are symlinks on macOS and tests compare paths.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "devvm-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir
}
