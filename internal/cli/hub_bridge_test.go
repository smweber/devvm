package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/auth"
	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// The laptop half of the bridge for hub machines (hub.md §8, roadmap step
// 8): attach/shell/auth HUB/NAME subscribe on this host's daemon for
// HUB@NAME before the leaf is proxied with DEVVM_NO_SUBSCRIBE=1 (the fake
// hub devvm refuses to run without it).

// orderedBridge is a session stand-in that records its calls into log,
// with whether the hub had been dialed yet at each.
type orderedBridge struct {
	sshLog string
	log    *[]string
	subErr error
	// ping is what Ping answers; the zero value is an up daemon reporting
	// the shim.
	ping *session.PingInfo
}

func (b *orderedBridge) note(what string) {
	if _, err := os.Stat(b.sshLog); err == nil {
		what += " (after ssh)"
	}
	*b.log = append(*b.log, what)
}

func (b *orderedBridge) Subscribe() error { b.note("subscribe"); return b.subErr }
func (b *orderedBridge) Close() error     { b.note("close"); return nil }
func (b *orderedBridge) Ping() (session.PingInfo, error) {
	b.note("ping")
	if b.ping != nil {
		return *b.ping, nil
	}
	return session.PingInfo{State: session.StateUp, Version: "test", Shim: "/usr/local/bin/devvm-open-url"}, nil
}
func (b *orderedBridge) Add(pref, guest int, exact bool) (int, bool, bool, error) {
	b.note(fmt.Sprintf("add %d exact=%v", guest, exact))
	return pref, false, false, nil
}

// stubHubBridges replaces both session openers for one test.
func stubHubBridges(t *testing.T, br *orderedBridge) *[]string {
	t.Helper()
	var dials []string
	oldB, oldA := openBridgeSession, openAuthBridge
	openBridgeSession = func(dir, name string, opts session.SessionOptions) (bridgeSub, error) {
		if opts.OnEvent == nil {
			t.Error("bridge session opened without an event handler")
		}
		dials = append(dials, name)
		return br, nil
	}
	openAuthBridge = func(dir, name string, opts session.SessionOptions) (auth.Bridge, error) {
		if opts.OnEvent == nil {
			t.Error("auth bridge opened without an event handler")
		}
		dials = append(dials, name)
		return br, nil
	}
	t.Cleanup(func() { openBridgeSession, openAuthBridge = oldB, oldA })
	return &dials
}

// attach/shell HUB/NAME dial this host's daemon for the hub machine,
// subscribe (acked) before anything runs on the hub, proxy the leaf with
// DEVVM_NO_SUBSCRIBE=1, and release the subscription after.
func TestHubAttachSubscribesBeforeProxy(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	a := newTestApp(t)
	sshLog, argvLog := fakeHub(t, a)
	pinTTY(t, true)
	var log []string
	dials := stubHubBridges(t, &orderedBridge{sshLog: sshLog, log: &log})
	for _, leaf := range []string{"attach", "shell"} {
		log = nil
		os.Remove(sshLog)
		if err := runTree(t, a, leaf, "h/web"); err != nil {
			t.Fatalf("%s: %v\n%s", leaf, err, a.Stderr)
		}
		if got := strings.Join(log, ", "); got != "subscribe, close (after ssh)" {
			t.Errorf("%s: bridge calls = %q, want subscribe before the hub is dialed, close after", leaf, got)
		}
		if lines := sshLines(t, sshLog); len(lines) != 1 || lines[0] != wantSSH(a, true, leaf, "web") {
			t.Errorf("%s ssh argv = %q", leaf, lines)
		}
	}
	if !slices.Equal(*dials, []string{"h/web", "h/web"}) {
		t.Errorf("dialed %q, want this host's daemon for h/web each time", *dials)
	}
	if got := devvmCalls(t, argvLog); len(got) != 2 {
		t.Errorf("hub devvm calls = %q", got)
	}

	// DEVVM_NO_SUBSCRIBE on this host too: no daemon at all.
	t.Setenv(backend.NoSubscribeEnv, "1")
	*dials = nil
	if err := runTree(t, a, "attach", "h/web"); err != nil {
		t.Fatal(err)
	}
	if len(*dials) != 0 {
		t.Errorf("dialed %q under DEVVM_NO_SUBSCRIBE", *dials)
	}
}

