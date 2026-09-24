package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// ownerDaemon is a fake forward daemon for the owner-aware CLI paths: it
// answers per op from a table, records every request, and (exitOnStop)
// behaves like a real daemon on stop: it closes its listener and unlinks
// its socket, so WaitGone sees it go.
type ownerDaemon struct {
	mu   sync.Mutex
	reqs []session.Request
	resp map[string]session.Response
}

func (o *ownerDaemon) requests(op string) []session.Request {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []session.Request
	for _, r := range o.reqs {
		if r.Op == op {
			out = append(out, r)
		}
	}
	return out
}

func serveOwnerDaemon(t *testing.T, configDir, name string, resp map[string]session.Response, exitOnStop bool) *ownerDaemon {
	t.Helper()
	if err := config.EnsureRuntimeDir(configDir); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(config.RuntimeDir(configDir), config.RuntimeName(name)+".sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	od := &ownerDaemon{resp: resp}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					return
				}
				var req session.Request
				_ = json.Unmarshal([]byte(line), &req)
				od.mu.Lock()
				od.reqs = append(od.reqs, req)
				r, ok := od.resp[req.Op]
				od.mu.Unlock()
				if !ok {
					r = session.Response{OK: true}
				}
				b, _ := json.Marshal(r)
				c.Write(append(b, '\n'))
				if req.Op == session.OpStop && exitOnStop {
					ln.Close()
					os.Remove(sock)
				}
			}(c)
		}
	}()
	return od
}

func saveRemote(t *testing.T, dir, name string, ports ...string) {
	t.Helper()
	m := &config.Machine{Name: name, Backend: config.BackendRemoteUnmanaged, SSHHost: "dev@example", Ports: ports}
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
}

// `ports rm` (bridge §4): a configured mapping drops conf and reports who
// still holds the forward; an unconfigured port with a ttl owner drops ttl;
// a connection owner is never dropped.
func TestUnportDropsOnlyTheRightOwner(t *testing.T) {
	var out bytes.Buffer
	a := &App{ConfigDir: shortTempDir(t), Stdout: &out, Stderr: io.Discard}
	saveRemote(t, a.ConfigDir, "box", "3000")
	list := session.Response{OK: true, State: session.StateUp, Forwards: []session.Forward{
		{Host: 3000, Guest: 3000, Owners: []string{session.OwnerConf, session.OwnerConnection}},
		{Host: 1455, Guest: 1455, Owners: []string{session.OwnerTTL}},
		{Host: 4000, Guest: 4000, Owners: []string{session.OwnerConnection}},
	}}
	od := serveOwnerDaemon(t, a.ConfigDir, "box", map[string]session.Response{
		session.OpList: list,
		session.OpRemove: {OK: true, Forwards: []session.Forward{
			{Host: 3000, Guest: 3000, Owners: []string{session.OwnerConnection}},
		}},
	}, false)

	if err := a.runUnport("box", "3000"); err != nil {
		t.Fatal(err)
	}
	rm := od.requests(session.OpRemove)
	if len(rm) != 1 || rm[0].Guest != 3000 || rm[0].Owner != session.OwnerConf {
		t.Fatalf("configured rm sent %+v, want one conf remove of 3000", rm)
	}
	if !strings.Contains(out.String(), "localhost:3000 stays up: still held by connection") {
		t.Errorf("output lacks the surviving owner:\n%s", out.String())
	}
	if m, _ := config.LoadAny(a.ConfigDir, "box"); len(m.Ports) != 0 {
		t.Errorf("conf still lists %v", m.Ports)
	}

	od.mu.Lock()
	od.resp[session.OpRemove] = session.Response{OK: true}
	od.mu.Unlock()
	if err := a.runUnport("box", "1455"); err != nil {
		t.Fatalf("rm of a ttl forward: %v", err)
	}
	if rm := od.requests(session.OpRemove); len(rm) != 2 || rm[1].Guest != 1455 || rm[1].Owner != session.OwnerTTL {
		t.Fatalf("ttl rm sent %+v", rm)
	}

	err := a.runUnport("box", "4000")
	if err == nil || !strings.Contains(err.Error(), "held by connection") {
		t.Fatalf("rm of a session's forward = %v, want a refusal", err)
	}
	if rm := od.requests(session.OpRemove); len(rm) != 2 {
		t.Fatalf("a connection-owned forward got a remove: %+v", rm)
	}

	if err := a.runUnport("box", "5000"); err == nil || !strings.Contains(err.Error(), "no forward '5000' configured") {
		t.Fatalf("rm of an unknown port = %v", err)
	}
}

