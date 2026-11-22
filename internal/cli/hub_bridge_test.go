package cli

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// The laptop half of the bridge for hub machines (hub.md §8, roadmap step
// 8): attach/shell HUB/NAME subscribe on this host's daemon for
// HUB@NAME before the leaf is proxied with DEVVM_NO_SUBSCRIBE=1 (the fake
// hub devvm refuses to run without it).

// orderedBridge is a session stand-in that records its calls into log,
// with whether the hub had been dialed yet at each.
type orderedBridge struct {
	sshLog string
	log    *[]string
	subErr error
}

func (b *orderedBridge) note(what string) {
	if _, err := os.Stat(b.sshLog); err == nil {
		what += " (after ssh)"
	}
	*b.log = append(*b.log, what)
}

func (b *orderedBridge) Subscribe() error { b.note("subscribe"); return b.subErr }
func (b *orderedBridge) Close() error     { b.note("close"); return nil }

// stubHubBridges replaces the session opener for one test.
func stubHubBridges(t *testing.T, br *orderedBridge) *[]string {
	t.Helper()
	var dials []string
	old := openBridgeSession
	openBridgeSession = func(dir config.Paths, name string, opts session.SessionOptions) (bridgeSub, error) {
		if opts.OnEvent == nil {
			t.Error("bridge session opened without an event handler")
		}
		dials = append(dials, name)
		return br, nil
	}
	t.Cleanup(func() { openBridgeSession = old })
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
	release := openHubBridge(t, a)
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
	openBridgeSession = func(dir config.Paths, name string, opts session.SessionOptions) (bridgeSub, error) {
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
	openBridgeSession = func(config.Paths, string, session.SessionOptions) (bridgeSub, error) {
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

// openHubBridge is a proxied attach's hubBridge on hubMachine, with a boot
// that must not run.
func openHubBridge(t *testing.T, a *App) (release func()) {
	t.Helper()
	release, err := a.hubBridge(hubMachine(), func() error { t.Error("booted"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return release
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

// A proxied attach/shell on a VM its hub says is not running: the proxied
// `start` first (no terminal: its ssh must not read the user's typeahead),
// then the bridge opened again and subscribed, and only
// then the session, so it is bridged from its first URL.
func TestHubAttachStartsStoppedVM(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	noRetries(t)
	stopped := errors.New("h/web: the hub's __session ended before it started: devvm: web is not running; start it first")
	for _, leaf := range []string{"attach", "shell"} {
		t.Run(leaf, func(t *testing.T) {
			a := newTestApp(t)
			sshLog, argvLog := fakeHub(t, a)
			if err := config.SavePorts(a.Paths, config.JoinHubName("h", "web"), nil); err != nil { // no daemon to dial after the start
				t.Fatal(err)
			}
			pinTTY(t, true)
			s := newSignalBridge()
			var atOpen []int
			old := openBridgeSession
			openBridgeSession = func(config.Paths, string, session.SessionOptions) (bridgeSub, error) {
				atOpen = append(atOpen, len(devvmCalls(t, argvLog)))
				if len(atOpen) == 1 {
					return nil, stopped
				}
				return s, nil
			}
			t.Cleanup(func() { openBridgeSession = old })
			if err := runTree(t, a, leaf, "h/web"); err != nil {
				t.Fatalf("%s: %v\n%s", leaf, err, a.Stderr)
			}
			if !slices.Equal(atOpen, []int{0, 1}) {
				t.Errorf("hub calls before each open = %v, want none, then the start", atOpen)
			}
			if lines := sshLines(t, sshLog); len(lines) != 2 || lines[0] != wantSSH(a, false, "start", "web") || lines[1] != wantSSH(a, true, leaf, "web") {
				t.Errorf("ssh lines = %q", lines)
			}
			<-s.closed
			msg := a.Stderr.(interface{ String() string }).String()
			if !strings.Contains(msg, "devvm: starting h/web…") || strings.Contains(msg, "bridge unavailable") {
				t.Errorf("stderr = %q", msg)
			}
		})
	}

	// A start that fails is the attach's failure: nothing else runs.
	a := newTestApp(t)
	sshLog, _ := fakeHub(t, a)
	pinTTY(t, true)
	t.Setenv("FAKE_DEVVM_EXIT", "1")
	opens := scriptedOpens(t, nil, stopped)
	if err := runTree(t, a, "attach", "h/web"); err == nil {
		t.Fatal("attach went ahead after a failed start")
	}
	if lines := sshLines(t, sshLog); len(lines) != 1 || lines[0] != wantSSH(a, false, "start", "web") || opens.Load() != 1 {
		t.Errorf("ssh lines %q, opens %d: want the failed start alone", lines, opens.Load())
	}
}

// Past hubBridgeWait a not-running refusal is retried: the hub-side
// attach starts the VM (bootForSession), and the retry adopts the bridge.
func TestHubAttachLateStoppedVMRetried(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	tick, ended := controlRetries(t)
	old := hubBridgeWait
	hubBridgeWait = 20 * time.Millisecond
	t.Cleanup(func() { hubBridgeWait = old })
	gate := make(chan struct{})
	s := newSignalBridge()
	opens := scriptedOpens(t, gate, errors.New("h/web: hub: devvm: web is not running; start it first"), s)
	a, _ := bridgeApp(t)
	release := openHubBridge(t, a)
	close(gate)
	tick <- time.Time{}
	<-ended
	if opens.Load() != 2 {
		t.Fatalf("opened %d times, want 2", opens.Load())
	}
	<-s.subscribed
	release()
	<-s.closed
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
	release := openHubBridge(t, a)
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
	release := openHubBridge(t, a)
	release()
	<-ended
	if opens.Load() != 1 {
		t.Fatalf("opened %d times after release, want only the first", opens.Load()-1)
	}

	oldWaits := bridgeRetryWaits
	bridgeRetryWaits = []time.Duration{time.Second, time.Second}
	t.Cleanup(func() { bridgeRetryWaits = oldWaits })
	opens = scriptedOpens(t, nil, down, down, down, down)
	release = openHubBridge(t, a)
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
		"subscribed here, but not upstream: no browser events from this machine on its hub: web on hub h carries no browser events there",
	} {
		fb := &fakeBridge{subErr: errors.New(msg)}
		opens := scriptedOpens(t, nil, fb)
		a, errBuf := bridgeApp(t)
		openHubBridge(t, a)()
		if !strings.Contains(errBuf.String(), "browser bridge unavailable ("+msg+"); forwards still work") {
			t.Errorf("notice = %q", errBuf.String())
		}
		if opens.Load() != 1 || fb.closed != 1 {
			t.Errorf("%q: opened %d, closed %d", msg, opens.Load(), fb.closed)
		}
	}
}

// A hub whose start returns before the VM runs (no saved forwards there,
// before start waited): the reopen after the boot is refused again, and
// that is retried in the background, not a notice, so the session ends
// with a bridge.
func TestHubBridgeBootThenStillStoppedRetried(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	tick, ended := controlRetries(t)
	stopped := errors.New("h/web: the hub's __session ended before it started: devvm: web is not running; start it first")
	s := newSignalBridge()
	opens := scriptedOpens(t, nil, stopped, stopped, s)
	a, errBuf := bridgeApp(t)
	boots := 0
	release, err := a.hubBridge(hubMachine(), func() error { boots++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	tick <- time.Time{}
	<-ended
	<-s.subscribed
	release()
	<-s.closed
	if boots != 1 || opens.Load() != 3 {
		t.Errorf("boots %d, opens %d; want 1 and 3", boots, opens.Load())
	}
	if msg := errBuf.String(); strings.Contains(msg, "bridge unavailable") || !strings.Contains(msg, "retrying in the background") {
		t.Errorf("stderr = %q", msg)
	}
}

// A hub machine's saved forwards here come up on this host's daemon for
// it as soon as an open reaches that daemon, even one whose subscribe then
// fails; the retry adopted after it pushes again, which only re-adds the
// same forward.
func TestHubBridgeRetryBringsSavedForwardsUp(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	tick, ended := controlRetries(t)
	a := newTestApp(t)
	var errBuf bytes.Buffer
	a.Stdout, a.Stderr = new(bytes.Buffer), &errBuf
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}})
	od := serveOwnerDaemon(t, a.Paths, "h/web", nil, false)
	pushed := observePushes(t)
	s := newSignalBridge()
	opens := scriptedOpens(t, nil, &fakeBridge{subErr: errors.New("subscribe timed out")}, s)
	release := openHubBridge(t, a)
	waitPushed(t, pushed, "h/web") // the first open's, before any retry
	if adds := od.requests(session.OpAdd); len(adds) != 1 || adds[0].Guest != 3000 {
		t.Fatalf("adds = %+v, want 3000 up behind the first open", adds)
	}
	tick <- time.Time{}
	<-ended
	<-s.subscribed
	release()
	waitPushed(t, pushed, "h/web")
	if opens.Load() != 2 {
		t.Errorf("opens = %d, want the failed one and its retry", opens.Load())
	}
	for _, add := range od.requests(session.OpAdd) {
		if add.Guest != 3000 || add.Host != 3000 {
			t.Errorf("add %+v, want 3000 alone", add)
		}
	}
	if a.Stdout.(*bytes.Buffer).Len() != 0 || strings.Contains(errBuf.String(), "forward") {
		t.Errorf("the push printed: stdout %q, stderr %q", a.Stdout, errBuf.String())
	}
}

// A proxied attach that starts the VM brings this host's saved forwards
// up behind the bridge it then opens (no separate push after the boot),
// quietly, and the session runs.
func TestHubAttachStartsStoppedVMPushesForwards(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	shrinkRunningPoll(t)
	noRetries(t)
	a := newTestApp(t)
	sshLog, _ := fakeHub(t, a) // saves h/web's 3000 here
	pinTTY(t, true)
	od := serveOwnerDaemon(t, a.Paths, "h/web", nil, false)
	pushed := observePushes(t)
	stopped := errors.New("h/web: the hub's __session ended before it started: devvm: web is not running; start it first")
	scriptedOpens(t, nil, stopped, newSignalBridge())
	if err := runTree(t, a, "attach", "h/web"); err != nil {
		t.Fatalf("attach: %v\n%s", err, a.Stderr)
	}
	waitPushed(t, pushed, "h/web")
	if adds := od.requests(session.OpAdd); len(adds) != 1 || adds[0].Guest != 3000 {
		t.Errorf("adds = %+v, want 3000 once", adds)
	}
	if lines := sshLines(t, sshLog); len(lines) < 2 || lines[len(lines)-1] != wantSSH(a, true, "attach", "web") {
		t.Errorf("ssh lines = %q, want the attach last", lines)
	}
	if msg := a.Stderr.(interface{ String() string }).String(); strings.Contains(msg, "saved forwards") {
		t.Errorf("stderr = %q", msg)
	}
}
