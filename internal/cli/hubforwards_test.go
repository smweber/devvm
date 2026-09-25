package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// hubPorts reads the hub conf's [machines.NAME] ports as saved.
func hubPorts(t *testing.T, a *App, hub, machine string) ([]string, bool) {
	t.Helper()
	h, err := config.Load(a.ConfigDir, hub)
	if err != nil {
		t.Fatal(err)
	}
	hm, ok := h.Machines[machine]
	return hm.Ports, ok
}

// `ports add/rm/list/up/down` on HUB/NAME run on this host (hub.md §7):
// the mapping lands in the hub conf's [machines.NAME] table, the forward is
// asked of this host's daemon for h@web, and nothing dials the hub (the
// daemon's __session does that, and here a fake daemon serves the socket).
func TestHubPortsRunLaptopSide(t *testing.T) {
	a := &App{ConfigDir: shortTempDir(t)}
	sshLog, _ := fakeHub(t, a) // [machines.web] ports = ["3000"]
	od := serveOwnerDaemon(t, a.ConfigDir, "h/web", map[string]session.Response{
		session.OpAdd: {OK: true, Host: 4001, Bumped: true},
		session.OpList: {OK: true, State: session.StateUp, Forwards: []session.Forward{
			{Host: 3000, Guest: 3000, Owners: []string{session.OwnerConf}},
			{Host: 4001, Guest: 4000, Owners: []string{session.OwnerConf}},
		}},
		session.OpDown: {OK: true, Stopped: true},
	}, false)

	if err := runTree(t, a, "ports", "add", "h/web", "4000"); err != nil {
		t.Fatalf("ports add: %v\n%s", err, a.Stderr)
	}
	if got, _ := hubPorts(t, a, "h", "web"); !slices.Equal(got, []string{"3000", "4000"}) {
		t.Errorf("table after add = %v", got)
	}
	if adds := od.requests(session.OpAdd); len(adds) != 1 || adds[0].Guest != 4000 || adds[0].Host != 4000 {
		t.Errorf("adds = %+v", adds)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "localhost:4001 -> h/web:4000 (preferred 4000 taken)") {
		t.Errorf("ports add said %q", out)
	}
	// A second add of the same mapping records nothing new.
	if err := runTree(t, a, "ports", "add", "h/web", "4000:4000"); err != nil {
		t.Fatal(err)
	}
	if got, _ := hubPorts(t, a, "h", "web"); !slices.Equal(got, []string{"3000", "4000"}) {
		t.Errorf("table after a repeat add = %v", got)
	}

	if err := runTree(t, a, "ports", "list", "h/web"); err != nil {
		t.Fatal(err)
	}
	out := a.Stdout.(*bytes.Buffer).String()
	for _, want := range []string{"configured:\n  3000\n  4000\n", "    guest 4000  -> localhost:4001  owner=conf\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("ports list lacks %q:\n%s", want, out)
		}
	}

	if err := runTree(t, a, "ports", "rm", "h/web", "4000"); err != nil {
		t.Fatal(err)
	}
	if got, _ := hubPorts(t, a, "h", "web"); !slices.Equal(got, []string{"3000"}) {
		t.Errorf("table after rm = %v", got)
	}
	if rms := od.requests(session.OpRemove); len(rms) != 1 || rms[0].Guest != 4000 || rms[0].Owner != session.OwnerConf {
		t.Errorf("removes = %+v", rms)
	}
	// The last mapping gone, the table goes too (delete HUB refuses while
	// one exists).
	if err := runTree(t, a, "ports", "rm", "h/web", "3000"); err != nil {
		t.Fatal(err)
	}
	if _, ok := hubPorts(t, a, "h", "web"); ok {
		t.Error("an empty [machines.web] table was left behind")
	}

	if err := runTree(t, a, "ports", "down", "h/web"); err != nil {
		t.Fatal(err)
	}
	if len(od.requests(session.OpDown)) != 1 {
		t.Error("ports down sent no down")
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		data, _ := os.ReadFile(sshLog)
		t.Errorf("a ports verb dialed the hub itself:\n%s", data)
	}
}