// `ports down` sends down, not stop: a daemon a session holds stays up and
// says why; one nothing holds reports stopped; a pre-owner daemon that
// answers "unknown op" is stopped outright, as `ports down` always did.
func TestTunnelDownLeavesAHeldDaemon(t *testing.T) {
	var out bytes.Buffer
	a := &App{ConfigDir: shortTempDir(t), Stdout: &out, Stderr: io.Discard}
	saveRemote(t, a.ConfigDir, "held", "3000")
	saveRemote(t, a.ConfigDir, "free", "3000")
	saveRemote(t, a.ConfigDir, "old", "3000")
	held := serveOwnerDaemon(t, a.ConfigDir, "held", map[string]session.Response{
		session.OpDown: {OK: true, Sessions: 1},
	}, false)
	free := serveOwnerDaemon(t, a.ConfigDir, "free", map[string]session.Response{
		session.OpDown: {OK: true, Stopped: true},
	}, false)
	old := serveOwnerDaemon(t, a.ConfigDir, "old", map[string]session.Response{
		session.OpDown: {Err: "unknown op: down"},
	}, false)

	if err := a.tunnelDown("held"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "stays up for 1 session") {
		t.Errorf("held output:\n%s", out.String())
	}
	if len(held.requests(session.OpStop)) != 0 || len(held.requests(session.OpDown)) != 1 {
		t.Errorf("held daemon got %+v", held.requests(session.OpStop))
	}
	out.Reset()
	if err := a.tunnelDown("free"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "forwards stopped") || len(free.requests(session.OpStop)) != 0 {
		t.Errorf("free: output %q, stops %+v", out.String(), free.requests(session.OpStop))
	}
	if err := a.tunnelDown("old"); err != nil {
		t.Fatal(err)
	}
	if len(old.requests(session.OpStop)) != 1 {
		t.Errorf("a pre-down daemon was not stopped")
	}
}

// `ports list NAME` shows each forward's owner kinds and exactness after
// the tokens the menubar parses, and the sessions holding the daemon.
func TestPortsListShowsOwners(t *testing.T) {
	var out bytes.Buffer
	a := &App{ConfigDir: shortTempDir(t), Stdout: &out, Stderr: io.Discard}
	saveRemote(t, a.ConfigDir, "box", "3000")
	serveOwnerDaemon(t, a.ConfigDir, "box", map[string]session.Response{
		session.OpList: {OK: true, State: session.StateUp, Sessions: 2, Forwards: []session.Forward{
			{Host: 3000, Guest: 3000, Owners: []string{session.OwnerConf, session.OwnerConnection}},
			{Host: 1455, Guest: 1455, Pending: true, Exact: true, Owners: []string{session.OwnerTTL}},
		}},
	}, false)
	if err := a.runPortsList("box"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"    guest 3000  -> localhost:3000  owner=conf+connection\n",
		"    guest 1455  -> localhost:1455  (pending)  owner=ttl  exact\n",
		"  sessions: 2 holding the forward daemon\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ports list lacks %q:\n%s", want, out.String())
		}
	}
	// What Swift's parsePortsList needs: after trimming, "guest", N, "->",
	// "localhost:M" as the first four space-split tokens, and "(pending)"
	// as a token of its own.
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "guest ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 || f[2] != "->" || !strings.HasPrefix(f[3], "localhost:") {
			t.Errorf("line not parseable by the menubar: %q", line)
		}
	}
}

