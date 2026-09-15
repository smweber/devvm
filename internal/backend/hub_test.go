package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

func TestForHub(t *testing.T) {
	hub := config.NewHub("h", "u@host")
	b, err := For(hub, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The hub itself is reached like a remote box: same ssh plumbing, and
	// unmanaged, so the user's own known_hosts (no TOFU pin file).
	sb, ok := b.(*sshBackend)
	if !ok || b.Kind() != config.BackendHub {
		t.Fatalf("hub backend = %T kind %q, want *sshBackend/hub", b, b.Kind())
	}
	if flags := strings.Join(sb.sshFlags(), " "); strings.Contains(flags, "UserKnownHostsFile") {
		t.Errorf("hub must not get the managed known_hosts pin: %s", flags)
	}

	rec := &config.Machine{Name: "h/web", Hub: hub, Backend: config.BackendHub, SSHHost: hub.SSHHost}
	b, err = For(rec, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hb, ok := b.(*hubBackend)
	if !ok || hb.hub.m != hub {
		t.Fatalf("hub-machine backend = %T, want *hubBackend over the hub conf", b)
	}
	if ok, err := hb.Exists(); !ok || err != nil {
		t.Errorf("Exists = %v, %v; resolve never asks the hub", ok, err)
	}
	if _, err := hb.Spawn(context.Background(), ExecOpts{}, "devvm-agent", "serve"); err == nil {
		t.Error("Spawn must refuse: the hub daemon owns the only agent exec")
	}
	for name, err := range map[string]error{
		"run":    hb.Run(context.Background(), ExecOpts{}, "true"),
		"start":  hb.PowerStart(),
		"attach": hb.Attach(""),
	} {
		if !errors.Is(err, ErrHubProxy) {
			t.Errorf("%s = %v, want ErrHubProxy until the proxy lands", name, err)
		}
	}
}

// The version probe and (later) the proxy must run under the hub user's
// login shell, since only it has ~/.local/bin or Homebrew on PATH. Render the
// remote command exactly as ssh would hand it to the remote shell and run it
// through a real sh with a fake devvm reachable only via a profile, to prove
// the quoting survives both shells and -l is honoured.
func TestLoginShellArgvRunsUnderLoginShell(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\nprintf 'devvm version v1.2.3 [%s]\\n' \"$*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "devvm"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	// Only the login shell sees this PATH, as on the hubs.
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte("PATH="+bin+":$PATH\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := remoteCommand(ExecOpts{}, LoginShellArgv("devvm", "--version", "it's", "a b"))
	cmd := exec.Command("sh", "-c", remote) // what sshd does with the command string
	cmd.Env = append(os.Environ(), "HOME="+home, "SHELL=/bin/sh", "ENV=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if want := "devvm version v1.2.3 [--version it's a b]\n"; string(out) != want {
		t.Errorf("got %q, want %q (remote command: %s)", out, want, remote)
	}
	if !strings.Contains(remote, `-lc "$0"`) {
		t.Errorf("not a login shell invocation: %s", remote)
	}
}
