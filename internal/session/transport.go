package session

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// errPortBusy signals that a host port could not be bound, so the allocator
// should bump to the next one.
var errPortBusy = errors.New("host port busy")

// transport is the backend-specific carrier for a machine's forwards. smol
// multiplexes them over the agent's yamux session; ssh adds native -L forwards
// to a dedicated ControlMaster. dead is closed when the underlying channel dies
// (VM stopped / ssh dropped) so the daemon can shut down.
type transport interface {
	// forward binds 127.0.0.1:hostPort and carries connections to the guest's
	// 127.0.0.1:guestPort. Returns errPortBusy if hostPort can't be bound.
	forward(hostPort, guestPort int) (io.Closer, error)
	dead() <-chan struct{}
	Close() error
}

// newTransport builds the right transport for a resolved machine.
func newTransport(ctx context.Context, m *config.Machine, b backend.Backend) (transport, error) {
	switch m.Backend {
	case config.BackendSmol:
		return newSmolTransport(ctx, b)
	case config.BackendSSH:
		conn, ok := b.(backend.SSHConnector)
		if !ok {
			return nil, fmt.Errorf("ssh backend does not expose a connector")
		}
		return newSSHTransport(conn.SSHConn())
	default:
		return nil, fmt.Errorf("no forward transport for backend %q", m.Backend)
	}
}
