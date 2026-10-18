package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smweber/devvm/internal/auth"
	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/hostbrowser"
	"github.com/smweber/devvm/internal/session"
)

// bridgeSub is what attach/shell hold for their lifetime: a session on the
// machine's forward daemon, subscribed to its browser events
// (browser-bridge.md §3). *session.Session in production.
type bridgeSub interface {
	Subscribe() error
	Close() error
}

// openBridgeSession dials the machine's daemon (spawning it if needed) and
// opens a session on it; a var so tests can see whether, and how, it runs.
var openBridgeSession = func(paths config.Paths, name string, opts session.SessionOptions) (bridgeSub, error) {
	return session.OpenSession(paths, name, opts)
}

// hubBridgeWait bounds how long a proxied attach/shell waits for its
// bridge: the laptop daemon's come-up (a relay to the hub, whose daemon may
// first exec into its VM) and the subscribe acked across the hop. Past it
// the session is proxied at once and the bridge keeps coming up in the
// background (lateBridge), so an unreachable or slow hub costs the attach
// at most this before its own ssh runs. A var for tests.
var hubBridgeWait = 5 * time.Second

// bridgeRetryWaits are the pauses before each background retry of a hub
// bridge that failed or outlived hubBridgeWait (about 45s in all): long
// enough for a slow or briefly unreachable hub, short enough that one that
// stays unreachable is given up on. A var for tests.
//
// A VM the hub reported not running only after the attach stopped waiting
// is retried too: a hub at this version starts it in the hub-side attach
// (bootForSession; an older hub's attach still fails on a stopped VM), and
// the retries' exec startups then overlap only that attach's, which smolvm
// 1.16.1 merely serializes (an exec during a boot waits for it, ~1s;
// measured on real smol, ROADMAP "Lessons").
var bridgeRetryWaits = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second}

// bridgeRetryAfter is time.After for the retries; tests swap in a channel
// they fire themselves, so no retry is timed by a sleep.
var bridgeRetryAfter = time.After

// lateBridge holds a bridge that finished opening after the attach stopped
// waiting for it: adopted for the rest of the attach, closed by release,
// or closed at once when it arrives after release already ran. release
// also ends the retries (retry), and an open in flight then lands on a
// released holder and is closed there.
type lateBridge struct {
	mu       sync.Mutex
	s        bridgeSub
	released bool
	stop     chan struct{} // closed by release; retry waits on it
}

func newLateBridge() *lateBridge { return &lateBridge{stop: make(chan struct{})} }

func (l *lateBridge) adopt(s bridgeSub) {
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		s.Close()
		return
	}
	l.s = s
	l.mu.Unlock()
}

