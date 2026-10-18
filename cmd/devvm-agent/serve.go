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

// sockName is the guest unix socket where the running serve agent accepts
// open-url handoffs from the BROWSER shim. There is one: the machine's
// forward daemon owns the only serve exec on smol and on remote boxes alike,
// and `auth` rides that daemon (browser-bridge.md §6), so the private auth
// session and its own socket (`agent-auth.sock`, `serve --auth`) are gone.
const sockName = "agent.sock"

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
// read back what the host did. The host may also push its clock (clock.go).
func runServe(args []string) {
	if len(args) > 0 {
		fatal("unknown serve flag: " + args[0])
	}
	h := agentrpc.GuestHandlers{Clock: newClockSyncer().handle}
	err := agentrpc.Serve(os.Stdin, os.Stdout, h,
		func(g *agentrpc.GuestSession) { go serveURLSocket(g, guestSockPath(sockName)) })
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
