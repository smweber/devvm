package session

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingTransport counts how many of its forwards were closed one by one,
// so a test can tell a per-forward close from the transport's own Close.
type countingTransport struct {
	*fakeTransport
	perForward atomic.Int32
}

type countedCloser struct {
	io.Closer
	n *atomic.Int32
}

func (c countedCloser) Close() error { c.n.Add(1); return c.Closer.Close() }

func (c *countingTransport) forward(h, g int) (io.Closer, error) {
	cl, err := c.fakeTransport.forward(h, g)
	if err != nil {
		return nil, err
	}
	return countedCloser{cl, &c.perForward}, nil
}

// Teardown closes the transport, and the transport drops every forward it
// carries: no forward is closed one by one (against a wedged link each
// would wait out its own timeout), and every port is released all the same.
func TestTeardownClosesTransportNotForwards(t *testing.T) {
	d := newTestDaemon(t)
	tr := &countingTransport{fakeTransport: newFakeTransport()}
	d.tr = tr
	var ports []int
	for i := 0; i < 3; i++ {
		_, g := echoServer(t)
		p := freePort(t)
		if _, _, _, err := d.add(p, g, false, confOwner); err != nil {
			t.Fatal(err)
		}
		ports = append(ports, p)
	}
	d.teardown()
	if !tr.closed.Load() {
		t.Fatal("transport not closed")
	}
	if n := tr.perForward.Load(); n != 0 {
		t.Fatalf("teardown closed %d forwards one by one", n)
	}
	for _, p := range ports {
		if portOpen(p) {
			t.Errorf("port %d still open after the transport closed", p)
		}
	}
	if fs := d.list(); len(fs) != 3 || !fs[0].Pending {
		t.Fatalf("forwards after teardown = %+v, want all kept pending", fs)
	}
	// `ports rm` still closes its one forward itself.
	d2 := newTestDaemon(t)
	tr2 := &countingTransport{fakeTransport: newFakeTransport()}
	d2.tr = tr2
	_, g := echoServer(t)
	p := freePort(t)
	d2.add(p, g, false, confOwner)
	d2.remove(g, confOwner)
	if tr2.perForward.Load() != 1 || portOpen(p) {
		t.Fatalf("remove: per-forward closes %d, open=%v", tr2.perForward.Load(), portOpen(p))
	}
}

// A transport death drops relay forwards and closes the transport; the
// relays' forwards go with the transport, not one by one.
func TestOnDeadClosesTransportOnly(t *testing.T) {
	d := newTestDaemon(t)
	tr := &countingTransport{fakeTransport: newFakeTransport()}
	d.tr = tr
	d.mu.Lock()
	d.sessions[9] = &sess{id: 9, conn: nopConn{}, relay: true, pending: map[int64]chan json.RawMessage{}, done: make(chan struct{})}
	d.mu.Unlock()
	_, g1 := echoServer(t)
	_, g2 := echoServer(t)
	p1, p2 := freePort(t), freePort(t)
	d.add(p1, g1, false, connOwner(9))
	d.add(p2, g2, false, confOwner)
	d.onDead()
	if tr.perForward.Load() != 0 || !tr.closed.Load() || portOpen(p1) || portOpen(p2) {
		t.Fatalf("per-forward %d, closed %v, open %v/%v", tr.perForward.Load(), tr.closed.Load(), portOpen(p1), portOpen(p2))
	}
	fs := d.list()
	if len(fs) != 1 || fs[0].Guest != g2 || !fs[0].Pending {
		t.Fatalf("after onDead: %+v, want only the conf forward, pending", fs)
	}
}

type nopConn struct{ net.Conn }

func (nopConn) Close() error { return nil }

// A forwardSet refuses (and closes) a forward bound after the transport
// closed, so nothing outlives the Close that should have dropped it.
func TestForwardSetRefusesAfterClose(t *testing.T) {
	var s forwardSet
	var closed atomic.Int32
	c, err := s.track(countedCloser{io.NopCloser(nil), &closed})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	c.Close() // idempotent
	s.track(countedCloser{io.NopCloser(nil), &closed})
	s.closeAll()
	if n := closed.Load(); n != 2 {
		t.Fatalf("closes = %d, want 2 (one explicit, one by closeAll)", n)
	}
	if _, err := s.track(countedCloser{io.NopCloser(nil), &closed}); !errors.Is(err, errTransportClosed) || closed.Load() != 3 {
		t.Fatalf("track after close = %v (closes %d)", err, closed.Load())
	}
}

