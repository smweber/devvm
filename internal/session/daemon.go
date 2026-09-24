package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// idleTimeout is how long the daemon lingers with zero forwards and zero
// sessions before exiting (ControlPersist-style), so a `ports down` or last
// `ports rm` reaps it.
const idleTimeout = 60 * time.Second

// exactRetryInterval is the ticker that re-binds pending exact forwards
// while the transport is up (browser-bridge.md §4). An exact forward is
// never bumped, so one whose port was taken during an outage would
// otherwise stay pending until the next add or reconnect even after the
// port frees. The same ticker expires `ttl` owners once the bridge creates
// them (roadmap step 7).
const exactRetryInterval = 5 * time.Second

// Reconnect backoff bounds. A laptop waking from sleep usually has its link
// back within a few seconds; a box that is really gone should not be hammered.
const (
	minReconnectBackoff = 2 * time.Second
	maxReconnectBackoff = 30 * time.Second
)

// errVMNotRunning is returned by a smol dial when the VM is stopped: the
// daemon must not exec into it (smolvm's exec may boot a stopped machine, and
// the user stopped it on purpose), so reconnect waits without dialing.
var errVMNotRunning = errors.New("VM is not running")

// errStopped is returned by an interrupted dial when the daemon was told to
// stop mid-attempt.
var errStopped = errors.New("daemon stopped")

// owner is one holder of a forward (browser-bridge.md §4). conn names the
// session connection for OwnerConnection and is zero for the other kinds:
// there is one conf owner (the conf) and one ttl owner (refreshed, not
// stacked, by a repeat callback) per forward.
type owner struct {
	kind string
	conn uint64
}

var (
	confOwner = owner{kind: OwnerConf}
	ttlOwner  = owner{kind: OwnerTTL}
)

func connOwner(id uint64) owner { return owner{kind: OwnerConnection, conn: id} }

// fwd is one forward the daemon is responsible for. closer is nil while the
// transport is down: the forward is remembered and re-bound on reconnect.
type fwd struct {
	host, guest int
	// exact: the host port may never bump, not on add and not on restore.
	// Sticky: once any request asked for exact it stays for the forward's
	// life, even after that owner is gone (bridge §4: recomputing it per
	// owner adds policy nobody sees; `ports down`/`up` clears it).
	exact  bool
	closer io.Closer
	// binding marks a slot claimed by an in-flight bind (an add, restore, or
	// the retry ticker); exactly one binder holds it at a time. A second add
	// for the same guest must not bind again: on ssh both closers would carry
	// the same -L spec and the loser's `ssh -O cancel` would kill the winner.
	// Nor may it answer before the bind has a result: it would report a port
	// the forward may never get (a failed or bumped bind). So it waits on
	// ready, closed when the binder finishes either way, and re-evaluates.
	binding bool
	ready   chan struct{}
	owners  map[owner]struct{} // torn down when empty
}

// startBinding claims the slot for one binder; d.mu must be held.
func (f *fwd) startBinding() {
	f.binding = true
	f.ready = make(chan struct{})
}

// finishBinding releases the slot and wakes every add waiting on it; d.mu
// must be held.
func (f *fwd) finishBinding() {
	if f.binding {
		f.binding = false
		close(f.ready)
	}
}

// settleLocked ends a binder's claim on f and deletes the slot if every
// owner went while it was binding (remove/dropOwners keep such a slot, see
// releaseLocked). d.mu must be held.
func (d *daemon) settleLocked(f *fwd) {
	f.finishBinding()
	if d.forwards[f.guest] == f && len(f.owners) == 0 {
		delete(d.forwards, f.guest)
	}
}

// releaseLocked deletes a forward whose last owner just went and returns
// its closer for the caller to close after unlocking. A slot that is being
// bound stays in the map, ownerless, until its binder settles it: deleting
// it would let a new add for the same guest bind at once, and on ssh the
// first binder's losing adopt would then `-O cancel` the identical -L spec,
// killing the new forward. A waiter on ready re-evaluates once it settles.
// d.mu must be held.
func (d *daemon) releaseLocked(f *fwd) io.Closer {
	if len(f.owners) > 0 || d.forwards[f.guest] != f || f.binding {
		return nil
	}
	delete(d.forwards, f.guest)
	return f.closer
}

func newFwd(host, guest int, exact bool, own owner) *fwd {
	return &fwd{host: host, guest: guest, exact: exact, owners: map[owner]struct{}{own: {}}}
}

