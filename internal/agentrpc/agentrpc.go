// Package agentrpc is the wire protocol carried over the one yamux session that
// links the host session daemon (yamux client) to the guest devvm-agent (yamux
// server). Every stream opens with a single newline-terminated header line
// naming its type:
//
//	forward HOST:PORT   host->guest: carry a TCP connection to HOST:PORT (mux)
//	rpc                 host->guest: one JSON request/response (keys/repos/...)
//	event               guest->host: pushed events (open-url / callback-wanted)
//
// The forward path is the direct descendant of devvm-mux: one exec, many
// streams, because smolvm has poor concurrency across separate exec sessions.
package agentrpc

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/hashicorp/yamux"
)

// Stream type headers.
const (
	TypeForward = "forward" // "forward HOST:PORT"
	TypeRPC     = "rpc"
	TypeEvent   = "event"
)

// Stdio adapts a separate reader/writer (an exec's stdin/stdout) into the single
// io.ReadWriteCloser yamux wants. Close is a no-op: the session ends by tearing
// down the exec, not by closing these fds.
type Stdio struct {
	In  io.Reader
	Out io.Writer
}

func (s Stdio) Read(p []byte) (int, error)  { return s.In.Read(p) }
func (s Stdio) Write(p []byte) (int, error) { return s.Out.Write(p) }
func (Stdio) Close() error                  { return nil }

// MuxConfig tunes yamux for a session carried over an exec pipe. Keepalives let
// either end notice a dead peer (VM stopped, ssh dropped) instead of hanging.
func MuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.LogOutput = os.Stderr
	return c
}

// WriteHeader sends a stream's type/args header line.
func WriteHeader(w io.Writer, line string) error {
	_, err := io.WriteString(w, line+"\n")
	return err
}

// ReadHeader reads a single header line from a stream (no trailing newline).
func ReadHeader(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Splice copies both directions between two connections, returning once either
// side closes, then closes both so the other copy unblocks. Ports splice() from
// devvm-mux.
//
// Half-close (shutdown(WR)) is NOT propagated: yamux streams (this version)
// have no CloseWrite, so when one direction hits EOF the whole splice tears
// down. Protocols that send EOF and then expect to keep reading the response
// will see it truncated; everything devvm forwards today (HTTP, VNC, OAuth
// callbacks) closes bidirectionally.
func Splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

// ForwardHeader builds the header line that asks the guest to dial target.
func ForwardHeader(target string) string {
	return fmt.Sprintf("%s %s", TypeForward, target)
}
