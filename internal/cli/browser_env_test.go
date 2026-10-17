package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

// `setup cloud` and `attach cloud` on a remote box leave BROWSER alone:
// no agent or shim install (not even its uname probe; the prereqs'
// /etc/devvm-agent marker is older and unrelated), no /etc/profile.d
// write, no tmux set-environment (browser-bridge.md §5: remote is deferred
// with the ssh agent exec).
func TestRemoteSetupAndAttachLeaveBrowserAlone(t *testing.T) {
	for _, be := range []string{"managed", "unmanaged"} {
		t.Run(be, func(t *testing.T) {
			a := newTestApp(t)
			log := fakeSSH(t, "exit 0")
			t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "")
			m := config.NewRemote("cloud", be == "managed", "dev@cloud.example")
			if err := writeEntry(a.Paths, m); err != nil {
				t.Fatal(err)
			}
			// setup never shapes an adopted host; it refuses before dialing.
			err := runTree(t, a, "setup", "cloud")
			if be == "managed" && err != nil {
				t.Fatalf("setup: %v\n%s", err, a.Stderr)
			}
			if be == "unmanaged" {
				if err == nil || !strings.Contains(err.Error(), "never shapes") {
					t.Fatalf("setup on an adopted host: err = %v, want a refusal", err)
				}
				if _, statErr := os.Stat(log); statErr == nil {
					t.Fatal("setup's refusal dialed the host")
				}
			}
			if err := runTree(t, a, "attach", "cloud"); err != nil {
				t.Fatalf("attach: %v\n%s", err, a.Stderr)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			calls := string(data)
			if !strings.Contains(calls, "new-session") {
				t.Fatalf("attach never reached ssh:\n%s", calls)
			}
			for _, bad := range []string{"profile.d", "devvm-open-url", "bin/devvm-agent", "uname", "set-environment", "BROWSER"} {
				if strings.Contains(calls, bad) {
					t.Errorf("ssh calls mention %q:\n%s", bad, calls)
				}
			}
		})
	}
}