// addOwner records own, reporting whether it is new.
func (f *fwd) addOwner(own owner) bool {
	if _, ok := f.owners[own]; ok {
		return false
	}
	f.owners[own] = struct{}{}
	return true
}

// ownerKinds is the forward's owners for the wire: kinds, sorted, deduped.
func (f *fwd) ownerKinds() []string {
	seen := map[string]bool{}
	var out []string
	for o := range f.owners {
		if !seen[o.kind] {
			seen[o.kind] = true
			out = append(out, o.kind)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fwd) wire() Forward {
	return Forward{Host: f.host, Guest: f.guest, Pending: f.closer == nil, Exact: f.exact, Owners: f.ownerKinds()}
}

type daemon struct {
	configDir string
	name      string
	version   string                    // build that spawned this daemon (cli.Version)
	dial      func() (transport, error) // re-dials the transport after it dies
	ln        net.Listener
	sockInfo  os.FileInfo // the socket we created; shutdown unlinks only that inode
	logf      func(format string, args ...any)

	// late counts transports that arrived after a stop interrupted their
	// dial; shutdown waits (bounded by lateWait) for them to be closed so the
	// process never exits with an orphaned smol exec or ssh master.
	late     sync.WaitGroup
	lateWait time.Duration

	// Backoff/idle/retry knobs live on the struct so tests can shrink them.
	minBackoff, maxBackoff, idle, retryEvery time.Duration

	mu       sync.Mutex
	tr       transport
	forwards map[int]*fwd // keyed by guest port
	state    string       // StateUp | StateReconnecting
	since    time.Time    // when state last changed

	// Sessions (browser-bridge.md §3). Every open session holds the daemon;
	// subs is the subscriber order, most recent first: events go to subs[0]
	// and fall through to the next when it unsubscribes or disconnects.
	sessions  map[uint64]*sess
	subs      []*sess
	nextConn  uint64
	nextEvent int64

	stopOnce sync.Once
	stop     chan struct{}
	kick     chan struct{} // OpKick: retry now instead of waiting out the backoff
}

// newDaemon wires a daemon around an already-connected transport; dial is how
// it gets a fresh one when that transport dies.
func newDaemon(configDir, name, version string, tr transport, dial func() (transport, error)) *daemon {
	logger := log.New(os.Stderr, "devvm-daemon: ", log.LstdFlags)
	return &daemon{
		configDir:  configDir,
		name:       name,
		version:    version,
		tr:         tr,
		dial:       dial,
		logf:       logger.Printf,
		minBackoff: minReconnectBackoff,
		maxBackoff: maxReconnectBackoff,
		idle:       idleTimeout,
		retryEvery: exactRetryInterval,
		lateWait:   lateWaitFor(),
		forwards:   map[int]*fwd{},
		sessions:   map[uint64]*sess{},
		state:      StateUp,
		since:      time.Now(),
		stop:       make(chan struct{}),
		kick:       make(chan struct{}, 1),
	}
}

// RunDaemon is the per-machine daemon entrypoint (the hidden `__daemon`
// command). It owns the transport for the machine's lifetime and serves control
// requests until idle or stopped, reconnecting when the transport dies. version
// is the build running it, reported on ping so a client can spot a stale daemon.
func RunDaemon(ctx context.Context, configDir string, m *config.Machine, b backend.Backend, version string) error {
	sock := socketPath(configDir, m.Name)
	if err := config.EnsureRuntimeDir(configDir); err != nil {
		return err
	}
	// Serialize startup: two racing daemons could otherwise both pass the dial
	// check, and the loser's stale-socket Remove would unlink the winner's live
	// socket (and, on smol, both would briefly hold parallel agent execs).
	lock, err := acquireLock(lockPath(configDir, m.Name))
	if err != nil {
		return err
	}
	defer lock.Close() // Close releases the flock

	// If another daemon already owns the socket, defer to it.
	if c, err := net.Dial("unix", sock); err == nil {
		c.Close()
		return nil
	}
	_ = os.Remove(sock) // clear a stale socket (nobody answered it, and we hold the lock)

	// A stopped smol VM is never dialed — not on reconnect and not on the
	// first dial either: exec'ing into it could boot a machine the user
	// deliberately stopped, and it can't hold forwards anyway. `devvm start`
	// brings the forwards back via tunnelUp.
	vmRunning := func() bool {
		if m.Backend != config.BackendSmol {
			return true
		}
		st, err := b.Status()
		return err != nil || st.Running
	}
	if !vmRunning() {
		return fmt.Errorf("%w; start it first", errVMNotRunning)
	}
	tr, err := newTransport(ctx, m, b)
	if err != nil {
		return err
	}
	ln, info, err := listenControl(sock)
	if err != nil {
		tr.Close()
		return err
	}
	// The socket is live and dialable; let any queued starter in to see that.
	lock.Close()

	dial := func() (transport, error) {
		if !vmRunning() {
			return nil, errVMNotRunning
		}
		return newTransport(ctx, m, b)
	}
	d := newDaemon(configDir, m.Name, version, tr, dial)
	d.ln, d.sockInfo = ln, info
	go d.serveControl()
	d.loop()
	return d.shutdown()
}

// listenControl opens the control socket. Go unlinks a unix socket when its
// listener closes; that is turned off so shutdown() controls when the path
// disappears (last, after the transport is released — see shutdown). The
// socket's file info is returned so shutdown can tell its own socket from a
// replacement daemon's.
// ownsSocket reports whether the path still holds the socket this daemon
// created, so shutdown never unlinks a successor's. Inode identity alone is
// not enough: ext4 and APFS recycle inode numbers, so a successor that
// unlinked our stale socket and listened on the same path can get the very
// same inode (tmpfs never does, which is how this hid locally). Our own
// listener is closed by now, so anything that answers a dial is a successor.
func (d *daemon) ownsSocket(sock string) bool {
	info, err := os.Lstat(sock)
	if err != nil {
		return false
	}
	if d.sockInfo != nil && !os.SameFile(info, d.sockInfo) {
		return false
	}
	if c, err := net.DialTimeout("unix", sock, 500*time.Millisecond); err == nil {
		c.Close()
		return false
	}
	return true
}

func listenControl(sock string) (*net.UnixListener, os.FileInfo, error) {
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return nil, nil, err
	}
	ln.SetUnlinkOnClose(false)
	info, err := os.Lstat(sock)
	if err != nil {
		ln.Close()
		return nil, nil, err
	}
	return ln, info, nil
}

// loop supervises the daemon: exit on stop or idle; on transport death, drop
// the dead channel and reconnect rather than exit. Before this, a laptop sleep
// long enough to exhaust ssh keepalives killed the daemon and every forward
// with it, and the user had to run `ports up` after each wake.
//
// The idle rule is `forwards == 0 && sessions == 0` (bridge §3): a session
// with no forwards still holds the daemon, or a held `attach` would lose its
// bridge a minute in and a hub's `__session` would see its daemon respawn
// every idle period.
func (d *daemon) loop() {
	idle := time.NewTimer(d.idle)
	defer idle.Stop()
	retry := time.NewTicker(d.retryEvery)
	defer retry.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-d.transport().dead():
			d.onDead()
			if !d.reconnect() {
				return
			}
			// Restart the idle clock: the reconnect loop ran its own.
			idle.Reset(d.idle)
		case <-retry.C:
			d.retryPending()
		case <-idle.C:
			if d.idleNow() {
				return
			}
			idle.Reset(d.idle)
		}
	}
}

