package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// The merged listing (docs/proposals/hub.md §5, roadmap step 3). `status`
// asks every hub for its own `status --plain --local` rows, all hubs in
// parallel under one deadline, and renders each row as HUB/NAME with the
// state and backend the hub reported and the forwards column from *this*
// host's daemon for HUB@NAME — the hub's forwards column describes the hub's
// localhost, which says nothing about what this host can reach. The last
// successful answer is cached in cache/hub-HUB.list, outside machines/ and
// run/ (the two directories `status --watch` observes), so the cache write
// never wakes the watcher that made it; a hub that does not answer renders
// its cached rows with state `unreachable`, so the menu keeps showing the
// machines. `--watch` never re-dials per event: it holds one `status --plain
// --local --watch` pipe per hub (hubWatcher) and re-merges on every block.

// hubTiming bounds the listing. connect is ssh's ConnectTimeout in seconds:
// 2, not the 10s default, because the menu bar app re-runs a plain status
// every time its menu opens and an asleep desktop must not turn that into a
// ten-second hang. deadline is the whole budget of one listing, as the
// caller sees it: ConnectTimeout bounds only connect and key exchange, not
// authentication, the login shell or the remote devvm, so a hub that accepts
// the connection and then hangs is killed inside it and reads `unreachable`
// like one that never answered. The kill is not the end: the ControlMaster
// mux holds the killed client's stdout for up to backend.HostWaitDelay, so
// the context deadline is the budget minus that (listBudget), and `status`
// against a hung hub returns in deadline, not deadline plus a second. The
// backoffs re-spawn a dead --watch pipe, on the daemon's 2–30s schedule;
// eofGrace is how long a stopping watcher gives the hub side to exit on
// stdin EOF before killing its ssh. A var so tests run with short values.
type hubTiming struct {
	connect    int
	deadline   time.Duration
	backoffMin time.Duration
	backoffMax time.Duration
	eofGrace   time.Duration
}

var hubTimes = hubTiming{connect: 2, deadline: 5 * time.Second, backoffMin: 2 * time.Second, backoffMax: 30 * time.Second, eofGrace: time.Second}

// listBudget is the context deadline for one listing: the overall deadline
// less the pipe drain that follows a kill. Floored at 100ms so a deadline
// shorter than the drain (tests) still bounds the ssh itself; the
// production budget is 5s − 1s = 4s and never near the floor.
func listBudget() time.Duration {
	return max(hubTimes.deadline-backend.HostWaitDelay, 100*time.Millisecond)
}

// hubListArgv is the command line run on a hub for its listing: the proxy's
// `env DEVVM_NO_SUBSCRIBE=1 devvm …` under the user's login shell (only the
// login shell has devvm on PATH on the hubs). --local is what stops a hub
// that has hubs of its own from fanning out in turn, and two hosts registered
// as each other's hubs from recursing (hub.md §5). The held --watch pipe
// also asks the hub side to exit when its stdin reaches EOF (statusExitFlag):
// a watcher that dies here — the menu bar app restarting it, `devvm update`
// cycling it, a laptop shutting down — would otherwise leave its hub-side
// watcher alive until that one's next write EPIPEs, which on an idle hub is
// never, so every restart would leave one more orphan behind.
func hubListArgv(watch bool) []string {
	argv := []string{"status", "--plain", "--local"}
	if watch {
		argv = append(argv, "--watch", statusExitFlag)
	}
	return backend.LoginShellArgv(backend.ProxyArgv(argv...)...)
}

// hubListOpts is how a listing ssh runs: BatchMode (no prompt can hang it),
// the short connect timeout, and ssh's own chatter dropped — a refused key
// or an unknown host key is `unreachable` here, not a paragraph in the
// middle of `status`. stdin is the caller's: empty for the one-shot listing,
// a pipe held open for the life of a watch pipe (see hubWatcher.run).
func hubListOpts(stdin io.Reader, stdout io.Writer) backend.ExecOpts {
	return backend.ExecOpts{
		BatchMode: true, ConnectTimeout: hubTimes.connect,
		Stdin: stdin, Stdout: stdout, Stderr: io.Discard,
	}
}

