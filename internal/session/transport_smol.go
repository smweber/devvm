package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/hashicorp/yamux"
	"github.com/smweber/devvm/internal/agentbin"
	"github.com/smweber/devvm/internal/agentrpc"
	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/hostbrowser"
)

// smolTransport carries every forward for a smol machine over one persistent
// `devvm-agent serve` exec, multiplexed with yamux — the direct successor to
// devvm-mux. A single exec is mandatory: smolvm has poor concurrency across
// separate exec sessions.
type smolTransport struct {
	agent *backend.Session // owns the exec child tree
	mux   *yamux.Session   // yamux client over the exec's stdio
}

func newSmolTransport(ctx context.Context, m *config.Machine, b backend.Backend) (*smolTransport, error) {
	// smol is managed, so no approval gate (nil) and a root /usr/local/bin install.
	agentPath, err := agentbin.Install(ctx, b, m, nil)
	if err != nil {
		return nil, err
	}
	// Spawn the agent as the dev user, raw (no login shell) so no banner
	// corrupts the yamux stream.
	agent, err := b.Spawn(ctx, backend.ExecOpts{User: backend.DefaultUser},
		agentPath, "serve")
	if err != nil {
		return nil, err
	}
	mux, err := yamux.Client(agentrpc.Stdio{In: agent.Stdout, Out: agent.Stdin}, agentrpc.MuxConfig())
	if err != nil {
		agent.Close()
		return nil, err
	}
	t := &smolTransport{agent: agent, mux: mux}
	go t.drainEvents()
	return t, nil
}

// drainEvents consumes guest-initiated streams for the transport's lifetime.
// The agent pushes open-url events (a login tool run outside `devvm auth`, or
// after an auth session ended); with no auth session to bridge callbacks, the
// best we can do is open the URL on the host. Not accepting these at all would
// queue the streams in yamux forever.
func (t *smolTransport) drainEvents() {
	for {
		stream, err := t.mux.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			br := bufio.NewReader(c)
			header, err := agentrpc.ReadHeader(br)
			if err != nil || header != agentrpc.TypeEvent {
				return
			}
			var ev agentrpc.Event
			if json.NewDecoder(br).Decode(&ev) == nil && ev.Type == agentrpc.EventOpenURL {
				fmt.Fprintf(os.Stderr, "devvm: opening on host -> %s\n", ev.URL)
				hostbrowser.Open(ev.URL)
			}
		}(stream)
	}
}

func (t *smolTransport) forward(hostPort, guestPort int) (io.Closer, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort))
	if err != nil {
		return nil, errPortBusy
	}
	go t.serve(ln, guestPort)
	return ln, nil
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

func (t *smolTransport) Close() error {
	t.mux.Close()
	return t.agent.Close()
}

var _ transport = (*smolTransport)(nil)
