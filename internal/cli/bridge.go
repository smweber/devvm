package cli

import (
	"fmt"
	"os"
	"sync"
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
var openBridgeSession = func(configDir, name string, opts session.SessionOptions) (bridgeSub, error) {
	return session.OpenSession(configDir, name, opts)
}

// hubBridgeWait bounds how long a proxied attach/shell waits for its
// bridge: the laptop daemon's come-up (a relay to the hub, whose daemon may
// first exec into its VM) and the subscribe acked across the hop. Past it
// the session is proxied at once and the bridge keeps coming up in the
// background (lateBridge), so an unreachable or slow hub costs the attach
// at most this before its own ssh runs. A var for tests.
var hubBridgeWait = 5 * time.Second

// lateBridge holds a bridge that finished opening after the attach stopped
// waiting for it: adopted for the rest of the attach, closed by release,
// or closed at once when it arrives after release already ran.
type lateBridge struct {
	mu       sync.Mutex
	s        bridgeSub
	released bool
}

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
	l.s, l.released = nil, true
	l.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

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
var openAuthBridge = func(configDir, name string, opts session.SessionOptions) (auth.Bridge, error) {
	return session.OpenSession(configDir, name, opts)
}

// hostOpener is the OnEvent of every session this host subscribes: open
// the URL here, and print it on this terminal when no opener can.
func (a *App) hostOpener() session.SessionOptions {
	return session.SessionOptions{OnEvent: session.OpenOnHost(a.Stderr, hostbrowser.Open)}
}

// bridged reports whether attach/shell/auth hold a subscription for m: a
// local smol machine (its daemon's agent exec carries events), or a machine
// on a hub, through this host's daemon for HUB@NAME, whose relay subscribes
// on the hub daemon (hub.md §8). Remote boxes wait for the ssh agent exec
// (bridge §1, "Remote", deferred).
func bridged(m *config.Machine) bool {
	return m.Backend == config.BackendSmol || m.IsHubMachine()
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
// ahead and the bridge is adopted whenever it comes up. b is only asked whether a smol VM runs (a
// stopped one gets no daemon), so it may be nil for a hub machine.
func (a *App) bridgeSession(m *config.Machine, b backend.Backend) (release func()) {
	release = func() {}
	if noSubscribe() || !bridged(m) {
		return release
	}
	if m.Backend == config.BackendSmol && requireRunningForForwards(m, b, 0) != nil {
		return release
	}
	lost := "URLs opened in the guest will be printed there"
	if !m.IsHubMachine() {
		s, err := a.openSubscribed(m)
		if err != nil {
			fmt.Fprintf(a.Stderr, "devvm: browser bridge unavailable (%v); %s\n", err, lost)
			return release
		}
		return func() { s.Close() }
	}
	// A hub machine: the wait is bounded (hubBridgeWait), the setup is
	// not. A cold start (the first attach after a break, when a login is
	// most likely) pays for the laptop and hub daemons either way, so the
	// open finishes in the background and the late session is adopted for
	// the rest of the attach instead of being thrown away.
	lost = "forwards still work; URLs opened in the guest will not open here"
	type opened struct {
		s   bridgeSub
		err error
	}
	done := make(chan opened, 1)
	go func() {
		s, err := a.openSubscribed(m)
		done <- opened{s, err}
	}()
	t := time.NewTimer(hubBridgeWait)
	defer t.Stop()
	select {
	case r := <-done:
		if r.err != nil {
			fmt.Fprintf(a.Stderr, "devvm: browser bridge unavailable (%v); %s\n", r.err, lost)
			return release
		}
		return func() { r.s.Close() }
	case <-t.C:
		late := &lateBridge{}
		go func() {
			// Its failure is not reported: the attach owns the terminal by
			// then, and the guest's shim prints the URL all the same.
			if r := <-done; r.err == nil {
				late.adopt(r.s)
			}
		}()
		fmt.Fprintf(a.Stderr, "devvm: browser bridge to hub %s not up yet; URLs opened before it is will print in the guest\n", m.Hub.Name)
		return late.release
	}
}

// openSubscribed opens a session on m's daemon and subscribes it (acked),
// closing it again when the subscribe fails.
func (a *App) openSubscribed(m *config.Machine) (bridgeSub, error) {
	s, err := openBridgeSession(a.ConfigDir, m.Name, a.hostOpener())
	if err != nil {
		return nil, err
	}
	if err := s.Subscribe(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
