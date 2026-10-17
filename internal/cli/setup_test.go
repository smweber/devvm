package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/reach"
)

func writeDevvmTOML(t *testing.T, a *App, body string) {
	t.Helper()
	if err := os.MkdirAll(a.Paths.Config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.Paths.FilePath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Precedence: flag > --template > the old conf > [templates.default] >
// config.toml > built-in, with the named template falling back to default
// one level.
func TestResolveTemplatePrecedence(t *testing.T) {
	a := newTestApp(t)
	writeDevvmTOML(t, a, `
[templates.default]
hook = "cmd:/default"
memory = 4096
repos = ["o/default"]
ports = ["3000"]

[templates.web]
memory = 8192
harden = true
`)
	if err := config.SaveDefaults(a.Paths.Config, &config.Defaults{BootstrapHook: "cmd:/toml", Memory: 1024, Disk: 7}); err != nil {
		t.Fatal(err)
	}
	conf := config.NewRemote("cloud", config.BackendRemoteManaged, "dev@c")
	conf.BootstrapHook = "cmd:/conf"
	conf.AuthorizedKeysGithub = []string{"someone"}

	// No --template: the conf beats default, default beats config.toml.
	got, err := a.resolveTemplate("", tmplFlags{}, conf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "default" || got.Hook != "cmd:/conf" || got.Memory != 4096 || got.Disk != 7 ||
		!slices.Equal(got.Repos, []string{"o/default"}) || !slices.Equal(got.AuthorizedKeysGithub, []string{"someone"}) ||
		got.User != "dev" {
		t.Errorf("default = %+v", got)
	}
	// --template web: its own fields beat the conf; the rest fall through.
	got, err = a.resolveTemplate("web", tmplFlags{}, conf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Memory != 8192 || !got.Harden || got.Hook != "cmd:/conf" || !slices.Equal(got.Ports, []string{"3000"}) {
		t.Errorf("web = %+v", got)
	}
	// --template default is no flag at all: the conf still beats it.
	if got, err := a.resolveTemplate("default", tmplFlags{}, conf); err != nil || got.Hook != "cmd:/conf" || got.Name != "default" {
		t.Errorf("--template default = %+v, %v", got, err)
	}
	// Flags beat everything, a false bool included.
	got, err = a.resolveTemplate("web", tmplFlags{Memory: 512, Hook: "none", Harden: false, HardenSet: true,
		Repos: []string{"o/flag"}, User: "alice"}, conf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Memory != 512 || got.Hook != "none" || got.Harden || !slices.Equal(got.Repos, []string{"o/flag"}) || got.User != "alice" {
		t.Errorf("flags = %+v", got)
	}
	// Inline: the flags are the whole template, nothing on this host fills in.
	got, err = a.resolveTemplate("web", tmplFlags{Inline: true, Hook: "cmd:/x"}, conf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Memory != 0 || got.Harden || len(got.Repos) != 0 || got.Hook != "cmd:/x" || got.User != "dev" {
		t.Errorf("inline = %+v", got)
	}
	// Bad values are refused before anything runs.
	for _, f := range []tmplFlags{{Hook: "bogus"}, {Ports: []string{"x"}}, {User: "root"}, {Memory: 1}} {
		if _, err := a.resolveTemplate("", f, nil); err == nil {
			t.Errorf("%+v accepted", f)
		}
	}
	if _, err := a.resolveTemplate("nope", tmplFlags{}, nil); err == nil {
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
ports = ["5173"]
`)
	s := createSpec{Name: "box", Template: "big", Yes: true, tmplFlags: tmplFlags{Disk: 3}}
	if err := a.gatherCreateSpec(&s); err != nil {
		t.Fatal(err)
	}
	if s.Backend != config.BackendSmol || s.Memory != 8192 || s.Disk != 3 || s.Hook != "cmd:/d" ||
		!slices.Equal(s.Repos, []string{"o/r"}) || !slices.Equal(s.Ports, []string{"5173"}) {
		t.Errorf("spec = %+v", s)
	}
	if tt := s.template(); tt.Name != "big" || tt.User != "dev" || tt.Memory != 8192 {
		t.Errorf("template = %+v", tt)
	}
	// A hub reads no template.
	h := createSpec{Name: "h", Backend: config.BackendHub, SSHHost: "u@h", Template: "big", Yes: true}
	if err := a.gatherCreateSpec(&h); err == nil {
		t.Error("--template on a hub accepted")
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
ports = ["3000"]
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
		"apt-get install -y build-essential ca-certificates curl git rsync tmux mosh", "gh", // prereqs
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
	// A re-run merges the template's forwards into this host's own.
	if got, _ := config.LoadPorts(a.Paths, "web"); !slices.Equal(got, []string{"4000", "3000"}) {
		t.Errorf("ports = %v, want the template's merged in", got)
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
	for _, b := range []string{config.BackendRemoteManaged, config.BackendRemoteUnmanaged} {
		m := config.NewRemote(strings.TrimPrefix(b, "remote-"), b, "dev@x")
		if err := m.Save(a.Paths.Config); err != nil {
			t.Fatal(err)
		}
	}
	pinTTYs(t, true, true)
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"setup", "web"}, "devvm setup --adopt web"},
		{[]string{"setup", "--adopt", "made"}, "already managed"},
		{[]string{"setup", "--adopt", "managed"}, "--adopt applies only"},
		{[]string{"setup", "--adopt", "unmanaged"}, "--adopt applies only"},
		{[]string{"setup", "unmanaged"}, "never shapes"},
		{[]string{"setup", "--adopt", "--yes", "web"}, "--yes does not apply"},
		{[]string{"setup", "--host", "1.2.3.4", "made"}, "--host applies only"},
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
	writeDevvmTOML(t, a, "[templates.default]\nhook = \"cmd:/opt/h\"\nrepos = [\"o/r\"]\nports = [\"3000\"]\n")
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
		"run the hook: cmd:/opt/h", "clone into ~/src: o/r", "save forwards: 3000",
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
	if got, _ := config.LoadPorts(a.Paths, "web"); !slices.Equal(got, []string{"3000"}) {
		t.Errorf("ports = %v: a fresh adoption replaces a stale file", got)
	}
}

// fakeRemote is a fake ssh for the remote flow: it logs its argv, answers
// `id -un` with the destination's user, and runs body first (dest is the
// destination, $last the remote command).
func fakeRemote(t *testing.T, body string) string {
	t.Helper()
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "")
	return fakeSSH(t, `eval "dest=\${$(($# - 1))}"
`+body+`
case "$last" in *"'id' '-un'"*|"id -un"*|*"id -un"*) echo "${dest%%@*}";; esac
exit 0`)
}

func shrinkReachWait(t *testing.T, budget, interval time.Duration) {
	t.Helper()
	ob, oi := setupReachBudget, setupReachInterval
	setupReachBudget, setupReachInterval = budget, interval
	t.Cleanup(func() { setupReachBudget, setupReachInterval = ob, oi })
}

// The remote order: reach, prereqs, hook, repos, keys, harden; no agent.
func TestSetupRemoteOrder(t *testing.T) {
	a := newTestApp(t)
	log := fakeRemote(t, "")
	key := filepath.Join(t.TempDir(), "k.pub")
	if err := os.WriteFile(key, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBxM5xX4pQ9bJ8u5Yb6m8yRZ3qWZJ1hX0b0W9b3G7k5f me@laptop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDevvmTOML(t, a, `
[templates.default]
hook = "cmd:/opt/hook.sh"
repos = ["o/r"]
memory = 4096
authorized_keys = ["`+key+`"]
harden = true
fail2ban = true
`)
	m := config.NewRemote("cloud", config.BackendRemoteManaged, "dev@cloud.example")
	if err := m.Save(a.Paths.Config); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "setup", "cloud"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	inOrder(t, string(data),
		"BatchMode=yes", "dev@cloud.example 'true'", // reach
		"'id' '-un'",                   // login user
		"build-essential", "tmux mosh", // prereqs
		"/opt/hook.sh",                  // hook
		"gh repo clone",                 // repos
		"authorized_keys.devvm",         // keys
		"WANT_FAIL2BAN=1", "AllowUsers", // harden
	)
	for _, bad := range []string{"root@", "devvm-agent", "profile.d"} {
		if strings.Contains(string(data), bad) {
			t.Errorf("remote setup mentions %q", bad)
		}
	}
	if w := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(w, "ignoring memory (not applicable to a remote box)") {
		t.Errorf("warning = %q", w)
	}
	// Every step above ran over ssh, so the box is a verified contact now.
	if e := reach.Read(a.Paths, "cloud"); e.State != reach.Reachable || e.How != "setup" {
		t.Errorf("setup recorded %+v, want reachable/setup", e)
	}
}

// Root adoption: the configured login is refused (permission denied), root
// answers, so the template's user is created and setup continues as it.
// The old conf's ssh_host gets the user part rewritten, its address kept.
func TestSetupRootAdoption(t *testing.T) {
	a := newTestApp(t)
	log := fakeRemote(t, `case "$dest" in
  root@*|alice@*) ;;
  *) echo "$dest: Permission denied (publickey)." >&2; exit 255;;
esac`)
	writeDevvmTOML(t, a, "[templates.ops]\nuser = \"alice\"\n")
	m := config.NewRemote("cloud", config.BackendRemoteManaged, "203.0.113.5")
	if err := m.Save(a.Paths.Config); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "setup", "cloud", "--template", "ops"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	inOrder(t, string(data),
		" 203.0.113.5 'true'", " root@203.0.113.5 'true'", // denied, then root
		"root@203.0.113.5 'bash' '-c'", "useradd", "'alice'", // EnsureUser("alice") as root
		"alice@203.0.113.5", "build-essential", // prereqs as alice
	)
	if strings.Contains(string(data), "/home/dev") {
		t.Error("EnsureUser hardcoded /home/dev")
	}
	got, err := config.Load(a.Paths.Config, "cloud")
	if err != nil || got.SSHHost != "alice@203.0.113.5" {
		t.Errorf("conf ssh_host = %q (%v), want alice@203.0.113.5", got.SSHHost, err)
	}
}

// Unreachable is not permission denied: it is waited for (bounded), and
// root is never tried.
func TestSetupUnreachable(t *testing.T) {
	a := newTestApp(t)
	shrinkReachWait(t, 300*time.Millisecond, 50*time.Millisecond)
	log := fakeRemote(t, `echo "ssh: connect to host cloud.example port 22: Connection refused" >&2; exit 255`)
	m := config.NewRemote("cloud", config.BackendRemoteManaged, "dev@cloud.example")
	if err := m.Save(a.Paths.Config); err != nil {
		t.Fatal(err)
	}
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

// A box that comes up within the budget is shaped; --host reaches it at
// another address for this run and writes nothing.
func TestSetupWaitsAndHostOverride(t *testing.T) {
	a := newTestApp(t)
	shrinkReachWait(t, 5*time.Second, 20*time.Millisecond)
	count := filepath.Join(t.TempDir(), "n")
	log := fakeRemote(t, `case "$last" in "'true'")
  n=$(cat `+count+` 2>/dev/null || echo 0); n=$((n+1)); echo $n > `+count+`
  [ $n -ge 3 ] || { echo "Connection timed out" >&2; exit 255; };;
esac`)
	m := config.NewRemote("cloud", config.BackendRemoteManaged, "dev@cloud.tail")
	if err := m.Save(a.Paths.Config); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(config.MachinesDir(a.Paths.Config), "cloud.toml"))
	if err := runTree(t, a, "setup", "cloud", "--host", "198.51.100.7"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), "cloud.tail") || !strings.Contains(string(data), "dev@198.51.100.7 'sudo' 'bash'") {
		t.Errorf("--host not used for every call:\n%s", data)
	}
	if !strings.Contains(a.Stderr.(*bytes.Buffer).String(), "waiting up to") {
		t.Error("no wait notice")
	}
	after, _ := os.ReadFile(filepath.Join(config.MachinesDir(a.Paths.Config), "cloud.toml"))
	if !bytes.Equal(before, after) {
		t.Errorf("--host was written:\n%s", after)
	}
}

// probeSSH tells permission denied from unreachable by stderr, not by the
// exit status (255 for both).
func TestProbeSSH(t *testing.T) {
	a := newTestApp(t)
	m := config.NewRemote("c", config.BackendRemoteManaged, "dev@c")
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

// Root adoption re-checks the new user before writing the conf: a user that
// already existed with other keys refuses us, and nothing is written.
func TestSetupRootAdoptionUserRefuses(t *testing.T) {
	a := newTestApp(t)
	fakeRemote(t, `case "$dest" in
  root@*) ;;
  *) echo "$dest: Permission denied (publickey)." >&2; exit 255;;
esac`)
	m := config.NewRemote("cloud", config.BackendRemoteManaged, "203.0.113.5")
	if err := m.Save(a.Paths.Config); err != nil {
		t.Fatal(err)
	}
	err := runTree(t, a, "setup", "cloud")
	if err == nil || !strings.Contains(err.Error(), "user dev exists on 'cloud' but your key is not in its authorized_keys") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := config.Load(a.Paths.Config, "cloud"); got.SSHHost != "203.0.113.5" {
		t.Errorf("conf written: ssh_host = %q", got.SSHHost)
	}
}

// A configured login that is neither root nor the template's user wins,
// with a warning that the template user is ignored.
func TestSetupOtherLoginWarns(t *testing.T) {
	a := newTestApp(t)
	fakeRemote(t, "")
	m := config.NewRemote("cloud", config.BackendRemoteManaged, "ubuntu@cloud.example")
	if err := m.Save(a.Paths.Config); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "setup", "cloud"); err != nil {
		t.Fatal(err)
	}
	if w := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(w, `logs in as ubuntu: template user "dev" is ignored, and hardening would pin AllowUsers to ubuntu`) {
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
// this host's state for HUB@NAME, seeded here.
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
ports = ["3000", "5173"]
harden = true
`)
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
	if got, _ := config.LoadPorts(a.Paths, "h/web"); !slices.Equal(got, []string{"3000", "5173"}) {
		t.Errorf("laptop forwards for h/web = %v", got)
	}

	// create: the same set, and its forwards replace a stale list.
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
	if got, _ := config.LoadPorts(a.Paths, "h/api"); !slices.Equal(got, []string{"3000", "5173"}) {
		t.Errorf("laptop forwards for h/api = %v", got)
	}

	// Without --template (or with --template default, the same thing) the
	// leaf goes across as typed, except --port: forwards stay this host's.
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
	if got, _ := config.LoadPorts(a.Paths, "h/web"); !slices.Equal(got, []string{"3000", "5173", "8080"}) {
		t.Errorf("laptop forwards for h/web after --port = %v", got)
	}

	// --adopt is fresh: the laptop's stale list is replaced.
	os.Remove(argvLog)
	if err := config.SavePorts(a.Paths, "h/old", []string{"1"}); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "setup", "--adopt", "h/old", "-t", "devvm"); err != nil {
		t.Fatal(err)
	}
	if got, _ := config.LoadPorts(a.Paths, "h/old"); !slices.Equal(got, []string{"3000", "5173"}) {
		t.Errorf("adopted h/old forwards = %v, want the template's alone", got)
	}
}

