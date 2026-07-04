package auth

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/hashicorp/yamux"
	"github.com/smweber/devvm/internal/agentbin"
	"github.com/smweber/devvm/internal/agentrpc"
	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/hostbrowser"
)

// session holds the auth-scoped agent exec + yamux channel and any callback
// forwards stood up during login.
type session struct {
	ctx   context.Context
	b     backend.Backend
	agent *backend.Session
	mux   *yamux.Session
	shim  string // guest $BROWSER wrapper path (co-located with the agent)

	mu        sync.Mutex
	callbacks map[int][]net.Listener // guest callback port -> host listeners (v4 and/or v6)
}

// Authenticate logs in the requested tools (github/codex/claude) inside the
// guest, bridging the host browser and OAuth loopback callbacks over one agent
// session. tools is the resolved list (e.g. all -> github,codex,claude). approve
// gates the agent install on adopt hosts (see agentbin.Install).
func Authenticate(ctx context.Context, b backend.Backend, m *config.Machine, tools []string, approve func() error) error {
	agentPath, err := agentbin.Install(ctx, b, m, approve)
	if err != nil {
		return err
	}
	shim, err := installBrowserShim(ctx, b, m, agentPath)
	if err != nil {
		return err
	}
	// --auth: listen on the auth session's own guest socket, so this agent and
	// a forward daemon's agent never steal each other's URL socket.
	agent, err := b.Spawn(ctx, backend.ExecOpts{User: backend.DefaultUser},
		agentPath, "serve", "--auth")
	if err != nil {
		return err
	}
	defer agent.Close()
	mux, err := yamux.Client(agentrpc.Stdio{In: agent.Stdout, Out: agent.Stdin}, agentrpc.MuxConfig())
	if err != nil {
		return err
	}
	s := &session{ctx: ctx, b: b, agent: agent, mux: mux, shim: shim, callbacks: map[int][]net.Listener{}}
	defer s.close()

	go s.eventLoop()

	for i, tool := range tools {
		err := s.login(tool)
		if err == nil {
			continue
		}
		// Ctrl-C / signal exits abort the whole flow.
		if isSignalExit(err) {
			return err
		}
		if tool == "codex" {
			fmt.Fprintln(os.Stderr, "devvm: codex login exited non-zero; stopping")
			return err
		}
		fmt.Fprintf(os.Stderr, "devvm: %s login exited non-zero; continuing\n", tool)
		_ = i
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
	br := newLineReader(stream)
	header, err := br.header()
	if err != nil || header != agentrpc.TypeEvent {
		return
	}
	ev, err := br.event()
	if err != nil {
		return
	}
	if ev.Type == agentrpc.EventOpenURL {
		s.onOpenURL(ev.URL)
	}
}

func (s *session) onOpenURL(url string) {
	if port, ok := CallbackPort(url); ok {
		s.ensureCallback(port)
	}
	fmt.Fprintf(os.Stderr, "devvm: opening on host -> %s\n", url)
	hostbrowser.Open(url)
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
		s.ensureCallback(codexFixedPort) // route 1455 without restarting the VM
		return s.interactive(`exec codex login`)
	case "claude":
		fmt.Fprintln(os.Stderr, "Claude Code logs in on first launch; use /login if prompted.")
		return s.interactive(`exec claude`)
	default:
		return fmt.Errorf("tool must be github, codex, claude, or all")
	}
}

// interactive runs a login shell command with a tty and the browser shim env.
func (s *session) interactive(script string) error {
	return s.b.Run(s.ctx, backend.ExecOpts{
		TTY:   true,
		Login: true,
		Env:   map[string]string{"BROWSER": s.shim},
	}, "bash", "-lc", script)
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

// installBrowserShim writes the $BROWSER wrapper (a tiny script that forwards
// URLs to the serve agent) next to the agent, and returns its path. It follows
// the agent's ownership: root-owned beside a managed /usr/local/bin agent, or
// user-owned in ~/.local/bin on an adopt host.
func installBrowserShim(ctx context.Context, b backend.Backend, m *config.Machine, agentPath string) (string, error) {
	shimPath := path.Join(path.Dir(agentPath), "devvm-open-url")
	script := fmt.Sprintf(
		`printf '#!/bin/sh\nexec %s open-url "$@"\n' > %s && chmod 0755 %s`,
		agentPath, shimPath, shimPath)
	user := ""
	if m.Managed() {
		user = "root"
	}
	return shimPath, b.Run(ctx, backend.ExecOpts{User: user}, "sh", "-c", script)
}

// Tools expands a tool selector into the ordered login list.
func Tools(sel string) ([]string, error) {
	switch sel {
	case "all":
		return []string{"github", "codex", "claude"}, nil
	case "github", "codex", "claude":
		return []string{sel}, nil
	default:
		return nil, fmt.Errorf("tool must be github, codex, claude, or all")
	}
}
