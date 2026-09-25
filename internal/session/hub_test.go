package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/agentrpc"
	"github.com/smweber/devvm/internal/config"
)

// Hub forwards (hub.md §7, roadmap step 6), in process: a hub daemon on
// fakeTransport serving a real control socket, `__session`'s body
// (Relay) over os.Pipe, and a laptop daemon whose hubTransport gets that
// pipe and a fake -L (fakeLocal) that splices this host's port to the hub
// port the hub reported.

// hubRig is the hub side: its daemon for machine "web", the transport it
// currently runs on, and a gate on its re-dial (closed = dials succeed).
type hubRig struct {
	t    *testing.T
	dir  string
	d    *daemon
	mu   sync.Mutex
	tr   *fakeTransport
	gate chan struct{}
	done <-chan struct{}
}

func newHubRig(t *testing.T) *hubRig {
	t.Helper()
	h := &hubRig{t: t, dir: shortTempDir(t), tr: newFakeTransport(), gate: make(chan struct{})}
	close(h.gate) // dials allowed until a test says otherwise
	if err := os.MkdirAll(config.RuntimeDir(h.dir), 0o700); err != nil {
		t.Fatal(err)
	}
	h.d = newDaemon(h.dir, "web", "test", h.tr, func() (transport, error) {
		h.mu.Lock()
		gate := h.gate
		h.mu.Unlock()
		select {
		case <-gate:
		default:
			return nil, errors.New("hub transport still down (test gate)")
		}
		tr := newFakeTransport()
		h.mu.Lock()
		h.tr = tr
		h.mu.Unlock()
		return tr, nil
	})
	h.d.logf = func(f string, a ...any) { t.Logf("hub: "+f, a...) }
	h.d.minBackoff, h.d.maxBackoff = 5*time.Millisecond, 20*time.Millisecond
	ln, info, err := listenControl(socketPath(h.dir, "web"))
	if err != nil {
		t.Fatal(err)
	}
	h.d.ln, h.d.sockInfo = ln, info
	go h.d.serveControl()
	h.done = runLoop(h.d)
	t.Cleanup(func() {
		h.d.triggerStop()
		<-h.done
		h.d.ln.Close()
		h.d.closeAllSessions()
	})
	return h
}

// kill drops the hub's transport to the VM, with re-dials refused until
// heal.
func (h *hubRig) kill() {
	h.mu.Lock()
	h.gate = make(chan struct{})
	tr := h.tr
	h.mu.Unlock()
	tr.die()
}

func (h *hubRig) heal() {
	h.mu.Lock()
	close(h.gate)
	h.mu.Unlock()
}

func (h *hubRig) forward(guest int) (Forward, bool) {
	for _, f := range h.d.list() {
		if f.Guest == guest {
			return f, true
		}
	}
	return Forward{}, false
}

// relay is one in-process `__session`: the hub end runs Relay exactly as
// the hidden command does, the laptop end is the pipe pair a hubTransport
// reads and writes.
type relay struct {
	laptop lineConn // what hubTransport speaks over
	conn   net.Conn // the relay's connection to the hub daemon
	stdout *os.File // the relay's stdout (write end); closing it is "the process is gone"
	stdin  *os.File // the laptop's write end of the relay's stdin
	done   chan struct{}
	banner string
}

// startRelay starts a __session against the hub's socket. The hub side
// prints banner first, as a chatty login shell would.
func startRelay(t *testing.T, hubDir, banner string) (*relay, error) {
	conn, err := net.Dial("unix", socketPath(hubDir, "web"))
	if err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{laptop: pipeConn{r: outR, w: inW}, conn: conn, stdout: outW, stdin: inW, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		if banner != "" {
			io.WriteString(outW, banner)
		}
		if err := Relay(conn, inR, outW); err != nil {
			t.Logf("relay: %v", err)
		}
		outW.Close() // the process exits: EOF on the laptop's side
		inR.Close()
	}()
	return r, nil
}

// kill is the __session process dying: its daemon connection and its
// stdout go at once.
func (r *relay) kill() {
	r.conn.Close()
	r.stdout.Close()
}