func (d *daemon) transport() transport {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tr
}

// onDead marks the daemon reconnecting and tears down the dead transport and
// its forwards. The forwards stay in the map, closer-less, so reconnect()
// knows what to restore and clients can still see them as pending.
func (d *daemon) onDead() {
	d.mu.Lock()
	n := len(d.forwards)
	d.setStateLocked(StateReconnecting)
	d.mu.Unlock()
	config.TouchChanged(d.configDir)
	d.logf("%s: transport died; reconnecting (%d forwards to restore)", d.name, n)
	d.teardown()
}

// teardown closes every bound forward and the current transport. It runs
// outside d.mu: on ssh each close is an `ssh -O` subprocess, and against a
// wedged master (socket present, nobody home — the classic post-sleep state)
// they only return on their timeout. Holding the lock through that would
// freeze every ping/list/add for as long.
func (d *daemon) teardown() {
	d.mu.Lock()
	var closers []io.Closer
	for _, f := range d.forwards {
		if f.closer != nil {
			closers = append(closers, f.closer)
			f.closer = nil
		}
	}
	tr := d.tr
	d.tr = nil // a bind in flight on the old transport sees this and stays pending
	d.mu.Unlock()
	for _, c := range closers {
		c.Close()
	}
	if tr != nil {
		_ = tr.Close()
	}
}

// setStateLocked records a state change; d.mu must be held. Callers touch the
// change marker after unlocking — it's a file write, and nothing that holds
// the daemon lock should wait on the filesystem.
func (d *daemon) setStateLocked(state string) (changed bool) {
	if d.state == state {
		return false
	}
	d.state, d.since = state, time.Now()
	return true
}