// A hub too old for browser opens costs attach one line, naming it; the
// session still runs (its forwards are unaffected).
func TestHubAttachOldHubIsANotice(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	a := newTestApp(t)
	sshLog, _ := fakeHub(t, a)
	pinTTY(t, true)
	var log []string
	stubHubBridges(t, &orderedBridge{sshLog: sshLog, log: &log,
		subErr: errors.New("subscribed here, but not upstream: hub devvm too old for browser opens: the daemon for web on hub h predates v0.1.15")})
	if err := runTree(t, a, "attach", "h/web"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	msg := a.Stderr.(interface{ String() string }).String()
	if !strings.Contains(msg, "browser bridge unavailable (subscribed here, but not upstream: hub devvm too old for browser opens") ||
		!strings.Contains(msg, "forwards still work") {
		t.Errorf("notice = %q", msg)
	}
	if lines := sshLines(t, sshLog); len(lines) != 1 {
		t.Errorf("the attach did not run: %q", lines)
	}
}

// auth HUB/NAME subscribes here, binds the tools' fixed callback ports on
// that session (codex's 1455, exact), then proxies `auth NAME TOOL` to the
// hub with DEVVM_NO_SUBSCRIBE=1, whose thin auth runs the logins. No shim
// is read here (the hub's auth reads its own daemon's) and nothing is
// installed.
func TestHubAuthSubscribesThenProxies(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	a := newTestApp(t)
	sshLog, argvLog := fakeHub(t, a)
	pinTTY(t, true)
	var log []string
	dials := stubHubBridges(t, &orderedBridge{sshLog: sshLog, log: &log})
	if err := runTree(t, a, "auth", "h/web", "codex"); err != nil {
		t.Fatalf("auth: %v\n%s", err, a.Stderr)
	}
	if got := strings.Join(log, ", "); got != "subscribe, ping, add 1455 exact=true, close (after ssh)" {
		t.Errorf("bridge calls = %q", got)
	}
	if !slices.Equal(*dials, []string{"h/web"}) {
		t.Errorf("dialed %q", *dials)
	}
	if lines := sshLines(t, sshLog); len(lines) != 1 || lines[0] != wantSSH(a, true, "auth", "web", "codex") {
		t.Errorf("auth ssh argv = %q\nwant %q", lines, wantSSH(a, true, "auth", "web", "codex"))
	}
	if got := devvmCalls(t, argvLog); len(got) != 1 || !slices.Equal(got[0], []string{"auth", "web", "codex"}) {
		t.Errorf("hub devvm calls = %q", got)
	}

	// github needs no fixed port: nothing bound here.
	log = nil
	os.Remove(sshLog)
	if err := runTree(t, a, "auth", "h/web", "github"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log, ", "); got != "subscribe, ping, close (after ssh)" {
		t.Errorf("github: bridge calls = %q", got)
	}
}

// A subscription that fails (the laptop daemon cannot come up, or the hub
// is too old for browser opens) refuses auth HUB/NAME before anything runs
// on the hub: an old hub's auth would spawn a second agent exec there.
func TestHubAuthRefusedWithoutBridge(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	a := newTestApp(t)
	sshLog, _ := fakeHub(t, a)
	pinTTY(t, true)
	var log []string
	stubHubBridges(t, &orderedBridge{sshLog: sshLog, log: &log,
		subErr: errors.New("subscribed here, but not upstream: hub devvm too old for browser opens")})
	err := runTree(t, a, "auth", "h/web")
	if err == nil || !strings.HasPrefix(err.Error(), "auth h/web: hub devvm too old for browser opens") {
		t.Fatalf("err = %v, want the refusal without the daemon's 'subscribed here' prefix", err)
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Error("auth ran on a hub too old for browser opens")
	}

	old := openAuthBridge
	openAuthBridge = func(string, string, session.SessionOptions) (auth.Bridge, error) {
		return nil, errors.New("forward daemon for 'h/web' exited")
	}
	t.Cleanup(func() { openAuthBridge = old })
	if err := runTree(t, a, "auth", "h/web"); err == nil || !strings.Contains(err.Error(), "browser bridge: forward daemon for 'h/web' exited") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Error("auth ran on the hub without a bridge")
	}
}