// fakeLocal is the -L: this host's port (both families, as the real one)
// spliced to 127.0.0.1:hubPort. onForward runs before the bind.
type fakeLocal struct {
	dc        chan struct{}
	once      sync.Once
	mu        sync.Mutex
	hubPorts  []int
	onForward func(hostPort, hubPort int)
	// cancelDelay makes each -L's close as slow as an `ssh -O cancel`
	// against a wedged master.
	cancelDelay time.Duration
	open        []io.Closer // every -L, dropped by Close as the master's -O exit does
}

type slowCloser struct {
	io.Closer
	delay time.Duration
}

func (c slowCloser) Close() error {
	time.Sleep(c.delay)
	return c.Closer.Close()
}

func newFakeLocal() *fakeLocal { return &fakeLocal{dc: make(chan struct{})} }

func (l *fakeLocal) forwardTo(hostPort, hubPort int) (io.Closer, error) {
	if l.onForward != nil {
		l.onForward(hostPort, hubPort)
	}
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
					up, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", hubPort))
					if err != nil {
						c.Close()
						return
					}
					agentrpc.Splice(c, up)
				}(c)
			}
		}(ln)
	}
	l.mu.Lock()
	l.hubPorts = append(l.hubPorts, hubPort)
	l.open = append(l.open, listeners(lns))
	l.mu.Unlock()
	if l.cancelDelay > 0 {
		return slowCloser{listeners(lns), l.cancelDelay}, nil
	}
	return listeners(lns), nil
}

func (l *fakeLocal) lastHubPort() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hubPorts) == 0 {
		return 0
	}
	return l.hubPorts[len(l.hubPorts)-1]
}

func (l *fakeLocal) dead() <-chan struct{} { return l.dc }
func (l *fakeLocal) Close() error {
	l.once.Do(func() { close(l.dc) })
	l.mu.Lock()
	open := l.open
	l.open = nil
	l.mu.Unlock()
	for _, c := range open {
		c.Close()
	}
	return nil
}

// laptopRig is the laptop daemon for h/web, dialing a fresh relay (a fresh
// hubTransport, with a fresh -L master) on every reconnect, as RunDaemon's
// dial does. local is the first transport's -L; lastLocal the current one.
type laptopRig struct {
	d      *daemon
	local  *fakeLocal
	mu     sync.Mutex
	relays []*relay
	locals []*fakeLocal
	dials  atomic.Int32
}

func (l *laptopRig) lastLocal() *fakeLocal {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.locals[len(l.locals)-1]
}

func (l *laptopRig) lastRelay() *relay {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.relays[len(l.relays)-1]
}

func (l *laptopRig) hubTransport() *hubTransport {
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	ht, _ := l.d.tr.(*hubTransport)
	return ht
}

func dialHub(t *testing.T, hubDir string, local *fakeLocal, banner string) (*hubTransport, *relay, error) {
	r, err := startRelay(t, hubDir, banner)
	if err != nil {
		return nil, nil, err
	}
	stop := func() {
		r.stdin.Close()
		<-r.done
	}
	ht, err := openHubTransport("h/web", r.laptop, local, stop, nil)
	return ht, r, err
}

func newLaptopRig(t *testing.T, h *hubRig, loop bool) *laptopRig {
	t.Helper()
	l := &laptopRig{local: newFakeLocal()}
	// A banner with no newline of its own: the relay's leading newline keeps
	// it off the marker line.
	ht, r, err := dialHub(t, h.dir, l.local, "Welcome to the hub")
	if err != nil {
		t.Fatalf("first hub dial: %v", err)
	}
	l.relays = append(l.relays, r)
	l.locals = append(l.locals, l.local)
	l.d = newDaemon(shortTempDir(t), "h/web", "test", ht, func() (transport, error) {
		l.dials.Add(1)
		local := newFakeLocal()
		ht, r, err := dialHub(t, h.dir, local, "")
		if err != nil {
			return nil, err
		}
		l.mu.Lock()
		l.relays = append(l.relays, r)
		l.locals = append(l.locals, local)
		l.mu.Unlock()
		return ht, nil
	})
	l.d.logf = func(f string, a ...any) { t.Logf("laptop: "+f, a...) }
	l.d.minBackoff, l.d.maxBackoff = 5*time.Millisecond, 20*time.Millisecond
	if loop {
		done := runLoop(l.d)
		t.Cleanup(func() { l.d.triggerStop(); <-done; l.d.teardown() })
	} else {
		t.Cleanup(func() { l.d.teardown() })
	}
	return l
}

