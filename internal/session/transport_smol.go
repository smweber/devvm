package session

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/hashicorp/yamux"
	"github.com/smweber/devvm/internal/agentbin"
	"github.com/smweber/devvm/internal/agentrpc"
	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// smolTransport carries every forward for a smol machine over one persistent
// `devvm-agent serve` exec, multiplexed with yamux — the direct successor to
// devvm-mux. A single exec is mandatory: smolvm has poor concurrency across
// separate exec sessions.
type smolTransport struct {
	agent *backend.Session   // owns the exec child tree
	mux   *yamux.Session     // yamux client over the exec's stdio
	fwds  forwardSet         // this transport's listeners; Close drops them all
	evCh  chan bridgeRequest // the guest's open-url requests (events)
	paths agentbin.Paths     // what Install put in the guest (step 8: ping reports the shim)
}

func newSmolTransport(ctx context.Context, m *config.Machine, b backend.Backend) (*smolTransport, error) {
	// The agent and the BROWSER shim, in one install, before the exec that
	// runs the agent (browser-bridge.md §5): a smol box whose daemon is up
	// has both, which is what `attach` relies on the moment it subscribes.
	// smol is managed, so no approval gate (nil) and a root /usr/local/bin
	// install.
	paths, err := agentbin.Install(ctx, b, m, nil)
	if err != nil {
		return nil, err
	}
	// Spawn the agent as the dev user, raw (no login shell) so no banner
	// corrupts the yamux stream.
	agent, err := b.Spawn(ctx, backend.ExecOpts{User: backend.DefaultUser},
		paths.Agent, "serve")
	if err != nil {
		return nil, err
	}
	mux, err := yamux.Client(agentrpc.Stdio{In: agent.Stdout, Out: agent.Stdin}, agentrpc.MuxConfig())
	if err != nil {
		agent.Close()
		return nil, err
	}
	t := &smolTransport{agent: agent, mux: mux, evCh: make(chan bridgeRequest), paths: paths}
	go serveEvents(mux, t.evCh)
	return t, nil
}

// maxEventStreams bounds the guest's event streams in flight at once, from
// accept until the reply is written: each holds a goroutine here, and one
// admitted to the daemon a handler and up to a subscriber's wait. The rate
// limit applies only once an open reaches the daemon, so without this a
// hostile guest could open streams faster than anything refuses them. A
// stream past the bound is closed unread (the shim then prints the URL).
const maxEventStreams = 16

// serveEvents accepts the guest-initiated streams on the agent's session for
// its lifetime and hands each open-url to out, for the daemon's loop. Not
// accepting them at all would queue the streams in yamux forever.
func serveEvents(mux *yamux.Session, out chan<- bridgeRequest) {
	sem := make(chan struct{}, maxEventStreams)
	for {
		stream, err := mux.Accept()
		if err != nil {
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			stream.Close()
			continue
		}
		release := func() { <-sem }
		go serveEventStream(stream, out, mux.CloseChan(), release)
	}
}

// serveEventStream reads one open-url and hands it on with a reply that
// writes the answer to the stream and closes it (the agent is waiting on
// that line, agentrpc's open-url exchange). An old agent closes the stream
// right after its event and reads nothing: the reply's write fails or lands
// on a closed stream within its deadline, so nothing waits on it. A stream
// that is not an open-url is closed. done is the session's end: nobody will
// take the request then. release frees the stream's slot (serveEvents'
// bound) once it is answered or dropped.
func serveEventStream(s net.Conn, out chan<- bridgeRequest, done <-chan struct{}, release func()) {
	var once sync.Once
	finish := func(r *agentrpc.OpenReply) {
		once.Do(func() {
			if r != nil {
				_ = agentrpc.WriteReply(s, *r)
			}
			s.Close()
			release()
		})
	}
	ev, err := agentrpc.ReadEvent(s)
	if err != nil || ev.Type != agentrpc.EventOpenURL || ev.URL == "" {
		finish(nil)
		return
	}
	req := bridgeRequest{url: ev.URL, reply: func(r agentrpc.OpenReply) { finish(&r) }}
	select {
	case out <- req:
	case <-done:
		finish(nil)
	}
}

func (t *smolTransport) events() <-chan bridgeRequest { return t.evCh }

// forward listens on both loopback families (listenLoopback); each accepted
// connection, from either, becomes one yamux stream on the single exec.
func (t *smolTransport) forward(hostPort, guestPort int) (io.Closer, error) {
	lns, err := listenLoopback(hostPort)
	if err != nil {
		return nil, err
	}
	c, err := t.fwds.track(listeners(lns))
	if err != nil {
		return nil, err
	}
	for _, ln := range lns {
		go t.serve(ln, guestPort)
	}
	return c, nil
}

func (t *smolTransport) serve(ln net.Listener, guestPort int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go t.pump(conn, guestPort)
	}
}

// pump opens a yamux stream per accepted connection and splices it to the guest
// port, exactly like devvm-mux's per-connection Open.
func (t *smolTransport) pump(conn net.Conn, guestPort int) {
	stream, err := t.mux.Open()
	if err != nil {
		conn.Close()
		return
	}
	target := fmt.Sprintf("127.0.0.1:%d", guestPort)
	if err := agentrpc.WriteHeader(stream, agentrpc.ForwardHeader(target)); err != nil {
		stream.Close()
		conn.Close()
		return
	}
	agentrpc.Splice(conn, stream)
}

func (t *smolTransport) dead() <-chan struct{} { return t.mux.CloseChan() }

// Close kills the agent exec FIRST, then closes the yamux session. The order
// is load-bearing: yamux's Close blocks until its receive loop sees a read
// error, and agentrpc.Stdio never fails a read on its own (Stdio.Close is a
// no-op) — only the exec dying does. Closing the mux first deadlocked here,
// leaving a "stopped" daemon holding the single agent exec while the next
// `ports up` spawned a second one. (Not unit-testable without smolvm; the
// same gotcha is pinned in the agentrpc tests.) The listeners go last, with
// nothing left to carry their connections.
func (t *smolTransport) Close() error {
	err := t.agent.Close()
	t.mux.Close()
	t.fwds.closeAll()
	return err
}

var _ transport = (*smolTransport)(nil)