// hubRow is one row of a hub's --plain listing: a bare hub-side name and the
// tokens as the hub emitted them, passed through and never interpreted, so a
// newer hub's new token reaches this host's consumer intact.
type hubRow struct{ name, backend, state, forwards string }

// parseHubRows reads --plain rows out of text. Anything that is not a row is
// dropped: a login-shell banner, a blank line, a row whose name is not a bare
// machine name (hubs never nest, so a HUB/ or HUB@ form is never valid here;
// ValidName rejects both).
func parseHubRows(text string) []hubRow {
	var rows []hubRow
	for _, line := range strings.Split(text, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 || config.ValidName(f[0]) != nil {
			continue
		}
		r := hubRow{name: f[0], backend: f[1], state: f[2], forwards: "-"}
		if len(f) > 3 {
			r.forwards = f[3]
		}
		rows = append(rows, r)
	}
	return rows
}

// hubListing is what one hub contributes to the merged listing: its rows,
// and whether the hub said so itself (fresh) or the cache is standing in for
// a hub that did not answer.
type hubListing struct {
	rows  []hubRow
	fresh bool
}

// hubListings is keyed by hub name.
type hubListings map[string]hubListing

// hubConfs loads every hub conf, sorted by name. A conf that fails to load is
// not a hub here; status lists it as broken conf.
func (a *App) hubConfs() []*config.Machine {
	names, _ := config.List(a.ConfigDir)
	var hubs []*config.Machine
	for _, n := range names {
		if m, err := config.Load(a.ConfigDir, n); err == nil && m.IsHub() {
			hubs = append(hubs, m)
		}
	}
	sort.Slice(hubs, func(i, j int) bool { return hubs[i].Name < hubs[j].Name })
	return hubs
}

// fetchHubListings asks every hub at once and returns a listing per hub:
// fresh rows (cached on the way) or, for a hub that failed, the cache marked
// stale. Parallel so N asleep hubs cost one deadline, not N.
func (a *App) fetchHubListings(ctx context.Context, hubs []*config.Machine) hubListings {
	out := make(hubListings, len(hubs))
	results := make([]hubListing, len(hubs))
	var wg sync.WaitGroup
	for i, hub := range hubs {
		wg.Add(1)
		go func(i int, hub *config.Machine) {
			defer wg.Done()
			results[i] = a.listHub(ctx, hub)
		}(i, hub)
	}
	wg.Wait()
	for i, hub := range hubs {
		out[hub.Name] = results[i]
	}
	return out
}

// listHub runs one hub's listing under the deadline and settles it: on
// success the rows are cached and returned fresh; on any failure — ssh's
// 255, a devvm there that does not know --local, the deadline killing a
// hung ssh — the cache is returned stale. The deadline lives here so every
// caller (the fan-out, the `status HUB/NAME` drill-in) is bounded.
func (a *App) listHub(ctx context.Context, hub *config.Machine) hubListing {
	ctx, cancel := context.WithTimeout(ctx, listBudget())
	defer cancel()
	rows, err := a.runHubList(ctx, hub)
	if err != nil {
		return hubListing{rows: readHubCache(a.ConfigDir, hub.Name)}
	}
	_ = writeHubCache(a.ConfigDir, hub.Name, rows) // derived data; a failed write is not a failed status
	return hubListing{rows: rows, fresh: true}
}

func (a *App) runHubList(ctx context.Context, hub *config.Machine) ([]hubRow, error) {
	b, err := backend.For(hub, a.ConfigDir)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := b.Run(ctx, hubListOpts(strings.NewReader(""), &buf), hubListArgv(false)...); err != nil {
		return nil, err
	}
	return parseHubRows(buf.String()), nil
}

