package cli

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

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
// shim prints the URL all the same.
func (l *lateBridge) retry(open func() (bridgeSub, error)) {
	for _, w := range bridgeRetryWaits {
		select {
		case <-l.stop:
			return
		case <-bridgeRetryAfter(w):
		}
		select {
		case <-l.stop:
			return
		default:
		}
		s, err := open()
		if err == nil {
			l.adopt(s)
			return
		}
		if noBridgeRetry(err) {
			return
		}
	}
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

// hostOpener is the OnEvent of every session this host subscribes: open
// the URL here, and print it on this terminal when no opener can.
func (a *App) hostOpener() session.SessionOptions {
	return session.SessionOptions{OnEvent: session.OpenOnHost(a.Stderr, hostbrowser.Open)}
}

// bridged reports whether attach/shell hold a subscription for m: a
// local smol VM (Managed: devvm made it, which every smol record is; its
// daemon's agent exec carries events), a remote box (its daemon runs the
// agent exec on its own ssh master: browser-bridge.md §1, "Remote"; on an
// entry only once the agent is there, which bridgeSession checks), or a
// machine on a hub, through this host's daemon for HUB@NAME, whose relay
// subscribes on the hub daemon (hub.md §8; the hub refuses a VM it did not
// create). A hub itself is not a machine.
func bridged(m *config.Machine) bool {
	switch {
	case m.IsHub():
		return false
	case m.IsHubMachine(), m.IsRemote():
		return true
	default:
		return m.Managed()
	}
}

// shimPinger is the session's ping (*session.Session), asked after a
// remote box's subscribe whether its daemon has an agent at all.
type shimPinger interface {
	Ping() (session.PingInfo, error)
}

// remoteWithoutAgent is attach/shell's check on a remote box's bridge: a
// daemon that reports no shim has no agent exec to hear the guest (an entry
// without agent = true, or an install that failed), so the session is
// let go with one line naming the way to get one, and the daemon idles out
// as if never asked, unless the open brought saved forwards up on it
// (pushSaved). A ping that fails says nothing either way: the
// subscription is kept. say false: the same check, silently (a late
// bridge, behind a session that owns the terminal).
func (a *App) remoteWithoutAgent(m *config.Machine, s bridgeSub, say bool) bool {
	p, ok := s.(shimPinger)
	if !m.IsRemote() || m.IsHubMachine() || !ok {
		return false
	}
	info, err := p.Ping()
	if err != nil || info.Shim != "" || info.State != session.StateUp {
		return false
	}
	// The daemon decided from the record it read at its start, which may
	// predate the file this record came from.
	restart := "'devvm ports down " + m.Name + "', and end any attach holding it"
	why := "its devvm agent did not come up (see the daemon log)"
	switch {
	case m.Managed():
	case m.Agent:
		why = "its devvm agent did not come up: its forward daemon may have started before agent = true was added (restart it: " + restart + "), or the install failed (see the daemon log)"
	default:
		why = "no current devvm agent there (agent = true in " + config.MachineTable(m.Name) + " lets devvm install one in ~/.local/bin once the forward daemon restarts: " + restart + ")"
	}
	if say {
		fmt.Fprintf(a.Stderr, "devvm: browser bridge unavailable: %s; URLs opened in the guest will be printed there\n", why)
	}
	return true
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
//
// On a remote box the session spawns the daemon (and its ssh master) if no
// forward holds one, for the life of the session: it idles out once the
// session ends with no forwards.
//
// The session's open also brings this host's saved forwards up behind it
// (pushSaved), whichever backend, so connecting restores them as `start`
// does; a machine that gets no bridge here (DEVVM_NO_SUBSCRIBE on a hub)
// gets no push either.
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
	if m.IsRemote() {
		return a.remoteBridge(m)
	}
	s, err := a.openSubscribed(m)
	if err != nil {
		fmt.Fprintf(a.Stderr, "devvm: browser bridge unavailable (%v); URLs opened in the guest will be printed there\n", err)
		return release
	}
	return func() { s.Close() }
}

// remoteBridgeWait bounds how long attach/shell on a remote box wait for
// the bridge before their own ssh runs. The bridge may have to spawn the
// daemon, whose first dial is a new ssh master (ConnectTimeout) plus the
// agent's first attempt, and the daemon is not up until that dial ends:
// unbounded, a box that does not answer cost the session the daemon's
// whole come-up wait (ConnectTimeout + 10s) before its own ssh failed the
// same way. Past it the session starts and the open finishes behind it
// (lateBridge), as a hub machine's does. A var for tests.
var remoteBridgeWait = 5 * time.Second

// remoteBridge is bridgeSession on a remote box: the open bounded by
// remoteBridgeWait, then adopted late if it lands (and only if its daemon
// has an agent: one reporting no shim is let go quietly, the session owns
// the terminal by then). A failure is not retried: the session's own ssh
// says whether the box answers, and the next attach opens again.
func (a *App) remoteBridge(m *config.Machine) (release func()) {
	type opened struct {
		s   bridgeSub
		err error
	}
	done := make(chan opened, 1)
	go func() {
		s, err := a.openSubscribed(m)
		done <- opened{s, err}
	}()
	t := time.NewTimer(remoteBridgeWait)
	defer t.Stop()
	select {
	case r := <-done:
		if r.err != nil {
			fmt.Fprintf(a.Stderr, "devvm: browser bridge unavailable (%v); URLs opened in the guest will be printed there\n", r.err)
			return func() {}
		}
		if a.remoteWithoutAgent(m, r.s, true) {
			r.s.Close()
			return func() {}
		}
		return func() { r.s.Close() }
	case <-t.C:
		late := newLateBridge()
		go func() {
			defer bridgeRetryEnded()
			r := <-done
			switch {
			case r.err != nil:
			case a.remoteWithoutAgent(m, r.s, false):
				r.s.Close()
			default:
				late.adopt(r.s)
			}
		}()
		fmt.Fprintf(a.Stderr, "devvm: browser bridge for %s not up yet; URLs opened before it is will print in the guest\n", m.Name)
		return late.release
	}
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
// Whenever an open reaches this host's daemon for the machine (in time,
// after a boot, late or on a retry), this host's saved forwards for it
// come up behind it (pushSaved; a retry's push again is harmless: the
// daemon reuses a forward it holds).
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
	open := func() (bridgeSub, error) { return a.openSubscribed(m) }
	booted := false
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
				late.retry(open)
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
				case !noBridgeRetry(r.err):
					late.retry(open)
				}
			}()
			fmt.Fprintf(a.Stderr, "devvm: browser bridge to hub %s not up yet; URLs opened before it is will print in the guest\n", m.Hub.Name)
			return late.release, nil
		}
	}
}

