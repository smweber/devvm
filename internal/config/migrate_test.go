package config

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// migrateRig is a pre-split config dir under /tmp (unix socket paths must
// stay short for macOS) with a state dir beside it that does not exist yet.
func migrateRig(t *testing.T) Paths {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "devvm-mig-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	p := Paths{Config: filepath.Join(root, "c"), State: filepath.Join(root, "s")}
	if err := os.MkdirAll(filepath.Join(p.Config, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func gone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s still exists (%v)", path, err)
	}
}

// staleSocket leaves a socket file nothing listens on, as a killed daemon or
// ssh master does.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
}

func liveSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
}

func TestMigrateStateMovesHostState(t *testing.T) {
	p := migrateRig(t)
	legacy := LegacyPaths(p.Config)
	write(t, filepath.Join(p.Config, "machines", "web.toml"), "name = 'web'\n")
	write(t, legacy.KnownHostsPath(), "host ssh-ed25519 AAAA\n")
	write(t, filepath.Join(legacy.CacheDir(), "hub-h.list"), "web\tsmol\trunning\t-\n")
	write(t, filepath.Join(legacy.RuntimeDir(), "web.log"), "log\n")
	write(t, filepath.Join(legacy.RuntimeDir(), "h@web.lock"), "")
	write(t, legacy.ChangedPath(), "stamp\n")
	staleSocket(t, filepath.Join(legacy.RuntimeDir(), "api.master")) // its daemon is gone
	staleSocket(t, filepath.Join(p.Config, "cm-abc"))
	write(t, filepath.Join(p.Config, "cm-notes"), "the user's\n") // not a socket: not ours

	MigrateState(p)

	if got := read(t, p.KnownHostsPath()); got != "host ssh-ed25519 AAAA\n" {
		t.Errorf("known_hosts = %q", got)
	}
	if got := read(t, filepath.Join(p.CacheDir(), "hub-h.list")); got == "" {
		t.Error("hub cache not moved")
	}
	if got := read(t, filepath.Join(p.RuntimeDir(), "web.log")); got != "log\n" {
		t.Errorf("log = %q", got)
	}
	read(t, filepath.Join(p.RuntimeDir(), "h@web.lock"))
	if info, err := os.Stat(p.RuntimeDir()); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("run dir = %v, %v; want 0700", info, err)
	}
	// Stale masters are not carried over; the marker is rewritten on the
	// next change, in the new run dir.
	gone(t, filepath.Join(p.RuntimeDir(), "api.master"))
	gone(t, p.ChangedPath())
	for _, old := range []string{legacy.KnownHostsPath(), legacy.CacheDir(), legacy.RuntimeDir(), filepath.Join(p.Config, "cm-abc")} {
		gone(t, old)
	}
	// The config dir keeps the user's files, a cm-* regular file included.
	if got := read(t, filepath.Join(p.Config, "machines", "web.toml")); got != "name = 'web'\n" {
		t.Errorf("conf touched: %q", got)
	}
	if got := read(t, filepath.Join(p.Config, "cm-notes")); got != "the user's\n" {
		t.Errorf("cm-notes = %q", got)
	}

	MigrateState(p) // idempotent: nothing left to move, nothing disturbed
	read(t, p.KnownHostsPath())
	read(t, filepath.Join(p.RuntimeDir(), "web.log"))
}

