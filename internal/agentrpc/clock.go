package agentrpc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

// The clock exchange on one "time" stream, host -> guest:
//
//	host → guest:  time SEC.NSEC\n               (the host's wall clock)
//	guest → host:  guest=SEC.NSEC drift=DUR RESULT\n, then close
//
// RESULT is "ok" (drift under the guest's threshold: left alone), "stepped"
// (the guest set its clock to the host's) or "error: WHY". drift is host
// minus guest, as the guest saw it before any step. A smol guest's clock
// does not resync after the host suspends (no service manager in the image
// to run a time daemon), so the daemon that holds the agent exec pushes its
// own clock over that exec instead of installing anything.
//
// An agent older than this closes the stream on the unknown header (Serve's
// default case) without a reply: SyncClock reports ErrClockUnsupported.

// ErrClockUnsupported is SyncClock's answer when the agent closed the stream
// without a reply: an agent older than the time stream.
var ErrClockUnsupported = errors.New("the agent does not support clock sync")

// Clock results, the reply's last field.
const (
	ClockOK      = "ok"
	ClockStepped = "stepped"
)

// ClockReply is the guest's answer to a time request.
type ClockReply struct {
	Guest  time.Time     // the guest's clock when the request arrived
	Drift  time.Duration // host - guest
	Result string        // ClockOK, ClockStepped, or "error: WHY"
}

func (r ClockReply) String() string {
	return fmt.Sprintf("guest=%s drift=%s %s", fmtUnix(r.Guest), r.Drift, r.Result)
}

// clockWait bounds a time exchange, both ways: the guest answers at once,
// or after one `sudo -n date`, so a stream that stalls longer is broken.
const clockWait = 10 * time.Second

// maxClockLine bounds the reply line (a few dozen bytes, plus an error).
const maxClockLine = 1 << 10

func fmtUnix(t time.Time) string {
	return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond())
}

func parseUnix(s string) (time.Time, error) {
	secs, frac, _ := strings.Cut(s, ".")
	sec, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad unix time %q", s)
	}
	var nsec int64
	if frac != "" {
		if len(frac) > 9 {
			frac = frac[:9]
		}
		frac += strings.Repeat("0", 9-len(frac))
		if nsec, err = strconv.ParseInt(frac, 10, 64); err != nil || nsec < 0 {
			return time.Time{}, fmt.Errorf("bad unix time %q", s)
		}
	}
	return time.Unix(sec, nsec), nil
}

// SyncClock sends now as the host's clock on a fresh time stream and returns
// the guest's reply, host side.
func SyncClock(sess *yamux.Session, now time.Time) (ClockReply, error) {
	s, err := sess.Open()
	if err != nil {
		return ClockReply{}, err
	}
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(clockWait))
	if err := WriteHeader(s, TypeTime+" "+fmtUnix(now)); err != nil {
		return ClockReply{}, err
	}
	line, err := bufio.NewReader(io.LimitReader(s, maxClockLine)).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		if err == nil || errors.Is(err, io.EOF) {
			return ClockReply{}, ErrClockUnsupported
		}
		return ClockReply{}, err
	}
	return parseClockReply(line)
}

func parseClockReply(line string) (ClockReply, error) {
	f := strings.SplitN(line, " ", 3)
	if len(f) != 3 || !strings.HasPrefix(f[0], "guest=") || !strings.HasPrefix(f[1], "drift=") {
		return ClockReply{}, fmt.Errorf("bad clock reply %q", line)
	}
	g, err := parseUnix(strings.TrimPrefix(f[0], "guest="))
	if err != nil {
		return ClockReply{}, err
	}
	d, err := time.ParseDuration(strings.TrimPrefix(f[1], "drift="))
	if err != nil {
		return ClockReply{}, fmt.Errorf("bad clock reply %q", line)
	}
	return ClockReply{Guest: g, Drift: d, Result: f[2]}, nil
}

// guestClock answers one time stream with h's verdict on the host's clock.
func guestClock(stream net.Conn, arg string, h func(host time.Time) ClockReply) {
	defer stream.Close()
	_ = stream.SetWriteDeadline(time.Now().Add(clockWait))
	host, err := parseUnix(strings.TrimSpace(arg))
	var r ClockReply
	if err != nil {
		r = ClockReply{Guest: time.Now(), Result: "error: " + err.Error()}
	} else {
		r = h(host)
	}
	// One line: an error from the setter must not split the reply.
	r.Result = strings.Join(strings.Fields(r.Result), " ")
	_ = WriteHeader(stream, r.String())
}
