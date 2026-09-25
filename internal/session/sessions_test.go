package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
)

// startSockDaemon serves a daemon on a real control socket under dir, so
// sessions and one-shot clients reach it exactly as the CLI does.
func startSockDaemon(t *testing.T, dir string) *daemon {
	t.Helper()
	if err := os.MkdirAll(config.RuntimeDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	d := newDaemon(dir, "t", "test", newFakeTransport(), func() (transport, error) {
		return nil, errors.New("no dial in this test")
	})
	d.logf = t.Logf
	d.minBackoff, d.maxBackoff = 5*time.Millisecond, 20*time.Millisecond
	ln, info, err := listenControl(socketPath(dir, "t"))
	if err != nil {
		t.Fatal(err)
	}
	d.ln, d.sockInfo = ln, info
	go d.serveControl()
	t.Cleanup(func() { d.ln.Close(); d.closeAllSessions() })
	return d
}

// testSession opens a session client on dir's socket without spawning
// anything, with test-sized backoff.
func testSession(t *testing.T, dir string, opts SessionOptions) *Session {
	t.Helper()
	s, err := openSession("t", func(<-chan struct{}) (net.Conn, error) {
		return net.Dial("unix", socketPath(dir, "t"))
	}, opts, 5*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// named is an OnEvent handler that answers with a fixed name, so a test can
// tell which subscriber got an event.
func named(name string) func(json.RawMessage) json.RawMessage {
	return func(json.RawMessage) json.RawMessage { return json.RawMessage(strconv.Quote(name)) }
}

func deliverTo(t *testing.T, d *daemon) string {
	t.Helper()
	r, err := d.deliver(json.RawMessage(`{"k":1}`), 2*time.Second)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	var s string
	if err := json.Unmarshal(r, &s); err != nil {
		t.Fatalf("reply %s: %v", r, err)
	}
	return s
}

func owners(d *daemon, guest int) []string {
	for _, f := range d.list() {
		if f.Guest == guest {
			return f.Owners
		}
	}
	return nil
}

// One forward with conf, connection and ttl owners closes only when the last
// of them drops.
func TestForwardClosesOnlyWhenLastOwnerDrops(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	pref := freePort(t)
	for _, own := range []owner{confOwner, connOwner(7), ttlOwner} {
		if host, _, _, err := d.add(pref, guest, false, own); err != nil || host != pref {
			t.Fatalf("add %v = %d, %v", own, host, err)
		}
	}
	if got := fmt.Sprint(owners(d, guest)); got != "[conf connection ttl]" {
		t.Fatalf("owners = %s", got)
	}
	if d.tr.(*fakeTransport).binds.Load() != 1 {
		t.Fatalf("reuse bound again: %d binds", d.tr.(*fakeTransport).binds.Load())
	}
	for i, own := range []owner{confOwner, ttlOwner, connOwner(7)} {
		if !portOpen(pref) {
			t.Fatalf("forward closed before its last owner (after %d drops)", i)
		}
		d.remove(guest, own)
	}
	if portOpen(pref) || len(d.list()) != 0 {
		t.Fatal("forward still up after its last owner dropped")
	}
}

// A one-shot remove drops conf by default, or ttl when asked, and never a
// connection owner: that belongs to its session.
func TestOneShotRemoveOwnerRules(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	pref := freePort(t)
	d.add(pref, guest, false, confOwner)
	d.add(pref, guest, false, connOwner(3))
	if r := d.dispatch(Request{Op: OpRemove, Guest: guest, Owner: OwnerConnection}); r.OK {
		t.Fatalf("remove of a connection owner accepted: %+v", r)
	}
	r := d.dispatch(Request{Op: OpRemove, Guest: guest})
	if !r.OK || len(r.Forwards) != 1 || fmt.Sprint(r.Forwards[0].Owners) != "[connection]" {
		t.Fatalf("conf remove = %+v, want the forward left to its connection", r)
	}
	// A ttl remove on a forward no ttl holds leaves it alone.
	if r := d.dispatch(Request{Op: OpRemove, Guest: guest, Owner: OwnerTTL}); !r.OK || len(r.Forwards) != 1 {
		t.Fatalf("ttl remove = %+v", r)
	}
	if !portOpen(pref) {
		t.Fatal("the session's forward was torn down")
	}
	d.add(pref, guest, false, ttlOwner)
	d.remove(guest, connOwner(3))
	if r := d.dispatch(Request{Op: OpRemove, Guest: guest, Owner: OwnerTTL}); !r.OK || len(r.Forwards) != 0 {
		t.Fatalf("last ttl remove = %+v", r)
	}
	if portOpen(pref) {
		t.Fatal("ttl-only forward survived its ttl remove")
	}
}

// `ports down` with a session open drops every conf owner and leaves the
// daemon (and the session's forwards) up; with nothing left it stops.
func TestDownWithSessionLeavesDaemon(t *testing.T) {
	_, g1 := echoServer(t)
	_, g2 := echoServer(t)
	dir := shortTempDir(t)
	d := startSockDaemon(t, dir)
	done := runLoop(d)
	s := testSession(t, dir, SessionOptions{})
	p1, p2 := freePort(t), freePort(t)
	if _, _, _, err := (&Client{configDir: dir, name: "t"}).Add(p1, g1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Add(p2, g2, false); err != nil {
		t.Fatal(err)
	}
	r := d.dispatch(Request{Op: OpDown})
	if !r.OK || r.Stopped || r.Sessions != 1 || len(r.Forwards) != 1 || r.Forwards[0].Guest != g2 {
		t.Fatalf("down = %+v, want the session's forward left and the daemon up", r)
	}
	if portOpen(p1) || !portOpen(p2) {
		t.Fatalf("after down: conf forward open=%v, session forward open=%v", portOpen(p1), portOpen(p2))
	}
	select {
	case <-done:
		t.Fatal("down stopped a daemon a session holds")
	case <-time.After(50 * time.Millisecond):
	}
	s.Close()
	// The session and its owners go in one critical section: once the
	// session is gone no forward it held may be listed (and a `down` sent in
	// between would wrongly answer "stays up for 1 forward").
	waitFor(t, "session gone", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		if len(d.sessions) != 0 {
			return false
		}
		if n := d.heldLocked(); n != 0 {
			t.Fatalf("session gone but %d forward(s) still held", n)
		}
		return true
	})
	// Its listener closes right after the unlock.
	waitFor(t, "the session's forward closed", func() bool { return !portOpen(p2) })
	if r := d.dispatch(Request{Op: OpDown}); !r.Stopped {
		t.Fatalf("down with nothing held = %+v, want stopped", r)
	}
	waitDone(t, done, "down with nothing held")
}

// A session with no forwards holds the daemon past the idle period; when it
// closes the daemon idles out.
func TestSessionHoldsDaemonPastIdle(t *testing.T) {
	dir := shortTempDir(t)
	d := startSockDaemon(t, dir)
	d.idle = 30 * time.Millisecond
	s := testSession(t, dir, SessionOptions{})
	done := runLoop(d)
	select {
	case <-done:
		t.Fatal("daemon idled out under an open session")
	case <-time.After(10 * d.idle):
	}
	s.Close()
	waitDone(t, done, "the last session closed")
}

// The idle rule holds in reconnect() too: a session keeps a reconnecting
// daemon with nothing to restore, and its close lets it go.
func TestReconnectIdleRuleCountsSessions(t *testing.T) {
	dir := shortTempDir(t)
	d := startSockDaemon(t, dir)
	d.idle = 30 * time.Millisecond
	first := d.tr.(*fakeTransport)
	s := testSession(t, dir, SessionOptions{})
	done := runLoop(d)
	first.die()
	waitFor(t, "reconnecting", func() bool { st, _ := d.status(); return st == StateReconnecting })
	select {
	case <-done:
		t.Fatal("reconnect idled out under an open session")
	case <-time.After(10 * d.idle):
	}
	s.Close()
	waitDone(t, done, "the session closed while reconnecting")
}

// subscribe is acked only once registered; the most recent subscriber gets
// events; re-subscribing moves a session to the front; a disconnect or an
// unsubscribe falls through to the next.
func TestSubscribeOrder(t *testing.T) {
	dir := shortTempDir(t)
	d := startSockDaemon(t, dir)
	if _, err := d.deliver(nil, time.Second); !errors.Is(err, errNoSubscriber) {
		t.Fatalf("deliver with no subscriber = %v", err)
	}
	sa := testSession(t, dir, SessionOptions{OnEvent: named("A")})
	sb := testSession(t, dir, SessionOptions{OnEvent: named("B")})
	sc := testSession(t, dir, SessionOptions{OnEvent: named("C")})
	for i, s := range []*Session{sa, sb, sc} {
		if err := s.Subscribe(); err != nil {
			t.Fatal(err)
		}
		// Acked means registered: no waiting here.
		d.mu.Lock()
		n := len(d.subs)
		d.mu.Unlock()
		if n != i+1 {
			t.Fatalf("after subscribe ack %d, %d subscribers registered", i+1, n)
		}
	}
	if got := deliverTo(t, d); got != "C" {
		t.Fatalf("most recent subscriber: got %s, want C", got)
	}
	if err := sa.Subscribe(); err != nil {
		t.Fatal(err)
	}
	if got := deliverTo(t, d); got != "A" {
		t.Fatalf("after A re-subscribed: got %s, want A", got)
	}
	sa.Close()
	waitFor(t, "A gone", func() bool { return d.sessionCount() == 2 })
	if got := deliverTo(t, d); got != "C" {
		t.Fatalf("after A disconnected: got %s, want C", got)
	}
	if err := sc.Unsubscribe(); err != nil {
		t.Fatal(err)
	}
	if got := deliverTo(t, d); got != "B" {
		t.Fatalf("after C unsubscribed: got %s, want B", got)
	}
}

// One reader per side: the subscriber's event handler sends an add on the
// same session while the event is still open, and the add's reply has to
// share the pipe with the event and its reply. A reader that blocked on a
// reply would deadlock here.
func TestSessionEventAndAddInterleave(t *testing.T) {
	_, guest := echoServer(t)
	dir := shortTempDir(t)
	d := startSockDaemon(t, dir)
	pref := freePort(t)
	var s *Session
	s = testSession(t, dir, SessionOptions{OnEvent: func(json.RawMessage) json.RawMessage {
		host, _, _, err := s.Add(pref, guest, false)
		if err != nil {
			return json.RawMessage(strconv.Quote(err.Error()))
		}
		return json.RawMessage(strconv.Itoa(host))
	}})
	if err := s.Subscribe(); err != nil {
		t.Fatal(err)
	}
	r, err := d.deliver(json.RawMessage(`{}`), 2*time.Second)
	if err != nil || string(r) != strconv.Itoa(pref) {
		t.Fatalf("deliver = %s, %v; want the host port the handler's add got", r, err)
	}
	if got := fmt.Sprint(owners(d, guest)); got != "[connection]" {
		t.Fatalf("owners = %s, want the session's connection", got)
	}
}

// The wire itself: every request's reply echoes its id, every event carries
// an id, and an add sent while an event is unanswered is answered first.
func TestSessionWireIDs(t *testing.T) {
	_, guest := echoServer(t)
	dir := shortTempDir(t)
	d := startSockDaemon(t, dir)
	c, err := net.Dial("unix", socketPath(dir, "t"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	send := func(line string) {
		t.Helper()
		if _, err := c.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	recv := func() Response {
		t.Helper()
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		var r Response
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	send(`{"id":1,"op":"session"}`)
	if r := recv(); r.ID != 1 || !r.OK || r.Version != "test" {
		t.Fatalf("session reply = %+v", r)
	}
	send(`{"id":5,"op":"subscribe"}`)
	if r := recv(); r.ID != 5 || !r.OK {
		t.Fatalf("subscribe reply = %+v", r)
	}
	got := make(chan string, 1)
	go func() {
		r, err := d.deliver(json.RawMessage(`{"n":1}`), 2*time.Second)
		got <- fmt.Sprintf("%s %v", r, err)
	}()
	ev := recv()
	if ev.Event == nil || ev.Event.ID == 0 || string(ev.Event.Data) != `{"n":1}` {
		t.Fatalf("event line = %+v", ev)
	}
	pref := freePort(t)
	send(fmt.Sprintf(`{"id":6,"op":"add","host":%d,"guest":%d}`, pref, guest))
	if r := recv(); r.ID != 6 || !r.OK || r.Host != pref {
		t.Fatalf("add reply behind an open event = %+v", r)
	}
	send(fmt.Sprintf(`{"reply":{"id":%d,"data":"ok"}}`, ev.Event.ID))
	if g := <-got; g != `"ok" <nil>` {
		t.Fatalf("deliver returned %s", g)
	}
	send(`{"id":7,"op":"ping"}`)
	if r := recv(); r.ID != 7 || r.Sessions != 1 {
		t.Fatalf("ping reply = %+v", r)
	}
}

// A reply for an id the daemon no longer waits on (the event timed out) or
// never issued is dropped, and the session carries on.
func TestLateReplyDropped(t *testing.T) {
	dir := shortTempDir(t)
	d := startSockDaemon(t, dir)
	release := make(chan struct{})
	var calls atomic.Int32
	s := testSession(t, dir, SessionOptions{OnEvent: func(json.RawMessage) json.RawMessage {
		if calls.Add(1) == 1 {
			<-release // answer the first event only after it timed out
		}
		return json.RawMessage(`"late-or-not"`)
	}})
	if err := s.Subscribe(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.deliver(nil, 30*time.Millisecond); !errors.Is(err, errReplyTimeout) {
		t.Fatalf("first deliver = %v, want a timeout", err)
	}
	close(release) // the late reply now arrives and must be dropped
	sc := s.current()
	if err := sc.writeLine(replyLine{Reply: &EventReply{ID: 999}}); err != nil {
		t.Fatal(err)
	}
	if r, err := d.deliver(nil, 2*time.Second); err != nil || string(r) != `"late-or-not"` {
		t.Fatalf("deliver after a late reply = %s, %v", r, err)
	}
	if _, err := sc.call(Request{Op: OpPing}); err != nil {
		t.Fatalf("session broken by a late reply: %v", err)
	}
}

// Transport death keeps local sessions, and restore() re-binds the forwards
// they own.
func TestTransportDeathKeepsSessions(t *testing.T) {
	_, guest := echoServer(t)
	dir := shortTempDir(t)
	d := startSockDaemon(t, dir)
	first := d.tr.(*fakeTransport)
	second := newFakeTransport()
	d.dial = func() (transport, error) { return second, nil }
	s := testSession(t, dir, SessionOptions{})
	pref := freePort(t)
	if _, _, _, err := s.Add(pref, guest, false); err != nil {
		t.Fatal(err)
	}
	states := recordStates(d)
	done := runLoop(d)
	first.die()
	states.wait(t, StateReconnecting, StateUp) // the re-dial succeeds at once
	fs := d.list()
	if len(fs) != 1 || fs[0].Pending || fs[0].Host != pref || fmt.Sprint(fs[0].Owners) != "[connection]" {
		t.Fatalf("after restore: %+v", fs)
	}
	if !portOpen(pref) || second.binds.Load() != 1 || d.sessionCount() != 1 {
		t.Fatalf("open=%v binds=%d sessions=%d", portOpen(pref), second.binds.Load(), d.sessionCount())
	}
	if _, err := s.current().call(Request{Op: OpPing}); err != nil {
		t.Fatalf("session did not survive the transport: %v", err)
	}
	d.triggerStop()
	waitDone(t, done, "stop")
}

// exact is sticky: a reuse that asks for it sets it, and it stays after the
// owner that asked is gone. An exact request for a different host port than
// the forward has is refused like a busy port; a busy exact port is refused
// and not remembered.
func TestExactStickyThroughReuseAndOwnerExpiry(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	pref := freePort(t)
	if _, _, _, err := d.add(pref, guest, false, confOwner); err != nil {
		t.Fatal(err)
	}
	if fs := d.list(); fs[0].Exact {
		t.Fatal("a bumpable add came out exact")
	}
	if host, _, _, err := d.add(pref, guest, true, ttlOwner); err != nil || host != pref {
		t.Fatalf("exact reuse = %d, %v", host, err)
	}
	d.remove(guest, ttlOwner) // the owner that asked for exact expires
	if fs := d.list(); len(fs) != 1 || !fs[0].Exact {
		t.Fatalf("exact did not stick past its owner: %+v", fs)
	}
	if _, _, _, err := d.add(pref+1, guest, true, ttlOwner); !errors.Is(err, errPortBusy) {
		t.Fatalf("exact add at another port = %v, want busy", err)
	}

	occ, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()
	busy := occ.Addr().(*net.TCPAddr).Port
	_, g2 := echoServer(t)
	if _, _, _, err := d.add(busy, g2, true, ttlOwner); !errors.Is(err, errPortBusy) {
		t.Fatalf("exact add on a held port = %v, want busy", err)
	}
	if len(d.list()) != 1 {
		t.Fatalf("a refused exact add was remembered: %+v", d.list())
	}
}

// restore() never bumps an exact forward: with its port held it stays
// pending on that port, and the retry ticker binds it once the port frees.
func TestRestoreLeavesExactPendingAndTickerRebinds(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	d.retryEvery = 10 * time.Millisecond
	first := d.tr.(*fakeTransport)
	pref := freePort(t)
	if _, _, _, err := d.add(pref, guest, true, confOwner); err != nil {
		t.Fatal(err)
	}
	var occ net.Listener
	var redialed atomic.Bool
	second := newFakeTransport()
	d.dial = func() (transport, error) {
		if occ == nil {
			ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", pref))
			if err != nil {
				return nil, err // the old forward's listener is still closing
			}
			occ = ln
		}
		redialed.Store(true)
		return second, nil
	}
	done := runLoop(d)
	first.die()
	waitFor(t, "back up on the new transport", func() bool {
		st, _ := d.status()
		return redialed.Load() && st == StateUp
	})
	fs := d.list()
	if len(fs) != 1 || !fs[0].Pending || fs[0].Host != pref || !fs[0].Exact {
		t.Fatalf("exact forward after restore with its port held = %+v, want pending on %d", fs, pref)
	}
	time.Sleep(5 * d.retryEvery)
	if second.binds.Load() != 0 {
		t.Fatalf("exact forward bound (bumped?) while its port was held: %d binds", second.binds.Load())
	}
	occ.Close()
	waitFor(t, "ticker re-binds the freed port", func() bool {
		fs := d.list()
		return len(fs) == 1 && !fs[0].Pending && fs[0].Host == pref
	})
	if second.binds.Load() != 1 {
		t.Errorf("binds = %d, want 1", second.binds.Load())
	}
	d.triggerStop()
	waitDone(t, done, "stop")
}

// The session client reconnects when its daemon goes away (shutdown closes
// sessions and unlinks the socket) and re-subscribes on the one that comes
// back, so a held attach keeps getting events.
func TestSessionClientReconnectsAndResubscribes(t *testing.T) {
	dir := shortTempDir(t)
	d1 := startSockDaemon(t, dir)
	reconnected := make(chan struct{}, 1)
	s := testSession(t, dir, SessionOptions{
		OnEvent:     named("me"),
		OnReconnect: func() { reconnected <- struct{}{} },
		Logf:        t.Logf,
	})
	if err := s.Subscribe(); err != nil {
		t.Fatal(err)
	}
	if got := deliverTo(t, d1); got != "me" {
		t.Fatalf("first daemon: %s", got)
	}
	d1.shutdown()
	if _, err := os.Stat(socketPath(dir, "t")); !os.IsNotExist(err) {
		t.Fatalf("socket still there after shutdown: %v", err)
	}
	time.Sleep(30 * time.Millisecond) // a few failed redials against no socket
	d2 := startSockDaemon(t, dir)
	select {
	case <-reconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("session client did not reconnect")
	}
	d2.mu.Lock()
	n := len(d2.subs)
	d2.mu.Unlock()
	if n != 1 {
		t.Fatalf("new daemon has %d subscribers, want the re-subscribed session", n)
	}
	if got := deliverTo(t, d2); got != "me" {
		t.Fatalf("second daemon: %s", got)
	}
}

// A daemon from before sessions answers "unknown op" with no id; the client
// reads that reply before its reader starts and says what to do.
func TestOpenSessionOnPreSessionDaemon(t *testing.T) {
	dir := shortTempDir(t)
	if err := os.MkdirAll(config.RuntimeDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", socketPath(dir, "t"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		bufio.NewReader(c).ReadString('\n')
		c.Write([]byte(`{"ok":false,"err":"unknown op: session"}` + "\n"))
	}()
	_, err = openSession("t", func(<-chan struct{}) (net.Conn, error) { return net.Dial("unix", socketPath(dir, "t")) },
		SessionOptions{}, time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "predates sessions") {
		t.Fatalf("open on an old daemon = %v", err)
	}
}

// Every forward binds 127.0.0.1 and, where the host has it, ::1; another
// process on [::1]:P does not make P busy (IPv4 decides).
func TestForwardDualStack(t *testing.T) {
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	}
	probe.Close()
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	pref := freePort(t)
	if _, _, _, err := d.add(pref, guest, false, confOwner); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{fmt.Sprintf("127.0.0.1:%d", pref), fmt.Sprintf("[::1]:%d", pref)} {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("forward not listening on %s: %v", addr, err)
		}
		c.Write([]byte("x\n"))
		buf := make([]byte, 2)
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(buf); err != nil || string(buf) != "x\n" {
			t.Fatalf("echo via %s = %q, %v", addr, buf, err)
		}
		c.Close()
	}

	// Hold [::1]:Q only; Q must still bind, exactly, on IPv4.
	var held net.Listener
	var q int
	for i := 0; i < 20; i++ {
		ln, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			t.Fatal(err)
		}
		p := ln.Addr().(*net.TCPAddr).Port
		if v4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			v4.Close()
			held, q = ln, p
			break
		}
		ln.Close()
	}
	if held == nil {
		t.Skip("no port free on IPv4 while held on ::1")
	}
	defer held.Close()
	_, g2 := echoServer(t)
	host, bumped, _, err := d.add(q, g2, true, confOwner)
	if err != nil || bumped || host != q {
		t.Fatalf("add with [::1]:%d held = %d bumped=%v %v; want it bound on IPv4", q, host, bumped, err)
	}
	if !portOpen(q) {
		t.Fatal("IPv4 side not listening")
	}
}

// ssh forwards use one `localhost:` spec, so OpenSSH binds every loopback
// family and there is one -O cancel per forward.
func TestSSHForwardSpecIsDualStack(t *testing.T) {
	if got := sshForwardSpec(3000, 3001); got != "localhost:3000:localhost:3001" {
		t.Fatalf("spec = %q", got)
	}
}

// A forward from a daemon older than owners lists none and counts as
// configured; ConfCount counts conf owners only.
func TestForwardConfCounting(t *testing.T) {
	st := Status{Forwards: []Forward{
		{Guest: 1},                           // pre-owner daemon
		{Guest: 2, Owners: []string{"conf"}}, // configured
		{Guest: 3, Owners: []string{"connection", "ttl"}},
		{Guest: 4, Owners: []string{"conf", "connection"}},
	}}
	if n := st.ConfCount(); n != 3 {
		t.Fatalf("ConfCount = %d, want 3", n)
	}
}
