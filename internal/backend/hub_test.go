package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

func TestForHub(t *testing.T) {
	hub := config.NewHub("h", "u@host")
	b, err := For(hub, config.LegacyPaths(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	// The hub itself is reached like a remote box: same ssh plumbing, and
	// unmanaged, so the user's own known_hosts (no TOFU pin file).
	sb, ok := b.(*sshBackend)
	if !ok || b.Kind() != config.BackendHub {
		t.Fatalf("hub backend = %T kind %q, want *sshBackend/hub", b, b.Kind())
	}
	if flags := strings.Join(sb.sshFlags(), " "); strings.Contains(flags, "UserKnownHostsFile") {
		t.Errorf("hub must not get the managed known_hosts pin: %s", flags)
	}

	rec := &config.Machine{Name: "h/web", Hub: hub, Backend: config.BackendHub, SSHHost: hub.SSHHost}
	b, err = For(rec, config.LegacyPaths(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	hb, ok := b.(*hubBackend)
	if !ok || hb.hub.m != hub {
		t.Fatalf("hub-machine backend = %T, want *hubBackend over the hub conf", b)
	}
	if ok, err := hb.Exists(); !ok || err != nil {
		t.Errorf("Exists = %v, %v; resolve never asks the hub", ok, err)
	}
	if _, err := hb.Spawn(context.Background(), ExecOpts{}, "devvm-agent", "serve"); err == nil {
		t.Error("Spawn must refuse: the hub daemon owns the only agent exec")
	}
	for name, err := range map[string]error{
		"start":  hb.PowerStart(),
		"delete": hb.PowerDelete(),
		"copy":   hb.Copy("a", "b"),
	} {
		if !errors.Is(err, ErrHubProxy) {
			t.Errorf("%s = %v, want ErrHubProxy (the cli proxies these from the parsed command)", name, err)
		}
	}
}

// fakeSSHLog puts an `ssh` on PATH that records its argv (one line, joined
// by spaces) and exits 0.
func fakeSSHLog(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "ssh.log")
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// hubBackend.Run is a proxied `exec NAME -- argv…` under the login-shell
// wrapper with DEVVM_NO_SUBSCRIBE=1; root becomes a guest-side sudo (User
// would sudo the hub's ssh command instead) and guest Env becomes an `env`
// prefix in the guest argv (as ExecOpts it would land on the hub process).
func TestHubBackendRunIsProxiedExec(t *testing.T) {
	hub := config.NewHub("h", "u@host")
	rec := &config.Machine{Name: "h/web", Hub: hub, Backend: config.BackendHub, SSHHost: hub.SSHHost}
	b, err := For(rec, config.LegacyPaths(shortTempDir(t))) // a ControlPath ssh can bind
	if err != nil {
		t.Fatal(err)
	}
	log := fakeSSHLog(t)
	ctx := context.Background()
	if err := b.Run(ctx, ExecOpts{BatchMode: true, Stdin: strings.NewReader(""), Login: true}, "sh", "-c", "id; echo $X"); err != nil {
		t.Fatal(err)
	}
	if err := b.Run(ctx, ExecOpts{User: "root", Stdin: strings.NewReader("")}, "apt-get", "install", "-y", "python3"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("ssh calls = %q", lines)
	}
	wantRemote := remoteCommand(ExecOpts{}, LoginShellArgv(ProxyArgv("exec", "web", "--", "sh", "-c", "id; echo $X")...))
	if !strings.HasSuffix(lines[0], " u@host "+wantRemote) || !strings.Contains(lines[0], "BatchMode=yes") {
		t.Errorf("exec line = %q\nwant suffix %q", lines[0], wantRemote)
	}
	if strings.Contains(lines[0], " -t ") || strings.Contains(lines[0], " sudo ") {
		t.Errorf("non-tty, non-root exec got -t or sudo: %q", lines[0])
	}
	wantRoot := remoteCommand(ExecOpts{}, LoginShellArgv(ProxyArgv("exec", "web", "--", "sudo", "apt-get", "install", "-y", "python3")...))
	if !strings.HasSuffix(lines[1], " u@host "+wantRoot) {
		t.Errorf("root exec line = %q\nwant suffix %q", lines[1], wantRoot)
	}
	// Guest Env rides in the exec argv (roadmap step 8: BROWSER for a login
	// on a hub machine), never on the hub-side ssh process: keys sorted,
	// each K=V one token so a value with spaces or `$` survives.
	if err := b.Run(ctx, ExecOpts{TTY: true, Env: map[string]string{"BROWSER": "/usr/local/bin/devvm-open-url", "A_B": "x y $z"}}, "bash", "-lc", "gh auth login"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(log)
	lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	wantEnv := remoteCommand(ExecOpts{}, LoginShellArgv(ProxyArgv("exec", "web", "--", "env", "A_B=x y $z", "BROWSER=/usr/local/bin/devvm-open-url", "bash", "-lc", "gh auth login")...))
	if len(lines) != 3 || !strings.HasSuffix(lines[2], " u@host "+wantEnv) || !strings.Contains(lines[2], " -t ") {
		t.Errorf("env exec line = %q\nwant -t and suffix %q", lines[len(lines)-1], wantEnv)
	}
	if err := b.Run(ctx, ExecOpts{Env: map[string]string{"X=Y": "1"}}, "true"); err == nil {
		t.Error("an env name that is not a plain variable name must be refused")
	}
	// The wrapper's shape: env is the prefix, the assignment is a plain
	// token (quoted by shellJoin, which is why it is not a bare VAR=1 prefix).
	if got := ProxyArgv("stop", "web"); !slices.Equal(got, []string{"env", "DEVVM_NO_SUBSCRIBE=1", "devvm", "stop", "web"}) {
		t.Errorf("ProxyArgv = %q", got)
	}
}

// The version probe and (later) the proxy must run under the hub user's
// login shell, since only it has ~/.local/bin or Homebrew on PATH. Render the
// remote command exactly as ssh would hand it to the remote shell and run it
// through a real sh with a fake devvm reachable only via a profile, to prove
// the quoting survives both shells and -l is honoured.
func TestLoginShellArgvRunsUnderLoginShell(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\nprintf 'devvm version v1.2.3 [%s]\\n' \"$*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "devvm"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	// Only the login shell sees this PATH, as on the hubs.
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte("PATH="+bin+":$PATH\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := remoteCommand(ExecOpts{}, LoginShellArgv("devvm", "--version", "it's", "a b"))
	cmd := exec.Command("sh", "-c", remote) // what sshd does with the command string
	cmd.Env = append(os.Environ(), "HOME="+home, "SHELL=/bin/sh", "ENV=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if want := "devvm version v1.2.3 [--version it's a b]\n"; string(out) != want {
		t.Errorf("got %q, want %q (remote command: %s)", out, want, remote)
	}
	if !strings.Contains(remote, `-lc "$0"`) {
		t.Errorf("not a login shell invocation: %s", remote)
	}
}

// The pin file and the ControlMaster sockets are host state: they live in
// the state dir, never beside the confs.
func TestSSHStateFilesUnderStateDir(t *testing.T) {
	p := config.Paths{Config: t.TempDir(), State: t.TempDir()}
	b, err := For(config.NewRemote("cloud", true, "dev@cloud"), p)
	if err != nil {
		t.Fatal(err)
	}
	ssh := strings.Join(b.(*sshBackend).base(), " ")
	for _, want := range []string{"UserKnownHostsFile=" + p.KnownHostsPath(), "ControlPath=" + p.ControlPath()} {
		if !strings.Contains(ssh, want) {
			t.Errorf("ssh argv %q lacks %q", ssh, want)
		}
	}
	if strings.Contains(ssh, p.Config) {
		t.Errorf("ssh argv names the config dir: %q", ssh)
	}
}
