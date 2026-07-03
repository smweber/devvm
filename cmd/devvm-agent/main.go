// Command devvm-agent runs inside a guest (as the dev user). It is copied in by
// the host and invoked over the machine's exec channel. Modes:
//
//	serve            long-lived: yamux-multiplex forwards + rpc + events over stdio
//	open-url URL     BROWSER shim: hand a login URL to the running serve agent
//	keys ...         one-shot authorized_keys operations (list/add/revoke/cleanup)
//
// The single persistent `serve` exec is the crux of the design: smolvm has poor
// concurrency across separate exec sessions, so every forward and event must
// ride one exec (see devvm/mux history). One-shot modes are a single exec each,
// which is fine.
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
	case "keys":
		runKeys(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: devvm-agent {serve|open-url|keys} ...")
	os.Exit(2)
}
