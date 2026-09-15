package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// fakeSSH puts an `ssh` script first on PATH that appends its argv to a log
// and runs body (a shell snippet; $last is the remote command string). The
// log path is returned so a test can assert what was (or was not) dialed.
func fakeSSH(t *testing.T, body string) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "ssh.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\nfor last; do :; done\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func writeHub(t *testing.T, a *App, name string, tables map[string][]string) {
	t.Helper()
	h := config.NewHub(name, "u@"+name+".example")
	if len(tables) > 0 {
		h.Machines = map[string]config.HubMachine{}
		for m, ports := range tables {
			h.Machines[m] = config.HubMachine{Ports: ports}
		}
	}
	if err := h.Save(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
}

func TestResolveHubMachineNeverDials(t *testing.T) {
	a := newTestApp(t)
	log := fakeSSH(t, "exit 0")
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}})

	m, b, err := a.resolveAny("h/web")
	if err != nil {
		t.Fatalf("resolveAny: %v", err)
	}
	if !m.IsHubMachine() || m.Hub.Name != "h" || m.Ports[0] != "3000" || b.Kind() != config.BackendHub {
		t.Errorf("record = %+v backend %q", m, b.Kind())
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		data, _ := os.ReadFile(log)
		t.Errorf("resolve dialed the hub:\n%s", data)
	}
	// A hub the user has not registered, and a machine with no table, both
	// resolve from the conf alone (or fail on it alone).
	if _, _, err := a.resolveAny("h/api"); err != nil {
		t.Errorf("untabled machine: %v", err)
	}
	if _, _, err := a.resolveAny("nope/web"); err == nil {
		t.Error("unknown hub should fail")
	}
	// The shaping path refuses a hub machine until the proxy lands (step 2)…
	if _, _, err := a.resolve("h/web"); !errors.Is(err, backend.ErrHubProxy) {
		t.Errorf("resolve(h/web) = %v, want ErrHubProxy", err)
	}
	// …and refuses the hub itself for good, with the one shared message.
	if _, _, err := a.resolve("h"); err == nil || err.Error() != "h is a hub, not a machine" {
		t.Errorf("resolve(h) = %v", err)
	}
	if _, _, err := a.resolveAny("h"); err != nil {
		t.Errorf("resolveAny(h) = %v; status/delete need the hub", err)
	}
}

// Every shaping verb goes through resolve, so each fails on a hub with the
// same message — checked through the real command tree so a leaf that
// bypasses resolve would show up here.
func TestShapingVerbsRefuseHub(t *testing.T) {
	a := newTestApp(t)
	fakeSSH(t, "exit 0")
	writeHub(t, a, "h", nil)
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	verbs := [][]string{
		{"bootstrap", "h"}, {"lockdown", "h"}, {"provision", "h"}, {"deprovision", "h", "--yes"},
		{"start", "h"}, {"stop", "h"},
		{"attach", "h"}, {"shell", "h"}, {"exec", "h", "true"}, {"auth", "h"},
		{"cp-in", "h", src, "/tmp"}, {"cp-out", "h", "/etc/hostname", t.TempDir()},
		{"repos", "add", "h", "o/r"}, {"repos", "rm", "h", "o/r"}, {"repos", "list", "h"}, {"repos", "clone", "h"},
		{"ports", "add", "h", "3000"}, {"ports", "rm", "h", "3000"}, {"ports", "list", "h"},
		{"ports", "up", "h"}, {"ports", "down", "h"},
		{"keys", "add", "h", src}, {"keys", "list", "h"}, {"keys", "rm", "h", "x"}, {"keys", "dedupe", "h"},
	}
	for _, argv := range verbs {
		a.Stdout, a.Stderr = new(bytes.Buffer), new(bytes.Buffer)
		root := a.rootCmd()
		root.SetArgs(argv)
		root.SetOut(a.Stdout.(*bytes.Buffer))
		root.SetErr(a.Stderr.(*bytes.Buffer))
		err := root.Execute()
		if err == nil || err.Error() != "h is a hub, not a machine" {
			t.Errorf("%v: err = %v, want the shared guard message", argv, err)
		}
	}
	// A daemon for the hub itself makes no sense either.
	if _, _, err := a.resolve("h"); err == nil {
		t.Error("__daemon resolves through the guard")
	}
}

