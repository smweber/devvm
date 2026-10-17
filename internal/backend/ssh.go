package backend

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
	"github.com/smweber/devvm/internal/reach"
)

type sshBackend struct {
	m     *config.Machine
	paths config.Paths
}

func (b *sshBackend) Kind() string { return b.m.Backend }

// defaultSSHConnectTimeout bounds ssh's TCP connect and banner exchange.
const defaultSSHConnectTimeout = 10

var warnConnectTimeoutOnce sync.Once

// SSHConnectTimeout is the ConnectTimeout (whole seconds) for every ssh
// invocation. DEVVM_SSH_CONNECT_TIMEOUT overrides it, as bare seconds
// ("30") or a Go duration ("30s", "1m") — the same override pattern as
// DEVVM_COMPLETE_TIMEOUT. An unparseable value warns once and falls back,
// rather than silently timing out at the default.
func SSHConnectTimeout() int {
	v := os.Getenv("DEVVM_SSH_CONNECT_TIMEOUT")
	n, err := parseConnectTimeout(v)
	if err != nil {
		warnConnectTimeoutOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "devvm: ignoring DEVVM_SSH_CONNECT_TIMEOUT=%q: %v (using %ds)\n", v, err, defaultSSHConnectTimeout)
		})
		return defaultSSHConnectTimeout
	}
	return n
}

// SSHConnectTimeoutFor is SSHConnectTimeout for one box: the env override
// when set (it is this invocation's explicit word), else the entry's
// connect_timeout (a box behind a relayed or waking first hop, config.md
// §1), else the default.
func SSHConnectTimeoutFor(m *config.Machine) int {
	if os.Getenv("DEVVM_SSH_CONNECT_TIMEOUT") == "" && m != nil && m.ConnectTimeout > 0 {
		return m.ConnectTimeout
	}
	return SSHConnectTimeout()
}

// parseConnectTimeout accepts "", bare seconds, or a Go duration (rounded up
// to whole seconds, ssh's granularity).
func parseConnectTimeout(v string) (int, error) {
	if v == "" {
		return defaultSSHConnectTimeout, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("must be positive")
		}
		return n, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("not seconds or a duration")
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	secs := int((d + time.Second - 1) / time.Second)
	return secs, nil
}

// sshFlags builds the shared port/identity/known_hosts options used by ssh,
// scp, and mosh (build_ssh_flags). ControlMaster options are added separately
// by base(), since scp/mosh don't want them.
func (b *sshBackend) sshFlags() []string { return b.sshFlagsTimeout(SSHConnectTimeoutFor(b.m)) }

// sshFlagsTimeout is sshFlags with an explicit ConnectTimeout (seconds). It
// has to be the first -o ConnectTimeout on the line: ssh keeps the first
// value it sees for an option, so a later override would be ignored.
func (b *sshBackend) sshFlagsTimeout(connectSecs int) []string {
	// Fail fast on a host that is gone rather than sitting in the TCP timeout
	// (~75s on macOS, longer on Linux): the forward daemon's reconnect dial,
	// scp from the menu bar app, and status probes all need a bounded wait.
	// Established sessions are unaffected, but OpenSSH applies the limit to
	// the banner exchange too, so a host that is slow to answer can be given
	// longer via DEVVM_SSH_CONNECT_TIMEOUT (seconds), the same override
	// pattern as DEVVM_COMPLETE_TIMEOUT.
	f := []string{"-o", "ConnectTimeout=" + strconv.Itoa(connectSecs)}
	if b.m.SSHPort != 22 && b.m.SSHPort != 0 {
		f = append(f, "-o", fmt.Sprintf("Port=%d", b.m.SSHPort))
	}
	if b.m.Identity != "" {
		f = append(f, "-i", config.ExpandHome(b.m.Identity))
	}
	// Managed hosts get an isolated, TOFU-pinned known_hosts.
	// ssh records a new pin only if the file's directory exists, and the
	// state dir, unlike the config dir the pins used to share, may not yet.
	if b.m.Managed() {
		_ = os.MkdirAll(b.paths.State, 0o700)
		f = append(f,
			"-o", "UserKnownHostsFile="+b.paths.KnownHostsPath(),
			"-o", "StrictHostKeyChecking=accept-new")
	}
	return f
}

