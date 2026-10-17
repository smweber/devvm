package backend

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

// The real shape of `smolvm machine ls --json`, captured from smolvm 1.16.1
// on a Linux host with one running VM (testdata/smolvm-machine-ls.json). It
// pins what config.md §2/§3 rely on: sizes per machine, and created_at as a
// number of Unix seconds.
func TestParseSmolListFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "smolvm-machine-ls.json"))
	if err != nil {
		t.Fatal(err)
	}
	ms, err := parseSmolList(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []SmolMachine{{Name: "web", State: "running", MemoryMiB: 1024, StorageGB: 10, CPUs: 4, CreatedAt: "1789490152"}}
	if !slices.Equal(ms, want) {
		t.Errorf("parsed = %+v, want %+v", ms, want)
	}
	if !ms[0].Running() {
		t.Error("running VM reads not running")
	}
}

// The same VM stopped (smolvm 1.16.1, testdata/smolvm-machine-ls-stopped.json):
// created_at is unchanged across a stop (so a marker survives it), sizes are
// reported while stopped, pid is null. It settles managed against a matching
// marker, sizes and all.
func TestStoppedRowFixtureSettles(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "smolvm-machine-ls-stopped.json"))
	if err != nil {
		t.Fatal(err)
	}
	ls := string(data)
	fakeSmolList(t, &ls)
	p := testPaths(t)
	if err := config.WriteManagedMarker(p, "web", "1789490152"); err != nil {
		t.Fatal(err)
	}
	d, ok, err := Discover(p, "web")
	if err != nil || !ok {
		t.Fatalf("Discover = %v, %v", ok, err)
	}
	if !d.Machine.Managed() {
		t.Error("stopped VM with a matching marker reads unmanaged")
	}
	row, _ := SmolRow(d.Backend)
	if row.Running() || row.State != "stopped" || row.MemoryMiB != 1024 || row.StorageGB != 10 || row.CPUs != 4 {
		t.Errorf("stopped row = %+v, want stopped with sizes", row)
	}
}

