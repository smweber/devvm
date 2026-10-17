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

// fakeLocalSmolvm puts a smolvm on PATH answering `machine ls --json` with
// rows (one JSON array) and logging every call, so a test resolves local
// smol VMs exactly as the cli does: through discovery, one listing.
func fakeLocalSmolvm(t *testing.T, rows string) (log string) {
	t.Helper()
	dir := t.TempDir()
	ls, log := filepath.Join(dir, "ls.json"), filepath.Join(dir, "smolvm.log")
	if err := os.WriteFile(ls, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" +
		"[ \"$*\" = 'machine ls --json' ] && cat " + ls + "\nexit 0\n"
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
		if c != "" && c != "machine ls --json" {
			calls = append(calls, c)
		}
	}
	return calls
}

// A smol VM devvm did not create does little (config.md §2): every verb
// beyond status/start/stop/shell/exec refuses in one line naming the
// handover, before anything runs in the VM.
func TestUnmanagedSmolRefusals(t *testing.T) {
	a := newTestApp(t)
	log := fakeLocalSmolvm(t, `[{"name":"web","state":"running","created_at":100}]`)
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"attach", "web"}, {"auth", "web"}, {"bootstrap", "web"},
		{"cp-in", "web", src, "/tmp"}, {"cp-out", "web", "/etc/hostname", t.TempDir()},
		{"ports", "add", "web", "3000"}, {"ports", "up", "web"}, {"ports", "down", "web"},
		{"repos", "add", "web", "o/r"}, {"repos", "list", "web"},
		{"__daemon", "web"}, {"__session", "web"},
	} {
		err := runTree(t, a, argv...)
		if err == nil || !strings.Contains(err.Error(), "devvm setup --adopt web") ||
			!strings.Contains(err.Error(), "smolvm machine rm --name web") || strings.Contains(err.Error(), "\n") {
			t.Errorf("%v: err = %v, want one line naming the handover and the removal", argv, err)
		}
	}
	if calls := smolvmCalls(t, log); len(calls) > 0 {
		t.Errorf("refused verbs ran smolvm: %q", calls)
	}
	if _, err := os.Stat(a.Paths.PortsPath("web")); err == nil {
		t.Error("a refused `ports add` saved a forward")
	}

	// delete refuses too, pointing at smolvm: devvm destroys only what it made.
	err := runTree(t, a, "delete", "web")
	if err == nil || !strings.Contains(err.Error(), "smolvm machine rm --name web") {
		t.Errorf("delete: err = %v, want a pointer at smolvm machine rm", err)
	}

	// exec runs, as smolvm's default user over a plain exec; start and stop too.
	for _, argv := range [][]string{{"exec", "web", "--", "id"}, {"start", "web"}, {"stop", "web"}} {
		if err := runTree(t, a, argv...); err != nil {
			t.Errorf("%v: %v", argv, err)
		}
	}
	want := []string{"machine exec --name web -- id", "machine start --name web", "machine stop --name web"}
	if calls := smolvmCalls(t, log); strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("smolvm calls = %q, want %q", calls, want)
	}
}

// `ports list` and `ports rm` are host-side only: a stray ports file for a
// VM devvm did not create, or for one that is gone, can be seen and emptied
// (delete refuses the first and has nothing to delete for the second).
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

// bridged needs Managed on smol: an unmanaged VM's shell holds no bridge,
// since subscribing would spawn a daemon (and an agent install).
func TestBridgedRequiresManagedSmol(t *testing.T) {
	if bridged(&config.Machine{Name: "web", Backend: config.BackendSmol}) {
		t.Error("an unmanaged smol VM is bridged")
	}
	if !bridged(managedSmol("web")) {
		t.Error("a managed smol VM is not bridged")
	}
}

