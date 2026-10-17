package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// listMachines carries every local smol VM smolvm lists (discovery), in
// one `ls`; one an entry of the same name shadows is listed once, as the
// entry. A hub-machine daemon still serving from the pre-split run/ (a
// v0.1.15 one) is listed with no cache or ports file naming it.
func TestListMachinesSmolAndLegacyHubSocket(t *testing.T) {
	a := &App{Paths: testPaths(shortTempDir(t)), Stdout: io.Discard, Stderr: io.Discard}
	log := fakeLocalSmolvm(t, `[{"name":"web","state":"running","created_at":1},{"name":"box","state":"stopped","created_at":2}]`)
	saveRemote(t, a.Paths, "box")
	legacy := config.LegacyPaths(a.Paths.Config)
	serveOwnerDaemon(t, legacy, "h/leg", nil, true)
	want := []string{"box", "h/leg", "web"}
	if got := a.listMachines(); !slices.Equal(got, want) {
		t.Errorf("listMachines = %v, want %v", got, want)
	}
	if calls := smolvmLsCalls(t, log); calls != 1 {
		t.Errorf("listMachines ran %d smolvm listings, want 1", calls)
	}
	// Completion offers the smol VM: resolve-verbs, delete (no entry), any.
	for name, comp := range map[string]func() []string{
		"completeMachines":   func() []string { c, _ := a.completeMachines(nil, nil, ""); return c },
		"completeDeletable":  func() []string { c, _ := a.completeDeletable(nil, nil, ""); return c },
		"completeAnyMachine": func() []string { c, _ := a.completeAnyMachine(nil, nil, ""); return c },
	} {
		if got := comp(); !slices.Contains(got, "web") {
			t.Errorf("%s = %v, lacks the smol VM", name, got)
		}
	}
	if got, _ := a.completeDeletable(nil, nil, ""); slices.Contains(got, "box") {
		t.Errorf("completeDeletable offers the entry: %v", got)
	}
}

func smolvmLsCalls(t *testing.T, log string) int {
	t.Helper()
	n := 0
	for _, c := range strings.Split(readFileOrEmpty(t, log), "\n") {
		if c == "machine ls --json" {
			n++
		}
	}
	return n
}

// update's restartDaemons cycles a smol VM's daemon in the state dir's
// run/ and in the pre-split layout's, and a legacy hub-machine daemon.
func TestRestartDaemonsCyclesSmolBothLayouts(t *testing.T) {
	old := daemonGoneTimeout
	daemonGoneTimeout = 2 * time.Second
	t.Cleanup(func() { daemonGoneTimeout = old })
	var out bytes.Buffer
	a := &App{Paths: testPaths(shortTempDir(t)), Stdout: &out, Stderr: io.Discard}
	fakeLocalSmolvm(t, `[{"name":"web","state":"running","created_at":1},{"name":"api","state":"running","created_at":2}]`)
	up := map[string]session.Response{session.OpList: {OK: true, State: session.StateUp, Version: "v0.1.15"}}
	legacy := config.LegacyPaths(a.Paths.Config)
	web := serveOwnerDaemon(t, a.Paths, "web", up, true)
	api := serveOwnerDaemon(t, legacy, "api", up, true)
	hub := serveOwnerDaemon(t, legacy, "h/leg", up, true)
	res := a.restartDaemons()
	for name, d := range map[string]*ownerDaemon{"web": web, "api": api, "h/leg": hub} {
		if len(d.requests(session.OpStop)) != 1 {
			t.Errorf("%s: daemon not stopped", name)
		}
	}
	slices.Sort(res.cycled)
	if !slices.Equal(res.cycled, []string{"api", "h/leg", "web"}) || len(res.failed) != 0 {
		t.Errorf("cycled = %v failed = %v\n%s", res.cycled, res.failed, out.String())
	}
}

// The global `ports list` lists a smol VM's saved forwards.
func TestPortsListAllListsSmol(t *testing.T) {
	a := &App{Paths: testPaths(shortTempDir(t))}
	fakeLocalSmolvm(t, `[{"name":"web","state":"running","created_at":1}]`)
	if err := config.SavePorts(a.Paths, "web", []string{"3000:3000"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a.Stdout, a.Stderr = &out, io.Discard
	if err := a.runPortsListAll(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "web") || !strings.Contains(out.String(), "3000:3000") {
		t.Errorf("ports list =\n%s", out.String())
	}
}

func readFileOrEmpty(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(data)
}
