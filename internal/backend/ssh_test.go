package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

func TestParseConnectTimeout(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		bad  bool
	}{
		{in: "", want: defaultSSHConnectTimeout},
		{in: "30", want: 30},
		{in: "30s", want: 30},
		{in: "1m", want: 60},
		{in: "1500ms", want: 2}, // rounded up: ssh takes whole seconds
		{in: "0", bad: true},
		{in: "-5", bad: true},
		{in: "0s", bad: true},
		{in: "soon", bad: true},
	} {
		got, err := parseConnectTimeout(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("%q: want error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}

func TestSSHConnectTimeoutEnv(t *testing.T) {
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "45s")
	if got := SSHConnectTimeout(); got != 45 {
		t.Fatalf("got %d, want 45", got)
	}
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "later")
	if got := SSHConnectTimeout(); got != defaultSSHConnectTimeout {
		t.Fatalf("unparseable value gave %d, want the default %d", got, defaultSSHConnectTimeout)
	}
}

// scp splits host from path at the first colon, so a bare IPv6 host (a
// provider box with ipv4 = false) is bracketed; nothing else changes.
func TestSCPTargetBracketsIPv6(t *testing.T) {
	for _, tc := range []struct{ dest, want string }{
		{"dev@2001:db8::1", "dev@[2001:db8::1]:/var/tmp/x"},
		{"2001:db8::1", "[2001:db8::1]:/var/tmp/x"},
		{"dev@[2001:db8::1]", "dev@[2001:db8::1]:/var/tmp/x"},
		{"dev@192.0.2.1", "dev@192.0.2.1:/var/tmp/x"},
		{"alias", "alias:/var/tmp/x"},
	} {
		if got := scpTarget(tc.dest, "/var/tmp/x"); got != tc.want {
			t.Errorf("scpTarget(%q) = %q, want %q", tc.dest, got, tc.want)
		}
	}
}

// shortTempDir is a temp dir short enough for an ssh ControlPath under it
// (t.TempDir's names alone can push c-%C past sun_path).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "devvm-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir
}

// A ControlPath ssh could not bind (the master's path plus ".", 16
// characters, inside sun_path) is refused before ssh runs, naming its
// length; a normal state dir passes.
func TestControlPathTooLong(t *testing.T) {
	if err := controlPathErr("/home/dev/.local/state/devvm/c-%C"); err != nil {
		t.Fatalf("a normal state dir: %v", err)
	}
	long := "/" + strings.Repeat("x", 60) + "/c-%C" // 40-hex %C: 104 bytes
	err := controlPathErr(long)
	if err == nil || !strings.Contains(err.Error(), "state dir path too long for ssh ControlPath (104 bytes)") ||
		!strings.Contains(err.Error(), "--state-dir or XDG_STATE_HOME") {
		t.Fatalf("long: %v", err)
	}
	// Run refuses it without running ssh at all.
	dir := shortTempDir(t)
	log := filepath.Join(dir, "ssh.log")
	os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\necho ran >> "+log+"\n"), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := filepath.Join(dir, strings.Repeat("s", 80))
	b := &sshBackend{m: config.NewRemote("x", true, "dev@x.example"), paths: config.LegacyPaths(state)}
	if err := b.Run(context.Background(), ExecOpts{}, "true"); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("ssh ran")
	}
}