func echoOK(t *testing.T, port int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial localhost:%d: %v", port, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	io.WriteString(c, "hub\n")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hub\n" {
		t.Fatalf("echo through localhost:%d = %q, %v", port, buf, err)
	}
}

// add returns the hub port, and the laptop binds only after it: at the -L,
// the hub already holds the forward, bound, on exactly that port, owned by
// the relay's connection alone. remove clears both sides.
func TestHubForwardAddRemove(t *testing.T) {
	_, guest := echoServer(t)
	h := newHubRig(t)
	l := newLaptopRig(t, h, false)
	var checked atomic.Bool
	l.local.onForward = func(_, hubPort int) {
		f, ok := h.forward(guest)
		if !ok || f.Pending || f.Host != hubPort {
			t.Errorf("at the -L the hub forward is %+v (ok=%v), want bound on %d", f, ok, hubPort)
		}
		checked.Store(true)
	}
	pref := farPort(t)
	host, _, pending, err := l.d.add(pref, guest, false, confOwner)
	if err != nil || pending || host != pref {
		t.Fatalf("laptop add = %d pending=%v err=%v", host, pending, err)
	}
	if !checked.Load() {
		t.Fatal("the -L was never bound")
	}
	echoOK(t, host)
	f, _ := h.forward(guest)
	if fmt.Sprint(f.Owners) != "[connection]" {
		t.Errorf("hub owners = %v, want the relay's connection only", f.Owners)
	}
	if h.d.sessionCount() != 1 {
		t.Errorf("hub sessions = %d, want the one relay", h.d.sessionCount())
	}
	if l.local.lastHubPort() != f.Host {
		t.Errorf("-L targets hub port %d, hub forward is on %d", l.local.lastHubPort(), f.Host)
	}

	l.d.remove(guest, confOwner)
	if _, ok := h.forward(guest); ok {
		t.Error("hub still holds the forward after the laptop's remove")
	}
	if portOpen(host) {
		t.Error("laptop port still open after remove")
	}
	if h.d.sessionCount() != 1 {
		t.Error("remove closed the relay")
	}
}

