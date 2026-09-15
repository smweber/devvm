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

// idleTimeout is how long the daemon lingers with zero forwards before exiting
// (ControlPersist-style), so a `tunnel down` or last `unport` reaps it.
const idleTimeout = 60 * time.Second

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

// fwd is one forward the daemon is responsible for. closer is nil while the
// transport is down: the forward is remembered and re-bound on reconnect.
type fwd struct {
	host, guest int
	closer      io.Closer
	// binding marks a slot claimed by an in-flight add. A second add for the
	// same guest must not bind again: on ssh both closers would carry the same
	// -L spec and the loser's `ssh -O cancel` would kill the winner.
	binding bool
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

	// Backoff/idle knobs live on the struct so tests can shrink them.
	minBackoff, maxBackoff, idle time.Duration

	mu       sync.Mutex
	tr       transport
	forwards map[int]*fwd // keyed by guest port
	state    string       // StateUp | StateReconnecting
	since    time.Time    // when state last changed

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
		lateWait:   lateWaitFor(),
		forwards:   map[int]*fwd{},
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
func (d *daemon) loop() {
	idle := time.NewTimer(d.idle)
	defer idle.Stop()
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
		case <-idle.C:
			if d.count() == 0 {
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
			if d.count() == 0 {
				d.logf("%s: no forwards to restore; exiting", d.name)
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
// exactly like a first-time add. Returns false if a forward failed for any
// reason other than port contention: that forward stays pending and the
// caller retries the whole attempt — a forward is never dropped because one
// `ssh -O forward` hiccupped right after wake. Port exhaustion is logged and
// the forward left pending without failing the rest.
//
// Binds run outside d.mu (each is an `ssh -O forward`, bounded only by its
// timeout against a degraded master), so ping/list/add stay responsive; the
// loop picks up forwards added while a batch was binding.
func (d *daemon) restore(tr transport) bool {
	d.mu.Lock()
	d.tr = tr
	d.mu.Unlock()
	restored := 0
	tried := map[int]bool{}
	for {
		d.mu.Lock()
		var batch []fwd
		for g, f := range d.forwards {
			if f.closer == nil && !tried[g] {
				batch = append(batch, *f)
			}
		}
		d.mu.Unlock()
		if len(batch) == 0 {
			break
		}
		sort.Slice(batch, func(i, j int) bool { return batch[i].guest < batch[j].guest })
		for _, f := range batch {
			tried[f.guest] = true
			host, closer, bumped, err := bind(tr, f.host, f.guest)
			if err != nil {
				d.logf("%s: forward for guest %d still pending: %v", d.name, f.guest, err)
				if !errors.Is(err, errPortExhausted) {
					return false
				}
				continue
			}
			if bumped {
				d.logf("%s: host port %d taken; guest %d now on localhost:%d", d.name, f.host, f.guest, host)
			}
			if d.adopt(tr, f.guest, host, closer) {
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

// adopt records a forward bound outside the lock. It is dropped (closed) if
// the guest was removed meanwhile, if another bind already won, or if the
// transport it was bound on is no longer the current one.
func (d *daemon) adopt(tr transport, guest, host int, closer io.Closer) bool {
	d.mu.Lock()
	f, ok := d.forwards[guest]
	if ok {
		f.binding = false
	}
	if !ok || f.closer != nil || d.tr != tr {
		d.mu.Unlock()
		closer.Close()
		return false
	}
	f.host, f.closer = host, closer
	d.mu.Unlock()
	return true
}

func (d *daemon) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.forwards)
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
	sock := socketPath(d.configDir, d.name)
	if info, err := os.Lstat(sock); err == nil && (d.sockInfo == nil || os.SameFile(info, d.sockInfo)) {
		os.Remove(sock)
	}
	// The socket vanishing is itself a watch event; the marker covers the
	// no-forwards-on-exit case where a consumer would otherwise infer nothing.
	config.TouchChanged(d.configDir)
	return nil
}

// add allocates a host port (bumping on conflict, up to +20) and starts the
// forward, mirroring smol_forward_up / ssh_forwards. While reconnecting it
// only records the request (pending=true) so a `ports up` issued during an
// outage comes up as soon as the transport is back, instead of erroring. A
// forward left pending while up (port exhaustion during restore) is re-bound
// here rather than reported pending forever.
func (d *daemon) add(pref, guest int) (host int, bumped, pending bool, err error) {
	d.mu.Lock()
	if f, ok := d.forwards[guest]; ok && (f.closer != nil || f.binding || d.state == StateReconnecting) {
		host, pending := f.host, f.closer == nil // read under the lock; adopt writes them
		d.mu.Unlock()
		return host, false, pending, nil
	}
	tr := d.tr
	// No transport to bind on: reconnecting, or shutdown already tore it
	// down while this request was in flight. Record it as pending either way
	// rather than dereferencing nil.
	if d.state == StateReconnecting || tr == nil {
		if _, ok := d.forwards[guest]; !ok {
			d.forwards[guest] = &fwd{host: pref, guest: guest}
		}
		d.mu.Unlock()
		config.TouchChanged(d.configDir)
		return pref, false, true, nil
	}
	f, ok := d.forwards[guest]
	if !ok {
		f = &fwd{host: pref, guest: guest} // claim the slot; bind below
		d.forwards[guest] = f
	}
	f.binding = true
	d.mu.Unlock()
	host, closer, bumped, err := bind(tr, pref, guest)
	if err != nil {
		d.mu.Lock()
		if f, ok := d.forwards[guest]; ok {
			f.binding = false
			if f.closer == nil && d.state == StateUp {
				delete(d.forwards, guest) // an add that never bound isn't remembered
			}
		}
		d.mu.Unlock()
		return 0, false, false, err
	}
	if !d.adopt(tr, guest, host, closer) {
		// Removed, or the transport died and the slot is pending again.
		d.mu.Lock()
		f, ok := d.forwards[guest]
		var host int
		var pending bool
		if ok {
			host, pending = f.host, f.closer == nil
		}
		d.mu.Unlock()
		if ok {
			return host, false, pending, nil
		}
		return 0, false, false, fmt.Errorf("forward for guest %d was removed while binding", guest)
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
// it runs never blocks ping/list.
func bind(tr transport, pref, guest int) (host int, closer io.Closer, bumped bool, err error) {
	if tr == nil {
		return 0, nil, false, errors.New("no transport")
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

func (d *daemon) remove(guest int) {
	d.mu.Lock()
	f, ok := d.forwards[guest]
	if ok {
		delete(d.forwards, guest)
	}
	d.mu.Unlock()
	if !ok {
		return
	}
	if f.closer != nil {
		f.closer.Close()
	}
	config.TouchChanged(d.configDir)
}

func (d *daemon) list() []Forward {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Forward, 0, len(d.forwards))
	for _, f := range d.forwards {
		out = append(out, Forward{Host: f.host, Guest: f.guest, Pending: f.closer == nil})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Guest < out[j].Guest })
	return out
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
	// A connected-but-silent client must not pin a goroutine forever.
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return
	}
	var req Request
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		writeResp(conn, Response{Err: "bad request: " + err.Error()})
		return
	}
	writeResp(conn, d.dispatch(req))
}

func (d *daemon) dispatch(req Request) Response {
	switch req.Op {
	case OpAdd:
		host, bumped, pending, err := d.add(req.Host, req.Guest)
		if err != nil {
			return Response{Err: err.Error()}
		}
		return Response{OK: true, Host: host, Bumped: bumped, Pending: pending}
	case OpRemove:
		d.remove(req.Guest)
		return Response{OK: true}
	case OpList:
		state, since := d.status()
		return Response{OK: true, State: state, Since: since, Version: d.version, Forwards: d.list()}
	case OpPing:
		state, since := d.status()
		return Response{OK: true, State: state, Since: since, Version: d.version}
	case OpKick:
		d.requestKick()
		return Response{OK: true}
	case OpStop:
		defer d.triggerStop()
		return Response{OK: true}
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
