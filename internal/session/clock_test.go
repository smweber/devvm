package session

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/smweber/devvm/internal/agentrpc"
)

// clockPipe is a host yamux session to an agentrpc.Serve guest with
// handlers h, over a real socket pair (yamux deadlocks over net.Pipe).
// closeConns ends the session as the exec dying does.
func clockPipe(t *testing.T, h agentrpc.GuestHandlers) (host *yamux.Session, closeConns func()) {
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
	go func() { _ = agentrpc.Serve(gc, gc, h, nil) }()
	host, err = yamux.Client(agentrpc.Stdio{In: hc, Out: hc}, agentrpc.MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	closeConns = func() { once.Do(func() { hc.Close(); gc.Close(); host.Close() }) }
	t.Cleanup(closeConns)
	return host, closeConns
}

// captureTransportLog swaps transportLogf for a recorder.
func captureTransportLog(t *testing.T) func() []string {
	var mu sync.Mutex
	var lines []string
	old := transportLogf
	transportLogf = func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	t.Cleanup(func() { transportLogf = old })
	return func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), lines...) }
}

// The daemon pushes its clock right after the dial and then on every tick,
// logs a step but never an ok, and stops with the session, Close never
// waiting on it.
func TestSyncClockDialThenTicks(t *testing.T) {
	logs := captureTransportLog(t)
	reqs := make(chan time.Time, 8)
	n := 0
	var mu sync.Mutex
	host, closeConns := clockPipe(t, agentrpc.GuestHandlers{Clock: func(h time.Time) agentrpc.ClockReply {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		reqs <- h
		if first {
			return agentrpc.ClockReply{Guest: h.Add(-time.Hour), Drift: time.Hour, Result: agentrpc.ClockStepped}
		}
		return agentrpc.ClockReply{Guest: h, Result: agentrpc.ClockOK}
	}})
	done := make(chan struct{})
	go func() { syncClock(host, 20*time.Millisecond, nil); close(done) }()
	for i := 0; i < 3; i++ {
		select {
		case h := <-reqs:
			if d := time.Since(h); d < 0 || d > 5*time.Second {
				t.Errorf("request %d carried %v, not this host's clock", i, h)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no clock request %d", i)
		}
	}
	closeConns()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("syncClock outlived its session")
	}
	got := logs()
	if len(got) != 1 || !strings.Contains(got[0], "stepped") || !strings.Contains(got[0], "1h0m0s") {
		t.Errorf("log = %q, want one step line (no line for an ok)", got)
	}
}

