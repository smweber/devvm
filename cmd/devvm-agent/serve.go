package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"

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
// unix-socket listener so the open-url shim can push login URLs to the host.
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
			stream, err := g.OpenEvent()
			if err != nil {
				return
			}
			defer stream.Close()
			json.NewEncoder(stream).Encode(agentrpc.Event{Type: agentrpc.EventOpenURL, URL: url})
		}(conn)
	}
}
