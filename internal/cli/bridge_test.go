package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// fakeBridge stands in for the session client attach/shell hold.
type fakeBridge struct {
	subErr     error
	subscribed int
	closed     int
}

func (f *fakeBridge) Subscribe() error { f.subscribed++; return f.subErr }
func (f *fakeBridge) Close() error     { f.closed++; return nil }

// stubBridge replaces openBridgeSession for one test, recording its calls.
func stubBridge(t *testing.T, fb *fakeBridge, openErr error) *[]string {
	t.Helper()
	var calls []string
	old := openBridgeSession
	openBridgeSession = func(dir config.Paths, name string, opts session.SessionOptions) (bridgeSub, error) {
		if opts.OnEvent == nil {
			t.Error("bridge session opened without an event handler")
		}
		calls = append(calls, name)
		if openErr != nil {
			return nil, openErr
		}
		return fb, nil
	}
	t.Cleanup(func() { openBridgeSession = old })
	return &calls
}

func bridgeApp(t *testing.T) (*App, *bytes.Buffer) {
	var errBuf bytes.Buffer
	return &App{Paths: testPaths(t.TempDir()), Stderr: &errBuf}, &errBuf
}

// attach/shell on a running smol machine hold a subscribed session for
// their lifetime, and let it go when they return.
func TestBridgeSessionSubscribes(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	fb := &fakeBridge{}
	calls := stubBridge(t, fb, nil)
	a, _ := bridgeApp(t)
	release := a.bridgeSession(managedSmol("web"), &statusBackend{})
	if len(*calls) != 1 || (*calls)[0] != "web" || fb.subscribed != 1 || fb.closed != 0 {
		t.Fatalf("calls %v, subscribed %d, closed %d", *calls, fb.subscribed, fb.closed)
	}
	release()
	if fb.closed != 1 {
		t.Fatalf("release closed the session %d times", fb.closed)
	}
}

// DEVVM_NO_SUBSCRIBE=1 (every proxied command on a hub) keeps attach/shell
// off the daemon entirely: no dial, no session, no subscription. A remote
// box subscribes like smol (its daemon runs the agent exec on its master).
func TestBridgeSessionSkipped(t *testing.T) {
	shrinkRunningPoll(t)
	for _, tc := range []struct {
		name  string
		env   string
		m     *config.Machine
		b     backend.Backend
		wants int
	}{
		{"DEVVM_NO_SUBSCRIBE=1", "1", managedSmol("web"), &statusBackend{}, 0},
		{"DEVVM_NO_SUBSCRIBE=0 subscribes", "0", managedSmol("web"), &statusBackend{}, 1},
		{"remote subscribes", "", config.NewRemote("box", "box"), &statusBackend{}, 1},
		{"entry without agent = true subscribes", "", config.NewRemote("box", "box"), &statusBackend{}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(backend.NoSubscribeEnv, tc.env)
			fb := &fakeBridge{}
			calls := stubBridge(t, fb, nil)
			a, errBuf := bridgeApp(t)
			a.bridgeSession(tc.m, tc.b)()
			if len(*calls) != tc.wants {
				t.Fatalf("session opened %d times, want %d", len(*calls), tc.wants)
			}
			if errBuf.Len() != 0 {
				t.Fatalf("printed %q", errBuf.String())
			}
		})
	}
}

// A smol VM still stopped when the bridge opens (attach/shell started it
// first, bootForSession, and it was stopped again) gets no daemon: its
// first dial would exec into it. One line, no retry.
func TestBridgeSessionStoppedVMIsANotice(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	noRetries(t)
	var opens atomic.Int32
	old := openBridgeSession
	openBridgeSession = func(config.Paths, string, session.SessionOptions) (bridgeSub, error) {
		opens.Add(1)
		return newSignalBridge(), nil
	}
	t.Cleanup(func() { openBridgeSession = old })
	a, errBuf := bridgeApp(t)
	a.bridgeSession(managedSmol("web"), &statusBackend{runningAfter: -1})()
	want := "devvm: browser bridge unavailable: web is not running; URLs opened in the guest will be printed there\n"
	if errBuf.String() != want {
		t.Fatalf("notice = %q, want %q", errBuf.String(), want)
	}
	if opens.Load() != 0 {
		t.Fatal("a session was opened for a stopped VM")
	}
}