// bridged: the machines attach/shell/auth subscribe for.
func TestBridgedMachines(t *testing.T) {
	hub := config.NewHub("h", "u@h.example")
	for _, tc := range []struct {
		m    *config.Machine
		want bool
	}{
		{&config.Machine{Name: "web", Backend: config.BackendSmol}, true},
		{&config.Machine{Name: "h/web", Backend: config.BackendHub, Hub: hub}, true},
		{&config.Machine{Name: "box", Backend: config.BackendRemoteManaged}, false},
		{&config.Machine{Name: "box", Backend: config.BackendRemoteUnmanaged}, false},
		{hub, false},
	} {
		if got := bridged(tc.m); got != tc.want {
			t.Errorf("bridged(%s %s) = %v", tc.m.Name, tc.m.Backend, got)
		}
	}
}

// auth HUB/NAME runs nothing on the hub unless this host can prove the
// hub's daemon is subscribed for it: after the ack a ping must report the
// shim (none: this daemon lost its link, or the hub's machine has no
// browser events), and a host that itself runs under DEVVM_NO_SUBSCRIBE
// subscribes nothing, so it is refused before any dial.
func TestHubAuthRefusedWithoutProof(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	a := newTestApp(t)
	sshLog, _ := fakeHub(t, a)
	pinTTY(t, true)
	for _, tc := range []struct {
		ping session.PingInfo
		want string
	}{
		{session.PingInfo{State: session.StateReconnecting}, "auth h/web: the link to hub h is reconnecting; retry in a moment"},
		{session.PingInfo{State: session.StateUp}, "auth h/web: the hub's daemon for web reports no browser shim (is it a smol machine?), so logins there would not open here"},
	} {
		var log []string
		ping := tc.ping
		stubHubBridges(t, &orderedBridge{sshLog: sshLog, log: &log, ping: &ping})
		err := runTree(t, a, "auth", "h/web", "codex")
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.ping.State, err, tc.want)
		}
		if got := strings.Join(log, ", "); got != "subscribe, ping, close" {
			t.Errorf("%s: bridge calls = %q (nothing bound, released before any ssh)", tc.ping.State, got)
		}
		if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
			t.Errorf("%s: auth ran on the hub without a shim reported", tc.ping.State)
		}
	}

	// The hub's cached listing says web is not smol: the refusal says so.
	if err := writeHubCache(a.ConfigDir, "h", []hubRow{{name: "web", backend: config.BackendRemoteManaged, state: "reachable", forwards: "-"}}); err != nil {
		t.Fatal(err)
	}
	noShim := session.PingInfo{State: session.StateUp}
	stubHubBridges(t, &orderedBridge{sshLog: sshLog, log: new([]string), ping: &noShim})
	if err := runTree(t, a, "auth", "h/web"); err == nil || err.Error() != "auth h/web: auth on a non-smol hub machine is not supported through the hub yet (h/web is remote-managed there)" {
		t.Errorf("non-smol hub machine: err = %v", err)
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Error("auth ran on the hub for a non-smol machine")
	}

	t.Setenv(backend.NoSubscribeEnv, "1")
	var log []string
	dials := stubHubBridges(t, &orderedBridge{sshLog: sshLog, log: &log})
	err := runTree(t, a, "auth", "h/web")
	if err == nil || !strings.Contains(err.Error(), backend.NoSubscribeEnv+" is set here") {
		t.Fatalf("under %s: err = %v", backend.NoSubscribeEnv, err)
	}
	if len(*dials) != 0 {
		t.Errorf("dialed %q under %s", *dials, backend.NoSubscribeEnv)
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Error("auth ran on the hub with nothing subscribed here")
	}
}

