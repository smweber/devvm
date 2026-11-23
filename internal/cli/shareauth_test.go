package cli

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shareToken is the fake host token: every test asserts where it goes.
const shareToken = "gho_FAKEshare0123456789token"

// hostGHFake stands in for this host's gh: token answers `auth token`
// (err when tokenErr), status answers `auth status --json hosts`. It
// returns the argv of every call.
func hostGHFake(t *testing.T, token string, tokenErr error, status string) *[][]string {
	t.Helper()
	var calls [][]string
	orig := hostGH
	hostGH = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		switch strings.Join(args, " ") {
		case "auth token --hostname github.com":
			return []byte(token + "\n"), tokenErr
		case "auth status --json hosts":
			if status == "" { // a gh too old for --json
				return nil, errors.New("exit status 1: unknown flag: --json")
			}
			return []byte(status), nil
		}
		return nil, errors.New("unexpected host gh call")
	}
	t.Cleanup(func() { hostGH = orig })
	fakeHostname(t, "laptop")
	return &calls
}

// notExpiring is gh's status for a login that does not expire: gh tags
// the field `refreshable,omitempty`, so it is absent.
const notExpiring = `{"hosts":{"github.com":[{"state":"success","active":true,"host":"github.com","login":"me"}]}}`

// fakeShareSmol is smolvm for a labelled, running web: `gh api user` in
// the guest exits apiExit, the install exits loginExit, and the install's
// stdin lands in the returned file. Every call's argv goes to log and its
// environment to envLog.
func fakeShareSmol(t *testing.T, apiExit, loginExit string) (log, envLog, stdin string) {
	t.Helper()
	dir := t.TempDir()
	log, envLog, stdin = filepath.Join(dir, "smolvm.log"), filepath.Join(dir, "env.log"), filepath.Join(dir, "stdin")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\nenv >> " + envLog + "\n" +
		"case \"$*\" in\n" +
		"  'machine ls --json') echo '[{\"name\":\"web\",\"state\":\"running\"," + devvmLabel + "}]';;\n" +
		"  *'gh api user'*) exit " + apiExit + ";;\n" +
		"  *'exec -i '*'gh auth login --hostname'*) cat > " + stdin + "; exit " + loginExit + ";;\n" +
		"  *'gh auth login --hostname'*) echo 'no -i' > " + stdin + "; exit 1;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "smolvm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stubBrowser(t, log)
	return log, envLog, stdin
}

// noTokenOutside fails if the token is anywhere but where it belongs: in
// a logged argv or environment, devvm's output, or any file this host
// keeps (devvm.toml and the state dir).
func noTokenOutside(t *testing.T, a *App, logs ...string) {
	t.Helper()
	for _, l := range logs {
		if data, _ := os.ReadFile(l); bytes.Contains(data, []byte(shareToken)) {
			t.Errorf("token in %s:\n%s", filepath.Base(l), data)
		}
	}
	for _, out := range []string{a.Stdout.(*bytes.Buffer).String(), a.Stderr.(*bytes.Buffer).String()} {
		if strings.Contains(out, shareToken) {
			t.Errorf("token in devvm's output: %q", out)
		}
	}
	for _, dir := range []string{a.Paths.Config, a.Paths.State} {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if data, _ := os.ReadFile(p); bytes.Contains(data, []byte(shareToken)) {
					t.Errorf("token at rest on this host: %s", p)
				}
			}
			return nil
		})
	}
}

// share_auth = ["github"] on a smol VM: the guest has no working login, so
// this host's token goes in over the install exec's stdin (smol's -i) and
// nowhere else, before the hook; one line names the host it came from. A
// host gh too old for `auth status --json` cannot hold an expiring login,
// so it shares too.
func TestShareAuthSmolInstalls(t *testing.T) {
	for name, status := range map[string]string{"json": notExpiring, "gh without --json": ""} {
		t.Run(name, func(t *testing.T) { testShareAuthSmolInstalls(t, status) })
	}
}