// The hub's __session refuses a stopped VM with requireRunningForForwards'
// own words; vmNotRunning must keep recognizing them.
func TestVMNotRunningMatchesRefusal(t *testing.T) {
	shrinkRunningPoll(t)
	m := managedSmol("web")
	err := requireRunningForForwards(m, &statusBackend{runningAfter: -1}, 0)
	if !vmNotRunning(err) {
		t.Fatalf("vmNotRunning(%v) = false", err)
	}
	laptop := errors.New("forward daemon for 'h/web' exited: h/web: the hub's __session ended before it started: devvm: " + err.Error() + " (see run/h@web.log)")
	if !vmNotRunning(laptop) {
		t.Fatalf("the hub's refusal as the laptop sees it is not recognized: %v", laptop)
	}
	if noBridgeRetry(laptop) {
		t.Fatalf("a VM not running is a refusal for good: %v", laptop)
	}
	if vmNotRunning(errors.New("h/web: hub: connection refused")) {
		t.Fatal("a link failure read as a stopped VM")
	}
}

// pingBridge is a fakeBridge whose daemon answers ping (session.Session's).
type pingBridge struct {
	fakeBridge
	info session.PingInfo
}

func (p *pingBridge) Ping() (session.PingInfo, error) { return p.info, nil }

// A remote box's daemon that reports no shim has no agent to hear the
// guest: attach/shell let the session go (the daemon idles out) with one
// line naming the way to an agent, and the shell still opens. One that
// reports a shim keeps the session for the shell's life.
func TestBridgeSessionRemoteWithoutAgent(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	consented := config.NewRemote("box", "me@box")
	consented.Agent = true
	for _, tc := range []struct {
		m    *config.Machine
		want string
	}{
		{consented, "devvm: browser bridge unavailable: its devvm agent did not come up: its forward daemon may have started before agent = true was added (restart it: 'devvm ports down box', and end any attach holding it), or the install failed (see the daemon log); URLs opened in the guest will be printed there\n"},
		{config.NewRemote("adopted", "me@adopted"), "devvm: browser bridge unavailable: no current devvm agent there (agent = true in [machines.adopted] lets devvm install one in ~/.local/bin once the forward daemon restarts: 'devvm ports down adopted', and end any attach holding it); URLs opened in the guest will be printed there\n"},
		{config.NewProviderMachine("cloud", config.BackendHetzner, config.ProviderServer{Address: "cloud"}), "devvm: browser bridge unavailable: its devvm agent did not come up (see the daemon log); URLs opened in the guest will be printed there\n"},
	} {
		pb := &pingBridge{info: session.PingInfo{State: session.StateUp}}
		old := openBridgeSession
		openBridgeSession = func(config.Paths, string, session.SessionOptions) (bridgeSub, error) { return pb, nil }
		a, errBuf := bridgeApp(t)
		release := a.bridgeSession(tc.m, &statusBackend{})
		if errBuf.String() != tc.want || pb.subscribed != 1 || pb.closed != 1 {
			t.Errorf("%s: notice %q, subscribed %d, closed %d", tc.m.Name, errBuf.String(), pb.subscribed, pb.closed)
		}
		release()
		openBridgeSession = old
	}
	pb := &pingBridge{info: session.PingInfo{State: session.StateUp, Shim: "/home/me/.local/bin/devvm-open-url"}}
	old := openBridgeSession
	openBridgeSession = func(config.Paths, string, session.SessionOptions) (bridgeSub, error) { return pb, nil }
	t.Cleanup(func() { openBridgeSession = old })
	a, errBuf := bridgeApp(t)
	release := a.bridgeSession(config.NewRemote("adopted", "me@adopted"), &statusBackend{})
	if errBuf.Len() != 0 || pb.closed != 0 {
		t.Fatalf("with an agent: notice %q, closed %d", errBuf.String(), pb.closed)
	}
	release()
	if pb.closed != 1 {
		t.Fatalf("release closed %d times", pb.closed)
	}
}

