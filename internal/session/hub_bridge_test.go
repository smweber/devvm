package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/agentrpc"
	"github.com/smweber/devvm/internal/config"
)

// The bridge across a relay (hub.md §8, roadmap step 8), in process: the
// step 6 rig (a hub daemon, `__session`'s Relay over os.Pipe, a laptop
// daemon on a hubTransport with a fake -L), plus a control socket on the
// laptop daemon so local sessions (a laptop attach/auth) can subscribe to
// it. A hub event is the hub daemon's own handler (bridgeOpen), exactly
// what its smol transport's events() would start.

const testShim = "/usr/local/bin/devvm-open-url"

// serveLaptop gives the laptop rig's daemon a control socket.
func serveLaptop(t *testing.T, l *laptopRig) config.Paths {
	t.Helper()
	dir := l.d.paths
	if err := os.MkdirAll(dir.RuntimeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, info, err := listenControl(socketPath(dir, l.d.name))
	if err != nil {
		t.Fatal(err)
	}
	l.d.ln, l.d.sockInfo = ln, info
	go l.d.serveControl()
	t.Cleanup(func() { ln.Close(); l.d.closeAllSessions() })
	return dir
}

// sessionOn opens a session client on name's daemon in dir, spawning
// nothing, and subscribes it answering with h.
func sessionOn(t *testing.T, dir config.Paths, name string, h func(json.RawMessage) json.RawMessage) *Session {
	t.Helper()
	s, err := openSession(name, func(<-chan struct{}) (net.Conn, error) {
		return net.Dial("unix", socketPath(dir, name))
	}, SessionOptions{OnEvent: h}, 5*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Subscribe(); err != nil {
		t.Fatalf("subscribe on %s: %v", name, err)
	}
	return s
}

// tagged answers every event with opened:true and its own tag as the URL,
// recording what it saw, so a test can tell which subscriber got an open.
func tagged(tag string, c *catcher) func(json.RawMessage) json.RawMessage {
	c.reply = agentrpc.OpenReply{Opened: true, URL: tag}
	return c.handle
}

func subCount(d *daemon) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.subs)
}

// The laptop daemon subscribes on the hub on its first local subscriber
// and unsubscribes when the last one leaves; the local subscribe is acked
// only once the hub has registered the relay (the ack composes across the
// hop, so no waiting is needed to see it on the hub); its ping reports the
// shim the hub's transport installed.
func TestHubUpstreamSubscribeFollowsLocalSubscribers(t *testing.T) {
	h := newHubRig(t)
	h.tr.shim = testShim
	l := newLaptopRig(t, h, true)
	dir := serveLaptop(t, l)

	a := sessionOn(t, dir, "h/web", (&catcher{}).handle)
	h.d.mu.Lock()
	n, relayFirst := len(h.d.subs), len(h.d.subs) > 0 && h.d.subs[0].relay
	h.d.mu.Unlock()
	if n != 1 || !relayFirst {
		t.Fatalf("after the local ack the hub has %d subscribers (relay first: %v), want the relay", n, relayFirst)
	}
	if info, err := a.Ping(); err != nil || info.Shim != testShim || info.State != StateUp {
		t.Fatalf("laptop ping = %+v, %v; want up with the hub's shim %q", info, err, testShim)
	}
	c := sessionOn(t, dir, "h/web", (&catcher{}).handle)
	c.Close()
	waitFor(t, "the laptop to drop C", func() bool { return subCount(l.d) == 1 })
	if subCount(h.d) != 1 {
		t.Fatal("the hub lost the relay while a local subscriber remained")
	}
	a.Close()
	waitFor(t, "the relay to unsubscribe on the hub", func() bool { return subCount(h.d) == 0 })
	if len(h.d.list()) != 0 || h.d.sessionCount() != 1 {
		t.Fatalf("hub after the unsubscribe: forwards %+v, sessions %d; want the relay session held, nothing else", h.d.list(), h.d.sessionCount())
	}
}