// Owners carry data: a ttl owner's expiry is on the wire, refreshed (never
// shortened) by a repeat, and absent for other kinds.
func TestOwnerInfoExpiry(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)
	pref := freePort(t)
	t0 := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if _, _, _, err := d.add(pref, guest, false, confOwner); err != nil {
		t.Fatal(err)
	}
	if fs := d.list(); fs[0].Expires != nil {
		t.Fatalf("conf-only forward has an expiry: %v", fs[0].Expires)
	}
	b, _ := json.Marshal(d.list()[0])
	if strings.Contains(string(b), "expires") {
		t.Fatalf("wire carries expires with no ttl owner: %s", b)
	}
	d.addOwned(pref, guest, true, ttlOwner, ttlFor(t0, 10*time.Minute))
	d.addOwned(pref, guest, true, ttlOwner, ttlFor(t0.Add(time.Minute), 10*time.Minute))
	d.addOwned(pref, guest, true, ttlOwner, ttlFor(t0, time.Minute)) // earlier: ignored
	fs := d.list()
	if fs[0].Expires == nil || !fs[0].Expires.Equal(t0.Add(11*time.Minute)) {
		t.Fatalf("expires = %v, want %v", fs[0].Expires, t0.Add(11*time.Minute))
	}
	b, _ = json.Marshal(fs[0])
	var back Forward
	if err := json.Unmarshal(b, &back); err != nil || back.Expires == nil || !back.Expires.Equal(*fs[0].Expires) {
		t.Fatalf("round trip %s -> %+v, %v", b, back, err)
	}
}

// The ticker's expiry drops lapsed ttl owners: a ttl-only forward closes,
// one a conf owner also holds stays for it.
func TestExpireTTL(t *testing.T) {
	_, g1 := echoServer(t)
	_, g2 := echoServer(t)
	d := newTestDaemon(t)
	now := time.Now()
	d.now = func() time.Time { return now }
	p1, p2 := freePort(t), freePort(t)
	d.addOwned(p1, g1, true, ttlOwner, ttlFor(now, 10*time.Minute))
	d.addOwned(p2, g2, true, ttlOwner, ttlFor(now, 10*time.Minute))
	d.add(p2, g2, false, confOwner)
	d.expireTTL()
	if len(d.list()) != 2 {
		t.Fatal("a ttl expired early")
	}
	now = now.Add(10 * time.Minute)
	d.expireTTL()
	fs := d.list()
	if len(fs) != 1 || fs[0].Guest != g2 || fs[0].Expires != nil || !fs[0].Exact {
		t.Fatalf("after expiry: %+v, want the conf forward only (still exact: sticky)", fs)
	}
	if portOpen(p1) || !portOpen(p2) {
		t.Fatalf("open after expiry: ttl-only %v, conf %v", portOpen(p1), portOpen(p2))
	}
}

// rebindPending leaves a forward only a relay holds: its far daemon re-adds
// through its own reconnect.
func TestRebindPendingSkipsRelayOnly(t *testing.T) {
	d := newTestDaemon(t)
	d.mu.Lock()
	d.sessions[4] = &sess{id: 4, relay: true}
	d.sessions[5] = &sess{id: 5}
	d.forwards[100] = newFwd(freePort(t), 100, false, connOwner(4), ownerInfo{})
	d.forwards[101] = newFwd(freePort(t), 101, false, connOwner(5), ownerInfo{})
	d.setStateLocked(StateReconnecting)
	d.mu.Unlock()
	fresh := newFakeTransport()
	if !d.restore(fresh) {
		t.Fatal("restore failed")
	}
	if n := fresh.binds.Load(); n != 1 {
		t.Fatalf("binds = %d, want only the local session's forward", n)
	}
	d.mu.Lock()
	relayPending, localBound := d.forwards[100].closer == nil, d.forwards[101].closer != nil
	d.mu.Unlock()
	if !relayPending || !localBound {
		t.Fatalf("relay forward pending=%v, local forward bound=%v", relayPending, localBound)
	}
}

// subTransport records the subscription hook's calls, in order with the
// daemon's state changes (both land in the same log).
type subTransport struct {
	*fakeTransport
	mu    sync.Mutex
	log   *[]string
	block chan struct{} // non-nil: every call waits on it
	err   error
}

