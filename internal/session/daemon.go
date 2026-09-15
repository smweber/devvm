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
}

type daemon struct {
	configDir string
	name      string
	version   string                    // build that spawned this daemon (cli.Version)
	dial      func() (transport, error) // re-dials the transport after it dies
	ln        net.Listener
	logf      func(format string, args ...any)

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

	tr, err := newTransport(ctx, m, b)
	if err != nil {
		return err
	}
	ln, err := listenControl(sock)
	if err != nil {
		tr.Close()
		return err
	}
	// The socket is live and dialable; let any queued starter in to see that.
	lock.Close()

	dial := func() (transport, error) {
		// A stopped smol VM is never dialed: exec'ing into it could boot a
		// machine the user deliberately stopped, and it can't hold forwards
		// anyway. `devvm start` brings the forwards back via tunnelUp.
		if m.Backend == config.BackendSmol {
			if st, err := b.Status(); err == nil && !st.Running {
				return nil, errVMNotRunning
			}
		}
		return newTransport(ctx, m, b)
	}
	d := newDaemon(configDir, m.Name, version, tr, dial)
	d.ln = ln
	go d.serveControl()
	d.loop()
	return d.shutdown()
}

// listenControl opens the control socket. Go unlinks a unix socket when its
// listener closes; that is turned off so shutdown() controls when the path
// disappears (last, after the transport is released — see shutdown).
func listenControl(sock string) (*net.UnixListener, error) {
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return nil, err
	}
	ln.SetUnlinkOnClose(false)
	return ln, nil
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
		go func() {
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
func (d *daemon) restore(tr transport) bool {
	d.mu.Lock()
	d.tr = tr
	guests := make([]int, 0, len(d.forwards))
	for g := range d.forwards {
		guests = append(guests, g)
	}
	sort.Ints(guests)
	restored, ok := 0, true
	for _, g := range guests {
		f := d.forwards[g]
		host, bumped, err := d.bind(f.host, g)
		if err != nil {
			d.logf("%s: forward for guest %d still pending: %v", d.name, g, err)
			if !errors.Is(err, errPortExhausted) {
				ok = false
				break
			}
			continue
		}
		if bumped {
			d.logf("%s: host port %d taken; guest %d now on localhost:%d", d.name, f.host, g, host)
		}
		restored++
	}
	if !ok {
		d.mu.Unlock()
		return false
	}
	d.setStateLocked(StateUp)
	d.mu.Unlock()
	config.TouchChanged(d.configDir)
	d.logf("%s: reconnected, %d forwards restored", d.name, restored)
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
func (d *daemon) shutdown() error {
	d.ln.Close()
	d.teardown()
	d.mu.Lock()
	d.forwards = map[int]*fwd{}
	d.mu.Unlock()
	os.Remove(socketPath(d.configDir, d.name))
	// The socket vanishing is itself a watch event; the marker covers the
	// no-forwards-on-exit case where a consumer would otherwise infer nothing.
	config.TouchChanged(d.configDir)
	return nil
}

// add allocates a host port (bumping on conflict, up to +20) and starts the
// forward, mirroring smol_forward_up / ssh_forwards. While reconnecting it
// only records the request (pending=true) so a `ports up` issued during an
// outage comes up as soon as the transport is back, instead of erroring.
func (d *daemon) add(pref, guest int) (host int, bumped, pending bool, err error) {
	d.mu.Lock()
	if f, ok := d.forwards[guest]; ok {
		d.mu.Unlock()
		return f.host, false, f.closer == nil, nil
	}
	if d.state == StateReconnecting {
		d.forwards[guest] = &fwd{host: pref, guest: guest}
		d.mu.Unlock()
		config.TouchChanged(d.configDir)
		return pref, false, true, nil
	}
	host, bumped, err = d.bind(pref, guest)
	d.mu.Unlock()
	config.TouchChanged(d.configDir)
	return host, bumped, false, err
}

// errPortExhausted: no host port in the bump range could be bound.
var errPortExhausted = errors.New("no free host port")

// bind is the port-allocating core of add/restore; d.mu must be held. It
// records the forward in the map (creating or updating the entry for guest).
func (d *daemon) bind(pref, guest int) (host int, bumped bool, err error) {
	h := pref
	for tries := 0; tries < 20; tries++ {
		closer, ferr := d.tr.forward(h, guest)
		if ferr == nil {
			d.forwards[guest] = &fwd{host: h, guest: guest, closer: closer}
			return h, h != pref, nil
		}
		if !errors.Is(ferr, errPortBusy) {
			return 0, false, ferr
		}
		h++
	}
	return 0, false, fmt.Errorf("%w for guest %d in range %d-%d", errPortExhausted, guest, pref, pref+19)
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
