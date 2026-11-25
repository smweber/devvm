package cli

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

func writeHook(t *testing.T, a *App, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(a.Paths.Config, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(a.Paths.Config, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A file: hook is read on this host when the template is resolved, before
// anything is created: beside devvm.toml unless absolute, and a missing or
// empty script is an error then.
func TestFileHookReadAtResolve(t *testing.T) {
	a := newTestApp(t)
	writeHook(t, a, "hook.sh", "echo hi\n")
	writeDevvmTOML(t, a, "[templates.default]\nhook = \"file:hook.sh --fast\"\nmemory = 1024\n")
	tm, err := a.resolveTemplate("", tmplFlags{})
	if err != nil || string(tm.HookScript) != "echo hi\n" {
		t.Fatalf("script = %q, %v", tm.HookScript, err)
	}
	abs := filepath.Join(t.TempDir(), "abs.sh")
	if err := os.WriteFile(abs, []byte("echo abs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tm, err = a.resolveTemplate("", tmplFlags{Hook: "file:" + abs}); err != nil || string(tm.HookScript) != "echo abs\n" {
		t.Fatalf("absolute: %q, %v", tm.HookScript, err)
	}
	if _, err = a.resolveTemplate("", tmplFlags{Hook: "file:nope.sh"}); err == nil || !strings.Contains(err.Error(), "nope.sh") {
		t.Errorf("missing script: err = %v", err)
	}
	writeHook(t, a, "empty.sh", " \n")
	if _, err = a.resolveTemplate("", tmplFlags{Hook: "file:empty.sh"}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty script: err = %v", err)
	}
	// A hook that is no file: carries no script.
	if tm, err = a.resolveTemplate("", tmplFlags{Hook: "cmd:/x"}); err != nil || tm.HookScript != nil {
		t.Errorf("cmd: script = %q, %v", tm.HookScript, err)
	}
	// create --yes fails on the missing script before it could make a VM.
	s := createSpec{Name: "box", Backend: config.BackendSmol, tmplFlags: tmplFlags{Hook: "file:nope.sh"}, Yes: true}
	if err := a.gatherCreateSpec(&s); err == nil {
		t.Error("gatherCreateSpec took a missing file: hook")
	}
}

// setup runs a file: hook as `bash -s -- ARGS` with the script on the
// exec's stdin (smolvm's -i), as the last step before repos.
func TestSetupRunsFileHook(t *testing.T) {
	a := newTestApp(t)
	log := fakeManagedSmol(t, a, "web")
	stubBrowser(t, log)
	writeHook(t, a, "hook.sh", "echo hi\n")
	writeDevvmTOML(t, a, "[templates.default]\nhook = \"file:hook.sh --fast\"\n")
	if err := runTree(t, a, "setup", "web"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	var hook string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasSuffix(l, "bash -s -- --fast") {
			hook = l
		}
	}
	if hook == "" || !strings.Contains(hook, "machine exec -i --stream") {
		t.Errorf("no stdin hook exec in:\n%s", data)
	}
	inOrder(t, string(data), "AGENT-INSTALL web", "bash -s -- --fast")
}

// config check reads a file: hook's script too: one that is missing here
// is an error naming it.
func TestConfigCheckFileHook(t *testing.T) {
	a := newTestApp(t)
	writeDevvmTOML(t, a, "[templates.default]\nhook = \"file:hook.sh\"\n")
	if err := runTree(t, a, "config", "check"); err == nil || !strings.Contains(err.Error(), "hook.sh") {
		t.Fatalf("check with no script: %v", err)
	}
	writeHook(t, a, "hook.sh", "true\n")
	if err := runTree(t, a, "config", "check"); err != nil {
		t.Fatalf("check: %v", err)
	}
}

// A file: hook names a script on the host it was typed on: a proxied
// create/setup sends the content as --hook-script, from a template and
// from a typed --hook alike, and the hub runs what it was sent.
func TestProxiedFileHook(t *testing.T) {
	a := newTestApp(t)
	_, argvLog := fakeHub(t, a)
	pinTTY(t, false)
	writeHook(t, a, "hook.sh", "echo laptop\n")
	writeDevvmTOML(t, a, "[templates.t]\nhook = \"file:hook.sh\"\n")
	want := "--hook-script=" + base64.StdEncoding.EncodeToString([]byte("echo laptop\n"))

	if err := runTree(t, a, "setup", "h/web", "--template", "t"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	calls := devvmCalls(t, argvLog)
	if len(calls) != 1 || !slices.Contains(calls[0], "--hook=file:hook.sh") || !slices.Contains(calls[0], want) {
		t.Errorf("template: hub got %q", calls)
	}

	os.Remove(argvLog)
	if err := runTree(t, a, "setup", "h/web", "--hook", "file:hook.sh"); err != nil {
		t.Fatalf("setup --hook: %v\n%s", err, a.Stderr)
	}
	calls = devvmCalls(t, argvLog)
	if len(calls) != 1 || !slices.Contains(calls[0], "--hook=file:hook.sh") || !slices.Contains(calls[0], want) ||
		slices.Contains(calls[0], "--inline-template") {
		t.Errorf("typed --hook: hub got %q", calls)
	}

	// Too big for one argument: refused here, nothing sent.
	os.Remove(argvLog)
	writeHook(t, a, "big.sh", strings.Repeat("# x\n", hubHookScriptMax/4+1))
	if err := runTree(t, a, "setup", "h/web", "--hook", "file:big.sh"); err == nil || !strings.Contains(err.Error(), "capped") {
		t.Errorf("big script: err = %v", err)
	}
	if calls = devvmCalls(t, argvLog); len(calls) != 0 {
		t.Errorf("big script still sent: %q", calls)
	}

	// The hub side: the sent script wins, and its own disk is not read.
	hub := newTestApp(t)
	tm, err := hub.resolveTemplate("", tmplFlags{Inline: true, Hook: "file:hook.sh", HookScript: base64.StdEncoding.EncodeToString([]byte("echo laptop\n"))})
	if err != nil || string(tm.HookScript) != "echo laptop\n" {
		t.Errorf("hub side: %q, %v", tm.HookScript, err)
	}
}

// A hub whose devvm predates file: hooks answers "unknown flag": one line
// naming the update.
func TestProxiedFileHookOldHub(t *testing.T) {
	a := newTestApp(t)
	fakeHubWith(t, a, t.TempDir(), `echo 'Error: unknown flag: --hook-script' >&2; exit 1`+"\n")
	pinTTY(t, false)
	writeHook(t, a, "hook.sh", "true\n")
	err := runTree(t, a, "setup", "h/web", "--hook", "file:hook.sh")
	if err == nil || !strings.Contains(err.Error(), "without file: hooks") {
		t.Errorf("err = %v", err)
	}
}

// The form's starter choice writes nothing itself; writeStarterHook, run
// once create is past its refusals, writes hook.sh beside devvm.toml once
// and never over the user's. A missing devvm.toml is created naming it, an
// existing one is left byte for byte.
func TestUseStarterHook(t *testing.T) {
	a := newTestApp(t)
	a.Stderr = new(bytes.Buffer)
	tm := config.Template{Name: "default", Hook: "none"}
	if err := a.useStarterHook(&tm); err != nil {
		t.Fatal(err)
	}
	if tm.Hook != "file:hook.sh" || !bytes.Equal(tm.HookScript, starterHook) {
		t.Errorf("template hook = %q (%d bytes)", tm.Hook, len(tm.HookScript))
	}
	hook := filepath.Join(a.Paths.Config, "hook.sh")
	if _, err := os.Stat(hook); err == nil {
		t.Fatal("the form's choice wrote hook.sh before create could refuse")
	}
	a.writeStarterHook()
	if got, _ := os.ReadFile(hook); !bytes.Equal(got, starterHook) {
		t.Errorf("hook.sh is not the starter")
	}
	// The file it wrote resolves to the same hook for the next create/setup,
	// and that hook is a set one: the starter is not offered again.
	if next, err := a.resolveTemplate("", tmplFlags{}); err != nil || next.Hook != "file:hook.sh" || !next.HookSet {
		t.Errorf("next create: hook = %q set=%v, %v", next.Hook, next.HookSet, err)
	}

	// Again, with both files the user's by now: neither is touched.
	if err := os.WriteFile(hook, []byte("echo mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mine := "[templates.default]\nmemory = 2048\n"
	if err := os.WriteFile(a.Paths.FilePath(), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	a.Stderr = new(bytes.Buffer)
	tm = config.Template{Name: "default", Hook: "none"}
	if err := a.useStarterHook(&tm); err != nil {
		t.Fatal(err)
	}
	a.writeStarterHook()
	if string(tm.HookScript) != "echo mine\n" {
		t.Errorf("script = %q, want the user's hook.sh", tm.HookScript)
	}
	if got, _ := os.ReadFile(hook); string(got) != "echo mine\n" {
		t.Errorf("hook.sh was overwritten:\n%s", got)
	}
	if got, _ := os.ReadFile(a.Paths.FilePath()); string(got) != mine {
		t.Errorf("devvm.toml was edited:\n%s", got)
	}
	if !strings.Contains(a.Stderr.(*bytes.Buffer).String(), `hook = "file:hook.sh"`) {
		t.Errorf("no line to add: %q", a.Stderr)
	}
}

// The Hook select: the starter is always offered beside the template's
// hook, none and custom (not when it is the template's hook, nor on a hub
// running a laptop's create). The template's hook is pre-selected; with
// no hook set by any layer the starter is; a written "none" stays none.
func TestHookChoices(t *testing.T) {
	a := newTestApp(t)
	keys := func(tm config.Template) (string, string) {
		opts, choice := a.hookChoices(&tm)
		var ks []string
		for _, o := range opts {
			ks = append(ks, o.Value)
		}
		return strings.Join(ks, " "), choice
	}
	for _, tt := range []struct {
		name         string
		tm           config.Template
		opts, choice string
	}{
		{"no hook anywhere", config.Template{Hook: "none"}, "starter none custom", optStarter},
		{"a written none", config.Template{Hook: "none", HookSet: true}, "starter none custom", optNone},
		{"the template's own", config.Template{Hook: "url:https://x/b.sh", HookSet: true}, "default starter none custom", optDefault},
		{"the starter is the template's", config.Template{Hook: "file:hook.sh", HookSet: true}, "default none custom", optDefault},
	} {
		if opts, choice := keys(tt.tm); opts != tt.opts || choice != tt.choice {
			t.Errorf("%s: %q pre-selecting %q, want %q pre-selecting %q", tt.name, opts, choice, tt.opts, tt.choice)
		}
	}
	t.Setenv("DEVVM_NO_SUBSCRIBE", "1")
	if opts, choice := keys(config.Template{Hook: "none"}); opts != "none custom" || choice != optNone {
		t.Errorf("on a hub: %q pre-selecting %q", opts, choice)
	}

	// A resolved template says whether a layer set the hook.
	if tm, err := a.resolveTemplate("", tmplFlags{}); err != nil || tm.HookSet || tm.Hook != "none" {
		t.Errorf("no file: %+v, %v", tm, err)
	}
	writeDevvmTOML(t, a, "[templates.default]\nhook = \"none\"\n")
	if tm, err := a.resolveTemplate("", tmplFlags{}); err != nil || !tm.HookSet {
		t.Errorf("a written none is not set: %+v, %v", tm, err)
	}
}

// "Beside devvm.toml" is beside the real file: with devvm.toml linked in
// from a dotfiles repo, the hook is read from (and the starter written to)
// the repo.
func TestFileHookBesideLinkedConfig(t *testing.T) {
	a := newTestApp(t)
	repo := t.TempDir()
	if err := os.MkdirAll(a.Paths.Config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "devvm.toml"), []byte("[templates.default]\nhook = \"file:hook.sh\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "hook.sh"), []byte("echo repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, "devvm.toml"), a.Paths.FilePath()); err != nil {
		t.Fatal(err)
	}
	if tm, err := a.resolveTemplate("", tmplFlags{}); err != nil || string(tm.HookScript) != "echo repo\n" {
		t.Errorf("script = %q, %v", tm.HookScript, err)
	}
}

// The starter is valid bash, asks nothing (every apt reads /dev/null: the
// script itself is on stdin), and its tmux config is the hook's own file.
func TestStarterHookScript(t *testing.T) {
	if bash, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = bytes.NewReader(starterHook)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bash -n: %v\n%s", err, out)
		}
	}
	for _, l := range strings.Split(string(starterHook), "\n") {
		if strings.HasPrefix(l, "sudo ") && strings.Contains(l, "apt-get") && !strings.HasSuffix(l, "\\") && !strings.Contains(l, "</dev/null") {
			t.Errorf("apt reads the script's stdin: %s", l)
		}
	}
	if !strings.Contains(string(starterHook), "set -g mouse on") {
		t.Error("no tmux config in the starter")
	}
}
