package bootstrap

import (
	"context"
	"fmt"
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

func (f *fakeBackend) Kind() string          { return config.BackendRemoteManaged }
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

func TestEnsureDevUser(t *testing.T) {
	ctx := context.Background()

	// Root login on remote-managed: creates the user and reports the switch.
	fb := &fakeBackend{loginUser: "root"}
	m := config.NewRemote("x", config.BackendRemoteManaged, "root@h")
	created, err := EnsureDevUser(ctx, fb, m)
	if err != nil || !created {
		t.Fatalf("root login: created=%v err=%v, want true, nil", created, err)
	}
	if len(fb.runs) != 2 || fb.runs[1][0] != "bash" {
		t.Fatalf("root login: runs = %v, want id -un then the bash setup script", fb.runs)
	}

	// Non-root login: probe only, no setup script.
	fb = &fakeBackend{loginUser: "dev"}
	created, err = EnsureDevUser(ctx, fb, m)
	if err != nil || created {
		t.Fatalf("dev login: created=%v err=%v, want false, nil", created, err)
	}
	if len(fb.runs) != 1 {
		t.Fatalf("dev login: runs = %v, want only the id -un probe", fb.runs)
	}

	// Unmanaged host: never even probed.
	fb = &fakeBackend{loginUser: "root"}
	um := config.NewRemote("y", config.BackendRemoteUnmanaged, "root@h")
	created, err = EnsureDevUser(ctx, fb, um)
	if err != nil || created || len(fb.runs) != 0 {
		t.Fatalf("unmanaged: created=%v err=%v runs=%v, want no action", created, err, fb.runs)
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
