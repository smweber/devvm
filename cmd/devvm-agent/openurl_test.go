package main

import (
	"errors"
	"net"
	"os"
	"testing"

	"github.com/smweber/devvm/internal/agentrpc"
)

// The shim prints the host's reply; with none (an agent whose host predates
// replies closes the socket without one) it keeps the old wording.
func TestShimMessage(t *testing.T) {
	for _, tc := range []struct {
		agent string
		want  string
	}{
		{`{"opened":true,"url":"http://localhost:3001/","guest":3000,"host_port":3001}` + "\n",
			"devvm: opened on host -> http://localhost:3001/   (guest 3000, host port bumped to 3001)"},
		{`{"opened":false,"err":"no devvm session is attached to this machine"}` + "\n",
			"devvm: not opened on host (no devvm session is attached to this machine); open it yourself: https://x/"},
		{"", "devvm: sent to host browser -> https://x/"},
	} {
		shim, agent := net.Pipe()
		go func() {
			agent.Write([]byte(tc.agent))
			agent.Close()
		}()
		if got := shimMessage(shim, "https://x/"); got != tc.want {
			t.Errorf("reply %q: shim printed %q, want %q", tc.agent, got, tc.want)
		}
		shim.Close()
	}
}

// The agent's own answer when the host gave none: a timeout or a broken
// stream is named, never passed off as "sent"; a host that closed without
// a reply (older than replies) gets the shim's old wording.
func TestShimReply(t *testing.T) {
	if got := shimReply([]byte("{\"opened\":true}\n"), nil); string(got) != "{\"opened\":true}\n" {
		t.Errorf("verbatim reply = %q", got)
	}
	if got := shimReply(nil, agentrpc.ErrNoReply); got != nil {
		t.Errorf("old host = %q, want nothing", got)
	}
	if got := string(shimReply(nil, os.ErrDeadlineExceeded)); got != `{"opened":false,"err":"the host did not answer in time"}`+"\n" {
		t.Errorf("timeout = %q", got)
	}
	if got := string(shimReply(nil, errors.New("stream reset"))); got != `{"opened":false,"err":"the host did not answer: stream reset"}`+"\n" {
		t.Errorf("error = %q", got)
	}
}