// reconnect re-dials with exponential backoff until it succeeds (true), is
// stopped (false), or has sat with nothing to restore for the idle period
// (false) — the same idle rule an up daemon follows. A kick (from `ports up`
// or `start`) retries immediately rather than waiting out the backoff.
func (d *daemon) reconnect() bool {
	backoff := d.minBackoff
	wait := time.NewTimer(backoff)
	defer wait.Stop()
	idle := time.NewTimer(d.idle)
	defer idle.Stop()
	loggedNotRunning := false
	for attempt := 1; ; attempt++ {
		select {
		case <-d.stop:
			return false
		case <-idle.C:
			if d.idleNow() {
				d.logf("%s: no forwards to restore and no sessions; exiting", d.name)
				return false
			}
			idle.Reset(d.idle)
		case <-d.kick:
			backoff = d.minBackoff
			if !wait.Stop() {
				select {
				case <-wait.C:
				default:
				}
			}
			wait.Reset(0)
		case <-wait.C:
			tr, err := d.dialInterruptible()
			switch {
			case errors.Is(err, errStopped):
				return false
			case errors.Is(err, errVMNotRunning):
				if !loggedNotRunning {
					d.logf("%s: VM not running; waiting", d.name)
					loggedNotRunning = true
				}
				backoff = min(backoff*2, d.maxBackoff)
				wait.Reset(backoff)
				continue
			case err != nil:
				backoff = min(backoff*2, d.maxBackoff)
				d.logf("%s: reconnect attempt %d failed: %v (retrying in %s)", d.name, attempt, err, backoff)
				wait.Reset(backoff)
				continue
			}
			loggedNotRunning = false
			if !d.restore(tr) {
				// The transport came up but couldn't carry the forwards (a
				// degraded master, a flapping link): treat it as a failed
				// attempt and try again, forwards still pending.
				d.teardown()
				backoff = min(backoff*2, d.maxBackoff)
				d.logf("%s: reconnect attempt %d could not restore forwards (retrying in %s)", d.name, attempt, backoff)
				wait.Reset(backoff)
				continue
			}
			return true
		}
	}
}

// dialInterruptible runs dial but returns errStopped as soon as the daemon is
// told to stop, instead of waiting out a dial that may take the full ssh
// connect timeout. A transport that arrives after the stop is closed.
func (d *daemon) dialInterruptible() (transport, error) {
	type result struct {
		tr  transport
		err error
	}
	ch := make(chan result, 1)
	go func() {
		tr, err := d.dial()
		ch <- result{tr, err}
	}()
	select {
	case r := <-ch:
		return r.tr, r.err
	case <-d.stop:
		d.late.Add(1)
		go func() {
			defer d.late.Done()
			if r := <-ch; r.tr != nil {
				r.tr.Close()
			}
		}()
		return nil, errStopped
	}
}

// restore adopts a fresh transport and re-binds every remembered forward at
// its previous host port, so browser tabs and tool configs pointing at
// localhost:PORT keep working across the outage. A port taken meanwhile bumps
// exactly like a first-time add, unless the forward is exact: that one is
// never bumped (the browser still targets the original port) and stays
// pending until the retry ticker finds the port free. Returns false if a
// forward failed for any reason other than port contention: that forward
// stays pending and the caller retries the whole attempt — a forward is
// never dropped because one `ssh -O forward` hiccupped right after wake. Port
// exhaustion is logged and the forward left pending without failing the rest.
//
// Binds run outside d.mu (each is an `ssh -O forward`, bounded only by its
// timeout against a degraded master), so ping/list/add stay responsive; the
// loop picks up forwards added while a batch was binding. Forwards of every
// owner kind come back: sessions survive a transport death (only relay
// sessions, roadmap step 6, will not).
func (d *daemon) restore(tr transport) bool {
	d.mu.Lock()
	d.tr = tr
	d.mu.Unlock()
	restored := 0
	tried := map[*fwd]bool{}
	type job struct {
		f           *fwd
		host, guest int
		exact       bool
	}
	for {
		d.mu.Lock()
		var batch []job
		for _, f := range d.forwards {
			// A slot some add is still binding (on the dead transport) is
			// left to it: binding it here too would race its listener for
			// the same port (on smol the old listener can briefly hold P and
			// this bind would bump to P+1). That add ends pending, and the
			// retry ticker binds it once the daemon is up.
			if f.closer == nil && !f.binding && !tried[f] {
				f.startBinding()
				batch = append(batch, job{f, f.host, f.guest, f.exact})
			}
		}
		d.mu.Unlock()
		if len(batch) == 0 {
			break
		}
		sort.Slice(batch, func(i, j int) bool { return batch[i].guest < batch[j].guest })
		for i, j := range batch {
			tried[j.f] = true
			host, closer, bumped, err := bind(tr, j.host, j.guest, j.exact)
			if err != nil {
				d.mu.Lock()
				d.settleLocked(j.f)
				d.mu.Unlock()
				d.logf("%s: forward for guest %d still pending: %v", d.name, j.guest, err)
				if !errors.Is(err, errPortExhausted) && !errors.Is(err, errPortBusy) {
					d.mu.Lock()
					for _, rest := range batch[i+1:] {
						d.settleLocked(rest.f)
					}
					d.mu.Unlock()
					return false
				}
				continue
			}
			if bumped {
				d.logf("%s: host port %d taken; guest %d now on localhost:%d", d.name, j.host, j.guest, host)
			}
			if d.adopt(tr, j.f, host, closer, j.exact) {
				restored++
			}
		}
	}
	d.mu.Lock()
	d.setStateLocked(StateUp)
	d.mu.Unlock()
	config.TouchChanged(d.configDir)
	d.logf("%s: reconnected, %d forwards restored", d.name, restored)
	return true
}

