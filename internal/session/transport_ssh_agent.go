package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/smweber/devvm/internal/agentbin"
	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// sshAgent is a remote box's agent exec (browser-bridge.md §1, "Remote"):
// one `devvm-agent serve` on the daemon's own ControlMaster, carrying the
// guest's open-url events (and nothing else: forwards are native -L, and a
// remote box runs its own time daemon, so no clock sync). ssh has no
// one-exec limit; one exec per daemon is simply all the bridge needs, and
// the guest's single agent socket (`~/.devvm/agent.sock`) is its.
//
// Two failure modes, kept apart:
//
//   - The master dies: the exec dies with it. The supervisor finds the
//     master gone and marks the transport dead (sooner than monitor's
//     next check), and the daemon's reconnect builds a new transport,
//     agent included. Nothing here retries.
//   - The exec dies with the master alive (the agent killed, the guest's
//     agent replaced): the supervisor respawns it after a backoff of its
//     own (agentMinBackoff doubling to agentMaxBackoff, back to the minimum
//     after an exec that lived agentHealthy). The forwards never notice.
//
// While no exec is up, events() is the same channel with nothing feeding
// it, ping still reports the shim of an agent known to be current (it is
// installed and right; the guest's shim finds no agent socket answering
// and prints the URL there, as with no subscriber), and the daemon goes on
// serving forwards. An agent known to be missing or stale reports no shim
// and no events (hasEvents: the Bridge echo to a relay).
//
// Presence follows ownership. A managed box gets agentbin.Install, as smol
// does (root /usr/local/bin, no prompt). An adopt host is only probed
// (agentbin.Probe: this build's agent in ~/.local/bin, sha-matched, nothing
// written); without one the supervisor waits for a nudge (a kick, a new
// subscriber), which is how `auth`, having installed the agent with the
// user's consent, hands it to this daemon.
type sshAgent struct {
	name    string
	hooks   agentHooks
	evCh    chan bridgeRequest
	nudgeCh chan struct{} // nudge: try now (buffered 1)
	stop    <-chan struct{}
	ctx     context.Context // cancelled by close: kills a probe or install in flight
	cancel  context.CancelFunc
	// masterAlive and markDead are the transport's.
	masterAlive func() bool
	markDead    func()

	mu     sync.Mutex
	paths  agentbin.Paths // a current agent's; zero while none is known
	exec   *backend.Session
	mux    *yamux.Session
	closed bool
}

// agentHooks are how the supervisor gets an agent: prepare makes sure one is
// there (installing it only on a managed box) and says whether it is this
// build's; spawn starts `AGENT serve` on the daemon's master. Tests stand
// in for both.
type agentHooks struct {
	prepare func(ctx context.Context) (agentbin.Paths, agentbin.State, error)
	spawn   func(ctx context.Context, agentPath string) (*backend.Session, error)
}

// Agent supervision knobs, vars so tests can run them in milliseconds.
var (
	// agentFirstWait bounds how long the transport's dial waits for the
	// first agent attempt: a daemon's come-up (comeUpWait) leaves about 10s
	// past ssh's connect for it. Past it the attempt goes on behind a
	// transport that is already up; ping reports the shim once it lands.
	agentFirstWait = 8 * time.Second
	// agentPrepareTimeout bounds one probe-or-install (an 8 MiB copy over a
	// slow link included).
	agentPrepareTimeout = 2 * time.Minute
	agentMinBackoff     = time.Second
	agentMaxBackoff     = 30 * time.Second
	// agentSetupMaxBackoff caps the retries of a failed probe or install
	// (sudo refused before `setup`, an unsupported guest): slow, since what
	// fixes them is usually a person, whose next attach or kick nudges.
	agentSetupMaxBackoff = 5 * time.Minute
	// agentHealthy is how long an exec must live for its death to start
	// the respawn backoff over.
	agentHealthy = 60 * time.Second
)

