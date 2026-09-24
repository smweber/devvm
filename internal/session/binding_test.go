package session

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/backend"
)

// gateTransport holds every forward until the test releases the gate, then
// fails (fail), reports busy (busy[port]), or binds for real. entered
// receives each port a bind is attempted on, so a test can catch a bind in
// flight. (Ported from the step 5 review.)
type gateTransport struct {
	*fakeTransport
	gate    chan struct{}
	entered chan int
	mu      sync.Mutex
	fail    bool
	busy    map[int]bool
}

func newGateTransport() *gateTransport {
	return &gateTransport{fakeTransport: newFakeTransport(), gate: make(chan struct{}), entered: make(chan int, 64), busy: map[int]bool{}}
}

func (g *gateTransport) forward(h, gp int) (io.Closer, error) {
	g.entered <- h
	<-g.gate
	g.mu.Lock()
	busy, fail := g.busy[h], g.fail
	g.mu.Unlock()
	if busy {
		return nil, errPortBusy
	}
	if fail {
		return nil, errors.New("ssh -O forward: exit 255")
	}
	return g.fakeTransport.forward(h, gp)
}

type addResult struct {
	host    int
	pending bool
	err     error
}

func addAsync(d *daemon, pref, guest int, exact bool, own owner) <-chan addResult {
	ch := make(chan addResult, 1)
	go func() {
		h, _, p, err := d.add(pref, guest, exact, own)
		ch <- addResult{h, p, err}
	}()
	return ch
}

// waitBlocked gives a second add time to reach the in-flight slot and wait
// on it; it must not return while the first bind is held.
func waitBlocked(t *testing.T, ch <-chan addResult, what string) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("%s answered before the in-flight bind finished: %+v", what, r)
	case <-time.After(30 * time.Millisecond):
	}
}

func recv(t *testing.T, ch <-chan addResult) addResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("add did not return")
		return addResult{}
	}
}

// Review 1a: a connection add reuses a slot whose conf bind then fails. It
// must not be told OK at a port nothing listens on and leave a forward
// pending for good: it waits, finds the slot gone, and binds it itself.
func TestRacingAddsFirstBindFails(t *testing.T) {
	_, guest := echoServer(t)
	g := newGateTransport()
	d := newTestDaemon(t)
	d.tr = g
	pref := freePort(t)
	first := addAsync(d, pref, guest, false, confOwner)
	<-g.entered
	second := addAsync(d, pref, guest, false, connOwner(7))
	waitBlocked(t, second, "the reusing add")
	g.mu.Lock()
	g.fail = true
	g.mu.Unlock()
	close(g.gate)
	if r := recv(t, first); r.err == nil {
		t.Fatalf("first add = %+v, want its bind error", r)
	}
	r := recv(t, second)
	fs := d.list()
	// The second add bound the slot itself, and failed the same way: an
	// error, and nothing left behind.
	if r.err == nil || len(fs) != 0 {
		t.Fatalf("second add = %+v, forwards %+v; want its own error and no forward", r, fs)
	}

	// And when the retry succeeds, the second caller's answer is the truth.
	g2 := newGateTransport()
	close(g2.gate)
	d.tr = g2
	g2.mu.Lock()
	g2.fail = false
	g2.mu.Unlock()
	if host, _, pending, err := d.add(pref, guest, false, connOwner(7)); err != nil || pending || host != pref || !portOpen(pref) {
		t.Fatalf("add on a healthy transport = %d pending=%v %v", host, pending, err)
	}
}