// base returns the ssh command with shared flags + a reused ControlMaster, so
// concurrent sessions (login + a URL watcher, say) share one connection.
func (b *sshBackend) base() []string { return b.baseTimeout(SSHConnectTimeoutFor(b.m)) }

func (b *sshBackend) baseTimeout(connectSecs int) []string {
	ssh := append([]string{"ssh"}, b.sshFlagsTimeout(connectSecs)...)
	_ = os.MkdirAll(b.paths.State, 0o700)
	// Keepalives as the daemon's master has them (session's startMaster):
	// without them a master outliving a laptop sleep keeps answering
	// `ssh -O check` until TCP's own keepalive gives up, and the reach
	// probe would read that stale master as `reachable`.
	ssh = append(ssh,
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+b.paths.ControlPath(),
		"-o", "ControlPersist=60",
		"-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=3")
	return ssh
}

// remoteCommand renders argv into a single string for ssh to run. Login wraps
// it in `bash -lc` (the double layer that survives the user's login shell,
// e.g. fish) exactly like ssh_wrap.
func remoteCommand(o ExecOpts, argv []string) string {
	toks := argv
	if len(o.Env) > 0 {
		toks = append(append([]string{"env"}, envAssignments(o.Env)...), argv...)
	}
	joined := shellJoin(toks)
	if o.Login {
		return "bash -lc " + posixQuote(joined)
	}
	return joined
}

func shellJoin(toks []string) string {
	q := make([]string, len(toks))
	for i, t := range toks {
		q[i] = posixQuote(t)
	}
	return strings.Join(q, " ")
}

func (b *sshBackend) Run(ctx context.Context, o ExecOpts, argv ...string) error {
	if err := needCmd("ssh"); err != nil {
		return err
	}
	if err := b.checkControlPaths(); err != nil {
		return err
	}
	connect := o.ConnectTimeout
	if connect <= 0 {
		connect = SSHConnectTimeoutFor(b.m)
	}
	host := b.baseTimeout(connect)
	if o.BatchMode {
		// RequestTTY=no with it: a BatchMode run is a non-interactive one
		// whose stdout may be a byte stream (the hub cp's tar), and a user's
		// ~/.ssh/config `RequestTTY force` would otherwise give it a pty whose
		// ONLCR mangles every \n. -t below still wins when asked for.
		host = append(host, "-o", "BatchMode=yes", "-o", "RequestTTY=no")
	}
	if o.TTY {
		host = append(host, "-t")
	}
	if o.Quiet {
		host = append(host, "-o", "LogLevel=ERROR")
	}
	host = append(host, b.m.SSHHost, remoteCommand(o, rootWrap(o, argv)))
	return runHost(ctx, o, host)
}

func (b *sshBackend) Spawn(ctx context.Context, o ExecOpts, argv ...string) (*Session, error) {
	if err := needCmd("ssh"); err != nil {
		return nil, err
	}
	if err := b.checkControlPaths(); err != nil {
		return nil, err
	}
	host := append(b.base(), b.m.SSHHost, remoteCommand(o, rootWrap(o, argv)))
	return spawnHost(ctx, host)
}

// sunPathMax is sizeof(sockaddr_un.sun_path): 108 on Linux, 104 on macOS
// and the BSDs.
func sunPathMax() int {
	if runtime.GOOS == "linux" {
		return 108
	}
	return 104
}

// controlPathErr refuses a ControlPath ssh could not bind, before ssh is
// run: ssh finds out only at connect time, as an exit 255 a bounded wait
// would otherwise retry for its whole budget. %C expands to a 40-hex hash,
// and a master binds the path plus "." and 16 random characters first.
func controlPathErr(path string) error {
	n := len(strings.ReplaceAll(path, "%C", strings.Repeat("0", 40)))
	if n+17 >= sunPathMax() {
		return fmt.Errorf("state dir path too long for ssh ControlPath (%d bytes); use a shorter --state-dir or XDG_STATE_HOME", n)
	}
	return nil
}

