package backend

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

func TestParseConnectTimeout(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		bad  bool
	}{
		{in: "", want: defaultSSHConnectTimeout},
		{in: "30", want: 30},
		{in: "30s", want: 30},
		{in: "1m", want: 60},
		{in: "1500ms", want: 2}, // rounded up: ssh takes whole seconds
		{in: "0", bad: true},
		{in: "-5", bad: true},
		{in: "0s", bad: true},
		{in: "soon", bad: true},
	} {
		got, err := parseConnectTimeout(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("%q: want error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}

func TestSSHConnectTimeoutEnv(t *testing.T) {
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "45s")
	if got := SSHConnectTimeout(); got != 45 {
		t.Fatalf("got %d, want 45", got)
	}
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "later")
	if got := SSHConnectTimeout(); got != defaultSSHConnectTimeout {
		t.Fatalf("unparseable value gave %d, want the default %d", got, defaultSSHConnectTimeout)
	}
}

// scp splits host from path at the first colon, so a bare IPv6 host (a
// provider box with ipv4 = false) is bracketed; nothing else changes.
func TestSCPTargetBracketsIPv6(t *testing.T) {
	for _, tc := range []struct{ dest, want string }{
		{"dev@2001:db8::1", "dev@[2001:db8::1]:/var/tmp/x"},
		{"2001:db8::1", "[2001:db8::1]:/var/tmp/x"},
		{"dev@[2001:db8::1]", "dev@[2001:db8::1]:/var/tmp/x"},
		{"dev@192.0.2.1", "dev@192.0.2.1:/var/tmp/x"},
		{"alias", "alias:/var/tmp/x"},
	} {
		if got := scpTarget(tc.dest, "/var/tmp/x"); got != tc.want {
			t.Errorf("scpTarget(%q) = %q, want %q", tc.dest, got, tc.want)
		}
	}
}

// shortTempDir is a temp dir short enough for an ssh ControlPath under it
// (t.TempDir's names alone can push c-%C past sun_path).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "devvm-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir
}

// A ControlPath ssh could not bind (the master's path plus ".", 16
// characters, inside sun_path) is refused before ssh runs, naming its
// length; a normal state dir passes.
func TestControlPathTooLong(t *testing.T) {
	if err := controlPathErr("/home/dev/.local/state/devvm/c-%C"); err != nil {
		t.Fatalf("a normal state dir: %v", err)
	}
	long := "/" + strings.Repeat("x", 60) + "/c-%C" // 40-hex %C: 104 bytes
	err := controlPathErr(long)
	if err == nil || !strings.Contains(err.Error(), "state dir path too long for ssh ControlPath (104 bytes)") ||
		!strings.Contains(err.Error(), "--state-dir or XDG_STATE_HOME") {
		t.Fatalf("long: %v", err)
	}
	// Run refuses it without running ssh at all.
	dir := shortTempDir(t)
	log := filepath.Join(dir, "ssh.log")
	os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\necho ran >> "+log+"\n"), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := filepath.Join(dir, strings.Repeat("s", 80))
	b := &sshBackend{m: config.NewRemote("x", true, "dev@x.example"), paths: config.LegacyPaths(state)}
	if err := b.Run(context.Background(), ExecOpts{}, "true"); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("ssh ran")
	}
}