// Review 1b: two conf adds share a slot and the first bind fails. The
// first's rollback must not remove the conf owner the second relies on
// while the second is told OK.
func TestRacingConfAddsFirstBindFails(t *testing.T) {
	_, guest := echoServer(t)
	g := newGateTransport()
	d := newTestDaemon(t)
	d.tr = g
	pref := freePort(t)
	failOnce := &onceFailTransport{gateTransport: g}
	d.tr = failOnce
	first := addAsync(d, pref, guest, false, confOwner)
	<-g.entered
	second := addAsync(d, pref, guest, false, confOwner)
	waitBlocked(t, second, "the second conf add")
	close(g.gate)
	if r := recv(t, first); r.err == nil {
		t.Fatalf("first add = %+v, want its bind error", r)
	}
	r := recv(t, second)
	fs := d.list()
	if r.err != nil || r.pending || r.host != pref || len(fs) != 1 || fs[0].Pending || !portOpen(pref) {
		t.Fatalf("second add = %+v, forwards %+v; want it bound on %d", r, fs, pref)
	}
}

// onceFailTransport fails the first bind and binds every later one.
type onceFailTransport struct {
	*gateTransport
	mu     sync.Mutex
	failed bool
}

func (o *onceFailTransport) forward(h, gp int) (io.Closer, error) {
	o.gateTransport.entered <- h
	<-o.gateTransport.gate
	o.mu.Lock()
	first := !o.failed
	o.failed = true
	o.mu.Unlock()
	if first {
		return nil, errors.New("ssh -O forward: exit 255")
	}
	return o.gateTransport.fakeTransport.forward(h, gp)
}

// Review 1c: an exact add for P arrives while a bumpable bind for P is in
// flight. It must be answered against where that bind lands: bumped to P+1,
// so the exact request is refused, and the forward is not made exact.
func TestExactReuseDuringBumpableBind(t *testing.T) {
	_, guest := echoServer(t)
	g := newGateTransport()
	d := newTestDaemon(t)
	d.tr = g
	pref := freePort(t)
	g.busy[pref] = true
	first := addAsync(d, pref, guest, false, confOwner)
	<-g.entered
	second := addAsync(d, pref, guest, true, ttlOwner)
	waitBlocked(t, second, "the exact add")
	close(g.gate)
	r1 := recv(t, first)
	if r1.err != nil || r1.host == pref {
		t.Fatalf("bumpable add = %+v, want bumped off %d", r1, pref)
	}
	r2 := recv(t, second)
	if !errors.Is(r2.err, errPortBusy) {
		t.Fatalf("exact add = %+v, want refused (the forward is on %d)", r2, r1.host)
	}
	fs := d.list()
	if len(fs) != 1 || fs[0].Exact || fs[0].Host != r1.host || fmt.Sprint(fs[0].Owners) != "[conf]" {
		t.Fatalf("forward = %+v, want bumpable on %d with only its conf owner", fs, r1.host)
	}
}

// Review 3: a refused exact request (its port busy) must not leave the
// forward it tried to reuse exact.
func TestRefusedExactReuseLeavesForwardBumpable(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	pref := freePort(t)
	d.teardown() // no transport: the conf add is recorded pending at pref
	if _, _, pending, err := d.add(pref, guest, false, confOwner); err != nil || !pending {
		t.Fatalf("pending add = %v, %v", pending, err)
	}
	occ, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", pref))
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()
	d.mu.Lock()
	d.tr = newFakeTransport()
	d.mu.Unlock()
	if _, _, _, err := d.add(pref, guest, true, ttlOwner); !errors.Is(err, errPortBusy) {
		t.Fatalf("exact add on a held port = %v, want busy", err)
	}
	fs := d.list()
	if len(fs) != 1 || fs[0].Exact || fmt.Sprint(fs[0].Owners) != "[conf]" {
		t.Fatalf("forward after a refused exact reuse = %+v, want bumpable, conf only", fs)
	}
}

// Review 4: a bumpable forward left pending while up (exhaustion during
// restore) is re-bound by the ticker, not only by a later add.
func TestTickerRetriesBumpablePending(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	d.retryEvery = 10 * time.Millisecond
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
	waitFor(t, "up with the forward pending", func() bool {
		st, _ := d.status()
		fs := d.list()
		return st == StateUp && len(fs) == 1 && fs[0].Pending && d.transport() == second
	})
	second.busy.Store(false)
	waitFor(t, "ticker binds it", func() bool {
		fs := d.list()
		return len(fs) == 1 && !fs[0].Pending && fs[0].Host == pref
	})
	d.triggerStop()
	waitDone(t, done, "stop")
}

