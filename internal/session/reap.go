package session

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/smweber/devvm/internal/config"
)

// Reap is `delete`'s end of a machine's runtime state: it stops the
// machine's daemon if one answers, waits (up to timeout) until it is gone,
// then removes the run files nothing else ever removes: NAME.log, and a
// NAME.sock or NAME.master nobody answers any more (a daemon or master that
// was killed rather than stopped). NAME.lock is not among them: see
// RemoveLock, which runs only once the entry is gone. Both the
// state dir's run/ and a pre-split one in the config dir are reaped. A
// socket something still answers on is never removed: a live master belongs to a daemon still shutting down, and a
// live control socket to a daemon started since.
//
// The files go only under the startup lock (acquireLock), taken without
// waiting: holding it, no daemon is between its first dial and its listen,
// so a control socket that does not answer is stale, not about to be
// served. A daemon starting right now is left to it, and so are its files.
func Reap(p config.Paths, name string, timeout time.Duration) error {
	// Up to two daemons: one under the state dir and one an older build
	// left in the pre-split layout (Existing finds the new one first).
	for range 2 {
		cl, err := Existing(p, name)
		if err != nil {
			break
		}
		_ = cl.Stop()
		if !WaitGone(cl.Paths(), name, timeout) {
			return fmt.Errorf("its forward daemon did not exit within %s; run files left in place", timeout)
		}
	}
	for _, at := range runLayouts(p) {
		if err := reapFiles(at, name); err != nil {
			return err
		}
	}
	return nil
}

// runLayouts is p plus, when it differs, the pre-split layout, whose run/
// may still hold an older build's files.
func runLayouts(p config.Paths) []config.Paths {
	if old := config.LegacyPaths(p.Config); old != p {
		return []config.Paths{p, old}
	}
	return []config.Paths{p}
}

func reapFiles(p config.Paths, name string) error {
	lp := lockPath(p, name)
	lock, err := os.OpenFile(lp, os.O_RDWR, 0)
	switch {
	case err == nil:
		defer lock.Close()
		if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			return fmt.Errorf("a forward daemon for it is starting; run files left in place")
		}
	case os.IsNotExist(err):
	default:
		return err
	}
	sock := socketPath(p, name)
	if answers(sock) {
		return fmt.Errorf("a forward daemon for it is running again; run files left in place")
	}
	_ = os.Remove(sock)
	if master := masterPath(p, name); !answers(master) {
		_ = os.Remove(master)
	}
	_ = os.Remove(logPath(p, name))
	return nil
}

// RemoveLock unlinks a machine's startup lock. Only once the machine can no
// longer be resolved (its VM deleted, or its devvm.toml entry removed): a starter already blocked on the
// lock holds the old inode and would take it when released, while the next
// starter's O_CREATE made a fresh file and took that at once, putting two
// daemons past the lock (two agent execs on smol, and the loser's
// stale-socket Remove unlinking the winner's socket). With the machine gone,
// that next starter fails at resolve before it reaches the lock. A
// HUB/NAME keeps its lock after `delete HUB/NAME`, since the hub entry still
// resolves it; an empty file is harmless.
func RemoveLock(p config.Paths, name string) {
	for _, at := range runLayouts(p) {
		_ = os.Remove(lockPath(at, name))
	}
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

// HubRuntimeNames is every HUB/NAME with a run file on this host (a
// daemon's socket, lock or log, a master): what `delete HUB` must reap,
// whether or not a daemon is running or saved forwards exist.
func HubRuntimeNames(p config.Paths, hub string) []string {
	prefix := config.RuntimeName(config.JoinHubName(hub, ""))
	var matches []string
	for _, at := range runLayouts(p) { // an older build's run/ too
		m, _ := filepath.Glob(filepath.Join(at.RuntimeDir(), prefix+"*"))
		matches = append(matches, m...)
	}
	var names []string
	for _, p := range matches {
		base := filepath.Base(p)
		ext := filepath.Ext(base)
		switch ext {
		case ".sock", ".lock", ".log", ".master":
		default:
			continue
		}
		display := config.DisplayName(strings.TrimSuffix(base, ext))
		if h, _, ok, err := config.SplitHubName(display); !ok || err != nil || h != hub {
			continue
		}
		if !slices.Contains(names, display) {
			names = append(names, display)
		}
	}
	slices.Sort(names)
	return names
}
