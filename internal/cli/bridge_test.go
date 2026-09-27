package cli

import (
	"bytes"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

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
	openBridgeSession = func(dir, name string, opts session.SessionOptions) (bridgeSub, error) {
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
	return &App{ConfigDir: t.TempDir(), Stderr: &errBuf}, &errBuf
}

// attach/shell on a running smol machine hold a subscribed session for
// their lifetime, and let it go when they return.
func TestBridgeSessionSubscribes(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	fb := &fakeBridge{}
	calls := stubBridge(t, fb, nil)
	a, _ := bridgeApp(t)
	release := a.bridgeSession(&config.Machine{Name: "web", Backend: config.BackendSmol}, &statusBackend{})
	if len(*calls) != 1 || (*calls)[0] != "web" || fb.subscribed != 1 || fb.closed != 0 {
		t.Fatalf("calls %v, subscribed %d, closed %d", *calls, fb.subscribed, fb.closed)
	}
	release()
	if fb.closed != 1 {
		t.Fatalf("release closed the session %d times", fb.closed)
	}
}

// DEVVM_NO_SUBSCRIBE=1 (every proxied command on a hub) keeps attach/shell
// off the daemon entirely: no dial, no session, no subscription. So do a
// remote machine (no events until the ssh agent exec) and a stopped VM (no
// daemon is ever spawned for one).
func TestBridgeSessionSkipped(t *testing.T) {
	shrinkRunningPoll(t)
	for _, tc := range []struct {
		name  string
		env   string
		m     *config.Machine
		b     backend.Backend
		wants int
	}{
		{"DEVVM_NO_SUBSCRIBE=1", "1", &config.Machine{Name: "web", Backend: config.BackendSmol}, &statusBackend{}, 0},
		{"DEVVM_NO_SUBSCRIBE=0 subscribes", "0", &config.Machine{Name: "web", Backend: config.BackendSmol}, &statusBackend{}, 1},
		{"remote", "", &config.Machine{Name: "box", Backend: config.BackendRemoteManaged}, &statusBackend{}, 0},
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

// attach on a stopped smol VM (which the attach boots) spawns no daemon
// for it, now or later: a background open would overlap the attach's own
// execs while the VM boots. One line says how to get the bridge.
func TestBridgeSessionStoppedVMIsANotice(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	noRetries(t)
	var opens atomic.Int32
	old := openBridgeSession
	openBridgeSession = func(string, string, session.SessionOptions) (bridgeSub, error) {
		opens.Add(1)
		return newSignalBridge(), nil
	}
	t.Cleanup(func() { openBridgeSession = old })
	a, errBuf := bridgeApp(t)
	a.bridgeSession(&config.Machine{Name: "web", Backend: config.BackendSmol}, &statusBackend{runningAfter: -1})()
	want := "devvm: browser bridge unavailable: web was not running when this session started; re-attach (or run 'devvm shell web') for browser opens\n"
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
	m := &config.Machine{Name: "web", Backend: config.BackendSmol}
	err := requireRunningForForwards(m, &statusBackend{runningAfter: -1}, 0)
	if !vmNotRunning(err) {
		t.Fatalf("vmNotRunning(%v) = false", err)
	}
	laptop := errors.New("forward daemon for 'h/web' exited: h/web: the hub's __session ended before it started: devvm: " + err.Error() + " (see run/h@web.log)")
	if !vmNotRunning(laptop) || !noBridgeRetry(laptop) {
		t.Fatalf("the hub's refusal as the laptop sees it is not recognized: %v", laptop)
	}
	if vmNotRunning(errors.New("h/web: hub: connection refused")) {
		t.Fatal("a link failure read as a stopped VM")
	}
}

// A bridge that cannot be had costs one line, never the shell.
func TestBridgeSessionFailureIsANotice(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	a, errBuf := bridgeApp(t)
	stubBridge(t, &fakeBridge{}, errors.New("forward daemon for 'web' did not come up"))
	a.bridgeSession(&config.Machine{Name: "web", Backend: config.BackendSmol}, &statusBackend{})()
	if !strings.Contains(errBuf.String(), "browser bridge unavailable (forward daemon for 'web' did not come up)") {
		t.Fatalf("notice = %q", errBuf.String())
	}

	a, errBuf = bridgeApp(t)
	fb := &fakeBridge{subErr: errors.New("subscribe: no reply")}
	stubBridge(t, fb, nil)
	a.bridgeSession(&config.Machine{Name: "web", Backend: config.BackendSmol}, &statusBackend{})()
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