// Review 5: the transport dying mid-bind leaves the forward pending (restore
// or the ticker binds it), and add says pending rather than failing.
func TestAddTransportDiesMidBindIsPending(t *testing.T) {
	_, guest := echoServer(t)
	g := newGateTransport()
	d := newTestDaemon(t)
	d.tr = g
	pref := freePort(t)
	res := addAsync(d, pref, guest, false, confOwner)
	<-g.entered
	d.mu.Lock() // what onDead does, minus the closes
	d.setStateLocked(StateReconnecting)
	d.tr = nil
	d.mu.Unlock()
	g.mu.Lock()
	g.fail = true
	g.mu.Unlock()
	close(g.gate)
	r := recv(t, res)
	if r.err != nil || !r.pending || r.host != pref {
		t.Fatalf("add cut off by a transport death = %+v, want pending on %d", r, pref)
	}
	fs := d.list()
	if len(fs) != 1 || !fs[0].Pending || fmt.Sprint(fs[0].Owners) != "[conf]" {
		t.Fatalf("forward = %+v, want it kept pending with its owner", fs)
	}
	// Back up: the ticker binds it.
	d.mu.Lock()
	d.tr = newFakeTransport()
	d.setStateLocked(StateUp)
	d.mu.Unlock()
	d.retryPending()
	if fs := d.list(); len(fs) != 1 || fs[0].Pending || !portOpen(pref) {
		t.Fatalf("after retry: %+v", fs)
	}
}

// restore leaves a slot an add is still binding to that add (binding it too
// would race the add's listener for the same port and bump).
func TestRestoreSkipsSlotBeingBound(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	d.mu.Lock()
	f := newFwd(freePort(t), guest, false, confOwner)
	f.startBinding()
	d.forwards[guest] = f
	d.setStateLocked(StateReconnecting)
	d.mu.Unlock()
	fresh := newFakeTransport()
	if !d.restore(fresh) {
		t.Fatal("restore failed")
	}
	if fresh.binds.Load() != 0 {
		t.Fatalf("restore bound a slot that was being bound (%d binds)", fresh.binds.Load())
	}
}

// Review 7: Close does not wait out a reconnect's dial (which can sit in a
// daemon come-up wait for ~20s).
func TestSessionCloseDoesNotWaitOutDial(t *testing.T) {
	dir := shortTempDir(t)
	d := startSockDaemon(t, dir)
	var dials sync.WaitGroup
	calls := 0
	var mu sync.Mutex
	s, err := openSession("t", func(cancel <-chan struct{}) (net.Conn, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return net.Dial("unix", socketPath(dir, "t"))
		}
		dials.Add(1)
		defer dials.Done()
		select { // a come-up wait that only a cancel ends
		case <-cancel:
			return nil, errDialCanceled
		case <-time.After(time.Minute):
			return nil, errors.New("not canceled")
		}
	}, SessionOptions{}, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	d.closeAllSessions() // the supervisor starts redialing
	waitFor(t, "a redial in progress", func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 2 })
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited out the dial")
	}
	dials.Wait()
}

// The ssh transport's IPv4 pre-probe decides busy: a held 127.0.0.1:P is
// busy without asking ssh; a held [::1]:P alone is not, and the forward is
// one dual-stack `localhost:` spec.
func TestSSHPreProbeDecidesBusy(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "ssh.log")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	tr := &sshTransport{conn: backend.SSHConn{Host: "dev@example", ControlPath: filepath.Join(bin, "cm")}}

	v4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	held := v4.Addr().(*net.TCPAddr).Port
	if _, err := tr.forward(held, 3000); !errors.Is(err, errPortBusy) {
		t.Fatalf("forward on a held IPv4 port = %v, want busy", err)
	}
	v4.Close()
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Fatalf("ssh was asked about a port the probe found busy: %s", b)
	}

	v6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	defer v6.Close()
	p := v6.Addr().(*net.TCPAddr).Port
	if probe, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", p)); err != nil {
		t.Skip("port taken on IPv4 too")
	} else {
		probe.Close()
	}
	c, err := tr.forward(p, 3000)
	if err != nil {
		t.Fatalf("forward with only [::1]:%d held = %v, want it bound", p, err)
	}
	b, _ := os.ReadFile(log)
	if want := fmt.Sprintf("-L localhost:%d:localhost:3000", p); !strings.Contains(string(b), "-O forward") || !strings.Contains(string(b), want) {
		t.Fatalf("ssh calls = %q, want -O forward with %q", b, want)
	}
	c.Close()
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "-O cancel") {
		t.Fatalf("close did not cancel the one spec: %q", b)
	}
}

