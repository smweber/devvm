package cli

import (
	"fmt"
	"os"

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

// noSubscribe reports whether DEVVM_NO_SUBSCRIBE is set: the hub proxy sets
// it on every proxied command so the laptop's subscription is the only one
// for that session (hub.md §8), and then attach/shell do not touch the
// daemon at all (no dial, no session, no subscription).
func noSubscribe() bool {
	v := os.Getenv(backend.NoSubscribeEnv)
	return v != "" && v != "0"
}

// bridgeSession opens the browser bridge for an interactive session on a
// local machine and returns what releases it. While it is held, a URL a
// guest program opens (the BROWSER shim, devvm-open-url) is opened here,
// the forward it needs is bound, and the guest is told what happened; the
// session client reconnects and re-subscribes on its own if the daemon is
// cycled under it (update, ports down/up). Best-effort: without it the
// shell still opens, and the guest's shim prints the URL instead.
//
// Only smol has events today (the agent exec); remote boxes wait for the
// ssh agent exec (bridge §1, "Remote", deferred), so they get no daemon
// here. A stopped VM gets none either: no daemon is spawned for one.
// Hub machines never come here: their attach/shell is proxied, and the
// laptop's subscription through the relay is roadmap step 8.
func (a *App) bridgeSession(m *config.Machine, b backend.Backend) (release func()) {
	release = func() {}
	if noSubscribe() || m.Backend != config.BackendSmol {
		return release
	}
	if requireRunningForForwards(m, b, 0) != nil {
		return release
	}
	s, err := openBridgeSession(a.ConfigDir, m.Name, session.SessionOptions{
		OnEvent: session.OpenOnHost(a.Stderr, hostbrowser.Open),
	})
	if err != nil {
		fmt.Fprintf(a.Stderr, "devvm: browser bridge unavailable (%v); URLs opened in the guest will be printed there\n", err)
		return release
	}
	if err := s.Subscribe(); err != nil {
		s.Close()
		fmt.Fprintf(a.Stderr, "devvm: browser bridge unavailable (%v); URLs opened in the guest will be printed there\n", err)
		return release
	}
	return func() { s.Close() }
}