// The --plain fifth column for every local row kind (config.md §7): a VM
// devvm made, one it did not, an old smol conf (managed by the migration
// rule, its marker written), remote entries managed and adopted, a remote
// entry shadowing a VM of the same name, a broken conf, and a hub, whose
// own row is `-` (unless it too shadows a VM).
func TestStatusPlainManagedColumn(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[
		{"name":"made","state":"running","created_at":1},
		{"name":"found","state":"stopped","created_at":2},
		{"name":"legacy","state":"running","created_at":3},
		{"name":"cloud","state":"stopped","created_at":4},
		{"name":"h","state":"stopped","created_at":5}
	]`)
	if err := config.WriteManagedMarker(a.Paths, "made", "1"); err != nil {
		t.Fatal(err)
	}
	for _, m := range []*config.Machine{
		{Name: "legacy", Backend: config.BackendSmol, Memory: 1024},
		config.NewRemote("cloud", config.BackendRemoteUnmanaged, "u@cloud"),
		config.NewRemote("box", config.BackendRemoteManaged, "u@box"),
		config.NewRemote("adopted", config.BackendRemoteUnmanaged, "u@adopted"),
	} {
		if err := m.Save(a.Paths.Config); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(config.MachinesDir(a.Paths.Config), "broken.toml"), []byte("backend = ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "status", "--plain", "--local"); err != nil {
		t.Fatal(err)
	}
	got := a.Stdout.(*bytes.Buffer).String()
	for _, want := range []string{
		"made\tsmol\trunning\t-\tmanaged\n",
		"found\tsmol\tstopped\t-\tunmanaged\n",
		"legacy\tsmol\trunning\t-\tmanaged\n",
		"cloud\tremote-unmanaged\treachable\t-\tshadowed\n",
		"box\tremote-managed\treachable\t-\tmanaged\n",
		"adopted\tremote-unmanaged\treachable\t-\tunmanaged\n",
		"broken\t?\tbroken conf\t-\t-\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plain lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "cloud\t") != 1 {
		t.Errorf("the shadowed VM got a row of its own:\n%s", got)
	}
	if strings.Contains(got, "dormant") {
		t.Errorf("dormant is gone:\n%s", got)
	}
	if m, ok, _ := config.ReadManagedMarker(a.Paths, "legacy"); !ok || m != "3" {
		t.Errorf("legacy conf's marker = %q, %v; want 3 written by the migration rule", m, ok)
	}

	// The hub's own row: `-`, or shadowed when a VM shares its name.
	hub := config.NewHub("h", "u@hub")
	r := a.hubRows(hub, hubListing{fresh: true, rows: []hubRow{
		{name: "new", backend: "smol", state: "running", forwards: "-", managed: "unmanaged"},
		{name: "old", backend: "smol", state: "running", forwards: "-", managed: "-"},
	}})
	if got := []string{plainManaged(r[0]), plainManaged(r[1]), plainManaged(r[2])}; strings.Join(got, ",") != "-,unmanaged,-" {
		t.Errorf("hub rows' column = %v, want -, the hub's unmanaged passed through, - for an old hub", got)
	}
	smols := smolSnapshot()
	smols.shadow(&r[0])
	if plainManaged(r[0]) != "shadowed" {
		t.Errorf("a hub entry named like a local VM: %q, want shadowed", plainManaged(r[0]))
	}
}

// A conf whose smol VM does not exist is just not found now (no dormant
// state); delete drops the stale conf without a prompt.
func TestStaleSmolConf(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	m := &config.Machine{Name: "gone", Backend: config.BackendSmol, Memory: 1024}
	if err := m.Save(a.Paths.Config); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteManagedMarker(a.Paths, "gone", "9"); err != nil {
		t.Fatal(err)
	}
	err := runTree(t, a, "attach", "gone")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("attach: err = %v, want not found", err)
	}
	if err := runTree(t, a, "delete", "gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if config.Exists(a.Paths.Config, "gone") {
		t.Error("stale conf survived delete")
	}
	if _, ok, _ := config.ReadManagedMarker(a.Paths, "gone"); ok {
		t.Error("marker survived delete")
	}
	if err := runTree(t, a, "attach", "nope"); err == nil || !strings.Contains(err.Error(), "devvm create nope") {
		t.Errorf("unknown name: err = %v, want a hint to create", err)
	}
}

// forgetHostState drops the marker and the machine's pinned host key, but
// keeps a pin another registered machine still uses (known_hosts is per
// host, not per name).
func TestForgetHostStateDropsMarkerAndPin(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	a := newTestApp(t)
	if err := os.MkdirAll(a.Paths.State, 0o700); err != nil {
		t.Fatal(err)
	}
	const key = "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	kh := "gone.example ssh-ed25519 " + key + "\n" +
		"[kept.example]:2222 ssh-ed25519 " + key + "\n"
	if err := os.WriteFile(a.Paths.KnownHostsPath(), []byte(kh), 0o600); err != nil {
		t.Fatal(err)
	}
	sshWithConfig(t, "")
	gone := config.NewRemote("gone", config.BackendRemoteManaged, "dev@gone.example")
	kept := config.NewRemote("kept", config.BackendRemoteManaged, "dev@kept.example")
	kept.SSHPort = 2222
	other := config.NewRemote("other", config.BackendRemoteManaged, "root@kept.example")
	other.SSHPort = 2222
	if err := other.Save(a.Paths.Config); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteManagedMarker(a.Paths, "gone", "1"); err != nil {
		t.Fatal(err)
	}
	if err := a.forgetHostState("gone", gone); err != nil {
		t.Fatal(err)
	}
	if err := a.forgetHostState("kept", kept); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(a.Paths.KnownHostsPath())
	if strings.Contains(string(data), "gone.example") {
		t.Errorf("deleted machine's pin survived:\n%s", data)
	}
	if !strings.Contains(string(data), "[kept.example]:2222") {
		t.Errorf("a pin another machine uses was dropped:\n%s", data)
	}
	if _, ok, _ := config.ReadManagedMarker(a.Paths, "gone"); ok {
		t.Error("marker survived")
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
		m := config.NewRemote("x", config.BackendRemoteManaged, tc.dest)
		m.SSHPort = tc.port
		if got := knownHostsKey(m); got != tc.want {
			t.Errorf("knownHostsKey(%s, port %d) = %q, want %q", tc.dest, tc.port, got, tc.want)
		}
	}
}
