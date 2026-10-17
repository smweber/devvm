package agentrpc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// clockSession is a host yamux session to a Serve guest with handlers h.
func clockSession(t *testing.T, h GuestHandlers) *yamux.Session {
	t.Helper()
	hostConn, guestConn := tcpPipe(t)
	go func() { _ = Serve(guestConn, guestConn, h, nil) }()
	sess, err := yamux.Client(Stdio{In: hostConn, Out: hostConn}, MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hostConn.Close(); guestConn.Close() })
	return sess
}

// The host's clock reaches the handler exactly (nanoseconds included), and
// the handler's reply, an error with a newline in it included, comes back
// as one line; the session carries on afterwards.
func TestClockRoundTrip(t *testing.T) {
	host := time.Unix(1760000000, 123456789)
	got := make(chan time.Time, 4)
	replies := []ClockReply{
		{Guest: host.Add(-time.Second), Drift: time.Second, Result: ClockOK},
		{Guest: host.Add(-36 * time.Hour), Drift: 36 * time.Hour, Result: ClockStepped},
		{Guest: host.Add(5 * time.Second), Drift: -5 * time.Second, Result: "error: sudo: a password\nis required"},
	}
	i := 0
	sess := clockSession(t, GuestHandlers{Clock: func(h time.Time) ClockReply {
		got <- h
		r := replies[i]
		i++
		return r
	}})
	for n, want := range replies {
		r, err := SyncClock(sess, host)
		if err != nil {
			t.Fatalf("exchange %d: %v", n, err)
		}
		if h := <-got; !h.Equal(host) {
			t.Errorf("exchange %d: guest saw %v, want %v", n, h, host)
		}
		wantRes := strings.Join(strings.Fields(want.Result), " ")
		if !r.Guest.Equal(want.Guest) || r.Drift != want.Drift || r.Result != wantRes {
			t.Errorf("exchange %d: reply %+v, want %+v (result %q)", n, r, want, wantRes)
		}
	}
	if sess.IsClosed() {
		t.Error("the session closed after the exchanges")
	}
}

// An agent older than the time stream (no Clock handler) closes it on the
// unknown header: the host reads that as unsupported, and the session
// survives for everything else.
func TestClockOldAgent(t *testing.T) {
	sess := clockSession(t, GuestHandlers{})
	if _, err := SyncClock(sess, time.Now()); !errors.Is(err, ErrClockUnsupported) {
		t.Fatalf("SyncClock = %v, want ErrClockUnsupported", err)
	}
	if sess.IsClosed() {
		t.Error("an old agent's refusal closed the session")
	}
}

func TestParseUnix(t *testing.T) {
	for in, want := range map[string]time.Time{
		"1760000000":            time.Unix(1760000000, 0),
		"1760000000.5":          time.Unix(1760000000, 500000000),
		"1760000000.000000001":  time.Unix(1760000000, 1),
		"1760000000.1234567891": time.Unix(1760000000, 123456789),
	} {
		if got, err := parseUnix(in); err != nil || !got.Equal(want) {
			t.Errorf("parseUnix(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "x", "1.x", "1.-5"} {
		if _, err := parseUnix(bad); err == nil {
			t.Errorf("parseUnix(%q) accepted", bad)
		}
	}
}