// `ports up HUB/NAME` asks this host's daemon for every mapping in the
// machine's table.
func TestHubPortsUp(t *testing.T) {
	a := &App{ConfigDir: shortTempDir(t)}
	writeHub(t, a, "h", map[string][]string{"web": {"3000", "8443:443"}})
	od := serveOwnerDaemon(t, a.ConfigDir, "h/web", map[string]session.Response{
		session.OpList: {OK: true, State: session.StateUp},
	}, false)
	if err := runTree(t, a, "ports", "up", "h/web"); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range od.requests(session.OpAdd) {
		got = append(got, fmt.Sprintf("%d:%d", r.Host, r.Guest))
	}
	if !slices.Equal(got, []string{"3000:3000", "8443:443"}) {
		t.Errorf("ports up adds = %v", got)
	}
}

// Two concurrent `ports add` for different machines on one hub both land:
// every machine on a hub shares the hub conf, Save is atomic but not
// serialized, and the flock around the read-modify-write is what keeps one
// edit from dropping the other.
func TestHubPortsConcurrentAddsBothLand(t *testing.T) {
	a := &App{ConfigDir: shortTempDir(t), Stdout: io.Discard, Stderr: io.Discard}
	writeHub(t, a, "h", nil)
	for _, m := range []string{"h/a", "h/b"} {
		serveOwnerDaemon(t, a.ConfigDir, m, map[string]session.Response{session.OpAdd: {OK: true, Host: 1}}, false)
	}
	const n = 15
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for _, m := range []string{"h/a", "h/b"} {
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				if err := a.runPort(m, fmt.Sprint(20000+i)); err != nil {
					errs <- fmt.Errorf("%s: %w", m, err)
				}
			}
		}(m)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, m := range []string{"a", "b"} {
		got, _ := hubPorts(t, a, "h", m)
		if len(got) != n {
			t.Errorf("[machines.%s] has %d ports, want %d (a lost update): %v", m, len(got), n, got)
		}
	}
	// The lock is a dotfile ending in .lock: never a machine, watch noise.
	names, _ := config.List(a.ConfigDir)
	if !slices.Equal(names, []string{"h"}) {
		t.Errorf("config.List = %v", names)
	}
	if !isWatchNoise(filepath.Join(config.MachinesDir(a.ConfigDir), ".h.toml.lock")) {
		t.Error("the hub conf lock would wake status --watch")
	}
}

// A live run/h@web.sock alone (no table, no cache) makes h/web a machine:
// a daemon for a hub machine can exist with no entry anywhere else.
func TestListMachinesSeesHubSocket(t *testing.T) {
	a := newTestApp(t)
	writeHub(t, a, "h", nil)
	if err := config.EnsureRuntimeDir(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.RuntimeDir(a.ConfigDir), "h@web.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := a.listMachines(); !slices.Equal(got, []string{"h", "h/web"}) {
		t.Errorf("listMachines = %v", got)
	}
}

// The laptop daemon's __session: ssh over this machine's own master
// (run/h@web.master, not the hub's), BatchMode with RequestTTY=no, the
// login-shell wrapper around `env DEVVM_NO_SUBSCRIBE=1 devvm __session
// web`, and that reaches the hub's devvm as exactly `__session web`.
func TestHubSessionSSHArgv(t *testing.T) {
	a := newTestApp(t)
	sshLog, argvLog := fakeHub(t, a)
	_, b, err := a.resolveAny("h/web")
	if err != nil {
		t.Fatal(err)
	}
	hs, ok := b.(backend.HubSessioner)
	if !ok {
		t.Fatalf("hub machine backend %T is no HubSessioner", b)
	}
	master := filepath.Join(config.RuntimeDir(a.ConfigDir), "h@web.master")
	if c := hs.SSHConn(); c.ControlPath != master || c.Host != "u@h.example" {
		t.Errorf("SSHConn = %+v", c)
	}
	argv := hs.SessionArgv()
	var out bytes.Buffer
	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(""), &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out.String())
	}
	want := "-o ConnectTimeout=10 -o ControlMaster=no -o ControlPath=" + master + " -o BatchMode=yes -o RequestTTY=no u@h.example " + wantRemote("__session", "web")
	if lines := sshLines(t, sshLog); len(lines) != 1 || lines[0] != want {
		t.Errorf("ssh argv =\n%s\nwant\n%s", strings.Join(lines, "\n"), want)
	}
	if got := devvmCalls(t, argvLog); len(got) != 1 || !slices.Equal(got[0], []string{"__session", "web"}) {
		t.Errorf("devvm on the hub got %q", got)
	}
}

