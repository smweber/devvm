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
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// Saved forwards are host state (config.md §5): `ports add` saves to
// <State>/ports/NAME (not the conf), --ephemeral forwards without saving,
// `ports rm` drops the line, and `delete` removes the file with the machine.
func TestPortsVerbsUseStateFile(t *testing.T) {
	a := &App{Paths: testPaths(shortTempDir(t))}
	m := config.NewRemote("box", config.BackendRemoteManaged, "dev@example")
	if err := m.Save(a.Paths.Config); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(config.MachinesDir(a.Paths.Config), "box.toml")
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

	// delete clears the machine's host state along with its conf.
	if err := config.SavePorts(a.Paths, "box", []string{"5000"}); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "delete", "box"); err != nil {
		t.Fatalf("delete: %v\n%s", err, a.Stderr)
	}
	if _, err := os.Stat(a.Paths.PortsPath("box")); !os.IsNotExist(err) {
		t.Errorf("delete left the ports file: %v", err)
	}
	if config.Exists(a.Paths.Config, "box") {
		t.Error("delete left the conf")
	}
}

// An old conf's `ports` seed the file on first read, and the conf is left
// as it was: VM commands never write config.
func TestPortsMigrateFromConf(t *testing.T) {
	a := &App{Paths: testPaths(shortTempDir(t))}
	if err := os.MkdirAll(config.MachinesDir(a.Paths.Config), 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(config.MachinesDir(a.Paths.Config), "box.toml")
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

// create starts a name clean: a stale ports file from a same-named
// predecessor (its conf removed by hand, never `delete`) is replaced by the
// new machine's own list, and is gone when it has none.
func TestCreateClearsStalePorts(t *testing.T) {
	a := newTestApp(t)
	a.Stdout, a.Stderr = new(bytes.Buffer), new(bytes.Buffer)
	fakeSSH(t, "exit 0")
	for _, tc := range []struct {
		name  string
		ports []string
	}{{"box", nil}, {"web", []string{"8080"}}} {
		if err := config.SavePorts(a.Paths, tc.name, []string{"3000", "4000"}); err != nil {
			t.Fatal(err)
		}
		spec := createSpec{Name: tc.name, Backend: config.BackendRemoteUnmanaged, SSHHost: "dev@" + tc.name, Ports: tc.ports, Yes: true}
		_ = a.runCreate(spec) // bootstrap against the fake ssh may fail; the ports file is settled before it
		if !config.Exists(a.Paths.Config, tc.name) {
			t.Fatalf("%s: not registered\n%s", tc.name, a.Stderr)
		}
		if got, _ := config.LoadPorts(a.Paths, tc.name); !slices.Equal(got, tc.ports) {
			t.Errorf("%s: saved after create = %v, want %v", tc.name, got, tc.ports)
		}
	}
}

// Re-creating a hub clears the forwards its predecessor left for machines
// on it; another hub's are untouched.
func TestCreateHubClearsStaleMachinePorts(t *testing.T) {
	a := newTestApp(t)
	a.Stdout, a.Stderr = new(bytes.Buffer), new(bytes.Buffer)
	fakeSSH(t, `case "$last" in *devvm*) echo "devvm version dev";; esac; exit 0`)
	for _, n := range []string{"h/web", "h/api", "other/web"} {
		if err := config.SavePorts(a.Paths, n, []string{"3000"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.runCreate(createSpec{Name: "h", Backend: config.BackendHub, SSHHost: "u@hub", Yes: true}); err != nil {
		t.Fatalf("create hub: %v", err)
	}
	if got, _ := config.SavedPortsNames(a.Paths); !slices.Equal(got, []string{"other/web"}) {
		t.Errorf("saved after re-creating h = %v, want only other/web", got)
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

	if err := config.Remove(a.Paths.Config, "h"); err != nil {
		t.Fatal(err)
	}
	if err := config.RemovePorts(a.Paths, "h/web"); err != nil {
		t.Fatal(err)
	}
	// editPorts saw the hub registered when it began (m was resolved
	// before the delete), so it refuses once it finds the conf gone.
	unlock, err := config.LockPorts(a.Paths, "h/web")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	if err := config.NewHub("h", "u@h.example").Save(a.Paths.Config); err != nil {
		t.Fatal(err)
	}
	go func() { done <- a.editPorts(m, func(p []string) []string { return append(p, "1") }) }()
	time.Sleep(200 * time.Millisecond) // let it take its registered snapshot and block on the lock
	if err := config.Remove(a.Paths.Config, "h"); err != nil {
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