// The ordering in detail, against a scripted hub: the local subscribe is
// acked only after the hub's ack (an ordered log of the hub answering, the
// daemon's hook seeing that answer, and the local ack, which a daemon
// acking early would put out of order: the hub holds its answer until the
// test has given an early ack time to land); a second local subscriber
// re-sends subscribe (moving this host to the front there); one leaving
// while another stays sends nothing; the last leaving unsubscribes.
func TestHubLocalAckWaitsForUpstreamAck(t *testing.T) {
	var mu sync.Mutex
	var ops, order []string
	note := func(what string) {
		mu.Lock()
		order = append(order, what)
		mu.Unlock()
	}
	seen := make(chan string, 16)
	release := make(chan struct{})
	conn, stop := scriptedHubOpen(t,
		func(Request) Response {
			return Response{OK: true, State: StateUp, Relay: true, Bridge: true, Shim: testShim}
		},
		func(req Request) Response {
			if req.Op == OpSubscribe || req.Op == OpUnsubscribe {
				mu.Lock()
				ops = append(ops, req.Op)
				first := len(ops) == 1
				mu.Unlock()
				seen <- req.Op
				if first {
					<-release // the hub is slow to ack the first subscribe
					note("hub acks")
				}
			}
			return Response{OK: true}
		})
	ht, err := openHubTransport("h/web", conn, newFakeLocal(), stop, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := &laptopRig{d: newDaemon(shortTempPaths(t), "h/web", "test", ht, func() (transport, error) {
		return nil, errors.New("no dial in this test")
	})}
	l.d.logf = t.Logf
	var hooked sync.Once
	l.d.onUpstream = func(on bool, err error) {
		hooked.Do(func() { note(fmt.Sprintf("daemon sees upstream(%v)=%v", on, err)) })
	}
	t.Cleanup(func() { releaseDaemon(l.d) })
	dir := serveLaptop(t, l)

	a, err := openSession("h/web", func(<-chan struct{}) (net.Conn, error) {
		return net.Dial("unix", socketPath(dir, "h/web"))
	}, SessionOptions{}, 5*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	acked := make(chan error, 1)
	go func() {
		err := a.Subscribe()
		note("local ack")
		acked <- err
	}()
	if op := <-seen; op != OpSubscribe {
		t.Fatalf("first upstream op = %s", op)
	}
	// Give an early ack every chance to land while the hub still holds its
	// answer: a correct daemon cannot ack here at all, so this wait never
	// decides a passing run, only how surely a broken one is caught.
	select {
	case err := <-acked:
		t.Fatalf("the local subscribe was acked (%v) before the hub's ack", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-acked; err != nil {
		t.Fatalf("local subscribe: %v", err)
	}
	mu.Lock()
	got := strings.Join(order, " / ")
	mu.Unlock()
	if want := "hub acks / daemon sees upstream(true)=<nil> / local ack"; got != want {
		t.Fatalf("order = %q, want %q", got, want)
	}

	c := sessionOn(t, dir, "h/web", nil)
	if op := <-seen; op != OpSubscribe {
		t.Fatalf("a second local subscriber sent %s upstream, want a re-sent subscribe", op)
	}
	c.Close()
	waitFor(t, "the laptop to drop C", func() bool { return subCount(l.d) == 1 })
	a.Close()
	if op := <-seen; op != OpUnsubscribe {
		t.Fatalf("the last subscriber leaving sent %s upstream", op)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(ops, " "); got != "subscribe subscribe unsubscribe" {
		t.Fatalf("upstream ops = %q; C leaving while A stayed must send nothing", got)
	}
}

// nullBindTransport is a hub transport whose forwards bind nothing on this
// host (the hub is another host in real life; here the two share a
// loopback, and a hub listener would hold the laptop's port). A port in
// busy is refused, as a port held on the hub.
type nullBindTransport struct {
	*fakeTransport
	mu   sync.Mutex
	busy map[int]bool
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func (n *nullBindTransport) forward(h, g int) (io.Closer, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.busy[h] {
		return nil, errPortBusy
	}
	return nopCloser{}, nil
}

// startHubDaemon is a hub daemon for "web" on tr with a control socket and
// its loop running.
func startHubDaemon(t *testing.T, tr transport) *hubRig {
	t.Helper()
	dir := shortTempPaths(t)
	if err := os.MkdirAll(dir.RuntimeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	d := newDaemon(dir, "web", "test", tr, func() (transport, error) { return nil, errors.New("no dial in this test") })
	d.logf = func(f string, a ...any) { t.Logf("hub: "+f, a...) }
	ln, info, err := listenControl(socketPath(dir, "web"))
	if err != nil {
		t.Fatal(err)
	}
	d.ln, d.sockInfo = ln, info
	go d.serveControl()
	done := runLoop(d)
	t.Cleanup(func() {
		d.triggerStop()
		<-done
		ln.Close()
		d.closeAllSessions()
		releaseDaemon(d)
	})
	return &hubRig{t: t, dir: dir, d: d}
}

// An event from hubTransport.events() runs the laptop daemon's own
// handler: it binds on the hub transport (a relay-owned add on the hub,
// never exact, then the -L), delivers to the local subscriber with the
// laptop's port, and the local reply is the hub's answer to its guest. A
// redirect is exact here and ttl-owned; a port held on the hub bumps
// there and changes nothing here (the relay binds nothing: busy hub ports
// never refuse a laptop login).
func TestHubEventBindsThroughRelayAndRepliesUpstream(t *testing.T) {
	tr := &nullBindTransport{fakeTransport: newEventTransport(), busy: map[int]bool{}}
	tr.shim = testShim
	h := startHubDaemon(t, tr)
	l := newLaptopRig(t, h, true)
	dir := serveLaptop(t, l)
	var mu sync.Mutex
	var opened []string
	a := sessionOn(t, dir, "h/web", OpenOnHost(io.Discard, func(u string) error {
		mu.Lock()
		opened = append(opened, u)
		mu.Unlock()
		return nil
	}))
	_ = a

	p := farPort(t)
	raw := fmt.Sprintf("http://localhost:%d/app", p)
	rep := h.d.bridgeOpen(raw)
	if !rep.Opened || rep.Guest != p || rep.HostPort != p || rep.URL != raw {
		t.Fatalf("hub's reply = %+v, want opened on the laptop's port %d", rep, p)
	}
	mu.Lock()
	if len(opened) != 1 || opened[0] != raw {
		t.Fatalf("opened on the laptop: %v", opened)
	}
	mu.Unlock()
	lf := l.d.list()
	if len(lf) != 1 || lf[0].Host != p || lf[0].Guest != p || fmt.Sprint(lf[0].Owners) != "[connection]" || lf[0].Exact {
		t.Fatalf("laptop forwards = %+v, want the local session's direct forward", lf)
	}
	hf, ok := h.forward(p)
	if !ok || fmt.Sprint(hf.Owners) != "[connection]" || hf.Exact || l.lastLocal().lastHubPort() != hf.Host {
		t.Fatalf("hub forward = %+v (found %v), -L to hub port %d; want the relay's, not exact", hf, ok, l.lastLocal().lastHubPort())
	}

	q := farPort(t)
	tr.mu.Lock()
	tr.busy[q] = true // held on the hub
	tr.mu.Unlock()
	rep = h.d.bridgeOpen(redirectURL("localhost", q))
	if !rep.Opened || rep.HostPort != q {
		t.Fatalf("redirect reply = %+v, want opened with the laptop's exact %d", rep, q)
	}
	var cb Forward
	for _, f := range l.d.list() {
		if f.Guest == q {
			cb = f
		}
	}
	if cb.Host != q || !cb.Exact || fmt.Sprint(cb.Owners) != "[ttl]" || cb.Expires == nil {
		t.Fatalf("laptop callback forward = %+v, want exact %d, ttl-owned", cb, q)
	}
	if hf, _ := h.forward(q); hf.Host != q+1 || hf.Exact || l.lastLocal().lastHubPort() != q+1 {
		t.Fatalf("hub callback hop = %+v, -L to %d; want bumped to %d on the hub, not exact", hf, l.lastLocal().lastHubPort(), q+1)
	}
}

// Priority across a relay is per host (hub.md §8): laptop A, desktop B,
// laptop C subscribe; each newest gets the open; C leaves, and the next
// event goes to A, not B, because the laptop's relay keeps the place C's
// re-subscribe gave it.
func TestHubPriorityIsPerHost(t *testing.T) {
	h := newHubRig(t)
	l := newLaptopRig(t, h, true)
	dir := serveLaptop(t, l)
	var ca, cb, cc catcher
	sessionOn(t, dir, "h/web", tagged("A", &ca))
	if rep := h.d.bridgeOpen("https://example.com/1"); rep.URL != "A" {
		t.Fatalf("with A alone the open went to %q", rep.URL)
	}
	sessionOn(t, h.dir, "web", tagged("B", &cb))
	if rep := h.d.bridgeOpen("https://example.com/2"); rep.URL != "B" {
		t.Fatalf("after desktop B subscribed the open went to %q", rep.URL)
	}
	c := sessionOn(t, dir, "h/web", tagged("C", &cc))
	if rep := h.d.bridgeOpen("https://example.com/3"); rep.URL != "C" {
		t.Fatalf("after laptop C subscribed the open went to %q", rep.URL)
	}
	c.Close()
	waitFor(t, "the laptop to drop C", func() bool { return subCount(l.d) == 1 })
	if rep := h.d.bridgeOpen("https://example.com/4"); rep.URL != "A" {
		t.Fatalf("after C left the open went to %q, want laptop A (host-level priority)", rep.URL)
	}
	if n := len(cb.events()); n != 1 {
		t.Fatalf("desktop B saw %d opens, want only the one made while it was newest", n)
	}
}

// Version skew: a hub daemon that does not echo Bridge (v0.1.14, or the
// unreleased step 7 build) still carries forwards, but a local subscribe
// is answered "hub devvm too old for browser opens" and nothing is sent
// upstream: such a hub would ack and then never deliver.
func TestHubTooOldForBrowserOpens(t *testing.T) {
	if HubBridgeMinVersion != "v0.1.15" {
		t.Fatalf("HubBridgeMinVersion = %s; it must be the first tag that ships roadmap step 8", HubBridgeMinVersion)
	}
	var mu sync.Mutex
	var ops []string
	conn, stop := scriptedHubOpen(t,
		func(Request) Response { return Response{OK: true, State: StateUp, Version: "v0.1.14", Relay: true} },
		func(req Request) Response {
			mu.Lock()
			ops = append(ops, req.Op)
			mu.Unlock()
			if req.Op == OpAdd {
				return Response{OK: true, Host: req.Guest + 1}
			}
			return Response{OK: true}
		})
	local := newFakeLocal()
	ht, err := openHubTransport("h/web", conn, local, stop, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := &laptopRig{d: newDaemon(shortTempPaths(t), "h/web", "test", ht, func() (transport, error) {
		return nil, errors.New("no dial in this test")
	})}
	l.d.logf = t.Logf
	t.Cleanup(func() { releaseDaemon(l.d) })
	dir := serveLaptop(t, l)

	pref := farPort(t)
	if host, _, pending, err := l.d.add(pref, pref, false, confOwner); err != nil || pending || host != pref {
		t.Fatalf("forward on an old hub = %d pending=%v err=%v; forwards must still work", host, pending, err)
	}
	s, err := openSession("h/web", func(<-chan struct{}) (net.Conn, error) {
		return net.Dial("unix", socketPath(dir, "h/web"))
	}, SessionOptions{}, 5*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Subscribe()
	if err == nil || !strings.Contains(err.Error(), "hub devvm too old for browser opens") || !strings.Contains(err.Error(), HubBridgeMinVersion) {
		t.Fatalf("subscribe through an old hub = %v", err)
	}
	if info, _ := s.Ping(); info.Shim != "" {
		t.Fatalf("an old hub reported shim %q", info.Shim)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, op := range ops {
		if op == OpSubscribe {
			t.Fatalf("a subscribe was sent to a hub too old to deliver: %v", ops)
		}
	}
}

// The daemon reports its transport's shim on ping and on the session open
// (what a relay learns it from); a transport without one reports none.
func TestPingReportsShim(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	d.mu.Lock()
	d.tr.(*fakeTransport).shim = testShim
	d.mu.Unlock()
	cl := &Client{paths: dir, name: "t"}
	if shim, err := cl.Shim(); err != nil || shim != testShim {
		t.Fatalf("one-shot ping shim = %q, %v", shim, err)
	}
	d.mu.Lock()
	d.tr.(*fakeTransport).ev = make(chan bridgeRequest) // a smol transport: it carries events
	d.mu.Unlock()
	l := dialLine(t, dir, "t")
	if open := l.open(Request{Relay: true}); !open.Bridge || open.NoEvents || open.Shim != testShim {
		t.Fatalf("relay open reply = %+v, want the bridge echo and the shim", open)
	}
	d.mu.Lock()
	d.tr.(*fakeTransport).shim = ""
	d.mu.Unlock()
	if shim, err := cl.Shim(); err != nil || shim != "" {
		t.Fatalf("no shim: %q, %v", shim, err)
	}
}

// The Bridge echo follows the transport: a daemon whose transport carries
// no browser events (events() nil here; an ssh box without an agent is
// TestRelayBridgeEchoForRemoteMachine) says NoEvents instead, so a laptop
// never subscribes "successfully" through it and then hears nothing, and
// can tell it from a hub too old to echo Bridge at all.
func TestBridgeEchoOnlyWithEvents(t *testing.T) {
	dir := shortTempPaths(t)
	startSockDaemon(t, dir) // fakeTransport: ev nil, as ssh
	open := dialLine(t, dir, "t").open(Request{Relay: true})
	if !open.OK || open.Bridge || !open.NoEvents {
		t.Fatalf("relay open on a transport without events = %+v, want NoEvents and no Bridge", open)
	}
}

// openSession reads the Bridge decision under the lock that admits the
// relay, and returns it: an onDead after admission cannot turn a smol
// machine's echo into NoEvents.
func TestOpenSessionReportsBridgeAtAdmission(t *testing.T) {
	d := newDaemon(shortTempPaths(t), "t", "test", newEventTransport(), func() (transport, error) {
		return nil, errors.New("no dial in this test")
	})
	d.logf = t.Logf
	t.Cleanup(func() { releaseDaemon(d) })
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	s, bridge, err := d.openSession(a, true)
	if err != nil || !bridge {
		t.Fatalf("relay admitted on a transport with events: bridge=%v err=%v", bridge, err)
	}
	d.closeSession(s)
	dropTransport(d) // between transports, as onDead leaves it
	if _, _, err := d.openSession(b, true); err != errRelayRefused {
		t.Fatalf("relay while down: %v, want refused", err)
	}
	d.mu.Lock()
	d.tr = newFakeTransport() // ssh-like: no events
	d.setStateLocked(StateUp)
	d.mu.Unlock()
	s, bridge, err = d.openSession(b, true)
	if err != nil || bridge {
		t.Fatalf("relay on a transport without events: bridge=%v err=%v", bridge, err)
	}
	d.closeSession(s)
}

// A laptop relay to a current hub daemon whose machine has no events: the
// local subscribe is refused as such (not as a hub too old), forwards still
// work, and nothing is sent upstream.
func TestHubMachineWithoutEvents(t *testing.T) {
	var mu sync.Mutex
	var ops []string
	conn, stop := scriptedHubOpen(t,
		func(Request) Response {
			return Response{OK: true, State: StateUp, Version: "test", Relay: true, NoEvents: true}
		},
		func(req Request) Response {
			mu.Lock()
			ops = append(ops, req.Op)
			mu.Unlock()
			if req.Op == OpAdd {
				return Response{OK: true, Host: req.Guest + 1}
			}
			return Response{OK: true}
		})
	ht, err := openHubTransport("h/box", conn, newFakeLocal(), stop, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := &laptopRig{d: newDaemon(shortTempPaths(t), "h/box", "test", ht, func() (transport, error) {
		return nil, errors.New("no dial in this test")
	})}
	l.d.logf = t.Logf
	t.Cleanup(func() { releaseDaemon(l.d) })
	if l.d.carriesEvents() {
		t.Fatal("a laptop daemon on a hub relay without events claims events")
	}
	dir := serveLaptop(t, l)

	pref := farPort(t)
	if host, _, pending, err := l.d.add(pref, pref, false, confOwner); err != nil || pending || host != pref {
		t.Fatalf("forward = %d pending=%v err=%v; forwards must still work", host, pending, err)
	}
	s, err := openSession("h/box", func(<-chan struct{}) (net.Conn, error) {
		return net.Dial("unix", socketPath(dir, "h/box"))
	}, SessionOptions{}, 5*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Subscribe()
	if err == nil || !HubNoEvents(err) || !BridgeRefusedForGood(err) || strings.Contains(err.Error(), "too old") {
		t.Fatalf("subscribe through a hub machine without events = %v", err)
	}
	if !strings.Contains(err.Error(), "box on hub h carries no browser events there") {
		t.Fatalf("refusal does not name the machine: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, op := range ops {
		if op == OpSubscribe {
			t.Fatalf("a subscribe was sent to a hub daemon without events: %v", ops)
		}
	}
}

// BridgeRefusedForGood: what the laptop's attach stops retrying on.
func TestBridgeRefusedForGood(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("forward daemon for 'h/web' exited: h/web: devvm on hub h predates hub forwards (needs v0.1.14 or newer); run 'devvm update' there"), true},
		{errors.New("h/web: the forward daemon for web on hub h predates hub forwards; restart it there"), true},
		{errors.New(NotUpstreamPrefix + errHubTooOld.Error() + ": the daemon for web on hub h predates v0.1.15"), true},
		{errors.New(NotUpstreamPrefix + errHubNoEvents.Error() + ": box on hub h carries no browser events there"), true},
		{errors.New(NotUpstreamPrefix + errUpstreamReconnecting.Error()), false},
		{errors.New("h/web: hub: web is not running"), false},
	} {
		if got := BridgeRefusedForGood(tc.err); got != tc.want {
			t.Errorf("BridgeRefusedForGood(%v) = %v", tc.err, got)
		}
	}
}

// scriptedLaptop is a laptop daemon for h/web on a hubTransport over a
// scripted hub (bridge echoed), with a control socket; no loop, no dial.
func scriptedLaptop(t *testing.T, reply func(Request) Response) (*laptopRig, *hubTransport, config.Paths, func()) {
	t.Helper()
	conn, stop := scriptedHubOpen(t,
		func(Request) Response {
			return Response{OK: true, State: StateUp, Version: "test", Relay: true, Bridge: true, Shim: testShim}
		}, reply)
	ht, err := openHubTransport("h/web", conn, newFakeLocal(), stop, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := &laptopRig{d: newDaemon(shortTempPaths(t), "h/web", "test", ht, func() (transport, error) {
		return nil, errors.New("no dial in this test")
	})}
	l.d.logf = t.Logf
	t.Cleanup(func() { releaseDaemon(l.d); ht.Close() }) // ht too: a test may drop it from the daemon
	return l, ht, serveLaptop(t, l), stop
}

// dropTransport puts the laptop daemon between transports, as onDead does.
func dropTransport(d *daemon) {
	d.mu.Lock()
	d.setStateLocked(StateReconnecting)
	d.tr = nil
	d.mu.Unlock()
}

// A hub machine's daemon between transports refuses a local subscribe
// rather than ack one nothing upstream will honour (the one-exec hole a
// review found: `auth HUB/NAME` trusted that ack and proxied to a hub too
// old to honour DEVVM_NO_SUBSCRIBE, whose auth spawned a second agent
// exec). The registration stays for restore(); ping says reconnecting,
// with no shim. A daemon for a machine of this host acks as before: its
// events do not depend on anything upstream.
func TestSubscribeRefusedWhileHubLinkReconnecting(t *testing.T) {
	var mu sync.Mutex
	var ops []string
	l, _, dir, _ := scriptedLaptop(t, func(req Request) Response {
		mu.Lock()
		ops = append(ops, req.Op)
		mu.Unlock()
		return Response{OK: true}
	})
	dropTransport(l.d)
	s, err := openSession("h/web", func(<-chan struct{}) (net.Conn, error) {
		return net.Dial("unix", socketPath(dir, "h/web"))
	}, SessionOptions{}, 5*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Subscribe()
	if err == nil || err.Error() != NotUpstreamPrefix+errUpstreamReconnecting.Error() {
		t.Fatalf("subscribe while the hub link is down = %v, want the reconnecting refusal", err)
	}
	if subCount(l.d) != 1 {
		t.Fatal("the refused subscriber is not registered for restore()")
	}
	if info, err := s.Ping(); err != nil || info.State != StateReconnecting || info.Shim != "" {
		t.Fatalf("ping = %+v, %v; want reconnecting, no shim", info, err)
	}
	mu.Lock()
	if len(ops) != 0 {
		t.Fatalf("sent upstream with no transport: %v", ops)
	}
	mu.Unlock()

	local := startSockDaemon(t, shortTempPaths(t))
	dropTransport(local)
	subscribed(t, local.paths, (&catcher{}).handle) // fails the test on a refusal
}

// A relay subscriber gets relaySubscriberWait, not subscriberWait: its far
// daemon binds and then waits on its own subscriber first. The budget stays
// under the guest agent's own wait, so the guest hears this daemon's
// answer. A local subscriber keeps the short wait.
func TestRelaySubscriberWait(t *testing.T) {
	if relaySubscriberWait != subscriberWait+hubCallTimeout+15*time.Second {
		t.Fatalf("relaySubscriberWait = %s, want subscriberWait + hubCallTimeout + 15s", relaySubscriberWait)
	}
	if relaySubscriberWait >= agentrpc.AgentReplyWait {
		t.Fatalf("relaySubscriberWait %s must stay below the agent's %s", relaySubscriberWait, agentrpc.AgentReplyWait)
	}
	h := newHubRig(t)
	h.d.replyWait, h.d.relayReplyWait = 20*time.Millisecond, 5*time.Second
	l := newLaptopRig(t, h, true)
	dir := serveLaptop(t, l)
	slow := func(tag string) func(json.RawMessage) json.RawMessage {
		return func(json.RawMessage) json.RawMessage {
			time.Sleep(200 * time.Millisecond) // past replyWait, well inside relayReplyWait
			b, _ := json.Marshal(agentrpc.OpenReply{Opened: true, URL: tag})
			return b
		}
	}
	sessionOn(t, dir, "h/web", slow("laptop"))
	if rep := h.d.bridgeOpen("https://example.com/1"); !rep.Opened || rep.URL != "laptop" {
		t.Fatalf("relay open = %+v, want the slow laptop's answer", rep)
	}
	sessionOn(t, h.dir, "web", slow("desktop"))
	if rep := h.d.bridgeOpen("https://example.com/2"); rep.Err != reasonNoReply {
		t.Fatalf("local open = %+v, want %q past the local wait", rep, reasonNoReply)
	}
}

// onEvent answers what it cannot hand to the loop itself: an event it does
// not know (malformed, another type, no URL), and one arriving after the
// transport died, before the loop took it (pending: the reconnect is
// coming).
func TestHubTransportOnEventEdges(t *testing.T) {
	conn, stop := scriptedHubOpen(t,
		func(Request) Response { return Response{OK: true, State: StateUp, Relay: true, Bridge: true} },
		func(Request) Response { return Response{OK: true} })
	ht, err := openHubTransport("h/web", conn, newFakeLocal(), stop, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ht.Close()
	answer := func(raw string) agentrpc.OpenReply {
		var r agentrpc.OpenReply
		if err := json.Unmarshal(ht.onEvent(json.RawMessage(raw)), &r); err != nil {
			t.Fatalf("onEvent(%s) answered unreadably: %v", raw, err)
		}
		return r
	}
	for _, raw := range []string{`{not json`, `{"type":"something-else","url":"https://x.example"}`, `{"type":"` + agentrpc.EventOpenURL + `"}`} {
		if r := answer(raw); r.Err != "this devvm does not know that event" || r.Opened {
			t.Errorf("onEvent(%s) = %+v", raw, r)
		}
	}
	select {
	case req := <-ht.events():
		t.Fatalf("an unknown event reached the loop: %+v", req)
	default:
	}

	stop() // the relay ends: the transport dies with nobody reading events()
	select {
	case <-ht.dead():
	case <-time.After(5 * time.Second):
		t.Fatal("the transport did not die")
	}
	r := answer(`{"type":"` + agentrpc.EventOpenURL + `","url":"http://localhost:5173/","guest":5173}`)
	if r.Err != reasonPending || r.Guest != 5173 {
		t.Fatalf("event after death = %+v, want %q for guest 5173", r, reasonPending)
	}
}

// After the relay dies and the laptop daemon reconnects on a fresh one,
// restore() subscribes it upstream again before the daemon reports up, so
// the hub's next open reaches the laptop's subscriber with no local
// re-subscribe.
func TestHubResubscribesAfterRelayReconnect(t *testing.T) {
	h := newHubRig(t)
	h.tr.shim = testShim
	l := newLaptopRig(t, h, true)
	dir := serveLaptop(t, l)
	var c catcher
	sessionOn(t, dir, "h/web", tagged("A", &c))
	if rep := h.d.bridgeOpen("https://example.com/1"); rep.URL != "A" {
		t.Fatalf("before the reconnect the open went to %q", rep.URL)
	}
	first := l.hubTransport()
	l.lastRelay().kill()
	<-first.dead()
	waitFor(t, "the laptop to be back up on a fresh relay", func() bool {
		st, _ := l.d.status()
		return st == StateUp && l.dials.Load() >= 1 && l.hubTransport() != first
	})
	if rep := h.d.bridgeOpen("https://example.com/2"); rep.URL != "A" {
		t.Fatalf("after the reconnect the open = %+v, want the laptop's subscriber", rep)
	}
	if n := len(c.events()); n != 2 {
		t.Fatalf("the laptop subscriber saw %d opens, want 2", n)
	}
}

// An upstream subscribe that outlives upTimeout fails the local subscribe
// (the registration stays); when the hub's answer finally comes, the
// late re-sync subscribes again, since a subscriber is still there, and
// the daemon ends up subscribed upstream.
func TestUpstreamSubscribeTimeoutThenLateResync(t *testing.T) {
	seen := make(chan string, 16)
	release := make(chan struct{})
	var once sync.Once
	l, _, dir, _ := scriptedLaptop(t, func(req Request) Response {
		if req.Op == OpSubscribe || req.Op == OpUnsubscribe {
			seen <- req.Op
			once.Do(func() { <-release }) // the first answer is late
		}
		return Response{OK: true}
	})
	l.d.upTimeout = 50 * time.Millisecond
	outcomes := make(chan string, 16)
	l.d.onUpstream = func(on bool, err error) { outcomes <- fmt.Sprintf("%v=%v", on, err) }
	s, err := openSession("h/web", func(<-chan struct{}) (net.Conn, error) {
		return net.Dial("unix", socketPath(dir, "h/web"))
	}, SessionOptions{}, 5*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Subscribe()
	if err == nil || !strings.Contains(err.Error(), "did not answer within") {
		t.Fatalf("subscribe past upTimeout = %v", err)
	}
	if op := <-seen; op != OpSubscribe {
		t.Fatalf("first upstream op = %s", op)
	}
	if subCount(l.d) != 1 {
		t.Fatal("the subscriber was dropped on an upstream timeout")
	}
	close(release)
	if op := <-seen; op != OpSubscribe {
		t.Fatalf("the late re-sync sent %s, want subscribe (a subscriber remains)", op)
	}
	if got := <-outcomes; got != "true=<nil>" {
		t.Fatalf("re-sync outcome = %s", got)
	}
	l.d.upMu.Lock()
	on, dirty := l.d.upOn, l.d.upDirty
	l.d.upMu.Unlock()
	if !on || dirty {
		t.Fatalf("after the re-sync upOn=%v upDirty=%v, want subscribed and known", on, dirty)
	}
}

// A session whose re-subscribe after a reconnect is refused keeps the new
// connection (N5): the daemon registered it locally and re-sends upstream
// itself, so tearing the connection down to retry would only churn.
func TestSessionKeepsConnectionWhenResubscribeRefused(t *testing.T) {
	l, _, dir, _ := scriptedLaptop(t, func(Request) Response { return Response{OK: true} })
	var mu sync.Mutex
	var logs []string
	reconnects := make(chan struct{}, 4)
	s, err := openSession("h/web", func(<-chan struct{}) (net.Conn, error) {
		return net.Dial("unix", socketPath(dir, "h/web"))
	}, SessionOptions{
		Logf: func(f string, a ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(f, a...))
			mu.Unlock()
		},
		OnReconnect: func() { reconnects <- struct{}{} },
	}, 5*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Subscribe(); err != nil {
		t.Fatal(err)
	}
	dropTransport(l.d)
	l.d.closeAllSessions() // the session reconnects to a daemon that refuses its re-subscribe
	select {
	case <-reconnects:
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not reconnect")
	}
	if info, err := s.Ping(); err != nil || info.State != StateReconnecting {
		t.Fatalf("ping on the new connection = %+v, %v", info, err)
	}
	mu.Lock()
	joined := strings.Join(logs, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "re-subscribe after reconnect: "+NotUpstreamPrefix+errUpstreamReconnecting.Error()) {
		t.Fatalf("logs = %q, want the refused re-subscribe logged", joined)
	}
	if strings.Contains(joined, "reconnect attempt") {
		t.Fatalf("the refused re-subscribe failed the connection: %q", joined)
	}
	waitFor(t, "the laptop to hold the re-subscribed session", func() bool { return subCount(l.d) == 1 })
}
