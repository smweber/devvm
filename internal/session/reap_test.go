package session

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// staleSocket leaves a unix socket file nobody answers, as a killed daemon
// or master does.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
}

// A killed daemon's leftovers go: its log, and the socket and master nobody
// answers. Its lock stays until RemoveLock (a starter may be blocked on
// it). Another machine's files stay.
func TestReapRemovesStaleRunFiles(t *testing.T) {
	dir := shortTempDir(t)
	if err := config.EnsureRuntimeDir(dir); err != nil {
		t.Fatal(err)
	}
	name := "h/web"
	touch(t, lockPath(dir, name))
	touch(t, logPath(dir, name))
	staleSocket(t, socketPath(dir, name))
	staleSocket(t, masterPath(dir, name))
	touch(t, lockPath(dir, "h/api"))
	if err := Reap(dir, name, time.Second); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{logPath(dir, name), socketPath(dir, name), masterPath(dir, name)} {
		if exists(p) {
			t.Errorf("%s survived the reap", filepath.Base(p))
		}
	}
	if !exists(lockPath(dir, name)) {
		t.Error("Reap unlinked the startup lock; only RemoveLock may, once the conf is gone")
	}
	RemoveLock(dir, name)
	if exists(lockPath(dir, name)) {
		t.Error("RemoveLock left the lock")
	}
	if !exists(lockPath(dir, "h/api")) {
		t.Error("another machine's lock was reaped")
	}
	// Nothing there at all is fine too.
	if err := Reap(dir, name, time.Second); err != nil {
		t.Errorf("reap of nothing: %v", err)
	}
}

// A running daemon is stopped and waited out before anything is removed;
// a master something still answers on is never removed.
func TestReapStopsDaemonAndKeepsLiveMaster(t *testing.T) {
	dir := shortTempDir(t)
	d := startSockDaemon(t, dir)
	done := make(chan struct{})
	go func() { d.loop(); d.shutdown(); close(done) }()
	touch(t, lockPath(dir, "t"))
	touch(t, logPath(dir, "t"))
	master, err := net.Listen("unix", masterPath(dir, "t"))
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err := Reap(dir, "t", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("Reap returned before the daemon was gone")
	}
	if exists(socketPath(dir, "t")) || exists(logPath(dir, "t")) {
		t.Error("the stopped daemon's run files survived")
	}
	if !exists(masterPath(dir, "t")) {
		t.Error("a live master's socket was removed")
	}
}

// A daemon mid-start (holding the startup lock) keeps every file: its
// socket is about to be served.
func TestReapLeavesStartingDaemon(t *testing.T) {
	dir := shortTempDir(t)
	if err := config.EnsureRuntimeDir(dir); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireLock(lockPath(dir, "t"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	staleSocket(t, socketPath(dir, "t"))
	touch(t, logPath(dir, "t"))
	err = Reap(dir, "t", time.Second)
	if err == nil || !strings.Contains(err.Error(), "starting") {
		t.Fatalf("reap under a held startup lock: err = %v", err)
	}
	if !exists(lockPath(dir, "t")) || !exists(socketPath(dir, "t")) || !exists(logPath(dir, "t")) {
		t.Error("files of a starting daemon were removed")
	}
	// The flock is the daemon's own kind: released, the reap goes through.
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := Reap(dir, "t", time.Second); err != nil {
		t.Fatal(err)
	}
	if exists(socketPath(dir, "t")) || exists(logPath(dir, "t")) {
		t.Error("files survived once the lock was free")
	}
}

func TestHubRuntimeNames(t *testing.T) {
	dir := shortTempDir(t)
	if err := config.EnsureRuntimeDir(dir); err != nil {
		t.Fatal(err)
	}
	run := config.RuntimeDir(dir)
	for _, f := range []string{"h@web.sock", "h@web.lock", "h@api.log", "h@db.master", "h@x.tmp", "hh@web.lock", "h.lock", "other@web.sock"} {
		touch(t, filepath.Join(run, f))
	}
	if got, want := HubRuntimeNames(dir, "h"), []string{"h/api", "h/db", "h/web"}; !slices.Equal(got, want) {
		t.Errorf("HubRuntimeNames = %v, want %v", got, want)
	}
}
