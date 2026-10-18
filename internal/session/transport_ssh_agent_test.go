package session

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/agentbin"
	"github.com/smweber/devvm/internal/agentrpc"
	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// fastAgentKnobs runs the agent supervisor's waits in milliseconds.
func fastAgentKnobs(t *testing.T) {
	t.Helper()
	oldF, oldMin, oldMax, oldS := agentFirstWait, agentMinBackoff, agentMaxBackoff, agentSetupMaxBackoff
	agentFirstWait, agentMinBackoff, agentMaxBackoff, agentSetupMaxBackoff = 2*time.Second, 10*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() {
		agentFirstWait, agentMinBackoff, agentMaxBackoff, agentSetupMaxBackoff = oldF, oldMin, oldMax, oldS
	})
}

// fakeAgents stands in for `devvm-agent serve` execs: each spawn is a real
// loopback socket pair (yamux deadlocks over net.Pipe) with agentrpc.Serve
// as the guest, whose GuestSession arrives on guests. kill ends the newest
// exec as its process dying would.
type fakeAgents struct {
	t      *testing.T
	mu     sync.Mutex
	spawns []string // agent paths spawned
	conns  []net.Conn
	guests chan *agentrpc.GuestSession
}

func newFakeAgents(t *testing.T) *fakeAgents {
	return &fakeAgents{t: t, guests: make(chan *agentrpc.GuestSession, 8)}
}

func (f *fakeAgents) spawn(_ context.Context, agentPath string) (*backend.Session, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	gc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return nil, err
	}
	hc := <-acc
	go func() {
		_ = agentrpc.Serve(gc, gc, agentrpc.GuestHandlers{}, func(g *agentrpc.GuestSession) { f.guests <- g })
	}()
	f.mu.Lock()
	f.spawns = append(f.spawns, agentPath)
	f.conns = append(f.conns, gc, hc)
	f.mu.Unlock()
	f.t.Cleanup(func() { gc.Close(); hc.Close() })
	return &backend.Session{Stdin: hc, Stdout: hc}, nil
}

func (f *fakeAgents) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.spawns)
}

// kill ends the newest exec: its guest side goes away.
func (f *fakeAgents) kill() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns[len(f.conns)-2].Close()
}

func (f *fakeAgents) guest(t *testing.T) *agentrpc.GuestSession {
	t.Helper()
	select {
	case g := <-f.guests:
		return g
	case <-time.After(5 * time.Second):
		t.Fatal("no agent exec came up")
		return nil
	}
}

// openURLRoundTrip has the guest open a URL and the test answer it from
// the transport's events channel, as the daemon's bridge would.
func openURLRoundTrip(t *testing.T, tr transport, g *agentrpc.GuestSession) {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		reply, err := g.OpenURL("https://example.com/login", 5*time.Second)
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		got <- strings.TrimSpace(string(reply))
	}()
	select {
	case req := <-tr.events():
		if req.url != "https://example.com/login" {
			t.Errorf("event url = %q", req.url)
		}
		req.reply(agentrpc.OpenReply{Opened: true, URL: req.url})
	case <-time.After(5 * time.Second):
		t.Fatal("the open-url never reached events()")
	}
	if r := <-got; !strings.Contains(r, `"opened":true`) {
		t.Fatalf("guest got %q", r)
	}
}

