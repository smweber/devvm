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

// guestSockPath is where the running serve agent listens for open-url handoffs
// from the BROWSER shim (a guest-local unix socket under the dev user's home).
func guestSockPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/home/" + os.Getenv("USER")
	}
	return filepath.Join(home, ".devvm", "agent.sock")
}

// runServe carries every forward (and rpc/events) for one machine over this
// single exec's stdio, multiplexed with yamux. On ready it also starts a guest
// unix-socket listener so the open-url shim can push login URLs to the host.
func runServe() {
	err := agentrpc.Serve(os.Stdin, os.Stdout, agentrpc.GuestHandlers{},
		func(g *agentrpc.GuestSession) { go serveURLSocket(g) })
	if err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func serveURLSocket(g *agentrpc.GuestSession) {
	path := guestSockPath()
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
