package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

// devvmLabel is a `machine ls --json` row's labels as smolvm 1.35.5 prints
// devvm's: the VM is one devvm created.
const devvmLabel = `"labels":{"devvm":"true"}`

// fakeLocalSmolvm puts a smolvm on PATH answering `machine ls --json` with
// rows (one JSON array) and logging every call, so a test resolves local
// smol VMs exactly as the cli does: through discovery, one listing. A
// version makes `--version` print "smolvm VERSION" (else nothing: no block).
func fakeLocalSmolvm(t *testing.T, rows string, version ...string) (log string) {
	t.Helper()
	dir := t.TempDir()
	ls, log := filepath.Join(dir, "ls.json"), filepath.Join(dir, "smolvm.log")
	if err := os.WriteFile(ls, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" +
		"[ \"$*\" = 'machine ls --json' ] && cat " + ls + "\n"
	if len(version) > 0 {
		script += "[ \"$*\" = --version ] && echo 'smolvm " + version[0] + "'\n"
	}
	script += "exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "smolvm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func smolvmCalls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var calls []string
	for _, c := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if c != "" && c != "machine ls --json" && c != "--version" { // queries, not actions
			calls = append(calls, c)
		}
	}
	return calls
}

// A smol VM devvm did not create (no devvm=true label) is invisible to it
// (simplify.md step 15): every verb that names it says so in one line,
// before anything runs in the VM, and status lists nothing.
func TestUnlabelledSmolInvisible(t *testing.T) {
	a := newTestApp(t)
	log := fakeLocalSmolvm(t, `[{"name":"web","state":"running","labels":{}}]`)
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"attach", "web"}, {"shell", "web"}, {"exec", "web", "--", "id"}, {"setup", "web"},
		{"start", "web"}, {"stop", "web"}, {"delete", "web", "--yes"}, {"status", "web"},
		{"cp-in", "web", src, "/tmp"}, {"cp-out", "web", "/etc/hostname", t.TempDir()},
		{"ports", "add", "web", "3000"}, {"ports", "up", "web"}, {"ports", "down", "web"},
		{"__daemon", "web"}, {"__session", "web"},
	} {
		err := runTree(t, a, argv...)
		if err == nil || err.Error() != "a smol VM named web exists, but devvm did not create it; if an older devvm made it, 'smolvm machine update --name web --label devvm=true' makes it devvm's" {
			t.Errorf("%v: err = %v, want the one line", argv, err)
		}
	}
	if calls := smolvmCalls(t, log); len(calls) > 0 {
		t.Errorf("refused verbs ran smolvm: %q", calls)
	}
	if _, err := os.Stat(a.Paths.PortsPath("web")); err == nil {
		t.Error("a refused `ports add` saved a forward")
	}
	// create refuses the name: smolvm cannot make a second VM of it.
	if err := runTree(t, a, "create", "web", "--memory", "1024", "--yes"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("create: err = %v, want the name refused", err)
	}
	if err := runTree(t, a, "status", "--plain", "--local"); err != nil {
		t.Fatal(err)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); strings.Contains(out, "web") {
		t.Errorf("status lists the unlabelled VM:\n%s", out)
	}
}

// `ports list` and `ports rm` are host-side only: a stray ports file for a
// VM devvm did not create, or for one that is gone, can be seen and emptied
// (delete finds neither).
func TestStrayPortsFileIsRemovable(t *testing.T) {
	a := newTestApp(t)
	log := fakeLocalSmolvm(t, `[{"name":"web","state":"running","created_at":100}]`)
	for _, name := range []string{"web", "gone"} {
		if err := config.SavePorts(a.Paths, name, []string{"3000"}); err != nil {
			t.Fatal(err)
		}
		if err := runTree(t, a, "ports", "list", name); err != nil || !strings.Contains(a.Stdout.(*bytes.Buffer).String(), "3000") {
			t.Errorf("ports list %s: %v, out %q", name, err, a.Stdout.(*bytes.Buffer).String())
		}
		if err := runTree(t, a, "ports", "rm", name, "3000"); err != nil {
			t.Errorf("ports rm %s: %v", name, err)
		}
		if saved, _ := config.LoadPorts(a.Paths, name); len(saved) != 0 {
			t.Errorf("%s: saved forwards %v survived ports rm", name, saved)
		}
	}
	if calls := smolvmCalls(t, log); len(calls) > 0 {
		t.Errorf("host-side ports verbs ran smolvm: %q", calls)
	}
	if err := runTree(t, a, "ports", "list", "../x"); err == nil {
		t.Error("ports list took an invalid name")
	}
}