// sshAgentHooks are the production hooks: everything on ob, the backend
// bound to the daemon's master (backend.MasterBinder).
func sshAgentHooks(m *config.Machine, ob backend.Backend) agentHooks {
	return agentHooks{
		prepare: func(ctx context.Context) (agentbin.Paths, agentbin.State, error) {
			if m.Managed() {
				p, err := agentbin.Install(ctx, ob, m, nil)
				if err != nil {
					return agentbin.Paths{}, agentbin.Missing, err
				}
				return p, agentbin.Current, nil
			}
			// An adopt host: never a write from a detached daemon.
			return agentbin.Probe(ctx, ob, m)
		},
		spawn: func(ctx context.Context, agentPath string) (*backend.Session, error) {
			// As the login user, raw (no login shell), so no banner corrupts
			// the yamux stream.
			return ob.Spawn(ctx, backend.ExecOpts{}, agentPath, "serve")
		},
	}
}

// startAgent starts the agent supervisor and waits, at most agentFirstWait,
// for its first attempt, so a daemon that comes up with an agent reports
// its shim on the first ping (thin `auth`, attach's check). A failure is
// the supervisor's to retry and log; it never fails the transport: the
// forwards do not need the agent.
func (t *sshTransport) startAgent(ctx context.Context, name string, h agentHooks) {
	actx, cancel := context.WithCancel(ctx)
	a := &sshAgent{
		name: name, hooks: h,
		evCh: make(chan bridgeRequest), nudgeCh: make(chan struct{}, 1),
		stop: t.stopCh, ctx: actx, cancel: cancel,
		masterAlive: t.masterAlive, markDead: t.markDead,
	}
	t.agent = a
	first := make(chan struct{})
	go a.supervise(first)
	select {
	case <-first:
	case <-time.After(agentFirstWait):
		transportLogf("%s: the agent is not up after %s; serving forwards meanwhile", name, agentFirstWait)
	}
}

// errAgentClosed refuses an exec spawned after the transport closed.
var errAgentClosed = errors.New("agent closed")

// supervise brings the agent up and keeps it up until the transport stops
// (sshAgent). first is closed after the first attempt.
func (a *sshAgent) supervise(first chan struct{}) {
	firstDone := func() {
		if first != nil {
			close(first)
			first = nil
		}
	}
	defer firstDone()
	execBackoff, setupBackoff := agentMinBackoff, agentMinBackoff
	lastMsg := ""
	logOnce := func(msg string) {
		if msg != lastMsg {
			lastMsg = msg
			transportLogf("%s: %s", a.name, msg)
		}
	}
	for {
		paths, st, err := a.prepare()
		if err == nil && st == agentbin.Current {
			err = a.spawn(paths)
		}
		firstDone()
		switch {
		case a.stopped():
			return
		case err != nil:
			// A transient failure is not evidence the agent is gone: a
			// shim already known stays reported.
			logOnce(fmt.Sprintf("agent: %v; no browser events until it is retried", err))
			if !a.wait(setupBackoff) {
				return
			}
			setupBackoff = min(2*setupBackoff, agentSetupMaxBackoff)
			continue
		case st != agentbin.Current:
			a.setPaths(agentbin.Paths{})
			what := "no devvm-agent"
			if st == agentbin.Stale {
				what = "another build's devvm-agent"
			}
			logOnce(fmt.Sprintf("%s in ~/.local/bin (an adopt host: never installed from here); no browser events until 'devvm auth %s' installs it", what, a.name))
			if !a.wait(0) {
				return
			}
			continue
		}
		setupBackoff, lastMsg = agentMinBackoff, ""
		started := time.Now()
		ended := a.currentMuxClosed()
	up:
		for {
			select {
			case <-ended:
				break up
			case <-a.nudgeCh: // up already: nothing to retry
			case <-a.stop:
				return
			}
		}
		a.dropExec()
		if a.stopped() {
			return
		}
		if !a.masterAlive() {
			// The master went and took the exec: the transport's dead()
			// path, now rather than at monitor's next check. The daemon's
			// reconnect brings a new master and a new agent.
			a.markDead()
			return
		}
		if time.Since(started) >= agentHealthy {
			execBackoff = agentMinBackoff
		}
		transportLogf("%s: the agent exec ended with the master up; respawning in %s", a.name, execBackoff)
		if !a.wait(execBackoff) {
			return
		}
		execBackoff = min(2*execBackoff, agentMaxBackoff)
	}
}

