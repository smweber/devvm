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
	"os/signal"
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
// port frees. The same ticker expires `ttl` owners (expireTTL).
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

// ownerInfo is what an owner carries besides its identity. expires is a ttl
// owner's wall-clock expiry (browser-bridge.md §4: fixed at creation,
// refreshed when a new open names the same port, no pause while
// reconnecting); zero for every other kind, which lives until it is dropped.
type ownerInfo struct {
	expires time.Time
}

// ttlFor is a ttl owner's info for a callback opened at now.
func ttlFor(now time.Time, ttl time.Duration) ownerInfo {
	return ownerInfo{expires: now.Add(ttl)}
}

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
	owners  map[owner]ownerInfo // torn down when empty
	// v6: the bound forward's [::1] side bound too (v6Reporter). The
	// bridge refuses a literal [::1] URL on a forward without it.
	v6 bool
	// lastErr is the last bind failure logged for this forward while it is
	// pending: the ticker retries every few seconds, and logs again only
	// when the reason changes, so a forward stuck on one error (a hub that
	// keeps refusing) shows up in the log once per reason, not per tick.
	lastErr string
}

// noteBindErr records a pending forward's bind failure and reports whether
// it differs from the last one logged; d.mu must be held.
func (f *fwd) noteBindErr(err error) bool {
	msg := err.Error()
	if msg == f.lastErr {
		return false
	}
	f.lastErr = msg
	return true
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

func newFwd(host, guest int, exact bool, own owner, info ownerInfo) *fwd {
	return &fwd{host: host, guest: guest, exact: exact, owners: map[owner]ownerInfo{own: info}}
}

// addOwner records own, reporting whether it is new. An owner already there
// is refreshed: a later expiry replaces an earlier one (a repeat callback
// open extends its ttl; it never shortens one).
func (f *fwd) addOwner(own owner, info ownerInfo) bool {
	if cur, ok := f.owners[own]; ok {
		if info.expires.After(cur.expires) {
			f.owners[own] = info
		}
		return false
	}
	f.owners[own] = info
	return true
}

// expires is the forward's ttl expiry for the wire, nil when no ttl owner
// holds it.
func (f *fwd) expires() *time.Time {
	var out *time.Time
	for o, info := range f.owners {
		if o.kind == OwnerTTL && !info.expires.IsZero() && (out == nil || info.expires.After(*out)) {
			e := info.expires
			out = &e
		}
	}
	return out
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
	return Forward{Host: f.host, Guest: f.guest, Pending: f.closer == nil, Exact: f.exact, Owners: f.ownerKinds(), Expires: f.expires()}
}

type daemon struct {
	paths    config.Paths
	name     string
	version  string                    // build that spawned this daemon (cli.Version)
	dial     func() (transport, error) // re-dials the transport after it dies
	ln       net.Listener
	sockInfo os.FileInfo // the socket we created; shutdown unlinks only that inode
	logf     func(format string, args ...any)

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

	// now is the wall clock (a var so tests can move it past a ttl).
	now func() time.Time
	// opens is the bridge's rate-limit window (allowOpen), guarded by mu;
	// replyWait bounds its wait on a subscriber's reply.
	opens     []time.Time
	replyWait time.Duration

	// upMu serializes the subscription hook (subscriber) so an upstream
	// subscribe and unsubscribe never cross; upOn is what was last sent to
	// the current transport (a fresh one starts unsubscribed). Both are
	// guarded by upMu, never d.mu: the calls wait on the far daemon.
	// upDirty: the upstream state is unknown (a call timed out), so the
	// next sync sends the wanted state whatever upOn says; upInflight is
	// that timed-out call, closed when it returns (callUpstream).
	upMu       sync.Mutex
	upOn       bool
	upDirty    bool
	upInflight chan struct{}
	// upTimeout bounds one subscription hook call.
	upTimeout time.Duration
	// hubMachine: this daemon serves a machine on a hub (HUB/NAME), so a
	// local subscribe means nothing until the relay has subscribed
	// upstream; while there is no transport it is refused (syncUpstream).
	hubMachine bool
	// relayReplyWait bounds the wait on a relay subscriber's reply, which
	// covers the far daemon's own bind and its own subscriber's wait.
	relayReplyWait time.Duration
	// onUpstream, if set (tests only), sees each subscription hook call's
	// outcome once callUpstream has it, before the caller acks anything.
	onUpstream func(on bool, err error)

	// onState, if set (tests only), sees every state change, under d.mu:
	// a transition too quick to sample (a re-dial that succeeds at once) is
	// still observed. It must not block or take d.mu.
	onState func(state string)
	// onRetry and onIdle, if set (tests only), see each retry-ticker pass
	// once it finished, and each idle check with its verdict, on the loop's
	// goroutine: a test asserts "nothing happened over N ticks" by counting
	// ticks, never by sleeping and hoping the ticks came.
	onRetry func()
	onIdle  func(idle bool)

	stopOnce sync.Once
	stop     chan struct{}
	kick     chan struct{} // OpKick: retry now instead of waiting out the backoff
}

// newDaemon wires a daemon around an already-connected transport; dial is how
// it gets a fresh one when that transport dies.
func newDaemon(p config.Paths, name, version string, tr transport, dial func() (transport, error)) *daemon {
	logger := log.New(os.Stderr, "devvm-daemon: ", log.LstdFlags)
	return &daemon{
		paths:          p,
		name:           name,
		version:        version,
		tr:             tr,
		dial:           dial,
		logf:           logger.Printf,
		minBackoff:     minReconnectBackoff,
		maxBackoff:     maxReconnectBackoff,
		idle:           idleTimeout,
		retryEvery:     exactRetryInterval,
		upTimeout:      upstreamSubscribeTimeout,
		hubMachine:     isHubName(name),
		relayReplyWait: relaySubscriberWait,
		now:            time.Now,
		replyWait:      subscriberWait,
		lateWait:       lateWaitFor(p, name),
		forwards:       map[int]*fwd{},
		sessions:       map[uint64]*sess{},
		state:          StateUp,
		since:          time.Now(),
		stop:           make(chan struct{}),
		kick:           make(chan struct{}, 1),
	}
}

// RunDaemon is the per-machine daemon entrypoint (the hidden `__daemon`
// command). It owns the transport for the machine's lifetime and serves control
// requests until idle or stopped, reconnecting when the transport dies. version
// is the build running it, reported on ping so a client can spot a stale daemon.
func RunDaemon(ctx context.Context, paths config.Paths, m *config.Machine, b backend.Backend, version string) error {
	sock := socketPath(paths, m.Name)
	if err := paths.EnsureRuntimeDir(); err != nil {
		return err
	}
	// Termination signals run the same shutdown a client's stop does, so a
	// killed daemon releases its smol exec, ssh master or hub relay instead
	// of orphaning it (see daemonSignals). Registered before the first dial,
	// so a signal during it is held for the check below, not fatal.
	sigs, stopSignals := daemonSignals()
	defer stopSignals()
	// Serialize startup: two racing daemons could otherwise both pass the dial
	// check, and the loser's stale-socket Remove would unlink the winner's live
	// socket (and, on smol, both would briefly hold parallel agent execs).
	lock, err := acquireLock(lockPath(paths, m.Name))
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
	// Signalled while waiting for the lock: stop before dialing, rather than
	// start a master or an exec only to close it.
	if s, ok := pendingSignal(sigs); ok {
		return fmt.Errorf("%v before startup; not serving", s)
	}

	// A discovered machine (smol, backend.Refresher) is asked again before
	// every dial, the first included. A stopped VM is never dialed: exec'ing
	// into it could boot a machine the user deliberately stopped, and it
	// can't hold forwards anyway (`devvm start` brings the forwards back via
	// tunnelUp). And Managed is re-settled each time (config.md §2): the VM
	// may have been deleted and recreated under the same name while this
	// daemon slept, and the transport must then refuse to install into it.
	// The fresh record goes to a fresh backend; m and b stay untouched. An
	// unmanaged answer stops here, before any exec: the transport would
	// otherwise run agentbin's probes in a VM devvm does not own before its
	// install gate refused, and reconnect treats ErrUnmanaged as final.
	settle := func() (*config.Machine, backend.Backend, error) {
		r, ok := b.(backend.Refresher)
		if !ok {
			return m, b, nil
		}
		fresh, running, err := r.Refresh()
		if err != nil {
			return nil, nil, err
		}
		if !running {
			return nil, nil, errVMNotRunning
		}
		if !fresh.Managed() {
			return nil, nil, fmt.Errorf("%s: %w (recreated outside devvm?); not dialing it", m.Name, backend.ErrUnmanaged)
		}
		fb, err := backend.For(fresh, paths)
		if err != nil {
			return nil, nil, err
		}
		return fresh, fb, nil
	}
	dialFresh := func() (transport, error) {
		fm, fb, err := settle()
		if err != nil {
			return nil, err
		}
		return newTransport(ctx, fm, fb)
	}
	tr, err := dialFresh()
	if errors.Is(err, errVMNotRunning) {
		return fmt.Errorf("%w; start it first", errVMNotRunning)
	}
	if err != nil {
		return err
	}
	// Signalled during the first dial: release what it made and go, before
	// any socket exists.
	if s, ok := pendingSignal(sigs); ok {
		tr.Close()
		return fmt.Errorf("%v during startup; not serving", s)
	}
	ln, info, err := listenControl(sock)
	if err != nil {
		tr.Close()
		return err
	}
	// The socket is live and dialable; let any queued starter in to see that.
	lock.Close()

	d := newDaemon(paths, m.Name, version, tr, dialFresh)
	d.ln, d.sockInfo = ln, info
	return d.run(sigs)
}

// daemonSignals registers the signals that stop a daemon: SIGTERM (kill,
// a logout or shutdown), SIGINT and SIGHUP (sent by hand; the daemon is
// setsid'd, so no terminal sends either). Without a handler each one killed
// the process outright, leaving its transport behind: on ssh the `ssh -M
// -f` master (a process of its own) kept answering on the ControlPath and
// holding every -L port, on smol the `machine exec` kept the agent running
// (the next daemon's would be a parallel exec), on a hub the `__session`
// relay stayed open. SIGKILL cannot be handled: startMaster's `-O exit` of
// an orphaned master covers it on the next start (ssh and hub, whose relay
// rides that master); a SIGKILLed smol daemon's exec is out of scope.
func daemonSignals() (<-chan os.Signal, func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	return sigs, func() { signal.Stop(sigs) }
}

// pendingSignal takes a signal already received, without waiting.
func pendingSignal(sigs <-chan os.Signal) (os.Signal, bool) {
	select {
	case s := <-sigs:
		return s, true
	default:
		return nil, false
	}
}

// run serves the control socket and supervises until a stop, an idle exit
// or a signal on sigs, then shuts down once. The first signal is a stop like
// any client's. Later ones are absorbed (sigs stays registered until
// RunDaemon returns, and its one-slot buffer drops the rest): a second
// SIGTERM does not force an exit, since exiting mid-teardown is exactly the
// orphan the handler exists to prevent, and shutdown is bounded anyway (each
// `ssh -O` by controlTimeout, a late dial by lateWait). SIGKILL is the
// override, cleaned up after as daemonSignals says.
func (d *daemon) run(sigs <-chan os.Signal) error {
	go d.serveControl()
	go func() {
		select {
		case s := <-sigs:
			d.logf("%s: %v; shutting down", d.name, s)
			d.triggerStop()
		case <-d.stop:
		}
	}()
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
		tr := d.transport()
		if tr == nil {
			// Only a stop leaves no transport here (restore installs one
			// before reconnect returns). Anything else is a bug: say so and
			// exit, rather than sit forever never idling out or reconnecting.
			select {
			case <-d.stop:
			default:
				d.logf("%s: supervising with no transport; exiting", d.name)
			}
			return
		}
		select {
		case <-d.stop:
			return
		case req := <-tr.events():
			// Each open on its own goroutine: it binds and waits on a
			// subscriber, and the loop must go on supervising meanwhile.
			go d.handleOpen(req)
		case <-tr.dead():
			d.onDead()
			if !d.reconnect() {
				return
			}
			// Restart the idle clock: the reconnect loop ran its own.
			idle.Reset(d.idle)
		case <-retry.C:
			d.retryPending()
			d.expireTTL()
			if d.onRetry != nil {
				d.onRetry()
			}
		case <-idle.C:
			if d.checkIdle() {
				return
			}
			idle.Reset(d.idle)
		}
	}
}