// A remote box's agent exec carries the guest's open-url to events(), and
// when it dies with the master alive the transport respawns it itself
// (after its own short backoff): the transport is not dead, the channel is
// the same, the shim stays reported, and events flow again. When the
// master is gone too, the transport is marked dead instead, for the
// daemon's reconnect, and nothing is respawned.
func TestSSHAgentRespawnsWithMasterAlive(t *testing.T) {
	fastAgentKnobs(t)
	dir, tr := fakeMasterSSH(t)
	if err := os.WriteFile(filepath.Join(dir, "alive"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fa := newFakeAgents(t)
	paths := agentbin.Paths{Agent: "/usr/local/bin/devvm-agent", Shim: testShim}
	tr.startAgent(context.Background(), "box", agentHooks{
		prepare: func(context.Context) (agentbin.Paths, agentbin.State, error) { return paths, agentbin.Current, nil },
		spawn:   fa.spawn,
	})
	defer tr.Close()
	ev := tr.events()
	if ev == nil || tr.shimPath() != testShim || !tr.hasEvents() {
		t.Fatalf("after the first attempt: events %v, shim %q, hasEvents %v", ev, tr.shimPath(), tr.hasEvents())
	}
	openURLRoundTrip(t, tr, fa.guest(t))

	fa.kill()
	g := fa.guest(t) // the respawn
	if n := fa.count(); n != 2 {
		t.Fatalf("%d spawns, want a respawn", n)
	}
	select {
	case <-tr.dead():
		t.Fatal("an agent exec dying marked the transport dead")
	default:
	}
	if tr.events() != ev || tr.shimPath() != testShim {
		t.Fatal("the respawn changed the events channel or lost the shim")
	}
	openURLRoundTrip(t, tr, g)

	// The master goes, and the exec with it.
	if err := os.Remove(filepath.Join(dir, "alive")); err != nil {
		t.Fatal(err)
	}
	fa.kill()
	select {
	case <-tr.dead():
	case <-time.After(5 * time.Second):
		t.Fatal("the master's death did not mark the transport dead")
	}
	time.Sleep(50 * time.Millisecond)
	if n := fa.count(); n != 2 {
		t.Fatalf("%d spawns, want none after the master died", n)
	}
}

// On an adopt host without a current agent the transport has an events
// channel nothing feeds, reports no shim and no events (so a relay is told
// NoEvents), and spawns nothing; a nudge (the kick `auth` sends after
// installing the agent) makes it look again and bring the exec up.
func TestSSHAgentAdoptHostWaitsForNudge(t *testing.T) {
	fastAgentKnobs(t)
	dir, tr := fakeMasterSSH(t)
	if err := os.WriteFile(filepath.Join(dir, "alive"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fa := newFakeAgents(t)
	var mu sync.Mutex
	st, probes := agentbin.Missing, 0
	paths := agentbin.Paths{Agent: "/home/me/.local/bin/devvm-agent", Shim: "/home/me/.local/bin/devvm-open-url"}
	tr.startAgent(context.Background(), "box", agentHooks{
		prepare: func(context.Context) (agentbin.Paths, agentbin.State, error) {
			mu.Lock()
			defer mu.Unlock()
			probes++
			return paths, st, nil
		},
		spawn: fa.spawn,
	})
	defer tr.Close()
	if tr.events() == nil || tr.shimPath() != "" || tr.hasEvents() {
		t.Fatalf("no agent: events %v, shim %q, hasEvents %v", tr.events(), tr.shimPath(), tr.hasEvents())
	}
	time.Sleep(60 * time.Millisecond) // several backoffs' worth: it must not poll
	mu.Lock()
	if probes != 1 {
		t.Fatalf("%d probes without a nudge, want 1", probes)
	}
	st = agentbin.Current
	mu.Unlock()
	if fa.count() != 0 {
		t.Fatal("spawned an agent that is not there")
	}
	tr.nudge()
	openURLRoundTrip(t, tr, fa.guest(t))
	if tr.shimPath() != paths.Shim || !tr.hasEvents() || fa.spawns[0] != paths.Agent {
		t.Fatalf("after the nudge: shim %q, hasEvents %v, spawned %v", tr.shimPath(), tr.hasEvents(), fa.spawns)
	}
}

// guestRec is a Backend whose guest has nothing installed: it answers
// agentbin's read-only probes and records every command and copy.
type guestRec struct {
	backend.Backend
	mu    sync.Mutex
	calls []string
}

func (g *guestRec) Run(_ context.Context, o backend.ExecOpts, argv ...string) error {
	out := o.Stdout
	if out == nil {
		out = io.Discard
	}
	joined := strings.Join(argv, " ")
	g.mu.Lock()
	g.calls = append(g.calls, "run as "+o.User+": "+joined)
	g.mu.Unlock()
	switch {
	case joined == "uname -s -m":
		fmt.Fprintln(out, "Linux x86_64")
	case joined == "sh -c echo $HOME":
		fmt.Fprintln(out, "/home/me")
	case strings.Contains(joined, "sha256sum"):
		fmt.Fprint(out, "-\n-\n")
	}
	return nil
}

func (g *guestRec) Copy(_, dst string) error {
	g.mu.Lock()
	g.calls = append(g.calls, "copy "+dst)
	g.mu.Unlock()
	return nil
}

func (g *guestRec) writes() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var w []string
	for _, c := range g.calls {
		if !strings.Contains(c, "uname") && !strings.Contains(c, "echo $HOME") && !strings.Contains(c, "sha256sum") {
			w = append(w, c)
		}
	}
	return w
}

// The production hooks follow ownership: a managed remote box gets the
// root /usr/local/bin install, as smol does; an adopt host is only probed,
// and with no agent there nothing at all is written.
func TestSSHAgentHooksInstallOnlyWhenManaged(t *testing.T) {
	g := &guestRec{}
	_, st, err := sshAgentHooks(config.NewRemote("adopted", false, "me@adopted"), g).prepare(context.Background())
	if err != nil || st != agentbin.Missing {
		t.Fatalf("adopt host: %v, %v", st, err)
	}
	if w := g.writes(); len(w) != 0 {
		t.Fatalf("adopt host: the daemon wrote %q", w)
	}

	g = &guestRec{}
	p, st, err := sshAgentHooks(config.NewRemote("cloud", true, "dev@cloud"), g).prepare(context.Background())
	if err != nil || st != agentbin.Current || p.Shim != backend.ManagedShimPath {
		t.Fatalf("managed: %+v, %v, %v", p, st, err)
	}
	w := strings.Join(g.writes(), "\n")
	if !strings.Contains(w, "copy /home/me/.devvm-agent.new") || !strings.Contains(w, "run as root: sh -c install -m 0755") ||
		!strings.Contains(w, "devvm-open-url") {
		t.Fatalf("managed: writes\n%s\nwant the root install of agent and shim", w)
	}
}

// A hub daemon whose machine is a remote box echoes Bridge (with the shim)
// on a relay open while its ssh transport has a current agent, and
// NoEvents while it has none (hub.md §8): the echo follows the transport's
// events, not the backend kind.
func TestRelayBridgeEchoForRemoteMachine(t *testing.T) {
	_, tr := fakeMasterSSH(t)
	tr.agent = &sshAgent{evCh: make(chan bridgeRequest), nudgeCh: make(chan struct{}, 1), cancel: func() {}}
	dir := shortTempPaths(t)
	d := startSockDaemon(t, dir)
	d.mu.Lock()
	d.tr = tr
	d.mu.Unlock()

	tr.agent.setPaths(agentbin.Paths{Agent: "/usr/local/bin/devvm-agent", Shim: testShim})
	if open := dialLine(t, dir, "t").open(Request{Relay: true}); !open.Bridge || open.NoEvents || open.Shim != testShim {
		t.Fatalf("relay open with an agent = %+v, want Bridge and the shim", open)
	}
	tr.agent.setPaths(agentbin.Paths{})
	if open := dialLine(t, dir, "t").open(Request{Relay: true}); open.Bridge || !open.NoEvents {
		t.Fatalf("relay open without an agent = %+v, want NoEvents", open)
	}
}
