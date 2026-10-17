package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/agentrpc"
)

// fakeClock is a clockSyncer over a fixed guest clock and a recording setter.
func fakeClock(guest time.Time, setErr error) (*clockSyncer, *[]time.Time) {
	var sets []time.Time
	return &clockSyncer{
		now: func() time.Time { return guest },
		set: func(t time.Time) error { sets = append(sets, t); return setErr },
	}, &sets
}

func TestClockThreshold(t *testing.T) {
	guest := time.Unix(1760000000, 0)
	for _, tc := range []struct {
		drift  time.Duration
		result string
	}{
		{0, agentrpc.ClockOK},
		{1999 * time.Millisecond, agentrpc.ClockOK},
		{-1999 * time.Millisecond, agentrpc.ClockOK},
		{2 * time.Second, agentrpc.ClockStepped},
		{-2 * time.Second, agentrpc.ClockStepped},
		{38 * time.Hour, agentrpc.ClockStepped}, // the ThinkPad's 1.6 days
	} {
		c, sets := fakeClock(guest, nil)
		host := guest.Add(tc.drift)
		r := c.handle(host)
		if r.Result != tc.result || r.Drift != tc.drift || !r.Guest.Equal(guest) {
			t.Errorf("drift %v: reply %+v, want %s", tc.drift, r, tc.result)
		}
		if tc.result == agentrpc.ClockOK && len(*sets) != 0 {
			t.Errorf("drift %v: stepped %v under the threshold", tc.drift, *sets)
		}
		if tc.result == agentrpc.ClockStepped && (len(*sets) != 1 || !(*sets)[0].Equal(host)) {
			t.Errorf("drift %v: sets %v, want one to %v", tc.drift, *sets, host)
		}
	}
}

func TestClockSetError(t *testing.T) {
	guest := time.Unix(1760000000, 0)
	c, sets := fakeClock(guest, errors.New("sudo: a password is required"))
	r := c.handle(guest.Add(time.Hour))
	if len(*sets) != 1 || r.Result != "error: sudo: a password is required" {
		t.Fatalf("reply %+v after %d sets", r, len(*sets))
	}
	// The reply's wire form, as the host parses it.
	if s := r.String(); s != "guest=1760000000.000000000 drift=1h0m0s error: sudo: a password is required" {
		t.Errorf("reply line %q", s)
	}
	if s := (agentrpc.ClockReply{Guest: guest, Drift: -3 * time.Second, Result: agentrpc.ClockStepped}).String(); !strings.HasSuffix(s, " drift=-3s stepped") {
		t.Errorf("reply line %q", s)
	}
}
