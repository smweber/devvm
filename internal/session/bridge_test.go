package session

import (
	"github.com/smweber/devvm/internal/config"

	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/smweber/devvm/internal/agentrpc"
	"github.com/smweber/devvm/internal/hostbrowser"
)

// The browser bridge (roadmap step 7): classification, the bind policy per
// kind, delivery to the most recent subscriber, and the reply the guest
// reads, from the daemon's handler down to the agent's event stream.

func TestClassify(t *testing.T) {
	enc := url.QueryEscape
	for _, tc := range []struct {
		url  string
		kind string
		port int
		v6   bool
	}{
		{"http://localhost:3000/", KindDirect, 3000, false},
		{"http://127.0.0.1:8080/x?y=1", KindDirect, 8080, false},
		{"http://[::1]:5000/", KindDirect, 5000, true},
		{"http://LOCALHOST:3001", KindDirect, 3001, false},
		{"http://localhost/", KindDirect, 80, false}, // refused later: privileged
		{"https://auth.example/authorize?redirect_uri=" + enc("http://localhost:1455/cb"), KindRedirect, 1455, false},
		{"https://auth.example/authorize?redirect_uri=" + enc("http://127.0.0.1:54231/callback") + "&state=x", KindRedirect, 54231, false},
		{"https://auth.example/authorize?redirect_uri=" + enc("http://[::1]:7777/cb"), KindRedirect, 7777, true},
		// A redirect_uri on a non-loopback host is nothing to forward.
		{"https://auth.example/authorize?redirect_uri=" + enc("https://app.example/cb"), KindExternal, 0, false},
		{"https://auth.example/authorize?redirect_uri=" + enc("http://10.0.0.5:8000/cb"), KindExternal, 0, false},
		// Only the parameter itself counts, never a loopback elsewhere.
		{"https://auth.example/a?redirect_uri=" + enc("https://evil.example/cb") + "&state=localhost:6666", KindExternal, 0, false},
		{"https://example.com/", KindExternal, 0, false},
		{"file:///etc/passwd", KindExternal, 0, false},
		// The URL's own host decides first.
		{"https://localhost:9999/auth?redirect_uri=" + enc("https://evil.example/cb"), KindDirect, 9999, false},
	} {
		got := classify(tc.url)
		if got.kind != tc.kind || got.port != tc.port || got.v6 != tc.v6 {
			t.Errorf("classify(%q) = %+v, want %s %d v6=%v", tc.url, got, tc.kind, tc.port, tc.v6)
		}
	}
}

// Moved from internal/auth, where it skipped codex's 1455: the daemon
// bridges that callback like any other now.
func TestCallbackPort(t *testing.T) {
	tests := []struct {
		name string
		url  string
		port int
		ok   bool
	}{
		{"claude random port", "https://claude.ai/oauth/authorize?redirect_uri=http%3A%2F%2Flocalhost%3A54231%2Fcallback&state=x", 54231, true},
		{"127.0.0.1 literal", "https://x/auth?redirect_uri=http://127.0.0.1:8976/cb", 8976, true},
		{"codex 1455 no longer skipped", "https://x/auth?redirect_uri=http%3A%2F%2Flocalhost%3A1455%2F", 1455, true},
		{"no redirect_uri", "https://x/auth?client_id=abc", 0, false},
		{"no loopback", "https://x/auth?redirect_uri=https%3A%2F%2Fexample.com%2Fcb", 0, false},
		{"loopback outside redirect_uri", "https://x/auth?redirect_uri=https%3A%2F%2Fevil.example%2Fcb&state=localhost:6666", 0, false},
		{"loopback host page with remote redirect", "https://localhost:9999/auth?redirect_uri=https%3A%2F%2Fevil.example%2Fcb", 0, false},
		{"ipv6 loopback", "https://x/auth?redirect_uri=http%3A%2F%2F%5B%3A%3A1%5D%3A7777%2Fcb", 7777, true},
		{"no port", "https://x/auth?redirect_uri=http%3A%2F%2Flocalhost%2Fcb", 0, false},
	}
	for _, tt := range tests {
		port, ok := CallbackPort(tt.url)
		if ok != tt.ok || port != tt.port {
			t.Errorf("%s: CallbackPort = %d,%v want %d,%v", tt.name, port, ok, tt.port, tt.ok)
		}
	}
}