// created_at in any other form must not fail the listing: a string is
// taken as is, null and absent are "" (which settles unmanaged).
func TestSmolCreatedAtForms(t *testing.T) {
	ms, err := parseSmolList([]byte(`[
		{"name":"a","state":"stopped","created_at":"2026-10-17T10:00:00Z"},
		{"name":"b","state":"stopped","created_at":null},
		{"name":"c","state":"stopped"},
		{"name":"d","state":"stopped","created_at":1.7894901525e9},
		{"name":"e","state":"stopped","created_at":1.789490152e9},
		{"name":"f","state":"stopped","created_at":" 1789490152 "}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	var got []SmolCreatedAt
	for _, m := range ms {
		got = append(got, m.CreatedAt)
	}
	// Integral values normalize to one decimal integer, however spelled.
	want := []SmolCreatedAt{"2026-10-17T10:00:00Z", "", "", "1.7894901525e9", "1789490152", "1789490152"}
	if !slices.Equal(got, want) {
		t.Errorf("created_at = %q, want %q", got, want)
	}
	if _, err := parseSmolList([]byte("not json")); err == nil {
		t.Error("garbage listing parsed")
	}
}

// fakeSmolList makes smolvm "installed" (a stub on PATH, never run for the
// listing) and answers `machine ls --json` with the given rows.
func fakeSmolList(t *testing.T, json *string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "smolvm"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	orig := smolLs
	smolLs = func() ([]byte, error) { return []byte(*json), nil }
	t.Cleanup(func() { smolLs = orig })
}

func testPaths(t *testing.T) config.Paths {
	d := t.TempDir()
	return config.Paths{Config: filepath.Join(d, "config"), State: filepath.Join(d, "state")}
}

// The smol source settles Managed from the marker against created_at
// (config.md §2): matching is managed; a stale marker, no marker, or no
// created_at is unmanaged.
func TestDiscoverSmolSettlesManaged(t *testing.T) {
	ls := `[{"name":"web","state":"running","memory_mib":2048,"storage_gb":20,"cpus":4,"created_at":100},
		{"name":"old","state":"stopped","created_at":null}]`
	fakeSmolList(t, &ls)
	p := testPaths(t)

	discover := func(name string) Discovered {
		t.Helper()
		d, ok, err := Discover(p, name)
		if err != nil || !ok {
			t.Fatalf("Discover(%s) = %v, %v", name, ok, err)
		}
		return d
	}
	if d := discover("web"); d.Machine.Managed() {
		t.Error("no marker: discovered VM reads managed")
	}
	if err := config.WriteManagedMarker(p, "web", "100"); err != nil {
		t.Fatal(err)
	}
	d := discover("web")
	if !d.Machine.Managed() || d.Machine.Backend != config.BackendSmol {
		t.Errorf("matching marker: record = %+v, want managed smol", d.Machine)
	}
	if row, ok := SmolRow(d.Backend); !ok || row.MemoryMiB != 2048 || row.StorageGB != 20 || row.CPUs != 4 {
		t.Errorf("backend row = %+v, %v; want the listing's sizes", row, ok)
	}
	// Recreated under the same name: a new created_at, the marker is stale.
	ls = `[{"name":"web","state":"running","created_at":200}]`
	if discover("web").Machine.Managed() {
		t.Error("stale marker trusted")
	}
	if got, _, _ := config.ReadManagedMarker(p, "web"); got != "100" {
		t.Errorf("stale marker rewritten to %q", got)
	}
	// No created_at at all: unmanaged whatever the marker says.
	ls = `[{"name":"old","state":"stopped"}]`
	if err := config.WriteManagedMarker(p, "old", "1"); err != nil {
		t.Fatal(err)
	}
	if discover("old").Machine.Managed() {
		t.Error("a VM with no created_at reads managed")
	}
	// Nobody knows the name: not found, no error.
	if _, ok, err := Discover(p, "nope"); ok || err != nil {
		t.Errorf("Discover(nope) = %v, %v; want not found", ok, err)
	}
}

// With no smolvm installed the smol source is simply not consulted: a
// laptop that only reaches remote boxes never needs smolvm.
func TestDiscoverWithoutSmolvm(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, ok, err := Discover(testPaths(t), "web"); ok || err != nil {
		t.Errorf("Discover = %v, %v; want not found", ok, err)
	}
}

// The step 3 migration aid is gone with the old loader: a VM with no
// marker is unmanaged however it was made, and discovery writes nothing.
// The hand migration writes markers (config.md §8).
func TestNoMarkerIsUnmanaged(t *testing.T) {
	ls := `[{"name":"web","state":"stopped","created_at":100}]`
	fakeSmolList(t, &ls)
	p := testPaths(t)
	d, ok, err := Discover(p, "web")
	if err != nil || !ok {
		t.Fatalf("Discover = %v, %v", ok, err)
	}
	if d.Machine.Managed() {
		t.Error("a VM with no marker reads managed")
	}
	if _, ok, _ := config.ReadManagedMarker(p, "web"); ok {
		t.Error("discovery wrote a marker")
	}
}

// The daemon's re-read (config.md §2): Refresh re-settles on a copy, so a
// VM recreated while the daemon slept comes back unmanaged, and the record
// the daemon started with is left alone.
func TestSmolRefreshResettles(t *testing.T) {
	ls := `[{"name":"web","state":"running","created_at":100}]`
	fakeSmolList(t, &ls)
	p := testPaths(t)
	if err := config.WriteManagedMarker(p, "web", "100"); err != nil {
		t.Fatal(err)
	}
	d, _, _ := Discover(p, "web")
	r, ok := d.Backend.(Refresher)
	if !ok {
		t.Fatal("smol backend is not a Refresher")
	}
	fresh, running, err := r.Refresh()
	if err != nil || !running || !fresh.Managed() {
		t.Fatalf("Refresh = %+v, %v, %v; want managed and running", fresh, running, err)
	}
	ls = `[{"name":"web","state":"stopped","created_at":200}]`
	fresh, running, err = r.Refresh()
	if err != nil || running || fresh.Managed() {
		t.Errorf("recreated VM: Refresh = managed %v, running %v, %v; want unmanaged, stopped", fresh.Managed(), running, err)
	}
	if !d.Machine.Managed() {
		t.Error("Refresh mutated the original record")
	}
	// Deleted since: not running (the daemon waits as for a stopped VM).
	ls = `[]`
	if fresh, running, err := r.Refresh(); err != nil || running || fresh != nil {
		t.Errorf("deleted VM: Refresh = %v, %v, %v; want nil, not running, no error", fresh, running, err)
	}
}

// An unmanaged VM is reached as smolvm's own default user over a plain
// `machine exec`: no hostname re-assert, no sudo to `dev`, no login wrap,
// Env through smolvm's -e (config.md §2).
func TestUnmanagedSmolExecIsPlain(t *testing.T) {
	b := &smolBackend{m: &config.Machine{Name: "web", Backend: config.BackendSmol}}
	got := strings.Join(b.execArgv(ExecOpts{Login: true, User: "root", Env: map[string]string{"A": "1"}}, false, []string{"ls", "-l"}), " ")
	if want := "smolvm machine exec -e A=1 --name web -- ls -l"; got != want {
		t.Errorf("unmanaged exec = %q, want %q", got, want)
	}
	managed := &smolBackend{m: managedSmol("web")}
	if got := strings.Join(managed.execArgv(ExecOpts{}, false, []string{"ls"}), " "); !strings.Contains(got, "sudo -u dev") {
		t.Errorf("managed exec = %q, want the dev-user drop", got)
	}
	if err := b.Attach(""); err == nil {
		// needSmolvm may fail first on a host with no smolvm; either way no attach.
		t.Error("unmanaged Attach succeeded")
	}
}

// The upgrade seed: every old smol conf with no marker gets one from the
// VM's current created_at (so it stays managed); an existing marker is
// never rewritten; a name smolvm does not list, a remote conf, or a conf
// that does not parse is skipped; a second run lists nothing.
func TestSeedLegacyMarkers(t *testing.T) {
	ls := `[{"name":"web","state":"running","created_at":100},{"name":"old","state":"stopped","created_at":300},{"name":"r","state":"stopped","created_at":5}]`
	calls := 0
	fakeSmolList(t, &ls)
	orig := smolLs
	smolLs = func() ([]byte, error) { calls++; return orig() }
	p := testPaths(t)
	md := config.LegacyMachinesDir(p)
	if err := os.MkdirAll(md, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, body := range map[string]string{
		"web":  "backend = \"smol\"\nmemory = 1024\n",
		"old":  "backend = \"smol\"\n",
		"gone": "backend = \"smol\"\n",
		"r":    "backend = \"remote-managed\"\nssh_host = \"r\"\n",
		"bad":  "backend = [",
	} {
		if err := os.WriteFile(filepath.Join(md, n+".toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.WriteManagedMarker(p, "old", "200"); err != nil { // stale: kept as is
		t.Fatal(err)
	}
	SeedLegacyMarkers(p)
	if got, ok, _ := config.ReadManagedMarker(p, "web"); !ok || got != "100" {
		t.Errorf("web marker = %q, %v", got, ok)
	}
	if got, _, _ := config.ReadManagedMarker(p, "old"); got != "200" {
		t.Errorf("an existing marker was rewritten: %q", got)
	}
	for _, n := range []string{"gone", "r", "bad"} {
		if _, ok, _ := config.ReadManagedMarker(p, n); ok {
			t.Errorf("%s got a marker", n)
		}
	}
	if d, _, _ := Discover(p, "web"); d.Machine == nil || !d.Machine.Managed() {
		t.Error("the seeded VM is not managed")
	}
	calls = 0
	if err := os.Remove(filepath.Join(md, "gone.toml")); err != nil {
		t.Fatal(err)
	}
	SeedLegacyMarkers(p)
	if calls != 0 {
		t.Errorf("a second run listed smolvm %d times with nothing to mark", calls)
	}
}
