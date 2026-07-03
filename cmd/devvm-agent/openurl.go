package main

import (
	"fmt"
	"net"
	"os"
)

// runOpenURL is the BROWSER shim: a login tool launches it with a URL, and it
// hands the URL to the running serve agent over the guest socket, which pushes
// it to the host browser. If no serve agent is listening, it prints the URL so
// the user can open it manually.
func runOpenURL(args []string) {
	if len(args) == 0 {
		fatal("open-url needs a URL")
	}
	url := args[0]
	conn, err := net.Dial("unix", guestSockPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "devvm: open this URL in your host browser: %s\n", url)
		return
	}
	defer conn.Close()
	fmt.Fprintf(conn, "%s\n", url)
	fmt.Fprintf(os.Stderr, "devvm: sent to host browser -> %s\n", url)
}