// redirectURL is an authorize URL whose redirect_uri is loopback:port.
func redirectURL(host string, port int) string {
	return "https://auth.example/authorize?redirect_uri=" +
		url.QueryEscape(fmt.Sprintf("http://%s/cb", net.JoinHostPort(host, fmt.Sprint(port))))
}

// catcher is a subscriber's OnEvent that records every event and answers
// with reply (opened by default).
type catcher struct {
	mu    sync.Mutex
	evs   []BridgeEvent
	reply agentrpc.OpenReply
	block chan struct{} // non-nil: wait on it before answering
}

func (c *catcher) handle(data json.RawMessage) json.RawMessage {
	var ev BridgeEvent
	json.Unmarshal(data, &ev)
	c.mu.Lock()
	c.evs = append(c.evs, ev)
	rep := c.reply
	c.mu.Unlock()
	if c.block != nil {
		<-c.block
	}
	b, _ := json.Marshal(rep)
	return b
}

func (c *catcher) events() []BridgeEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]BridgeEvent(nil), c.evs...)
}

// subscribed opens a subscribed session on dir's daemon answering with h.
func subscribed(t *testing.T, dir config.Paths, h func(json.RawMessage) json.RawMessage) *Session {
	t.Helper()
	s := testSession(t, dir, SessionOptions{OnEvent: h})
	if err := s.Subscribe(); err != nil {
		t.Fatal(err)
	}
	return s
}

// A direct open binds the guest port connection-owned, bumping when the
// host port is taken (here the "guest" listener itself holds it: guest and
// host share a loopback in tests), and the subscriber opens the URL
// rewritten for the port it reaches; the forward goes with the session.
func TestBridgeDirectConnectionOwnedWithBump(t *testing.T) {
	_, guest := echoServer(t)
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	var opened []string
	var mu sync.Mutex
	s := subscribed(t, dir, OpenOnHost(io.Discard, func(u string) error {
		mu.Lock()
		opened = append(opened, u)
		mu.Unlock()
		return nil
	}))
	raw := fmt.Sprintf("http://localhost:%d/app?x=1", guest)
	rep := d.bridgeOpen(raw)
	if !rep.Opened || rep.Guest != guest || rep.HostPort == guest || rep.HostPort == 0 {
		t.Fatalf("reply = %+v, want opened on a bumped host port", rep)
	}
	want := fmt.Sprintf("http://localhost:%d/app?x=1", rep.HostPort)
	mu.Lock()
	got := append([]string(nil), opened...)
	mu.Unlock()
	if len(got) != 1 || got[0] != want || rep.URL != want {
		t.Fatalf("opened %v (reply url %q), want %q", got, rep.URL, want)
	}
	if msg := rep.Message(raw); !strings.Contains(msg, "devvm: opened on host -> "+want) || !strings.Contains(msg, "host port bumped") {
		t.Fatalf("guest line = %q", msg)
	}
	fs := d.list()
	if len(fs) != 1 || fmt.Sprint(fs[0].Owners) != "[connection]" || fs[0].Exact || fs[0].Host != rep.HostPort {
		t.Fatalf("forwards = %+v", fs)
	}
	echoOK(t, rep.HostPort)
	s.Close()
	waitFor(t, "the direct forward to go with its session", func() bool { return len(d.list()) == 0 })
}

// A redirect binds its exact port, ttl-owned with a 10-minute expiry, and
// the URL goes to the subscriber verbatim; with the port held on the host
// the open is refused with a reply the guest can read, and the subscriber
// never sees it.
func TestBridgeRedirectExactTTL(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	now := time.Now()
	d.now = func() time.Time { return now }
	c := &catcher{reply: agentrpc.OpenReply{Opened: true}}
	subscribed(t, dir, c.handle)
	port := farPort(t)
	raw := redirectURL("localhost", port)
	if rep := d.bridgeOpen(raw); !rep.Opened {
		t.Fatalf("reply = %+v", rep)
	}
	evs := c.events()
	if len(evs) != 1 || evs[0].Kind != KindRedirect || evs[0].Guest != port || evs[0].Bound != port || evs[0].URL != raw {
		t.Fatalf("event = %+v", evs)
	}
	fs := d.list()
	if len(fs) != 1 || fmt.Sprint(fs[0].Owners) != "[ttl]" || !fs[0].Exact || fs[0].Host != port ||
		fs[0].Expires == nil || !fs[0].Expires.Equal(now.Add(callbackTTL)) {
		t.Fatalf("forwards = %+v", fs)
	}

	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	busy := held.Addr().(*net.TCPAddr).Port
	rep := d.bridgeOpen(redirectURL("127.0.0.1", busy))
	if rep.Opened || rep.Err != fmt.Sprintf("callback port %d is in use on the host", busy) {
		t.Fatalf("redirect to a held port = %+v", rep)
	}
	if len(c.events()) != 1 {
		t.Fatal("a refused open reached the subscriber")
	}
	if len(d.list()) != 1 {
		t.Fatalf("a refused callback was remembered: %+v", d.list())
	}
}