func (s *subTransport) setSubscribed(on bool) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		*s.log = append(*s.log, "sub")
	} else {
		*s.log = append(*s.log, "unsub")
	}
	return s.err
}

func (s *subTransport) calls() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(*s.log, ",")
}

// The hook: every subscribe re-sends upstream before its ack, the last
// unsubscribe (or disconnect) sends one unsubscribe, and restore()
// re-subscribes a fresh transport after its binds and before StateUp.
func TestSubscriberHook(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	var log []string
	tr := &subTransport{fakeTransport: newFakeTransport(), log: &log}
	d.mu.Lock()
	d.tr = tr
	d.mu.Unlock()
	a := testSession(t, dir, SessionOptions{})
	b := testSession(t, dir, SessionOptions{})
	if err := a.Subscribe(); err != nil {
		t.Fatal(err)
	}
	if got := tr.calls(); got != "sub" {
		t.Fatalf("after the first subscribe's ack: %q", got)
	}
	if err := b.Subscribe(); err != nil {
		t.Fatal(err)
	}
	if err := a.Unsubscribe(); err != nil {
		t.Fatal(err)
	}
	if got := tr.calls(); got != "sub,sub" {
		t.Fatalf("a subscriber left while another remains: %q", got)
	}
	b.Close() // disconnect is the last subscriber leaving
	waitFor(t, "the upstream unsubscribe", func() bool { return tr.calls() == "sub,sub,unsub" })

	// restore() on a fresh transport: subscribed again before the daemon
	// reports up, only because a subscriber is held across the outage.
	if err := a.Subscribe(); err != nil {
		t.Fatal(err)
	}
	var log2 []string
	fresh := &subTransport{fakeTransport: newFakeTransport(), log: &log2}
	d.mu.Lock()
	d.onState = func(s string) { fresh.mu.Lock(); log2 = append(log2, s); fresh.mu.Unlock() }
	d.setStateLocked(StateReconnecting)
	d.mu.Unlock()
	if !d.restore(fresh) {
		t.Fatal("restore failed")
	}
	if got := fresh.calls(); got != "reconnecting,sub,up" {
		t.Fatalf("restore order = %q, want the hook between the outage and up", got)
	}
}

// A hook that never answers costs a subscribe its bound, not forever, and
// the ack says the upstream half failed.
func TestSubscriberHookTimeout(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	d.upTimeout = 50 * time.Millisecond
	var log []string
	tr := &subTransport{fakeTransport: newFakeTransport(), log: &log, block: make(chan struct{})}
	defer close(tr.block)
	d.mu.Lock()
	d.tr = tr
	d.mu.Unlock()
	a := testSession(t, dir, SessionOptions{})
	err := a.Subscribe()
	if err == nil || !strings.Contains(err.Error(), "not upstream") {
		t.Fatalf("subscribe against a wedged upstream = %v", err)
	}
	d.mu.Lock()
	n := len(d.subs)
	d.mu.Unlock()
	if n != 1 {
		t.Fatalf("local registration dropped on an upstream failure: %d subscribers", n)
	}
	// And a restore against it is bounded too, and still comes up.
	d.mu.Lock()
	d.setStateLocked(StateReconnecting)
	d.mu.Unlock()
	if !d.restore(tr) {
		t.Fatal("restore failed on a wedged upstream subscribe")
	}
}

// Upstream calls stay ordered: a subscribe that times out and answers late
// cannot land after the unsubscribe that followed it; the late answer
// triggers a re-sync, and the last desired state (unsubscribed) wins.
func TestSubscriberHookLateAnswerOrdered(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	d.upTimeout = 30 * time.Millisecond
	var log []string
	tr := &subTransport{fakeTransport: newFakeTransport(), log: &log, block: make(chan struct{})}
	d.mu.Lock()
	d.tr = tr
	d.mu.Unlock()
	a := testSession(t, dir, SessionOptions{})
	if err := a.Subscribe(); err == nil {
		t.Fatal("a wedged upstream subscribe was acked as done")
	}
	if err := a.Unsubscribe(); err != nil {
		t.Fatal(err)
	}
	if got := tr.calls(); got != "" {
		t.Fatalf("an unsubscribe was sent past the call still in flight: %q", got)
	}
	close(tr.block) // the late subscribe lands now, then the re-sync
	waitFor(t, "the re-sync after the late answer", func() bool { return tr.calls() == "sub,unsub" })
}