// Killing the __session end drops every forward it owned on the hub, and
// the laptop's transport reports dead.
func TestHubSessionKillDropsItsForwards(t *testing.T) {
	_, g1 := echoServer(t)
	_, g2 := echoServer(t)
	h := newHubRig(t)
	l := newLaptopRig(t, h, false)
	for _, g := range []int{g1, g2} {
		if _, _, _, err := l.d.add(farPort(t), g, false, confOwner); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(h.d.list()); n != 2 {
		t.Fatalf("hub forwards = %d, want 2", n)
	}
	ht := l.hubTransport()
	l.lastRelay().kill()
	waitFor(t, "the hub to drop the relay's forwards", func() bool { return len(h.d.list()) == 0 })
	waitFor(t, "the hub to drop the relay", func() bool { return h.d.sessionCount() == 0 })
	select {
	case <-ht.dead():
	case <-time.After(5 * time.Second):
		t.Fatal("hubTransport did not report dead after its __session died")
	}
}

// The laptop closing its transport (idle exit, stop) is stdin EOF to the
// __session: the relay ends cleanly and the hub drops what it owned.
func TestHubTransportCloseEndsRelay(t *testing.T) {
	_, guest := echoServer(t)
	h := newHubRig(t)
	l := newLaptopRig(t, h, false)
	if _, _, _, err := l.d.add(farPort(t), guest, false, confOwner); err != nil {
		t.Fatal(err)
	}
	l.hubTransport().Close()
	<-l.lastRelay().done
	waitFor(t, "the hub to drop the relay's forward", func() bool { return len(h.d.list()) == 0 && h.d.sessionCount() == 0 })
}

// A relay session is closed when the hub's transport dies, and its
// forwards go; a local session on the same daemon survives, and restore()
// brings its forward back.
func TestRelayClosedOnTransportDeathLocalSurvives(t *testing.T) {
	_, gLocal := echoServer(t)
	_, gRelay := echoServer(t)
	h := newHubRig(t)
	local, err := openSession("web", func(<-chan struct{}) (net.Conn, error) {
		return net.Dial("unix", socketPath(h.dir, "web"))
	}, SessionOptions{}, 5*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	if _, _, _, err := local.Add(farPort(t), gLocal, false); err != nil {
		t.Fatal(err)
	}
	l := newLaptopRig(t, h, false)
	if _, _, _, err := l.d.add(farPort(t), gRelay, false, confOwner); err != nil {
		t.Fatal(err)
	}
	ht := l.hubTransport()
	if h.d.sessionCount() != 2 {
		t.Fatalf("hub sessions = %d, want local + relay", h.d.sessionCount())
	}

	h.tr.die() // the dial gate stays open: the hub restores at once
	select {
	case <-ht.dead():
	case <-time.After(5 * time.Second):
		t.Fatal("the relay was not closed on the hub's transport death")
	}
	waitFor(t, "the hub to be back up with only the local session", func() bool {
		st, _ := h.d.status()
		return st == StateUp && h.d.sessionCount() == 1
	})
	if _, ok := h.forward(gRelay); ok {
		t.Error("the relay's forward outlived its session")
	}
	f, ok := h.forward(gLocal)
	if !ok || f.Pending {
		t.Errorf("the local session's forward = %+v (ok=%v), want restored", f, ok)
	}
	// The local session is the same connection, never reconnected: it can
	// still add.
	if _, _, _, err := local.Add(farPort(t), gRelay, false); err != nil {
		t.Errorf("local session after the hub's reconnect: %v", err)
	}
}

// Transport death on the hub: the laptop goes reconnecting, re-adds, and
// picks up a *changed* hub port (the old one is held during the outage),
// while this host's port stays what it was.
func TestHubReconnectPicksUpChangedHubPort(t *testing.T) {
	_, guest := echoServer(t)
	h := newHubRig(t)
	l := newLaptopRig(t, h, true)
	host, _, _, err := l.d.add(farPort(t), guest, false, confOwner)
	if err != nil {
		t.Fatal(err)
	}
	echoOK(t, host)
	old, _ := h.forward(guest)

	// The hub stays down (its re-dial is gated) until heal, so the laptop's
	// re-dials are all refused and it cannot get past reconnecting; onDead
	// makes the reconnecting state, the pending forwards and the dropped
	// relay one atomic change on each side, so what is read below is the
	// outage and nothing in between.
	laptopStates := recordStates(l.d)
	h.kill()
	laptopStates.wait(t, StateReconnecting)
	if _, ok := h.forward(guest); ok || h.d.sessionCount() != 0 {
		t.Errorf("hub during the outage still holds the relay or its forward")
	}
	hold, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", old.Host))
	if err != nil {
		t.Fatalf("hold the old hub port %d: %v", old.Host, err)
	}
	defer hold.Close()
	if fs := l.d.list(); len(fs) != 1 || !fs[0].Pending || fs[0].Host != host {
		t.Errorf("laptop during the outage = %+v, want %d pending", fs, host)
	}

	h.heal()
	// restore() sets up only once every forward is re-added and bound.
	laptopStates.wait(t, StateReconnecting, StateUp)
	if fs := l.d.list(); len(fs) != 1 || fs[0].Pending {
		t.Fatalf("laptop after the outage = %+v, want its forward bound", fs)
	}
	now, ok := h.forward(guest)
	if !ok || now.Host == old.Host {
		t.Fatalf("hub forward after the outage = %+v, want a port other than the held %d", now, old.Host)
	}
	if l.lastLocal() == l.local || l.lastLocal().lastHubPort() != now.Host {
		t.Errorf("-L on the new master targets hub port %d, hub forward is on %d", l.lastLocal().lastHubPort(), now.Host)
	}
	if fs := l.d.list(); fs[0].Host != host {
		t.Errorf("laptop port moved: %d, want %d", fs[0].Host, host)
	}
	echoOK(t, host)
}

// A relay opened while the hub's transport is down is refused (an error
// reply, then the close); the laptop stays reconnecting through its own
// retries and comes up on the first one after the hub's restore().
func TestRelayRefusedWhileTransportDown(t *testing.T) {
	_, guest := echoServer(t)
	h := newHubRig(t)
	l := newLaptopRig(t, h, true)
	host, _, _, err := l.d.add(farPort(t), guest, false, confOwner)
	if err != nil {
		t.Fatal(err)
	}
	hubStates, laptopStates := recordStates(h.d), recordStates(l.d)
	h.kill()
	// The hub's re-dial is gated until heal: it stays reconnecting, and
	// onDead has dropped the killed relay from its sessions in the same
	// critical section that set the state.
	hubStates.wait(t, StateReconnecting)
	if n := h.d.sessionCount(); n != 0 {
		t.Fatalf("hub sessions right after its transport died = %d, want the relay gone", n)
	}
	h.d.mu.Lock()
	ids := h.d.nextConn
	h.d.mu.Unlock()

	// A relay dialed by hand now is refused with a reply that says why.
	_, _, err = dialHub(t, h.dir, newFakeLocal(), "")
	if err == nil || !strings.Contains(err.Error(), "relay session refused") {
		t.Fatalf("relay during the hub's outage: err = %v, want refused", err)
	}
	// The laptop keeps retrying, every attempt refused.
	before := l.dials.Load()
	waitFor(t, "the laptop to retry", func() bool { return l.dials.Load() >= before+3 })
	// Never counted, not even briefly: no refused open (this one or the
	// laptop's) was ever given a session id.
	h.d.mu.Lock()
	idsAfter, n := h.d.nextConn, len(h.d.sessions)
	h.d.mu.Unlock()
	if idsAfter != ids || n != 0 {
		t.Errorf("refused relays were registered: %d session ids handed out, %d sessions", idsAfter-ids, n)
	}
	if laptopStates.has(StateUp) {
		t.Error("the laptop came up while the hub was down")
	}

	h.heal()
	hubStates.wait(t, StateReconnecting, StateUp)
	laptopStates.wait(t, StateReconnecting, StateUp)
	if fs := l.d.list(); len(fs) != 1 || fs[0].Pending {
		t.Fatalf("laptop after the hub's restore = %+v, want its forward bound", fs)
	}
	echoOK(t, host)
}

// scriptedHub answers the laptop's side of a relay by hand: the session
// open as a current hub daemon does, then every add with reply(add).
func scriptedHub(t *testing.T, reply func(Request) Response) (lineConn, func()) {
	t.Helper()
	return scriptedHubOpen(t, func(Request) Response { return Response{OK: true, State: StateUp, Relay: true} }, reply)
}

// scriptedHubOpen is scriptedHub with the open reply given too. A reply
// with ok=false is followed by EOF, as a daemon closes a refused open. A
// zero Response from reply means "never answer" (a wedged hub).
func scriptedHubOpen(t *testing.T, open, reply func(Request) Response) (lineConn, func()) {
	t.Helper()
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	go func() {
		defer outW.Close()
		io.WriteString(outW, SessionMarker+"\n")
		br := bufio.NewReader(inR)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			var req Request
			_ = json.Unmarshal([]byte(line), &req)
			var resp Response
			switch req.Op {
			case OpSession:
				if !req.Relay {
					t.Errorf("the hub transport opened a session without relay: %s", line)
				}
				resp = open(req)
				resp.ID = req.ID
				b, _ := json.Marshal(resp)
				outW.Write(append(b, '\n'))
				if !resp.OK {
					return
				}
				continue
			default:
				resp = reply(req)
			}
			if !resp.OK && resp.Err == "" {
				continue // never answered
			}
			resp.ID = req.ID
			b, _ := json.Marshal(resp)
			outW.Write(append(b, '\n'))
		}
	}()
	return pipeConn{r: outR, w: inW}, func() { inW.Close(); inR.Close() }
}

// A `pending` reply to the hub add is a bind failure, never a forward: the
// laptop records its forward pending (not failed, not bound to a hub port
// nothing listens on) and the transport reports dead so the reconnect
// re-adds.
func TestHubPendingReplyLeavesLaptopPending(t *testing.T) {
	c, stop := scriptedHub(t, func(req Request) Response {
		return Response{OK: true, Host: req.Guest, Pending: true}
	})
	local := newFakeLocal()
	ht, err := openHubTransport("h/web", c, local, stop, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := newDaemon(shortTempDir(t), "h/web", "test", ht, func() (transport, error) {
		return nil, errors.New("no dial in this test")
	})
	d.logf = t.Logf
	pref := farPort(t)
	host, _, pending, err := d.add(pref, 3000, false, confOwner)
	if err != nil || !pending || host != pref {
		t.Fatalf("add on a pending hub = %d pending=%v err=%v, want %d pending", host, pending, err, pref)
	}
	if fs := d.list(); len(fs) != 1 || !fs[0].Pending {
		t.Errorf("laptop forwards = %+v, want one pending", fs)
	}
	if local.lastHubPort() != 0 || portOpen(pref) {
		t.Error("a -L was bound for a pending hub forward")
	}
	select {
	case <-ht.dead():
	default:
		t.Error("the hub transport did not mark itself dead on a pending reply")
	}
	ht.Close()
}

// The marker reader discards a login banner before the marker, and the
// hub transport refuses a stream that never shows it, naming the hub's own
// last stderr line.
func TestHubTransportMarker(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("Welcome\nlast login: x\n" + SessionMarker + "\n{\"x\":1}\n"))
	if err := SkipToMarker(br, SessionMarker, MarkerLimit); err != nil {
		t.Fatal(err)
	}
	if rest, _ := br.ReadString('\n'); rest != "{\"x\":1}\n" {
		t.Errorf("after the marker: %q", rest)
	}
	if err := SkipToMarker(bufio.NewReader(strings.NewReader("nope\n")), SessionMarker, MarkerLimit); !errors.Is(err, ErrMarkerMissing) {
		t.Errorf("no marker: %v", err)
	}
	if err := SkipToMarker(bufio.NewReader(strings.NewReader(strings.Repeat("x", 100)+"\n"+SessionMarker+"\n")), SessionMarker, 50); !errors.Is(err, ErrMarkerLimit) {
		t.Errorf("past the limit: %v", err)
	}

	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	io.WriteString(outW, "Welcome\n")
	outW.Close()
	stopped := false
	_, err := openHubTransport("h/web", pipeConn{r: outR, w: inW}, newFakeLocal(), func() { stopped = true; inR.Close() },
		func() string { return "web is not running; start it first" })
	if err == nil || !strings.Contains(err.Error(), "web is not running") {
		t.Errorf("no marker: err = %v, want the hub's reason", err)
	}
	if !stopped {
		t.Error("a failed open did not stop the __session process")
	}
}

// startHubSession gives the child *os.File stdio (never an io.Pipe, whose
// copy goroutine Wait would block on) and turns the child's exit into EOF.
func TestStartHubSessionPipes(t *testing.T) {
	c, stop, tail, err := startHubSession([]string{"sh", "-c", `echo oops >&2; echo ` + SessionMarker + `; read line; echo "got $line"`})
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	if err := SkipToMarker(br, SessionMarker, MarkerLimit); err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "ping\n")
	if line, _ := br.ReadString('\n'); line != "got ping\n" {
		t.Errorf("echo = %q", line)
	}
	if _, err := br.ReadString('\n'); err != io.EOF {
		t.Errorf("after the child exits: %v, want EOF", err)
	}
	if got := tail(); got != "oops" {
		t.Errorf("stderr tail = %q", got)
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop hung")
	}
}

