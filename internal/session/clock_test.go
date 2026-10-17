package session

import (
	"fmt"
	"net"
	"strings"
	"sync"
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
	go func() { syncClock(host, 20*time.Millisecond); close(done) }()
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
	go func() { syncClock(host, time.Millisecond); close(done) }()
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
	go func() { syncClock(host, 10*time.Millisecond); close(done) }()
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