// checkIdle is idleNow, reported to the onIdle test hook.
func (d *daemon) checkIdle() bool {
	idle := d.idleNow()
	if d.onIdle != nil {
		d.onIdle(idle)
	}
	return idle
}

func (d *daemon) transport() transport {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tr
}

// onDead marks the daemon reconnecting and tears down the dead transport and
// its forwards. The forwards stay in the map, closer-less, so reconnect()
// knows what to restore and clients can still see them as pending.
//
// Relay sessions are closed here (browser-bridge.md §3, hub.md §7): a
// relay's forwards are intermediate hops whose host port restore() may
// bump, and the far daemon learns the new port only by re-adding through
// its own reconnect, which the close starts. Their forwards are dropped
// here, in the same critical section that marks the daemon reconnecting,
// so restore() never re-binds a hop nobody will use; closing the
// connection then ends the session's reader, and closeSession sweeps up
// anything its worker added after. Local sessions stay, and restore()
// re-binds their forwards.
func (d *daemon) onDead() {
	d.mu.Lock()
	d.setStateLocked(StateReconnecting)
	// One critical section for everything a reader could observe, so no
	// `list`, `ping` or `down` ever sees the daemon reconnecting with a
	// forward still reported bound, or a closed relay still counted as a
	// session (its own goroutine's closeSession runs later and finds it
	// already gone).
	relays := d.relaySessionsLocked()
	for id, rs := range relays {
		delete(d.sessions, id)
		d.subs = removeSess(d.subs, rs)
	}
	// The relays' forwards were bound on the transport being detached
	// below, whose Close drops every forward it carries: their closers are
	// not closed one by one (against a wedged link each would wait out its
	// own timeout).
	d.dropOwnersLocked(func(o owner, _ ownerInfo) bool {
		return o.kind == OwnerConnection && relays[o.conn] != nil
	})
	n := len(d.forwards) // after the relay drop: those are not restored
	tr := d.detachLocked()
	d.mu.Unlock()
	config.TouchChanged(d.paths)
	d.logf("%s: transport died; reconnecting (%d forwards to restore)", d.name, n)
	if len(relays) > 0 {
		d.logf("%s: closing %d relay session(s); they re-add once the transport is back", d.name, len(relays))
	}
	for _, s := range relays {
		s.conn.Close()
	}
	closeTransport(tr)
}