// A proxied attach waits at most hubBridgeWait for its bridge: past it the
// attach runs at once with a one-line notice, the open is neither
// cancelled nor abandoned, and a session that opens after the attach has
// already ended (release ran) is closed the moment it arrives.
func TestHubAttachBridgeSetupBounded(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	a := newTestApp(t)
	sshLog, _ := fakeHub(t, a)
	pinTTY(t, true)
	gate := slowHubBridge(t)
	late := newSignalBridge()
	if err := runTree(t, a, "attach", "h/web"); err != nil {
		t.Fatalf("attach: %v\n%s", err, a.Stderr)
	}
	if lines := sshLines(t, sshLog); len(lines) != 1 {
		t.Errorf("the attach did not run: %q", lines)
	}
	msg := a.Stderr.(interface{ String() string }).String()
	if !strings.Contains(msg, "browser bridge to hub h not up yet; URLs opened before it is will print in the guest") {
		t.Errorf("notice = %q", msg)
	}
	gate <- late // the open finishes only now, after the attach returned
	select {
	case <-late.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("a session that opened after release was not closed")
	}
}

// A bridge that comes up after the bound is adopted for the rest of the
// attach (a cold start is when a login is most likely), and release closes
// it.
func TestHubAttachAdoptsLateBridge(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	a, errBuf := bridgeApp(t)
	gate := slowHubBridge(t)
	late := newSignalBridge()
	m := &config.Machine{Name: "h/web", Backend: config.BackendHub, Hub: config.NewHub("h", "u@h.example")}
	release := a.bridgeSession(m, nil)
	if !strings.Contains(errBuf.String(), "not up yet") {
		t.Fatalf("notice = %q", errBuf.String())
	}
	gate <- late
	select {
	case <-late.subscribed:
	case <-time.After(5 * time.Second):
		t.Fatal("the late session was never subscribed")
	}
	// Only a broken holder closes it here; a correct one waits for release.
	select {
	case <-late.closed:
		t.Fatal("the late session was closed before release")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-late.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("release did not close the adopted session")
	}
}

// lateBridge in isolation: adopted then released closes once; arriving
// after release closes at once; releasing empty is harmless.
func TestLateBridgeHolder(t *testing.T) {
	l, s := newLateBridge(), newSignalBridge()
	l.adopt(s)
	select {
	case <-s.closed:
		t.Fatal("adopt closed the session")
	default:
	}
	l.release()
	<-s.closed
	l, s = newLateBridge(), newSignalBridge()
	l.release()
	l.adopt(s)
	<-s.closed
}

// slowHubBridge makes hub bridge opens wait for the test to hand them their
// session, with hubBridgeWait cut to 20ms.
func slowHubBridge(t *testing.T) chan<- bridgeSub {
	t.Helper()
	old, oldWait := openBridgeSession, hubBridgeWait
	hubBridgeWait = 20 * time.Millisecond
	t.Cleanup(func() { openBridgeSession, hubBridgeWait = old, oldWait })
	gate := make(chan bridgeSub)
	openBridgeSession = func(dir, name string, opts session.SessionOptions) (bridgeSub, error) {
		select {
		case s := <-gate:
			return s, nil
		case <-time.After(10 * time.Second):
			return nil, errors.New("the test never released the open")
		}
	}
	return gate
}

// signalBridge is a bridge session whose Subscribe and Close are
// observable across goroutines.
type signalBridge struct {
	subOnce, closeOnce sync.Once
	subscribed, closed chan struct{}
}

func newSignalBridge() *signalBridge {
	return &signalBridge{subscribed: make(chan struct{}), closed: make(chan struct{})}
}

func (b *signalBridge) Subscribe() error {
	b.subOnce.Do(func() { close(b.subscribed) })
	return nil
}
func (b *signalBridge) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

