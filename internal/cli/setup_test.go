package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
	"github.com/smweber/devvm/internal/reach"
)

// writeDevvmTOML replaces devvm.toml with body. A valid body with no
// [machines.*] of its own (templates only) keeps the entries a test already wrote
// (writeEntry, writeHub, fakeHub), re-appended after it.
func writeDevvmTOML(t *testing.T, a *App, body string) {
	t.Helper()
	if err := os.MkdirAll(a.Paths.Config, 0o755); err != nil {
		t.Fatal(err)
	}
	var keep map[string]config.Entry
	if _, err := config.ParseFile("body", []byte(body)); err == nil && !strings.Contains(body, "[machines") {
		if f, err := config.LoadFile(a.Paths); err == nil {
			keep = f.Entries
		}
	}
	if err := os.WriteFile(a.Paths.FilePath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, n := range sortedNames(keep) {
		if err := config.AppendEntry(a.Paths, n, keep[n]); err != nil {
			t.Fatal(err)
		}
	}
}

// Precedence: flag > --template > [templates.default] > built-in, with the
// named template falling back to default one level.
func TestResolveTemplatePrecedence(t *testing.T) {
	a := newTestApp(t)
	writeDevvmTOML(t, a, `
[templates.default]
hook = "cmd:/default"
memory = 4096
repos = ["o/default"]

[templates.web]
memory = 8192
harden = true
`)
	got, err := a.resolveTemplate("", tmplFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "default" || got.Hook != "cmd:/default" || got.Memory != 4096 || got.Disk != 0 ||
		!slices.Equal(got.Repos, []string{"o/default"}) || got.User != "dev" {
		t.Errorf("default = %+v", got)
	}
	// --template web: its own fields win; the rest fall through to default.
	got, err = a.resolveTemplate("web", tmplFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Memory != 8192 || !got.Harden || got.Hook != "cmd:/default" || !slices.Equal(got.Repos, []string{"o/default"}) {
		t.Errorf("web = %+v", got)
	}
	if got, err := a.resolveTemplate("default", tmplFlags{}); err != nil || got.Hook != "cmd:/default" || got.Name != "default" {
		t.Errorf("--template default = %+v, %v", got, err)
	}
	// Flags beat everything, a false bool included.
	got, err = a.resolveTemplate("web", tmplFlags{Memory: 512, Hook: "none", Harden: false, HardenSet: true,
		Repos: []string{"o/flag"}, User: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Memory != 512 || got.Hook != "none" || got.Harden || !slices.Equal(got.Repos, []string{"o/flag"}) || got.User != "alice" {
		t.Errorf("flags = %+v", got)
	}
	// Inline: the flags are the whole template, nothing on this host fills in.
	got, err = a.resolveTemplate("web", tmplFlags{Inline: true, Hook: "cmd:/x"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Memory != 0 || got.Harden || len(got.Repos) != 0 || got.Hook != "cmd:/x" || got.User != "dev" {
		t.Errorf("inline = %+v", got)
	}
	// Bad values are refused before anything runs.
	for _, f := range []tmplFlags{{Hook: "bogus"}, {User: "root"}, {Memory: 1}} {
		if _, err := a.resolveTemplate("", f); err == nil {
			t.Errorf("%+v accepted", f)
		}
	}
	if _, err := a.resolveTemplate("nope", tmplFlags{}); err == nil {
		t.Error("unknown template accepted")
	}
}

// create --yes resolves flag > template > built-in.
func TestCreateSeedsFromTemplate(t *testing.T) {
	a := newTestApp(t)
	writeDevvmTOML(t, a, `
[templates.default]
memory = 4096
hook = "cmd:/d"
[templates.big]
memory = 8192
repos = ["o/r"]
`)
	s := createSpec{Name: "box", Template: "big", Yes: true, tmplFlags: tmplFlags{Disk: 3}}
	if err := a.gatherCreateSpec(&s); err != nil {
		t.Fatal(err)
	}
	if s.Memory != 8192 || s.Disk != 3 || s.Hook != "cmd:/d" ||
		!slices.Equal(s.Repos, []string{"o/r"}) {
		t.Errorf("spec = %+v", s)
	}
	if tt := s.template(); tt.Name != "big" || tt.User != "dev" || tt.Memory != 8192 {
		t.Errorf("template = %+v", tt)
	}
}

// fakeManagedSmol registers a local smol VM devvm made (marker = created_at).
func fakeManagedSmol(t *testing.T, a *App, name string) (log string) {
	t.Helper()
	log = fakeLocalSmolvm(t, `[{"name":"`+name+`","state":"running","created_at":100}]`)
	if err := config.WriteManagedMarker(a.Paths, name, "100"); err != nil {
		t.Fatal(err)
	}
	return log
}

// stubBrowser stands in for the agent install, logging a marker line into
// log so the step order can be read from one file.
func stubBrowser(t *testing.T, log string) {
	t.Helper()
	orig := browserStep
	browserStep = func(_ context.Context, _ backend.Backend, m *config.Machine) error {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString("AGENT-INSTALL " + m.Name + "\n")
		return err
	}
	t.Cleanup(func() { browserStep = orig })
}

// inOrder fails unless each needle appears in hay after the one before it.
func inOrder(t *testing.T, hay string, needles ...string) {
	t.Helper()
	at := 0
	for _, n := range needles {
		i := strings.Index(hay[at:], n)
		if i < 0 {
			t.Fatalf("%q missing (or out of order) after offset %d in:\n%s", n, at, hay)
		}
		at += i + len(n)
	}
}

// setup on a managed smol VM: the fixed order, re-run without asking;
// keys and hardening are ignored with one warning line.
func TestSetupSmolOrder(t *testing.T) {
	a := newTestApp(t)
	log := fakeManagedSmol(t, a, "web")
	stubBrowser(t, log)
	writeDevvmTOML(t, a, `
[templates.default]
hook = "cmd:/opt/hook.sh"
repos = ["o/r"]
harden = true
authorized_keys_github = ["x"]
`)
	if err := config.SavePorts(a.Paths, "web", []string{"4000"}); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "setup", "web"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	inOrder(t, string(data),
		"useradd", "NOPASSWD", // ensure user
		"apt-get -o DPkg::Lock::Timeout=300 install -y build-essential ca-certificates curl git rsync tmux mosh", "gh", // prereqs
		"AGENT-INSTALL web",    // agent + shim
		"/opt/hook.sh",         // hook
		"gh repo clone", "o/r", // repos
	)
	if strings.Contains(string(data), "ufw") || strings.Contains(string(data), "authorized_keys.devvm") {
		t.Errorf("smol setup hardened or wrote keys:\n%s", data)
	}
	if w := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(w, "ignoring keys, harden") {
		t.Errorf("no ignore warning: %q", w)
	}
	// setup never touches this host's saved forwards.
	if got, _ := config.LoadPorts(a.Paths, "web"); !slices.Equal(got, []string{"4000"}) {
		t.Errorf("ports = %v, want this host's own untouched", got)
	}
}

// Refusals, all before anything runs in the box.
func TestSetupRefusals(t *testing.T) {
	a := newTestApp(t)
	log := fakeLocalSmolvm(t, `[{"name":"web","state":"running","created_at":100},{"name":"made","state":"running","created_at":5}]`)
	if err := config.WriteManagedMarker(a.Paths, "made", "5"); err != nil {
		t.Fatal(err)
	}
	ssh := fakeSSH(t, "exit 0")
	if err := writeEntry(a.Paths, config.NewRemote("box", "dev@x")); err != nil {
		t.Fatal(err)
	}
	pinTTYs(t, true, true)
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"setup", "web"}, "devvm setup --adopt web"},
		{[]string{"setup", "--adopt", "made"}, "already managed"},
		{[]string{"setup", "box"}, "'box' is an entry in devvm.toml: devvm never shapes one"},
		{[]string{"setup", "--adopt", "box"}, "never shapes"},
		{[]string{"setup", "--adopt", "--yes", "web"}, "--yes does not apply"},
		{[]string{"setup", "--host", "1.2.3.4", "made"}, "unknown flag: --host"},
	} {
		err := runTree(t, a, tc.argv...)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "\n") {
			t.Errorf("%v: err = %v, want one line with %q", tc.argv, err, tc.want)
		}
	}
	// No terminal: --adopt refuses.
	pinTTYs(t, false, false)
	if err := runTree(t, a, "setup", "--adopt", "web"); err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Errorf("no tty: %v", err)
	}
	if calls := smolvmCalls(t, log); len(calls) > 0 {
		t.Errorf("refusals ran smolvm: %q", calls)
	}
	if _, err := os.Stat(ssh); err == nil {
		t.Error("refusals dialed ssh")
	}
	if _, ok, _ := config.ReadManagedMarker(a.Paths, "web"); ok {
		t.Error("a refused adoption wrote the marker")
	}
}

type fakeTTY struct {
	io.Reader
	out bytes.Buffer
}

func (f *fakeTTY) Write(p []byte) (int, error) { return f.out.Write(p) }
func (f *fakeTTY) Close() error                { return nil }

func typeIntoTTY(t *testing.T, typed string) *fakeTTY {
	t.Helper()
	tty := &fakeTTY{Reader: strings.NewReader(typed)}
	orig := openAdoptTTY
	openAdoptTTY = func() (io.ReadWriteCloser, error) { return tty, nil }
	t.Cleanup(func() { openAdoptTTY = orig })
	pinTTYs(t, true, true)
	return tty
}

// --adopt prints the plan and needs the VM's name typed: a wrong answer
// (y included) adopts nothing; the name writes the marker and shapes.
func TestSetupAdoptTypedName(t *testing.T) {
	a := newTestApp(t)
	log := fakeLocalSmolvm(t, `[{"name":"web","state":"running","created_at":100}]`)
	stubBrowser(t, log)
	writeDevvmTOML(t, a, "[templates.default]\nhook = \"cmd:/opt/h\"\nrepos = [\"o/r\"]\n")
	if err := config.SavePorts(a.Paths, "web", []string{"9999"}); err != nil {
		t.Fatal(err)
	}

	tty := typeIntoTTY(t, "y\n")
	err := runTree(t, a, "setup", "--adopt", "web")
	if err == nil || !strings.Contains(err.Error(), "not adopted") {
		t.Fatalf("typed y: %v", err)
	}
	for _, want := range []string{
		"hands the smol VM 'web' to devvm",
		"install devvm's prereqs", "as root", "devvm agent",
		"run the hook: cmd:/opt/h", "clone into ~/src: o/r",
		"treat 'web' as managed from now on",
		"Type the VM's name (web) to hand it over: ",
	} {
		if !strings.Contains(tty.out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, tty.out.String())
		}
	}
	if _, ok, _ := config.ReadManagedMarker(a.Paths, "web"); ok || len(smolvmCalls(t, log)) > 0 {
		t.Fatal("an aborted adoption marked or touched the VM")
	}

	typeIntoTTY(t, "web\n")
	if err := runTree(t, a, "setup", "--adopt", "web"); err != nil {
		t.Fatalf("typed the name: %v\n%s", err, a.Stderr)
	}
	if v, ok, _ := config.ReadManagedMarker(a.Paths, "web"); !ok || v != "100" {
		t.Errorf("marker = %q, %v", v, ok)
	}
	data, _ := os.ReadFile(log)
	// Managed now: the execs go through the dev-user wrapper.
	inOrder(t, string(data), "useradd", "build-essential", "AGENT-INSTALL web", "/opt/h", "gh repo clone")
	// setup never touches saved forwards (only create clears a leftover).
	if got, _ := config.LoadPorts(a.Paths, "web"); !slices.Equal(got, []string{"9999"}) {
		t.Errorf("ports = %v after an adoption", got)
	}
}

// fakeRemote is a fake ssh for the remote flow: it logs its argv, answers
// `id -un` with the destination's user, and runs body first (dest is the
// destination, $last the remote command).
//
// setup's agent step (bootstrap.Browser: agentbin's probes and install) is
// stood in for, logging `AGENT-INSTALL NAME` into the same log, so a
// remote setup's order reads from one file.
func fakeRemote(t *testing.T, body string) string {
	t.Helper()
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "")
	log := fakeSSH(t, `eval "dest=\${$(($# - 1))}"
`+body+`
case "$last" in *"'id' '-un'"*|"id -un"*|*"id -un"*) echo "${dest%%@*}";; esac
exit 0`)
	stubBrowser(t, log)
	return log
}

