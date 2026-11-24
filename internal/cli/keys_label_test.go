package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

// Commented keys pass through labelKeys untouched, options and comment
// preserved, so `keys add NAME ssh-ed25519 AAA... laptop` keeps its comment.
func TestLabelKeysCommentedPassThrough(t *testing.T) {
	lines := []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKcRCKRZai1DTb1XFzQXpaGyWXbcu1I6RsIsGqTNpO3Q laptop",
		`command="echo hi",no-agent-forwarding ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGWtNBZmSoOJ2FCbeZBUlA6DHDP7C5EJyLK6BhrBhQqQ locked-down`,
	}
	got, err := labelKeys(lines)
	if err != nil {
		t.Fatalf("labelKeys: %v", err)
	}
	for i := range lines {
		if got[i] != lines[i] {
			t.Errorf("line %d rewritten:\n got %s\nwant %s", i, got[i], lines[i])
		}
	}
}

// An uncommented key gets this host's short hostname, options kept; no
// prompt, no terminal.
func TestLabelKeysHostnameDefault(t *testing.T) {
	fakeHostname(t, "mac.local")
	got, err := labelKeys([]string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKcRCKRZai1DTb1XFzQXpaGyWXbcu1I6RsIsGqTNpO3Q",
		"no-pty,restrict ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGWtNBZmSoOJ2FCbeZBUlA6DHDP7C5EJyLK6BhrBhQqQ",
	})
	want := []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKcRCKRZai1DTb1XFzQXpaGyWXbcu1I6RsIsGqTNpO3Q mac",
		"no-pty,restrict ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGWtNBZmSoOJ2FCbeZBUlA6DHDP7C5EJyLK6BhrBhQqQ mac",
	}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("labelKeys = %q, %v; want %q", got, err, want)
	}
}

// github:USER fetches USER's keys (served here, never the network), each
// commented USER@github; the spec takes nothing after it.
func TestResolvePubkeysGithubSpec(t *testing.T) {
	const k = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKcRCKRZai1DTb1XFzQXpaGyWXbcu1I6RsIsGqTNpO3Q"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alice.keys" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintln(w, k)
	}))
	defer srv.Close()
	orig := githubBase
	githubBase = srv.URL
	t.Cleanup(func() { githubBase = orig })

	if got, err := resolvePubkeys([]string{"github:alice"}); err != nil || !slices.Equal(got, []string{k + " alice@github"}) {
		t.Errorf("github:alice = %q, %v", got, err)
	}
	if _, err := resolvePubkeys([]string{"github:bob"}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("github:bob (no such user) = %v", err)
	}
	for _, spec := range [][]string{{"github:"}, {"github:a/../b"}} {
		if _, err := resolvePubkeys(spec); err == nil || !strings.Contains(err.Error(), "invalid GitHub user") {
			t.Errorf("%q = %v, want invalid user", spec, err)
		}
	}
	if _, err := resolvePubkeys([]string{"github:alice", "extra"}); err == nil || !strings.Contains(err.Error(), "extra") {
		t.Errorf("github:alice extra = %v, want refused", err)
	}
}

// --from-github is retired: one line naming github:USER, before any
// resolve (the machine need not exist), in every form it was typed.
func TestKeysAddFromGithubRefused(t *testing.T) {
	a := newTestApp(t)
	for _, argv := range [][]string{
		{"keys", "add", "box", "--from-github", "alice"},
		{"keys", "add", "box", "--from-github=alice"},
		{"keys", "add", "--from-github", "alice", "box"},
		{"keys", "add", "box", "--from-github"},
	} {
		err := runTree(t, a, argv...)
		if err == nil || !strings.Contains(err.Error(), "github:USER") || strings.Contains(err.Error(), "\n") {
			t.Errorf("%q: err = %v, want one line naming github:USER", argv, err)
		}
	}
}