// Teardown against a wedged link stays bounded whatever the forward count:
// the relay's close drops every hub forward and the master's exit every -L,
// so no forward waits out its own remove (hubCallTimeout) or `ssh -O
// cancel` in turn. Found in review: 3 forwards took over 90s.
func TestHubTeardownBounded(t *testing.T) {
	c, stop := scriptedHub(t, func(req Request) Response {
		if req.Op == OpRemove {
			return Response{} // wedged: never answers
		}
		return Response{OK: true, Host: req.Guest}
	})
	local := newFakeLocal()
	local.cancelDelay = 5 * time.Second
	ht, err := openHubTransport("h/web", c, local, stop, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := newDaemon(shortTempDir(t), "h/web", "test", ht, func() (transport, error) { return nil, errors.New("no dial") })
	d.logf = t.Logf
	for i := 0; i < 4; i++ {
		_, g := echoServer(t)
		if _, _, p, err := d.add(farPort(t), g, false, confOwner); err != nil || p {
			t.Fatalf("add: %v pending=%v", err, p)
		}
	}
	start := time.Now()
	d.teardown()
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("teardown with 4 forwards took %s", took)
	}
}

// Outside teardown one forward's close still does both halves: the -L
// cancel and the hub remove (`ports rm`), and a remove the hub never
// answers costs hubCallTimeout, not the open's 45s.
func TestHubForwardCloseRemovesOnHub(t *testing.T) {
	var removes atomic.Int32
	c, stop := scriptedHub(t, func(req Request) Response {
		if req.Op == OpRemove {
			removes.Add(1)
			return Response{}
		}
		return Response{OK: true, Host: req.Guest}
	})
	old := hubCallTimeout
	hubCallTimeout = 200 * time.Millisecond
	defer func() { hubCallTimeout = old }()
	ht, err := openHubTransport("h/web", c, newFakeLocal(), stop, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ht.Close()
	_, g := echoServer(t)
	host := farPort(t)
	fc, err := ht.forward(host, g)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	fc.Close()
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("close with a silent hub took %s", took)
	}
	if removes.Load() != 1 || portOpen(host) {
		t.Errorf("close: removes=%d, port open=%v", removes.Load(), portOpen(host))
	}
}

