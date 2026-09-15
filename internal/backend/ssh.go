package backend

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/smweber/devvm/internal/config"
)

type sshBackend struct {
	m         *config.Machine
	configDir string
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
func (b *sshBackend) sshFlags() []string { return b.sshFlagsTimeout(SSHConnectTimeout()) }

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
	if b.m.Managed() {
		f = append(f,
			"-o", "UserKnownHostsFile="+config.KnownHostsPath(b.configDir),
			"-o", "StrictHostKeyChecking=accept-new")
	}
	return f
}

// base returns the ssh command with shared flags + a reused ControlMaster, so
// concurrent sessions (login + a URL watcher, say) share one connection.
func (b *sshBackend) base() []string { return b.baseTimeout(SSHConnectTimeout()) }

func (b *sshBackend) baseTimeout(connectSecs int) []string {
	ssh := append([]string{"ssh"}, b.sshFlagsTimeout(connectSecs)...)
	_ = os.MkdirAll(b.configDir, 0o755)
	ssh = append(ssh,
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+filepath.Join(b.configDir, "cm-%C"),
		"-o", "ControlPersist=60")
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
	connect := o.ConnectTimeout
	if connect <= 0 {
		connect = SSHConnectTimeout()
	}
	host := b.baseTimeout(connect)
	if o.BatchMode {
		host = append(host, "-o", "BatchMode=yes")
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
	host := append(b.base(), b.m.SSHHost, remoteCommand(o, rootWrap(o, argv)))
	return spawnHost(ctx, host)
}

// rootWrap prefixes sudo when the caller wants root: an ssh host's login user is
// the unprivileged dev user (with NOPASSWD sudo), unlike smol where exec already
// runs as root. Other users run as the login user. On a managed box that only
// had root (a fresh cloud VM), bootstrap.EnsureDevUser establishes this
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
	args = append(args, hostSrc, b.m.SSHHost+":"+guestDst)
	return quietHost(args) // scp's message rides the error, not a bare exit status
}

func (b *sshBackend) Exists() (bool, error) { return true, nil } // devvm doesn't create ssh hosts

// PowerStart/Stop are no-ops for ssh hosts: devvm doesn't manage their power.
func (b *sshBackend) PowerStart() error { return b.lifecycleNoop("start") }
func (b *sshBackend) PowerStop() error  { return b.lifecycleNoop("stop") }

// PowerDelete is a backend no-op; the cli removes the registry entry.
func (b *sshBackend) PowerDelete() error { return nil }

func (b *sshBackend) lifecycleNoop(action string) error {
	fmt.Fprintf(os.Stderr,
		"devvm: '%s' is a remote host; devvm does not manage its power ('%s' is a no-op).\n",
		b.m.Name, action)
	return nil
}

func (b *sshBackend) Status() (State, error) {
	return State{
		Name:    b.m.Name,
		Backend: b.m.Backend,
		Exists:  true,
		Running: true,
		Raw:     "ssh -> " + b.m.SSHHost,
	}, nil
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
		ControlPath: filepath.Join(config.RuntimeDir(b.configDir), b.m.Name+".master"),
	}
}
