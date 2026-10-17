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
		// --local reads the reach cache and never probes, like the
		// one-shot --local: it is what a hub runs for its callers.
		return watchStatus(ctx, a.Paths, func() string { return a.plainSnapshot(statusScope{local: true}) }, a.Stdout, watchDebounce, nil, nil)
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
	round := func(ctx context.Context) { a.reachRound(ctx, a.remoteConfs()) }
	hw.sync(a.hubConfs())
	// The opening round runs alongside the pipes' settle, inside the same
	// first-block wait (both are bounded by the listing deadline), so the
	// first block already carries probed remotes rather than a cache that
	// may be days old.
	probed := make(chan struct{})
	go func() { defer close(probed); round(ctx) }()
	hw.settle(ctx, hubTimes.deadline)
	<-probed
	return watchStatus(ctx, a.Paths, snapshot, out, debounce, hw.wake, round)
}

// plainSnapshot renders the plain listing as one string so identical
// snapshots can be suppressed byte-for-byte.
func (a *App) plainSnapshot(sc statusScope) string {
	var b []byte
	for _, r := range a.gatherRows(sc) {
		b = append(b, plainLine(r)...)
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
// should still re-run a plain status on demand (e.g. when its menu opens),
// or ask this watcher for a refresh (below).
//
// refresh, if not nil, is the live round (reachRound) that a refresh
// request runs: a write of run/refresh (`devvm __refresh`) runs it once, in
// the background, and the block it changes is emitted when it ends. Every
// other event re-snapshots from the caches only, so a daemon touching run/
// never costs a probe. A request that lands while a round is in flight
// joins that round rather than starting another, and the debounce
// coalesces a burst of requests, so a double click costs one round.
//
// Returns nil on ctx cancellation and on a write error, so a supervising
// process sees a clean exit when it is the one that closed the pipe. (Execute
// ignores SIGPIPE, so the write error is the only way a dead consumer shows.)
//
// If a watched directory is removed or renamed the kernel silently drops its
// watch (verified on both inotify and kqueue), so it is recreated and
// re-added, and any watcher error triggers a re-snapshot rather than being
// treated as fatal — a long-running consumer must never go blind.
func watchStatus(ctx context.Context, paths config.Paths, snapshot func() string, out io.Writer, debounce time.Duration, wake <-chan struct{}, refresh func(context.Context)) error {
	// Both dirs (machines/ under config, run/ under state) must exist to be
	// watched; an empty registry is a valid thing to
	// watch (the first `create` is the change). Match Save's and
	// EnsureRuntimeDir's modes so nothing is loosened.
	if err := os.MkdirAll(config.MachinesDir(paths.Config), 0o755); err != nil {
		return err
	}
	if err := paths.EnsureRuntimeDir(); err != nil {
		return err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	dirs := []string{config.MachinesDir(paths.Config), paths.RuntimeDir()}
	for _, dir := range dirs {
		if err := w.Add(dir); err != nil {
			return fmt.Errorf("watch %s: %w", dir, err)
		}
	}
	rewatch := func(dir string) {
		// Best-effort: the directory may be mid-rename; the next event or
		// error retries. Modes match the initial creation above.
		if dir == paths.RuntimeDir() {
			_ = paths.EnsureRuntimeDir()
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
	refreshWanted := false       // a refresh request is in the debounce window
	var roundDone chan struct{}  // the round in flight; nil (blocks forever) when idle
	refreshPath := filepath.Clean(paths.RefreshPath())
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
			if refresh != nil && filepath.Clean(ev.Name) == refreshPath && !ev.Has(fsnotify.Remove) && !ev.Has(fsnotify.Rename) {
				refreshWanted = true
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
			if refreshWanted && roundDone == nil {
				done := make(chan struct{})
				roundDone = done
				go func() { defer close(done); refresh(ctx) }()
			}
			refreshWanted = false // started, or joined to the round in flight
			if err := emit(); err != nil {
				return nil
			}
		case <-roundDone:
			roundDone = nil
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
