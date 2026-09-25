package session

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/backend"
)

// Relay heartbeats (hub.md §7): a relay that declared an interval and then
// went silent is closed after heartbeatMisses intervals, with its forwards;
// one that keeps pinging, or whose pings the hub only answers late, stays;
// a local session never has a deadline.

// fastHeartbeats runs the heartbeat protocol in units of unit instead of
// seconds for one test.
func fastHeartbeats(t *testing.T, unit time.Duration) {
	t.Helper()
	old := heartbeatUnit
	heartbeatUnit = unit
	t.Cleanup(func() { heartbeatUnit = old })
}

// lineClient speaks the session line protocol by hand, as a far daemon
// behind `__session` does, so a test decides exactly when it is silent.
type lineClient struct {
	t  *testing.T
	c  lineConn
	br *bufio.Reader
}

func (l *lineClient) send(req Request) {
	l.t.Helper()
	b, _ := json.Marshal(req)
	if _, err := l.c.Write(append(b, '\n')); err != nil {
		l.t.Fatalf("send %s: %v", req.Op, err)
	}
}

func (l *lineClient) recv(within time.Duration) (Response, error) {
	_ = l.c.SetReadDeadline(time.Now().Add(within))
	line, err := l.br.ReadString('\n')
	if err != nil {
		return Response{}, err
	}
	var resp Response
	err = json.Unmarshal([]byte(line), &resp)
	return resp, err
}

func (l *lineClient) open(req Request) Response {
	l.t.Helper()
	req.ID, req.Op = 1, OpSession
	l.send(req)
	resp, err := l.recv(5 * time.Second)
	if err != nil || !resp.OK {
		l.t.Fatalf("session open = %+v, %v", resp, err)
	}
	return resp
}