// OnMaster binds every exec and copy to the daemon's own ControlMaster:
// the agent exec (Spawn), agentbin's probes and install (Run, Copy) all
// name its ControlPath, never become a master themselves, never prompt,
// and never touch the CLI's shared c-%C master.
func TestOnMasterUsesDaemonControlPath(t *testing.T) {
	log := fakeSSHLog(t)
	dir := filepath.Dir(log)
	scpLog := filepath.Join(dir, "scp.log")
	if err := os.WriteFile(filepath.Join(dir, "scp"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+scpLog+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths := config.LegacyPaths(shortTempDir(t))
	b, err := For(config.NewRemote("box", true, "dev@box.example"), paths)
	if err != nil {
		t.Fatal(err)
	}
	master := filepath.Join(paths.RuntimeDir(), "box.master")
	mb, ok := b.(MasterBinder)
	if !ok {
		t.Fatal("the ssh backend is not a MasterBinder")
	}
	if conn := b.(SSHConnector).SSHConn(); conn.ControlPath != master {
		t.Fatalf("daemon master %q, want %q", conn.ControlPath, master)
	}
	ob := mb.OnMaster(master)
	s, err := ob.Spawn(context.Background(), ExecOpts{}, "/usr/local/bin/devvm-agent", "serve")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := ob.Run(context.Background(), ExecOpts{User: "root"}, "true"); err != nil {
		t.Fatal(err)
	}
	if err := ob.Copy("/dev/null", "/tmp/x"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	scp, _ := os.ReadFile(scpLog)
	lines := append(strings.Split(strings.TrimSpace(string(data)), "\n"), strings.TrimSpace(string(scp)))
	if len(lines) != 3 {
		t.Fatalf("calls:\n%s", strings.Join(lines, "\n"))
	}
	for _, l := range lines {
		for _, want := range []string{"ControlPath=" + master, "ControlMaster=no", "BatchMode=yes"} {
			if !strings.Contains(l, want) {
				t.Errorf("call %q lacks %s", l, want)
			}
		}
		if strings.Contains(l, paths.ControlPath()) || strings.Contains(l, "ControlPersist") || strings.Contains(l, "ControlMaster=auto") {
			t.Errorf("call %q rides or makes the CLI's master", l)
		}
	}
	if !strings.HasSuffix(lines[0], "dev@box.example '/usr/local/bin/devvm-agent' 'serve'") {
		t.Errorf("spawn = %q, want the agent run raw as the login user", lines[0])
	}
	if !strings.HasSuffix(lines[1], "'sudo' 'true'") {
		t.Errorf("root run = %q", lines[1])
	}
	// The backend it came from is unchanged: the CLI's own auto-master.
	if err := b.Run(context.Background(), ExecOpts{}, "true"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(log)
	all := strings.Split(strings.TrimSpace(string(data)), "\n")
	if last := all[len(all)-1]; !strings.Contains(last, "ControlPath="+paths.ControlPath()) || !strings.Contains(last, "ControlMaster=auto") {
		t.Errorf("plain Run = %q, want the CLI's c-%%C auto-master", last)
	}
}

// Remote attach puts BROWSER into the dev session's environment only when
// the shim beside this box's agent is executable (root's on a managed box,
// ~/.local/bin's on an adopt host), creating the session first when it is
// missing, then attaches. Run through a real sh with a fake tmux.
func TestRemoteAttachArgvSetsBrowserOnlyWithShim(t *testing.T) {
	for _, tc := range []struct {
		name    string
		managed bool
		shim    bool
		session bool
		want    string
	}{
		{"adopt, shim, new session", false, true, false,
			"has-session -t dev|new-session -d -s dev|set-environment -t dev BROWSER HOME/.local/bin/devvm-open-url|attach-session -t dev"},
		{"adopt, shim, existing session", false, true, true,
			"has-session -t dev|set-environment -t dev BROWSER HOME/.local/bin/devvm-open-url|attach-session -t dev"},
		{"adopt, no agent", false, false, true,
			"has-session -t dev|attach-session -t dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			bin := t.TempDir()
			log := filepath.Join(bin, "tmux.log")
			hasSession := "exit 1"
			if tc.session {
				hasSession = "exit 0"
			}
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n[ \"$1\" = has-session ] && " + hasSession + "\nexit 0\n"
			if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.shim {
				if err := os.MkdirAll(filepath.Join(home, ".local/bin"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, ".local/bin/devvm-open-url"), []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			argv := remoteAttachArgv(tc.managed)
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("attach script: %v\n%s", err, out)
			}
			data, _ := os.ReadFile(log)
			got := strings.ReplaceAll(strings.Join(strings.Split(strings.TrimSpace(string(data)), "\n"), "|"), home, "HOME")
			if got != tc.want {
				t.Errorf("tmux calls = %s\nwant          %s", got, tc.want)
			}
		})
	}
	// A managed box's shim is the root one, whatever its user's home (not
	// run: this host may have that very path).
	if argv := remoteAttachArgv(true); !strings.HasPrefix(argv[2], "s="+ManagedShimPath+";") || strings.Contains(argv[2], "HOME") {
		t.Errorf("managed attach does not use the root shim: %q", argv[2])
	}
}