// A template's keys are resolved for a hub machine only when it may take
// them: never for a smol one (no GitHub fetch, no warning every run).
func TestProxiedKeysSkipSmol(t *testing.T) {
	a := newTestApp(t)
	_, argvLog := fakeHub(t, a)
	pinTTY(t, false)
	key := filepath.Join(t.TempDir(), "k.pub")
	line := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBxM5xX4pQ9bJ8u5Yb6m8yRZ3qWZJ1hX0b0W9b3G7k5f me@laptop"
	if err := os.WriteFile(key, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDevvmTOML(t, a, "[templates.k]\nauthorized_keys = [\""+key+"\"]\n")
	if err := writeHubCache(a.Paths, "h", []hubRow{
		{name: "web", backend: "smol", state: "running", forwards: "-", managed: "managed"},
		{name: "box", backend: config.BackendRemoteManaged, state: "reachable", forwards: "-", managed: "managed"},
	}); err != nil {
		t.Fatal(err)
	}
	for vm, want := range map[string]bool{"web": false, "new": false, "box": true} {
		os.Remove(argvLog)
		if err := runTree(t, a, "setup", "h/"+vm, "-t", "k"); err != nil {
			t.Fatal(err)
		}
		calls := devvmCalls(t, argvLog)
		if got := len(calls) == 1 && slices.Contains(calls[0], "--key="+line); got != want {
			t.Errorf("%s: key sent = %v, want %v (%q)", vm, got, want, calls)
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

// On the hub side, --no-ports (sent on every proxied create) saves no
// forwards and asks for none: they are the laptop's.
func TestCreateNoPortsOnHub(t *testing.T) {
	a := newTestApp(t)
	if err := config.SavePorts(a.Paths, "box", []string{"1"}); err != nil {
		t.Fatal(err)
	}
	writeDevvmTOML(t, a, "[templates.default]\nmemory = 1024\nports = [\"3000\"]\n")
	s := createSpec{Name: "box", Yes: true, tmplFlags: tmplFlags{NoPorts: true, Ports: []string{"9"}}}
	if err := a.gatherCreateSpec(&s); err != nil {
		t.Fatal(err)
	}
	if len(s.Ports) != 0 || s.template().Ports != nil {
		t.Errorf("ports = %v under --no-ports", s.Ports)
	}
}

// The hub side of the proxied form: --inline-template makes the flags the
// whole template, ignoring this host's own file.
func TestInlineTemplateIgnoresHostFile(t *testing.T) {
	a := newTestApp(t)
	log := fakeManagedSmol(t, a, "web")
	stubBrowser(t, log)
	writeDevvmTOML(t, a, "[templates.default]\nhook = \"cmd:/hub-own\"\nports = [\"7000\"]\n")
	if err := runTree(t, a, "setup", "web", "--inline-template", "--hook=cmd:/laptop", "--repo=o/r", "--user=dev", "--harden=false"); err != nil {
		t.Fatalf("setup: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	if strings.Contains(string(data), "hub-own") || !strings.Contains(string(data), "/laptop") {
		t.Errorf("host file leaked into an inline setup:\n%s", data)
	}
	if got, _ := config.LoadPorts(a.Paths, "web"); len(got) != 0 {
		t.Errorf("ports = %v", got)
	}
}

func TestAdoptHintIsCurrent(t *testing.T) {
	h := backend.AdoptHint("web")
	if strings.Contains(h, "next release") || strings.Contains(h, "(") || !strings.Contains(h, "devvm setup --adopt web") {
		t.Errorf("AdoptHint = %q", h)
	}
}