// retryPending re-binds every forward left pending while the transport is
// up: an exact one whose port was taken when restore ran (never bumped; a
// port still taken waits for the next tick), a bumpable one that hit port
// exhaustion, or one whose add was cut off by a transport death and was
// skipped by restore because it was mid-bind. A bumpable forward tries its
// own port first and bumps from there. Binds run outside d.mu.
func (d *daemon) retryPending() {
	d.mu.Lock()
	tr := d.tr
	if d.state != StateUp || tr == nil {
		d.mu.Unlock()
		return
	}
	type job struct {
		f           *fwd
		host, guest int
		exact       bool
	}
	var batch []job
	for _, f := range d.forwards {
		if f.closer == nil && !f.binding {
			f.startBinding()
			batch = append(batch, job{f, f.host, f.guest, f.exact})
		}
	}
	d.mu.Unlock()
	for _, j := range batch {
		host, closer, _, err := bind(tr, j.host, j.guest, j.exact)
		if err != nil {
			d.mu.Lock()
			d.settleLocked(j.f)
			d.mu.Unlock()
			continue
		}
		if d.adopt(tr, j.f, host, closer, j.exact) {
			d.logf("%s: pending forward bound; guest %d on localhost:%d", d.name, j.guest, host)
			config.TouchChanged(d.configDir)
		}
	}
}

// adopt records a forward bound outside the lock. It is dropped (closed) if
// the forward was removed meanwhile (or replaced by a new record for the
// same guest), if another bind already won, or if the transport it was
// bound on is no longer the current one.
// It releases the binder's claim on the slot either way, and records exact
// when the bind that honoured it succeeded.
func (d *daemon) adopt(tr transport, f *fwd, host int, closer io.Closer, exact bool) bool {
	d.mu.Lock()
	d.settleLocked(f)
	if d.forwards[f.guest] != f || f.closer != nil || d.tr != tr {
		d.mu.Unlock()
		closer.Close()
		return false
	}
	f.host, f.closer = host, closer
	// exact sticks only once a bind honoured it: a refused exact request
	// must not leave a bumpable forward exact for good.
	f.exact = f.exact || exact
	d.mu.Unlock()
	return true
}

func (d *daemon) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.heldLocked()
}

// heldLocked counts forwards some owner holds: an ownerless slot is only
// waiting for its binder to settle it. d.mu must be held.
func (d *daemon) heldLocked() int {
	n := 0
	for _, f := range d.forwards {
		if len(f.owners) > 0 {
			n++
		}
	}
	return n
}

// idleNow is the idle rule: nothing to forward and nobody holding the daemon.
func (d *daemon) idleNow() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.heldLocked() == 0 && len(d.sessions) == 0
}

func (d *daemon) triggerStop() {
	d.stopOnce.Do(func() { close(d.stop) })
}