// prepare is one bounded probe-or-install.
func (a *sshAgent) prepare() (agentbin.Paths, agentbin.State, error) {
	ctx, cancel := context.WithTimeout(a.ctx, agentPrepareTimeout)
	defer cancel()
	return a.hooks.prepare(ctx)
}

// spawn starts the exec and serves its events on the transport's channel.
func (a *sshAgent) spawn(paths agentbin.Paths) error {
	s, err := a.hooks.spawn(a.ctx, paths.Agent)
	if err != nil {
		return fmt.Errorf("spawn %s serve: %w", paths.Agent, err)
	}
	mux, err := agentMux(s)
	if err != nil {
		return err
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		closeExec(s, mux)
		return errAgentClosed
	}
	a.paths, a.exec, a.mux = paths, s, mux
	a.mu.Unlock()
	go serveEvents(mux, a.evCh)
	return nil
}

func (a *sshAgent) currentMuxClosed() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mux == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return a.mux.CloseChan()
}

func (a *sshAgent) setPaths(p agentbin.Paths) {
	a.mu.Lock()
	a.paths = p
	a.mu.Unlock()
}

// dropExec forgets an exec that ended (its paths stay: the agent is still
// installed and current) and makes sure it is gone.
func (a *sshAgent) dropExec() {
	a.mu.Lock()
	s, mux := a.exec, a.mux
	a.exec, a.mux = nil, nil
	a.mu.Unlock()
	closeExec(s, mux)
}

// closeExec kills the exec, then closes the mux: yamux's Close waits for
// its receive loop, which ends only when a read fails, and agentrpc.Stdio
// never fails one on its own (the smol transport's Close has the same
// order for the same reason). Closing stdin too ends a stand-in exec's
// connection in tests.
func closeExec(s *backend.Session, mux *yamux.Session) {
	if s != nil {
		s.Close()
		if s.Stdin != nil {
			s.Stdin.Close()
		}
	}
	if mux != nil {
		mux.Close()
	}
}

func (a *sshAgent) stopped() bool {
	select {
	case <-a.stop:
		return true
	default:
		return false
	}
}

// wait sleeps d (0: until nudged), or until a nudge or a stop. false on a
// stop.
func (a *sshAgent) wait(d time.Duration) bool {
	var timer <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-a.stop:
		return false
	case <-a.nudgeCh:
		return true
	case <-timer:
		return true
	}
}

// close ends the agent for good: a probe or install in flight is
// cancelled, the exec is killed, and nothing is spawned after. The
// transport's stop channel is closed first, so the supervisor exits.
func (a *sshAgent) close() {
	a.cancel()
	a.mu.Lock()
	a.closed = true
	s, mux := a.exec, a.mux
	a.exec, a.mux = nil, nil
	a.mu.Unlock()
	closeExec(s, mux)
}

// shimPath is the shim beside a current agent (shimReporter), "" while none
// is known.
func (t *sshTransport) shimPath() string {
	if t.agent == nil {
		return ""
	}
	t.agent.mu.Lock()
	defer t.agent.mu.Unlock()
	return t.agent.paths.Shim
}

// hasEvents is whether a current agent is known (eventCarrier): what the
// daemon echoes to a relay as Bridge.
func (t *sshTransport) hasEvents() bool { return t.shimPath() != "" }

// nudge asks the supervisor to try now (nudger): an adopt host whose agent
// `auth` just installed, or an exec waiting out its respawn backoff.
// Non-blocking; a no-op while an exec is up.
func (t *sshTransport) nudge() {
	if t.agent == nil {
		return
	}
	select {
	case t.agent.nudgeCh <- struct{}{}:
	default:
	}
}

var (
	_ eventCarrier = (*sshTransport)(nil)
	_ shimReporter = (*sshTransport)(nil)
	_ nudger       = (*sshTransport)(nil)
)
