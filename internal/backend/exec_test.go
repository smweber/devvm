package backend

import (
	"slices"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

// The quoting layer is the riskiest text logic carried over from bin/devvm:
// every remote exec funnels through it, so pin the rendered strings.

func TestPosixQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "'plain'"},
		{"has space", "'has space'"},
		{"", "''"},
		{"don't", `'don'\''t'`},
		{"$HOME `id` ;rm", "'$HOME `id` ;rm'"},
	}
	for _, tt := range tests {
		if got := posixQuote(tt.in); got != tt.want {
			t.Errorf("posixQuote(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestRemoteCommand(t *testing.T) {
	tests := []struct {
		name string
		o    ExecOpts
		argv []string
		want string
	}{
		{"bare", ExecOpts{}, []string{"true"}, "'true'"},
		{"args quoted", ExecOpts{}, []string{"echo", "a b"}, "'echo' 'a b'"},
		{"login wraps in bash -lc", ExecOpts{Login: true}, []string{"tmux", "new"},
			`bash -lc ''\''tmux'\'' '\''new'\'''`},
		{"env prefixes env(1)", ExecOpts{Env: map[string]string{"BROWSER": "/x/shim"}},
			[]string{"gh", "auth"}, "'env' 'BROWSER=/x/shim' 'gh' 'auth'"},
	}
	for _, tt := range tests {
		if got := remoteCommand(tt.o, tt.argv); got != tt.want {
			t.Errorf("%s: remoteCommand = %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestRootWrap(t *testing.T) {
	if got := rootWrap(ExecOpts{User: "root"}, []string{"apt", "update"}); got[0] != "sudo" || len(got) != 3 {
		t.Errorf("root exec should be sudo-prefixed, got %v", got)
	}
	if got := rootWrap(ExecOpts{}, []string{"ls"}); len(got) != 1 || got[0] != "ls" {
		t.Errorf("non-root exec should pass through, got %v", got)
	}
}

func TestSmolGuestArgv(t *testing.T) {
	b := &smolBackend{m: &config.Machine{Name: "box"}}
	// The hostname re-assert must come first (as root, before the sudo drop) —
	// smolvm resets the runtime hostname every boot, and a mismatch makes each
	// sudo print "unable to resolve host".
	hostnameWrap := []string{"sh", "-c", `hostname "$1" 2>/dev/null; shift; exec "$@"`, "_", "box"}
	tests := []struct {
		name string
		o    ExecOpts
		argv []string
		want []string
	}{
		{"dev user via sudo", ExecOpts{}, []string{"true"},
			append(slices.Clone(hostnameWrap), "sudo", "-u", "dev", "-H", "env", "SMOLVM_GUEST=1", "true")},
		{"root skips sudo", ExecOpts{User: "root"}, []string{"apt", "update"},
			append(slices.Clone(hostnameWrap), "env", "SMOLVM_GUEST=1", "apt", "update")},
		{"login wraps in bash -lc", ExecOpts{Login: true}, []string{"tmux", "new"},
			append(slices.Clone(hostnameWrap), "sudo", "-u", "dev", "-H", "env", "SMOLVM_GUEST=1",
				"bash", "-lc", `exec "$@"`, "_", "tmux", "new")},
	}
	for _, tt := range tests {
		if got := b.guestArgv(tt.o, tt.argv); !slices.Equal(got, tt.want) {
			t.Errorf("%s: guestArgv = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestEnvAssignments(t *testing.T) {
	got := envAssignments(map[string]string{"K": "v v"})
	if len(got) != 1 || got[0] != "K=v v" {
		t.Errorf("envAssignments = %v, want [K=v v]", got)
	}
	if envAssignments(nil) != nil {
		t.Error("nil env should yield nil")
	}
}
