package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/smweber/devvm/internal/config"
)

// watchDebounce coalesces a burst of filesystem events (a conf save is several
// writes; a daemon start touches the socket, the lock, and the marker) into
// one snapshot.
const watchDebounce = 200 * time.Millisecond

// statusExitFlag is the hidden `status --watch` flag that ends the watch on
// stdin EOF. The laptop's hub watch pipe passes it so the hub-side watcher
// dies with the laptop's (hubListArgv); nothing else sets it.
const statusExitFlag = "--exit-on-stdin-eof"

// runStatusWatch is `status --plain --watch`: the plain listing, re-emitted
// whenever devvm's own state on disk changes — or, unless local, whenever a
// hub's held listing pipe emits a block. It never polls; see watchStatus for
// what it observes and what it can't. exitOnEOF (statusExitFlag) ends it
// when stdin closes: over ssh that is the laptop side going away.
func (a *App) runStatusWatch(ctx context.Context, local, exitOnEOF bool) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if exitOnEOF {
		go func() {
			_, _ = io.Copy(io.Discard, a.stdin())
			cancel()
		}()
	}
	if local {
		return watchStatus(ctx, a.ConfigDir, func() string { return a.plainSnapshot(statusScope{local: true}) }, a.Stdout, watchDebounce, nil)
	}
	return a.watchWithHubs(ctx, a.Stdout, watchDebounce)
}

// watchWithHubs is the merged --watch: the local loop plus a hubWatcher
// whose pipes wake it. The hub confs are re-read on every snapshot, and the
// loop takes one on every machines/ event, so the pipe set follows a hub
// conf created, deleted or re-pointed while watching. The first block waits
// (bounded by the listing deadline) for the pipes to speak, so a consumer
// does not start from a flash of `unreachable`.
func (a *App) watchWithHubs(ctx context.Context, out io.Writer, debounce time.Duration) error {
	hw := newHubWatcher(a)
	defer hw.stop()
	snapshot := func() string {
		hubs := a.hubConfs() // once per block: the pipe set and the rows use the same read
		hw.sync(hubs)
		return a.plainSnapshot(statusScope{hubs: hubs, listings: hw.listings()})
	}
	hw.sync(a.hubConfs())
	hw.settle(ctx, hubTimes.deadline)
	return watchStatus(ctx, a.ConfigDir, snapshot, out, debounce, hw.wake)
}

// plainSnapshot renders the plain listing as one string so identical
// snapshots can be suppressed byte-for-byte.
func (a *App) plainSnapshot(sc statusScope) string {
	var b []byte
	for _, r := range a.gatherRows(sc) {
		b = fmt.Appendf(b, "%s\t%s\t%s\t%s\n", r.name, r.backend, r.state, plainForwards(r))
	}
	return string(b)
}

// watchStatus prints snapshot() once, then again after every change under
// the machines dir (conf create/delete/edit) or the runtime dir (daemon sockets
// come and go; the change marker every state-changing command and daemon
// transition rewrites), blank-line separated so a consumer replaces its whole
// list on each block. Identical consecutive snapshots are dropped. wake, if
// not nil, is a third source of changes: the hub pipes nudge it on every
// block they receive and every time one of them dies.
//
// It sees only what devvm itself does. A `smolvm machine stop` run directly, a
// VM crash, or a remote host going away leaves nothing on disk here, so a UI
// should still re-run a plain status on demand (e.g. when its menu opens).
//
// Returns nil on ctx cancellation and on a write error, so a supervising
// process sees a clean exit when it is the one that closed the pipe. (Execute
// ignores SIGPIPE, so the write error is the only way a dead consumer shows.)
//
// If a watched directory is removed or renamed the kernel silently drops its
// watch (verified on both inotify and kqueue), so it is recreated and
// re-added, and any watcher error triggers a re-snapshot rather than being
// treated as fatal — a long-running consumer must never go blind.
func watchStatus(ctx context.Context, configDir string, snapshot func() string, out io.Writer, debounce time.Duration, wake <-chan struct{}) error {
	// Both dirs must exist to be watched; an empty registry is a valid thing to
	// watch (the first `create` is the change). Match Save's and
	// EnsureRuntimeDir's modes so nothing is loosened.
	if err := os.MkdirAll(config.MachinesDir(configDir), 0o755); err != nil {
		return err
	}
	if err := config.EnsureRuntimeDir(configDir); err != nil {
		return err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	dirs := []string{config.MachinesDir(configDir), config.RuntimeDir(configDir)}
	for _, dir := range dirs {
		if err := w.Add(dir); err != nil {
			return fmt.Errorf("watch %s: %w", dir, err)
		}
	}
	rewatch := func(dir string) {
		// Best-effort: the directory may be mid-rename; the next event or
		// error retries. Modes match the initial creation above.
		if dir == config.RuntimeDir(configDir) {
			_ = config.EnsureRuntimeDir(configDir)
		} else {
			_ = os.MkdirAll(dir, 0o755)
		}
		_ = w.Add(dir)
	}

	last := ""
	emit := func() error {
		s := snapshot()
		if s == last {
			return nil
		}
		last = s
		_, err := io.WriteString(out, s+"\n")
		return err
	}
	// The first block is unconditional so the consumer starts with a full list
	// (an empty registry yields a lone blank line, which is still "a block").
	last = snapshot()
	if _, err := io.WriteString(out, last+"\n"); err != nil {
		return nil
	}

	var pending <-chan time.Time // nil until an event arms the debounce
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			// The lock file and log churn on every daemon start/stop; the
			// socket and marker are what matter there, so skip the noise.
			if isWatchNoise(ev.Name) {
				continue
			}
			if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
				for _, dir := range dirs {
					if filepath.Clean(ev.Name) == filepath.Clean(dir) {
						rewatch(dir)
					}
				}
			}
			if pending == nil {
				pending = time.After(debounce)
			}
		case <-wake:
			if pending == nil {
				pending = time.After(debounce)
			}
		case _, ok := <-w.Errors:
			if !ok {
				return nil
			}
			// A watcher error (overflow, a watch lost) is not fatal: make sure
			// both dirs are still watched, re-snapshot so nothing is missed,
			// keep going.
			for _, dir := range dirs {
				if !slices.Contains(w.WatchList(), dir) {
					rewatch(dir)
				}
			}
			if pending == nil {
				pending = time.After(debounce)
			}
		case <-pending:
			pending = nil
			if err := emit(); err != nil {
				return nil
			}
		}
	}
}

// isWatchNoise filters runtime-dir entries that change without meaning a
// state change: the daemon startup lock and the per-daemon stderr log.
func isWatchNoise(name string) bool {
	return strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".log")
}