func (l *lateBridge) release() {
	l.mu.Lock()
	s := l.s
	if !l.released {
		close(l.stop)
	}
	l.s, l.released = nil, true
	l.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

// retry opens the bridge again after each of bridgeRetryWaits, adopting
// the first that opens, until release or a refusal no retry cures (a hub
// too old for browser opens, a machine without events there). Failures
// are not reported: the attach owns the terminal by then, and the guest's
// shim prints the URL all the same. It reports whether one was adopted.
func (l *lateBridge) retry(open func() (bridgeSub, error)) bool {
	for _, w := range bridgeRetryWaits {
		select {
		case <-l.stop:
			return false
		case <-bridgeRetryAfter(w):
		}
		select {
		case <-l.stop:
			return false
		default:
		}
		s, err := open()
		if err == nil {
			l.adopt(s)
			return true
		}
		if noBridgeRetry(err) {
			return false
		}
	}
	return false
}

// noBridgeRetry: a bridge failure no background retry should follow, a
// permanent refusal (session.BridgeRefusedForGood). A VM that was not
// running is retried (see bridgeRetryWaits).
func noBridgeRetry(err error) bool {
	return session.BridgeRefusedForGood(err)
}

// notRunningText is in requireRunningForForwards' refusal, which is also
// what a hub's `__session` answers for a stopped VM (runSession), quoted
// back to this host in its daemon's exit error.
const notRunningText = "is not running; start it first"

// vmNotRunning reports whether a bridge open failed because the VM was
// not running, here or on the hub.
func vmNotRunning(err error) bool {
	return err != nil && strings.Contains(err.Error(), notRunningText)
}

// notRunningNotice is attach/shell's one line for a VM still not running
// when the bridge opens, although the session started it first
// (bootForSession, hubBootForSession): stopped again meanwhile. No bridge
// for this session.
func (a *App) notRunningNotice(m *config.Machine) {
	fmt.Fprintf(a.Stderr, "devvm: browser bridge unavailable: %s is not running; URLs opened in the guest will be printed there\n", m.Name)
}

// bridgeRetryEnded runs when a background bridge open (the late first
// open and its retries) is over, whatever became of it; a hook for tests.
var bridgeRetryEnded = func() {}

// noSubscribe reports whether DEVVM_NO_SUBSCRIBE is set: the hub proxy sets
// it on every proxied command so the laptop's subscription is the only one
// for that session (hub.md §8), and then attach/shell do not touch the
// daemon at all (no dial, no session, no subscription).
func noSubscribe() bool {
	v := os.Getenv(backend.NoSubscribeEnv)
	return v != "" && v != "0"
}

// openAuthBridge is openBridgeSession for thin `auth`, which also reads the
// shim from ping and binds its callback port on the session (auth.Bridge);
// a var so tests can stand in for the daemon.
var openAuthBridge = func(paths config.Paths, name string, opts session.SessionOptions) (auth.Bridge, error) {
	return session.OpenSession(paths, name, opts)
}

// hostOpener is the OnEvent of every session this host subscribes: open
// the URL here, and print it on this terminal when no opener can.
func (a *App) hostOpener() session.SessionOptions {
	return session.SessionOptions{OnEvent: session.OpenOnHost(a.Stderr, hostbrowser.Open)}
}

// bridged reports whether attach/shell/auth hold a subscription for m: a
// local smol machine devvm manages (its daemon's agent exec carries
// events), or a machine on a hub, through this host's daemon for HUB@NAME,
// whose relay subscribes on the hub daemon (hub.md §8; the hub refuses a
// VM it does not manage). A smol VM devvm did not create gets no bridge:
// subscribing would spawn a daemon whose transport installs the agent
// (config.md §2). Remote boxes wait for the ssh agent exec (bridge §1,
// "Remote", deferred).
func bridged(m *config.Machine) bool {
	return m.IsHubMachine() || (!m.IsRemote() && m.Managed())
}

// bridgeSession opens the browser bridge for an interactive session and
// returns what releases it. While it is held, a URL a guest program opens
// (the BROWSER shim, devvm-open-url) is opened here, the forward it needs
// is bound, and the guest is told what happened; the session client
// reconnects and re-subscribes on its own if the daemon is cycled under it
// (update, ports down/up). Best-effort: without it the shell still opens,
// and the guest's shim prints the URL instead.
//
// For a machine on a hub this is the laptop half of a proxied attach/shell
// (hub.md §8): subscribed here, acked only once this host's daemon is
// subscribed on the hub's, before the leaf is proxied with
// DEVVM_NO_SUBSCRIBE=1, so the first URL the session opens already lands
// here. The attach waits at most hubBridgeWait for it: past that it goes
// ahead and the bridge is adopted whenever it comes up.
//
// A hub bridge that fails for a reason a retry may cure (a slow or briefly
// unreachable hub) is retried in the background for bridgeRetryWaits and
// adopted the same way; the attach itself never waits on a retry.
// attach/shell start a stopped VM before they get here (bootForSession,
// hubBootForSession), so a VM still not running gets one line and no
// bridge. b is only asked whether a smol VM runs (a stopped one gets no
// daemon), so it may be nil for a hub machine.
func (a *App) bridgeSession(m *config.Machine, b backend.Backend) (release func()) {
	release = func() {}
	if noSubscribe() || !bridged(m) {
		return release
	}
	if m.IsHubMachine() {
		release, _ = a.hubBridge(m, nil)
		return release
	}
	if requireRunningForForwards(m, b, 0) != nil {
		// A stopped VM gets no daemon (its first dial would exec into it).
		a.notRunningNotice(m)
		return release
	}
	s, err := a.openSubscribed(m)
	if err != nil {
		fmt.Fprintf(a.Stderr, "devvm: browser bridge unavailable (%v); URLs opened in the guest will be printed there\n", err)
		return release
	}
	return func() { s.Close() }
}

// hubBridge is bridgeSession for a machine on a hub. The wait is bounded
// (hubBridgeWait), the setup is not. A cold start (the first attach after
// a break, when a login is most likely) pays for the laptop and hub
// daemons either way, so the open finishes in the background and the late
// session is adopted for the rest of the attach instead of being thrown
// away. A failed open is retried in the background (bridgeRetryWaits).
//
// The hub's `__session` refuses a VM that is not running. Answered within
// the wait, boot (when set: proxyInteractive's hubBootForSession) starts
// it and the bridge is opened once more, so the session that follows is
// bridged from its first URL; boot's error is the attach's. Refused again
// after boot (a hub whose start returned before "running"), it is retried
// in the background. Without boot it is one line and no bridge.
//
// A bridge adopted late after a not-running refusal that nothing here
// booted (the hub-side attach did) brings this host's saved forwards for
// the machine up too, as the boot would have (lateHubForwards).
func (a *App) hubBridge(m *config.Machine, boot func() error) (release func(), err error) {
	release = func() {}
	if noSubscribe() {
		return release, nil
	}
	lost := "forwards still work; URLs opened in the guest will not open here"
	type opened struct {
		s   bridgeSub
		err error
	}
	var stopped atomic.Bool // an open was refused for a VM not running
	open := func() (bridgeSub, error) {
		s, err := a.openSubscribed(m)
		if vmNotRunning(err) {
			stopped.Store(true)
		}
		return s, err
	}
	booted := false
	// adopted runs once a late bridge is held: the forwards a boot here
	// would have brought up, unless one did.
	adopted := func() {
		if stopped.Load() && !booted {
			a.lateHubForwards(m)
		}
	}
	for {
		done := make(chan opened, 1)
		go func() {
			s, err := open()
			done <- opened{s, err}
		}()
		t := time.NewTimer(hubBridgeWait)
		select {
		case r := <-done:
			t.Stop()
			if r.err == nil {
				return func() { r.s.Close() }, nil
			}
			if vmNotRunning(r.err) && !booted {
				if boot != nil {
					if err := boot(); err != nil {
						return release, err
					}
					boot, booted = nil, true
					continue
				}
				a.notRunningNotice(m)
				return release, nil
			}
			if session.BridgeRefusedForGood(r.err) {
				fmt.Fprintf(a.Stderr, "devvm: browser bridge unavailable (%v); %s\n", r.err, lost)
				return release, nil
			}
			fmt.Fprintf(a.Stderr, "devvm: browser bridge to hub %s not up yet (%v); retrying in the background, URLs opened before it is up print in the guest\n", m.Hub.Name, r.err)
			late := newLateBridge()
			go func() {
				defer bridgeRetryEnded()
				if late.retry(open) {
					adopted()
				}
			}()
			return late.release, nil
		case <-t.C:
			late := newLateBridge()
			go func() {
				defer bridgeRetryEnded()
				// Its failure is not reported: the attach owns the terminal
				// by then, and the guest's shim prints the URL all the same.
				// A VM not running is retried too: the hub-side attach
				// starts it (bridgeRetryWaits).
				switch r := <-done; {
				case r.err == nil:
					late.adopt(r.s)
					adopted()
				case !noBridgeRetry(r.err):
					if late.retry(open) {
						adopted()
					}
				}
			}()
			fmt.Fprintf(a.Stderr, "devvm: browser bridge to hub %s not up yet; URLs opened before it is will print in the guest\n", m.Hub.Name)
			return late.release, nil
		}
	}
}

// lateHubForwards brings this host's saved forwards for a hub machine up
// behind a running attach: nothing on stdout (the session owns the
// terminal), a failure one warning line.
func (a *App) lateHubForwards(m *config.Machine) {
	q := *a
	q.Stdout = io.Discard
	if err := q.hubForwardsAfterStart(m.Name); err != nil {
		fmt.Fprintf(a.Stderr, "devvm: saved forwards for %s not brought up (%v); 'devvm ports up %s' retries\r\n", m.Name, err, m.Name)
	}
}

// openSubscribed opens a session on m's daemon and subscribes it (acked),
// closing it again when the subscribe fails.
func (a *App) openSubscribed(m *config.Machine) (bridgeSub, error) {
	s, err := openBridgeSession(a.Paths, m.Name, a.hostOpener())
	if err != nil {
		return nil, err
	}
	if err := s.Subscribe(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
