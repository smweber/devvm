package session

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// The hub side of a relay (hub.md §7, roadmap step 6). A laptop daemon
// forwarding to a machine on a hub runs one long-lived
//
//	ssh HUB sh -c 'exec "$SHELL" -lc "$0"' 'env DEVVM_NO_SUBSCRIBE=1 devvm __session NAME'
//
// over its master. `__session` dials (spawning if needed) the hub daemon for
// NAME, prints SessionMarker, and from then on relays bytes verbatim between
// its stdio and that daemon connection. It adds nothing but the marker and
// its own lifetime: the laptop's hubTransport writes the `session {relay:
// true}` open itself and speaks the session line protocol
// (browser-bridge.md §3) directly to the hub daemon, so every forward it adds
// is owned by the relayed connection and goes when the process does.

// SessionMarker is the line `__session` prints before the relayed stream.
// The hub side runs under the user's login shell, and a profile that echoes
// lands in front of it; the laptop discards everything up to and including
// this line, as cp's tar stream does with its own marker (hub.md §6).
const SessionMarker = "devvm-session-v1"

// MarkerLimit bounds how much a login shell may print before a marker. A
// banner is a few lines; a megabyte with no marker in it is not a banner.
const MarkerLimit = 1 << 20

// Errors from SkipToMarker, so callers can word them for their stream.
var (
	ErrMarkerMissing = errors.New("the stream ended before its marker line")
	ErrMarkerLimit   = errors.New("no marker line within the limit")
)

// SkipToMarker discards lines until one equal to marker, refusing a stream
// that ends first (ErrMarkerMissing) or that exceeds limit bytes without it
// (ErrMarkerLimit); any other read error is returned as is. ReadSlice, not
// ReadString: a junk line longer than the buffer is discarded a bufferful at
// a time rather than accumulated. A marker cannot be split by that: it is a
// short line of its own, and bufio compacts before each fill. Nothing past
// the marker is consumed, so br goes on to carry the stream.
func SkipToMarker(br *bufio.Reader, marker string, limit int) error {
	want := []byte(marker + "\n")
	for n := 0; ; {
		line, err := br.ReadSlice('\n')
		n += len(line)
		if err == nil && bytes.Equal(line, want) {
			return nil
		}
		switch err {
		case nil, bufio.ErrBufferFull:
		case io.EOF:
			return ErrMarkerMissing
		default:
			return err
		}
		if n > limit {
			return ErrMarkerLimit
		}
	}
}

// DialConn returns a raw connection to the machine's daemon, spawning the
// daemon first if none is running. `__session` relays it; the caller owns
// the connection and speaks whatever the far end sends.
func DialConn(configDir, name string) (net.Conn, error) {
	if _, err := Dial(configDir, name); err != nil {
		return nil, err
	}
	return net.Dial("unix", socketPath(configDir, name))
}

// Relay is `__session`'s body once the daemon connection is up: the marker,
// then stdin to conn and conn to stdout, verbatim, until either side ends.
//
// The daemon closing the connection (a relay closed on transport death, the
// daemon stopped or cycled) ends the relay, and the process exiting is what
// the laptop's hubTransport sees as dead(). stdin reaching EOF (the laptop
// daemon closed its transport, ssh went away) half-closes conn, so the
// daemon's reader ends the session and drops the forwards it owned, and the
// relay ends once the daemon has closed its side.
//
// stdin should be an *os.File (as a process's stdin is): once the daemon
// side is done, a read deadline unblocks the stdin copy so the relay can
// return; any other reader's copy is left to end with the process.
func Relay(conn net.Conn, stdin io.Reader, stdout io.Writer) error {
	defer conn.Close()
	// A leading newline: a profile whose last echo has no newline of its own
	// must not glue its text onto the marker line. The reader skips the
	// blank line like any other banner line.
	if _, err := io.WriteString(stdout, "\n"+SessionMarker+"\n"); err != nil {
		return err
	}
	inDone := make(chan struct{})
	go func() {
		defer close(inDone)
		_, _ = io.Copy(conn, stdin)
		if hc, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = hc.CloseWrite()
		} else {
			conn.Close()
		}
	}()
	_, err := io.Copy(stdout, conn)
	conn.Close()
	if dl, ok := stdin.(interface{ SetReadDeadline(time.Time) error }); ok && dl.SetReadDeadline(time.Now()) == nil {
		<-inDone
	}
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("relay: %w", err)
	}
	return nil
}