func testShareAuthSmolInstalls(t *testing.T, status string) {
	a := newTestApp(t)
	log, envLog, stdin := fakeShareSmol(t, "1", "0")
	calls := hostGHFake(t, shareToken, nil, status)
	writeDevvmTOML(t, a, "[templates.default]\nshare_auth = [\"github\"]\nhook = \"cmd:/opt/hook.sh\"\n")
	if err := runTree(t, a, "setup", "web"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	if got, _ := os.ReadFile(stdin); string(got) != shareToken+"\n" {
		t.Errorf("install stdin = %q, want the token", got)
	}
	data, _ := os.ReadFile(log)
	inOrder(t, string(data), "AGENT-INSTALL web", "gh api user",
		"exec -i --name web", "unset GH_TOKEN GITHUB_TOKEN", "gh auth login --help </dev/null",
		"gh auth login --hostname github.com --with-token",
		"gh auth setup-git", "gh config set git_protocol https --host github.com", "/opt/hook.sh")
	if w := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(w, "devvm: 'web' has laptop's GitHub login") {
		t.Errorf("stderr = %q", w)
	}
	if len(*calls) != 2 {
		t.Errorf("host gh calls = %q", *calls)
	}
	noTokenOutside(t, a, log, envLog)
}

// The cases where nothing is installed, each one line and never a failed
// setup: the guest already has a working login (left alone, this host's
// token never read), this host's login expires, this host has no login.
func TestShareAuthSkips(t *testing.T) {
	for _, tc := range []struct {
		name, apiExit, status, warn string
		tokenErr                    error
		tokenRead                   bool
	}{
		{"guest logged in", "0", notExpiring, "gh in 'web' already has a working GitHub login; share_auth leaves it alone", nil, false},
		{"expiring", "1", `{"hosts":{"github.com":[{"active":true,"refreshable":true}]}}`, "run 'gh auth login --expiring=false' here, then 'devvm setup web' again", nil, true},
		{"no host login", "1", notExpiring, "nothing to share from this host (gh auth token: exit status 1: no oauth token found for github.com)", errors.New("exit status 1: no oauth token found for github.com"), true},
		{"no host gh", "1", notExpiring, "nothing to share from this host (no gh)", exec.ErrNotFound, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			log, envLog, stdin := fakeShareSmol(t, tc.apiExit, "0")
			token := shareToken
			if tc.tokenErr != nil {
				token = ""
			}
			calls := hostGHFake(t, token, tc.tokenErr, tc.status)
			writeDevvmTOML(t, a, "[templates.default]\nshare_auth = [\"github\"]\nhook = \"cmd:/opt/hook.sh\"\n")
			if err := runTree(t, a, "setup", "web"); err != nil {
				t.Fatalf("setup: %v\n%s", err, a.Stderr)
			}
			if w := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(w, tc.warn) || strings.Count(w, "share_auth") > 1 {
				t.Errorf("stderr = %q, want one line with %q", w, tc.warn)
			}
			if _, err := os.Stat(stdin); err == nil {
				t.Error("installed a login")
			}
			if read := len(*calls) > 0; read != tc.tokenRead {
				t.Errorf("host gh calls = %q", *calls)
			}
			data, _ := os.ReadFile(log)
			if !strings.Contains(string(data), "/opt/hook.sh") {
				t.Errorf("the hook did not run:\n%s", data)
			}
			noTokenOutside(t, a, log, envLog)
		})
	}
}

// No share_auth (the built-in, or [] in a named template): this host's gh
// is never asked.
func TestShareAuthOff(t *testing.T) {
	a := newTestApp(t)
	log, _, _ := fakeShareSmol(t, "1", "0")
	calls := hostGHFake(t, shareToken, nil, notExpiring)
	writeDevvmTOML(t, a, "[templates.default]\nshare_auth = [\"github\"]\n[templates.untrusted]\nshare_auth = []\n")
	if err := runTree(t, a, "setup", "web", "-t", "untrusted"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	if len(*calls) != 0 || strings.Contains(string(data), "gh api user") {
		t.Errorf("share ran: host gh %q, guest:\n%s", *calls, data)
	}
}

// A provider server: the share runs after harden (so a failure never
// costs the box its hardening) and before the hook; a failed install is a
// warning, setup goes on, and the token is in the install's stdin alone
// (scrubbed from gh's error, should it echo it). A login that went in with
// a later command failing says so: the next setup leaves it alone.
func TestShareAuthServerAfterHarden(t *testing.T) {
	for _, mode := range []string{"ok", "fail", "partial"} {
		a := newTestApp(t)
		dir := t.TempDir()
		stdin, envLog := filepath.Join(dir, "stdin"), filepath.Join(dir, "env.log")
		install := `cat > ` + stdin
		switch mode {
		case "fail":
			install += `; echo "error validating token $(cat ` + stdin + `): HTTP 401" >&2; exit 1`
		case "partial":
			install += `; echo "some gh noise" >&2; echo 'gh auth setup-git' >&2; exit 87`
		}
		log := fakeRemote(t, `env >> `+envLog+`
case "$last" in
  *api*user*) exit 1;;
  *"gh auth login --hostname"*) `+install+`;;
esac`)
		hostGHFake(t, shareToken, nil, notExpiring)
		key := filepath.Join(t.TempDir(), "k.pub")
		if err := os.WriteFile(key, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBxM5xX4pQ9bJ8u5Yb6m8yRZ3qWZJ1hX0b0W9b3G7k5f me@laptop\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		fakeServer(t, a, "cloud", "cloud.example", `
[templates.default]
hook = "cmd:/opt/hook.sh"
authorized_keys = ["`+key+`"]
harden = true
share_auth = ["github"]
`)
		if err := runTree(t, a, "setup", "cloud"); err != nil {
			t.Fatalf("%s: setup: %v\n%s", mode, err, a.Stderr)
		}
		data, _ := os.ReadFile(log)
		inOrder(t, string(data), "authorized_keys.devvm", "AllowUsers", "gh auth login --hostname", "/opt/hook.sh")
		if got, _ := os.ReadFile(stdin); string(got) != shareToken+"\n" {
			t.Errorf("%s: install stdin = %q", mode, got)
		}
		want := map[string]string{
			"ok":      "'cloud' has laptop's GitHub login",
			"fail":    "share_auth github: installing the login in 'cloud' failed (exit status 1): error validating token [token]: HTTP 401 (setup continues)",
			"partial": "share_auth github: 'cloud' is logged in, but 'gh auth setup-git' failed: run it in the box (setup leaves a working login alone) (setup continues)",
		}[mode]
		if w := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(w, want) {
			t.Errorf("%s: stderr = %q, want %q", mode, w, want)
		}
		noTokenOutside(t, a, log, envLog)
	}
}
