package backend

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/reach"
)

// probeSSH puts a fake ssh on PATH: `-O check` exits with the code in
// <dir>/check (255, no master, when absent), `-G` prints <dir>/g, and every
// call is logged. ssh is exec'd directly (no login shell), so prepending
// to PATH is enough here.
func probeSSH(t *testing.T, check int, g string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" +
		"case \" $* \" in\n" +
		"*\" -O check \"*) exit " + strconv.Itoa(check) + ";;\n" +
		"*\" -G \"*) cat " + filepath.Join(dir, "g") + "; exit 0;;\n" +
		"esac\nexit 99\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "g"), []byte(g), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func probeBackend(t *testing.T) (*sshBackend, config.Paths) {
	d := t.TempDir()
	p := config.Paths{Config: filepath.Join(d, "c"), State: filepath.Join(d, "s")}
	return &sshBackend{m: config.NewRemote("box", config.BackendRemoteUnmanaged, "u@box"), paths: p}, p
}

func noDial(t *testing.T) {
	t.Helper()
	old := probeDial
	probeDial = func(context.Context, string) error { t.Error("dialled"); return nil }
	t.Cleanup(func() { probeDial = old })
}

func logLines(t *testing.T, log string) []string {
	data, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestProbeLiveMaster(t *testing.T) {
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "7")
	log := probeSSH(t, 0, "")
	noDial(t)
	b, p := probeBackend(t)
	e := b.Probe(context.Background())
	if e.State != reach.Reachable || e.How != reach.HowMaster {
		t.Fatalf("probe = %+v", e)
	}
	if c := reach.Read(p, "box"); c.State != reach.Reachable || c.How != reach.HowMaster {
		t.Errorf("cache = %+v", c)
	}
	lines := logLines(t, log)
	if len(lines) != 1 || !strings.Contains(lines[0], "ConnectTimeout=7") ||
		!strings.Contains(lines[0], "-O check -o ControlPath="+p.ControlPath()+" u@box") {
		t.Errorf("ssh calls:\n%s", strings.Join(lines, "\n"))
	}
}

// The daemon's master (run/NAME.master) is asked first when its socket
// exists, and the shared one after it.
func TestProbeDaemonMasterFirst(t *testing.T) {
	log := probeSSH(t, 255, "hostname 127.0.0.1\nport 1\nproxyjump j\n")
	noDial(t)
	b, p := probeBackend(t)
	if err := p.EnsureRuntimeDir(); err != nil {
		t.Fatal(err)
	}
	daemon := b.SSHConn().ControlPath
	if err := os.WriteFile(daemon, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b.Probe(context.Background())
	lines := logLines(t, log)
	if len(lines) != 3 || !strings.Contains(lines[0], "ControlPath="+daemon) ||
		!strings.Contains(lines[1], "ControlPath="+p.ControlPath()) || !strings.Contains(lines[2], " -G u@box") {
		t.Errorf("ssh calls:\n%s", strings.Join(lines, "\n"))
	}
}

func TestProbeProxyNotDialled(t *testing.T) {
	for _, tc := range []struct{ g, reason string }{
		{"hostname 10.0.0.1\nport 22\nproxyjump bastion\n", "ProxyJump"},
		{"hostname 10.0.0.1\nport 22\nproxycommand nc %h %p\n", "ProxyCommand"},
	} {
		probeSSH(t, 255, tc.g)
		noDial(t)
		b, p := probeBackend(t)
		e := b.Probe(context.Background())
		if e.State != reach.Unknown || e.Reason != tc.reason || !e.Verified.IsZero() {
			t.Errorf("%s: probe = %+v", tc.reason, e)
		}
		if c := reach.Read(p, "box"); c.State != reach.Unknown || c.Reason != tc.reason {
			t.Errorf("%s: cache = %+v", tc.reason, c)
		}
	}
}

func TestProbeTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	probeSSH(t, 255, "hostname 127.0.0.1\nport "+strconv.Itoa(port)+"\nproxyjump none\n")
	b, _ := probeBackend(t)
	if e := b.Probe(context.Background()); e.State != reach.Reachable || e.How != reach.HowProbe {
		t.Errorf("listening = %+v", e)
	}
	// The same port once nothing listens: refused, at once. (The reason's
	// words are the OS's errno, which a sandboxed network stack may not
	// spell as ECONNREFUSED; dialReason's mapping is tested on its own.)
	ln.Close()
	if e := b.Probe(context.Background()); e.State != reach.Unreachable || e.Reason == "" || e.How != reach.HowProbe {
		t.Errorf("refused = %+v", e)
	}
}

func TestDialReason(t *testing.T) {
	op := func(err error) error { return &net.OpError{Op: "dial", Net: "tcp", Err: err} }
	for _, tc := range []struct {
		err  error
		want string
	}{
		{op(os.NewSyscallError("connect", syscall.ECONNREFUSED)), "connection refused"},
		{op(os.NewSyscallError("connect", syscall.EHOSTUNREACH)), "no route to host"},
		{context.DeadlineExceeded, "connect timed out"},
		{op(&net.DNSError{Err: "no such host", Name: "nope", IsNotFound: true}), "cannot resolve nope"},
		{op(errors.New("something odd")), "something odd"},
	} {
		if got := dialReason(tc.err, "nope"); got != tc.want {
			t.Errorf("dialReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// A dial that never answers is cut by the round's deadline: unreachable,
// timed out, within it. Injected, so no unroutable address is relied on.
func TestProbeTimeout(t *testing.T) {
	probeSSH(t, 255, "hostname 192.0.2.1\nport 22\n")
	old := probeDial
	var addr string
	probeDial = func(ctx context.Context, a string) error { addr = a; <-ctx.Done(); return ctx.Err() }
	defer func() { probeDial = old }()
	b, _ := probeBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	e := b.Probe(ctx)
	if e.State != reach.Unreachable || e.Reason != "connect timed out" {
		t.Errorf("probe = %+v", e)
	}
	if addr != "192.0.2.1:22" {
		t.Errorf("dialled %q", addr)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("probe took %s", d)
	}
}

// Status probes a remote box, and a hub's own conf is left to its pipe.
func TestStatusProbes(t *testing.T) {
	log := probeSSH(t, 0, "")
	b, _ := probeBackend(t)
	st, err := b.Status()
	if err != nil || !st.Running || !strings.Contains(st.Raw, "reachable, verified") {
		t.Errorf("status = %+v, %v", st, err)
	}
	os.Remove(log)
	hb := &sshBackend{m: config.NewHub("h", "u@h"), paths: b.paths}
	if st, _ := hb.Status(); !st.Running {
		t.Errorf("hub status = %+v", st)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("a hub conf was probed")
	}
}
