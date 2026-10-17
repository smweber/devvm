package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func testPaths(t *testing.T) Paths {
	t.Helper()
	return Paths{Config: t.TempDir(), State: t.TempDir()}
}

func TestPortsFileRoundTrip(t *testing.T) {
	p := testPaths(t)
	if got, err := LoadPorts(p, "web"); err != nil || got != nil {
		t.Fatalf("no file: %v %v", got, err)
	}
	if err := SavePorts(p, "web", []string{"3000", "8080:80"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPorts(p, "web")
	if err != nil || !slices.Equal(got, []string{"3000", "8080:80"}) {
		t.Fatalf("LoadPorts = %v %v", got, err)
	}
	fi, err := os.Stat(p.PortsPath("web"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v %v, want 0600", fi.Mode().Perm(), err)
	}
	if di, err := os.Stat(p.PortsDir()); err != nil || di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v %v, want 0700", di.Mode().Perm(), err)
	}

	// Hand edits: comments and blank lines are skipped, entries trimmed.
	if err := os.WriteFile(p.PortsPath("web"), []byte("# mine\n\n  3000 \n# 9000\n1455:1455\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadPorts(p, "web"); !slices.Equal(got, []string{"3000", "1455:1455"}) {
		t.Errorf("hand-edited = %v", got)
	}

	// An emptied list removes the file, as RemovePorts does, idempotently.
	if err := SavePorts(p, "web", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.PortsPath("web")); !os.IsNotExist(err) {
		t.Errorf("emptied list left the file: %v", err)
	}
	if err := SavePorts(p, "web", []string{"1"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := RemovePorts(p, "web"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(p.PortsPath("web")); !os.IsNotExist(err) {
		t.Errorf("RemovePorts left the file: %v", err)
	}
}

// A hub machine's file uses the runtime form, like its run files.
func TestPortsHubName(t *testing.T) {
	p := testPaths(t)
	if got := p.PortsPath("h/web"); got != filepath.Join(p.State, "ports", "h@web") {
		t.Errorf("PortsPath = %s", got)
	}
	if err := SavePorts(p, "h/web", []string{"3000"}); err != nil {
		t.Fatal(err)
	}
	if err := SavePorts(p, "box", []string{"1"}); err != nil {
		t.Fatal(err)
	}
	if got, err := SavedPortsNames(p); err != nil || !slices.Equal(got, []string{"box", "h/web"}) {
		t.Errorf("SavedPortsNames = %v %v", got, err)
	}
	for _, bad := range []string{"../x", "h/../x", "a/b/c", ""} {
		if err := SavePorts(p, bad, nil); err == nil {
			t.Errorf("SavePorts(%q) should refuse", bad)
		}
	}
}

// The first read seeds the directory from old confs, both forms, leaving
// the confs untouched; after that the confs are never consulted, so a list
// dropped on this host stays dropped.
func TestMigratePorts(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p.Config, "box", "backend = \"smol\"\nports = [\"3000\", \"8080:80\"]\n")
	writeConf(t, p.Config, "none", "backend = \"smol\"\n")
	writeConf(t, p.Config, "h", "backend = \"hub\"\nssh_host = \"x\"\n\n[machines.web]\nports = [\"5173\"]\n\n[machines.api]\n")
	// A conf Load refuses still had a list worth keeping.
	writeConf(t, p.Config, "broken", "backend = \"nope\"\nports = [\"1\"]\n")
	before, _ := os.ReadFile(filepath.Join(LegacyMachinesDir(p), "box.toml"))

	if got, _ := LoadPorts(p, "box"); !slices.Equal(got, []string{"3000", "8080:80"}) {
		t.Errorf("box seeded = %v", got)
	}
	if got, _ := LoadPorts(p, "h/web"); !slices.Equal(got, []string{"5173"}) {
		t.Errorf("h/web seeded = %v", got)
	}
	if got, err := SavedPortsNames(p); err != nil || !slices.Equal(got, []string{"box", "broken", "h/web"}) {
		t.Errorf("seeded names = %v %v (none and the empty h/api seed nothing)", got, err)
	}
	after, _ := os.ReadFile(filepath.Join(LegacyMachinesDir(p), "box.toml"))
	if string(before) != string(after) {
		t.Errorf("conf changed by the migration:\n%s", after)
	}
	if entries := stateEntries(p); len(entries) != 1 {
		t.Errorf("temp dir left behind: %v", entries)
	}

	// Dropped here, still in the old confs: not seeded again.
	if err := RemovePorts(p, "h/web"); err != nil {
		t.Fatal(err)
	}
	if err := SavePorts(p, "box", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadPorts(p, "h/web"); got != nil {
		t.Errorf("h/web re-seeded: %v", got)
	}
	if got, _ := LoadPorts(p, "box"); got != nil {
		t.Errorf("box re-seeded: %v", got)
	}
}

// A ports dir that already exists (even empty) is never re-seeded or
// replaced.
func TestMigratePortsOnce(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(p.PortsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	writeConf(t, p.Config, "box", "backend = \"smol\"\nports = [\"3000\"]\n")
	if got, _ := LoadPorts(p, "box"); got != nil {
		t.Errorf("seeded into an existing dir: %v", got)
	}
	data, _ := os.ReadFile(filepath.Join(LegacyMachinesDir(p), "box.toml"))
	if !strings.Contains(string(data), "3000") {
		t.Error("conf touched")
	}
}

// The migration runs once, so it is all or nothing: any failure leaves no
// ports dir, every entry point returns the error, nothing (not even a save)
// creates an unseeded dir, and the next call retries.
func TestMigratePortsFailsClosed(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p.Config, "box", "backend = \"smol\"\nports = [\"3000\"]\n")
	writeConf(t, p.Config, "web", "backend = \"smol\"\nports = [\"80\"]\n")

	// A write that fails part-way.
	orig := writePortsFile
	writePortsFile = func(dir, file, display string, ports []string) error {
		if file == "web" {
			return errors.New("disk full")
		}
		return orig(dir, file, display, ports)
	}
	t.Cleanup(func() { writePortsFile = orig })
	if _, err := LoadPorts(p, "box"); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("LoadPorts with a failing write = %v", err)
	}
	if err := SavePorts(p, "new", []string{"1"}); err == nil {
		t.Fatal("SavePorts after a failed migration should fail too")
	}
	if _, err := os.Stat(p.PortsDir()); !os.IsNotExist(err) {
		t.Fatalf("a failed migration left a ports dir: %v", err)
	}
	if entries := stateEntries(p); len(entries) != 0 {
		t.Errorf("temp dir left behind: %v", entries)
	}
	writePortsFile = orig
	if got, err := LoadPorts(p, "web"); err != nil || !slices.Equal(got, []string{"80"}) {
		t.Errorf("retry = %v %v", got, err)
	}

	// A conf that is not TOML at all aborts it, naming the file.
	p = testPaths(t)
	writeConf(t, p.Config, "box", "backend = \"smol\"\nports = [\"3000\"]\n")
	writeConf(t, p.Config, "junk", "this is [not toml\n")
	if _, err := LoadPorts(p, "box"); err == nil || !strings.Contains(err.Error(), "junk.toml") {
		t.Fatalf("LoadPorts with an unparseable conf = %v", err)
	}
	if _, err := os.Stat(p.PortsDir()); !os.IsNotExist(err) {
		t.Fatal("an aborted migration left a ports dir")
	}
	writeConf(t, p.Config, "junk", "backend = \"smol\"\n")
	if got, err := LoadPorts(p, "box"); err != nil || !slices.Equal(got, []string{"3000"}) {
		t.Errorf("retry after the fix = %v %v", got, err)
	}

	// An unreadable machines dir.
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 dir")
	}
	p = testPaths(t)
	writeConf(t, p.Config, "box", "backend = \"smol\"\nports = [\"3000\"]\n")
	if err := os.Chmod(LegacyMachinesDir(p), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(LegacyMachinesDir(p), 0o755) })
	if _, err := LoadPorts(p, "box"); err == nil {
		t.Fatal("LoadPorts with an unreadable machines dir should fail")
	}
	if _, err := os.Stat(p.PortsDir()); !os.IsNotExist(err) {
		t.Fatal("an unreadable machines dir left a ports dir")
	}
	if err := os.Chmod(LegacyMachinesDir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadPorts(p, "box"); err != nil || !slices.Equal(got, []string{"3000"}) {
		t.Errorf("retry = %v %v", got, err)
	}
}

// LockPorts serializes read-modify-writes of one machine's file: concurrent
// appends all land, and the lock files never show up as machines.
func TestLockPortsSerializesEdits(t *testing.T) {
	p := testPaths(t)
	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			unlock, err := LockPorts(p, "h/web")
			if err != nil {
				t.Error(err)
				return
			}
			defer unlock()
			ports, err := LoadPorts(p, "h/web")
			if err != nil {
				t.Error(err)
				return
			}
			if err := SavePorts(p, "h/web", append(ports, fmt.Sprint(20000+i))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if got, _ := LoadPorts(p, "h/web"); len(got) != n {
		t.Errorf("%d of %d appends landed: %v", len(got), n, got)
	}
	if names, err := SavedPortsNames(p); err != nil || !slices.Equal(names, []string{"h/web"}) {
		t.Errorf("SavedPortsNames = %v %v", names, err)
	}
}

// stateEntries lists <State> but the migration's lock file, which is meant
// to stay.
func stateEntries(p Paths) []string {
	entries, _ := os.ReadDir(p.State)
	var out []string
	for _, e := range entries {
		if e.Name() != ".ports-migrate.lock" {
			out = append(out, e.Name())
		}
	}
	return out
}