// status --plain's N counts conf owners only, through the real
// forwardSummary: a daemon whose forwards are all session/ttl-owned reads
// `down` (ports configured) or `-`, never `up:0`.
func TestStatusPlainCountsConfOwnersOnly(t *testing.T) {
	a := &App{ConfigDir: shortTempDir(t), Stdout: io.Discard, Stderr: io.Discard}
	saveRemote(t, a.ConfigDir, "mixed", "3000")
	saveRemote(t, a.ConfigDir, "sessonly", "3000")
	saveRemote(t, a.ConfigDir, "bare")
	serveOwnerDaemon(t, a.ConfigDir, "mixed", map[string]session.Response{
		session.OpList: {OK: true, State: session.StateUp, Forwards: []session.Forward{
			{Host: 3000, Guest: 3000, Owners: []string{session.OwnerConf}},
			{Host: 3001, Guest: 3001, Owners: []string{session.OwnerConnection}},
			{Host: 1455, Guest: 1455, Owners: []string{session.OwnerTTL}},
		}},
	}, false)
	sessionOnly := session.Response{OK: true, State: session.StateUp, Sessions: 1, Forwards: []session.Forward{
		{Host: 3001, Guest: 3001, Owners: []string{session.OwnerConnection}},
	}}
	serveOwnerDaemon(t, a.ConfigDir, "sessonly", map[string]session.Response{session.OpList: sessionOnly}, false)
	serveOwnerDaemon(t, a.ConfigDir, "bare", map[string]session.Response{session.OpList: sessionOnly}, false)
	var out bytes.Buffer
	a.Stdout = &out
	if err := a.runStatusPlain(true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bare\tremote-unmanaged\treachable\t-\n", "mixed\tremote-unmanaged\treachable\tup:1\n", "sessonly\tremote-unmanaged\treachable\tdown\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plain lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "up:0") || strings.Contains(out.String(), "reconnecting:0") {
		t.Errorf("a zero count was emitted:\n%s", out.String())
	}
}

// update cycles a daemon that holds no configured forward (a session's
// forwards never let it exit on its own) and does not respawn it: the
// session client does that on the new binary.
func TestRestartDaemonsCyclesSessionHeldDaemon(t *testing.T) {
	old := daemonGoneTimeout
	daemonGoneTimeout = 2 * time.Second
	t.Cleanup(func() { daemonGoneTimeout = old })
	var out bytes.Buffer
	a := &App{ConfigDir: shortTempDir(t), Stdout: &out, Stderr: io.Discard}
	saveRemote(t, a.ConfigDir, "held")
	od := serveOwnerDaemon(t, a.ConfigDir, "held", map[string]session.Response{
		session.OpList: {OK: true, State: session.StateUp, Version: "v0.0.1", Sessions: 1, Forwards: []session.Forward{
			{Host: 3000, Guest: 3000, Owners: []string{session.OwnerConnection}},
		}},
	}, true)
	res := a.restartDaemons()
	if len(od.requests(session.OpStop)) != 1 {
		t.Fatalf("session-held daemon not stopped")
	}
	if strings.Join(res.cycled, ",") != "held" || len(res.failed)+len(res.skipped) != 0 {
		t.Fatalf("res = %+v\n%s", res, out.String())
	}
	if !strings.Contains(out.String(), "held: stopped forward daemon") {
		t.Errorf("output:\n%s", out.String())
	}
}

// After `ports down` a session can keep the daemon up with the conf still
// listing ports. update must cycle it without respawning: a `ports up`
// would bring back forwards the user took down.
func TestRestartDaemonsDoesNotRespawnAfterPortsDown(t *testing.T) {
	old := daemonGoneTimeout
	daemonGoneTimeout = 2 * time.Second
	t.Cleanup(func() { daemonGoneTimeout = old })
	var out bytes.Buffer
	a := &App{ConfigDir: shortTempDir(t), Stdout: &out, Stderr: io.Discard}
	saveRemote(t, a.ConfigDir, "downed", "3000")
	od := serveOwnerDaemon(t, a.ConfigDir, "downed", map[string]session.Response{
		session.OpList: {OK: true, State: session.StateUp, Version: "v0.0.1", Sessions: 1, Forwards: []session.Forward{
			{Host: 3000, Guest: 3000, Owners: []string{session.OwnerConnection}}, // configured port, not conf-owned
		}},
	}, true)
	res := a.restartDaemons()
	if len(od.requests(session.OpStop)) != 1 {
		t.Fatal("daemon not stopped")
	}
	if strings.Join(res.cycled, ",") != "downed" || len(res.failed)+len(res.skipped) != 0 {
		t.Fatalf("res = %+v\n%s", res, out.String())
	}
	if strings.Contains(out.String(), "restarting forwards") || !strings.Contains(out.String(), "downed: stopped forward daemon") {
		t.Errorf("forwards the user took down were brought back:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(config.RuntimeDir(a.ConfigDir), "downed.sock")); !os.IsNotExist(err) {
		t.Errorf("a daemon was respawned: %v", err)
	}
}
