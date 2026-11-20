package bootstrap

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

func TestParseSpec(t *testing.T) {
	tests := []struct {
		in     string
		kind   string
		target string
		nargs  int
		err    bool
	}{
		{"none", KindNone, "", 0, false},
		{"url:https://x/b.sh --profile agent-vm --yes", KindURL, "https://x/b.sh", 3, false},
		{"cmd:/opt/setup.sh --fast", KindCmd, "/opt/setup.sh", 1, false},
		{"url:", "", "", 0, true},
		{"bogus:x", "", "", 0, true},
		{"noscheme", "", "", 0, true},
	}
	for _, tt := range tests {
		s, err := ParseSpec(tt.in)
		if (err != nil) != tt.err {
			t.Errorf("%q: err=%v wantErr=%v", tt.in, err, tt.err)
			continue
		}
		if tt.err {
			continue
		}
		if s.Kind != tt.kind || s.Target != tt.target || len(s.Args) != tt.nargs {
			t.Errorf("%q: got %+v", tt.in, s)
		}
	}
}

func TestSwitchUser(t *testing.T) {
	tests := []struct{ in, want string }{
		{"root@203.0.113.5", "dev@203.0.113.5"},
		{"ubuntu@box.example", "dev@box.example"},
		{"myalias", "dev@myalias"}, // user@ overrides an ssh-config alias's User
	}
	for _, tt := range tests {
		if got := SwitchUser(tt.in, "dev"); got != tt.want {
			t.Errorf("SwitchUser(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// fakeBackend records Run calls and answers `id -un` with a fixed login user.
type fakeBackend struct {
	loginUser string
	runs      [][]string
}

func (f *fakeBackend) Run(_ context.Context, o backend.ExecOpts, argv ...string) error {
	f.runs = append(f.runs, argv)
	if len(argv) == 2 && argv[0] == "id" && argv[1] == "-un" {
		fmt.Fprintln(o.Stdout, f.loginUser)
	}
	return nil
}

func (f *fakeBackend) Kind() string          { return config.BackendRemote }
func (f *fakeBackend) Exists() (bool, error) { return true, nil }
func (f *fakeBackend) PowerStart() error     { return nil }
func (f *fakeBackend) PowerStop() error      { return nil }
func (f *fakeBackend) PowerDelete() error    { return nil }
func (f *fakeBackend) Status() (backend.State, error) {
	return backend.State{}, nil
}
func (f *fakeBackend) Copy(string, string) error { return nil }
func (f *fakeBackend) Spawn(context.Context, backend.ExecOpts, ...string) (*backend.Session, error) {
	return nil, nil
}

// EnsureUser takes the template's user, not a hardcoded dev: the name
// rides as $1 into the script (never interpolated), with the backend's
// as-root opts.
func TestEnsureUser(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBackend{loginUser: "root"}
	if err := EnsureUser(ctx, fb, backend.ExecOpts{}, "alice"); err != nil {
		t.Fatal(err)
	}
	if len(fb.runs) != 1 {
		t.Fatalf("runs = %v, want the one setup script", fb.runs)
	}
	run := fb.runs[0]
	if run[0] != "bash" || run[len(run)-1] != "alice" || strings.Contains(run[2], "alice") ||
		strings.Contains(run[2], "/home/dev") || !strings.Contains(run[2], "NOPASSWD") {
		t.Errorf("run = %q", run)
	}
	for _, bad := range []string{"root", "Bob", "-x", "a b", ""} {
		if err := EnsureUser(ctx, fb, backend.ExecOpts{}, bad); err == nil {
			t.Errorf("EnsureUser(%q) accepted", bad)
		}
	}
}

func TestParseSpecDefault(t *testing.T) {
	// Empty spec falls back to the compiled default, which is now "none" (do
	// nothing) — no personal bootstrap URL is baked into the binary.
	s, err := ParseSpec("")
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != KindNone {
		t.Errorf("default spec = %+v, want kind %q", s, KindNone)
	}
	if config.DefaultBootstrapHook != KindNone {
		t.Errorf("compiled default provision = %q, want %q", config.DefaultBootstrapHook, KindNone)
	}
}

// Harden opens mosh's UDP range beside the ssh port (prereqs install
// mosh-server, and the firewall is default-deny), and every apt-get devvm
// runs itself waits for apt's locks: installs with the dpkg lock timeout,
// updates through apt_update's bounded retry (the timeout does not cover
// the lists lock). A fresh cloud VM runs apt-daily from boot.
func TestHardenFirewallAndAptLock(t *testing.T) {
	fb := &fakeBackend{loginUser: "dev"}
	m := config.NewRemote("cloud", true, "dev@cloud.example")
	m.SSHPort = 2222
	if err := Harden(context.Background(), fb, m, false); err != nil {
		t.Fatal(err)
	}
	script := fb.runs[len(fb.runs)-1][2]
	for _, want := range []string{"SSH_PORT=2222\n", `ufw allow "${SSH_PORT}/tcp"` + "\nufw allow 60000:61000/udp\nufw --force enable"} {
		if !strings.Contains(script, want) {
			t.Errorf("harden script lacks %q:\n%s", want, script)
		}
	}
	for name, s := range map[string]string{"harden": script, "ensureUser": ensureUserScript,
		"smolPrereqs": smolPrereqs, "managedRemotePrereqs": managedRemotePrereqs} {
		if strings.Count(s, aptUpdateFn) != 1 || !strings.Contains(s, "apt_update;") && !strings.Contains(s, "\napt_update\n") {
			t.Errorf("%s: apt_update not defined once and called", name)
		}
		// Outside apt_update's own update, every apt-get (several may share
		// a line) is an install with the lock timeout.
		rest := strings.Replace(s, aptUpdateFn, "", 1)
		for i := strings.Index(rest, "apt-get"); i >= 0; i = strings.Index(rest, "apt-get") {
			rest = rest[i:]
			if !strings.HasPrefix(rest, aptGet+" install ") {
				line, _, _ := strings.Cut(rest, "\n")
				t.Errorf("%s: apt-get without the lock timeout or retry: %q", name, line)
			}
			rest = rest[len("apt-get"):]
		}
	}
}