// attach/shell on a remote box wait at most remoteBridgeWait for the
// bridge (a box that does not answer must not cost the session the
// daemon's whole come-up before its own ssh fails too): past it they say
// so in one line and go on, and the open lands behind them. A late
// session whose daemon has an agent is adopted and closed by release; one
// without is let go silently (the session owns the terminal by then).
func TestRemoteBridgeBoundedWait(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	oldWait, oldEnded, oldOpen := remoteBridgeWait, bridgeRetryEnded, openBridgeSession
	t.Cleanup(func() { remoteBridgeWait, bridgeRetryEnded, openBridgeSession = oldWait, oldEnded, oldOpen })
	remoteBridgeWait = 20 * time.Millisecond
	ended := make(chan struct{}, 1)
	bridgeRetryEnded = func() { ended <- struct{}{} }
	for _, shim := range []string{"/usr/local/bin/devvm-open-url", ""} {
		gate := make(chan struct{})
		pb := &pingBridge{info: session.PingInfo{State: session.StateUp, Shim: shim}}
		openBridgeSession = func(config.Paths, string, session.SessionOptions) (bridgeSub, error) {
			<-gate
			return pb, nil
		}
		a, errBuf := bridgeApp(t)
		start := time.Now()
		release := a.bridgeSession(config.NewRemote("cloud", "dev@cloud"), &statusBackend{})
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("bridgeSession waited %s for a daemon that never came up", d)
		}
		want := "devvm: browser bridge for cloud not up yet; URLs opened before it is will print in the guest\n"
		if errBuf.String() != want {
			t.Fatalf("shim %q: notice %q", shim, errBuf.String())
		}
		close(gate)
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatal("the late open never ended")
		}
		if errBuf.String() != want {
			t.Fatalf("shim %q: the late open printed %q", shim, errBuf.String())
		}
		wantClosed := 0
		if shim == "" {
			wantClosed = 1 // no agent: let go at once, quietly
		}
		if pb.subscribed != 1 || pb.closed != wantClosed {
			t.Fatalf("shim %q: subscribed %d, closed %d before release", shim, pb.subscribed, pb.closed)
		}
		release()
		if pb.closed != 1 {
			t.Fatalf("shim %q: closed %d after release", shim, pb.closed)
		}
	}
}

// A bridge that cannot be had costs one line, never the shell.
func TestBridgeSessionFailureIsANotice(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	a, errBuf := bridgeApp(t)
	stubBridge(t, &fakeBridge{}, errors.New("forward daemon for 'web' did not come up"))
	a.bridgeSession(managedSmol("web"), &statusBackend{})()
	if !strings.Contains(errBuf.String(), "browser bridge unavailable (forward daemon for 'web' did not come up)") {
		t.Fatalf("notice = %q", errBuf.String())
	}

	a, errBuf = bridgeApp(t)
	fb := &fakeBridge{subErr: errors.New("subscribe: no reply")}
	stubBridge(t, fb, nil)
	a.bridgeSession(managedSmol("web"), &statusBackend{})()
	if fb.closed != 1 || !strings.Contains(errBuf.String(), "browser bridge unavailable (subscribe: no reply)") {
		t.Fatalf("closed %d, notice %q", fb.closed, errBuf.String())
	}
}

// ports list shows a bridge callback's remaining ttl after the tokens the
// menubar parses.
func TestTTLLeft(t *testing.T) {
	if got := ttlLeft(9*60e9 + 42e9 + 3e8); got != "9m42s" {
		t.Errorf("ttlLeft = %q", got)
	}
	if got := ttlLeft(-1); got != "expiring" {
		t.Errorf("ttlLeft(past) = %q", got)
	}
}

// attach/shell on a stopped smol VM start it the way `start` does (smolvm
// refuses an exec into a stopped VM; it never boots one) and only
// then open the bridge and run their own execs, so the session is bridged.
func TestAttachStartsStoppedSmol(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	shrinkRunningPoll(t)
	for _, leaf := range []string{"attach", "shell"} {
		t.Run(leaf, func(t *testing.T) {
			a := newTestApp(t)
			log := fakeBootingSmolvm(t, "stopped", "")
			var atOpen []string
			old := openBridgeSession
			openBridgeSession = func(config.Paths, string, session.SessionOptions) (bridgeSub, error) {
				atOpen = smolvmCalls(t, log)
				return &fakeBridge{}, nil
			}
			t.Cleanup(func() { openBridgeSession = old })
			if err := runTree(t, a, leaf, "web"); err != nil {
				t.Fatalf("%s: %v\n%s", leaf, err, a.Stderr)
			}
			if len(atOpen) != 1 || atOpen[0] != "machine start --name web" {
				t.Errorf("smolvm calls when the bridge opened = %q, want only the start", atOpen)
			}
			calls := smolvmCalls(t, log)
			if len(calls) < 2 || calls[0] != "machine start --name web" || !strings.HasPrefix(calls[1], "machine exec") {
				t.Errorf("smolvm calls = %q, want the start before any exec", calls)
			}
			msg := a.Stderr.(*bytes.Buffer).String()
			if !strings.Contains(msg, "devvm: starting web…") || strings.Contains(msg, "bridge unavailable") {
				t.Errorf("stderr = %q", msg)
			}
		})
	}

	// A running VM is not started again; one that will not start fails the
	// session before any exec or bridge.
	a := newTestApp(t)
	log := fakeBootingSmolvm(t, "running", "")
	stubBridge(t, &fakeBridge{}, nil)
	if err := runTree(t, a, "shell", "web"); err != nil {
		t.Fatal(err)
	}
	if calls := smolvmCalls(t, log); len(calls) != 1 || !strings.HasPrefix(calls[0], "machine exec") {
		t.Errorf("smolvm calls = %q, want the shell's exec alone", calls)
	}
	log = fakeBootingSmolvm(t, "stopped", "exit 1")
	calls := stubBridge(t, &fakeBridge{}, nil)
	if err := runTree(t, a, "attach", "web"); err == nil {
		t.Fatal("attach went ahead on a VM that did not start")
	}
	if c := smolvmCalls(t, log); len(c) != 1 || len(*calls) != 0 {
		t.Errorf("smolvm calls %q, bridge opens %q: want the failed start alone", c, *calls)
	}
}