// shutdown tears everything down and unlinks the control socket LAST: the
// socket vanishing is what tells a caller cycling the daemon (update) that
// it is safe to spawn a replacement. Unlinking first would let the new
// daemon come up while this one still holds the ssh master (its `-M` would
// then silently degrade to a plain connection and every forward would fail)
// or, on smol, the agent exec (two parallel execs — the one-exec rule).
//
// Between ln.Close and the unlink, `ports up` can already have spawned a
// replacement that cleared our (unanswered) socket and listened on its own,
// so only the inode we created is removed — never a successor's.
func (d *daemon) shutdown() error {
	d.ln.Close()
	d.teardown()
	d.mu.Lock()
	d.forwards = map[int]*fwd{}
	d.mu.Unlock()
	lateDone := make(chan struct{})
	go func() { d.late.Wait(); close(lateDone) }()
	select {
	case <-lateDone:
	case <-time.After(d.lateWait):
		d.logf("%s: a dial interrupted by stop is still closing; not waiting further", d.name)
	}
	// Sessions are closed after the transport is released: a session client
	// reconnects at once, and the daemon it spawns must not find the old
	// master or exec still up.
	d.closeAllSessions()
	sock := socketPath(d.configDir, d.name)
	if d.ownsSocket(sock) {
		os.Remove(sock)
	}
	// The socket vanishing is itself a watch event; the marker covers the
	// no-forwards-on-exit case where a consumer would otherwise infer nothing.
	config.TouchChanged(d.configDir)
	return nil
}

// add allocates a host port (bumping on conflict, up to +20, unless exact)
// and starts the forward, owned by own, mirroring smol_forward_up /
// ssh_forwards. While reconnecting it only records the request (pending=true)
// so a `ports up` issued during an outage comes up as soon as the transport
// is back, instead of erroring. A forward left pending while up (port
// exhaustion during restore, or an exact port taken) is re-bound here rather
// than reported pending forever.
//
// Reuse (bridge §4): a request for a guest port already forwarded adds its
// owner to the existing forward and returns its host port, unless the
// request is exact and that host port differs, which is refused like any
// busy exact bind. An exact request makes the forward exact from then on,
// once a bind has honoured it.
//
// A slot another bind is working on is never answered from: the answer
// would be a port that bind may not get (it can fail, or bump). The add
// waits for that bind to finish, outside d.mu, and starts over: the slot is
// then bound (reuse, checked against the real port), pending, or gone (this
// add binds it itself).
func (d *daemon) add(pref, guest int, exact bool, own owner) (host int, bumped, pending bool, err error) {
	for {
		d.mu.Lock()
		f, ok := d.forwards[guest]
		if ok && f.binding {
			ready := f.ready
			d.mu.Unlock()
			<-ready
			continue
		}
		if ok && exact && f.host != pref {
			d.mu.Unlock()
			return 0, false, false, fmt.Errorf("host port %d unavailable: guest %d is already forwarded on localhost:%d: %w", pref, guest, f.host, errPortBusy)
		}
		tr := d.tr
		// Live (reuse), or no transport to bind on: reconnecting, or shutdown
		// already tore it down while this request was in flight. Record the
		// owner either way; a pending forward comes up on restore at the
		// port it has, and an exact request for it holds (host == pref).
		if (ok && f.closer != nil) || d.state == StateReconnecting || tr == nil {
			if !ok {
				f = newFwd(pref, guest, exact, own)
				d.forwards[guest] = f
			} else {
				f.addOwner(own)
				if exact {
					f.exact = true // the port is pref already: honoured, not requested
				}
			}
			host, pending := f.host, f.closer == nil
			d.mu.Unlock()
			config.TouchChanged(d.configDir)
			return host, false, pending, nil
		}
		// Bind: a new slot, or one pending while up. An exact forward
		// re-binds at its own port; a bumpable one pending from exhaustion
		// tries the new request's preference, as it always has.
		added := true
		target, bindExact := pref, exact
		if !ok {
			f = newFwd(pref, guest, false, own) // claim the slot; exact once bound
			d.forwards[guest] = f
		} else {
			added = f.addOwner(own)
			if f.exact {
				target, bindExact = f.host, true
			}
		}
		f.startBinding()
		d.mu.Unlock()
		return d.bindSlot(tr, f, own, added, target, bindExact)
	}
}

