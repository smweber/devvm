package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// hubTransport is the laptop daemon's transport for a machine on a hub
// (hub.md §7): a dedicated ControlMaster to the hub, and over it exactly one
// `__session NAME` process, whose stdio is a `session {relay: true}` on the
// hub daemon for NAME. The hub daemon owns the only agent exec into the VM;
// nothing here ever spawns one (the one-exec rule).
//
// One forwarding contract: forward(host, guest) names a guest port, like
// every transport. It asks the hub for that guest port (`add`, owned on the
// hub by the relayed connection), reads back the hub port the hub chose
// (bumped there or not, nothing here cares), and binds a native
// `-L localhost:host:127.0.0.1:hubPort` on the master. The forward is
// reported only once both hold. restore() re-resolves every hub port for
// free: it calls forward on a fresh transport, whose fresh `__session`
// re-adds.
//
// dead() fires when the master dies, when the `__session` process exits (the
// hub daemon closed the relay because its own transport died, or it was
// stopped or cycled), or when the hub answers `pending`: three causes, one
// recovery, the daemon's reconnect.
type hubTransport struct {
	name  string
	sc    *sessConn
	local hubLocal
	stop  func() // ends the __session process; idempotent

	deadCh    chan struct{}
	deadOnce  sync.Once
	closeOnce sync.Once
	// closing is set when the daemon starts tearing this transport down
	// (beginTeardown): Close drops the relay and the master, which drops
	// every forward on both sides at once, so a forward's own close does
	// nothing that would only wait on the hub or the master.
	closing atomic.Bool
}

// hubLocal is the host side of hub forwards: the -L on the master
// (*sshTransport), or a fake in tests.
type hubLocal interface {
	forwardTo(hostPort, hubPort int) (io.Closer, error)
	dead() <-chan struct{}
	Close() error
}

// hubOpenTimeout bounds the wait for `__session`'s marker and the relay's
// open reply together (one deadline). The hub side runs a login shell, then
// session.Dial there, which may spawn the hub daemon and wait out its first
// transport dial (Dial's own come-up bound is ConnectTimeout plus 10s), so
// this is well above that.
var hubOpenTimeout = 45 * time.Second

// hubCallTimeout bounds every call on an open relay (add, remove). A hub
// add is a local bind on the hub; a hub that takes longer is wedged, and
// the add treats it as the relay's death.
var hubCallTimeout = 10 * time.Second

// HubForwardsMinVersion is the first devvm release whose hub side serves
// hub forwards (`__session`, relay sessions). It is checked per feature,
// when a relay is opened, not by the hub floor (the cli's hubMinVersion),
// because listing, cp and proxying work against older hubs. It must be the
// first tag that ships roadmap step 6.
const HubForwardsMinVersion = "v0.1.14"

// errHubRefused marks a hub's explicit refusal of a relayed add (its error
// reply). The hub is up and answering; this one forward cannot be had there
// right now, so it stays pending here and the ticker retries it.
var errHubRefused = errors.New("the hub refused the forward")

// hubRefusal is one refusal, the hub's own text verbatim.
type hubRefusal struct{ msg string }

func (e *hubRefusal) Error() string        { return "hub: " + e.msg }
func (e *hubRefusal) Is(target error) bool { return target == errHubRefused }

// errHubPending is a `pending` reply to a relayed add: the hub has lost its
// transport and will close this relay (hub.md §7). A bind failure, never a
// forward: the -L would point at a hub port nothing listens on.
var errHubPending = errors.New("the hub's forward is pending (its transport to the VM is down)")

// newHubTransport starts the master, then the `__session` process over it,
// and opens the relay. argv is the full ssh command line for `__session`
// (backend.HubSessioner.SessionArgv).
func newHubTransport(name string, conn backend.SSHConn, argv []string) (*hubTransport, error) {
	master, err := newSSHTransport(conn)
	if err != nil {
		return nil, err
	}
	c, stop, tail, err := startHubSession(argv)
	if err != nil {
		master.Close()
		return nil, err
	}
	t, err := openHubTransport(name, c, master, stop, tail)
	if err != nil {
		master.Close()
		return nil, err
	}
	return t, nil
}