// controlRetries makes the background bridge retries wait on tick (one
// send per retry) instead of a timer, and reports each retry loop's end
// on ended.
func controlRetries(t *testing.T) (tick chan<- time.Time, ended <-chan struct{}) {
	t.Helper()
	oldAfter, oldEnded := bridgeRetryAfter, bridgeRetryEnded
	tc, ec := make(chan time.Time), make(chan struct{}, 8)
	bridgeRetryAfter = func(time.Duration) <-chan time.Time { return tc }
	bridgeRetryEnded = func() { ec <- struct{}{} }
	t.Cleanup(func() { bridgeRetryAfter, bridgeRetryEnded = oldAfter, oldEnded })
	return tc, ec
}

// scriptedOpens replaces openBridgeSession with one that answers each open
// from results in turn (an error, or the session), counting them; gate,
// when set, holds the first open until the test sends on it.
func scriptedOpens(t *testing.T, gate <-chan struct{}, results ...any) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	old := openBridgeSession
	openBridgeSession = func(string, string, session.SessionOptions) (bridgeSub, error) {
		i := int(n.Add(1)) - 1
		if i == 0 && gate != nil {
			<-gate
		}
		if i >= len(results) {
			return nil, errors.New("no more scripted opens")
		}
		switch r := results[i].(type) {
		case error:
			return nil, r
		case bridgeSub:
			return r, nil
		}
		t.Errorf("bad scripted open %T", results[i])
		return nil, errors.New("bad script")
	}
	t.Cleanup(func() { openBridgeSession = old })
	return &n
}

func hubMachine() *config.Machine {
	return &config.Machine{Name: "h/web", Backend: config.BackendHub, Hub: config.NewHub("h", "u@h.example")}
}

// noRetries fails the test if a background retry is ever scheduled.
func noRetries(t *testing.T) {
	t.Helper()
	oldAfter := bridgeRetryAfter
	bridgeRetryAfter = func(time.Duration) <-chan time.Time {
		t.Error("a bridge open was retried")
		return make(chan time.Time)
	}
	t.Cleanup(func() { bridgeRetryAfter = oldAfter })
}

// A cold attach on a stopped VM on a hub: the hub's __session refuses it,
// so the first open fails at once. The attach is not held up, and the open
// is not retried into the boot the proxied attach starts: one line says
// how to get the bridge.
func TestHubAttachStoppedVMIsANotice(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	noRetries(t)
	stopped := errors.New("forward daemon for 'h/web' exited: h/web: the hub's __session ended before it started: devvm: web is not running; start it first ('devvm start web' also brings its forwards up)")
	opens := scriptedOpens(t, nil, stopped, newSignalBridge())
	a, errBuf := bridgeApp(t)
	a.bridgeSession(hubMachine(), nil)()
	if msg := errBuf.String(); !strings.Contains(msg, "browser bridge unavailable: h/web was not running when this session started; re-attach") {
		t.Fatalf("notice = %q", msg)
	}
	if opens.Load() != 1 {
		t.Fatalf("opened %d times, want 1", opens.Load())
	}
}

// Past hubBridgeWait too: a late not-running refusal ends it, no retry.
func TestHubAttachLateStoppedVMNotRetried(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	noRetries(t)
	ended := make(chan struct{}, 1)
	oldEnded, oldWait := bridgeRetryEnded, hubBridgeWait
	bridgeRetryEnded = func() { ended <- struct{}{} }
	hubBridgeWait = 20 * time.Millisecond
	t.Cleanup(func() { bridgeRetryEnded, hubBridgeWait = oldEnded, oldWait })
	gate := make(chan struct{})
	opens := scriptedOpens(t, gate, errors.New("h/web: hub: devvm: web is not running; start it first"), newSignalBridge())
	a, _ := bridgeApp(t)
	release := a.bridgeSession(hubMachine(), nil)
	defer release()
	close(gate)
	<-ended
	if opens.Load() != 1 {
		t.Fatalf("opened %d times, want 1", opens.Load())
	}
}

