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
	"sync/atomic"
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
	// A file that does not parse, at start or later, emits the broken
	// conf row and keeps watching until it parses again (watchStatus).
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
		return watchStatus(ctx, a.Paths, func() (string, error) { return a.plainSnapshot(statusScope{local: true}) }, a.Stderr, a.Stdout, watchDebounce, nil, nil)
	}
	return a.watchWithHubs(ctx, a.Stdout, watchDebounce)
}

// watchWithHubs is the merged --watch: the local loop plus a hubWatcher
// whose pipes wake it. The hub entries are re-read on every snapshot, and
// the loop takes one on every devvm.toml event, so the pipe set follows a
// hub entry created, deleted or re-pointed while watching. While the file
// does not parse the pipes are left as they are and the block is the
// broken conf row. The first block waits
// (bounded by the listing deadline) for the pipes to speak, so a consumer
// does not start from a flash of `unreachable`.
func (a *App) watchWithHubs(ctx context.Context, out io.Writer, debounce time.Duration) error {
	hw := newHubWatcher(a)
	defer hw.stop()
	snapshot := func() (string, error) {
		if _, err := config.LoadFile(a.Paths); err != nil {
			return "", brokenConfError{err} // keep the pipes: a half-saved edit must not drop every hub
		}
		hubs := a.hubConfs() // once per block: the pipe set and the rows use the same read
		hw.sync(hubs)
		return a.plainSnapshot(statusScope{hubs: hubs, listings: hw.listings()})
	}
	round := func(ctx context.Context) { a.liveRound(ctx) } // probes and provider listings
	hw.sync(a.hubConfs())
	// The opening round runs alongside the pipes' settle, inside the same
	// first-block wait (both are bounded by the listing deadline), so the
	// first block already carries probed remotes rather than a cache that
	// may be days old.
	probed := make(chan struct{})
	go func() { defer close(probed); round(ctx) }()
	hw.settle(ctx, hubTimes.deadline)
	<-probed
	return watchStatus(ctx, a.Paths, snapshot, a.Stderr, out, debounce, hw.wake, round)
}

// plainSnapshot renders the plain listing as one string so identical
// snapshots can be suppressed byte-for-byte.
func (a *App) plainSnapshot(sc statusScope) (string, error) {
	rows, err := a.gatherRows(sc)
	if err != nil {
		return "", err
	}
	var b []byte
	for _, r := range rows {
		b = append(b, plainLine(r)...)
	}
	return string(b), nil
}

