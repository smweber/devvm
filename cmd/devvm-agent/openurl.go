package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/smweber/devvm/internal/agentrpc"
)

// runOpenURL is the BROWSER shim: a login tool launches it with a URL, and it
// hands the URL to the running serve agent over the guest socket, which
// pushes it to the host and waits for the host's answer (browser-bridge.md
// §2). With no agent listening, it prints the URL so the user can open it
// manually.
func runOpenURL(args []string) {
	if len(args) == 0 {
		fatal("open-url needs a URL")
	}
	url := args[0]
	conn, err := net.Dial("unix", guestSockPath(sockName))
	if err != nil {
		fmt.Fprintf(os.Stderr, "devvm: open this URL in your host browser: %s\n", url)
		return
	}
	defer conn.Close()
	fmt.Fprintf(conn, "%s\n", url)
	fmt.Fprintln(os.Stderr, shimMessage(conn, url))
}

// shimMessage reads the agent's reply line and words it. No reply (the
// agent's host closed the event without answering: a daemon older than
// replies, which opened it on its own) keeps the old wording.
func shimMessage(conn net.Conn, url string) string {
	_ = conn.SetReadDeadline(time.Now().Add(agentrpc.ShimReplyWait))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if strings.TrimSpace(line) == "" {
		if err != nil && isTimeout(err) {
			return "devvm: sent to host, no answer in time: " + url
		}
		return "devvm: sent to host browser -> " + url
	}
	var r agentrpc.OpenReply
	if json.Unmarshal([]byte(line), &r) != nil {
		return "devvm: sent to host browser -> " + url
	}
	return r.Message(url)
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}