// checkControlPaths checks the master Run and Spawn share
// (paths.ControlPath). The daemon's own master is under run/, which is
// shorter than or equal to the state dir's.
func (b *sshBackend) checkControlPaths() error {
	return controlPathErr(b.paths.ControlPath())
}

// rootWrap prefixes sudo when the caller wants root: an ssh host's login user is
// the unprivileged dev user (with NOPASSWD sudo), unlike smol where exec already
// runs as root. Other users run as the login user. On a managed box that only
// had root (a fresh cloud VM), `devvm setup` (bootstrap.EnsureUser) establishes this
// invariant before anything runs with User: "root".
func rootWrap(o ExecOpts, argv []string) []string {
	if o.User == "root" {
		return append([]string{"sudo"}, argv...)
	}
	return argv
}

func (b *sshBackend) Copy(hostSrc, guestDst string) error {
	if err := needCmd("scp"); err != nil {
		return err
	}
	args := append([]string{"scp"}, b.sshFlags()...)
	args = append(args, hostSrc, scpTarget(b.m.SSHHost, guestDst))
	return quietHost(args) // scp's message rides the error, not a bare exit status
}

// scpTarget is scp's [user@]host:path for dest. ssh takes a bare IPv6
// address as its destination, but scp splits host from path at the first
// colon, so a v6 host (a provider box with ipv4 = false is reached at one)
// must be bracketed: dev@[2001:db8::1]:path. A host already bracketed, or
// with no colon (a name, an alias, IPv4), is left as is.
func scpTarget(dest, path string) string {
	user, host := "", dest
	if i := strings.LastIndex(dest, "@"); i >= 0 {
		user, host = dest[:i+1], dest[i+1:]
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return user + host + ":" + path
}

func (b *sshBackend) Exists() (bool, error) { return true, nil } // devvm doesn't create ssh hosts

// PowerStart/Stop are no-ops for ssh hosts: devvm doesn't manage their power.
func (b *sshBackend) PowerStart() error { return b.lifecycleNoop("start") }
func (b *sshBackend) PowerStop() error  { return b.lifecycleNoop("stop") }

// PowerDelete is a backend no-op; the cli removes the entry.
func (b *sshBackend) PowerDelete() error { return nil }

func (b *sshBackend) lifecycleNoop(action string) error {
	fmt.Fprintf(os.Stderr,
		"devvm: '%s' is a remote host; devvm does not manage its power ('%s' is a no-op).\n",
		b.m.Name, action)
	return nil
}

// Status probes a remote box (Probe, which also refreshes the reach cache)
// rather than claiming it reachable unasked: it is what `status NAME`
// shows, and a human asked. Running is "it answered". A hub's own conf is
// reached through this backend too, but its state is its listing pipe's
// (cli's hubRows), so it is not probed here.
//
// A provider server is not probed either: the API row it was resolved from
// carries its power state, which is the truer answer (a stopped server
// would only read as a failed connect).
func (b *sshBackend) Status() (State, error) {
	st := State{Name: b.m.Name, Backend: b.m.Backend, Exists: true, Raw: "ssh -> " + b.m.SSHHost}
	if b.m.IsHub() {
		st.Running = true
		return st, nil
	}
	if s := b.m.Server; s != nil {
		st.Running = s.State == provider.StateRunning
		st.Raw = fmt.Sprintf("%s (server %d, %s)", s.State, s.ID, s.Status)
		return st, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), ProbeBudget)
	defer cancel()
	e := b.Probe(ctx)
	st.Running = e.State == reach.Reachable
	st.Raw += ": " + e.Describe(time.Now())
	return st, nil
}

// Shell opens a raw interactive login shell over the chosen transport.
func (b *sshBackend) Shell(transport string) error { return b.connect(transport, nil) }