func TestListMachinesUnion(t *testing.T) {
	a := newTestApp(t)
	if err := config.NewSmol("box").Save(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}, "api": nil})
	// A broken conf is still a name (status renders it as such).
	if err := os.WriteFile(filepath.Join(config.MachinesDir(a.ConfigDir), "bad.toml"), []byte("backend = \"nope\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The hub's cached listing (written by step 3's merged listing): bare
	// names in column 1; anything nested or invalid is ignored.
	if err := os.MkdirAll(config.CacheDir(a.ConfigDir), 0o755); err != nil {
		t.Fatal(err)
	}
	cache := "web\tsmol\trunning\t-\ndb\tsmol\tstopped\t-\nother/x\tsmol\trunning\t-\n\nbad name\tsmol\t?\t-\n"
	if err := os.WriteFile(hubCachePath(a.ConfigDir, "h"), []byte(cache), 0o644); err != nil {
		t.Fatal(err)
	}
	// Live daemons: a hub machine with no entry anywhere else, and a local
	// one (which config.List already covers).
	if err := config.EnsureRuntimeDir(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"h@live.sock", "box.sock", "h@web.sock"} {
		if err := os.WriteFile(filepath.Join(config.RuntimeDir(a.ConfigDir), s), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := a.listMachines()
	want := []string{"bad", "box", "h", "h/api", "h/db", "h/live", "h/web"}
	if !slices.Equal(got, want) {
		t.Errorf("listMachines = %v, want %v", got, want)
	}
	// Completion for status/delete offers the same set.
	comp, _ := a.completeAnyMachine(nil, nil, "")
	if !slices.Equal(comp, want) {
		t.Errorf("completeAnyMachine = %v", comp)
	}
}

func TestHubVersionCheck(t *testing.T) {
	tests := []struct {
		name, out, local string
		refused          bool
		warn             string // substring of the warning; "" means silent
	}{
		{"below floor", "devvm version v0.0.1\n", "v0.1.13", true, ""},
		{"below floor with banner", "Welcome!\ndevvm version v0.1.12\n", "v0.1.13", true, ""},
		{"exact match", "devvm version v0.1.13\n", "v0.1.13", false, ""},
		{"dev hub", "devvm version dev\n", "v0.1.13", false, `"dev", not a release`},
		{"newer hub", "devvm version v9.9.9\n", "v0.1.13", false, "newer devvm (v9.9.9)"},
		{"older supported hub", "devvm version v0.1.13\n", "v0.1.14", false, "older devvm (v0.1.13)"},
		{"dev laptop", "devvm version v0.1.13\n", "dev", false, "local build (dev)"},
		{"unparseable", "bash: devvm: command not found\n", "v0.1.13", false, "could not read"},
		{"empty", "", "v0.1.13", false, "could not read"},
	}
	for _, tt := range tests {
		warn, err := hubVersionCheck(tt.out, tt.local)
		if tt.refused {
			if err == nil {
				t.Errorf("%s: want refusal, got warn=%q", tt.name, warn)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected refusal: %v", tt.name, err)
			continue
		}
		if (tt.warn == "") != (warn == "") || !strings.Contains(warn, tt.warn) {
			t.Errorf("%s: warn = %q, want %q", tt.name, warn, tt.warn)
		}
	}
}

// create --backend hub test-connects, then runs the hub's devvm --version
// through its login shell, and refuses (writing nothing) below the floor.
func TestCreateHubVersionProbe(t *testing.T) {
	a := newTestApp(t)
	a.Stdout, a.Stderr = new(bytes.Buffer), new(bytes.Buffer)
	log := fakeSSH(t, `case "$last" in *devvm*) echo "devvm version $FAKE_DEVVM_VERSION";; esac; exit 0`)
	spec := createSpec{Name: "h", Backend: config.BackendHub, SSHHost: "u@hub", Yes: true}

	t.Setenv("FAKE_DEVVM_VERSION", "v0.0.1")
	if err := a.runCreate(spec); err == nil || !strings.Contains(err.Error(), "v0.0.1") {
		t.Fatalf("create with an old hub: err = %v", err)
	}
	if config.Exists(a.ConfigDir, "h") {
		t.Fatal("a refused hub must not be registered")
	}
	calls, _ := os.ReadFile(log)
	probe := ""
	for _, line := range strings.Split(string(calls), "\n") {
		if strings.Contains(line, "devvm") {
			probe = line
		}
	}
	for _, want := range []string{"BatchMode=yes", `-lc "$0"`, "devvm", "--version", "u@hub"} {
		if !strings.Contains(probe, want) {
			t.Errorf("version probe %q lacks %q", probe, want)
		}
	}

	// A dev build on the hub is accepted with a warning, and the conf lands.
	// The refused run above already touched the change marker (its defer runs
	// regardless), so clear it to see this run's own touch.
	if err := os.Remove(config.ChangedPath(a.ConfigDir)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_DEVVM_VERSION", "dev")
	if err := a.runCreate(spec); err != nil {
		t.Fatalf("create with a dev hub: %v", err)
	}
	if !strings.Contains(a.Stderr.(*bytes.Buffer).String(), "not a release") {
		t.Errorf("no warning for a dev hub: %s", a.Stderr.(*bytes.Buffer).String())
	}
	h, err := config.Load(a.ConfigDir, "h")
	if err != nil || !h.IsHub() || h.SSHHost != "u@hub" {
		t.Fatalf("saved hub = %+v, %v", h, err)
	}
	if _, err := os.Stat(config.ChangedPath(a.ConfigDir)); err != nil {
		t.Error("create did not touch the change marker")
	}
}

func TestStatusPlainHubRows(t *testing.T) {
	a := newTestApp(t)
	log := fakeSSH(t, "exit 1") // status never dials in step 1
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}})
	var out bytes.Buffer
	a.Stdout = &out
	if err := a.runStatusPlain(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		data, _ := os.ReadFile(log)
		t.Errorf("status dialed the hub:\n%s", data)
	}
	// The hub row uses the existing `reachable` token like every remote; a
	// hub machine's state is `?` until the merged listing (step 3), and its
	// forwards column is this host's own (none configured → down).
	want := "h\thub\treachable\t-\nh/web\thub\t?\tdown\n"
	if out.String() != want {
		t.Errorf("plain =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestDeleteHubMachineDropsTable(t *testing.T) {
	a := newTestApp(t)
	a.Stdout = new(bytes.Buffer)
	pinTTY(t, false)
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "")
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}, "api": {"80"}})
	// The hub's delete runs first and is the one that confirms (through the
	// forwarded tty); when it refuses — here, exit 1 — nothing local changes
	// and its status comes back as the proxied exit.
	log := fakeSSH(t, "exit 1")
	err := a.runDelete("h/web", false)
	var pe *proxyExit
	if !errors.As(err, &pe) || pe.code != 1 {
		t.Fatalf("delete h/web with the hub refusing: err = %v, want proxyExit 1", err)
	}
	h, err := config.Load(a.ConfigDir, "h")
	if err != nil {
		t.Fatal(err)
	}
	if _, still := h.Machines["web"]; !still || len(h.Machines) != 2 {
		t.Errorf("a refused delete changed the tables: %v", h.Machines)
	}
	if lines := sshLines(t, log); len(lines) != 1 || lines[0] != wantSSH(a, false, "delete", "web") {
		t.Errorf("proxied delete argv:\n%s\nwant\n%s", strings.Join(lines, "\n"), wantSSH(a, false, "delete", "web"))
	}
	// With the hub agreeing, the table goes; a machine with no local state
	// is still proxied (its registry entry is the hub's), and just drops
	// nothing here.
	fakeSSH(t, "exit 0")
	if err := a.runDelete("h/web", false); err != nil {
		t.Fatalf("delete h/web: %v", err)
	}
	h, _ = config.Load(a.ConfigDir, "h")
	if _, still := h.Machines["web"]; still || len(h.Machines) != 1 {
		t.Errorf("tables after delete: %v", h.Machines)
	}
	if err := a.runDelete("h/other", false); err != nil {
		t.Errorf("delete of an unrecorded machine: err = %v", err)
	}
	if _, err := os.Stat(config.ChangedPath(a.ConfigDir)); err != nil {
		t.Error("delete did not touch the change marker")
	}
}