// A relay subscriber gets the event unbound: the far daemon binds on its
// own transport. Nothing is forwarded here, and its reply is relayed as is.
func TestBridgeRelaySubscriberUnbound(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	l := dialLine(t, dir, "t")
	l.open(Request{Relay: true})
	l.send(Request{ID: 2, Op: OpSubscribe})
	if r, err := l.recv(2 * time.Second); err != nil || !r.OK {
		t.Fatalf("subscribe = %+v, %v", r, err)
	}
	port := farPort(t)
	got := make(chan agentrpc.OpenReply, 1)
	go func() { got <- d.bridgeOpen(fmt.Sprintf("http://localhost:%d/", port)) }()
	ev, err := l.recv(2 * time.Second)
	if err != nil || ev.Event == nil {
		t.Fatalf("event = %+v, %v", ev, err)
	}
	var be BridgeEvent
	json.Unmarshal(ev.Event.Data, &be)
	if be.Kind != KindDirect || be.Guest != port || be.Bound != 0 {
		t.Fatalf("relay event = %+v, want direct, guest %d, unbound", be, port)
	}
	if fs := d.list(); len(fs) != 0 {
		t.Fatalf("bound for a relay subscriber: %+v", fs)
	}
	b, _ := json.Marshal(agentrpc.OpenReply{Opened: true, HostPort: 4321, Guest: port})
	l.send(Request{Reply: &EventReply{ID: ev.Event.ID, Data: b}})
	if rep := <-got; !rep.Opened || rep.HostPort != 4321 {
		t.Fatalf("relayed reply = %+v", rep)
	}
}

// With no subscriber the URL is logged and the guest told it was not
// opened.
func TestBridgeNoSubscriber(t *testing.T) {
	d := newTestDaemon(t)
	var logged bytes.Buffer
	var mu sync.Mutex
	d.logf = func(f string, a ...any) { mu.Lock(); fmt.Fprintf(&logged, f+"\n", a...); mu.Unlock() }
	raw := "https://example.com/login\x1b[2J"
	rep := d.bridgeOpen(raw)
	if rep.Opened || rep.Err != reasonNoSession {
		t.Fatalf("reply = %+v", rep)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logged.String(), "https://example.com/login?[2J") {
		t.Fatalf("log = %q, want the URL with its escape defused", logged.String())
	}
	if !strings.HasPrefix(rep.Message(raw), "devvm: not opened on host (no devvm session") {
		t.Fatalf("guest line = %q", rep.Message(raw))
	}
}

// A loopback open while the daemon is reconnecting is refused with
// "forward pending"; an external one still goes through.
func TestBridgeReconnectingPending(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	c := &catcher{reply: agentrpc.OpenReply{Opened: true}}
	subscribed(t, dir, c.handle)
	d.mu.Lock()
	d.setStateLocked(StateReconnecting)
	d.mu.Unlock()
	rep := d.bridgeOpen(redirectURL("localhost", farPort(t)))
	if rep.Opened || rep.Err != reasonPending {
		t.Fatalf("redirect while reconnecting = %+v", rep)
	}
	if len(d.list()) != 0 || len(c.events()) != 0 {
		t.Fatal("bound or delivered while reconnecting")
	}
	if rep := d.bridgeOpen("https://example.com/"); !rep.Opened {
		t.Fatalf("external while reconnecting = %+v", rep)
	}
}

