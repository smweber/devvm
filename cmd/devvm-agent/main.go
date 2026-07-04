// Command devvm-agent runs inside a guest. It is copied in by the host and
// invoked over the machine's exec channel. Modes:
//
//	serve            long-lived: yamux-multiplex forwards + rpc + events over stdio
//	open-url URL     BROWSER shim: hand a login URL to the running serve agent
//
// The single persistent `serve` exec is the crux of the design: smolvm has poor
// concurrency across separate exec sessions, so every forward and event must
// ride one exec (see devvm/mux history). authorized_keys management is done
// host-side (internal/keys over a plain exec), so it needs no agent.
package main

import (
	"fmt"
	"log"
	"os"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("devvm-agent: ")
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		runServe()
	case "open-url":
		runOpenURL(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: devvm-agent {serve|open-url} ...")
	os.Exit(2)
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "devvm-agent: "+msg)
	os.Exit(1)
}