// fakeServer makes name a provider server devvm made (the devvm=true label)
// at addr: the one kind of box reached over ssh that setup shapes (an
// entry is never shaped). templates go into devvm.toml beside
// [providers.hetzner].
func fakeServer(t *testing.T, a *App, name, addr, templates string) {
	t.Helper()
	fakeLocalSmolvm(t, `[]`)
	p := withFakeHetzner(t, a, templates, false)
	addServer(t, p, name, addr, provider.StateRunning, true)
}

func shrinkReachWait(t *testing.T, budget, interval time.Duration) {
	t.Helper()
	ob, oi := setupReachBudget, setupReachInterval
	setupReachBudget, setupReachInterval = budget, interval
	t.Cleanup(func() { setupReachBudget, setupReachInterval = ob, oi })
}

// The order over ssh (a provider server devvm made): reach, prereqs, agent
// + shim + profile.d (as smol: its daemon runs the agent exec too), keys,
// harden, hook, repos. The security steps sit before user code and the
// network.
func TestSetupRemoteOrder(t *testing.T) {
	a := newTestApp(t)
	log := fakeRemote(t, "")
	key := filepath.Join(t.TempDir(), "k.pub")
	if err := os.WriteFile(key, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBxM5xX4pQ9bJ8u5Yb6m8yRZ3qWZJ1hX0b0W9b3G7k5f me@laptop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeServer(t, a, "cloud", "cloud.example", `
[templates.default]
hook = "cmd:/opt/hook.sh"
repos = ["o/r"]
memory = 4096
authorized_keys = ["`+key+`"]
harden = true
fail2ban = true
`)
	if err := runTree(t, a, "setup", "cloud"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	inOrder(t, string(data),
		"BatchMode=yes", "dev@cloud.example 'true'", // reach
		"'id' '-un'",                   // login user
		"build-essential", "tmux mosh", // prereqs
		"AGENT-INSTALL cloud",           // agent + shim + profile.d
		"authorized_keys.devvm",         // keys
		"WANT_FAIL2BAN=1", "AllowUsers", // harden
		"/opt/hook.sh",  // hook
		"gh repo clone", // repos
	)
	for _, bad := range []string{"root@"} {
		if strings.Contains(string(data), bad) {
			t.Errorf("remote setup mentions %q", bad)
		}
	}
	if w := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(w, "ignoring memory (not applicable to a Hetzner server)") {
		t.Errorf("warning = %q", w)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "devvm: 'cloud' is set up\n") {
		t.Errorf("no completion line: %q", out)
	}
	// Every step above ran over ssh, so the box is a verified contact now.
	if e := reach.Read(a.Paths, "cloud"); e.State != reach.Reachable || e.How != "setup" {
		t.Errorf("setup recorded %+v, want reachable/setup", e)
	}
}

// A failed hook still fails setup, but keys and hardening ran before it:
// user code can no longer leave a cloud box unhardened. Nothing after the
// hook runs.
func TestSetupHardensBeforeFailedHook(t *testing.T) {
	a := newTestApp(t)
	log := fakeRemote(t, `case "$last" in *hook.sh*) echo "hook broke" >&2; exit 7;; esac`)
	key := filepath.Join(t.TempDir(), "k.pub")
	if err := os.WriteFile(key, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBxM5xX4pQ9bJ8u5Yb6m8yRZ3qWZJ1hX0b0W9b3G7k5f me@laptop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeServer(t, a, "cloud", "cloud.example", `
[templates.default]
hook = "cmd:/opt/hook.sh"
repos = ["o/r"]
authorized_keys = ["`+key+`"]
harden = true
`)
	if err := runTree(t, a, "setup", "cloud"); err == nil {
		t.Fatal("setup succeeded past a failed hook")
	}
	data, _ := os.ReadFile(log)
	inOrder(t, string(data), "authorized_keys.devvm", "AllowUsers", "/opt/hook.sh")
	if strings.Contains(string(data), "gh repo clone") {
		t.Errorf("repos ran after a failed hook:\n%s", data)
	}
}

// Root adoption: the login ([templates.default]'s user) is refused
// (permission denied), root answers, so that user is created and setup
// continues as it. Nothing is written: every later command logs in as the
// same template user.
func TestSetupRootAdoption(t *testing.T) {
	a := newTestApp(t)
	made := filepath.Join(t.TempDir(), "made")
	log := fakeRemote(t, `case "$last" in *useradd*) touch `+made+`;; esac
case "$dest" in
  root@*) ;;
  alice@*) [ -e `+made+` ] || { echo "$dest: Permission denied (publickey)." >&2; exit 255; };;
  *) echo "$dest: Permission denied (publickey)." >&2; exit 255;;
esac`)
	fakeServer(t, a, "cloud", "203.0.113.5", "[templates.default]\nuser = \"alice\"\n")
	before, _ := os.ReadFile(a.Paths.FilePath())
	if err := runTree(t, a, "setup", "cloud"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	inOrder(t, string(data),
		" alice@203.0.113.5 'true'", " root@203.0.113.5 'true'", // denied, then root
		"root@203.0.113.5 'bash' '-c'", "useradd", "'alice'", // EnsureUser("alice") as root
		"alice@203.0.113.5", "build-essential", // prereqs as alice
	)
	if strings.Contains(string(data), "/home/dev") {
		t.Error("EnsureUser hardcoded /home/dev")
	}
	if after, _ := os.ReadFile(a.Paths.FilePath()); !bytes.Equal(before, after) {
		t.Errorf("setup wrote devvm.toml:\n%s", after)
	}
}

// Unreachable is not permission denied: it is waited for (bounded), and
// root is never tried.
func TestSetupUnreachable(t *testing.T) {
	a := newTestApp(t)
	shrinkReachWait(t, 300*time.Millisecond, 50*time.Millisecond)
	log := fakeRemote(t, `echo "ssh: connect to host cloud.example port 22: Connection refused" >&2; exit 255`)
	fakeServer(t, a, "cloud", "cloud.example", "")
	start := time.Now()
	err := runTree(t, a, "setup", "cloud")
	if err == nil || !strings.Contains(err.Error(), "cannot reach dev@cloud.example") || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the wait was not bounded")
	}
	data, _ := os.ReadFile(log)
	if n := strings.Count(string(data), "'true'"); n < 2 {
		t.Errorf("probed %d times, want retries", n)
	}
	if strings.Contains(string(data), "root@") {
		t.Error("an unreachable box was tried as root")
	}
}

// A box that comes up within the budget is shaped.
func TestSetupWaits(t *testing.T) {
	a := newTestApp(t)
	shrinkReachWait(t, 5*time.Second, 20*time.Millisecond)
	count := filepath.Join(t.TempDir(), "n")
	log := fakeRemote(t, `case "$last" in "'true'")
  n=$(cat `+count+` 2>/dev/null || echo 0); n=$((n+1)); echo $n > `+count+`
  [ $n -ge 3 ] || { echo "Connection timed out" >&2; exit 255; };;
esac`)
	fakeServer(t, a, "cloud", "198.51.100.7", "")
	if err := runTree(t, a, "setup", "cloud"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "dev@198.51.100.7 'sudo' 'bash'") {
		t.Errorf("prereqs did not run once it answered:\n%s", data)
	}
	if !strings.Contains(a.Stderr.(*bytes.Buffer).String(), "waiting up to") {
		t.Error("no wait notice")
	}
}

// probeSSH tells permission denied from unreachable by stderr, not by the
// exit status (255 for both).
func TestProbeSSH(t *testing.T) {
	a := newTestApp(t)
	m := config.NewRemote("c", "dev@c")
	for body, want := range map[string]reachResult{
		"exit 0": reachOK,
		`echo "This account is currently not available." >&2; exit 1`:        reachOK, // nologin: answered
		`echo "dev@c: Permission denied (publickey)." >&2; exit 255`:         reachDenied,
		`echo "ssh: Could not resolve hostname c" >&2; exit 255`:             reachDown,
		`echo "kex_exchange_identification: Connection reset" >&2; exit 255`: reachDown,
	} {
		fakeSSH(t, body)
		got, _, err := a.probeSSH(context.Background(), m)
		if err != nil || got != want {
			t.Errorf("%s: %v, %v, want %v", body, got, err, want)
		}
	}
	// Fatal at once: no retry changes a host key or the agent's key count.
	shrinkReachWait(t, 5*time.Second, time.Second)
	for body, want := range map[string]string{
		`printf '@@@@@\n@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @\n@@@@@\nHost key verification failed.\n' >&2; exit 255`: "REMOTE HOST IDENTIFICATION HAS CHANGED",
		`echo "Host key verification failed." >&2; exit 255`:                                      "Host key verification failed",
		`echo "Received disconnect from 1.2.3.4: Too many authentication failures" >&2; exit 255`: "Too many authentication failures",
		// Local: this host's ssh, never the box, so never worth the wait.
		`echo "ControlPath too long ('/x/c-abc' >= 108 bytes)" >&2; exit 255`:   "ControlPath too long",
		`echo "sh: 1: cloudflared: command not found" >&2; exit 255`:            "command not found",
		`echo "/x/config: line 3: Bad configuration option: foo" >&2; exit 255`: "Bad configuration option",
	} {
		fakeSSH(t, body)
		start := time.Now()
		_, err := a.waitSSH(context.Background(), m)
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "\n") {
			t.Errorf("%s: err = %v, want one line with %q", body, err, want)
		}
		if time.Since(start) > 500*time.Millisecond {
			t.Errorf("%s: retried a fatal answer", body)
		}
	}
}

// Root adoption re-checks the new user: a user that already existed with
// other keys refuses us, and nothing is written.
func TestSetupRootAdoptionUserRefuses(t *testing.T) {
	a := newTestApp(t)
	fakeRemote(t, `case "$dest" in
  root@*) ;;
  *) echo "$dest: Permission denied (publickey)." >&2; exit 255;;
esac`)
	fakeServer(t, a, "cloud", "203.0.113.5", "")
	before, _ := os.ReadFile(a.Paths.FilePath())
	err := runTree(t, a, "setup", "cloud")
	if err == nil || !strings.Contains(err.Error(), "user dev exists on 'cloud' but your key is not in its authorized_keys") {
		t.Fatalf("err = %v", err)
	}
	if after, _ := os.ReadFile(a.Paths.FilePath()); !bytes.Equal(before, after) {
		t.Errorf("setup wrote devvm.toml:\n%s", after)
	}
}

// setup --template T on a provider server, T's user other than the login
// ([templates.default]'s): the login wins, with a warning that the
// template user is ignored.
func TestSetupOtherLoginWarns(t *testing.T) {
	a := newTestApp(t)
	fakeRemote(t, "")
	fakeServer(t, a, "cloud", "cloud.example", "[templates.ops]\nuser = \"alice\"\n")
	if err := runTree(t, a, "setup", "cloud", "--template", "ops"); err != nil {
		t.Fatal(err)
	}
	if w := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(w, `logs in as dev: template user "alice" is ignored, and hardening would pin AllowUsers to dev`) {
		t.Errorf("warning = %q", w)
	}
}

// A stopped smol VM is started (and waited for) the way `start` does it,
// before setup's first exec, never booted implicitly by one.
func TestSetupStartsStoppedSmol(t *testing.T) {
	a := newTestApp(t)
	dir := t.TempDir()
	ls, log := filepath.Join(dir, "ls.json"), filepath.Join(dir, "smolvm.log")
	if err := os.WriteFile(ls, []byte(`[{"name":"web","state":"stopped","created_at":100}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"  'machine ls --json') cat " + ls + ";;\n" +
		"  'machine start --name web') echo '[{\"name\":\"web\",\"state\":\"running\",\"created_at\":100}]' > " + ls + ";;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "smolvm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := config.WriteManagedMarker(a.Paths, "web", "100"); err != nil {
		t.Fatal(err)
	}
	stubBrowser(t, log)
	if err := runTree(t, a, "setup", "web"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	inOrder(t, string(data), "machine start --name web", "machine exec")
	if !strings.Contains(a.Stderr.(*bytes.Buffer).String(), "starting web…") {
		t.Errorf("stderr = %q", a.Stderr)
	}
}

// setup HUB/NAME --template T resolves T here and sends every field as an
// explicit flag; the hub's own templates never matter. The forwards are
// this host's state for HUB@NAME: setup leaves them alone, create clears a
// leftover list.
func TestProxiedTemplateFlags(t *testing.T) {
	a := newTestApp(t)
	_, argvLog := fakeHub(t, a)
	pinTTY(t, false)
	writeDevvmTOML(t, a, `
[templates.default]
hook = "url:https://x/b.sh --profile agent-vm --yes"
memory = 4096
[templates.devvm]
memory = 8192
repos = ["smweber/devvm", "https://x.example/a,b.git"]
harden = true
`)
	if err := config.SavePorts(a.Paths, "h/web", []string{"3000"}); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "setup", "h/web", "--template", "devvm", "--yes", "--fail2ban"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	want := []string{"setup", "web", "--inline-template", "--user=dev",
		"--hook=url:https://x/b.sh --profile agent-vm --yes", "--memory=8192",
		"--repo=smweber/devvm", "--repo=https://x.example/a,b.git",
		"--harden=true", "--fail2ban=true", "--no-ports", "--yes"}
	calls := devvmCalls(t, argvLog)
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Errorf("hub got %q\nwant %q", calls, want)
	}
	if got, _ := config.LoadPorts(a.Paths, "h/web"); !slices.Equal(got, []string{"3000"}) {
		t.Errorf("laptop forwards for h/web = %v", got)
	}

	// create: the same set, and a stale list is cleared.
	os.Remove(argvLog)
	if err := config.SavePorts(a.Paths, "h/api", []string{"1"}); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "create", "h/api", "-t", "devvm", "--yes", "--disk", "9"); err != nil {
		t.Fatalf("create: %v\n%s", err, a.Stderr)
	}
	calls = devvmCalls(t, argvLog)
	if len(calls) != 1 || calls[0][0] != "create" || calls[0][1] != "api" ||
		!slices.Contains(calls[0], "--disk=9") || !slices.Contains(calls[0], "--inline-template") ||
		slices.ContainsFunc(calls[0], func(s string) bool { return strings.HasPrefix(s, "--template") || strings.HasPrefix(s, "--port") }) {
		t.Errorf("create argv = %q", calls)
	}
	if got, _ := config.LoadPorts(a.Paths, "h/api"); len(got) != 0 {
		t.Errorf("laptop forwards for h/api = %v, want the leftover cleared", got)
	}

	// Without --template (or with --template default, the same thing) the
	// leaf goes across as typed, except the ignored --port, which is
	// stripped.
	for _, argv := range [][]string{{"setup", "h/web", "--port", "8080"}, {"setup", "h/web", "-t", "default", "--port", "8080"}} {
		os.Remove(argvLog)
		if err := runTree(t, a, argv...); err != nil {
			t.Fatal(err)
		}
		want := []string{"setup", "web", "--no-ports"}
		if argv[2] == "-t" {
			want = append(want, "--template=default")
		}
		if calls = devvmCalls(t, argvLog); len(calls) != 1 || !slices.Equal(calls[0], want) {
			t.Errorf("%v: argv = %q, want %q", argv, calls, want)
		}
	}
	if got, _ := config.LoadPorts(a.Paths, "h/web"); !slices.Equal(got, []string{"3000"}) {
		t.Errorf("laptop forwards for h/web after --port = %v", got)
	}
}

// A template's keys are never resolved for a hub machine (no GitHub fetch,
// no warning every run): HUB/NAME is a hub's smol VM, which takes no keys,
// and a hub's own entries are never shaped.
func TestProxiedKeysNeverSent(t *testing.T) {
	a := newTestApp(t)
	_, argvLog := fakeHub(t, a)
	pinTTY(t, false)
	key := filepath.Join(t.TempDir(), "k.pub")
	line := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBxM5xX4pQ9bJ8u5Yb6m8yRZ3qWZJ1hX0b0W9b3G7k5f me@laptop"
	if err := os.WriteFile(key, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDevvmTOML(t, a, "[templates.k]\nauthorized_keys = [\""+key+"\"]\n")
	for _, vm := range []string{"web", "box"} {
		os.Remove(argvLog)
		if err := runTree(t, a, "setup", "h/"+vm, "-t", "k"); err != nil {
			t.Fatal(err)
		}
		calls := devvmCalls(t, argvLog)
		if len(calls) != 1 || slices.ContainsFunc(calls[0], func(s string) bool { return strings.HasPrefix(s, "--key") }) {
			t.Errorf("%s: argv = %q, want no --key", vm, calls)
		}
	}
}

// A hub older than setup gets one line naming the fix, not cobra's error.
func TestProxiedSetupOldHub(t *testing.T) {
	a := newTestApp(t)
	fakeHubWith(t, a, t.TempDir(), `echo 'Error: unknown command "setup" for "devvm"' >&2; exit 1`+"\n")
	pinTTY(t, false)
	for _, argv := range [][]string{{"setup", "h/web"}, {"create", "h/api", "--yes"}} {
		err := runTree(t, a, argv...)
		if err == nil || !strings.Contains(err.Error(), "hub h runs devvm older than v0.1.16 (needs setup)") {
			t.Errorf("%v: %v", argv, err)
		}
	}
	fakeHubWith(t, a, t.TempDir(), `echo 'Error: unknown flag: --no-ports' >&2; exit 1`+"\n")
	if err := runTree(t, a, "create", "h/api", "--yes"); err == nil || !strings.Contains(err.Error(), "older than v0.1.16") {
		t.Errorf("unknown flag: %v", err)
	}
}

// fakeCreateSmolvm is smolvm for a local `create web`: ls answers from a
// file that starts as initial, `machine create` makes web listed (unless
// listed is false: its managed marker then fails), and extra is one more
// case arm (a command to fail). setup's agent step is stubbed.
func fakeCreateSmolvm(t *testing.T, initial, extra string, listed bool) {
	t.Helper()
	dir := t.TempDir()
	ls, log := filepath.Join(dir, "ls.json"), filepath.Join(dir, "smolvm.log")
	if err := os.WriteFile(ls, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	made := ":"
	if listed {
		made = "echo '[{\"name\":\"web\",\"state\":\"running\",\"created_at\":100}]' > " + ls
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"  'machine ls --json') cat " + ls + ";;\n" +
		"  'machine create'*) " + made + ";;\n" +
		extra +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "smolvm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stubBrowser(t, log)
}

// A local create of web that ends each way a hub's create can: made (and
// then failing: setup, a clone, the marker) or refused before anything
// exists. cleared is whether create clears a leftover ports file.
var createOutcomes = []struct {
	name, toml, initial, extra string
	listed, cleared            bool
}{
	{"ok", "", "[]", "", true, true},
	{"setup failed", "[templates.default]\nhook = \"cmd:/opt/hook-fails\"\n", "[]", "  *hook-fails*) exit 1;;\n", true, true},
	{"clone failed", "[templates.default]\nrepos = [\"o/bad\"]\n", "[]", "  *o/bad*) exit 1;;\n", true, true},
	{"marker failed", "", "[]", "", false, true},
	{"an entry has the name", "[machines.web]\nbackend = \"remote\"\nhost = \"w.example\"\n", "[]", "", true, false},
	{"a smol VM has the name", "", `[{"name":"web","state":"running","created_at":5}]`, "", true, false},
}

// runLocalCreate runs `create web` for one outcome with a leftover saved
// forward, and reports create's error and whether the leftover went.
func runLocalCreate(t *testing.T, tc int) (error, bool) {
	t.Helper()
	o := createOutcomes[tc]
	a := newTestApp(t)
	fakeCreateSmolvm(t, o.initial, o.extra, o.listed)
	writeDevvmTOML(t, a, o.toml)
	if err := config.SavePorts(a.Paths, "web", []string{"1"}); err != nil {
		t.Fatal(err)
	}
	err := runTree(t, a, "create", "web", "--memory", "1024", "--yes", "--no-ports", "--port", "9")
	got, _ := config.LoadPorts(a.Paths, "web")
	return err, len(got) == 0
}

// create clears a leftover ports file once its VM exists, however it ends
// after that, and keeps the file of a name it refuses; --port and
// --no-ports are hidden and ignored (a laptop one release older still
// sends --no-ports). Every error after the VM exists carries wasCreated,
// which a laptop reads in its hub's output; no refusal does.
func TestCreateClearsLeftoverPorts(t *testing.T) {
	for i, o := range createOutcomes {
		t.Run(o.name, func(t *testing.T) {
			err, cleared := runLocalCreate(t, i)
			if cleared != o.cleared {
				t.Errorf("leftover cleared = %v, want %v (err %v)", cleared, o.cleared, err)
			}
			if (o.name == "ok") != (err == nil) {
				t.Fatalf("err = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), wasCreated("web")) != o.cleared {
				t.Errorf("err = %q: says %q = %v, want %v", err, wasCreated("web"), !o.cleared, o.cleared)
			}
		})
	}
}

// The hub side of the proxied form: --inline-template makes the flags the
// whole template, ignoring this host's own file.
func TestInlineTemplateIgnoresHostFile(t *testing.T) {
	a := newTestApp(t)
	log := fakeManagedSmol(t, a, "web")
	stubBrowser(t, log)
	writeDevvmTOML(t, a, "[templates.default]\nhook = \"cmd:/hub-own\"\n")
	if err := runTree(t, a, "setup", "web", "--inline-template", "--hook=cmd:/laptop", "--repo=o/r", "--user=dev", "--harden=false"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), "hub-own") || !strings.Contains(string(data), "/laptop") {
		t.Errorf("host file leaked into an inline setup:\n%s", data)
	}
}

func TestAdoptHintIsCurrent(t *testing.T) {
	h := backend.AdoptHint("web")
	if strings.Contains(h, "next release") || strings.Contains(h, "(") || !strings.Contains(h, "devvm setup --adopt web") {
		t.Errorf("AdoptHint = %q", h)
	}
}

// A failed clone does not end setup (simplify.md step 2): every repo is
// tried, keys and hardening ran before, and setup ends in
// setupIncomplete, exit status 3.
func TestSetupFinishesPastFailedClone(t *testing.T) {
	a := newTestApp(t)
	log := fakeRemote(t, `case "$last" in *o/bad*) echo "clone broke" >&2; exit 1;; esac`)
	key := filepath.Join(t.TempDir(), "k.pub")
	if err := os.WriteFile(key, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBxM5xX4pQ9bJ8u5Yb6m8yRZ3qWZJ1hX0b0W9b3G7k5f me@laptop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeServer(t, a, "cloud", "cloud.example", `
[templates.default]
repos = ["o/bad", "o/good"]
authorized_keys = ["`+key+`"]
harden = true
`)
	err := runTree(t, a, "setup", "cloud")
	var si *setupIncomplete
	if !errors.As(err, &si) || err.Error() != "setup finished, but: failed to clone o/bad" {
		t.Fatalf("err = %v, want setupIncomplete for o/bad", err)
	}
	if failureStatus(err) != exitSetupIncomplete || failureStatus(errors.New("x")) != 1 {
		t.Errorf("failureStatus = %d / %d", failureStatus(err), failureStatus(errors.New("x")))
	}
	data, _ := os.ReadFile(log)
	inOrder(t, string(data), "authorized_keys.devvm", "AllowUsers", "o/bad", "o/good")
	if out := a.Stdout.(*bytes.Buffer).String(); strings.Contains(out, "is set up") {
		t.Errorf("setup said it is set up: %q", out)
	}
}

// A smol create whose setup ends in setupIncomplete reports the VM as
// created, with the problem, not as "setup failed".
func TestCreateSmolCloneFailure(t *testing.T) {
	a := newTestApp(t)
	dir := t.TempDir()
	ls, log := filepath.Join(dir, "ls.json"), filepath.Join(dir, "smolvm.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"  'machine ls --json') cat " + ls + " 2>/dev/null || echo '[]';;\n" +
		"  'machine create'*) echo '[{\"name\":\"web\",\"state\":\"running\",\"created_at\":100}]' > " + ls + ";;\n" +
		"  *o/bad*) exit 1;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "smolvm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stubBrowser(t, log)
	writeDevvmTOML(t, a, "[templates.default]\nrepos = [\"o/bad\"]\n")
	err := runTree(t, a, "create", "web", "--memory", "1024", "--yes")
	var si *setupIncomplete
	if !errors.As(err, &si) || err.Error() != "'web' was created; setup finished, but: failed to clone o/bad" {
		t.Fatalf("err = %v", err)
	}
	if failureStatus(err) != exitSetupIncomplete {
		t.Errorf("failureStatus = %d", failureStatus(err))
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "Machine 'web' (smol) is ready.") {
		t.Errorf("no closing hints: %q", out)
	}
}

// A proxied create clears this host's leftover HUB@NAME forwards when the
// hub made the VM: it exited 0, or its error says wasCreated (setup
// failed: 1; a clone failed: 3, passed on as is). A refusal keeps them.
// The hub's output is the real create's, run here first: a wording change
// that drops wasCreated breaks this.
func TestProxiedCreateClearsWhenHubMadeVM(t *testing.T) {
	type hubSays struct {
		msg     string
		code    int
		cleared bool
	}
	var says []hubSays
	for i, o := range createOutcomes {
		err, _ := runLocalCreate(t, i)
		h := hubSays{cleared: o.cleared}
		if err != nil {
			h.msg, h.code = "devvm: "+err.Error(), failureStatus(err)
		}
		says = append(says, h)
	}
	a := newTestApp(t)
	fakeHubWith(t, a, t.TempDir(), `[ -z "$FAKE_HUB_MSG" ] || printf '%s\n' "$FAKE_HUB_MSG" >&2; exit ${FAKE_DEVVM_EXIT:-0}`+"\n")
	pinTTY(t, false)
	for i, h := range says {
		if err := config.SavePorts(a.Paths, "h/web", []string{"3000"}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("FAKE_HUB_MSG", h.msg)
		t.Setenv("FAKE_DEVVM_EXIT", strconv.Itoa(h.code))
		err := runTree(t, a, "create", "h/web", "--yes")
		var ec *exitCode
		if (h.code == 0) != (err == nil) || (err != nil && (!errors.As(err, &ec) || ec.code != h.code)) {
			t.Errorf("%s: err = %v, want the hub's status %d", createOutcomes[i].name, err, h.code)
		}
		if got, _ := config.LoadPorts(a.Paths, "h/web"); (len(got) == 0) != h.cleared {
			t.Errorf("%s: laptop forwards = %v, want cleared %v", createOutcomes[i].name, got, h.cleared)
		}
	}
	// setup never touches them, exit 3 or not.
	t.Setenv("FAKE_HUB_MSG", says[2].msg)
	t.Setenv("FAKE_DEVVM_EXIT", "3")
	err := runTree(t, a, "setup", "h/web", "--yes")
	var ec *exitCode
	if !errors.As(err, &ec) || ec.code != exitSetupIncomplete {
		t.Fatalf("setup: err = %v, want the hub's exit status 3", err)
	}
	if got, _ := config.LoadPorts(a.Paths, "h/web"); len(got) != 1 {
		t.Errorf("laptop forwards = %v after setup, want them untouched", got)
	}
}

// A ^C during the clones is an interrupt: the clone in flight is killed,
// no further repo is tried, and setup does not report "finished, but"
// (exit 3). The fake's SIGINT reaches this process, which setup's
// interruptContext catches, as it would a terminal's ^C.
func TestSetupInterruptedWhileCloning(t *testing.T) {
	a := newTestApp(t)
	log := fakeRemote(t, `case "$last" in *o/a*) kill -INT $PPID; exec sleep 10;; esac`)
	fakeServer(t, a, "cloud", "cloud.example", "[templates.default]\nrepos = [\"o/a\", \"o/b\"]\n")
	err := runTree(t, a, "setup", "cloud")
	var si *setupIncomplete
	if err == nil || errors.As(err, &si) || failureStatus(err) == exitSetupIncomplete ||
		!strings.Contains(err.Error(), "setup was interrupted while cloning") {
		t.Fatalf("err = %v, want an interrupt", err)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), "o/a") || strings.Contains(string(data), "o/b") {
		t.Errorf("clones after the interrupt:\n%s", data)
	}
}

// A guest gh with no working login: setup skips the owner/repo specs (the
// clone script exits ghNoLoginExit for them), names them in one line with
// the fix, still clones the URL specs, and succeeds (simplify.md step 3).
func TestSetupSkipsGHLoggedOut(t *testing.T) {
	a := newTestApp(t)
	log := fakeRemote(t, `case "$last" in *o/r*|*o/s*) exit 86;; esac`)
	fakeServer(t, a, "cloud", "cloud.example", `
[templates.default]
repos = ["o/r", "https://x.example/u.git", "o/s"]
`)
	if err := runTree(t, a, "setup", "cloud"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	inOrder(t, string(data), "o/r", "https://x.example/u.git", "o/s")
	want := "devvm: skipped o/r, o/s: gh in the box has no working login; run 'gh auth login' in the box, then clone them by hand\n"
	if w := a.Stderr.(*bytes.Buffer).String(); strings.Count(w, want) != 1 || strings.Contains(w, "failed to clone") {
		t.Errorf("stderr = %q, want the one skip line", w)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "devvm: 'cloud' is set up\n") {
		t.Errorf("stdout = %q", out)
	}
}

// loginWrapBackend runs argv locally the way the real backends run a
// Login exec: ssh's `bash -lc 'ARGV'` (remoteCommand), or smol's
// `bash -lc 'exec "$@"' _ ARGV` (guestArgv).
type loginWrapBackend struct {
	*localCopyBackend
	smol bool
}

func (b *loginWrapBackend) Run(ctx context.Context, o backend.ExecOpts, argv ...string) error {
	if o.Login {
		if b.smol {
			argv = append([]string{"bash", "-lc", `exec "$@"`, "_"}, argv...)
		} else {
			q := make([]string, len(argv))
			for i, t := range argv {
				q[i] = sq(t)
			}
			argv = []string{"bash", "-lc", strings.Join(q, " ")}
		}
		o.Login = false
	}
	return b.localCopyBackend.Run(ctx, o, argv...)
}

// The clone script itself, run locally under both backends' login wraps:
// gh's login is checked only for an owner/repo spec that is not cloned yet
// (an existing checkout is never reported as skipped), a gh with no login
// exits ghNoLoginExit, and a URL spec clones with git whatever gh says.
// The ~/.bash_logout ends in a failing command, as Ubuntu's stock one does
// without a console (clear_console): a script run as a login shell of its
// own, under set -e, would then exit 1 for every `exit N`.
func TestCloneScriptGHNoLogin(t *testing.T) {
	for _, smol := range []bool{false, true} {
		a := newTestApp(t)
		home, bin := t.TempDir(), t.TempDir()
		for name, body := range map[string]string{
			"gh":  `[ "$1" = auth ] && exit 1; echo "gh $*" >&2; exit 0`,
			"git": `[ "$1" = clone ] || exit 1; d="${2##*/}"; mkdir "${d%.git}"`,
		} {
			if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		// bash -l resets PATH (/etc/profile, macOS's path_helper): restate it.
		profile := "PATH=" + sq(bin+":"+os.Getenv("PATH")) + "\n"
		for f, body := range map[string]string{".bash_profile": profile, ".bash_logout": "false\n"} {
			if err := os.WriteFile(filepath.Join(home, f), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll(filepath.Join(home, "src", "done"), 0o755); err != nil {
			t.Fatal(err)
		}
		a.Stderr = new(bytes.Buffer)
		b := &loginWrapBackend{localCopyBackend: &localCopyBackend{home: home}, smol: smol}
		failed, err := a.cloneRepos(context.Background(), b, []string{"o/done", "o/r", "https://x.example/u.git"})
		if err != nil || len(failed) != 0 {
			t.Fatalf("smol=%v: failed %v, err %v\n%s", smol, failed, err, a.Stderr)
		}
		if _, err := os.Stat(filepath.Join(home, "src", "u")); err != nil {
			t.Errorf("smol=%v: URL spec not cloned: %v", smol, err)
		}
		if w := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(w, "devvm: skipped o/r: ") || strings.Contains(w, "o/done") || strings.Contains(w, "gh repo") {
			t.Errorf("smol=%v: stderr = %q", smol, w)
		}
	}
}

// shape's one backstop: a box devvm did not make is refused before any step
// runs (the nil backend would panic on the first one).
func TestShapeRefusesUnmanaged(t *testing.T) {
	a := newTestApp(t)
	for _, m := range []*config.Machine{{Name: "vm", Backend: config.BackendSmol}, config.NewRemote("box", "dev@box")} {
		err := a.shape(context.Background(), m, nil, config.Template{Name: config.DefaultTemplate, Hook: "none"}, setupOpts{})
		if err == nil || err.Error() != "'"+m.Name+"' is not a box devvm made: devvm never shapes it" {
			t.Errorf("%s: err = %v", m.Name, err)
		}
	}
}
