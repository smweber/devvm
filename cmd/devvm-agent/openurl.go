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
//
// It exits 0 only when the URL reached a person on the host: opened there,
// or printed on the host's terminal (Printed), or handed to a host too old
// to reply (which opened it on its own). Everything else (refused, no
// session attached, no agent to ask, no answer in time) exits 1 after the
// same line, so a caller that checks BROWSER's status can tell: gh then
// says it failed to open a browser and asks for the URL to be entered by
// hand, and codex and claude print the URL whatever the status.
func runOpenURL(args []string) {
	if len(args) == 0 {
		fatal("open-url needs a URL")
	}
	url := args[0]
	conn, err := net.Dial("unix", guestSockPath(sockName))
	if err != nil {
		fmt.Fprintf(os.Stderr, "devvm: open this URL in your host browser: %s\n", url)
		os.Exit(1)
	}
	fmt.Fprintf(conn, "%s\n", url)
	msg, ok := shimMessage(conn, url)
	conn.Close()
	fmt.Fprintln(os.Stderr, msg)
	if !ok {
		os.Exit(1)
	}
}

// shimMessage reads the agent's reply line and words it; ok is whether
// the URL reached the host's user (see runOpenURL). No reply (the agent's
// host closed the event without answering: a daemon older than replies,
// which opened it on its own) keeps the old wording.
func shimMessage(conn net.Conn, url string) (msg string, ok bool) {
	_ = conn.SetReadDeadline(time.Now().Add(agentrpc.ShimReplyWait))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if strings.TrimSpace(line) == "" {
		if err != nil && isTimeout(err) {
			return "devvm: sent to host, no answer in time: " + url, false
		}
		return "devvm: sent to host browser -> " + url, true
	}
	var r agentrpc.OpenReply
	if json.Unmarshal([]byte(line), &r) != nil {
		return "devvm: sent to host browser -> " + url, true
	}
	return r.Message(url), r.Opened || r.Printed
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}