// bindSlot runs add's bind for a slot it claimed and settles the result.
func (d *daemon) bindSlot(tr transport, f *fwd, own owner, added bool, target int, exact bool) (host int, bumped, pending bool, err error) {
	host, closer, bumped, err := bind(tr, target, f.guest, exact)
	if err != nil {
		d.mu.Lock()
		d.settleLocked(f) // removed while binding: the slot goes now
		if d.forwards[f.guest] == f && f.closer == nil && (d.tr != tr || d.state != StateUp) {
			// The transport died under the bind: the forward is pending like
			// any other, and restore or the ticker binds it. Not an error.
			host := f.host
			d.mu.Unlock()
			config.TouchChanged(d.configDir)
			return host, false, true, nil
		}
		// An add that never bound isn't remembered: only its own owner goes
		// (and the forward with it if nobody else holds it). An owner that
		// was already there stays, still pending, for the ticker.
		if d.forwards[f.guest] == f && f.closer == nil && added {
			delete(f.owners, own)
			if len(f.owners) == 0 {
				delete(d.forwards, f.guest)
			}
		}
		d.mu.Unlock()
		return 0, false, false, err
	}
	if !d.adopt(tr, f, host, closer, exact) {
		// The transport died and this slot is pending again, or it was
		// removed while binding. Only this add's own record answers: a new
		// record for the same guest (a later add, bound or binding by now)
		// is someone else's forward.
		d.mu.Lock()
		cur, ok := d.forwards[f.guest]
		ok = ok && cur == f
		var h int
		var p bool
		if ok {
			h, p = cur.host, cur.closer == nil
		}
		d.mu.Unlock()
		if ok {
			return h, false, p, nil
		}
		return 0, false, false, fmt.Errorf("forward for guest %d was removed while binding", f.guest)
	}
	config.TouchChanged(d.configDir)
	return host, bumped, false, nil
}

// lateWaitFor bounds shutdown's wait for a stop-interrupted dial to release
// its transport. It must outlast the dial itself — an ssh master's connect
// is bounded by ConnectTimeout — or the orphan it exists to prevent is
// routine on exactly the sleep/wake path that triggers it.
func lateWaitFor() time.Duration {
	return time.Duration(backend.SSHConnectTimeout())*time.Second + 5*time.Second
}

// errPortExhausted: no host port in the bump range could be bound.
var errPortExhausted = errors.New("no free host port")

// bind is the port-allocating core of add/restore. It touches no daemon
// state — callers adopt the result under the lock — so the `ssh -O forward`
// it runs never blocks ping/list. An exact bind tries pref alone and fails
// with errPortBusy rather than bumping.
func bind(tr transport, pref, guest int, exact bool) (host int, closer io.Closer, bumped bool, err error) {
	if tr == nil {
		return 0, nil, false, errors.New("no transport")
	}
	if exact {
		c, ferr := tr.forward(pref, guest)
		if errors.Is(ferr, errPortBusy) {
			return 0, nil, false, fmt.Errorf("host port %d is in use: %w", pref, errPortBusy)
		}
		if ferr != nil {
			return 0, nil, false, ferr
		}
		return pref, c, false, nil
	}
	h := pref
	for tries := 0; tries < 20; tries++ {
		c, ferr := tr.forward(h, guest)
		if ferr == nil {
			return h, c, h != pref, nil
		}
		if !errors.Is(ferr, errPortBusy) {
			return 0, nil, false, ferr
		}
		h++
	}
	return 0, nil, false, fmt.Errorf("%w for guest %d in range %d-%d", errPortExhausted, guest, pref, pref+19)
}

// remove drops one owner of a guest's forward and closes the forward if no
// owner remains. It reports whether the forward existed and, if it survives,
// what it looks like now (so `ports rm` can say who still holds it).
func (d *daemon) remove(guest int, own owner) (found bool, left *Forward) {
	d.mu.Lock()
	f, ok := d.forwards[guest]
	if !ok {
		d.mu.Unlock()
		return false, nil
	}
	_, had := f.owners[own]
	delete(f.owners, own)
	if len(f.owners) > 0 {
		w := f.wire()
		d.mu.Unlock()
		if had {
			config.TouchChanged(d.configDir)
		}
		return true, &w
	}
	closer := d.releaseLocked(f)
	d.mu.Unlock()
	if closer != nil {
		closer.Close()
	}
	config.TouchChanged(d.configDir)
	return true, nil
}

// dropOwners removes every owner match selects from every forward and
// closes the forwards left with none. Closes run outside d.mu.
func (d *daemon) dropOwners(match func(owner) bool) (dropped int) {
	d.mu.Lock()
	closers, dropped := d.dropOwnersLocked(match)
	d.mu.Unlock()
	closeAll(closers)
	if dropped > 0 {
		config.TouchChanged(d.configDir)
	}
	return dropped
}

