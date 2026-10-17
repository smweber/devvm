package session

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

// A daemon an older build left serving from the pre-split layout is the
// machine's one daemon: Dial, Existing, DialConn and OpenSession all reach
// it there and never spawn a sibling under the state dir.
func TestClientsUseOldLayoutDaemon(t *testing.T) {
	p := shortTempPaths(t)
	legacy := config.LegacyPaths(p.Config)
	startSockDaemon(t, legacy)

	c, err := Dial(p, "t") // a spawn would fail here: the test binary is no devvm
	if err != nil {
		t.Fatal(err)
	}
	if c.Paths() != legacy {
		t.Errorf("Dial paths = %+v, want the old layout", c.Paths())
	}
	if c, err := Existing(p, "t"); err != nil || c.Paths() != legacy {
		t.Errorf("Existing = %v, %v", c, err)
	}
	if _, err := ExistingIn(p, "t"); err != ErrNoDaemon {
		t.Errorf("ExistingIn(state) = %v, want ErrNoDaemon", err)
	}
	conn, err := DialConn(p, "t")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	s, err := OpenSession(p, "t", SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Lstat(p.RuntimeDir()); err == nil {
		t.Error("state run/ created: something spawned or logged there")
	}
}

// delete HUB reaps hub machines whose only run files are an older build's.
func TestHubRuntimeNamesScansOldLayout(t *testing.T) {
	p := shortTempPaths(t)
	legacy := config.LegacyPaths(p.Config)
	for _, f := range []string{
		filepath.Join(legacy.RuntimeDir(), "h@old.log"),
		filepath.Join(p.RuntimeDir(), "h@new.lock"),
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := HubRuntimeNames(p, "h"); !slices.Equal(got, []string{"h/new", "h/old"}) {
		t.Errorf("HubRuntimeNames = %v", got)
	}
	if err := Reap(p, "h/old", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(legacy.RuntimeDir(), "h@old.log")); err == nil {
		t.Error("Reap left the old layout's log")
	}
}
