package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// Saved forwards are host state (config.md §5): `ports add` saves to
// <State>/ports/NAME (not the conf), --ephemeral forwards without saving,
// `ports rm` drops the line, and `config rm` removes the file with the entry.
func TestPortsVerbsUseStateFile(t *testing.T) {
	a := &App{Paths: testPaths(shortTempDir(t))}
	m := config.NewRemote("box", true, "dev@example")
	if err := writeEntry(a.Paths, m); err != nil {
		t.Fatal(err)
	}
	conf := a.Paths.FilePath()
	confBefore, _ := os.ReadFile(conf)
	od := serveOwnerDaemon(t, a.Paths, "box", map[string]session.Response{
		session.OpAdd:  {OK: true, Host: 3000},
		session.OpList: {OK: true, State: session.StateUp},
	}, true) // exits on stop, so delete's reap sees it go

	if err := runTree(t, a, "ports", "add", "box", "3000"); err != nil {
		t.Fatalf("ports add: %v\n%s", err, a.Stderr)
	}
	if err := runTree(t, a, "ports", "add", "box", "4000", "--ephemeral"); err != nil {
		t.Fatalf("ports add --ephemeral: %v\n%s", err, a.Stderr)
	}
	if adds := od.requests(session.OpAdd); len(adds) != 2 || adds[0].Guest != 3000 || adds[1].Guest != 4000 {
		t.Errorf("adds = %+v, want 3000 then the ephemeral 4000", adds)
	}
	if saved, _ := config.LoadPorts(a.Paths, "box"); !slices.Equal(saved, []string{"3000"}) {
		t.Errorf("saved = %v, want only 3000 (--ephemeral saves nothing)", saved)
	}
	if data, _ := os.ReadFile(a.Paths.PortsPath("box")); !strings.Contains(string(data), "\n3000\n") {
		t.Errorf("ports file =\n%s", data)
	}
	if confAfter, _ := os.ReadFile(conf); string(confAfter) != string(confBefore) {
		t.Errorf("a ports verb wrote the conf:\n%s", confAfter)
	}

	if err := runTree(t, a, "ports", "list", "box"); err != nil {
		t.Fatal(err)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.HasPrefix(out, "configured:\n  3000\n") {
		t.Errorf("ports list =\n%s", out)
	}

	if err := runTree(t, a, "ports", "rm", "box", "3000:3000"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Paths.PortsPath("box")); !os.IsNotExist(err) {
		t.Errorf("ports rm of the last mapping left the file: %v", err)
	}

	// config rm clears the entry's host state along with the entry.
	if err := config.SavePorts(a.Paths, "box", []string{"5000"}); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "config", "rm", "box"); err != nil {
		t.Fatalf("config rm: %v\n%s", err, a.Stderr)
	}
	if _, err := os.Stat(a.Paths.PortsPath("box")); !os.IsNotExist(err) {
		t.Errorf("config rm left the ports file: %v", err)
	}
	if config.Exists(a.Paths, "box") {
		t.Error("config rm left the entry")
	}
}

// An old conf's `ports` seed the file on first read, and the conf is left
// as it was: the one-time seed outlives the old loader (it reads only
// `ports`), so the hand migration need not copy forwards.
func TestPortsMigrateFromConf(t *testing.T) {
	a := &App{Paths: testPaths(shortTempDir(t))}
	if err := os.MkdirAll(config.LegacyMachinesDir(a.Paths), 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(config.LegacyMachinesDir(a.Paths), "box.toml")
	raw := "backend = \"remote-managed\"\nssh_host = \"dev@example\"\nports = [\"8080\", \"9000:90\"]\n"
	if err := os.WriteFile(conf, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "ports", "list", "box"); err != nil {
		t.Fatal(err)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.HasPrefix(out, "configured:\n  8080\n  9000:90\n") {
		t.Errorf("ports list =\n%s", out)
	}
	if saved, _ := config.LoadPorts(a.Paths, "box"); !slices.Equal(saved, []string{"8080", "9000:90"}) {
		t.Errorf("seeded = %v", saved)
	}
	if data, _ := os.ReadFile(conf); string(data) != raw {
		t.Errorf("migration wrote the conf:\n%s", data)
	}
}

// Adding a hub keeps the saved forwards already there for its machines:
// they are most likely what MigratePorts just seeded from the old hub conf
// (a hub removed with `config rm` leaves none). Nothing is dialed.
func TestConfigAddHubKeepsMachinePorts(t *testing.T) {
	a := newTestApp(t)
	a.Stdout, a.Stderr = new(bytes.Buffer), new(bytes.Buffer)
	log := fakeSSH(t, "exit 0")
	for _, n := range []string{"h/web", "h/api", "other/web"} {
		if err := config.SavePorts(a.Paths, n, []string{"3000"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := runTree(t, a, "config", "add", "h", "--backend", "hub", "--host", "hub", "--user", "u"); err != nil {
		t.Fatalf("config add hub: %v", err)
	}
	if got, _ := config.SavedPortsNames(a.Paths); !slices.Equal(got, []string{"h/api", "h/web", "other/web"}) {
		t.Errorf("saved after adding h = %v, want all kept", got)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Error("config add dialed something")
	}
}

// Concurrent edits of one machine's ports file all land (editPorts holds
// its lock), and an edit that finds the machine deleted once it holds the
// lock refuses instead of writing the file back.
func TestEditPortsConcurrentAndDeleted(t *testing.T) {
	a := &App{Paths: testPaths(shortTempDir(t)), Stdout: io.Discard, Stderr: io.Discard}
	writeHub(t, a, "h", nil)
	m, _, err := a.resolveAny("h/web")
	if err != nil {
		t.Fatal(err)
	}
	const n = 15
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mapping := fmt.Sprint(20000 + i)
			if err := a.editPorts(m, func(ports []string) []string { return append(ports, mapping) }); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if got, _ := config.LoadPorts(a.Paths, "h/web"); len(got) != n {
		t.Errorf("%d of %d concurrent adds landed: %v", len(got), n, got)
	}

	if err := config.RemoveEntry(a.Paths, "h"); err != nil {
		t.Fatal(err)
	}
	if err := config.RemovePorts(a.Paths, "h/web"); err != nil {
		t.Fatal(err)
	}
	// editPorts saw the hub registered when it began (m was resolved
	// before the delete), so it refuses once it finds the entry gone.
	unlock, err := config.LockPorts(a.Paths, "h/web")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	if err := writeEntry(a.Paths, config.NewHub("h", "u@h.example")); err != nil {
		t.Fatal(err)
	}
	locking := make(chan struct{})
	editPortsLockingHook = func() { close(locking) }
	t.Cleanup(func() { editPortsLockingHook = nil })
	go func() { done <- a.editPorts(m, func(p []string) []string { return append(p, "1") }) }()
	<-locking // it took its registered snapshot and goes for the lock we hold
	if err := config.RemoveEntry(a.Paths, "h"); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "was deleted") {
		t.Errorf("edit racing a delete = %v, want a refusal", err)
	}
	if _, err := os.Stat(a.Paths.PortsPath("h/web")); !os.IsNotExist(err) {
		t.Errorf("a refused edit wrote the file: %v", err)
	}
}
