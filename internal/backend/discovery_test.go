package backend

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

// The real shape of `smolvm machine ls --json`, captured from smolvm 1.16.1
// on a Linux host with one running VM (testdata/smolvm-machine-ls.json). It
// pins what config.md §2/§3 rely on: sizes per machine, and labels (`{}`
// on a VM nobody labelled).
func TestParseSmolListFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "smolvm-machine-ls.json"))
	if err != nil {
		t.Fatal(err)
	}
	ms, err := parseSmolList(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []SmolMachine{{Name: "web", State: "running", MemoryMiB: 1024, StorageGB: 10, CPUs: 4, Labels: map[string]string{}}}
	if !reflect.DeepEqual(ms, want) {
		t.Errorf("parsed = %+v, want %+v", ms, want)
	}
	if !ms[0].Running() || ms[0].Labelled() {
		t.Error("running unlabelled VM reads otherwise")
	}
}

// The same row with devvm's label, as smolvm 1.35.5 prints labels
// (testdata/smolvm-machine-ls-labelled.json: the 1.16.1 row above with
// `"labels": {"devvm": "true"}`, the shape `machine create --label
// devvm=true` reads back as on the ThinkPad, 2026-11-20).
func TestParseSmolListLabelled(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "smolvm-machine-ls-labelled.json"))
	if err != nil {
		t.Fatal(err)
	}
	ms, err := parseSmolList(data)
	if err != nil || len(ms) != 1 || !ms[0].Labelled() || ms[0].Labels["devvm"] != "true" {
		t.Fatalf("parsed = %+v, %v; want web labelled devvm=true", ms, err)
	}
	// Another value, another key, null labels, no labels at all: unlabelled.
	ms, err = parseSmolList([]byte(`[
		{"name":"a","state":"stopped","labels":{"devvm":"false"}},
		{"name":"b","state":"stopped","labels":{"other":"true"}},
		{"name":"c","state":"stopped","labels":null},
		{"name":"d","state":"stopped"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.Labelled() {
			t.Errorf("%s reads labelled: %+v", m.Name, m)
		}
	}
	if _, err := parseSmolList([]byte("not json")); err == nil {
		t.Error("garbage listing parsed")
	}
}

// The same VM stopped (smolvm 1.16.1, testdata/smolvm-machine-ls-stopped.json):
// sizes are reported while stopped, pid is null; with devvm's label it
// settles managed, sizes and all.
func TestStoppedRowFixtureSettles(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "smolvm-machine-ls-stopped.json"))
	if err != nil {
		t.Fatal(err)
	}
	ls := strings.Replace(string(data), `"labels": {},`, `"labels": {"devvm": "true"},`, 1)
	fakeSmolList(t, &ls)
	p := testPaths(t)
	d, ok, err := Discover(p, "web")
	if err != nil || !ok {
		t.Fatalf("Discover = %v, %v", ok, err)
	}
	if !d.Machine.Managed() {
		t.Error("stopped labelled VM reads unmanaged")
	}
	row, _ := SmolRow(d.Backend)
	if row.Running() || row.State != "stopped" || row.MemoryMiB != 1024 || row.StorageGB != 10 || row.CPUs != 4 {
		t.Errorf("stopped row = %+v, want stopped with sizes", row)
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

// The smol source knows only the VMs with devvm's label (config.md §2,
// simplify.md step 15): one with it is devvm's, one without is not found,
// and discovery writes nothing.
func TestDiscoverSmolSettlesManaged(t *testing.T) {
	ls := `[{"name":"web","state":"running","memory_mib":2048,"storage_gb":20,"cpus":4,"labels":{"devvm":"true"}},
		{"name":"raw","state":"stopped","labels":{}}]`
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
	d := discover("web")
	if !d.Machine.Managed() || d.Machine.Backend != config.BackendSmol {
		t.Errorf("labelled: record = %+v, want managed smol", d.Machine)
	}
	if row, ok := SmolRow(d.Backend); !ok || row.MemoryMiB != 2048 || row.StorageGB != 20 || row.CPUs != 4 {
		t.Errorf("backend row = %+v, %v; want the listing's sizes", row, ok)
	}
	if _, ok, err := Discover(p, "raw"); ok || err != nil {
		t.Errorf("Discover(raw) = %v, %v; want an unlabelled VM not found", ok, err)
	}
	// Nobody knows the name: not found, no error.
	if _, ok, err := Discover(p, "nope"); ok || err != nil {
		t.Errorf("Discover(nope) = %v, %v; want not found", ok, err)
	}
	if _, err := os.Stat(p.State); err == nil {
		t.Error("discovery wrote host state")
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

// The daemon's re-read (config.md §2): Refresh asks again on a copy, so a
// VM recreated outside devvm (no label) or deleted while the daemon slept
// reads as not running, and the record the daemon started with is left
// alone.
func TestSmolRefreshResettles(t *testing.T) {
	ls := `[{"name":"web","state":"running","labels":{"devvm":"true"}}]`
	fakeSmolList(t, &ls)
	p := testPaths(t)
	d, _, _ := Discover(p, "web")
	r, ok := d.Backend.(Refresher)
	if !ok {
		t.Fatal("smol backend is not a Refresher")
	}
	fresh, running, err := r.Refresh()
	if err != nil || !running || !fresh.Managed() {
		t.Fatalf("Refresh = %+v, %v, %v; want managed and running", fresh, running, err)
	}
	ls = `[{"name":"web","state":"stopped","labels":{"devvm":"true"}}]`
	if fresh, running, err := r.Refresh(); err != nil || running || fresh == nil || !fresh.Managed() {
		t.Errorf("stopped VM: Refresh = %v, %v, %v; want managed, not running", fresh, running, err)
	}
	// Recreated outside devvm: running, but no label, so never dialled.
	ls = `[{"name":"web","state":"running","labels":{}}]`
	if fresh, running, err := r.Refresh(); err != nil || running || fresh != nil {
		t.Errorf("recreated VM: Refresh = %v, %v, %v; want nil, not running", fresh, running, err)
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
