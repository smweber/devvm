package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// BROWSER on a remote box (browser-bridge.md §5). `setup` on a managed
// box runs the agent step (agent + shim + /etc/profile.d/devvm.sh), as on
// smol; an adopted host is never shaped. `attach` subscribes on the
// box's daemon (whose agent exec carries the events) and copies BROWSER
// into the dev tmux session, guarded in the guest on the shim being
// executable: root's on a managed box, ~/.local/bin's on an adopt host,
// where it exists only once `auth` installed it with consent. attach
// itself installs nothing and writes no profile.d.
func TestRemoteSetupAndAttachBrowser(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	for _, be := range []string{"managed", "unmanaged"} {
		t.Run(be, func(t *testing.T) {
			a := newTestApp(t)
			log := fakeRemote(t, "")
			m := config.NewRemote("cloud", be == "managed", "dev@cloud.example")
			if err := writeEntry(a.Paths, m); err != nil {
				t.Fatal(err)
			}
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
			setup, _ := os.ReadFile(log)
			if got := strings.Contains(string(setup), "AGENT-INSTALL cloud"); got != (be == "managed") {
				t.Fatalf("setup ran the agent step: %v, want %v\n%s", got, be == "managed", setup)
			}

			fb := &fakeBridge{}
			opens := stubBridge(t, fb, nil)
			if err := runTree(t, a, "attach", "cloud"); err != nil {
				t.Fatalf("attach: %v\n%s", err, a.Stderr)
			}
			if len(*opens) != 1 || (*opens)[0] != "cloud" || fb.subscribed != 1 || fb.closed != 1 {
				t.Fatalf("bridge: opens %v, subscribed %d, closed %d; want one held for the attach", *opens, fb.subscribed, fb.closed)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			calls := strings.TrimPrefix(string(data), string(setup))
			shim := `s=` + backend.ManagedShimPath
			if be == "unmanaged" {
				shim = `s="$HOME/.local/bin/devvm-open-url"`
			}
			for _, want := range []string{shim, `[ -x "$s" ] && tmux set-environment -t dev BROWSER "$s"`, "exec tmux attach-session -t dev"} {
				if !strings.Contains(calls, want) {
					t.Errorf("attach's ssh calls lack %q:\n%s", want, calls)
				}
			}
			for _, bad := range []string{"profile.d", "bin/devvm-agent", "uname", "AGENT-INSTALL"} {
				if strings.Contains(calls, bad) {
					t.Errorf("attach's ssh calls mention %q:\n%s", bad, calls)
				}
			}
		})
	}
}

// A remote shell is the login shell, untouched: no remote command (so no
// BROWSER from the exec, which sshd would drop anyway), the bridge held.
func TestRemoteShellSendsNoCommand(t *testing.T) {
	t.Setenv(backend.NoSubscribeEnv, "")
	a := newTestApp(t)
	log := fakeRemote(t, "")
	if err := writeEntry(a.Paths, config.NewRemote("cloud", true, "dev@cloud.example")); err != nil {
		t.Fatal(err)
	}
	fb := &fakeBridge{}
	opens := stubBridge(t, fb, nil)
	if err := runTree(t, a, "shell", "cloud"); err != nil {
		t.Fatalf("shell: %v\n%s", err, a.Stderr)
	}
	data, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if last := lines[len(lines)-1]; !strings.HasSuffix(last, "-t dev@cloud.example") || strings.Contains(last, "BROWSER") {
		t.Errorf("shell ran %q, want a bare login shell", last)
	}
	if len(*opens) != 1 || fb.subscribed != 1 {
		t.Errorf("shell held no bridge: opens %v", *opens)
	}
}
