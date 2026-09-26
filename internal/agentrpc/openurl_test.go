package agentrpc

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// hostGuest is a yamux pair over a real socket pair: the guest is the
// agent's GuestSession, the host the daemon's client side.
func hostGuest(t *testing.T) (*yamux.Session, *GuestSession) {
	t.Helper()
	hc, gc := tcpPipe(t)
	host, err := yamux.Client(Stdio{In: hc, Out: hc}, MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	gs, err := yamux.Server(Stdio{In: gc, Out: gc}, MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hc.Close(); gc.Close(); host.Close(); gs.Close() })
	return host, &GuestSession{sess: gs}
}

// The new exchange: the agent's OpenURL gets back the host's reply line.
func TestOpenURLReply(t *testing.T) {
	host, g := hostGuest(t)
	go func() {
		s, err := host.Accept()
		if err != nil {
			return
		}
		ev, err := ReadEvent(s)
		if err != nil || ev.URL != "https://example.com/" {
			s.Close()
			return
		}
		WriteReply(s, OpenReply{Opened: true, URL: ev.URL})
		s.Close()
	}()
	line, err := g.OpenURL("https://example.com/", 5*time.Second)
	if err != nil || string(line) != `{"opened":true,"url":"https://example.com/"}`+"\n" {
		t.Fatalf("OpenURL = %q, %v", line, err)
	}
}

// A host older than replies reads the event and closes the stream: the
// agent reports ErrNoReply at once, it does not wait out its bound.
func TestOpenURLOldHost(t *testing.T) {
	host, g := hostGuest(t)
	go func() {
		s, err := host.Accept()
		if err != nil {
			return
		}
		ReadEvent(s)
		s.Close()
	}()
	start := time.Now()
	_, err := g.OpenURL("https://example.com/", time.Minute)
	if !errors.Is(err, ErrNoReply) || time.Since(start) > 5*time.Second {
		t.Fatalf("OpenURL against an old host = %v after %s", err, time.Since(start))
	}
}

// ReadEvent refuses a stream that is not an event.
func TestReadEventRefusesOtherStreams(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	go func() { b.Write([]byte("rpc\n")); b.Close() }()
	if _, err := ReadEvent(a); err == nil {
		t.Fatal("ReadEvent accepted an rpc stream")
	}
}

func TestOpenReplyMessage(t *testing.T) {
	u := "http://localhost:3000/"
	for _, tc := range []struct {
		r    OpenReply
		want string
	}{
		{OpenReply{Opened: true, URL: u, Guest: 3000, HostPort: 3000}, "devvm: opened on host -> " + u},
		{OpenReply{Opened: true, URL: "http://localhost:3001/", Guest: 3000, HostPort: 3001},
			"devvm: opened on host -> http://localhost:3001/   (guest 3000, host port bumped to 3001)"},
		{OpenReply{Opened: true}, "devvm: opened on host -> " + u},
		{OpenReply{Err: "callback port 1455 is in use on the host"},
			"devvm: not opened on host (callback port 1455 is in use on the host); open it yourself: " + u},
		{OpenReply{Err: "no browser opener", Printed: true}, "devvm: not opened on host (no browser opener); URL printed there"},
	} {
		if got := tc.r.Message(u); got != tc.want {
			t.Errorf("Message(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}

// An event body past MaxEventBody is refused, not buffered without bound.
func TestReadEventBodyBounded(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	go func() {
		b.Write([]byte("event\n{\"type\":\"open-url\",\"url\":\"https://x/"))
		big := make([]byte, 4096)
		for i := range big {
			big[i] = 'a'
		}
		for n := 0; n < MaxEventBody*2; n += len(big) {
			if _, err := b.Write(big); err != nil {
				return
			}
		}
		b.Close()
	}()
	if _, err := ReadEvent(a); err == nil {
		t.Fatal("ReadEvent accepted a body past its bound")
	}
}

// A guest that never sends a newline cannot make the host buffer past the
// bound: the header read ends at the limit, well before the deadline.
func TestReadEventHeaderBounded(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	var sent atomic.Int64
	go func() {
		chunk := make([]byte, 4096)
		for i := range chunk {
			chunk[i] = 'x'
		}
		for sent.Load() < 4*MaxEventBody {
			n, err := b.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
		b.Close()
	}()
	if _, err := ReadEvent(a); err == nil {
		t.Fatal("ReadEvent accepted a header with no newline")
	}
	// net.Pipe is unbuffered: every byte sent was read by ReadEvent.
	if n := sent.Load(); n > MaxEventBody+maxHeaderLine+4096 {
		t.Fatalf("ReadEvent read %d bytes of a newline-free header, bound %d", n, MaxEventBody+maxHeaderLine)
	}
}