// An agent older than the time stream closes it unanswered: one log line,
// and the loop stops instead of asking again every tick.
func TestSyncClockOldAgent(t *testing.T) {
	logs := captureTransportLog(t)
	host, _ := clockPipe(t, agentrpc.GuestHandlers{}) // no Clock: an old agent
	done := make(chan struct{})
	go func() { syncClock(host, time.Millisecond, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("syncClock kept going against an old agent")
	}
	if got := logs(); len(got) != 1 || !strings.Contains(got[0], "does not support") {
		t.Errorf("log = %q, want one unsupported line", got)
	}
	if host.IsClosed() {
		t.Error("an old agent's refusal closed the session")
	}
}

// A setter that fails is logged once per distinct error, not every tick.
func TestSyncClockErrorLoggedOnce(t *testing.T) {
	logs := captureTransportLog(t)
	reqs := make(chan struct{}, 8)
	host, closeConns := clockPipe(t, agentrpc.GuestHandlers{Clock: func(h time.Time) agentrpc.ClockReply {
		reqs <- struct{}{}
		return agentrpc.ClockReply{Guest: h.Add(-time.Hour), Drift: time.Hour, Result: "error: sudo: a password is required"}
	}})
	done := make(chan struct{})
	go func() { syncClock(host, 10*time.Millisecond, nil); close(done) }()
	for i := 0; i < 3; i++ {
		select {
		case <-reqs:
		case <-time.After(5 * time.Second):
			t.Fatalf("no clock request %d", i)
		}
	}
	closeConns()
	<-done
	if got := logs(); len(got) != 1 || !strings.Contains(got[0], "password is required") {
		t.Errorf("log = %q, want the error once", got)
	}
}

// A nudge syncs now, between ticks; nudges that land while an exchange is
// in flight are answered by it (coalesced), and two time streams never run
// at once.
func TestSyncClockNudgeCoalesced(t *testing.T) {
	captureTransportLog(t)
	reqs := make(chan struct{}, 8)
	hold := make(chan struct{})
	var n, inFlight, maxInFlight atomic.Int32
	host, closeConns := clockPipe(t, agentrpc.GuestHandlers{Clock: func(h time.Time) agentrpc.ClockReply {
		k := inFlight.Add(1)
		defer inFlight.Add(-1)
		if k > maxInFlight.Load() {
			maxInFlight.Store(k)
		}
		if n.Add(1) == 2 {
			<-hold // the second exchange stays in flight while nudges pile up
		}
		reqs <- struct{}{}
		return agentrpc.ClockReply{Guest: h, Result: agentrpc.ClockOK}
	}})
	nudge := make(chan struct{}, 1)
	send := func() {
		select {
		case nudge <- struct{}{}:
		default:
		}
	}
	wait := func(what string) {
		t.Helper()
		select {
		case <-reqs:
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s request", what)
		}
	}
	done := make(chan struct{})
	go func() { syncClock(host, time.Hour, nudge); close(done) }() // no tick in this test
	wait("dial")
	// Nudge until the second exchange reaches the guest: a nudge that lands
	// while the dial's exchange is still finishing is (rightly) answered by
	// it, so one alone could be absorbed.
	nudgeUntil := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for n.Load() < want {
			if time.Now().After(deadline) {
				t.Fatalf("no exchange %d", want)
			}
			send()
			time.Sleep(time.Millisecond)
		}
	}
	nudgeUntil(2)
	for i := 0; i < 5; i++ {
		send()
	}
	close(hold)
	wait("nudged")
	// The nudges sent during it were answered by it: no third exchange.
	select {
	case <-reqs:
		t.Fatal("nudges during an exchange started another")
	case <-time.After(300 * time.Millisecond):
	}
	nudgeUntil(3)
	wait("later nudge")
	closeConns()
	<-done
	if got := n.Load(); got != 3 {
		t.Errorf("%d exchanges, want 3 (dial, nudge, later nudge)", got)
	}
	if m := maxInFlight.Load(); m != 1 {
		t.Errorf("%d time streams at once", m)
	}
}

// nudgeTransport is a fakeTransport that counts clock nudges.
type nudgeTransport struct {
	*fakeTransport
	nudges chan struct{}
}

func (n *nudgeTransport) nudgeClock() { n.nudges <- struct{}{} }

// A kick (ports up/start) and a new subscriber (attach/shell/auth) each
// nudge the transport's clock sync.
func TestDaemonNudgesClock(t *testing.T) {
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	nt := &nudgeTransport{fakeTransport: newFakeTransport(), nudges: make(chan struct{}, 4)}
	d.mu.Lock()
	d.tr = nt
	d.mu.Unlock()
	got := func(what string) {
		t.Helper()
		select {
		case <-nt.nudges:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not nudge the clock", what)
		}
	}
	k := dialLine(t, dir, "t")
	k.send(Request{ID: 1, Op: OpKick})
	if r, err := k.recv(2 * time.Second); err != nil || !r.OK {
		t.Fatalf("kick = %+v, %v", r, err)
	}
	got("kick")
	l := dialLine(t, dir, "t")
	l.open(Request{})
	l.send(Request{ID: 2, Op: OpSubscribe})
	if r, err := l.recv(2 * time.Second); err != nil || !r.OK {
		t.Fatalf("subscribe = %+v, %v", r, err)
	}
	got("subscribe")
}