// startHubSession runs the `__session` ssh with its pipes as *os.File, never
// an io.Pipe: os/exec hands a file to the child as is, while any other
// reader or writer gets a copy goroutine that Wait then blocks on past
// WaitDelay (the step 3 deadlock, roadmap Lessons); the ControlMaster holds
// a mux client's stdio fds too, so such a copy could outlive the client.
// The write end of stdin and the read ends of stdout and stderr are ours;
// our copies of the child's ends are closed after Start, so the session
// ending is EOF here. stderr is copied to ours (the daemon log) by a reader
// of our own, which also keeps the last line: tail() is the hub's own
// words when `__session` refuses (a stopped VM, no such machine).
func startHubSession(argv []string) (c lineConn, stop func(), tail func() string, err error) {
	var files []*os.File
	closeAll := func() {
		for _, f := range files {
			f.Close()
		}
	}
	pipe := func() (r, w *os.File) {
		if err != nil {
			return nil, nil
		}
		r, w, err = os.Pipe()
		files = append(files, r, w)
		return r, w
	}
	stdinR, stdinW := pipe()
	stdoutR, stdoutW := pipe()
	stderrR, stderrW := pipe()
	if err != nil {
		closeAll()
		return nil, nil, nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	err = cmd.Start()
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()
	if err != nil {
		closeAll()
		return nil, nil, nil, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	errTail := newLineTail(stderrR, os.Stderr)
	var once sync.Once
	stop = func() {
		once.Do(func() {
			// stdin EOF first: ssh forwards it and the hub's __session ends
			// its session cleanly; the kill is for an ssh that does not go.
			stdinW.Close()
			select {
			case <-exited:
			case <-time.After(time.Second):
				_ = cmd.Process.Kill()
				<-exited
			}
			stdoutR.Close()
			stderrR.Close()
		})
	}
	return pipeConn{r: stdoutR, w: stdinW}, stop, errTail.last, nil
}

// lineTail copies r to w line by line and remembers the last non-empty line.
type lineTail struct {
	mu   sync.Mutex
	line string
	done chan struct{}
}

func newLineTail(r io.Reader, w io.Writer) *lineTail {
	t := &lineTail{done: make(chan struct{})}
	go func() {
		defer close(t.done)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text()
			fmt.Fprintln(w, line)
			if s := strings.TrimSpace(line); s != "" {
				t.mu.Lock()
				t.line = s
				t.mu.Unlock()
			}
		}
	}()
	return t
}

// last is the last line seen, after giving the stream a moment to reach
// EOF: a refusal's stderr and the stdout EOF that reveals it race.
func (t *lineTail) last() string {
	select {
	case <-t.done:
	case <-time.After(time.Second):
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.line
}

// pipeConn is a process's stdio as a lineConn.
type pipeConn struct{ r, w *os.File }

func (p pipeConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p pipeConn) Close() error {
	p.w.Close()
	return p.r.Close()
}
func (p pipeConn) SetReadDeadline(t time.Time) error  { return p.r.SetReadDeadline(t) }
func (p pipeConn) SetWriteDeadline(t time.Time) error { return p.w.SetWriteDeadline(t) }

// openHubTransport reads past `__session`'s marker (a login banner may come
// first), opens the relay, and starts the one reader. The relay being
// refused (the hub daemon's transport is down) is an ordinary dial error:
// the daemon's reconnect backoff retries it. On any failure c is closed and
// stop run; local is the caller's.
func openHubTransport(name string, c lineConn, local hubLocal, stop func(), tail func() string) (*hubTransport, error) {
	fail := func(err error) (*hubTransport, error) {
		c.Close()
		stop()
		return nil, err
	}
	hub, machine, _, _ := config.SplitHubName(name)
	br := bufio.NewReader(c)
	deadline := time.Now().Add(hubOpenTimeout)
	_ = c.SetReadDeadline(deadline)
	if err := SkipToMarker(br, SessionMarker, MarkerLimit); err != nil {
		switch {
		case errors.Is(err, ErrMarkerMissing):
			why := ""
			if tail != nil {
				why = tail()
			}
			if strings.Contains(why, "unknown command") {
				// A hub older than hub forwards has no __session at all.
				return fail(fmt.Errorf("%s: devvm on hub %s predates hub forwards (needs %s or newer); run 'devvm update' there", name, hub, HubForwardsMinVersion))
			}
			if why == "" {
				why = "no error output"
			}
			return fail(fmt.Errorf("%s: the hub's __session ended before it started: %s", name, why))
		case errors.Is(err, ErrMarkerLimit):
			return fail(fmt.Errorf("%s: no %q line in the first %d bytes from the hub", name, SessionMarker, MarkerLimit))
		case errors.Is(err, os.ErrDeadlineExceeded):
			return fail(fmt.Errorf("%s: the hub's __session did not start within %s", name, hubOpenTimeout))
		default:
			return fail(fmt.Errorf("%s: reading the hub's __session: %w", name, err))
		}
	}
	// A daemon older than hub forwards cannot hold a relay: one from before
	// sessions answers "unknown op", and one from step 5 ignores Relay and
	// admits a plain local session, which would never be closed on its
	// transport's death nor refused while it is down. A daemon that knows
	// relays echoes Relay, so its absence is the tell. Either way the
	// daemon, not devvm, is old: the hub's binary has __session, so the
	// daemon was spawned by an earlier one and never cycled.
	stale := fmt.Errorf("%s: the forward daemon for %s on hub %s predates hub forwards; restart it there with 'devvm ports down %s' (or 'devvm stop %s' then 'devvm start %s' if a session holds it)",
		name, machine, hub, machine, machine, machine)
	sc, resp, err := openSessConnOn(c, br, Request{ID: 1, Op: OpSession, Relay: true}, deadline, hubCallTimeout)
	switch {
	case err != nil && !resp.OK && strings.HasPrefix(resp.Err, "unknown op"):
		return fail(stale)
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fail(fmt.Errorf("%s: the hub's __session did not start within %s", name, hubOpenTimeout))
	case err != nil:
		return fail(fmt.Errorf("%s: hub: %w", name, err))
	case !resp.Relay:
		sc.close()
		return fail(stale)
	}
	t := &hubTransport{name: name, sc: sc, local: local, stop: stop, deadCh: make(chan struct{})}
	go sc.read(nil) // no events until the relay subscribes (roadmap step 8)
	go func() {
		select {
		case <-sc.gone:
		case <-local.dead():
		case <-t.deadCh:
		}
		t.markDead()
	}()
	return t, nil
}

func (t *hubTransport) markDead() { t.deadOnce.Do(func() { close(t.deadCh) }) }

func (t *hubTransport) dead() <-chan struct{} { return t.deadCh }

// forward is add on the hub, then the -L. This host's port is probed first,
// before the hub is asked for anything, so a busy port bumps (errPortBusy)
// without a hub round trip per try; the probe decides busy exactly as the
// ssh transport's does. The hub is asked for the guest port as its own
// preference and never exact: its port is an intermediate hop, and an
// exact callback port matters only here, where the browser is (hub.md §8).
func (t *hubTransport) forward(hostPort, guestPort int) (io.Closer, error) {
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", hostPort))
	if err != nil {
		return nil, errPortBusy
	}
	ln.Close()
	resp, err := t.sc.call(Request{Op: OpAdd, Host: guestPort, Guest: guestPort})
	if err != nil {
		if resp.Err != "" {
			// The hub answered and refused (no free port in its bump range,
			// say). errHubRefused, carrying the hub's words verbatim:
			// restore() leaves this one forward pending for the ticker, like
			// exhaustion, instead of failing the whole attempt and tearing
			// every forward down in a loop; a first add reports it as is.
			return nil, &hubRefusal{msg: resp.Err}
		}
		// No answer at all: the process went, or the hub stopped replying.
		// Either way this relay is done; say so now, so the daemon records
		// the forward pending rather than failed.
		t.markDead()
		return nil, fmt.Errorf("hub add for guest %d: %w", guestPort, err)
	}
	if resp.Pending {
		// The race hub.md §7 names: the hub's transport died after the relay
		// was admitted, and the close that follows is on its way. The -L
		// would point at nothing; treat it as the death it is.
		t.markDead()
		return nil, errHubPending
	}
	c, err := t.local.forwardTo(hostPort, resp.Host)
	if err != nil {
		t.removeOnHub(guestPort)
		return nil, err
	}
	return &hubForward{t: t, guest: guestPort, local: c}, nil
}

// removeOnHub drops this relay's ownership of a guest port on the hub.
// Skipped once the relay is dead: the hub dropped everything it owned when
// the connection went, and a call would only wait on a gone pipe.
// Skipped too once teardown has begun: Close drops the relay, and the hub
// with it every forward the relay owned.
func (t *hubTransport) removeOnHub(guest int) {
	if t.closing.Load() {
		return
	}
	select {
	case <-t.deadCh:
		return
	default:
	}
	_, _ = t.sc.call(Request{Op: OpRemove, Guest: guest})
}

// beginTeardown is teardownAware: the daemon is about to close every forward
// and then this transport.
func (t *hubTransport) beginTeardown() { t.closing.Store(true) }

// Close ends the relay (the hub drops every forward it owned), the process,
// and the master with every -L on it.
func (t *hubTransport) Close() error {
	t.closeOnce.Do(func() {
		t.markDead()
		t.sc.close()
		t.stop()
		_ = t.local.Close()
	})
	return nil
}

// hubForward is one forward's two halves: the -L here and the add on the hub.
type hubForward struct {
	t     *hubTransport
	guest int
	local io.Closer
}

// Close cancels the -L and drops the hub add: one `ports rm`. During
// teardown both are skipped; the master's exit and the relay's close drop
// every -L and every hub forward at once, where N forwards closed one by
// one against a wedged link would each wait out a timeout.
func (f *hubForward) Close() error {
	if f.t.closing.Load() {
		return nil
	}
	err := f.local.Close()
	f.t.removeOnHub(f.guest)
	return err
}

var _ transport = (*hubTransport)(nil)