// A held [::1]:P is logged per port; a host without an IPv6 loopback would
// be logged once (not reproducible here, so only the per-port case runs).
func TestV6HeldLoggedPerPort(t *testing.T) {
	v6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	defer v6.Close()
	p := v6.Addr().(*net.TCPAddr).Port
	var mu sync.Mutex
	var lines []string
	old := transportLogf
	transportLogf = func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	t.Cleanup(func() { transportLogf = old })
	lns, err := listenLoopback(p)
	if err != nil {
		t.Skip("port taken on IPv4 too")
	}
	listeners(lns).Close()
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || !strings.Contains(lines[0], "held by another process") {
		t.Fatalf("log = %q", lines)
	}
}

// Removing the last owner of a slot that is mid-bind keeps the slot (ownerless)
// until its binder settles it, so a new add for that guest waits instead of
// binding in parallel: on ssh the first binder's losing adopt would `-O
// cancel` the identical -L spec and kill the new forward.
func TestRemoveMidBindMakesNewAddWait(t *testing.T) {
	_, guest := echoServer(t)
	g := newGateTransport()
	d := newTestDaemon(t)
	d.tr = g
	pref := freePort(t)
	first := addAsync(d, pref, guest, false, connOwner(1))
	<-g.entered
	d.remove(guest, connOwner(1))
	if fs := d.list(); len(fs) != 0 {
		t.Fatalf("an ownerless slot is listed: %+v", fs)
	}
	if d.count() != 0 {
		t.Fatal("an ownerless slot counts as a held forward")
	}
	second := addAsync(d, pref, guest, false, confOwner)
	waitBlocked(t, second, "an add for a guest whose removed slot is still binding")
	close(g.gate)
	if r := recv(t, first); r.err == nil {
		t.Fatalf("the removed add = %+v, want 'removed while binding'", r)
	}
	r := recv(t, second)
	fs := d.list()
	if r.err != nil || r.pending || r.host != pref || len(fs) != 1 || fmt.Sprint(fs[0].Owners) != "[conf]" {
		t.Fatalf("new add = %+v, forwards %+v; want it bound on %d", r, fs, pref)
	}
	if !portOpen(pref) {
		t.Fatal("the new forward was killed by the first binder's cleanup")
	}
}

// Close cuts off a reconnect that is past its dial and waiting on a wedged
// daemon for the session reply (it would otherwise wait out callTimeout).
func TestSessionCloseDuringWedgedOpen(t *testing.T) {
	dir := shortTempDir(t)
	d := startSockDaemon(t, dir)
	wedged, err := net.Listen("unix", filepath.Join(dir, "wedged.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer wedged.Close()
	go func() { // accepts, reads, never answers
		for {
			c, err := wedged.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, c)
		}
	}()
	var mu sync.Mutex
	calls := 0
	s, err := openSession("t", func(<-chan struct{}) (net.Conn, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return net.Dial("unix", socketPath(dir, "t"))
		}
		return net.Dial("unix", filepath.Join(dir, "wedged.sock"))
	}, SessionOptions{}, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	d.closeAllSessions() // the supervisor redials, into the wedged daemon
	waitFor(t, "a redial into the wedged daemon", func() bool {
		mu.Lock()
		defer mu.Unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		return calls >= 2 && s.connecting != nil
	})
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited on a wedged session open")
	}
}