// __session is the hub side only: a HUB/NAME is refused (hubs do not
// chain) before anything is dialed.
func TestSessionRefusesHubMachine(t *testing.T) {
	a := newTestApp(t)
	sshLog, _ := fakeHub(t, a)
	err := runTree(t, a, "__session", "h/web")
	if err == nil || !strings.Contains(err.Error(), "hubs do not chain") {
		t.Errorf("__session h/web: err = %v", err)
	}
	if _, err := os.Stat(sshLog); !errors.Is(err, os.ErrNotExist) {
		t.Error("__session h/web dialed the hub")
	}
}

// update cycles a hub machine's daemon (run/h@web.sock) like a local one:
// stopped, waited out, and its configured forwards (the [machines.web]
// table) brought back on the replacement.
func TestRestartDaemonsCyclesHubMachineDaemon(t *testing.T) {
	old := daemonGoneTimeout
	daemonGoneTimeout = 2 * time.Second
	t.Cleanup(func() { daemonGoneTimeout = old })
	var out bytes.Buffer
	a := &App{ConfigDir: shortTempDir(t), Stdout: &out, Stderr: &out}
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}})
	first := serveOwnerDaemon(t, a.ConfigDir, "h/web", map[string]session.Response{
		session.OpList: {OK: true, State: session.StateUp, Version: "v0.0.1", Forwards: []session.Forward{
			{Host: 3000, Guest: 3000, Owners: []string{session.OwnerConf}},
		}},
	}, true)
	// The replacement: what `__daemon h/web` would be, listening as soon as
	// the old socket is gone (the test binary itself runs no daemon).
	sock := filepath.Join(config.RuntimeDir(a.ConfigDir), "h@web.sock")
	next := make(chan *ownerDaemon, 1)
	go func() {
		for len(first.requests(session.OpStop)) == 0 {
			time.Sleep(time.Millisecond)
		}
		for {
			if _, err := os.Stat(sock); os.IsNotExist(err) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		next <- serveOwnerDaemon(t, a.ConfigDir, "h/web", map[string]session.Response{
			session.OpList: {OK: true, State: session.StateUp, Version: Version},
		}, false)
	}()
	res := a.restartDaemons()
	if strings.Join(res.cycled, ",") != "h/web" || len(res.failed)+len(res.skipped) != 0 {
		t.Fatalf("res = %+v\n%s", res, out.String())
	}
	if adds := (<-next).requests(session.OpAdd); len(adds) != 1 || adds[0].Guest != 3000 {
		t.Errorf("forwards on the replacement = %+v", adds)
	}
}

// dialForwards retries a hub machine's daemon that fails to come up (the
// hub's VM still `starting` right after `start h/web`) until it does, and
// gives up after wait.
func TestDialForwardsRetriesHubMachine(t *testing.T) {
	origInterval := runningPollInterval
	runningPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { runningPollInterval = origInterval })
	a := &App{ConfigDir: shortTempDir(t), Stdout: io.Discard, Stderr: io.Discard}
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}})
	m, err := config.LoadHubMachine(a.ConfigDir, "h/web")
	if err != nil {
		t.Fatal(err)
	}
	// Nothing serves the socket: every Dial spawns the test binary, which
	// refuses to be a daemon (TestMain), so each try fails fast.
	start := time.Now()
	if _, err := a.dialForwards(m, 300*time.Millisecond); err == nil {
		t.Fatal("dialForwards with no daemon succeeded")
	} else if time.Since(start) > 20*time.Second {
		t.Errorf("gave up after %s", time.Since(start))
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		serveOwnerDaemon(t, a.ConfigDir, "h/web", map[string]session.Response{}, false)
	}()
	if _, err := a.dialForwards(m, 10*time.Second); err != nil {
		t.Errorf("dialForwards did not retry until the daemon came up: %v", err)
	}
}