func dialLine(t *testing.T, dir, name string) *lineClient {
	t.Helper()
	c, err := net.Dial("unix", socketPath(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &lineClient{t: t, c: c, br: bufio.NewReader(c)}
}

// A relay that declared a heartbeat and then sent nothing is closed once
// the deadline passes: the `__session` relaying it ends (so sshd would tear
// down), and the hub drops the session and every forward it owned.
func TestRelayHeartbeatSilenceClosesRelay(t *testing.T) {
	fastHeartbeats(t, 50*time.Millisecond)
	_, guest := echoServer(t)
	h := newHubRig(t)
	r, err := startRelay(t, h.dir, "")
	if err != nil {
		t.Fatal(err)
	}
	l := &lineClient{t: t, c: r.laptop, br: bufio.NewReader(r.laptop)}
	if err := SkipToMarker(l.br, SessionMarker, MarkerLimit); err != nil {
		t.Fatal(err)
	}
	opened := time.Now()
	if resp := l.open(Request{Relay: true, Heartbeat: 2}); !resp.Relay || resp.Heartbeat != 2 {
		t.Fatalf("open reply = %+v, want relay and heartbeat 2 echoed", resp)
	}
	l.send(Request{ID: 2, Op: OpAdd, Host: freePort(t), Guest: guest})
	if resp, err := l.recv(5 * time.Second); err != nil || !resp.OK {
		t.Fatalf("add = %+v, %v", resp, err)
	}
	if _, ok := h.forward(guest); !ok || h.d.sessionCount() != 1 {
		t.Fatalf("hub before the silence: forward %v, sessions %d", ok, h.d.sessionCount())
	}
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the __session outlived its relay's heartbeat deadline")
	}
	// Three intervals of 100ms: not before.
	if took := time.Since(opened); took < 300*time.Millisecond {
		t.Errorf("relay closed after %s, before its deadline", took)
	}
	waitFor(t, "the hub to drop the silent relay and its forward", func() bool {
		_, ok := h.forward(guest)
		return !ok && h.d.sessionCount() == 0
	})
}

// A laptop hub transport pings at its declared interval, so its relay
// outlives many deadlines and its forward keeps carrying traffic.
func TestRelayHeartbeatPingsKeepRelay(t *testing.T) {
	// Wide margins for a loaded CI runner under -race: pings every 250ms,
	// the laptop gives each 500ms, the hub closes after 750ms of silence.
	fastHeartbeats(t, 250*time.Millisecond)
	oldBeat := hubHeartbeat
	hubHeartbeat = 1
	t.Cleanup(func() { hubHeartbeat = oldBeat })
	_, guest := echoServer(t)
	h := newHubRig(t)
	l := newLaptopRig(t, h, false)
	host, _, _, err := l.d.add(freePort(t), guest, false, confOwner)
	if err != nil {
		t.Fatal(err)
	}
	ht := l.hubTransport()
	time.Sleep(2 * time.Second) // well past the hub's deadline
	select {
	case <-ht.dead():
		t.Fatal("the hub transport died while pinging")
	default:
	}
	if _, ok := h.forward(guest); !ok || h.d.sessionCount() != 1 {
		t.Fatalf("hub after the pings: forward %v, sessions %d", ok, h.d.sessionCount())
	}
	echoOK(t, host)
}

// The deadline is on reads: pings the hub reads on time but answers late,
// its one worker held by a slow bind, do not trip it.
func TestRelayHeartbeatLateRepliesDoNotTrip(t *testing.T) {
	fastHeartbeats(t, 100*time.Millisecond)
	dir := shortTempDir(t)
	d := startSockDaemon(t, dir)
	g := newGateTransport()
	d.mu.Lock()
	d.tr = g
	d.mu.Unlock()
	l := dialLine(t, dir, "t")
	l.open(Request{Relay: true, Heartbeat: 2}) // 600ms of silence closes it
	_, guest := echoServer(t)
	l.send(Request{ID: 2, Op: OpAdd, Host: freePort(t), Guest: guest})
	<-g.entered              // the worker is in the bind, and stays there
	for i := 0; i < 9; i++ { // 1.35s: past two deadlines, a ping every quarter of one
		time.Sleep(150 * time.Millisecond)
		l.send(Request{ID: int64(10 + i), Op: OpPing})
	}
	if d.sessionCount() != 1 {
		t.Fatal("the relay was closed while it was pinging")
	}
	close(g.gate)
	resp, err := l.recv(5 * time.Second)
	if err != nil || resp.ID != 2 || !resp.OK {
		t.Fatalf("the add's reply = %+v, %v", resp, err)
	}
	for i := 0; i < 9; i++ {
		if resp, err := l.recv(5 * time.Second); err != nil || resp.ID != int64(10+i) {
			t.Fatalf("ping %d reply = %+v, %v", i, resp, err)
		}
	}
}

// Only a relay gets a deadline: a local session idles for good even when
// it names a heartbeat (nothing is echoed), and a relay that declares none
// idles too (a hub daemon still sees laptops older than heartbeats).
func TestHeartbeatOnlyOnDeclaringRelay(t *testing.T) {
	fastHeartbeats(t, 20*time.Millisecond)
	dir := shortTempDir(t)
	d := startSockDaemon(t, dir)
	local := dialLine(t, dir, "t")
	if resp := local.open(Request{Heartbeat: 1}); resp.Heartbeat != 0 {
		t.Errorf("a local session was given heartbeat %d", resp.Heartbeat)
	}
	quiet := dialLine(t, dir, "t")
	if resp := quiet.open(Request{Relay: true}); resp.Heartbeat != 0 {
		t.Errorf("a relay without a heartbeat was given %d", resp.Heartbeat)
	}
	time.Sleep(500 * time.Millisecond) // eight deadlines of a 1-unit heartbeat
	for _, l := range []*lineClient{local, quiet} {
		l.send(Request{ID: 2, Op: OpPing})
		if resp, err := l.recv(5 * time.Second); err != nil || !resp.OK {
			t.Fatalf("ping after idling = %+v, %v", resp, err)
		}
	}
	if d.sessionCount() != 2 {
		t.Errorf("sessions = %d, want both still open", d.sessionCount())
	}
}

// The laptop declares its heartbeat at open, and a ping the hub never
// answers marks the transport dead: the reconnect every other death takes.
func TestHubHeartbeatUnansweredMarksDead(t *testing.T) {
	fastHeartbeats(t, 20*time.Millisecond)
	oldBeat := hubHeartbeat
	hubHeartbeat = 2
	t.Cleanup(func() { hubHeartbeat = oldBeat })
	declared := make(chan int, 1)
	c, stop := scriptedHubOpen(t, func(req Request) Response {
		declared <- req.Heartbeat
		return Response{OK: true, State: StateUp, Relay: true, Heartbeat: req.Heartbeat}
	}, func(Request) Response { return Response{} }) // never answers
	ht, err := openHubTransport("h/web", c, newFakeLocal(), stop, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ht.Close()
	if got := <-declared; got != 2 {
		t.Errorf("open declared heartbeat %d, want 2", got)
	}
	select {
	case <-ht.dead():
	case <-time.After(5 * time.Second):
		t.Fatal("an unanswered heartbeat did not mark the hub transport dead")
	}
}

// `ssh -O exit` stderr ("Exit request sent.", or "Control socket connect"
// once the master is gone) stays out of the daemon log; a cancel's, which
// is the diagnosis when one fails, still reaches it.
func TestSSHControlStderr(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"ssh-said $2\" >&2\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	logf, err := os.Create(filepath.Join(bin, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = logf
	tr := &sshTransport{conn: backend.SSHConn{Host: "dev@example", ControlPath: filepath.Join(bin, "cm")}, deadCh: make(chan struct{}), stopCh: make(chan struct{})}
	_ = (&sshForwardCloser{t: tr, spec: "localhost:1:localhost:2"}).Close()
	_ = tr.Close()
	os.Stderr = old
	logf.Close()
	b, _ := os.ReadFile(filepath.Join(bin, "stderr"))
	if got := string(b); !strings.Contains(got, "ssh-said cancel") || strings.Contains(got, "ssh-said exit") {
		t.Errorf("daemon log = %q, want the cancel's stderr and not the exit's", got)
	}
}