// A transient failure after the attach stopped waiting (a slow hub): the
// first open that fails past hubBridgeWait is retried, not dropped.
func TestHubAttachRetriesAfterLateFailure(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	tick, ended := controlRetries(t)
	old := hubBridgeWait
	hubBridgeWait = 20 * time.Millisecond
	t.Cleanup(func() { hubBridgeWait = old })
	gate := make(chan struct{})
	s := newSignalBridge()
	opens := scriptedOpens(t, gate, errors.New("h/web: the hub's __session did not start within 20s"), s)
	a, errBuf := bridgeApp(t)
	release := a.bridgeSession(hubMachine(), nil)
	if !strings.Contains(errBuf.String(), "browser bridge to hub h not up yet; URLs opened before it is will print in the guest") {
		t.Fatalf("notice = %q", errBuf.String())
	}
	close(gate) // the first open fails only now
	tick <- time.Time{}
	<-ended
	if opens.Load() != 2 {
		t.Fatalf("opened %d times, want 2", opens.Load())
	}
	<-s.subscribed
	release()
	<-s.closed
}

// release ends the retries (no open after it), and they give up after
// bridgeRetryWaits on their own.
func TestHubAttachRetriesBounded(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	tick, ended := controlRetries(t)
	down := errors.New("h/web: hub: connection refused")
	opens := scriptedOpens(t, nil, down, down, down, down)
	a, _ := bridgeApp(t)
	release := a.bridgeSession(hubMachine(), nil)
	release()
	<-ended
	if opens.Load() != 1 {
		t.Fatalf("opened %d times after release, want only the first", opens.Load()-1)
	}

	oldWaits := bridgeRetryWaits
	bridgeRetryWaits = []time.Duration{time.Second, time.Second}
	t.Cleanup(func() { bridgeRetryWaits = oldWaits })
	opens = scriptedOpens(t, nil, down, down, down, down)
	release = a.bridgeSession(hubMachine(), nil)
	defer release()
	tick <- time.Time{}
	tick <- time.Time{}
	<-ended
	if opens.Load() != 3 {
		t.Fatalf("opened %d times, want the first and one per retry wait", opens.Load())
	}
}

// A refusal no retry cures (a hub too old for browser opens, a machine
// with no events on its hub) is one line and no retries.
func TestHubAttachNoRetryWhenRefusedForGood(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	noRetries(t)
	for _, msg := range []string{
		"forward daemon for 'h/web' exited: h/web: devvm on hub h predates hub forwards (needs v0.1.14 or newer); run 'devvm update' there",
		"subscribed here, but not upstream: hub devvm too old for browser opens: the daemon for web on hub h predates v0.1.15",
		"subscribed here, but not upstream: no browser events from this machine on its hub: web on hub h is not a smol machine there",
	} {
		fb := &fakeBridge{subErr: errors.New(msg)}
		opens := scriptedOpens(t, nil, fb)
		a, errBuf := bridgeApp(t)
		a.bridgeSession(hubMachine(), nil)()
		if !strings.Contains(errBuf.String(), "browser bridge unavailable ("+msg+"); forwards still work") {
			t.Errorf("notice = %q", errBuf.String())
		}
		if opens.Load() != 1 || fb.closed != 1 {
			t.Errorf("%q: opened %d, closed %d", msg, opens.Load(), fb.closed)
		}
	}
}

// auth HUB/NAME on a machine the hub's daemon says has no browser events
// (NoEvents): refused as unsupported, before anything runs on the hub.
func TestHubAuthRefusedWithoutEvents(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	a := newTestApp(t)
	sshLog, _ := fakeHub(t, a)
	pinTTY(t, true)
	var log []string
	stubHubBridges(t, &orderedBridge{sshLog: sshLog, log: &log,
		subErr: errors.New("subscribed here, but not upstream: no browser events from this machine on its hub: web on hub h is not a smol machine there")})
	err := runTree(t, a, "auth", "h/web")
	if err == nil || err.Error() != "auth h/web: auth on a non-smol hub machine is not supported through the hub yet (its daemon on hub h carries no browser events)" {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Error("auth ran on the hub for a machine without events")
	}
}