// Verbs that refuse hubs do not offer them; status/delete offer everything.
func TestCompletionOmitsHubsForShapingVerbs(t *testing.T) {
	a := newTestApp(t)
	if err := config.NewSmol("box").Save(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}})
	got, _ := a.completeMachines(nil, nil, "")
	if want := []string{"box", "h/web"}; !slices.Equal(got, want) {
		t.Errorf("completeMachines = %v, want %v", got, want)
	}
	got, _ = a.completeAnyMachine(nil, nil, "")
	if want := []string{"box", "h", "h/web"}; !slices.Equal(got, want) {
		t.Errorf("completeAnyMachine = %v, want %v", got, want)
	}
	// The wiring: attach filters, status and delete do not.
	root := a.rootCmd()
	for _, tt := range []struct {
		verb    string
		wantHub bool
	}{{"attach", false}, {"bootstrap", false}, {"status", true}, {"delete", true}} {
		c, _, err := root.Find([]string{tt.verb})
		if err != nil {
			t.Fatal(err)
		}
		names, _ := c.ValidArgsFunction(c, nil, "")
		if slices.Contains(names, "h") != tt.wantHub {
			t.Errorf("%s completion = %v, want hub offered: %v", tt.verb, names, tt.wantHub)
		}
	}
}

// Shaping flags on `create --backend hub` are refused with validateHub's own
// field list rather than silently dropped, so the flag path and a hand-edited
// conf agree on what a hub may carry.
func TestCreateHubRefusesShapingFlags(t *testing.T) {
	a := newTestApp(t)
	fakeSSH(t, "exit 0")
	spec := createSpec{Name: "h", Backend: config.BackendHub, SSHHost: "u@hub", Yes: true,
		Memory: 4096, Disk: 20, BootstrapHook: "cmd:/x", Ports: []string{"3000"}, Repos: []string{"o/r"}}
	err := a.runCreate(spec)
	if err == nil {
		t.Fatal("hub with shaping flags should be refused")
	}
	for _, f := range []string{"ports", "memory", "disk", "repos", "bootstrap_hook"} {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error %q does not name %s", err, f)
		}
	}
	if config.Exists(a.ConfigDir, "h") {
		t.Error("refused hub must not be registered")
	}
	// The same spec without them is fine (the version probe answers dev).
	fakeSSH(t, `case "$last" in *devvm*) echo "devvm version dev";; esac; exit 0`)
	a.Stdout, a.Stderr = new(bytes.Buffer), new(bytes.Buffer)
	if err := a.runCreate(createSpec{Name: "h", Backend: config.BackendHub, SSHHost: "u@hub", Yes: true}); err != nil {
		t.Fatalf("plain hub create: %v", err)
	}
}

