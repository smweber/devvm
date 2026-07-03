package backend

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/smweber/devvm/internal/config"
)

type sshBackend struct {
	m         *config.Machine
	configDir string
}

func (b *sshBackend) Kind() string { return b.m.Backend }

// sshFlags builds the shared port/identity/known_hosts options used by ssh,
// scp, and mosh (build_ssh_flags). ControlMaster options are added separately
// by base(), since scp/mosh don't want them.
func (b *sshBackend) sshFlags() []string {
	var f []string
	if b.m.SSHPort != 22 && b.m.SSHPort != 0 {
		f = append(f, "-o", fmt.Sprintf("Port=%d", b.m.SSHPort))
	}
	if b.m.Identity != "" {
		f = append(f, "-i", expandHome(b.m.Identity))
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
func (b *sshBackend) base() []string {
	ssh := append([]string{"ssh"}, b.sshFlags()...)
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
	host := b.base()
	if o.TTY {
		host = append(host, "-t")
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
// runs as root. Other users run as the login user.
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
	return exec.Command(args[0], args[1:]...).Run()
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

// Mosh connects via mosh, threading devvm's port/identity/known_hosts through
// mosh's --ssh so a non-default port or managed known_hosts still apply.
func (b *sshBackend) Mosh() error {
	if err := needCmd("mosh"); err != nil {
		return err
	}
	if err := RequireGuestTmux(b, b.m); err != nil {
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
	fmt.Fprintf(os.Stderr, "devvm: using %s; attaching tmux session 'dev'\n", server)
	// mosh-server runs without a login shell, so keep Homebrew's tmux/fish on
	// PATH or tmux's fish default-command exits at once.
	serverDir := filepath.Dir(server)
	remotePath := serverDir + ":/home/linuxbrew/.linuxbrew/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
	args := []string{
		"--ssh=" + sshCmd, "--server=" + server, "--", b.m.SSHHost,
		"env", "PATH=" + remotePath, "tmux", "new-session", "-A", "-s", "dev",
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

// VNC brings the tunnel up (via the supplied hook) and opens a viewer.
func (b *sshBackend) VNC(tunnelUp func() error) error {
	if err := tunnelUp(); err != nil {
		return err
	}
	port := b.m.VNCPort
	if runtime.GOOS == "darwin" {
		return exec.Command("open", fmt.Sprintf("vnc://localhost:%d", port)).Run()
	}
	if _, err := exec.LookPath("gvncviewer"); err == nil {
		return exec.Command("gvncviewer", fmt.Sprintf("localhost:%d", port-5900)).Run()
	}
	return fmt.Errorf("gvncviewer not found; install it or connect to localhost:%d", port)
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

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
