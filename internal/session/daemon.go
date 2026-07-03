package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// idleTimeout is how long the daemon lingers with zero forwards before exiting
// (ControlPersist-style), so a `tunnel down` or last `unport` reaps it.
const idleTimeout = 60 * time.Second

type fwd struct {
	host, guest int
	closer      io.Closer
}

type daemon struct {
	configDir string
	name      string
	tr        transport
	ln        net.Listener

	mu       sync.Mutex
	forwards map[int]*fwd // keyed by guest port

	stopOnce sync.Once
	stop     chan struct{}
}

// RunDaemon is the per-machine daemon entrypoint (the hidden `__daemon`
// command). It owns the transport for the machine's lifetime and serves control
// requests until idle, stopped, or the transport dies.
func RunDaemon(ctx context.Context, configDir string, m *config.Machine, b backend.Backend) error {
	sock := socketPath(configDir, m.Name)
	if err := os.MkdirAll(config.RuntimeDir(configDir), 0o755); err != nil {
		return err
	}
	// If another daemon already owns the socket, defer to it.
	if c, err := net.Dial("unix", sock); err == nil {
		c.Close()
		return nil
	}
	_ = os.Remove(sock) // clear a stale socket

	tr, err := newTransport(ctx, m, b)
	if err != nil {
		return err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		tr.Close()
		return err
	}

	d := &daemon{
		configDir: configDir,
		name:      m.Name,
		tr:        tr,
		ln:        ln,
		forwards:  map[int]*fwd{},
		stop:      make(chan struct{}),
	}
	go d.serveControl()
	d.loop()
	return d.shutdown()
}

// loop supervises the daemon: exit on stop, transport death, or idle.
func (d *daemon) loop() {
	idle := time.NewTimer(idleTimeout)
	defer idle.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-d.tr.dead():
			return
		case <-idle.C:
			if d.count() == 0 {
				return
			}
			idle.Reset(idleTimeout)
		}
	}
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
	d.mu.Lock()
	for _, f := range d.forwards {
		f.closer.Close()
	}
	d.forwards = map[int]*fwd{}
	d.mu.Unlock()
	return d.tr.Close()
}

// add allocates a host port (bumping on conflict, up to +20) and starts the
// forward, mirroring smol_forward_up / ssh_forwards.
func (d *daemon) add(pref, guest int) (host int, bumped bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f, ok := d.forwards[guest]; ok {
		return f.host, false, nil
	}
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
		f.closer.Close()
		delete(d.forwards, guest)
	}
}

func (d *daemon) list() []Forward {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Forward, 0, len(d.forwards))
	for _, f := range d.forwards {
		out = append(out, Forward{Host: f.host, Guest: f.guest})
	}
	return out
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
		host, bumped, err := d.add(req.Host, req.Guest)
		if err != nil {
			return Response{Err: err.Error()}
		}
		return Response{OK: true, Host: host, Bumped: bumped}
	case OpRemove:
		d.remove(req.Guest)
		return Response{OK: true}
	case OpList:
		return Response{OK: true, Forwards: d.list()}
	case OpPing:
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
