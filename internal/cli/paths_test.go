package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// How --config-dir and --state-dir resolve: a non-default --config-dir alone
// is an isolated instance with its state inside it (the pre-split layout);
// naming the default config dir (what a pre-split `update` re-execs its
// successor with) keeps the default state dir; --state-dir always wins.
func TestRootPathFlags(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	def := config.DefaultPaths()
	other := t.TempDir()
	for _, tt := range []struct {
		args []string
		want config.Paths
	}{
		{nil, def},
		{[]string{"--config-dir", def.Config}, def},
		{[]string{"--config-dir", other}, config.LegacyPaths(other)},
		{[]string{"--config-dir", other, "--state-dir", other + "/st"}, config.Paths{Config: other, State: other + "/st"}},
		{[]string{"--state-dir", other}, config.Paths{Config: def.Config, State: other}},
	} {
		a := newApp()
		root := a.rootCmd()
		root.SetArgs(append(tt.args, "defaults", "path"))
		a.Stdout = io.Discard
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", tt.args, err)
		}
		if a.Paths != tt.want {
			t.Errorf("%v: paths = %+v, want %+v", tt.args, a.Paths, tt.want)
		}
	}
}

// A symlinked default config dir (dotfiles) is still the default: state
// goes to the state dir, never into the link's target.
func TestStateDirForSymlinkedDefault(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	real := filepath.Join(t.TempDir(), "dotfiles-devvm")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(cfgHome, "devvm")); err != nil {
		t.Fatal(err)
	}
	if got := stateDirFor(real); got != config.DefaultStateDir() {
		t.Errorf("stateDirFor(resolved default) = %q, want %q", got, config.DefaultStateDir())
	}
	if other := t.TempDir(); stateDirFor(other) != other {
		t.Errorf("stateDirFor(other) = %q", stateDirFor(other))
	}
}

// An old-layout daemon an older build left running is the machine's one
// daemon: `ports up` talks to it where it is and spawns no sibling under the
// state dir (which would be a second agent exec on smol).
func TestPortsUpUsesOldLayoutDaemon(t *testing.T) {
	var out bytes.Buffer
	a := &App{Paths: testPaths(shortTempDir(t)), Stdout: &out, Stderr: io.Discard}
	saveRemote(t, a.Paths, "box", "3000")
	od := serveOwnerDaemon(t, config.LegacyPaths(a.Paths.Config), "box", map[string]session.Response{
		session.OpAdd: {OK: true, Host: 3000},
	}, false)

	if err := a.tunnelUp("box"); err != nil {
		t.Fatal(err)
	}
	if adds := od.requests(session.OpAdd); len(adds) != 1 || adds[0].Guest != 3000 {
		t.Fatalf("old daemon got adds %+v, want one for 3000", adds)
	}
	for _, f := range []string{"box.sock", "box.lock", "box.log"} {
		if _, err := os.Lstat(filepath.Join(a.Paths.RuntimeDir(), f)); err == nil {
			t.Errorf("%s exists under the state dir: a sibling daemon was spawned", f)
		}
	}
	if cl, err := session.Existing(a.Paths, "box"); err != nil || cl.Paths() != config.LegacyPaths(a.Paths.Config) {
		t.Errorf("Existing = %v, %v; want the old-layout daemon", cl, err)
	}
	if _, err := session.ExistingIn(a.Paths, "box"); err == nil {
		t.Error("ExistingIn found a daemon under the state dir")
	}
}

// update cycles a daemon at the old path: stopped there, waited out there,
// its forwards brought back by a daemon under the state dir.
func TestRestartDaemonsCyclesOldLayoutDaemon(t *testing.T) {
	old := daemonGoneTimeout
	daemonGoneTimeout = 2 * time.Second
	t.Cleanup(func() { daemonGoneTimeout = old })
	var out bytes.Buffer
	a := &App{Paths: testPaths(shortTempDir(t)), Stdout: &out, Stderr: &out}
	saveRemote(t, a.Paths, "box") // no ports: stopped, not respawned
	legacy := config.LegacyPaths(a.Paths.Config)
	od := serveOwnerDaemon(t, legacy, "box", map[string]session.Response{
		session.OpList: {OK: true, State: session.StateUp, Version: "v0.0.1"},
	}, true)

	res := a.restartDaemons()
	if len(od.requests(session.OpStop)) != 1 {
		t.Fatalf("old-layout daemon not stopped; output:\n%s", out.String())
	}
	if len(res.cycled) != 1 || len(res.failed) != 0 {
		t.Errorf("result = %+v; output:\n%s", res, out.String())
	}
	if _, err := os.Lstat(filepath.Join(legacy.RuntimeDir(), "box.sock")); err == nil {
		t.Error("old socket still there after WaitGone")
	}
}
