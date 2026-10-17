package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/hashicorp/yamux"
	"github.com/smweber/devvm/internal/agentbin"
	"github.com/smweber/devvm/internal/agentrpc"
	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/hostbrowser"
	fwdsession "github.com/smweber/devvm/internal/session"
)

// session is one auth run: the backend the logins run through, the guest
// path of the BROWSER shim they get, and ensure, which bridges a login's
// loopback callback port before the login starts (codex's fixed 1455).
// On a remote backend it also holds the private agent exec, its yamux
// channel and the callback listeners stood up over it; the thin path (smol)
// has none of those: its daemon owns the one exec and binds callbacks.
type session struct {
	ctx    context.Context
	b      backend.Backend
	shim   string // guest $BROWSER wrapper path (co-located with the agent)
	ensure func(port int)

	// The private session (remote backends only).
	agent *backend.Session
	mux   *yamux.Session

	mu        sync.Mutex
	callbacks map[int][]net.Listener // guest callback port -> host listeners (v4 and/or v6)
}

// installAgent is agentbin.Install, a var so a test can prove the thin path
// never calls it: on smol the daemon's transport is the sole installer
// (browser-bridge.md §5, §6).
var installAgent = agentbin.Install

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
	Close() error
}

// Options are what Run needs beyond the machine: the consent gate for the
// remote branch's install, and the thin branch's way to the daemon.
type Options struct {
	// Approve gates the agent install on an adopt host (agentbin.Install);
	// remote branch only.
	Approve func() error
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

// Thin reports whether auth on m takes the thin path (browser-bridge.md §6):
// the machine's forward daemon has events, which today means smol. A hub
// machine is thin too, but its auth runs on the hub (the cli proxies it
// with this host subscribed), so it never reaches Run here. Remote boxes
// keep the private session until the ssh agent exec ships.
func Thin(m *config.Machine) bool { return !m.IsRemote() }

// Run logs in the requested tools on m: the thin path where the daemon has
// events, the private session everywhere else.
func Run(ctx context.Context, b backend.Backend, m *config.Machine, tools []string, o Options) error {
	if !Thin(m) {
		return Authenticate(ctx, b, m, tools, o.Approve)
	}
	return authenticateThin(ctx, b, m, tools, o)
}

// authenticateThin is `auth` on smol (browser-bridge.md §6): no agent exec
// of its own and no install. The machine's daemon holds the one exec and
// installed the agent and shim before spawning it; a session on it reports
// the shim (ping) and, once subscribed, brings every URL a login opens to
// this host, binding its callback on the way. So `auth` during a held
// attach adds no second `devvm-agent serve` to the VM (the one-exec rule).
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

// noShimError explains a daemon that reports no shim, from the same ping:
// between transports (the shim comes back with the next one), another build
// (in practice one from before thin auth: it has the shim, step 7 installs
// it, but does not say where; a newer one would report it), or neither. Nothing is installed from here.
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

// Authenticate logs in the requested tools (github/codex/claude) inside the
// guest over a private agent session: the remote branch (Run), until the
// ssh agent exec ships. It bridges the host browser and OAuth loopback
// callbacks itself. tools is the resolved list (e.g. all ->
// github,codex,claude). approve gates the agent install on adopt hosts
// (see agentbin.Install).
func Authenticate(ctx context.Context, b backend.Backend, m *config.Machine, tools []string, approve func() error) error {
	// Agent and BROWSER shim, one install (agentbin.Install).
	paths, err := installAgent(ctx, b, m, approve)
	if err != nil {
		return err
	}
	shim := paths.Shim
	// --auth: listen on the auth session's own guest socket, so this agent and
	// a forward daemon's agent never steal each other's URL socket.
	agent, err := b.Spawn(ctx, backend.ExecOpts{User: backend.DefaultUser},
		paths.Agent, "serve", "--auth")
	if err != nil {
		return err
	}
	defer agent.Close()
	mux, err := yamux.Client(agentrpc.Stdio{In: agent.Stdout, Out: agent.Stdin}, agentrpc.MuxConfig())
	if err != nil {
		return err
	}
	s := &session{ctx: ctx, b: b, agent: agent, mux: mux, shim: shim, callbacks: map[int][]net.Listener{}}
	s.ensure = s.ensureCallback
	defer s.close()

	go s.eventLoop()
	return s.runLogins(tools)
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

// eventLoop consumes agent-pushed streams (open-url events) for the auth
// session's lifetime.
func (s *session) eventLoop() {
	for {
		stream, err := s.mux.Accept()
		if err != nil {
			return
		}
		go s.handleStream(stream)
	}
}

func (s *session) handleStream(stream net.Conn) {
	defer stream.Close()
	ev, err := agentrpc.ReadEvent(stream)
	if err != nil || ev.Type != agentrpc.EventOpenURL {
		return
	}
	// The agent waits for the answer and the shim prints it in the guest
	// (browser-bridge.md §2).
	_ = agentrpc.WriteReply(stream, s.onOpenURL(ev.URL))
}

// onOpenURL bridges a callback port (session.CallbackPort, which no longer
// skips codex's 1455: ensureCallback is idempotent, so the pre-bound port
// is simply reused) and opens the URL here.
func (s *session) onOpenURL(url string) agentrpc.OpenReply {
	if port, ok := fwdsession.CallbackPort(url); ok {
		s.ensureCallback(port)
	}
	fmt.Fprintf(os.Stderr, "devvm: opening on host -> %s\n", url)
	if err := hostbrowser.Open(url); err != nil {
		why := err.Error()
		if errors.Is(err, hostbrowser.ErrNoOpener) {
			why = "no browser opener"
		}
		if errors.Is(err, hostbrowser.ErrRefused) {
			fmt.Fprintf(os.Stderr, "devvm: %s\n", why)
			return agentrpc.OpenReply{Err: why}
		}
		fmt.Fprintf(os.Stderr, "devvm: could not open a browser (%s); open this URL in your browser: %s\n", why, url)
		return agentrpc.OpenReply{Err: why, URL: url, Printed: true}
	}
	return agentrpc.OpenReply{Opened: true, URL: url}
}

// ensureCallback binds the host loopback (both families, for macOS localhost->::1)
// on the callback port and pipes each connection to the guest's 127.0.0.1:port
// over a forward stream — the callback-as-forward that replaces nc + curl replay.
func (s *session) ensureCallback(port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.callbacks[port]; ok {
		return
	}
	var lns []net.Listener
	for _, addr := range []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port)} {
		if ln, err := net.Listen("tcp", addr); err == nil {
			lns = append(lns, ln)
			go s.serveForward(ln, port)
		}
	}
	if len(lns) > 0 {
		s.callbacks[port] = lns
		fmt.Fprintf(os.Stderr, "devvm: bridging login callback on port %d\n", port)
	}
}

func (s *session) serveForward(ln net.Listener, guestPort int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.pump(conn, guestPort)
	}
}

func (s *session) pump(conn net.Conn, guestPort int) {
	stream, err := s.mux.Open()
	if err != nil {
		conn.Close()
		return
	}
	target := fmt.Sprintf("127.0.0.1:%d", guestPort)
	if err := agentrpc.WriteHeader(stream, agentrpc.ForwardHeader(target)); err != nil {
		stream.Close()
		conn.Close()
		return
	}
	agentrpc.Splice(conn, stream)
}

func (s *session) close() {
	s.mu.Lock()
	for _, lns := range s.callbacks {
		for _, ln := range lns {
			ln.Close()
		}
	}
	s.mu.Unlock()
	s.mux.Close()
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