// fakeBootingSmolvm is fakeLocalSmolvm for one VM, web (devvm's: labelled),
// in state: `machine start` runs startBody (exit 0 if empty) and then lists
// it running; every other call succeeds.
func fakeBootingSmolvm(t *testing.T, state, startBody string) (log string) {
	t.Helper()
	labels := devvmLabel
	dir := t.TempDir()
	ls, log := filepath.Join(dir, "ls.json"), filepath.Join(dir, "smolvm.log")
	if err := os.WriteFile(ls, []byte(`[{"name":"web","state":"`+state+`",`+labels+`}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"  'machine ls --json') cat " + ls + ";;\n" +
		"  'machine start --name web') " + startBody + "\n" +
		"    echo '[{\"name\":\"web\",\"state\":\"running\"," + labels + "}]' > " + ls + ";;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "smolvm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// managedSmol is a smol record its discovery source settled as managed
// (devvm made it): what every smol path that installs or shapes assumes.
func managedSmol(name string) *config.Machine {
	m := &config.Machine{Name: name, Backend: config.BackendSmol}
	m.SetManaged(true)
	return m
}

// bootBackend is a smol backend whose Status walks states (the last one
// repeats) and whose PowerStart is counted and may fail.
type bootBackend struct {
	backend.Backend
	states   []string
	calls    int
	starts   int
	startErr error
}

func (b *bootBackend) Status() (backend.State, error) {
	raw := b.states[min(b.calls, len(b.states)-1)]
	b.calls++
	return backend.State{Running: raw == "running", Raw: raw}, nil
}

func (b *bootBackend) PowerStart() error { b.starts++; return b.startErr }

// startStopped starts a VM listed booting too (smolvm 1.35.5 lists one as
// unreachable, and answers a second start "already running", rc 0),
// forgives a PowerStart that failed when the VM runs anyway, and never
// blocks the session on a Status error.
func TestStartStoppedRaces(t *testing.T) {
	shrinkRunningPoll(t)
	m := managedSmol("web")
	for _, tc := range []struct {
		name       string
		b          *bootBackend
		wantBooted bool
		wantStarts int
		wantErr    string
		wantLine   string
	}{
		{"a booting VM is started again", &bootBackend{states: []string{"unreachable", "unreachable", "running"}}, true, 1, "", "starting web…"},
		{"stopped is started", &bootBackend{states: []string{"stopped", "stopped", "running"}}, true, 1, "", "starting web…"},
		{"a failed start that raced another", &bootBackend{states: []string{"stopped", "stopped", "running"}, startErr: errors.New("already starting")}, true, 1, "", "starting web…"},
		{"a failed start", &bootBackend{states: []string{"stopped"}, startErr: errors.New("boom")}, false, 1, "boom", "starting web…"},
		{"never running", &bootBackend{states: []string{"stopped"}}, true, 1, "web did not reach running within", "starting web…"},
		{"running", &bootBackend{states: []string{"running"}}, false, 0, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, errBuf := bridgeApp(t)
			booted, err := a.startStopped(m, tc.b)
			if booted != tc.wantBooted || tc.b.starts != tc.wantStarts {
				t.Errorf("booted %v starts %d, want %v %d", booted, tc.b.starts, tc.wantBooted, tc.wantStarts)
			}
			if (err == nil) != (tc.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
			if !strings.Contains(errBuf.String(), tc.wantLine) {
				t.Errorf("stderr = %q", errBuf.String())
			}
		})
	}
	a, _ := bridgeApp(t)
	if booted, err := a.startStopped(m, &statusBackend{statusErr: errors.New("smolvm missing")}); booted || err != nil {
		t.Errorf("a Status error: booted %v, err %v; want neither", booted, err)
	}
}

// `start` waits for "running" even with no saved forwards: smolvm may
// return while the VM is still starting, and a proxied attach's __session
// right after it would be refused.
func TestStartWaitsForRunning(t *testing.T) {
	shrinkRunningPoll(t)
	a := newTestApp(t)
	dir := t.TempDir()
	ls := filepath.Join(dir, "ls.json")
	if err := os.WriteFile(ls, []byte(`[{"name":"web","state":"stopped","labels":{"devvm":"true"}}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	// The start leaves it starting; the second listing after it says running.
	n := filepath.Join(dir, "n")
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"  'machine ls --json') if [ -f " + n + " ]; then c=$(cat " + n + "); echo $((c+1)) > " + n + "; [ $c -ge 2 ] && echo '[{\"name\":\"web\",\"state\":\"running\",\"labels\":{\"devvm\":\"true\"}}]' && exit 0; fi; cat " + ls + ";;\n" +
		"  'machine start --name web') echo 0 > " + n + "; echo '[{\"name\":\"web\",\"state\":\"starting\",\"labels\":{\"devvm\":\"true\"}}]' > " + ls + ";;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "smolvm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := runTree(t, a, "start", "web"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if c, _ := os.ReadFile(n); strings.TrimSpace(string(c)) != "3" {
		t.Errorf("listings after the start = %q, want start to poll until running", c)
	}
}

// observePushes reports each pushSaved whose background Adds are over,
// by machine name.
func observePushes(t *testing.T) <-chan string {
	t.Helper()
	old := savedPushed
	ch := make(chan string, 8)
	savedPushed = func(name string) { ch <- name }
	t.Cleanup(func() { savedPushed = old })
	return ch
}

// waitPushed waits a few seconds at most for the next push to end and
// checks it was name's: a push that never comes fails the test, not
// go test's timeout.
func waitPushed(t *testing.T, pushed <-chan string, name string) {
	t.Helper()
	select {
	case got := <-pushed:
		if got != name {
			t.Fatalf("pushed %q, want %q", got, name)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no push for %s", name)
	}
}

// attach/shell bring the saved forwards up when they connect (simplify.md
// step 11), a stopped VM's after its boot: one Add per line on the daemon
// the bridge opened, one-shot (so conf-owned, as `ports up`'s), and
// nothing printed: the session owns the terminal.
func TestAttachStoppedSmolPushesSavedForwards(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	shrinkRunningPoll(t)
	a := newTestApp(t)
	log := fakeBootingSmolvm(t, "stopped", "")
	if err := config.SavePorts(a.Paths, "web", []string{"3000", "8080:80"}); err != nil {
		t.Fatal(err)
	}
	od := serveOwnerDaemon(t, a.Paths, "web", nil, false)
	pushed := observePushes(t)
	stubBridge(t, &fakeBridge{}, nil)
	if err := runTree(t, a, "shell", "web"); err != nil {
		t.Fatalf("shell: %v\n%s", err, a.Stderr)
	}
	waitPushed(t, pushed, "web")
	adds := od.requests(session.OpAdd)
	if len(adds) != 2 || adds[0].Host != 3000 || adds[0].Guest != 3000 || adds[1].Host != 8080 || adds[1].Guest != 80 {
		t.Errorf("adds = %+v, want 3000 then 8080:80", adds)
	}
	if out := a.Stdout.(*bytes.Buffer).String() + a.Stderr.(*bytes.Buffer).String(); strings.Contains(out, "forward") {
		t.Errorf("the push printed: %q", out)
	}
	if c := smolvmCalls(t, log); len(c) < 2 || c[0] != "machine start --name web" || !strings.HasPrefix(c[len(c)-1], "machine exec") {
		t.Errorf("smolvm calls = %q, want the start first and the shell's exec last", c)
	}
}

// The push never spawns a daemon, prints nothing and fails nothing: with
// no daemon to take the Adds (the bridge's open is faked here) it is
// over at once. With no saved forwards there is no push at all.
func TestBridgePushIsQuietAndBestEffort(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	pushed := observePushes(t)
	stubBridge(t, &fakeBridge{}, nil)
	a, errBuf := bridgeApp(t)
	m := config.NewRemote("box", "dev@example")
	a.bridgeSession(m, nil)()
	select {
	case name := <-pushed:
		t.Fatalf("pushed %q with no saved forwards", name)
	default:
	}
	if err := config.SavePorts(a.Paths, "box", []string{"3000"}); err != nil {
		t.Fatal(err)
	}
	a.bridgeSession(m, nil)()
	waitPushed(t, pushed, "box")
	if errBuf.Len() != 0 {
		t.Errorf("stderr = %q", errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(a.Paths.RuntimeDir(), "box.sock")); err == nil {
		t.Error("the push spawned a daemon")
	}
}
