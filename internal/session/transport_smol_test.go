package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// fakeSmolvmGuest puts a smolvm on PATH that lists the VMs in the file ls
// names, logs every call, and answers agentbin's read-only probes (uname,
// $HOME, the sha256 check) the way a guest with nothing installed would.
func fakeSmolvmGuest(t *testing.T) (ls, log string) {
	t.Helper()
	dir := t.TempDir()
	ls, log = filepath.Join(dir, "ls.json"), filepath.Join(dir, "smolvm.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"  'machine ls --json') cat " + ls + " ;;\n" +
		"  *uname*) echo 'Linux x86_64' ;;\n" +
		"  *'echo $HOME'*) echo /root ;;\n" +
		"  *sha256sum*) printf -- '-\\n-\\n' ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "smolvm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return ls, log
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSmolInstallGate(t *testing.T) {
	m := &config.Machine{Name: "web", Backend: config.BackendSmol}
	if err := smolInstallGate(m)(); !errors.Is(err, backend.ErrUnmanaged) || !strings.Contains(err.Error(), "setup --adopt web") {
		t.Errorf("unmanaged gate = %v, want ErrUnmanaged naming setup --adopt", err)
	}
	m.SetManaged(true)
	if err := smolInstallGate(m)(); err != nil {
		t.Errorf("managed gate = %v", err)
	}
}

// runDaemonBounded is RunDaemon with a deadline: a daemon that dials (and
// so serves) where it should refuse fails the test in seconds rather than
// hanging the package until go test's own timeout.
func runDaemonBounded(t *testing.T, paths config.Paths, d backend.Discovered) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunDaemon(ctx, paths, d.Machine, d.Backend, "test") }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("RunDaemon did not refuse within 10s: it dialled")
		return nil
	}
}

// The daemon asks again before it dials (config.md §2), the backend rather
// than any host file: a VM devvm made, deleted and recreated under the same
// name with raw smolvm while the daemon was down is listed without devvm's
// label, so the dial refuses it as not listed and stops before any exec:
// not even agentbin's read-only probes run in it.
func TestDaemonRefusesInstallIntoRecreatedVM(t *testing.T) {
	ls, log := fakeSmolvmGuest(t)
	state, err := os.MkdirTemp("", "dv") // short: the daemon's socket lives here
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	paths := config.Paths{Config: t.TempDir(), State: state}
	writeFile(t, ls, `[{"name":"web","state":"running","labels":{"devvm":"true"}}]`)
	d, ok, err := backend.Discover(paths, "web")
	if err != nil || !ok || !d.Machine.Managed() {
		t.Fatalf("Discover = %+v, %v, %v; want a managed VM", d.Machine, ok, err)
	}
	// Recreated while no daemon ran: same name, no label.
	writeFile(t, ls, `[{"name":"web","state":"running","labels":{}}]`)
	err = runDaemonBounded(t, paths, d)
	if !errors.Is(err, errVMNotListed) {
		t.Fatalf("RunDaemon = %v, want it not dialled", err)
	}
	data, _ := os.ReadFile(log)
	for _, call := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if call != "machine ls --json" {
			t.Errorf("unlabelled VM got %q", call)
		}
	}
	// A stopped VM is still never dialed at all.
	writeFile(t, ls, `[{"name":"web","state":"stopped","labels":{"devvm":"true"}}]`)
	if err := runDaemonBounded(t, paths, d); !errors.Is(err, errVMNotRunning) {
		t.Errorf("stopped VM: RunDaemon = %v, want errVMNotRunning", err)
	}
}
