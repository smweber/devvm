package main

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The shim prints the host's reply; with none, or one that is not JSON (an
// agent that failed), it says so and fails. It succeeds only when the URL
// reached the host's user: opened, or printed on the host's terminal.
func TestShimMessage(t *testing.T) {
	for _, tc := range []struct {
		agent string
		want  string
		ok    bool
	}{
		{`{"opened":true,"url":"http://localhost:3001/","guest":3000,"host_port":3001}` + "\n",
			"devvm: opened on host -> http://localhost:3001/   (guest 3000, host port bumped to 3001)", true},
		{`{"opened":false,"err":"no devvm session is attached to this machine"}` + "\n",
			"devvm: not opened on host (no devvm session is attached to this machine); open it yourself: https://x/", false},
		{`{"opened":false,"err":"rate limited"}` + "\n",
			"devvm: not opened on host (rate limited); open it yourself: https://x/", false},
		{`{"opened":false,"err":"no opener","printed":true}` + "\n",
			"devvm: not opened on host (no opener); URL printed there", true},
		{"", "devvm: no answer from the devvm agent; open it yourself: https://x/", false},
		{"not json\n", "devvm: no answer from the devvm agent; open it yourself: https://x/", false},
	} {
		shim, agent := net.Pipe()
		go func() {
			agent.Write([]byte(tc.agent))
			agent.Close()
		}()
		got, ok := shimMessage(shim, "https://x/")
		if got != tc.want || ok != tc.ok {
			t.Errorf("reply %q: shim printed %q ok=%v, want %q ok=%v", tc.agent, got, ok, tc.want, tc.ok)
		}
		shim.Close()
	}
}

// The agent's own answer when the host gave none: a timeout, a broken
// stream or a close without a reply is named, never passed off as "sent".
func TestShimReply(t *testing.T) {
	if got := shimReply([]byte("{\"opened\":true}\n"), nil); string(got) != "{\"opened\":true}\n" {
		t.Errorf("verbatim reply = %q", got)
	}
	if got := string(shimReply(nil, io.ErrUnexpectedEOF)); got != `{"opened":false,"err":"the host did not answer: unexpected EOF"}`+"\n" {
		t.Errorf("no reply = %q", got)
	}
	if got := string(shimReply(nil, os.ErrDeadlineExceeded)); got != `{"opened":false,"err":"the host did not answer in time"}`+"\n" {
		t.Errorf("timeout = %q", got)
	}
	if got := string(shimReply(nil, errors.New("stream reset"))); got != `{"opened":false,"err":"the host did not answer: stream reset"}`+"\n" {
		t.Errorf("error = %q", got)
	}
}

// The shim's exit status, end to end (this test binary re-exec'ed as the
// shim): 0 when the host opened the URL, 1 when it refused, and 1 when no
// agent listens and the URL is only printed in the guest.
func TestOpenURLExitStatus(t *testing.T) {
	if os.Getenv("DEVVM_TEST_OPENURL") == "1" {
		runOpenURL([]string{"https://x/"})
		os.Exit(0)
	}
	// A short HOME: the socket path must fit sun_path on macOS too.
	home, err := os.MkdirTemp("", "dvo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	run := func() (int, string) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestOpenURLExitStatus$")
		cmd.Env = append(os.Environ(), "DEVVM_TEST_OPENURL=1", "HOME="+home)
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), string(out)
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0, string(out)
	}
	if code, out := run(); code != 1 || !strings.Contains(out, "open this URL in your host browser: https://x/") {
		t.Errorf("no agent: exit %d, output %q; want 1 and the URL", code, out)
	}

	if err := os.MkdirAll(filepath.Join(home, ".devvm"), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(home, ".devvm", sockName))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, tc := range []struct {
		reply string
		code  int
	}{
		{`{"opened":true}`, 0},
		{`{"opened":false,"err":"refused"}`, 1},
		{`{"opened":false,"err":"no opener","printed":true}`, 0},
	} {
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 64)
			_, _ = c.Read(buf)
			c.Write([]byte(tc.reply + "\n"))
			c.Close()
		}()
		if code, out := run(); code != tc.code {
			t.Errorf("reply %s: exit %d, want %d\n%s", tc.reply, code, tc.code, out)
		}
	}
}
