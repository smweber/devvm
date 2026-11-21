package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
