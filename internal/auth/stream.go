package auth

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os/exec"
	"syscall"

	"github.com/smweber/devvm/internal/agentrpc"
)

// lineReader reads a stream's header line then its JSON event body from one
// buffered reader (so the event bytes buffered past the header aren't lost).
type lineReader struct{ r *bufio.Reader }

func newLineReader(c net.Conn) *lineReader { return &lineReader{r: bufio.NewReader(c)} }

func (l *lineReader) header() (string, error) { return agentrpc.ReadHeader(l.r) }

func (l *lineReader) event() (agentrpc.Event, error) {
	var e agentrpc.Event
	err := json.NewDecoder(l.r).Decode(&e)
	return e, err
}

// isSignalExit reports whether a command exited due to a signal (e.g. Ctrl-C),
// which should abort the whole auth flow rather than continue to the next tool.
func isSignalExit(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return true
		}
		return ee.ExitCode() >= 128
	}
	return false
}