// A subscriber that does not answer in time, or goes away, is opened:false.
func TestBridgeSubscriberTimeoutAndDisconnect(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	d.replyWait = 50 * time.Millisecond
	c := &catcher{block: make(chan struct{})}
	defer close(c.block)
	subscribed(t, dir, c.handle)
	if rep := d.bridgeOpen("https://example.com/"); rep.Opened || rep.Err != reasonNoReply {
		t.Fatalf("slow subscriber = %+v", rep)
	}

	dir2 := shortTempPaths(t)
	d2 := startSockDaemon(t, dir2)
	l := dialLine(t, dir2, "t")
	l.open(Request{})
	l.send(Request{ID: 2, Op: OpSubscribe})
	l.recv(2 * time.Second)
	got := make(chan agentrpc.OpenReply, 1)
	go func() { got <- d2.bridgeOpen("https://example.com/") }()
	if ev, err := l.recv(2 * time.Second); err != nil || ev.Event == nil {
		t.Fatalf("event = %+v, %v", ev, err)
	}
	l.c.Close() // the subscriber goes away with the event open
	select {
	case rep := <-got:
		if rep.Opened || rep.Err != reasonGone {
			t.Fatalf("vanished subscriber = %+v", rep)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the open waited on a subscriber that is gone")
	}
}

// The subscriber's side: an opener that is missing or fails to start is
// opened:false with the reason, and the URL is printed on this host's
// terminal (control characters defused) for pasting.
func TestOpenOnHostFailures(t *testing.T) {
	ev := func(raw string) json.RawMessage {
		b, _ := json.Marshal(BridgeEvent{Type: agentrpc.EventOpenURL, URL: raw, Kind: KindExternal})
		return b
	}
	for _, tc := range []struct {
		err     error
		why     string
		printed bool
	}{
		{hostbrowser.ErrNoOpener, "no browser opener", true},
		{errors.New("browser opener xdg-open failed to start: exec format error"), "browser opener xdg-open failed to start: exec format error", true},
		{hostbrowser.ErrRefused, hostbrowser.ErrRefused.Error(), false},
	} {
		var term bytes.Buffer
		raw := "https://example.com/\x1b]0;x\x07"
		var rep agentrpc.OpenReply
		json.Unmarshal(OpenOnHost(&term, func(string) error { return tc.err })(ev(raw)), &rep)
		if rep.Opened || rep.Err != tc.why || rep.Printed != tc.printed {
			t.Errorf("%v: reply = %+v", tc.err, rep)
		}
		if tc.printed != strings.Contains(term.String(), "https://example.com/?]0;x?") || strings.ContainsAny(term.String(), "\x1b\x07") {
			t.Errorf("%v: terminal got %q", tc.err, term.String())
		}
	}
	var rep agentrpc.OpenReply
	json.Unmarshal(OpenOnHost(io.Discard, func(string) error { return hostbrowser.ErrNoOpener })(ev("https://x.example/")), &rep)
	if got := rep.Message("https://x.example/"); got != "devvm: not opened on host (no browser opener); URL printed there" {
		t.Errorf("guest line = %q", got)
	}
}

// v4OnlyTransport is a host whose forwards never get their ::1 side.
type v4OnlyTransport struct{ *fakeTransport }

type v4Closer struct{ io.Closer }

func (v4Closer) boundV6() bool { return false }

func (v *v4OnlyTransport) forward(h, g int) (io.Closer, error) {
	c, err := v.fakeTransport.forward(h, g)
	if err != nil {
		return nil, err
	}
	return v4Closer{c}, nil
}

// A literal [::1] URL names the family, so a forward without its ::1 side
// is refused rather than reported; localhost and 127.0.0.1 never need it.
func TestBridgeLiteralV6RefusedWithoutV6(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	d.mu.Lock()
	d.tr = &v4OnlyTransport{newFakeTransport()}
	d.mu.Unlock()
	c := &catcher{reply: agentrpc.OpenReply{Opened: true}}
	subscribed(t, dir, c.handle)
	port := farPort(t)
	rep := d.bridgeOpen(redirectURL("::1", port))
	if want := fmt.Sprintf("callback address [::1]:%d is unavailable on the host", port); rep.Opened || rep.Err != want {
		t.Fatalf("[::1] redirect on a v4-only host = %+v, want %q", rep, want)
	}
	if len(d.list()) != 0 || len(c.events()) != 0 {
		t.Fatalf("refused [::1] open left %+v, delivered %d", d.list(), len(c.events()))
	}
	if rep := d.bridgeOpen(redirectURL("localhost", port)); !rep.Opened {
		t.Fatalf("localhost redirect on the same host = %+v", rep)
	}
}

// The limits: at most 10 opens a minute, at most 20 bridge forwards, never
// a port below 1024; each refused with a reply.
func TestBridgeLimits(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	var clock atomic.Int64
	start := time.Now()
	d.now = func() time.Time { return start.Add(time.Duration(clock.Load())) }
	c := &catcher{reply: agentrpc.OpenReply{Opened: true}}
	subscribed(t, dir, c.handle)

	refused := 0
	for i := 0; i < 15; i++ {
		clock.Store(int64(i) * int64(time.Second))
		if rep := d.bridgeOpen("https://example.com/"); rep.Err == reasonRateLimit {
			refused++
		} else if !rep.Opened {
			t.Fatalf("open %d = %+v", i, rep)
		}
	}
	if refused != 5 || len(c.events()) != 10 {
		t.Fatalf("15 opens in a minute: %d refused, %d delivered; want the last 5 refused", refused, len(c.events()))
	}
	clock.Store(int64(2 * time.Minute)) // the window has passed
	if rep := d.bridgeOpen("https://example.com/"); !rep.Opened {
		t.Fatalf("open after the window = %+v", rep)
	}

	for _, raw := range []string{"http://localhost:80/", "http://localhost/", redirectURL("localhost", 443)} {
		clock.Add(int64(time.Minute))
		if rep := d.bridgeOpen(raw); rep.Opened || !strings.Contains(rep.Err, "privileged port") {
			t.Errorf("%s = %+v", raw, rep)
		}
	}

	d.mu.Lock()
	for i := 0; i < maxEphemeral; i++ {
		d.forwards[40000+i] = newFwd(40000+i, 40000+i, true, ttlOwner, ttlFor(start, time.Hour))
	}
	d.forwards[41000] = newFwd(41000, 41000, false, confOwner, ownerInfo{}) // conf: not ephemeral
	d.mu.Unlock()
	clock.Add(int64(time.Minute))
	if rep := d.bridgeOpen(redirectURL("localhost", farPort(t))); rep.Err != reasonEphemerals {
		t.Fatalf("21st bridge forward = %+v", rep)
	}
	// A repeat for a port already forwarded reuses it: no new forward.
	clock.Add(int64(time.Minute))
	if rep := d.bridgeOpen(redirectURL("localhost", 40003)); rep.Err == reasonEphemerals {
		t.Fatalf("a repeat callback was refused by the cap: %+v", rep)
	}
}

// guestPipe is the agent's yamux session over a real socket pair (yamux
// deadlocks over net.Pipe), with the host side's streams served by
// serveEvents into out.
func guestPipe(t *testing.T, out chan bridgeRequest) (guest *yamux.Session) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	gc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	hc := <-acc
	host, err := yamux.Client(agentrpc.Stdio{In: hc, Out: hc}, agentrpc.MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	guest, err = yamux.Server(agentrpc.Stdio{In: gc, Out: gc}, agentrpc.MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	go serveEvents(host, out)
	t.Cleanup(func() { hc.Close(); gc.Close(); host.Close(); guest.Close() })
	return guest
}

// sendEvent writes an open-url event on a fresh stream, as the agent does.
func sendEvent(t *testing.T, g *yamux.Session, raw string) net.Conn {
	t.Helper()
	s, err := g.Open()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(agentrpc.Event{Type: agentrpc.EventOpenURL, URL: raw})
	if _, err := s.Write([]byte(agentrpc.TypeEvent + "\n" + string(b) + "\n")); err != nil {
		t.Fatal(err)
	}
	return s
}

// An old agent closes its event stream right after writing: the daemon
// still opens the URL, its reply write does not hang, and the next event,
// from a new agent, gets its reply over the same session.
func TestBridgeOldAgentDoesNotHangDaemon(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	tr := d.tr.(*fakeTransport)
	tr.ev = make(chan bridgeRequest)
	c := &catcher{reply: agentrpc.OpenReply{Opened: true}}
	subscribed(t, dir, c.handle)
	g := guestPipe(t, tr.ev)
	done := runLoop(d)
	defer func() { d.triggerStop(); waitDone(t, done, "stop") }()

	old := sendEvent(t, g, "https://example.com/old")
	old.Close()
	waitFor(t, "the old agent's URL to reach the subscriber", func() bool { return len(c.events()) == 1 })

	s := sendEvent(t, g, "https://example.com/new")
	defer s.Close()
	s.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := io.ReadAll(s) // the reply, then the host's close
	if err != nil {
		t.Fatalf("new agent's reply: %v", err)
	}
	var rep agentrpc.OpenReply
	if err := json.Unmarshal(line, &rep); err != nil || !rep.Opened {
		t.Fatalf("new agent's reply = %q, %v", line, err)
	}
	if r := d.dispatch(Request{Op: OpPing}); !r.OK {
		t.Fatal("daemon unresponsive after an old agent's event")
	}
}

// handleOpen never runs on loop()'s goroutine: with a subscriber sitting on
// an event, the loop still sees the transport die and reconnects.
func TestBridgeOpenDoesNotBlockLoop(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	d.replyWait = time.Minute
	first := d.tr.(*fakeTransport)
	first.ev = make(chan bridgeRequest)
	d.dial = func() (transport, error) { return newFakeTransport(), nil }
	c := &catcher{block: make(chan struct{})}
	subscribed(t, dir, c.handle)
	states := recordStates(d)
	done := runLoop(d)
	replied := make(chan agentrpc.OpenReply, 1)
	first.ev <- bridgeRequest{url: "https://example.com/", reply: func(r agentrpc.OpenReply) { replied <- r }}
	waitFor(t, "the subscriber holding the event", func() bool { return len(c.events()) == 1 })
	first.die()
	states.wait(t, StateReconnecting, StateUp)
	close(c.block)
	select {
	case <-replied:
	case <-time.After(5 * time.Second):
		t.Fatal("the held event never got its reply")
	}
	d.triggerStop()
	waitDone(t, done, "stop")
}

// The ticker expires a callback: wall-clock, within one tick of its ttl.
func TestBridgeTTLExpiresOnTicker(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	d.retryEvery = 10 * time.Millisecond
	var clock atomic.Int64
	start := time.Now()
	d.now = func() time.Time { return start.Add(time.Duration(clock.Load())) }
	subscribed(t, dir, (&catcher{reply: agentrpc.OpenReply{Opened: true}}).handle)
	port := farPort(t)
	if rep := d.bridgeOpen(redirectURL("localhost", port)); !rep.Opened {
		t.Fatalf("reply = %+v", rep)
	}
	done := runLoop(d)
	defer func() { d.triggerStop(); waitDone(t, done, "stop") }()
	if !portOpen(port) {
		t.Fatal("callback not bound")
	}
	clock.Store(int64(callbackTTL))
	waitFor(t, "the callback to expire", func() bool { return len(d.list()) == 0 && !portOpen(port) })
}

// End to end through the real session client (the one attach and shell
// use): an event from the transport reaches a subscribed Session, its
// OnEvent opens the URL, and the reply reaches the guest; the daemon is
// then cycled (update, a killed daemon) and the client reconnects and
// re-subscribes on its own, so the next open still lands, with no
// re-attach.
func TestBridgeThroughSessionClientAcrossDaemonCycle(t *testing.T) {
	dir := shortTempPaths(t)
	d1 := startSockDaemon(t, dir)
	tr1 := d1.tr.(*fakeTransport)
	tr1.ev = make(chan bridgeRequest)
	done1 := runLoop(d1)
	var opened atomic.Int32
	reconnected := make(chan struct{}, 1)
	s := testSession(t, dir, SessionOptions{
		OnEvent:     OpenOnHost(io.Discard, func(string) error { opened.Add(1); return nil }),
		OnReconnect: func() { reconnected <- struct{}{} },
	})
	if err := s.Subscribe(); err != nil {
		t.Fatal(err)
	}
	open := func(ev chan bridgeRequest, raw string) agentrpc.OpenReply {
		t.Helper()
		got := make(chan agentrpc.OpenReply, 1)
		ev <- bridgeRequest{url: raw, reply: func(r agentrpc.OpenReply) { got <- r }}
		select {
		case r := <-got:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("no reply")
			return agentrpc.OpenReply{}
		}
	}
	_, guest := echoServer(t)
	rep := open(tr1.ev, fmt.Sprintf("http://127.0.0.1:%d/", guest))
	if !rep.Opened || rep.HostPort == 0 || opened.Load() != 1 {
		t.Fatalf("first open = %+v (opened %d)", rep, opened.Load())
	}
	echoOK(t, rep.HostPort)

	d1.triggerStop()
	waitDone(t, done1, "stop")
	d1.shutdown()
	d2 := startSockDaemon(t, dir)
	tr2 := d2.tr.(*fakeTransport)
	tr2.ev = make(chan bridgeRequest)
	done2 := runLoop(d2)
	defer func() { d2.triggerStop(); waitDone(t, done2, "stop") }()
	select {
	case <-reconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("the session client did not reconnect")
	}
	rep = open(tr2.ev, fmt.Sprintf("http://127.0.0.1:%d/", guest))
	if !rep.Opened || opened.Load() != 2 {
		t.Fatalf("open after the daemon cycled = %+v (opened %d)", rep, opened.Load())
	}
	echoOK(t, rep.HostPort)
}

// A direct [::1] URL on a host whose forward has no ::1 side is refused
// ("address …"), and a refusal takes back only an owner this open added:
// an earlier callback's ttl owner on the same port stays.
func TestBridgeLiteralV6DirectAndEarlierOwner(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	d.mu.Lock()
	d.tr = &v4OnlyTransport{newFakeTransport()}
	d.mu.Unlock()
	subscribed(t, dir, (&catcher{reply: agentrpc.OpenReply{Opened: true}}).handle)

	q := farPort(t)
	rep := d.bridgeOpen(fmt.Sprintf("http://[::1]:%d/", q))
	if want := fmt.Sprintf("address [::1]:%d is unavailable on the host", q); rep.Opened || rep.Err != want {
		t.Fatalf("direct [::1] on a v4-only host = %+v, want %q", rep, want)
	}
	if len(d.list()) != 0 {
		t.Fatalf("refused direct open left %+v", d.list())
	}

	p := farPort(t)
	if rep := d.bridgeOpen(redirectURL("localhost", p)); !rep.Opened {
		t.Fatalf("localhost callback = %+v", rep)
	}
	if rep := d.bridgeOpen(redirectURL("::1", p)); rep.Opened {
		t.Fatalf("[::1] callback on the same port = %+v, want refused", rep)
	}
	fs := d.list()
	if len(fs) != 1 || fs[0].Guest != p || fmt.Sprint(fs[0].Owners) != "[ttl]" || !portOpen(p) {
		t.Fatalf("the earlier callback's owner was taken by a refused open: %+v", fs)
	}
}

// A relay's forwards are another host's: they never count toward this
// host's bridge cap.
func TestBridgeCapIgnoresRelayForwards(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	relay := regSession(d, 900)
	relay.relay = true
	d.mu.Lock()
	for i := 0; i < maxEphemeral+5; i++ {
		d.forwards[42000+i] = newFwd(42000+i, 42000+i, false, connOwner(900), ownerInfo{})
	}
	d.mu.Unlock()
	subscribed(t, dir, (&catcher{reply: agentrpc.OpenReply{Opened: true}}).handle)
	if rep := d.bridgeOpen(redirectURL("localhost", farPort(t))); !rep.Opened {
		t.Fatalf("callback with only relay forwards around = %+v", rep)
	}
}

// The subscriber's session closing between the handler's choice and the
// bind (the review's proven race): the open is refused as gone, and no
// forward is left owned by a connection nobody will close. The session add
// path is held to the same rule.
func TestBridgeSubscriberGoneBeforeBind(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	s := subscribed(t, dir, (&catcher{}).handle)
	d.mu.Lock()
	sub := d.subs[0]
	d.mu.Unlock()
	s.Close()
	waitFor(t, "session gone", func() bool { return d.sessionCount() == 0 })
	_, guest := echoServer(t)
	if host, why := d.bridgeBind(classify(fmt.Sprintf("http://localhost:%d/", guest)), sub); why != reasonGone || host != 0 {
		t.Fatalf("bind for a gone subscriber = %d, %q", host, why)
	}
	if len(d.list()) != 0 || !d.idleNow() {
		t.Fatalf("forward left for a dead connection: %+v", d.list())
	}
	if _, _, _, err := d.add(farPort(t), guest, false, connOwner(sub.id)); !errors.Is(err, errSubscriberGone) {
		t.Fatalf("session add for a closed session = %v", err)
	}
}

// The cap is checked where the forward is created: concurrent opens at the
// edge admit exactly one.
func TestBridgeCapConcurrent(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	sub := regSession(d, 901)
	d.mu.Lock()
	for i := 0; i < maxEphemeral-1; i++ {
		d.forwards[43000+i] = newFwd(43000+i, 43000+i, true, ttlOwner, ttlFor(time.Now(), time.Hour))
	}
	d.mu.Unlock()
	const n = 8
	ports := make([]int, n)
	for i := range ports {
		ports[i] = farPort(t)
	}
	var wg sync.WaitGroup
	var admitted, capped atomic.Int32
	start := make(chan struct{})
	for _, p := range ports {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			<-start
			switch _, why := d.bridgeBind(classify(redirectURL("localhost", p)), sub); why {
			case "":
				admitted.Add(1)
			case reasonEphemerals:
				capped.Add(1)
			default:
				t.Errorf("bind %d: %s", p, why)
			}
		}(p)
	}
	close(start)
	wg.Wait()
	if admitted.Load() != 1 || capped.Load() != n-1 {
		t.Fatalf("admitted %d, capped %d; want exactly one past %d", admitted.Load(), capped.Load(), maxEphemeral-1)
	}
	d.mu.Lock()
	held := 0
	for _, f := range d.forwards {
		if d.bridgeHeldLocked(f) {
			held++
		}
	}
	d.mu.Unlock()
	if held != maxEphemeral {
		t.Fatalf("%d bridge forwards, cap %d", held, maxEphemeral)
	}
}

// A hostile guest cannot pile up event streams: past maxEventStreams in
// flight, a new stream is closed unread; a slot frees once its open is
// answered.
func TestEventStreamsBounded(t *testing.T) {
	out := make(chan bridgeRequest)
	g := guestPipe(t, out)
	var held []net.Conn
	for i := 0; i < maxEventStreams; i++ {
		held = append(held, sendEvent(t, g, fmt.Sprintf("https://example.com/%d", i)))
	}
	// Each of those is admitted and waiting on out (nobody reads it yet).
	// Take one, so its stream is known to be past accept; then the rest.
	first := <-out
	extra := sendEvent(t, g, "https://example.com/extra")
	extra.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, err := io.ReadAll(extra); err != nil || len(b) != 0 {
		t.Fatalf("stream past the bound = %q, %v; want closed unread", b, err)
	}
	first.reply(agentrpc.OpenReply{Opened: true})
	// A slot is free again: the next stream is admitted.
	s := sendEvent(t, g, "https://example.com/after")
	var got bridgeRequest
	for got.url != "https://example.com/after" {
		select {
		case got = <-out:
			if got.url != "https://example.com/after" {
				got.reply(agentrpc.OpenReply{})
				got = bridgeRequest{}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no stream admitted after a slot freed")
		}
	}
	got.reply(agentrpc.OpenReply{Opened: true})
	s.Close()
	for _, c := range held {
		c.Close()
	}
}

// restore() drops a ttl owner that lapsed during the outage before it
// re-binds anything: its exact port is not claimed again.
func TestRestoreExpiresLapsedTTLFirst(t *testing.T) {
	d := newTestDaemon(t)
	now := time.Now()
	d.now = func() time.Time { return now }
	d.mu.Lock()
	d.forwards[44000] = newFwd(farPort(t), 44000, true, ttlOwner, ttlFor(now.Add(-time.Hour), time.Minute))
	d.setStateLocked(StateReconnecting)
	d.mu.Unlock()
	fresh := newFakeTransport()
	if !d.restore(fresh) {
		t.Fatal("restore failed")
	}
	if fresh.binds.Load() != 0 || len(d.list()) != 0 {
		t.Fatalf("a lapsed callback was re-bound: binds %d, %+v", fresh.binds.Load(), d.list())
	}
}