// A machine whose old control socket exists keeps every run file where it
// is, whether its daemon answers (serving: clients use it in place) or not
// (mid-shutdown, or killed: Reap's business); a live old ssh master stays
// too. Everything else moves, and a later run, once the socket is gone,
// moves the rest.
func TestMigrateStateLeavesHeldMachinesAlone(t *testing.T) {
	p := migrateRig(t)
	legacy := LegacyPaths(p.Config)
	run := legacy.RuntimeDir()
	liveSocket(t, filepath.Join(run, "h@web.sock"))
	liveSocket(t, filepath.Join(run, "h@web.master"))
	write(t, filepath.Join(run, "h@web.lock"), "")
	write(t, filepath.Join(run, "h@web.log"), "live\n")
	staleSocket(t, filepath.Join(run, "box.sock")) // shutting down: not answering
	staleSocket(t, filepath.Join(run, "box.master"))
	write(t, filepath.Join(run, "box.lock"), "")
	write(t, filepath.Join(run, "api.log"), "dead\n")
	liveSocket(t, filepath.Join(p.Config, "cm-live"))

	MigrateState(p)

	for _, f := range []string{"h@web.sock", "h@web.master", "h@web.lock", "h@web.log", "box.sock", "box.master", "box.lock"} {
		if _, err := os.Lstat(filepath.Join(run, f)); err != nil {
			t.Errorf("held machine's %s moved or removed: %v", f, err)
		}
		gone(t, filepath.Join(p.RuntimeDir(), f))
	}
	if _, err := os.Lstat(filepath.Join(p.Config, "cm-live")); err != nil {
		t.Errorf("live master socket removed: %v", err)
	}
	if got := read(t, filepath.Join(p.RuntimeDir(), "api.log")); got != "dead\n" {
		t.Errorf("api.log = %q", got)
	}

	// The old daemon exits (unlinking its socket, its master gone with
	// it); its lock and log move on the next run, and a lock already in the
	// new run dir wins, keeping its inode (a new daemon may hold its flock).
	os.Remove(filepath.Join(run, "h@web.sock"))
	os.Remove(filepath.Join(run, "h@web.master"))
	newLock := filepath.Join(p.RuntimeDir(), "h@web.lock")
	write(t, newLock, "new")
	before, _ := os.Stat(newLock)
	MigrateState(p)
	after, _ := os.Stat(newLock)
	if !os.SameFile(before, after) || read(t, newLock) != "new" {
		t.Error("new lock replaced")
	}
	if got := read(t, filepath.Join(p.RuntimeDir(), "h@web.log")); got != "live\n" {
		t.Errorf("log = %q", got)
	}
	gone(t, filepath.Join(run, "h@web.lock"))
}

// A pin already in the state dir is never overwritten by an old one.
func TestMigrateStateExistingDestinationWins(t *testing.T) {
	p := migrateRig(t)
	legacy := LegacyPaths(p.Config)
	write(t, legacy.KnownHostsPath(), "old\n")
	write(t, p.KnownHostsPath(), "new\n")
	write(t, filepath.Join(legacy.CacheDir(), "hub-h.list"), "old\n")
	write(t, filepath.Join(p.CacheDir(), "hub-h.list"), "new\n")

	MigrateState(p)

	if got := read(t, p.KnownHostsPath()); got != "new\n" {
		t.Errorf("known_hosts = %q", got)
	}
	if got := read(t, filepath.Join(p.CacheDir(), "hub-h.list")); got != "new\n" {
		t.Errorf("cache = %q", got)
	}
	gone(t, legacy.KnownHostsPath())
	gone(t, legacy.CacheDir()) // a cache is safe to drop
}

// Across filesystems a file is copied to a temp name and linked into place,
// so it lands whole, an existing destination still wins, and no temp file
// is left behind either way.
func TestMoveFileAcrossFilesystems(t *testing.T) {
	orig := linkAcross
	linkAcross = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EXDEV} }
	t.Cleanup(func() { linkAcross = orig })
	src, dst := t.TempDir(), t.TempDir()

	write(t, filepath.Join(src, "a"), "moved\n")
	if err := os.Chmod(filepath.Join(src, "a"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := moveFile(filepath.Join(src, "a"), filepath.Join(dst, "a")); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dst, "a")); got != "moved\n" {
		t.Errorf("a = %q", got)
	}
	if info, _ := os.Stat(filepath.Join(dst, "a")); info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
	gone(t, filepath.Join(src, "a"))

	write(t, filepath.Join(src, "b"), "old\n")
	write(t, filepath.Join(dst, "b"), "new\n")
	if err := moveFile(filepath.Join(src, "b"), filepath.Join(dst, "b")); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dst, "b")); got != "new\n" {
		t.Errorf("b = %q, want the existing file kept", got)
	}
	gone(t, filepath.Join(src, "b"))

	if entries, _ := os.ReadDir(dst); len(entries) != 2 {
		t.Errorf("dst holds %d entries, want 2 (no temp left)", len(entries))
	}
}

// With state in the config dir (a non-default --config-dir alone) there is
// nothing to migrate, and with no old state nothing is created.
func TestMigrateStateNoop(t *testing.T) {
	p := migrateRig(t)
	legacy := LegacyPaths(p.Config)
	write(t, filepath.Join(legacy.RuntimeDir(), "web.log"), "x")
	MigrateState(legacy)
	read(t, filepath.Join(legacy.RuntimeDir(), "web.log"))

	os.RemoveAll(legacy.RuntimeDir())
	MigrateState(p)
	gone(t, p.State)
}
