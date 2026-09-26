package agentrpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// The open-url exchange on one event stream (browser-bridge.md §2):
//
//	guest → host:  event\n{"type":"open-url","url":"…"}\n
//	host → guest:  {"opened":…,…}\n          (an OpenReply), then close
//
// Request/response since roadmap step 7. The two sides stay compatible with
// the old write-and-close: an old agent closes after its event and never
// reads the reply (the host's write lands on a closed stream and is
// dropped, bounded by a deadline, so it never hangs); an old host closes
// without replying, which OpenURL reports as ErrNoReply.

// ErrNoReply is OpenURL's answer when the host closed the stream without a
// reply: a host older than replies, which opened (or dropped) the URL on its
// own.
var ErrNoReply = errors.New("the host closed the event without a reply")

// OpenURL pushes an open-url event and waits up to wait for the host's reply
// line, returned raw (the agent hands it to the shim verbatim).
func (g *GuestSession) OpenURL(url string, wait time.Duration) ([]byte, error) {
	s, err := g.OpenEvent()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return openURLOn(s, url, wait)
}

func openURLOn(s net.Conn, url string, wait time.Duration) ([]byte, error) {
	if err := json.NewEncoder(s).Encode(Event{Type: EventOpenURL, URL: url}); err != nil {
		return nil, err
	}
	_ = s.SetReadDeadline(time.Now().Add(wait))
	line, err := bufio.NewReader(s).ReadBytes('\n')
	line = bytes.TrimSpace(line)
	if len(line) > 0 {
		return append(line, '\n'), nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return nil, ErrNoReply
	}
	return nil, err
}

// eventReadWait bounds how long the host waits for an event's body after
// its header: the agent writes both at once, so a stream that stalls here
// is broken, not slow.
const eventReadWait = 10 * time.Second

// MaxEventBody bounds an event's JSON body, host side; maxHeaderLine its
// header line ("event\n", with room to spare).
const (
	MaxEventBody  = 64 << 10
	maxHeaderLine = 64
)

// replyWriteWait bounds the host's reply write. An old agent closed its
// side after the event; the write must fail or land, never block.
const replyWriteWait = 5 * time.Second

// ReadEvent reads an event stream's header and JSON body, host side. A
// stream that is not an event, or whose body does not arrive in time, is an
// error; the caller closes the stream.
func ReadEvent(s net.Conn) (Event, error) {
	_ = s.SetReadDeadline(time.Now().Add(eventReadWait))
	// The whole stream is bounded, header included: ReadHeader reads to a
	// newline, and a guest that never sends one would otherwise be
	// buffered without limit for the full eventReadWait.
	br := bufio.NewReader(io.LimitReader(s, MaxEventBody+maxHeaderLine))
	header, err := ReadHeader(br)
	if err != nil {
		return Event{}, err
	}
	if header != TypeEvent {
		return Event{}, fmt.Errorf("not an event stream: %q", header)
	}
	var ev Event
	// Bounded: the body is guest-supplied, and a URL is not 64 KiB.
	if err := json.NewDecoder(io.LimitReader(br, MaxEventBody)).Decode(&ev); err != nil {
		return Event{}, err
	}
	_ = s.SetReadDeadline(time.Time{})
	return ev, nil
}

// WriteReply answers an event, host side, bounded by replyWriteWait.
func WriteReply(s net.Conn, r OpenReply) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_ = s.SetWriteDeadline(time.Now().Add(replyWriteWait))
	_, err = s.Write(append(b, '\n'))
	return err
}