// dropOwnersLocked is dropOwners' map work; d.mu must be held. The closers
// it returns are for the caller to close once it has unlocked (each may be
// an `ssh -O cancel`).
func (d *daemon) dropOwnersLocked(match func(owner) bool) (closers []io.Closer, dropped int) {
	for _, f := range d.forwards {
		for o := range f.owners {
			if match(o) {
				delete(f.owners, o)
				dropped++
			}
		}
		if c := d.releaseLocked(f); c != nil {
			closers = append(closers, c)
		}
	}
	return closers, dropped
}

func closeAll(cs []io.Closer) {
	for _, c := range cs {
		c.Close()
	}
}

// down is `ports down` (bridge §4): drop every conf owner, and stop the
// daemon only if no forward and no session remains. Stopping outright would
// pull forwards out from under other owners, and with hub forwards (a
// laptop's `__session` holding this daemon) it would loop: the far side's
// reconnect respawns the daemon at once.
func (d *daemon) down() (stopped bool, left []Forward, sessions int) {
	d.dropOwners(func(o owner) bool { return o.kind == OwnerConf })
	d.mu.Lock()
	stopped = d.heldLocked() == 0 && len(d.sessions) == 0
	sessions = len(d.sessions)
	d.mu.Unlock()
	if stopped {
		d.triggerStop()
		return true, nil, 0
	}
	return false, d.list(), sessions
}

func (d *daemon) list() []Forward {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Forward, 0, len(d.forwards))
	for _, f := range d.forwards {
		if len(f.owners) > 0 { // an ownerless slot mid-bind is on its way out
			out = append(out, f.wire())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Guest < out[j].Guest })
	return out
}

func (d *daemon) sessionCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sessions)
}

// status snapshots the daemon's state for list/ping replies.
func (d *daemon) status() (state string, since time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state, d.since
}

// requestKick asks the reconnect loop to retry now. Non-blocking; a kick
// while up or while one is already queued is a no-op.
func (d *daemon) requestKick() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

func (d *daemon) serveControl() {
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return
		}
		go d.handleConn(conn)
	}
}

func (d *daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	// A connected-but-silent client must not pin a goroutine forever. A
	// session clears this once it is open: it is silent by design.
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	var req Request
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		writeResp(conn, Response{Err: "bad request: " + err.Error()})
		return
	}
	if req.Op == OpSession {
		d.serveSession(conn, br, req)
		return
	}
	resp := d.dispatch(req)
	resp.ID = req.ID
	writeResp(conn, resp)
}

// dispatch answers a one-shot request, and the ops a session shares with
// one-shot connections. Forwards added here are conf-owned: a one-shot
// connection closes right after, so it can own nothing (`ports add`/`up`).
func (d *daemon) dispatch(req Request) Response {
	switch req.Op {
	case OpAdd:
		host, bumped, pending, err := d.add(req.Host, req.Guest, req.Exact, confOwner)
		if err != nil {
			return Response{Err: err.Error()}
		}
		return Response{OK: true, Host: host, Bumped: bumped, Pending: pending}
	case OpRemove:
		own := confOwner
		switch req.Owner {
		case "", OwnerConf:
		case OwnerTTL:
			own = ttlOwner
		case OwnerConnection:
			return Response{Err: "a connection owner is dropped only by its own session"}
		default:
			return Response{Err: "unknown owner: " + req.Owner}
		}
		resp := Response{OK: true}
		if _, left := d.remove(req.Guest, own); left != nil {
			resp.Forwards = []Forward{*left}
		}
		return resp
	case OpDown:
		stopped, left, sessions := d.down()
		return Response{OK: true, Stopped: stopped, Forwards: left, Sessions: sessions}
	case OpList:
		state, since := d.status()
		return Response{OK: true, State: state, Since: since, Version: d.version, Sessions: d.sessionCount(), Forwards: d.list()}
	case OpPing:
		state, since := d.status()
		return Response{OK: true, State: state, Since: since, Version: d.version, Sessions: d.sessionCount()}
	case OpKick:
		d.requestKick()
		return Response{OK: true}
	case OpStop:
		defer d.triggerStop()
		return Response{OK: true}
	case OpSubscribe, OpUnsubscribe:
		return Response{Err: req.Op + " needs a session connection"}
	default:
		return Response{Err: "unknown op: " + req.Op}
	}
}

func writeResp(w io.Writer, r Response) {
	b, _ := json.Marshal(r)
	w.Write(append(b, '\n'))
}

// acquireLock takes an exclusive flock on path, blocking until it's free.
// Closing the returned file releases it.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}