// The cache. cache/hub-HUB.list holds the hub's --plain rows verbatim, one
// per line (`name<TAB>backend<TAB>state<TAB>forwards`, bare hub-side names in
// column 1): the grammar the hub already speaks, so cachedHubMachines reads
// column 1 with no second format to keep in step.

func hubCachePath(configDir, hub string) string {
	return filepath.Join(config.CacheDir(configDir), "hub-"+hub+".list")
}

func hubCacheText(rows []hubRow) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", r.name, r.backend, r.state, r.forwards)
	}
	return b.String()
}

// writeHubCache replaces a hub's cache atomically (temp + rename, as
// Machine.Save does): a `status` reading it mid-write sees the old listing
// or the new one, never half a row.
func writeHubCache(configDir, hub string, rows []hubRow) error {
	dir := config.CacheDir(configDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "hub-"+hub+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.WriteString(hubCacheText(rows))
	cerr := tmp.Close()
	if werr == nil && cerr == nil {
		if err := os.Rename(tmp.Name(), hubCachePath(configDir, hub)); err == nil {
			return nil
		} else {
			werr = err
		}
	} else if werr == nil {
		werr = cerr
	}
	os.Remove(tmp.Name())
	return werr
}

func readHubCache(configDir, hub string) []hubRow {
	data, err := os.ReadFile(hubCachePath(configDir, hub))
	if err != nil {
		return nil
	}
	return parseHubRows(string(data))
}

// removeHubCache is for `delete HUB`: the cache is derived from a conf that
// no longer exists. Best-effort; a missing file is the goal.
func removeHubCache(configDir, hub string) { _ = os.Remove(hubCachePath(configDir, hub)) }

// dropFromHubCache is for `delete HUB/NAME`: the next listing would drop the
// row anyway, but completion reads the cache in between and should not keep
// offering a machine the hub just destroyed.
func dropFromHubCache(configDir, hub, machine string) {
	rows := readHubCache(configDir, hub)
	kept := rows[:0]
	for _, r := range rows {
		if r.name != machine {
			kept = append(kept, r)
		}
	}
	if len(kept) != len(rows) {
		_ = writeHubCache(configDir, hub, kept)
	}
}

// hubWatcher is `status --plain --watch`'s view of the hubs: one long-lived
// `status --plain --local --watch` pipe per hub conf, the latest block each
// emitted, and a wake channel the watch loop selects on, so a hub block
// re-emits the merged listing exactly as a local change does. The hub side
// does the watching (its own fsnotify loop); this host only holds the pipe.
// sync runs on every snapshot, which the loop already takes on every
// machines/ event, so a hub conf created, deleted or re-pointed while
// watching opens, closes or restarts its pipe with no restart of --watch.
type hubWatcher struct {
	a    *App
	wake chan struct{}  // cap 1: a block or a pipe death nudges the loop
	wg   sync.WaitGroup // every pipe goroutine ever spawned, joined by stop

	mu    sync.Mutex
	pipes map[string]*hubPipe
	last  hubListings
}

// hubPipe is one hub's held listing.
type hubPipe struct {
	hub    *config.Machine // the conf it was spawned from; re-pointed means respawn
	cancel context.CancelFunc
	first  chan struct{} // closed once the pipe has spoken or died once (settle)
}

func newHubWatcher(a *App) *hubWatcher {
	return &hubWatcher{a: a, wake: make(chan struct{}, 1), pipes: map[string]*hubPipe{}, last: hubListings{}}
}

// sync makes the pipe set match the hub confs: a new hub gets a pipe, a
// deleted one loses it and its rows, one whose ssh fields changed is
// respawned against the new destination.
func (w *hubWatcher) sync(hubs []*config.Machine) {
	w.mu.Lock()
	defer w.mu.Unlock()
	want := map[string]bool{}
	for _, hub := range hubs {
		want[hub.Name] = true
		if p, ok := w.pipes[hub.Name]; ok {
			if sameHubHop(p.hub, hub) {
				continue
			}
			p.cancel()
			delete(w.last, hub.Name)
		}
		w.pipes[hub.Name] = w.spawn(hub)
	}
	for name, p := range w.pipes {
		if !want[name] {
			p.cancel()
			delete(w.pipes, name)
			delete(w.last, name)
		}
	}
}

func sameHubHop(a, b *config.Machine) bool {
	return a.SSHHost == b.SSHHost && a.SSHPort == b.SSHPort && a.Identity == b.Identity
}

func (w *hubWatcher) spawn(hub *config.Machine) *hubPipe {
	ctx, cancel := context.WithCancel(context.Background())
	p := &hubPipe{hub: hub, cancel: cancel, first: make(chan struct{})}
	w.wg.Add(1)
	go w.run(ctx, p)
	return p
}

// run holds one hub's pipe until the watcher stops, re-spawning it with
// backoff on EOF: the hub slept, sshd cycled, its devvm was updated, someone
// pkill'd its watcher. Between pipes the hub's cached rows read
// `unreachable`. A block resets the backoff (the hub is back and talking);
// a pipe that never speaks — an older devvm refusing --local, a host that
// refuses the key — backs off to the max.
//
// The ssh's stdin is an OS pipe this side holds open for the attempt's life
// and never writes to. It exists for its EOF: the hub side runs with
// statusExitFlag, so when this process dies (the kernel closes the write
// end with it) or the watcher stops (closed below, before the kill, with
// eofGrace for the hub side to see it) ssh forwards the EOF and the hub's
// watcher exits instead of lingering until its next write EPIPEs. An
// *os.File, not an io.Pipe: exec hands a file to the child as is, while any
// other reader gets a copy goroutine that Wait then waits on — past
// WaitDelay too — so a reader that never reaches EOF would hang Run.
func (w *hubWatcher) run(ctx context.Context, p *hubPipe) {
	defer w.wg.Done()
	var settled sync.Once
	settle := func() { settled.Do(func() { close(p.first) }) }
	backoff := hubTimes.backoffMin
	lastCache := "" // what the cache holds, so an unchanged block costs no write
	for {
		var spoke atomic.Bool
		if b, err := backend.For(p.hub, w.a.ConfigDir); err == nil {
			// One context per attempt, cancelled to kill this ssh: by an
			// overflowing pipe (blockSplitter), or by the watcher stopping,
			// after stdin EOF has had its grace. Detached from ctx on purpose
			// so the stop path is EOF first, kill second.
			actx, acancel := context.WithCancel(context.Background())
			stdinR, stdinW, perr := os.Pipe()
			if perr != nil {
				stdinR, stdinW = nil, nil // no pipe: EOF at once, as the one-shot listing; the hub side exits and is respawned
			}
			ran, graced := make(chan struct{}), make(chan struct{})
			grace := hubTimes.eofGrace // read here, on run's goroutine, not in the helper below
			go func() {
				defer close(graced)
				select {
				case <-ctx.Done():
					if stdinW != nil {
						stdinW.Close()
					}
					select {
					case <-ran:
					case <-time.After(grace):
					}
					acancel()
				case <-ran:
				}
			}()
			split := &blockSplitter{onOverflow: acancel, onBlock: func(text string) {
				// onBlock runs on exec's copy goroutine and can still fire
				// after stop() returned (WaitDelay closes the pipe under a
				// lingering holder): nothing may be recorded or written then.
				if ctx.Err() != nil {
					return
				}
				rows := parseHubRows(text)
				if text := hubCacheText(rows); text != lastCache {
					if writeHubCache(w.a.ConfigDir, p.hub.Name, rows) == nil {
						lastCache = text
					}
				}
				w.set(p, hubListing{rows: rows, fresh: true})
				spoke.Store(true)
				settle()
			}}
			var stdin io.Reader = strings.NewReader("")
			if stdinR != nil {
				stdin = stdinR
			}
			_ = b.Run(actx, hubListOpts(stdin, split), hubListArgv(true)...)
			close(ran)
			<-graced // joined here so stop()'s WaitGroup covers it too
			if stdinR != nil {
				stdinW.Close()
				stdinR.Close()
			}
			acancel()
		}
		w.set(p, hubListing{rows: readHubCache(w.a.ConfigDir, p.hub.Name)})
		settle()
		if spoke.Load() {
			backoff = hubTimes.backoffMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, hubTimes.backoffMax)
	}
}

// set records a pipe's latest listing and wakes the loop — unless sync has
// replaced or dropped the pipe since, in which case a late block or the
// death notice of the old pipe must not overwrite the new one's rows.
func (w *hubWatcher) set(p *hubPipe, l hubListing) {
	w.mu.Lock()
	if w.pipes[p.hub.Name] == p {
		w.last[p.hub.Name] = l
	}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// listings is a copy of the latest block per hub. A hub with a pipe that
// has not spoken yet is absent, which gatherRows renders as unreachable.
func (w *hubWatcher) listings() hubListings {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(hubListings, len(w.last))
	for k, v := range w.last {
		out[k] = v
	}
	return out
}

// settle waits until every pipe has produced a block or died once, or d
// elapses, so the first emitted listing shows the hubs' real state rather
// than a flash of `unreachable` while the pipes come up.
func (w *hubWatcher) settle(ctx context.Context, d time.Duration) {
	w.mu.Lock()
	var firsts []chan struct{}
	for _, p := range w.pipes {
		firsts = append(firsts, p.first)
	}
	w.mu.Unlock()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for _, f := range firsts {
		select {
		case <-f:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// stop closes every pipe and waits for every pipe goroutine — the live ones
// and any sync already cancelled — to go (bounded: a cancelled Run returns
// once ssh is killed and its pipes closed).
func (w *hubWatcher) stop() {
	w.mu.Lock()
	pipes := w.pipes
	w.pipes = map[string]*hubPipe{}
	w.mu.Unlock()
	for _, p := range pipes {
		p.cancel()
	}
	w.wg.Wait()
}

// hubBlockMax bounds what a hub may send without a block separator. A
// --plain block is a few hundred bytes; a stream that reaches this with no
// "\n\n" is not a listing (a shell stuck echoing, a wrong command) and
// would otherwise be buffered forever on a pipe held forever.
const hubBlockMax = 1 << 20

// blockSplitter hands each blank-line-terminated block of a --watch stream
// to onBlock as it completes. An empty listing is a lone "\n" block (see
// watchStatus), and it is a block here too — a hub with no machines left —
// but only once the stream has produced a real block: before that, leading
// newlines are a login-shell banner (an `echo` in .bash_profile) flushed
// ahead of devvm's output, and taking them for a block would flash the hub
// as reachable with no machines. The cost is that a hub whose registry is
// empty from the start never speaks on the held pipe and reads
// `unreachable` there until its first machine; the one-shot listing (the
// menu-open refresh) still shows it reachable.
type blockSplitter struct {
	buf        []byte
	spoke      bool // a real block has been delivered
	onBlock    func(text string)
	onOverflow func() // called once when hubBlockMax is exceeded, before the error
}

func (s *blockSplitter) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	if !s.spoke {
		s.buf = bytes.TrimLeft(s.buf, "\n")
	}
	for {
		i := bytes.Index(s.buf, []byte("\n\n"))
		if i < 0 {
			if len(s.buf) > hubBlockMax {
				s.buf = nil
				if s.onOverflow != nil {
					s.onOverflow()
				}
				return 0, fmt.Errorf("hub listing: %d bytes with no block separator", hubBlockMax)
			}
			if s.spoke && bytes.Equal(s.buf, []byte("\n")) {
				s.buf = s.buf[:0]
				s.onBlock("")
			}
			return len(p), nil
		}
		block := string(s.buf[:i+1])
		s.buf = append(s.buf[:0], s.buf[i+2:]...)
		s.spoke = true
		s.onBlock(block)
	}
}
