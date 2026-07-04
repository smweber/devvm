package main

import (
	"fmt"
	"net"
	"os"
)

// runOpenURL is the BROWSER shim: a login tool launches it with a URL, and it
// hands the URL to a running serve agent over a guest socket, which pushes it
// to the host browser. A live auth session (its own socket) wins over the
// forward daemon's agent; with neither listening, it prints the URL so the
// user can open it manually.
func runOpenURL(args []string) {
	if len(args) == 0 {
		fatal("open-url needs a URL")
	}
	url := args[0]
	for _, name := range []string{authSockName, defaultSockName} {
		conn, err := net.Dial("unix", guestSockPath(name))
		if err != nil {
			continue
		}
		defer conn.Close()
		fmt.Fprintf(conn, "%s\n", url)
		fmt.Fprintf(os.Stderr, "devvm: sent to host browser -> %s\n", url)
		return
	}
	fmt.Fprintf(os.Stderr, "devvm: open this URL in your host browser: %s\n", url)
}
