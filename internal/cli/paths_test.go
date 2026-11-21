package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// How --config-dir and --state-dir resolve: a non-default --config-dir alone
// is an isolated instance with its state inside it; naming the default
// config dir keeps the default state dir; --state-dir always wins.
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
		root.SetArgs(append(tt.args, "config", "path"))
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

// `ports up`/`start` kick the daemon even when it is up: the retry is a
// no-op there, but the kick also syncs a smol guest's clock (after a host
// wake). The "retrying now" line is for a reconnecting daemon only.
func TestPortsUpAlwaysKicks(t *testing.T) {
	for _, state := range []string{session.StateUp, session.StateReconnecting} {
		t.Run(state, func(t *testing.T) {
			var out bytes.Buffer
			a := &App{Paths: testPaths(shortTempDir(t)), Stdout: &out, Stderr: io.Discard}
			saveRemote(t, a.Paths, "box", "3000")
			od := serveOwnerDaemon(t, a.Paths, "box", map[string]session.Response{
				session.OpAdd:  {OK: true, Host: 3000},
				session.OpList: {OK: true, State: state},
			}, false)
			if err := a.tunnelUp("box"); err != nil {
				t.Fatal(err)
			}
			if kicks := od.requests(session.OpKick); len(kicks) != 1 {
				t.Fatalf("%d kicks, want 1", len(kicks))
			}
			retry := strings.Contains(out.String(), "retrying now")
			if retry != (state == session.StateReconnecting) {
				t.Errorf("state %s: output %q", state, out.String())
			}
		})
	}
}