// openSubscribed opens a session on m's daemon and subscribes it (acked),
// closing it again when the subscribe fails. The saved forwards are
// pushed once the session is open, the subscribe aside: the daemon is up,
// whether or not its transport carries browser events.
func (a *App) openSubscribed(m *config.Machine) (bridgeSub, error) {
	s, err := openBridgeSession(a.Paths, m.Name, a.hostOpener())
	if err != nil {
		return nil, err
	}
	a.pushSaved(m)
	if err := s.Subscribe(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// pushSaved brings this host's saved forwards for m up behind an
// attach/shell whose session just opened m's daemon (simplify.md step 11),
// as `start` and `ports up` do: one Add per line, conf-owned, so they
// outlive the session as theirs do, and `ports down` holds only until the
// next connect. The daemon still never reads the file. Best-effort and
// quiet: the file is read here, the Adds run in the background, on a
// one-shot client of the daemon the open just brought up (Existing: it
// never spawns one; a daemon reconnecting records an Add as pending), and
// nothing waits on a VM's come-up as tunnelUpWait does, prints (the
// session owns the terminal) or fails the session. No saved forwards, no
// goroutine.
func (a *App) pushSaved(m *config.Machine) {
	saved, err := a.savedPorts(m.Name)
	if err != nil || len(saved) == 0 {
		return
	}
	paths, name := a.Paths, m.Name // copied: the goroutine reads nothing of a, which a later command may reuse
	go func() {
		defer savedPushed(name)
		cl, err := session.Existing(paths, name)
		if err != nil {
			return
		}
		for _, mapping := range saved {
			if pref, guest, err := parseMapping(mapping); err == nil {
				_, _, _, _ = cl.Add(pref, guest)
			}
		}
	}()
}

// savedPushed runs when a pushSaved's background Adds are over, whatever
// became of them; a hook for tests.
var savedPushed = func(name string) {}
