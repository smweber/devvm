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
	for i := 0; i < 50; i++ {
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
	if err := os.MkdirAll(config.RuntimeDir(c.configDir), 0o755); err != nil {
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
func (c *Client) Add(pref, guest int) (host int, bumped bool, err error) {
	resp, err := c.request(Request{Op: OpAdd, Host: pref, Guest: guest})
	if err != nil {
		return 0, false, err
	}
	if !resp.OK {
		return 0, false, errors.New(resp.Err)
	}
	return resp.Host, resp.Bumped, nil
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

// List returns the live forwards.
func (c *Client) List() ([]Forward, error) {
	resp, err := c.request(Request{Op: OpList})
	if err != nil {
		return nil, err
	}
	return resp.Forwards, nil
}

// Stop asks the daemon to exit (reaping all forwards).
func (c *Client) Stop() error {
	_, err := c.request(Request{Op: OpStop})
	return err
}