// No spec means this host's own ~/.ssh/id_*.pub, commented with its name
// when they carry none; with none there, a refusal naming the other specs.
func TestHostKeyLinesNoSpecIsLocalPubkeys(t *testing.T) {
	fakeHostname(t, "laptop")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := hostKeyLines(nil); err == nil || !strings.Contains(err.Error(), "id_*.pub") || !strings.Contains(err.Error(), "github:USER") {
		t.Errorf("no local keys: err = %v", err)
	}
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	const bare = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKcRCKRZai1DTb1XFzQXpaGyWXbcu1I6RsIsGqTNpO3Q"
	const named = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC1 me@laptop"
	for f, body := range map[string]string{"id_ed25519.pub": bare + "\n", "id_rsa.pub": named + "\n", "id_rsa": "PRIVATE\n"} {
		if err := os.WriteFile(filepath.Join(ssh, f), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := hostKeyLines(nil)
	if want := []string{bare + " laptop", named}; err != nil || !slices.Equal(got, want) {
		t.Errorf("hostKeyLines() = %q, %v; want %q", got, err, want)
	}
}

// A template's key file seeded at setup: an uncommented key gets this
// host's name as `keys add` gives it, # lines are skipped, and a key
// already there is not added twice.
func TestSeedAuthorizedKeysLabelsFileKeys(t *testing.T) {
	fakeHostname(t, "laptop.example")
	a := newTestApp(t)
	var w strings.Builder
	a.Stderr = &w
	home := t.TempDir()
	const k1 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKcRCKRZai1DTb1XFzQXpaGyWXbcu1I6RsIsGqTNpO3Q"
	const k2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGWtNBZmSoOJ2FCbeZBUlA6DHDP7C5EJyLK6BhrBhQqQ old"
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte(k2+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(src, []byte("# mine\n"+k1+"\n"+k2+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &localCopyBackend{home: home}
	if err := a.seedAuthorizedKeys(b, nil, []string{src}); err != nil {
		t.Fatalf("seed: %v (%s)", err, w.String())
	}
	data, _ := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if want := k2 + "\n" + k1 + " laptop\n"; string(data) != want {
		t.Errorf("authorized_keys = %q, want %q", data, want)
	}
}

func TestLabelKeysRejectsJunk(t *testing.T) {
	if _, err := labelKeys([]string{"Not Found"}); err == nil {
		t.Error("junk line should be rejected")
	}
}

func TestGithubKeysRejectsBadUser(t *testing.T) {
	// Validation must fire before any network I/O: these would change the
	// fetched github.com path.
	for _, u := range []string{"", "a/../b", "user/repo", "-leading", "x y"} {
		if _, err := githubKeys(u); err == nil || !strings.Contains(err.Error(), "invalid GitHub user") {
			t.Errorf("githubKeys(%q) should fail validation, got %v", u, err)
		}
	}
}

func TestParseMapping(t *testing.T) {
	tests := []struct {
		in          string
		pref, guest int
		ok          bool
	}{
		{"8080:3000", 8080, 3000, true},
		{"8080", 8080, 8080, true},
		{"0:80", 0, 0, false},
		{"80:0", 0, 0, false},
		{"65536", 0, 0, false},
		{"-1:80", 0, 0, false},
		{"http:80", 0, 0, false},
	}
	for _, tt := range tests {
		pref, guest, err := parseMapping(tt.in)
		if (err == nil) != tt.ok || pref != tt.pref || guest != tt.guest {
			t.Errorf("parseMapping(%q) = %d,%d,%v want %d,%d,ok=%v", tt.in, pref, guest, err, tt.pref, tt.guest, tt.ok)
		}
	}
}

// addGuestKeys counts only the keys new to the file: one already there
// (under another comment) and a repeat within the batch are not "added".
func TestAddGuestKeysCountsNewOnly(t *testing.T) {
	a := newTestApp(t)
	home := t.TempDir()
	const k1 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKcRCKRZai1DTb1XFzQXpaGyWXbcu1I6RsIsGqTNpO3Q"
	const k2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGWtNBZmSoOJ2FCbeZBUlA6DHDP7C5EJyLK6BhrBhQqQ"
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), []byte(k1+" old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &localCopyBackend{home: home}
	n, err := a.addGuestKeys(b, []string{k1 + " me@github", k2 + " me@github", k2 + " again"})
	if err != nil || n != 1 {
		t.Errorf("addGuestKeys = %d, %v; want 1 new", n, err)
	}
	data, _ := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if want := k1 + " old\n" + k2 + " me@github\n"; string(data) != want {
		t.Errorf("authorized_keys = %q, want %q", data, want)
	}
}

// keys rm protects a provider server's ssh_key_file key, which need not be
// named id_*: it is the box's login key. Only on a provider server, and a
// missing file or a broken devvm.toml adds nothing.
func TestHostProtectedPubsProviderKeyFile(t *testing.T) {
	a := newTestApp(t)
	t.Setenv("HOME", t.TempDir()) // no id_*.pub
	const k = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKcRCKRZai1DTb1XFzQXpaGyWXbcu1I6RsIsGqTNpO3Q work"
	keyFile := filepath.Join(t.TempDir(), "hetzner-login.pub")
	if err := os.WriteFile(keyFile, []byte(k+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDevvmTOML(t, a, fmt.Sprintf("[providers.hetzner]\nssh_key_file = %q\n", keyFile))
	srv := config.NewProviderMachine("web", config.BackendHetzner, config.ProviderServer{Address: "1.2.3.4"})
	if got := a.hostProtectedPubs(srv); !slices.Equal(got, []string{k}) {
		t.Errorf("provider server: protected = %q, want the ssh_key_file key", got)
	}
	entry := &config.Machine{Name: "box", Backend: config.BackendRemote, SSHHost: "box"}
	if got := a.hostProtectedPubs(entry); len(got) != 0 {
		t.Errorf("entry: protected = %q, want none (not a provider server)", got)
	}

	writeDevvmTOML(t, a, "[providers.hetzner]\nssh_key_file = \"/nonexistent/key.pub\"\n")
	if got := a.hostProtectedPubs(srv); len(got) != 0 {
		t.Errorf("missing ssh_key_file: protected = %q, want none", got)
	}
	writeDevvmTOML(t, a, "[providers.hetzner\n")
	if got := a.hostProtectedPubs(srv); len(got) != 0 {
		t.Errorf("broken devvm.toml: protected = %q, want none", got)
	}
}
