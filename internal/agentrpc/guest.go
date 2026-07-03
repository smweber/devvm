package agentrpc

import (
	"bufio"
	"io"
	"net"
	"strings"

	"github.com/hashicorp/yamux"
)

// GuestHandlers are the guest-side callbacks the agent supplies. RPC handles an
// "rpc" stream (keys/repos/prereqs — wired in later phases); nil rejects them.
type GuestHandlers struct {
	RPC func(stream net.Conn)
}

// GuestSession wraps the yamux server so the agent can also push event streams
// to the host (open-url / callback-wanted). Held by the agent for its lifetime.
type GuestSession struct {
	sess *yamux.Session
}

// OpenEvent opens a host-bound event stream and writes its header.
func (g *GuestSession) OpenEvent() (net.Conn, error) {
	s, err := g.sess.Open()
	if err != nil {
		return nil, err
	}
	if err := WriteHeader(s, TypeEvent); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Serve runs the guest yamux server over the exec's stdio, dispatching inbound
// streams by header. It returns when the session ends (host closed the exec).
// The GuestSession is handed to onReady before the accept loop starts so the
// agent can wire up event pushing.
func Serve(in io.Reader, out io.Writer, h GuestHandlers, onReady func(*GuestSession)) error {
	sess, err := yamux.Server(Stdio{In: in, Out: out}, MuxConfig())
	if err != nil {
		return err
	}
	if onReady != nil {
		onReady(&GuestSession{sess: sess})
	}
	for {
		stream, err := sess.Accept()
		if err != nil {
			return nil // EOF: host closed the exec channel
		}
		go handleGuestStream(stream, h)
	}
}

func handleGuestStream(stream net.Conn, h GuestHandlers) {
	br := bufio.NewReader(stream)
	header, err := ReadHeader(br)
	if err != nil {
		stream.Close()
		return
	}
	switch {
	case strings.HasPrefix(header, TypeForward+" "):
		target := strings.TrimSpace(header[len(TypeForward)+1:])
		guestForward(stream, br, target)
	case header == TypeRPC:
		if h.RPC != nil {
			h.RPC(bufferedConn{Conn: stream, r: br})
		} else {
			stream.Close()
		}
	default:
		stream.Close()
	}
}

// guestForward dials the requested guest address and splices the stream to it.
// Any bytes already buffered past the header are flushed into the connection
// first so nothing is lost.
func guestForward(stream net.Conn, br *bufio.Reader, target string) {
	up, err := net.Dial("tcp", target)
	if err != nil {
		stream.Close()
		return
	}
	if n := br.Buffered(); n > 0 {
		buf, _ := br.Peek(n)
		up.Write(buf)
	}
	Splice(stream, up)
}

// bufferedConn re-attaches a bufio.Reader (holding bytes read past the header)
// to its net.Conn so RPC handlers read a continuous stream.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }
