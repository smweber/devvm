// Package auth orchestrates github/codex/claude logins inside a guest. It
// is thin everywhere it runs (browser-bridge.md §6): the machine's forward
// daemon bridges the host browser and OAuth loopback callbacks over its one
// agent exec (smol's, or a remote box's on the daemon's ssh master), and
// auth only reads the shim, subscribes, and runs the logins.
package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/smweber/devvm/internal/agentbin"
	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	fwdsession "github.com/smweber/devvm/internal/session"
)

// codexFixedPort is codex's well-known callback port. auth pre-binds it before
// `codex login` (no device auth), as an exact forward owned by its session,
// so the callback is bridged even if codex never hands its URL to the shim.
// Callback-port parsing itself is the daemon's (session.CallbackPort).
const codexFixedPort = 1455

// session is one auth run: the backend the logins run through, the guest
// path of the BROWSER shim they get, and ensure, which bridges a login's
// loopback callback port before the login starts (codex's fixed 1455).
type session struct {
	ctx    context.Context
	b      backend.Backend
	shim   string // guest $BROWSER wrapper path (co-located with the agent)
	ensure func(port int)
}

// installAgent and probeAgent are agentbin's, vars so a test can see what
// auth installs: only an agent a remote box's daemon reports missing, and
// on an adopt host only with consent. On smol the daemon's transport is the
// sole installer (browser-bridge.md §5, §6).
var (
	installAgent = agentbin.Install
	probeAgent   = agentbin.Probe
)

// agentPickupWait bounds how long auth waits, after installing the agent on
// a remote box and kicking its daemon, for that daemon to report the shim
// (its next probe, then its exec spawn). A var for tests.
var agentPickupWait = 15 * time.Second

// Bridge is thin auth's hold on the machine's forward daemon
// (browser-bridge.md §6): a session (*session.Session) whose events open on
// this host. Ping reads the BROWSER shim path, with the daemon's state and
// version to explain its absence; Subscribe is acked, so every URL a login
// opens after it reaches this host; Add binds a callback port owned by the
// session, gone when it closes.
type Bridge interface {
	Ping() (fwdsession.PingInfo, error)
	Subscribe() error
	Add(pref, guest int, exact bool) (host int, bumped, pending bool, err error)
	// Kick nudges the daemon's transport: after auth installs the agent on
	// a remote box, the daemon looks for it at once.
	Kick() error
	Close() error
}

// Options are what Run needs beyond the machine: the consent gate for an
// adopt host's agent install, and the way to the daemon.
type Options struct {
	// Approve gates the agent install on an adopt host: called only when
	// one would be written, with whether an agent is there but outdated
	// (another build's). nil: no consent needed (a managed box).
	Approve func(outdated bool) error
	// OpenBridge dials the machine's forward daemon (spawning it if needed)
	// and opens a session on it, with an OnEvent that opens URLs here.
	OpenBridge func() (Bridge, error)
	// NoSubscribe is DEVVM_NO_SUBSCRIBE: another host subscribed for this
	// run (the laptop proxying `auth HUB/NAME`, hub.md §8), so the daemon
	// is neither dialed nor subscribed; PingShim, if set, reads the shim
	// from a daemon that is already up, without holding it.
	NoSubscribe bool
	PingShim    func() (string, error)
	// Version is this devvm's build, to tell a daemon from before thin
	// auth (another version, no shim) from one that has none to report.
	Version string
}

// Thin reports whether auth on m takes the thin path (browser-bridge.md
// §6): every machine of this host's own, smol and remote alike, whose
// forward daemon carries events once it has an agent. A hub machine is
// thin too, but its auth runs on the hub (the cli proxies it with this
// host subscribed), so it never reaches Run here.
func Thin(m *config.Machine) bool { return !m.IsHubMachine() }

// Run logs in the requested tools on m over its forward daemon
// (authenticateThin). A hub machine is refused: the cli proxies its auth.
func Run(ctx context.Context, b backend.Backend, m *config.Machine, tools []string, o Options) error {
	if !Thin(m) {
		return fmt.Errorf("auth %s: a hub machine's auth runs on its hub", m.Name)
	}
	return authenticateThin(ctx, b, m, tools, o)
}

// authenticateThin is `auth` (browser-bridge.md §6): no agent exec of its
// own. The machine's daemon holds the one exec and, on smol and a managed
// remote box, installed the agent and shim before spawning it; a session on
// it reports the shim (ping) and, once subscribed, brings every URL a login
// opens to this host, binding its callback on the way. So `auth` during a
// held attach adds no second `devvm-agent serve` to the box.
//
// A remote box whose daemon reports no shim gets the agent from here
// (agentForDaemon) before anything subscribes: on an adopt host that is
// the only place devvm ever installs it, and only with consent.
func authenticateThin(ctx context.Context, b backend.Backend, m *config.Machine, tools []string, o Options) error {
	s := &session{ctx: ctx, b: b, ensure: func(int) {}}
	if o.NoSubscribe {
		// The browser is on the subscribing host, which bound what it
		// needs; nothing is pre-bound here, on the wrong host.
		if o.PingShim != nil {
			if shim, err := o.PingShim(); err == nil {
				s.shim = shim
			}
		}
		if s.shim == "" {
			fmt.Fprintf(os.Stderr, "devvm: no forward daemon for '%s' reports a browser shim; login URLs will be printed in the guest\n", m.Name)
		}
		return s.runLogins(tools)
	}
	br, err := o.OpenBridge()
	if err != nil {
		return fmt.Errorf("browser bridge for '%s': %w", m.Name, err)
	}
	defer br.Close()
	info, err := br.Ping()
	if err != nil {
		return fmt.Errorf("browser bridge for '%s': %w", m.Name, err)
	}
	if info.Shim == "" && m.IsRemote() && info.State == fwdsession.StateUp && info.Version == o.Version {
		if info, err = agentForDaemon(ctx, b, m, br, o); err != nil {
			return err
		}
	}
	if info.Shim == "" {
		return noShimError(m.Name, info, o.Version)
	}
	shim := info.Shim
	if err := br.Subscribe(); err != nil {
		return fmt.Errorf("browser bridge for '%s': %w", m.Name, err)
	}
	s.shim = shim
	s.ensure = func(port int) { BridgeCallback(br, port) }
	return s.runLogins(tools)
}

// agentForDaemon gives a remote box's daemon the agent it reports missing
// (browser-bridge.md §1, "Remote"): on an adopt host the daemon only ever
// probes, so the user-scoped install happens here, behind o.Approve (the
// --install-agent flag or the prompt), worded by what the probe found; on a
// managed box the daemon's own install failed or is still running, and
// this one, in the foreground, either lands it or says why. Then the daemon
// is kicked (its transport looks again at once) and asked until it reports
// the shim, bounded by agentPickupWait. Every probe and write here runs
// over this host's own ssh, never the daemon's exec.
func agentForDaemon(ctx context.Context, b backend.Backend, m *config.Machine, br Bridge, o Options) (fwdsession.PingInfo, error) {
	_, st, err := probeAgent(ctx, b, m)
	if err != nil {
		return fwdsession.PingInfo{}, fmt.Errorf("devvm-agent on '%s': %w", m.Name, err)
	}
	if st != agentbin.Current {
		if o.Approve != nil {
			if err := o.Approve(st == agentbin.Stale); err != nil {
				return fwdsession.PingInfo{}, err
			}
		}
		if _, err := installAgent(ctx, b, m, nil); err != nil {
			return fwdsession.PingInfo{}, fmt.Errorf("install devvm-agent on '%s': %w", m.Name, err)
		}
	}
	// One kick: a probe already in flight in the daemon keeps it queued
	// (the transport's nudge is buffered), so the daemon looks again after.
	if err := br.Kick(); err != nil {
		return fwdsession.PingInfo{}, fmt.Errorf("browser bridge for '%s': %w", m.Name, err)
	}
	deadline := time.Now().Add(agentPickupWait)
	for {
		info, err := br.Ping()
		if err != nil {
			return fwdsession.PingInfo{}, fmt.Errorf("browser bridge for '%s': %w", m.Name, err)
		}
		if info.Shim != "" || info.State != fwdsession.StateUp {
			return info, nil
		}
		if !time.Now().Before(deadline) {
			return fwdsession.PingInfo{}, fmt.Errorf("the forward daemon for '%s' did not pick up the devvm agent within %s; see its log, or restart it ('devvm ports down %s', and end any attach holding it) and retry", m.Name, agentPickupWait, m.Name)
		}
		select {
		case <-ctx.Done():
			return fwdsession.PingInfo{}, ctx.Err()
		case <-time.After(agentPickupPoll):
		}
	}
}

// agentPickupPoll is how often agentForDaemon asks again.
var agentPickupPoll = 250 * time.Millisecond

// noShimError explains a daemon that reports no shim, from the same ping:
// between transports (the shim comes back with the next one), another build
// (in practice one from before thin auth, or on a remote box one from
// before its agent exec), or neither. Nothing is installed from here.
func noShimError(name string, info fwdsession.PingInfo, version string) error {
	switch {
	case info.State != "" && info.State != fwdsession.StateUp:
		return fmt.Errorf("the forward daemon for '%s' is %s and reports no browser shim until its transport is back; retry in a moment", name, info.State)
	case info.Version != version:
		return fmt.Errorf("the forward daemon for '%s' runs another build of devvm (%s; this is %s) and reports no browser shim; restart it ('devvm ports down %s', and end any attach holding it) and retry", name, orUnknown(info.Version), version, name)
	default:
		return fmt.Errorf("the forward daemon for '%s' reports no browser shim; restart it ('devvm ports down %s', and end any attach holding it) and retry", name, name)
	}
}

func orUnknown(v string) string {
	if v == "" {
		return "(unknown)"
	}
	return v
}

// BridgeCallback binds a login's callback port on this host before the
// login runs, owned by the auth session (gone when it ends): codex without
// device auth may never hand its URL to the shim, so the redirect bind the
// daemon does on an open cannot be relied on. Exact, as every redirect
// callback: the provider sends the browser to that port verbatim.
func BridgeCallback(br Bridge, port int) {
	if _, _, pending, err := br.Add(port, port, true); err != nil {
		fmt.Fprintf(os.Stderr, "devvm: could not bridge login callback port %d: %v\n", port, err)
	} else if pending {
		fmt.Fprintf(os.Stderr, "devvm: login callback port %d is pending (host is reconnecting)\n", port)
	} else {
		fmt.Fprintf(os.Stderr, "devvm: bridging login callback on port %d\n", port)
	}
}

// CallbackPorts are the fixed callback ports the tools' logins need bound
// before they run (codex's 1455). A laptop proxying `auth HUB/NAME` binds
// them on its own session, the way thin auth does locally.
func CallbackPorts(tools []string) []int {
	for _, t := range tools {
		if t == "codex" {
			return []int{codexFixedPort}
		}
	}
	return nil
}

// runLogins runs each tool's login in order, the shared body of both paths.
func (s *session) runLogins(tools []string) error {
	for _, tool := range tools {
		// Skip tools whose CLI isn't installed — a user is unlikely to have
		// every service, so a missing one is graceful, not fatal.
		ok, err := s.installed(tool)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintf(os.Stderr, "devvm: %s not installed on the guest (no %q on PATH); skipping\n", tool, toolBinary(tool))
			continue
		}
		err = s.login(tool)
		if err == nil {
			continue
		}
		// Ctrl-C / signal exits abort the whole flow.
		if isSignalExit(err) {
			return err
		}
		fmt.Fprintf(os.Stderr, "devvm: %s login exited non-zero; continuing\n", tool)
	}
	return nil
}

// login runs one tool's interactive login with the browser shim wired in as
// $BROWSER. codex gets device-auth when available, else a 1455 callback forward.
func (s *session) login(tool string) error {
	switch tool {
	case "github":
		return s.interactive(`unset GH_TOKEN GITHUB_TOKEN; gh auth login && gh auth setup-git && gh config set git_protocol https --host github.com`)
	case "codex":
		if s.codexSupportsDeviceAuth() {
			return s.interactive(`exec codex login --device-auth`)
		}
		s.ensure(codexFixedPort) // route 1455 without restarting the VM
		return s.interactive(`exec codex login`)
	case "claude":
		fmt.Fprintln(os.Stderr, "Claude Code logs in on first launch; use /login if prompted.")
		return s.interactive(`exec claude`)
	default:
		return fmt.Errorf("tool must be %s, or all", strings.Join(Choices(), ", "))
	}
}

// interactive runs a login shell command with a tty and the browser shim env.
func (s *session) interactive(script string) error {
	o := backend.ExecOpts{TTY: true, Login: true}
	if s.shim != "" { // none known: leave the guest's own BROWSER alone
		o.Env = map[string]string{"BROWSER": s.shim}
	}
	return s.b.Run(s.ctx, o, "bash", "-lc", script)
}

// installed reports whether tool's guest CLI is on PATH in a login shell (so it
// sees the user's PATH additions — nvm, ~/.local/bin, etc.). A non-zero probe
// exit means "not found"; a cancelled context (Ctrl-C) aborts the whole flow.
func (s *session) installed(tool string) (bool, error) {
	bin := toolBinary(tool)
	err := s.b.Run(s.ctx, backend.ExecOpts{Login: true},
		"bash", "-lc", "command -v "+bin+" >/dev/null 2>&1")
	if err == nil {
		return true, nil
	}
	if s.ctx.Err() != nil {
		return false, err
	}
	return false, nil
}

func (s *session) codexSupportsDeviceAuth() bool {
	var buf bytes.Buffer
	err := s.b.Run(s.ctx, backend.ExecOpts{
		Login:  true,
		Stdout: &buf,
		Stderr: &buf,
	}, "bash", "-lc", "codex login --help 2>&1")
	return err == nil && strings.Contains(buf.String(), "--device-auth")
}

// loginTools lists the supported login selectors in the order `all` runs them,
// each paired with the guest CLI it drives. Adding a service is one entry here
// (plus its case in login).
var loginTools = []struct{ name, bin string }{
	{"github", "gh"},
	{"codex", "codex"},
	{"claude", "claude"},
}

// toolBinary returns the guest CLI backing a login selector, or "" if unknown.
func toolBinary(tool string) string {
	for _, t := range loginTools {
		if t.name == tool {
			return t.bin
		}
	}
	return ""
}

// Choices lists the valid tool selectors ("github", "codex", …), excluding the
// "all" meta-selector. Used for help text, completion, and error messages so the
// CLI stays in sync with loginTools.
func Choices() []string {
	names := make([]string, len(loginTools))
	for i, t := range loginTools {
		names[i] = t.name
	}
	return names
}

// Tools expands a tool selector into the ordered login list.
func Tools(sel string) ([]string, error) {
	if sel == "all" {
		return Choices(), nil
	}
	if toolBinary(sel) != "" {
		return []string{sel}, nil
	}
	return nil, fmt.Errorf("tool must be %s, or all", strings.Join(Choices(), ", "))
}

// isSignalExit reports whether a command exited due to a signal (e.g. Ctrl-C),
// which should abort the whole auth flow rather than continue to the next tool.
func isSignalExit(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return true
		}
		return ee.ExitCode() >= 128
	}
	return false
}