// `delete HUB` refuses while a [machines.*] table or a live run/HUB@*.sock
// exists, before any prompt, so it is scriptable and never orphans forwards.
func TestDeleteHubRefusesWhileHeld(t *testing.T) {
	a := newTestApp(t)
	fakeSSH(t, "exit 1")
	a.Stdout = new(bytes.Buffer)
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}})
	err := a.runDelete("h", false)
	if err == nil || !strings.Contains(err.Error(), "[machines.web]") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("delete with a table: err = %v", err)
	}
	if !config.Exists(a.ConfigDir, "h") {
		t.Fatal("refused delete removed the conf")
	}
	// A live daemon socket alone is enough to refuse, even with no table.
	writeHub(t, a, "h", nil)
	if err := config.EnsureRuntimeDir(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(config.RuntimeDir(a.ConfigDir), "h@api.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = a.runDelete("h", false)
	if err == nil || !strings.Contains(err.Error(), "forwards for h/api") {
		t.Fatalf("delete with a live socket: err = %v", err)
	}
	// Another hub's socket does not count.
	if err := os.Rename(sock, filepath.Join(config.RuntimeDir(a.ConfigDir), "other@api.sock")); err != nil {
		t.Fatal(err)
	}
	// With nothing held it proceeds to the prompt, which has no terminal here:
	// the error is the prompt's, not a refusal, and the conf is still there.
	err = a.runDelete("h", false)
	if err == nil || strings.Contains(err.Error(), "--force") {
		t.Fatalf("unheld delete should reach the prompt: err = %v", err)
	}
	// --force skips the refusal (and would also reach the prompt).
	writeHub(t, a, "h", map[string][]string{"web": nil})
	err = a.runDelete("h", true)
	if err == nil || strings.Contains(err.Error(), "--force") {
		t.Fatalf("--force should bypass the refusal: err = %v", err)
	}
}

// The floor is pinned so a change is deliberate: bump this and hubMinVersion
// together when roadmap step 3 (`status --plain --local`) ships in a tag.
func TestHubMinVersionPinned(t *testing.T) {
	if hubMinVersion != "v0.1.13" {
		t.Fatalf("hubMinVersion = %q; it must be the first tag that ships step 3 (see hub.go)", hubMinVersion)
	}
}