// Attach joins the persistent dev tmux session over the chosen transport.
func (b *sshBackend) Attach(transport string) error {
	if err := RequireGuestTmux(b, b.m); err != nil {
		return err
	}
	return b.connect(transport, []string{"tmux", "new-session", "-A", "-s", "dev"})
}

// connect runs remoteArgv interactively over the chosen transport (nil = the
// login shell). ssh -L forwards and exec always use ssh regardless; transport
// steers only this interactive session.
func (b *sshBackend) connect(transport string, remoteArgv []string) error {
	if transport == config.TransportMosh {
		return b.moshConnect(remoteArgv)
	}
	return b.sshConnect(remoteArgv)
}

func (b *sshBackend) sshConnect(remoteArgv []string) error {
	if err := needCmd("ssh"); err != nil {
		return err
	}
	host := append(b.base(), "-t", b.m.SSHHost)
	// No command -> ssh opens the login shell itself (honours fish/etc.).
	if len(remoteArgv) > 0 {
		host = append(host, remoteCommand(ExecOpts{Login: true}, remoteArgv))
	}
	return runHost(context.Background(), ExecOpts{TTY: true}, host)
}

// moshConnect connects via mosh, threading devvm's port/identity/known_hosts
// through mosh's --ssh so a non-default port or managed known_hosts still apply.
func (b *sshBackend) moshConnect(remoteArgv []string) error {
	if err := needCmd("mosh"); err != nil {
		return err
	}
	server, err := b.findMoshServer()
	if err != nil {
		return err
	}
	sshCmd := "ssh"
	if flags := b.sshFlags(); len(flags) > 0 {
		sshCmd = shellJoin(append([]string{"ssh"}, flags...))
	}
	fmt.Fprintf(os.Stderr, "devvm: using %s\n", server)
	args := []string{"--ssh=" + sshCmd, "--server=" + server, "--", b.m.SSHHost}
	if len(remoteArgv) > 0 {
		// mosh-server runs without a login shell, so keep Homebrew's tmux/fish on
		// PATH or tmux's fish default-command exits at once. (A bare login shell —
		// remoteArgv nil — needs none of this: mosh runs the user's login shell.)
		serverDir := filepath.Dir(server)
		remotePath := serverDir + ":/home/linuxbrew/.linuxbrew/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
		args = append(args, "env", "PATH="+remotePath)
		args = append(args, remoteArgv...)
	}
	cmd := exec.Command("mosh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// findMoshServer prefers a configured, still-executable path, else discovers
// mosh-server in the remote login environment (config may be shared by hosts
// with different Homebrew prefixes).
func (b *sshBackend) findMoshServer() (string, error) {
	ctx := context.Background()
	if b.m.MoshServer != "" {
		if b.Run(ctx, ExecOpts{Login: true, Stdout: io.Discard, Stderr: io.Discard}, "test", "-x", b.m.MoshServer) == nil {
			return b.m.MoshServer, nil
		}
	}
	discover := `command -v mosh-server 2>/dev/null ||
		for c in /home/linuxbrew/.linuxbrew/bin/mosh-server /opt/homebrew/bin/mosh-server \
			/usr/local/bin/mosh-server /usr/bin/mosh-server; do
			[ -x "$c" ] && { printf '%s\n' "$c"; exit 0; }
		done; exit 1`
	host := append(b.base(), b.m.SSHHost, "bash -lc "+posixQuote(discover))
	out, err := captureHost(ctx, host)
	if err != nil || out == "" {
		return "", fmt.Errorf("mosh-server was not found on '%s'", b.m.Name)
	}
	if b.m.MoshServer != "" && out != b.m.MoshServer {
		fmt.Fprintf(os.Stderr, "devvm: configured mosh-server not executable; using %s\n", out)
	}
	return out, nil
}

// SSHConn returns the parameters for a daemon-owned ControlMaster + native -L
// forwards (the session package drives these).
func (b *sshBackend) SSHConn() SSHConn {
	return SSHConn{
		Host:        b.m.SSHHost,
		Flags:       b.sshFlags(),
		ControlPath: filepath.Join(b.paths.RuntimeDir(), b.m.Name+".master"),
	}
}
