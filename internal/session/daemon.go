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

// fwd is one forward the daemon is responsible for. closer is nil while the
// transport is down: the forward is remembered and re-bound on reconnect.
type fwd struct {
	host, guest int
	closer      io.Closer
}

type daemon struct {
	configDir string
	name      string
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
}

// newDaemon wires a daemon around an already-connected transport; dial is how
// it gets a fresh one when that transport dies.
func newDaemon(configDir, name string, tr transport, dial func() (transport, error)) *daemon {
	logger := log.New(os.Stderr, "devvm-daemon: ", log.LstdFlags)
	return &daemon{
		configDir:  configDir,
		name:       name,
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
	}
}

// RunDaemon is the per-machine daemon entrypoint (the hidden `__daemon`
// command). It owns the transport for the machine's lifetime and serves control
// requests until idle, stopped, or the transport dies.
func RunDaemon(ctx context.Context, configDir string, m *config.Machine, b backend.Backend) error {
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
	ln, err := net.Listen("unix", sock)
	if err != nil {
		tr.Close()
		return err
	}
	// The socket is live and dialable; let any queued starter in to see that.
	lock.Close()

	d := newDaemon(configDir, m.Name, tr, func() (transport, error) { return newTransport(ctx, m, b) })
	d.ln = ln
	go d.serveControl()
	d.loop()
	return d.shutdown()
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

// onDead tears down every forward on the dead transport (their host listeners
// or -L channels are gone or about to be) and reaps the transport itself, then
// marks the daemon reconnecting. The forwards stay in the map, closer-less, so
// reconnect() knows what to restore and clients can still see them as pending.
func (d *daemon) onDead() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, f := range d.forwards {
		if f.closer != nil {
			f.closer.Close()
			f.closer = nil
		}
	}
	_ = d.tr.Close()
	d.setState(StateReconnecting)
	d.logf("%s: transport died; reconnecting (%d forwards to restore)", d.name, len(d.forwards))
}

// setState must be called with d.mu held.
func (d *daemon) setState(state string) {
	if d.state != state {
		d.state, d.since = state, time.Now()
		config.TouchChanged(d.configDir)
	}
}

// reconnect re-dials with exponential backoff until it succeeds (true), is
// stopped (false), or has sat with nothing to restore for the idle period
// (false) — the same idle rule an up daemon follows.
func (d *daemon) reconnect() bool {
	backoff := d.minBackoff
	wait := time.NewTimer(backoff)
	defer wait.Stop()
	idle := time.NewTimer(d.idle)
	defer idle.Stop()
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
		case <-wait.C:
			tr, err := d.dial()
			if err != nil {
				backoff = min(backoff*2, d.maxBackoff)
				d.logf("%s: reconnect attempt %d failed: %v (retrying in %s)", d.name, attempt, err, backoff)
				wait.Reset(backoff)
				continue
			}
			d.restore(tr)
			return true
		}
	}
}

// restore adopts a fresh transport and re-binds every remembered forward at
// its previous host port, so browser tabs and tool configs pointing at
// localhost:PORT keep working across the outage. A port taken meanwhile bumps
// exactly like a first-time add; any other failure drops that forward.
func (d *daemon) restore(tr transport) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tr = tr
	guests := make([]int, 0, len(d.forwards))
	for g := range d.forwards {
		guests = append(guests, g)
	}
	sort.Ints(guests)
	restored := 0
	for _, g := range guests {
		f := d.forwards[g]
		host, bumped, err := d.bind(f.host, g)
		if err != nil {
			d.logf("%s: dropping forward for guest %d: %v", d.name, g, err)
			delete(d.forwards, g)
			continue
		}
		if bumped {
			d.logf("%s: host port %d taken; guest %d now on localhost:%d", d.name, f.host, g, host)
		}
		restored++
	}
	d.setState(StateUp)
	d.logf("%s: reconnected, %d forwards restored", d.name, restored)
}

func (d *daemon) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.forwards)
}

func (d *daemon) triggerStop() {
	d.stopOnce.Do(func() { close(d.stop) })
}

func (d *daemon) shutdown() error {
	d.ln.Close()
	os.Remove(socketPath(d.configDir, d.name))
	// The socket vanishing is itself a watch event; the marker covers the
	// no-forwards-on-exit case where a consumer would otherwise infer nothing.
	defer config.TouchChanged(d.configDir)
	d.mu.Lock()
	for _, f := range d.forwards {
		if f.closer != nil {
			f.closer.Close()
		}
	}
	d.forwards = map[int]*fwd{}
	tr := d.tr
	d.mu.Unlock()
	return tr.Close()
}

// add allocates a host port (bumping on conflict, up to +20) and starts the
// forward, mirroring smol_forward_up / ssh_forwards. While reconnecting it
// only records the request (pending=true) so a `ports up` issued during an
// outage comes up as soon as the transport is back, instead of erroring.
func (d *daemon) add(pref, guest int) (host int, bumped, pending bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f, ok := d.forwards[guest]; ok {
		return f.host, false, f.closer == nil, nil
	}
	defer config.TouchChanged(d.configDir)
	if d.state == StateReconnecting {
		d.forwards[guest] = &fwd{host: pref, guest: guest}
		return pref, false, true, nil
	}
	host, bumped, err = d.bind(pref, guest)
	return host, bumped, false, err
}

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
	return 0, false, fmt.Errorf("no free host port for guest %d in range %d-%d", guest, pref, pref+19)
}

func (d *daemon) remove(guest int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f, ok := d.forwards[guest]; ok {
		if f.closer != nil {
			f.closer.Close()
		}
		delete(d.forwards, guest)
		config.TouchChanged(d.configDir)
	}
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
		return Response{OK: true, State: state, Since: since, Forwards: d.list()}
	case OpPing:
		state, since := d.status()
		return Response{OK: true, State: state, Since: since}
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
