package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/smweber/devvm/internal/agentrpc"
)

// Guest unix sockets where a running serve agent accepts open-url handoffs from
// the BROWSER shim. The forward daemon's agent owns the default socket; an
// `auth` session's agent listens on its own auth socket so the two never steal
// each other's (an auth session must receive its login URLs itself — it does
// the callback bridging — while the daemon agent merely opens URLs).
const (
	defaultSockName = "agent.sock"
	authSockName    = "agent-auth.sock"
)

// guestSockPath resolves a socket name under the dev user's ~/.devvm.
func guestSockPath(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/home/" + os.Getenv("USER")
	}
	return filepath.Join(home, ".devvm", name)
}

// runServe carries every forward (and rpc/events) for one machine over this
// single exec's stdio, multiplexed with yamux. On ready it also starts a guest
// unix-socket listener so the open-url shim can push URLs to the host and
// read back what the host did.
// --auth selects the auth session's socket (see the socket-name comment).
func runServe(args []string) {
	sock := defaultSockName
	for _, a := range args {
		switch a {
		case "--auth":
			sock = authSockName
		default:
			fatal("unknown serve flag: " + a)
		}
	}
	err := agentrpc.Serve(os.Stdin, os.Stdout, agentrpc.GuestHandlers{},
		func(g *agentrpc.GuestSession) { go serveURLSocket(g, guestSockPath(sock)) })
	if err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func serveURLSocket(g *agentrpc.GuestSession, path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			line, _ := bufio.NewReader(c).ReadString('\n')
			url := strings.TrimSpace(line)
			if url == "" {
				return
			}
			// Request/response (browser-bridge.md §2): the host's reply line
			// goes back to the shim verbatim, and the shim prints it.
			_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			c.Write(shimReply(g.OpenURL(url, agentrpc.AgentReplyWait)))
		}(conn)
	}
}

// shimReply is the line the agent hands the shim for OpenURL's result: the
// host's reply verbatim; nothing when the host closed without one (a host
// older than replies, which opened the URL itself: the shim then says it
// was sent); otherwise a reply of the agent's own naming why no answer
// came, so a timeout never reads as "sent to host browser".
func shimReply(reply []byte, err error) []byte {
	switch {
	case err == nil:
		return reply
	case errors.Is(err, agentrpc.ErrNoReply):
		return nil
	}
	why := "the host did not answer: " + err.Error()
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		why = "the host did not answer in time"
	}
	b, _ := json.Marshal(agentrpc.OpenReply{Err: why})
	return append(b, '\n')
}