// The --plain fifth column for every local row kind (config.md §7): a VM
// devvm made (its label; one it did not has no row), a remote entry (`-`), a remote
// entry shadowing a VM of the same name, a broken conf, and a hub, whose
// own row is `-` (unless it too shadows a VM).
func TestStatusPlainManagedColumn(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[
		{"name":"made","state":"running",`+devvmLabel+`},
		{"name":"found","state":"stopped","created_at":2},
		{"name":"cloud","state":"stopped",`+devvmLabel+`},
		{"name":"h","state":"stopped",`+devvmLabel+`}
	]`)
	for _, m := range []*config.Machine{
		config.NewRemote("cloud", "u@cloud"),
		config.NewRemote("box", "u@box"),
	} {
		if err := writeEntry(a.Paths, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := runTree(t, a, "status", "--plain", "--local"); err != nil {
		t.Fatal(err)
	}
	got := a.Stdout.(*bytes.Buffer).String()
	for _, want := range []string{
		"made\tsmol\trunning\t-\tmanaged\t-\n",
		"cloud\tremote\t?\t-\tshadowed\tverified=-\n",
		"box\tremote\t?\t-\t-\tverified=-\n", // an entry: devvm never shapes one
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plain lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "cloud\t") != 1 {
		t.Errorf("the shadowed VM got a row of its own:\n%s", got)
	}
	if strings.Contains(got, "found") || strings.Contains(got, "unmanaged") {
		t.Errorf("a VM devvm did not create has a row:\n%s", got)
	}

	// The hub's own row: `-`, or shadowed when a VM shares its name.
	hub := config.NewHub("h", "u@hub")
	r := a.hubRows(hub, hubListing{fresh: true, rows: []hubRow{
		{name: "new", backend: "smol", state: "running", forwards: "-", managed: "managed"},
		{name: "old", backend: "smol", state: "running", forwards: "-", managed: "-"},
	}}, nil)
	if got := []string{plainManaged(r[0]), plainManaged(r[1]), plainManaged(r[2])}; strings.Join(got, ",") != "-,managed,-" {
		t.Errorf("hub rows' column = %v, want -, then what the hub sent, passed through", got)
	}
	smols := smolSnapshot()
	smols.shadow(&r[0])
	if plainManaged(r[0]) != "shadowed" {
		t.Errorf("a hub entry named like a local VM: %q, want shadowed", plainManaged(r[0]))
	}
}

// delete never removes a config entry: it refuses and points at `config
// rm`, which owns the entry and this host's state for it.
func TestDeleteRefusesEntries(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	log := fakeSSH(t, "exit 0")
	writeDevvmTOML(t, a, "[machines.box]\nbackend = \"remote\"\nhost = \"b\"\n\n[machines.h]\nbackend = \"hub\"\nhost = \"h\"\n")
	before, _ := os.ReadFile(a.Paths.FilePath())
	for _, n := range []string{"box", "h"} {
		err := runTree(t, a, "delete", n)
		if err == nil || !strings.Contains(err.Error(), "'devvm config rm "+n+"'") {
			t.Errorf("delete %s = %v, want a pointer at config rm", n, err)
		}
	}
	if after, _ := os.ReadFile(a.Paths.FilePath()); string(after) != string(before) {
		t.Error("delete changed devvm.toml")
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Error("delete of an entry dialed it")
	}
	if err := runTree(t, a, "attach", "nope"); err == nil || !strings.Contains(err.Error(), "devvm create nope") || !strings.Contains(err.Error(), "config add nope") {
		t.Errorf("unknown name: err = %v, want hints to create and config add", err)
	}
}

// An entry wins over a discovered smol VM of the same name (config.md §2):
// the entry's record and backend, no smolvm call needed to say so.
func TestEntryShadowsSmolVM(t *testing.T) {
	a := newTestApp(t)
	smolLog := fakeLocalSmolvm(t, `[{"name":"web","state":"running",`+devvmLabel+`}]`)
	m, _, err := a.resolveAny("web")
	if err != nil || m.IsRemote() || !m.Managed() {
		t.Fatalf("no entry: %+v, %v; want the managed smol VM", m, err)
	}
	writeDevvmTOML(t, a, "[machines.web]\nbackend = \"remote\"\nhost = \"web.example\"\n")
	os.Remove(smolLog)
	m, _, err = a.resolveAny("web")
	if err != nil || !m.IsRemote() || m.Managed() {
		t.Fatalf("with an entry: %+v, %v", m, err)
	}
	if _, err := os.Stat(smolLog); !os.IsNotExist(err) {
		t.Error("resolving an entry ran smolvm")
	}
	// A broken file fails every name: it cannot say whether one is shadowed.
	writeDevvmTOML(t, a, "[machines.web\n")
	if _, _, err := a.resolveAny("other"); err == nil || !strings.Contains(err.Error(), "devvm.toml") {
		t.Errorf("broken file: %v", err)
	}
}

// forgetHostState drops a provider server's pinned host key (the only kind
// devvm pins), and nothing else in known_hosts. An entry has no pin to
// drop: forgetHostKey leaves the file alone for one.
func TestForgetHostStateDropsPin(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	a := newTestApp(t)
	if err := os.MkdirAll(a.Paths.State, 0o700); err != nil {
		t.Fatal(err)
	}
	const key = "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	kh := "gone.example ssh-ed25519 " + key + "\n" +
		"kept.example ssh-ed25519 " + key + "\n"
	if err := os.WriteFile(a.Paths.KnownHostsPath(), []byte(kh), 0o600); err != nil {
		t.Fatal(err)
	}
	sshWithConfig(t, "")
	gone := config.NewProviderMachine("gone", config.BackendHetzner, config.ProviderServer{Address: "gone.example"})
	entry := config.NewRemote("kept", "dev@kept.example")
	if err := a.forgetHostState("gone", gone); err != nil {
		t.Fatal(err)
	}
	if err := a.forgetHostState("kept", entry); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(a.Paths.KnownHostsPath())
	if strings.Contains(string(data), "gone.example") {
		t.Errorf("deleted machine's pin survived:\n%s", data)
	}
	if !strings.Contains(string(data), "kept.example") {
		t.Errorf("a pin no deleted server had was dropped:\n%s", data)
	}
	if _, err := os.Stat(a.Paths.KnownHostsPath() + ".old"); err == nil {
		t.Error("ssh-keygen's backup left in the state dir")
	}
}

// sshWithConfig puts an ssh on PATH that runs the real one with -F cfg, so
// `ssh -G` evaluates a test's ssh config instead of the user's (ssh reads
// ~/.ssh/config from the passwd entry, not $HOME).
func sshWithConfig(t *testing.T, cfg string) {
	t.Helper()
	real, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh")
	}
	dir := t.TempDir()
	conf := filepath.Join(dir, "config")
	if err := os.WriteFile(conf, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec " + real + " -F " + conf + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The pin key is what ssh records (config.md §9 item 6): an alias's own
// Port in ssh config wins over the conf's default 22 (real connections
// pass -o Port only when it is not 22), HostName is resolved, an explicit
// conf port wins over the alias's, and HostKeyAlias replaces it all.
func TestKnownHostsKeyFollowsSSHConfig(t *testing.T) {
	sshWithConfig(t, "Host box\n  HostName real.example\n  Port 2222\n"+
		"Host pinned\n  HostName other.example\n  HostKeyAlias pin-alias\n")
	for _, tc := range []struct {
		dest string
		port int
		want string
	}{
		{"dev@box", 22, "[real.example]:2222"},
		{"dev@box", 0, "[real.example]:2222"},
		{"dev@box", 2200, "[real.example]:2200"},
		{"dev@plain.example", 22, "plain.example"},
		{"dev@plain.example", 2022, "[plain.example]:2022"},
		{"dev@pinned", 22, "pin-alias"},
	} {
		m := config.NewRemote("x", tc.dest)
		m.SSHPort = tc.port
		if got := knownHostsKey(m); got != tc.want {
			t.Errorf("knownHostsKey(%s, port %d) = %q, want %q", tc.dest, tc.port, got, tc.want)
		}
	}
}
