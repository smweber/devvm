package session

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/backend"
)

// closeCountTransport is an orderedTransport that also counts its Closes.
type closeCountTransport struct {
	*orderedTransport
	closes atomic.Int32
}

func (c *closeCountTransport) Close() error {
	c.closes.Add(1)
	return c.orderedTransport.Close()
}

// A termination signal runs the client-stop shutdown: the transport (the
// ssh master, smol exec or hub relay) is closed before the socket goes, and
// nothing is left behind. A second signal during it is absorbed, not a
// second shutdown.
func TestDaemonSignalShutsDown(t *testing.T) {
	dir := shortTempPaths(t)
	if err := os.MkdirAll(dir.RuntimeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := socketPath(dir, "t")
	tr := &closeCountTransport{orderedTransport: &orderedTransport{fakeTransport: newFakeTransport(), sock: sock}}
	d := newDaemon(dir, "t", "test", tr, nil)
	d.logf = t.Logf
	ln, info, err := listenControl(sock)
	if err != nil {
		t.Fatal(err)
	}
	d.ln, d.sockInfo = ln, info
	sigs := make(chan os.Signal, 2)
	done := make(chan error, 1)
	go func() { done <- d.run(sigs) }()
	if _, err := Existing(dir, "t"); err != nil {
		t.Fatalf("daemon not serving before the signal: %v", err)
	}
	sigs <- syscall.SIGTERM
	sigs <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not shut down on SIGTERM")
	}
	if n := tr.closes.Load(); n != 1 {
		t.Errorf("transport closed %d times, want 1", n)
	}
	if !tr.sockAtClose {
		t.Error("socket was unlinked before the transport closed")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket still present after a signalled shutdown: %v", err)
	}
}

// The daemon's handler really catches each signal it names: without it the
// first of these would kill the test binary, which is the bug.
func TestDaemonSignalsRegistered(t *testing.T) {
	sigs, stop := daemonSignals()
	defer stop()
	for _, s := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		if err := syscall.Kill(os.Getpid(), s); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-sigs:
			if got != s {
				t.Errorf("got %v, want %v", got, s)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%v not delivered to the daemon's handler", s)
		}
	}
}

// fakeMasterSSH puts an ssh on PATH that plays a ControlMaster in dir: a
// master is the file dir/alive (answers -O check), its socket dir/cm. A -M
// while one is alive degrades, as OpenSSH's does, and is logged as such.
func fakeMasterSSH(t *testing.T) (dir string, tr *sshTransport) {
	t.Helper()
	dir = t.TempDir()
	script := `#!/bin/sh
d='` + dir + `'
case "$1 $2" in
"-O check") [ -e "$d/alive" ]; exit $? ;;
"-O exit") echo exit >>"$d/log"; rm -f "$d/alive" "$d/cm"; exit 0 ;;
esac
for a; do
	if [ "$a" = -M ]; then
		if [ -e "$d/alive" ]; then echo degraded >>"$d/log"; else echo master >>"$d/log"; : >"$d/alive"; : >"$d/cm"; fi
		exit 0
	fi
done
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tr = &sshTransport{conn: backend.SSHConn{Host: "dev@example", ControlPath: filepath.Join(dir, "cm")}, deadCh: make(chan struct{}), stopCh: make(chan struct{})}
	return dir, tr
}

func readLog(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// A master a killed daemon left answering on the ControlPath is told to
// exit before a fresh one starts, instead of degrading the new -M into a
// plain connection that the orphan's answers hid.
func TestStartMasterExitsOrphan(t *testing.T) {
	dir, tr := fakeMasterSSH(t)
	for _, f := range []string{"alive", "cm"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.startMaster(); err != nil {
		t.Fatal(err)
	}
	if got := readLog(t, dir); got != "exit\nmaster" {
		t.Errorf("ssh calls = %q, want the orphan exited, then a fresh master", got)
	}
}

// A socket nobody answers is only removed: there is nothing to exit.
func TestStartMasterRemovesDeadSocket(t *testing.T) {
	dir, tr := fakeMasterSSH(t)
	if err := os.WriteFile(filepath.Join(dir, "cm"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tr.startMaster(); err != nil {
		t.Fatal(err)
	}
	if got := readLog(t, dir); got != "master" {
		t.Errorf("ssh calls = %q, want only a fresh master", got)
	}
}