// A hub daemon older than hub forwards is named, with the fix to run on the
// hub: a step-5 daemon admits the relay without echoing it, a pre-session
// one answers "unknown op".
func TestHubStaleDaemonRefused(t *testing.T) {
	for name, open := range map[string]Response{
		"step 5 daemon (no relay echo)": {OK: true, State: StateUp, Version: "v0.1.13"},
		"pre-session daemon":            {Err: "unknown op: session"},
	} {
		c, stop := scriptedHubOpen(t, func(Request) Response { return open },
			func(Request) Response { return Response{OK: true} })
		_, err := openHubTransport("h/web", c, newFakeLocal(), stop, nil)
		if err == nil || !strings.Contains(err.Error(), "the forward daemon for web on hub h predates hub forwards") ||
			!strings.Contains(err.Error(), "'devvm ports down web'") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A hub whose devvm has no __session at all (older than hub forwards) is
// named with the version it needs.
func TestHubWithoutSessionCommand(t *testing.T) {
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	outW.Close()
	defer inR.Close()
	_, err := openHubTransport("h/web", pipeConn{r: outR, w: inW}, newFakeLocal(), func() {},
		func() string { return `devvm: unknown command "__session" for "devvm"` })
	if err == nil || !strings.Contains(err.Error(), "devvm on hub h predates hub forwards (needs "+HubForwardsMinVersion) {
		t.Errorf("err = %v", err)
	}
}

// A hub that answers an add with an error fails a first add with the hub's
// words verbatim (never dressed as local port exhaustion), but leaves a
// forward pending in restore() rather than failing the whole attempt, which
// would tear every forward down and retry in a loop. The ticker keeps
// retrying it and logs the hub's text again only when it changes.
func TestHubAddErrorLeavesRestorePending(t *testing.T) {
	var reason atomic.Value
	reason.Store("guest is not listening")
	c, stop := scriptedHub(t, func(req Request) Response {
		return Response{Err: reason.Load().(string)}
	})
	ht, err := openHubTransport("h/web", c, newFakeLocal(), stop, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ht.Close()
	d := newDaemon(shortTempDir(t), "h/web", "test", nil, func() (transport, error) { return nil, errors.New("no dial") })
	var logMu sync.Mutex
	var logged []string
	d.logf = func(f string, a ...any) {
		logMu.Lock()
		logged = append(logged, fmt.Sprintf(f, a...))
		logMu.Unlock()
	}
	count := func(sub string) int {
		logMu.Lock()
		defer logMu.Unlock()
		n := 0
		for _, l := range logged {
			if strings.Contains(l, sub) {
				n++
			}
		}
		return n
	}
	d.state = StateReconnecting
	pref := farPort(t)
	if _, _, pending, err := d.add(pref, 3000, false, confOwner); err != nil || !pending {
		t.Fatalf("add while reconnecting = pending %v, %v", pending, err)
	}
	if !d.restore(ht) {
		t.Fatal("restore failed the whole attempt on a hub-side refusal")
	}
	if fs := d.list(); len(fs) != 1 || !fs[0].Pending {
		t.Errorf("after restore: %+v, want the forward pending", fs)
	}
	if transportDead(ht) {
		t.Error("a hub refusal marked the relay dead")
	}
	// The ticker: the same reason is not logged again; a new one is.
	d.retryPending()
	d.retryPending()
	if n := count("hub: guest is not listening"); n != 1 {
		t.Errorf("an unchanged hub error was logged %d times", n)
	}
	reason.Store("no free host port for guest 3000 in range 3000-3019")
	d.retryPending()
	d.retryPending()
	if n := count("hub: no free host port for guest 3000"); n != 1 {
		t.Errorf("a changed hub error was logged %d times", n)
	}
	// A first add reports the hub's text as is.
	reason.Store("guest is not listening")
	_, _, _, err = d.add(farPort(t), 3001, false, confOwner)
	if err == nil || err.Error() != "hub: guest is not listening" || !errors.Is(err, errHubRefused) || errors.Is(err, errPortExhausted) {
		t.Errorf("a first add on a refusing hub: err = %v", err)
	}
}

// Dial's come-up wait says why at once when the spawned daemon exits early,
// quoting only what that daemon logged (never an earlier run's line); a
// clean exit (another daemon won the socket) keeps it polling.
func TestWaitComeUpEarlyExit(t *testing.T) {
	dir := shortTempDir(t)
	if err := config.EnsureRuntimeDir(dir); err != nil {
		t.Fatal(err)
	}
	log := logPath(dir, "h/web")
	if err := os.WriteFile(log, []byte("devvm: an earlier run's error\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Client{configDir: dir, name: "h/web"}
	off := logSize(log)
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("devvm: h/web: the hub's __session ended before it started: web is not running\n")
	f.Close()
	exited := make(chan daemonExit, 1)
	exited <- daemonExit{errors.New("exit status 1")}
	start := time.Now()
	_, err := c.waitComeUp(exited, off, nil, time.Now().Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "web is not running") || time.Since(start) > 2*time.Second {
		t.Errorf("early exit: err = %v after %s", err, time.Since(start))
	}
	// A silent death quotes nothing old.
	exited <- daemonExit{errors.New("exit status 1")}
	_, err = c.waitComeUp(exited, logSize(log), nil, time.Now().Add(time.Minute))
	if err == nil || strings.Contains(err.Error(), "earlier run") || !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("silent exit: err = %v", err)
	}
	// A clean exit is not a failure; the deadline decides.
	exited <- daemonExit{nil}
	_, err = c.waitComeUp(exited, 0, nil, time.Now().Add(300*time.Millisecond))
	if err == nil || !strings.Contains(err.Error(), "did not come up") {
		t.Errorf("clean exit: err = %v", err)
	}
}

// A hub machine's come-up budget covers the master's connect plus the
// relay's open deadline.
func TestComeUpWaitHubMachine(t *testing.T) {
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "")
	if got, want := comeUpWait("h/web")-comeUpWait("web"), hubOpenTimeout+10*time.Second; got != want {
		t.Errorf("hub extra = %s, want %s", got, want)
	}
	if HubForwardsMinVersion != "v0.1.14" {
		t.Errorf("HubForwardsMinVersion = %s: it must be the first tag shipping hub forwards; change this test with it", HubForwardsMinVersion)
	}
}

// farPort is a free loopback port from 20000-29999, below every OS's
// ephemeral range (Linux 32768+, macOS 49152+), for this host's side of a
// hub forward. The hub rig shares the host: it prefers the guest port,
// which the echo server holds, and bumps up from it, through ephemeral
// ports. freePort's answer is an ephemeral port too, and macOS hands them
// out sequentially, so the laptop's preference was exactly where the hub's
// bump landed (CI: "laptop add = P+1", "laptop port moved"). A port from
// this range can never be one of the hub's. Ports handed out are not
// handed out again in the same run, so two calls before either binds
// cannot collide.
func farPort(t *testing.T) int {
	t.Helper()
	farMu.Lock()
	defer farMu.Unlock()
	for tries := 0; tries < 1000; tries++ {
		p := 20000 + rand.IntN(10000)
		if farUsed[p] {
			continue
		}
		ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		ln.Close()
		farUsed[p] = true
		return p
	}
	t.Fatal("no free port in 20000-29999")
	return 0
}

var (
	farMu   sync.Mutex
	farUsed = map[int]bool{}
)
