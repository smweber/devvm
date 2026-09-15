package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// ErrNoDaemon is returned by Existing when no daemon is running for a machine.
var ErrNoDaemon = errors.New("no forward daemon running")

// Client talks to a machine's forward daemon over its unix control socket.
type Client struct {
	configDir string
	name      string
}

// Dial returns a client, spawning the daemon first if none is running.
func Dial(configDir, name string) (*Client, error) {
	c := &Client{configDir: configDir, name: name}
	if c.alive() {
		return c, nil
	}
	if err := c.spawnDaemon(); err != nil {
		return nil, err
	}
	// The daemon listens only after its first transport dial succeeds, and
	// on ssh that dial is bounded by ConnectTimeout (plus auth), so the
	// come-up wait must outlast it or a slow host reads as "did not come up"
	// while the daemon in fact arrives moments later, holds no forwards, and
	// idles out.
	deadline := time.Now().Add(time.Duration(backend.SSHConnectTimeout())*time.Second + 10*time.Second)
	for time.Now().Before(deadline) {
		if c.alive() {
			return c, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("forward daemon for '%s' did not come up (see %s)",
		name, logPath(configDir, name))
}

// Existing returns a client only if a daemon is already running, else ErrNoDaemon.
func Existing(configDir, name string) (*Client, error) {
	c := &Client{configDir: configDir, name: name}
	if !c.alive() {
		return nil, ErrNoDaemon
	}
	return c, nil
}

func (c *Client) alive() bool {
	resp, err := c.request(Request{Op: OpPing})
	return err == nil && resp.OK
}

// spawnDaemon re-execs devvm as a detached `__daemon` process. The daemon
// resolves the machine itself and owns the transport thereafter.
func (c *Client) spawnDaemon() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := config.EnsureRuntimeDir(c.configDir); err != nil {
		return err
	}
	logf, err := os.OpenFile(logPath(c.configDir, c.name),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "__daemon", c.name, "--config-dir", c.configDir)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from the CLI
	return cmd.Start()
}

func (c *Client) request(req Request) (Response, error) {
	conn, err := net.Dial("unix", socketPath(c.configDir, c.name))
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return Response{}, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}

// Add brings up a forward, returning the actual host port (after any bump).
// pending reports that the daemon is reconnecting and has only recorded the
// forward; it comes up, at host if still free, once the transport is back.
func (c *Client) Add(pref, guest int) (host int, bumped, pending bool, err error) {
	resp, err := c.request(Request{Op: OpAdd, Host: pref, Guest: guest})
	if err != nil {
		return 0, false, false, err
	}
	if !resp.OK {
		return 0, false, false, errors.New(resp.Err)
	}
	return resp.Host, resp.Bumped, resp.Pending, nil
}

// Remove tears down the forward for a guest port.
func (c *Client) Remove(guest int) error {
	resp, err := c.request(Request{Op: OpRemove, Guest: guest})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Err)
	}
	return nil
}

// Status is a daemon snapshot: its connection state, when that state began,
// and every forward it owns (live, or pending while reconnecting).
type Status struct {
	State    string
	Since    time.Time
	Version  string // build the daemon is running; "" from a pre-version daemon
	Forwards []Forward
}

// Reconnecting reports whether the daemon is between transports.
func (s Status) Reconnecting() bool { return s.State == StateReconnecting }

// Status returns the daemon's state and forwards.
func (c *Client) Status() (Status, error) {
	resp, err := c.request(Request{Op: OpList})
	if err != nil {
		return Status{}, err
	}
	if !resp.OK {
		return Status{}, errors.New(resp.Err)
	}
	return Status{State: resp.State, Since: resp.Since, Version: resp.Version, Forwards: resp.Forwards}, nil
}

// Kick asks a reconnecting daemon to retry now (e.g. after `devvm start`
// brought the VM back) instead of waiting out its backoff. No-op when up.
func (c *Client) Kick() error {
	resp, err := c.request(Request{Op: OpKick})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Err)
	}
	return nil
}

// List returns the daemon's forwards (live or pending).
func (c *Client) List() ([]Forward, error) {
	st, err := c.Status()
	return st.Forwards, err
}

// Stop asks the daemon to exit (reaping all forwards).
func (c *Client) Stop() error {
	_, err := c.request(Request{Op: OpStop})
	return err
}

// WaitGone blocks until no daemon answers for the machine and its socket is
// unlinked, or timeout elapses (reporting whether it is gone). Stop returns as
// soon as the daemon has taken the request; the daemon then closes its
// forwards and transport and unlinks the socket last (see daemon.shutdown),
// so a missing socket means the old transport is released too. A replacement
// spawned earlier could listen on the same path and have its fresh socket
// unlinked, or find the old ssh master still holding the ControlPath. Callers
// cycling a daemon (update) wait here between Stop and Dial.
func WaitGone(configDir, name string, timeout time.Duration) bool {
	c := &Client{configDir: configDir, name: name}
	deadline := time.Now().Add(timeout)
	for {
		_, statErr := os.Stat(socketPath(configDir, name))
		if os.IsNotExist(statErr) && !c.alive() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