// watchStatus prints snapshot() once, then again after every change in the
// config dir (devvm.toml written, renamed into place by `config add`/`rm`,
// or saved by an editor) or the runtime dir (daemon sockets
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
// refresh, if not nil, is the live round (liveRound) that a refresh
// request runs: a write of run/refresh (`devvm __refresh`) runs it once, in
// the background, and the block it changes is emitted when it ends. Every
// other event re-snapshots from the caches only, so a daemon touching run/
// never costs a probe. A request that lands while a round is in flight
// joins that round rather than starting another, and the debounce
// coalesces a burst of requests, so a double click costs one round.
//
// A snapshot that fails because devvm.toml does not parse (say mid-edit,
// or at start) emits brokenConfRow as the block, as a one-shot status
// does, and the watch goes on: the next event after the file parses again
// emits the listing. The error goes to errOut once per distinct message.
// Any other failing snapshot emits nothing (the consumer keeps the last
// block), except the first, which ends the watch with its error.
//
// Returns nil on ctx cancellation and on a write error, so a supervising
// process sees a clean exit when it is the one that closed the pipe. (Execute
// ignores SIGPIPE, so the write error is the only way a dead consumer shows.)
//
// If a watched directory is removed or renamed the kernel silently drops its
// watch (verified on both inotify and kqueue), so it is recreated and
// re-added, and any watcher error triggers a re-snapshot rather than being
// treated as fatal — a long-running consumer must never go blind.
func watchStatus(ctx context.Context, paths config.Paths, snapshot func() (string, error), errOut, out io.Writer, debounce time.Duration, wake <-chan struct{}, refresh func(context.Context)) error {
	// Both dirs (the config dir, run/ under state) must exist to be
	// watched; a missing devvm.toml is a valid thing to watch (the first
	// `config add` is the change). The directory, not the file: every
	// write of it is a rename into place, which a watch on the file itself
	// would lose. A devvm.toml that is a symlink into a dotfiles checkout
	// is written in its target's dir, so that dir is watched too, and
	// re-resolved on every config-dir event and again once its debounce
	// fires: a stow/chezmoi relink, or a devvm.toml that appears later as
	// a link, moves the watch with it. The second look is for kqueue
	// (macOS): it reports a new name in a watched dir but not a rename
	// over a name it has seen, so a temp-link-then-rename relink shows up
	// only as the temp link's Create, taken before the rename lands. An
	// in-place `rm` + `ln -s` of devvm.toml produces no event at all
	// there (fsnotify's kqueue watch on the entry holds the old target,
	// which survives), so every debounce re-resolves: the next event of
	// any kind (a run/ marker touch included) picks the new target up.
	// inotify reports both forms directly.
	// Config-dir events count only for devvm.toml (and its target), so an
	// editor's swap files never cost a snapshot. Match writeConfigFile's
	// and EnsureRuntimeDir's modes so nothing is loosened.
	if err := os.MkdirAll(paths.Config, 0o755); err != nil {
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
	cfgDir, runDir := filepath.Clean(paths.Config), filepath.Clean(paths.RuntimeDir())
	fileName := filepath.Clean(paths.FilePath())
	dirs := []string{cfgDir, runDir}
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

	// target is devvm.toml's symlink target ("" when it is no link);
	// targetDir its dir when that is a third directory, watched as well.
	// Both are fully resolved (EvalSymlinks), so they are compared with
	// the resolved config and run dirs: on macOS /tmp and /var are
	// themselves links into /private, and a plain devvm.toml under one
	// resolves to a path that is not fileName yet names the same file.
	// Events from the target-dir watch carry its resolved path, the
	// form target is in. retarget reports whether the target moved.
	realDir := func(d string) string {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			return filepath.Clean(r)
		}
		return d
	}
	cfgReal, runReal := realDir(cfgDir), realDir(runDir)
	var target, targetDir string
	retarget := func() bool {
		t := ""
		if r, err := filepath.EvalSymlinks(paths.FilePath()); err == nil {
			if r = filepath.Clean(r); r != fileName && r != filepath.Join(cfgReal, filepath.Base(fileName)) {
				t = r
			}
		}
		d := ""
		if t != "" {
			if d = filepath.Dir(t); d == cfgReal || d == runReal || d == cfgDir || d == runDir {
				d = ""
			}
		}
		if d != targetDir {
			// The new dir first, then the old: no window with neither.
			if d != "" {
				_ = w.Add(d)
			}
			if targetDir != "" {
				_ = w.Remove(targetDir)
			}
			targetDir = d
		}
		moved := t != target
		target = t
		return moved
	}
	retarget()
	relevant := func(name string) bool {
		name = filepath.Clean(name)
		dir := filepath.Dir(name)
		switch {
		case name == cfgDir || name == runDir || (targetDir != "" && name == targetDir):
			return true
		case dir == runDir:
			// The lock file and log churn on every daemon start/stop; the
			// socket and marker are what matter there.
			return !isWatchNoise(name)
		case dir == cfgDir:
			return name == fileName || name == target
		case targetDir != "" && dir == targetDir:
			return name == target
		}
		return false
	}

	last, lastErr := "", ""
	// take is one snapshot, a broken file rendered as its row.
	take := func() (string, error) {
		s, err := snapshot()
		if err == nil {
			lastErr = ""
			return s, nil
		}
		if err.Error() != lastErr && errOut != nil {
			if bc, ok := asBrokenConf(err); ok {
				fmt.Fprintf(errOut, "devvm: status --watch: listing 'broken conf' until devvm.toml parses again: %v\n", bc.err)
			} else {
				fmt.Fprintf(errOut, "devvm: status --watch: %v\n", err)
			}
		}
		lastErr = err.Error()
		if _, ok := asBrokenConf(err); ok {
			return brokenConfRow, nil
		}
		return "", err
	}
	emit := func() error {
		s, err := take()
		if err != nil {
			return nil
		}
		if s == last {
			return nil
		}
		last = s
		_, err = io.WriteString(out, s+"\n")
		return err
	}
	// The first block is unconditional so the consumer starts with a full list
	// (an empty registry yields a lone blank line, which is still "a block").
	first, err := take()
	if err != nil {
		return err
	}
	last = first
	if _, err := io.WriteString(out, last+"\n"); err != nil {
		return nil
	}

	var pending <-chan time.Time // nil until an event arms the debounce
	changed := false             // a relevant event is in the debounce window
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
			if d := filepath.Dir(filepath.Clean(ev.Name)); d == cfgDir || filepath.Clean(ev.Name) == cfgDir {
				// A relink (or a link appearing) moves the target watch:
				// now, and once more when the debounce fires (see top).
				if retarget() {
					changed = true
				}
				if pending == nil {
					pending = time.After(debounce)
				}
			}
			if !relevant(ev.Name) {
				continue
			}
			changed = true
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
			changed = true
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
			retarget()
			changed = true
			if pending == nil {
				pending = time.After(debounce)
			}
		case <-pending:
			pending = nil
			if retarget() { // cheap, and catches a relink kqueue did not report
				changed = true
			}
			if !changed {
				continue // only an unrelated config-dir file: no snapshot
			}
			changed = false
			if refreshWanted && roundDone == nil {
				done := make(chan struct{})
				roundDone = done
				go func() { defer close(done); refresh(ctx) }()
			} else if refreshWanted {
				if h := refreshJoinedHook.Load(); h != nil {
					(*h)()
				}
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

// refreshJoinedHook, when set (tests only), is called each time a refresh
// request joins the round in flight instead of starting one, so a test can
// wait for that rather than sleep past the debounce. Atomic: a watch
// goroutine may outlive its test.
var refreshJoinedHook atomic.Pointer[func()]

// isWatchNoise filters runtime-dir entries that change without meaning a
// state change: the daemon startup lock and the per-daemon stderr log.
func isWatchNoise(name string) bool {
	return strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".log")
}
