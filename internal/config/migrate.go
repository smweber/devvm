package config

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// MigrateState moves host state an older build left in the config dir (run/,
// cache/, known_hosts, cm-*) to p.State (config.md §1). It runs on every
// invocation but costs a few stats once there is nothing left to move (plus a
// dial per old cm-* socket while one exists; a live old master is rare and
// short-lived, ControlPersist=60).
//
// It never replaces or deletes anything another process may be using. Files
// are moved by hard link plus unlink, so a destination that exists (a new
// daemon's flocked lock, a fresh pin, a log a client just opened) is never
// replaced: it wins, and the old copy is dropped. Two racing runs therefore
// converge: each link either lands or finds the file already there, and an
// unlink that finds nothing is fine.
//
// A machine whose old control socket still exists, answering or not, keeps
// all its run files where they are: a daemon serving there (session clients
// use it in place, `update` cycles it) or one mid-shutdown, which closes its
// listener before tearing down its transport and unlinking the socket last.
// An old socket is never removed here; one a killed daemon left is cleared by
// `delete` (session.Reap) or simply lingers. Old cm-* ssh masters are
// removed only when nothing answers on them, as ssh itself would.
//
// Best-effort and silent: a state file left behind costs a re-pin or a cold
// cache, never a failed command.
func MigrateState(p Paths) {
	legacy := LegacyPaths(p.Config)
	if filepath.Clean(p.State) == filepath.Clean(legacy.State) {
		return // a non-default --config-dir alone: state is still in it
	}
	if !exists(legacy.RuntimeDir()) && !exists(legacy.CacheDir()) &&
		!exists(legacy.KnownHostsPath()) && len(controlSockets(legacy.State)) == 0 {
		return
	}
	if err := os.MkdirAll(p.State, 0o700); err != nil {
		return
	}

	// Only a regular file moves: a known_hosts symlink (say, a relative
	// one into dotfiles) would dangle from another dir, so it stays.
	_ = moveFile(legacy.KnownHostsPath(), p.KnownHostsPath())

	if entries, err := os.ReadDir(legacy.CacheDir()); err == nil {
		if os.MkdirAll(p.CacheDir(), 0o755) == nil {
			for _, e := range entries {
				_ = moveFile(filepath.Join(legacy.CacheDir(), e.Name()), filepath.Join(p.CacheDir(), e.Name()))
			}
		}
		_ = os.Remove(legacy.CacheDir()) // only once empty
	}

	for _, s := range controlSockets(legacy.State) {
		if !answers(s) {
			_ = os.Remove(s)
		}
	}

	migrateRunDir(legacy.RuntimeDir(), p)
}

// migrateRunDir moves run files machine by machine, skipping every machine
// whose control socket exists (see MigrateState).
func migrateRunDir(old string, p Paths) {
	entries, err := os.ReadDir(old)
	if err != nil {
		return
	}
	held := map[string]bool{}
	for _, e := range entries {
		if base, ok := strings.CutSuffix(e.Name(), ".sock"); ok {
			held[base] = true
		}
	}
	if err := p.EnsureRuntimeDir(); err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		src := filepath.Join(old, name)
		switch {
		case name == "changed":
			// The bare marker. A machine named "changed" has changed.sock
			// and the like, never this exact name. It is rewritten by the
			// next state change, in the run/ that is watched now.
			_ = os.Remove(src)
		case held[strings.TrimSuffix(name, filepath.Ext(name))]:
		case e.Type()&fs.ModeSocket != 0:
			// A daemon's ssh master with no control socket: its daemon
			// is gone, so a master nothing answers on is stale.
			if !answers(src) {
				_ = os.Remove(src)
			}
		default:
			_ = moveFile(src, filepath.Join(p.RuntimeDir(), name))
		}
	}
	_ = os.Remove(old) // only once empty: a held machine's files keep it
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// controlSockets is the old cm-* ssh ControlMaster sockets in dir. Only
// sockets: the config dir may live in dotfiles, and a regular file that
// happens to match is the user's.
func controlSockets(dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, "cm-*"))
	var out []string
	for _, s := range m {
		if info, err := os.Lstat(s); err == nil && info.Mode()&fs.ModeSocket != 0 {
			out = append(out, s)
		}
	}
	return out
}

// answers reports whether something accepts connections on a unix socket.
func answers(path string) bool {
	c, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// moveFile moves a regular file without ever replacing dst: link, then
// unlink src. If dst exists it wins and src is dropped. Across filesystems
// ($XDG_STATE_HOME on another mount) the content is copied to a temp file in
// dst's dir and linked into place the same way; only that temp file is ever
// removed on failure. Anything but a regular file is left alone.
func moveFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil || !info.Mode().IsRegular() {
		return err
	}
	err = linkAcross(src, dst)
	if errors.Is(err, syscall.EXDEV) {
		err = copyInto(src, dst, info.Mode().Perm())
	}
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return os.Remove(src)
}

// linkAcross is moveFile's first link, a seam so tests can take the
// cross-filesystem branch on one filesystem.
var linkAcross = os.Link

func copyInto(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".migrate-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Link(tmp.Name(), dst)
}