// teardown detaches every forward and the current transport, then closes
// the transport, which drops every forward it carries (transport.Close).
// The close runs outside d.mu: on ssh it is an `ssh -O exit` subprocess,
// and against a wedged master (socket present, nobody home — the classic
// post-sleep state) it only returns on its timeout. Holding the lock
// through that would freeze every ping/list/add for as long.
func (d *daemon) teardown() {
	d.mu.Lock()
	tr := d.detachLocked()
	d.mu.Unlock()
	closeTransport(tr)
}

// detachLocked marks every forward pending (its closer belongs to the
// transport, whose Close drops it) and takes the transport (leaving none)
// for the caller to close after unlocking; d.mu must be held. Every bound
// forward's closer came from d.tr: adopt records a closer only while its
// transport is the current one.
func (d *daemon) detachLocked() transport {
	for _, f := range d.forwards {
		f.closer = nil
	}
	tr := d.tr
	d.tr = nil // a bind in flight on the old transport sees this and stays pending
	return tr
}

// closeTransport closes what detachLocked took, outside d.mu.
func closeTransport(tr transport) {
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
	if d.onState != nil {
		d.onState(state)
	}
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
			if d.checkIdle() {
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
			case errors.Is(err, backend.ErrUnmanaged):
				// Not transient: the VM under this name is no longer one devvm
				// made, and no backoff makes it one. Exit rather than redial
				// it every 30s for as long as conf forwards are held.
				d.logf("%s: %v; exiting", d.name, err)
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
// its previous host port (rebindPending, failing fast), so browser tabs and
// tool configs pointing at localhost:PORT keep working across the outage.
// Returns false if a forward failed for any reason other than port
// contention: that forward stays pending and the caller retries the whole
// attempt — a forward is never dropped because one `ssh -O forward`
// hiccupped right after wake. Forwards of every owner kind come back but a
// relay's: local sessions survive a transport death, relay sessions do not
// (onDead closes them, and their forwards go with them).
func (d *daemon) restore(tr transport) bool {
	// A callback whose ttl lapsed during the outage goes before anything is
	// re-bound: re-binding it would claim its exact port for nothing until
	// the next tick (bridge §4: wall-clock, no pause while reconnecting).
	d.expireTTL()
	d.mu.Lock()
	d.tr = tr
	d.mu.Unlock()
	restored, ok := d.rebindPending(tr, true)
	if !ok {
		return false
	}
	// Re-subscribe upstream before reporting up: a fresh transport starts
	// unsubscribed, and a subscriber held across the outage must not miss
	// an event the moment the daemon looks healthy again. A failure is
	// logged, not fatal: the forwards are up, and the transport's own
	// death (a dead relay) is what brings the next attempt.
	if err := d.resubscribeUpstream(tr); err != nil {
		d.logf("%s: re-subscribing upstream: %v", d.name, err)
	}
	d.mu.Lock()
	d.setStateLocked(StateUp)
	d.mu.Unlock()
	config.TouchChanged(d.paths)
	d.logf("%s: reconnected, %d forwards restored", d.name, restored)
	return true
}

// upstreamSubscribeTimeout bounds one call of the subscription hook: an
// upstream subscribe is one round trip on the relay (hubCallTimeout's scale).
const upstreamSubscribeTimeout = 10 * time.Second

// syncUpstream brings the transport's upstream subscription in line with
// this daemon's subscribers (subscriber): subscribed again on every local
// subscribe (moving this host to the front upstream), subscribed when a
// subscriber exists and the last attempt failed, unsubscribed once when
// none is left, and whatever is wanted when the upstream state is unknown
// (upDirty: a call timed out). A no-op for a transport without the hook, or
// with none (reconnecting: restore re-subscribes), except that a local
// subscribe on a hub machine's daemon with no transport is refused
// (errUpstreamReconnecting): its ack would mean nothing upstream, and a
// caller that trusts it (auth HUB/NAME) could run a command on a hub too
// old to honour DEVVM_NO_SUBSCRIBE. Runs outside d.mu.
func (d *daemon) syncUpstream(subscribing bool) error {
	d.upMu.Lock()
	defer d.upMu.Unlock()
	d.mu.Lock()
	tr, want := d.tr, len(d.subs) > 0
	d.mu.Unlock()
	if tr == nil && subscribing && d.hubMachine {
		return errUpstreamReconnecting
	}
	sub, ok := tr.(subscriber)
	if !ok {
		return nil
	}
	if !d.upDirty && want == d.upOn && !(want && subscribing) {
		return nil
	}
	return d.callUpstream(sub, want)
}

// resubscribeUpstream is restore()'s hook call on a fresh transport: it
// starts unsubscribed, and is subscribed if this daemon has subscribers. A
// call still in flight on the old transport is no longer waited on: order
// matters per transport, and that one is gone.
func (d *daemon) resubscribeUpstream(tr transport) error {
	d.upMu.Lock()
	defer d.upMu.Unlock()
	d.upOn, d.upDirty, d.upInflight = false, false, nil
	sub, ok := tr.(subscriber)
	if !ok {
		return nil
	}
	d.mu.Lock()
	want := len(d.subs) > 0
	d.mu.Unlock()
	if !want {
		return nil
	}
	return d.callUpstream(sub, true)
}

// errUpstreamBusy: an earlier upstream call that timed out has still not
// returned, so a new one is not sent past it.
var errUpstreamBusy = errors.New("an earlier upstream subscribe has not answered yet")

// callUpstream runs one hook call, bounded by upTimeout, and records its
// outcome; upMu must be held. Calls are strictly ordered: one that
// outlives its bound is abandoned but still counted in flight, and the
// next call waits (bounded) for it to return first, so a late `true` can
// never land after a later `false`. When the abandoned call finally
// returns, the state it left upstream is unknown (upDirty), and a re-sync
// sends whatever is wanted by then: the last desired state wins.
func (d *daemon) callUpstream(sub subscriber, on bool) error {
	if prev := d.upInflight; prev != nil {
		t := time.NewTimer(d.upTimeout)
		select {
		case <-prev:
			t.Stop()
		case <-t.C:
			d.upDirty = true
			return errUpstreamBusy
		}
		d.upInflight = nil
	}
	done := make(chan struct{})
	var err error
	go func() { err = sub.setSubscribed(on); close(done) }()
	t := time.NewTimer(d.upTimeout)
	defer t.Stop()
	select {
	case <-done:
		if err == nil {
			d.upOn, d.upDirty = on, false
		}
		if d.onUpstream != nil {
			d.onUpstream(on, err)
		}
		return err
	case <-t.C:
		d.upInflight, d.upDirty = done, true
		go func() {
			<-done
			if err := d.syncUpstream(false); err != nil {
				d.logf("%s: upstream re-sync after a late answer: %v", d.name, err)
			}
		}()
		return fmt.Errorf("upstream subscribe(%v) did not answer within %s", on, d.upTimeout)
	}
}

// retryPending is the ticker's pass while the transport is up: it re-binds
// every forward left pending (rebindPending, never failing): an exact one
// whose port was taken when restore ran (a port still taken waits for the
// next tick), a bumpable one that hit port exhaustion, or one whose add was
// cut off by a transport death and was skipped by restore because it was
// mid-bind.
func (d *daemon) retryPending() {
	d.mu.Lock()
	tr := d.tr
	up := d.state == StateUp && tr != nil
	d.mu.Unlock()
	if !up {
		return
	}
	if n, _ := d.rebindPending(tr, false); n > 0 {
		config.TouchChanged(d.paths)
	}
}

// expireTTL drops every ttl owner past its expiry, and the forwards left
// with no owner (browser-bridge.md §4). Wall-clock: a callback whose bridge
// was down for its whole ttl belongs to a login that is dead anyway, so it
// expires on the first tick after restore() with no clock paused for the
// outage. Runs on the retry ticker, so a lapsed callback goes within one
// tick of its expiry.
func (d *daemon) expireTTL() {
	now := d.now()
	if n := d.dropOwners(func(o owner, info ownerInfo) bool {
		return o.kind == OwnerTTL && !info.expires.IsZero() && !now.Before(info.expires)
	}); n > 0 {
		d.logf("%s: %d callback forward(s) expired", d.name, n)
	}
}

// rebindPending binds every pending forward on tr, the one body behind
// restore() and the retry ticker, and reports how many it bound. Binds run
// outside d.mu (each is an `ssh -O forward`, bounded only by its timeout
// against a degraded master), so ping/list/add stay responsive; a forward
// added while a batch was binding is picked up by the next batch. The skip
// rules:
//
//   - a slot some add is still binding (on the dead transport, for restore)
//     is left to it: binding it here too would race its listener for the
//     same port (on smol the old listener can briefly hold P and this bind
//     would bump to P+1). That add ends pending and the ticker binds it;
//   - a forward only relays (or connections already gone) hold is left for
//     its session's close to drop: a relay's forwards are hops its far
//     daemon re-adds through its own reconnect (hub.md §7), never re-bound
//     here;
//   - an exact forward is never bumped (bind): its port taken, it stays
//     pending for the next tick; a bumpable one tries its own port first.
//
// failFast (restore) returns ok=false at the first failure that is not port
// contention (busy, exhausted, or a hub's refusal, which are left pending),
// settling the rest of the batch; otherwise every failure is left pending
// and logged when its reason changes.
func (d *daemon) rebindPending(tr transport, failFast bool) (bound int, ok bool) {
	type job struct {
		f           *fwd
		host, guest int
		exact       bool
	}
	tried := map[*fwd]bool{}
	for {
		d.mu.Lock()
		var batch []job
		for _, f := range d.forwards {
			if f.closer == nil && !f.binding && !tried[f] && !d.relayOnlyLocked(f) {
				f.startBinding()
				batch = append(batch, job{f, f.host, f.guest, f.exact})
			}
		}
		d.mu.Unlock()
		if len(batch) == 0 {
			return bound, true
		}
		sort.Slice(batch, func(i, j int) bool { return batch[i].guest < batch[j].guest })
		for i, j := range batch {
			tried[j.f] = true
			host, closer, bumped, err := bind(tr, j.host, j.guest, j.exact)
			if err != nil {
				d.mu.Lock()
				d.settleLocked(j.f)
				changed := j.f.noteBindErr(err)
				d.mu.Unlock()
				if changed || failFast {
					d.logf("%s: forward for guest %d still pending: %v", d.name, j.guest, err)
				}
				contention := errors.Is(err, errPortExhausted) || errors.Is(err, errPortBusy) || errors.Is(err, errHubRefused)
				if failFast && !contention {
					d.mu.Lock()
					for _, rest := range batch[i+1:] {
						d.settleLocked(rest.f)
					}
					d.mu.Unlock()
					return bound, false
				}
				continue
			}
			if bumped {
				d.logf("%s: host port %d taken; guest %d now on localhost:%d", d.name, j.host, j.guest, host)
			}
			if d.adopt(tr, j.f, host, closer, j.exact) {
				bound++
				if !failFast {
					d.logf("%s: pending forward bound; guest %d on localhost:%d", d.name, j.guest, host)
				}
			}
		}
	}
}

// relayOnlyLocked reports whether every owner of f is a connection owner
// of a relay session, or of a session already gone (its close drops the
// owner momentarily); d.mu must be held.
func (d *daemon) relayOnlyLocked(f *fwd) bool {
	if len(f.owners) == 0 {
		return false
	}
	for o := range f.owners {
		if o.kind != OwnerConnection {
			return false
		}
		if s := d.sessions[o.conn]; s != nil && !s.relay {
			return false
		}
	}
	return true
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
	f.host, f.closer, f.lastErr = host, closer, ""
	f.v6 = forwardHasV6(closer)
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
	sock := socketPath(d.paths, d.name)
	if d.ownsSocket(sock) {
		os.Remove(sock)
	}
	// The socket vanishing is itself a watch event; the marker covers the
	// no-forwards-on-exit case where a consumer would otherwise infer nothing.
	config.TouchChanged(d.paths)
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
	return d.addOwned(pref, guest, exact, own, ownerInfo{})
}

// addOwned is add with the owner's info (a ttl owner's expiry).
func (d *daemon) addOwned(pref, guest int, exact bool, own owner, info ownerInfo) (host int, bumped, pending bool, err error) {
	r, err := d.addWith(pref, guest, exact, own, addOpts{info: info})
	return r.host, r.bumped, r.pending, err
}

// addOpts are addWith's extras beyond add's.
type addOpts struct {
	info ownerInfo // the owner's data (a ttl owner's expiry)
	// admit, if set, decides whether a new forward may be created; it is
	// evaluated under d.mu in the same critical section that creates the
	// slot, so a cap it enforces cannot be overrun by concurrent adds (the
	// bridge's ephemeral cap). A reused forward is never asked.
	admit func() bool
}

// addResult is what addWith did. added: own was not already an owner of
// the forward, so the caller may take it back (the bridge's [::1] refusal).
type addResult struct {
	host            int
	bumped, pending bool
	added           bool
}

// errAdmitRefused is addWith's answer when opts.admit refused a new forward.
var errAdmitRefused = errors.New("new forward not admitted")

// addWith is add with its extras (addOpts) and a fuller result. A
// connection owner is recorded only while its session is registered,
// checked in the critical section that records it: a caller that chose the
// session earlier (the bridge's subscriber, a session worker racing its
// close or onDead) must not leave a forward owned by a connection nobody
// will ever close, which would pin the daemon for good.
func (d *daemon) addWith(pref, guest int, exact bool, own owner, o addOpts) (addResult, error) {
	info := o.info
	for {
		d.mu.Lock()
		if own.kind == OwnerConnection && d.sessions[own.conn] == nil {
			d.mu.Unlock()
			return addResult{}, errSubscriberGone
		}
		f, ok := d.forwards[guest]
		if ok && f.binding {
			ready := f.ready
			d.mu.Unlock()
			<-ready
			continue
		}
		if ok && exact && f.host != pref {
			d.mu.Unlock()
			return addResult{}, fmt.Errorf("host port %d unavailable: guest %d is already forwarded on localhost:%d: %w", pref, guest, f.host, errPortBusy)
		}
		if !ok && o.admit != nil && !o.admit() {
			d.mu.Unlock()
			return addResult{}, errAdmitRefused
		}
		tr := d.tr
		// Live (reuse), or no transport to bind on: reconnecting, or shutdown
		// already tore it down while this request was in flight. Record the
		// owner either way; a pending forward comes up on restore at the
		// port it has, and an exact request for it holds (host == pref).
		if (ok && f.closer != nil) || d.state == StateReconnecting || tr == nil {
			added := true
			if !ok {
				f = newFwd(pref, guest, exact, own, info)
				d.forwards[guest] = f
			} else {
				added = f.addOwner(own, info)
				if exact {
					f.exact = true // the port is pref already: honoured, not requested
				}
			}
			host, pending := f.host, f.closer == nil
			d.mu.Unlock()
			config.TouchChanged(d.paths)
			return addResult{host: host, pending: pending, added: added}, nil
		}
		// Bind: a new slot, or one pending while up. An exact forward
		// re-binds at its own port; a bumpable one pending from exhaustion
		// tries the new request's preference, as it always has.
		added := true
		target, bindExact := pref, exact
		if !ok {
			f = newFwd(pref, guest, false, own, info) // claim the slot; exact once bound
			d.forwards[guest] = f
		} else {
			added = f.addOwner(own, info)
			if f.exact {
				target, bindExact = f.host, true
			}
		}
		f.startBinding()
		d.mu.Unlock()
		host, bumped, pending, err := d.bindSlot(tr, f, own, added, target, bindExact)
		return addResult{host: host, bumped: bumped, pending: pending, added: added}, err
	}
}

// bindSlot runs add's bind for a slot it claimed and settles the result.
func (d *daemon) bindSlot(tr transport, f *fwd, own owner, added bool, target int, exact bool) (host int, bumped, pending bool, err error) {
	host, closer, bumped, err := bind(tr, target, f.guest, exact)
	if err != nil {
		d.mu.Lock()
		d.settleLocked(f) // removed while binding: the slot goes now
		if d.forwards[f.guest] == f && f.closer == nil && (d.tr != tr || d.state != StateUp || transportDead(tr)) {
			// The transport died under the bind: the forward is pending like
			// any other, and restore or the ticker binds it. Not an error.
			// transportDead covers the window before loop() has seen the
			// death: a hub transport fails an add the moment its __session
			// goes, or when the hub answers `pending` (hub.md §7), and marks
			// itself dead in the same breath.
			host := f.host
			d.mu.Unlock()
			config.TouchChanged(d.paths)
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
	config.TouchChanged(d.paths)
	return host, bumped, false, nil
}

// lateWaitFor bounds shutdown's wait for a stop-interrupted dial to release
// its transport. It must outlast the dial itself — an ssh master's connect
// is bounded by ConnectTimeout — or the orphan it exists to prevent is
// routine on exactly the sleep/wake path that triggers it.
func lateWaitFor(p config.Paths, name string) time.Duration {
	return time.Duration(connectTimeoutFor(p, name))*time.Second + 5*time.Second
}

// transportDead reports whether tr's dead channel has fired.
func transportDead(tr transport) bool {
	select {
	case <-tr.dead():
		return true
	default:
		return false
	}
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
			config.TouchChanged(d.paths)
		}
		return true, &w
	}
	closer := d.releaseLocked(f)
	d.mu.Unlock()
	if closer != nil {
		closer.Close()
	}
	config.TouchChanged(d.paths)
	return true, nil
}

// dropOwners removes every owner match selects from every forward and
// closes the forwards left with none. Closes run outside d.mu.
func (d *daemon) dropOwners(match func(owner, ownerInfo) bool) (dropped int) {
	d.mu.Lock()
	closers, dropped := d.dropOwnersLocked(match)
	d.mu.Unlock()
	closeAll(closers)
	if dropped > 0 {
		config.TouchChanged(d.paths)
	}
	return dropped
}

// dropOwnersLocked is dropOwners' map work; d.mu must be held. The closers
// it returns are for the caller to close once it has unlocked (each may be
// an `ssh -O cancel`).
func (d *daemon) dropOwnersLocked(match func(owner, ownerInfo) bool) (closers []io.Closer, dropped int) {
	for _, f := range d.forwards {
		for o, info := range f.owners {
			if match(o, info) {
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
	d.dropOwners(func(o owner, _ ownerInfo) bool { return o.kind == OwnerConf })
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

// isHubName reports whether a daemon's machine name is HUB/NAME.
func isHubName(name string) bool {
	_, _, ok, _ := config.SplitHubName(name)
	return ok
}

// errUpstreamReconnecting refuses a local subscribe on a hub machine's
// daemon while it has no transport: nothing can be sent upstream, and an
// ack would promise events the hub will not deliver. The registration
// stays; restore() subscribes upstream once the relay is back.
var errUpstreamReconnecting = errors.New("the link to the hub is reconnecting; retry in a moment")

// shimPath is the BROWSER shim the current transport reports (shimReporter),
// "" when it has none or the daemon is between transports. The call is a
// field read on the transport, so it runs under d.mu.
func (d *daemon) shimPath() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r, ok := d.tr.(shimReporter); ok {
		return r.shimPath()
	}
	return ""
}

// carriesEvents reports whether the current transport brings the guest's
// browser events to this daemon: smol's agent exec does, ssh's does while
// it has a current agent (eventCarrier: installed on a managed box, found
// on an adopt host), and a hub machine's relay does only when its hub
// daemon echoed Bridge (a hub machine's events come from the hub, or from
// nowhere). false between transports. Under d.mu, like shimPath.
func (d *daemon) carriesEvents() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.carriesEventsLocked()
}

// carriesEventsLocked is carriesEvents with d.mu held: openSession decides
// a relay's Bridge echo under the same lock that admitted it.
func (d *daemon) carriesEventsLocked() bool {
	if d.tr == nil {
		return false
	}
	if h, ok := d.tr.(*hubTransport); ok {
		return h.bridge
	}
	if ec, ok := d.tr.(eventCarrier); ok {
		return ec.hasEvents()
	}
	return d.tr.events() != nil
}

// status snapshots the daemon's state for list/ping replies.
func (d *daemon) status() (state string, since time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state, d.since
}

// nudge tells the current transport a user is about to use the guest
// (nudger: smol syncs its clock, ssh retries a missing agent exec now), if
// it does anything with that and one is up. Non-blocking.
func (d *daemon) nudge() {
	d.mu.Lock()
	tr := d.tr
	d.mu.Unlock()
	if n, ok := tr.(nudger); ok {
		n.nudge()
	}
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
		return Response{OK: true, State: state, Since: since, Version: d.version, Sessions: d.sessionCount(), Shim: d.shimPath()}
	case OpKick:
		d.requestKick()
		d.nudge()
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
