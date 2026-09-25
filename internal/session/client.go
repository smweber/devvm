package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
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
	return dialCancel(configDir, name, nil)
}

// errDialCanceled is returned when a dial's come-up wait was abandoned.
var errDialCanceled = errors.New("dial canceled")

// dialCancel is Dial whose come-up wait gives up as soon as cancel closes
// (nil never does), so a session being closed is not held for the whole
// wait by a reconnect in progress. A daemon already spawned carries on and
// idles out on its own.
func dialCancel(configDir, name string, cancel <-chan struct{}) (*Client, error) {
	c := &Client{configDir: configDir, name: name}
	if c.alive() {
		return c, nil
	}
	logOff := logSize(logPath(configDir, name))
	exited, err := c.spawnDaemon()
	if err != nil {
		return nil, err
	}
	return c.waitComeUp(exited, logOff, cancel, time.Now().Add(comeUpWait(name)))
}

// comeUpWait is how long Dial waits for a spawned daemon to listen. The
// daemon listens only after its first transport dial succeeds, and on ssh
// that dial is bounded by ConnectTimeout (plus auth), so the wait must
// outlast it or a slow host reads as "did not come up" while the daemon in
// fact arrives moments later, holds no forwards, and idles out. A hub
// machine's first dial is that ssh master, then `__session` on the hub up
// to its marker and the relay's open reply, which share one deadline
// (hubOpenTimeout; the hub's own Dial of its daemon runs inside it), plus
// slack for the hub's login shell: about 75s at the defaults.
func comeUpWait(name string) time.Duration {
	connect := time.Duration(backend.SSHConnectTimeout()) * time.Second
	if connect > 60*time.Second {
		connect = 60 * time.Second // an env override must not turn this into a minutes-long hang
	}
	wait := connect + 10*time.Second
	if config.RuntimeName(name) != name {
		wait += hubOpenTimeout + 10*time.Second
	}
	return wait
}

// waitComeUp polls until the spawned daemon answers, cancel closes, the
// deadline passes, or the daemon exits with an error before listening (a
// stopped VM, a refused relay, an unreachable host): then it says why at
// once, quoting what the daemon logged after logOff (so an earlier run's
// line is never mistaken for this one's). A clean exit is a daemon that
// found another one already serving the socket; polling goes on for that.
func (c *Client) waitComeUp(exited <-chan daemonExit, logOff int64, cancel <-chan struct{}, deadline time.Time) (*Client, error) {
	log := logPath(c.configDir, c.name)
	for time.Now().Before(deadline) {
		if c.alive() {
			return c, nil
		}
		select {
		case <-cancel:
			return nil, errDialCanceled
		case res := <-exited:
			if res.err != nil {
				if c.alive() {
					return c, nil
				}
				return nil, fmt.Errorf("forward daemon for '%s' exited: %s (see %s)", c.name, lastLogLine(log, logOff, res.err), log)
			}
			exited = nil
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("forward daemon for '%s' did not come up (see %s)", c.name, log)
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
// daemonExit is how a spawned daemon process ended.
type daemonExit struct{ err error }

// The returned channel yields once, when the spawned process exits (a
// daemon serving normally never does while the caller waits); Dial uses it
// to fail fast on a daemon that gave up before listening.
func (c *Client) spawnDaemon() (<-chan daemonExit, error) {
	// An empty name would spawn `__daemon ''`. Under any binary other than
	// devvm (a scratch tool reusing this package) that re-exec parses as a
	// fresh run with no machine, dials "" again and recurses into a fork
	// bomb, which took down a 2 GB test guest twice.
	if c.name == "" {
		return nil, errors.New("no machine name for the forward daemon")
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := config.EnsureRuntimeDir(c.configDir); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(logPath(c.configDir, c.name),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	cmd := exec.Command(self, "__daemon", c.name, "--config-dir", c.configDir)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from the CLI
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Reap it (no zombie while this process lives on, e.g. a __session or
	// a session client) and report how it ended.
	exited := make(chan daemonExit, 1)
	go func() { exited <- daemonExit{cmd.Wait()} }()
	return exited, nil
}

// logSize is the log's size now (0 if there is none): where a daemon about
// to be spawned starts writing.
func logSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

// lastLogLine is the last non-empty line a daemon wrote to its log after
// offset off (where a daemon that exits early prints its error), or
// fallback's text when it wrote none.
func lastLogLine(path string, off int64, fallback error) string {
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) <= off {
		return fallback.Error()
	}
	lines := strings.Split(strings.TrimRight(string(data[off:]), "\n"), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return strings.TrimPrefix(last, "devvm: ")
	}
	return fallback.Error()
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

// Remove drops the conf owner of a guest port's forward (`ports rm` on a
// configured mapping); the forward goes when no other owner holds it.
func (c *Client) Remove(guest int) error {
	_, err := c.RemoveOwner(guest, OwnerConf)
	return err
}

// RemoveOwner drops one owner (OwnerConf or OwnerTTL) of a guest port's
// forward. left is the forward as it stands afterwards if another owner
// still holds it, nil if it is gone (or never existed). A daemon older than
// owners removes the forward outright and reports nothing left.
func (c *Client) RemoveOwner(guest int, owner string) (left *Forward, err error) {
	resp, err := c.request(Request{Op: OpRemove, Guest: guest, Owner: owner})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Err)
	}
	if len(resp.Forwards) > 0 {
		return &resp.Forwards[0], nil
	}
	return nil, nil
}

// DownResult is what `ports down` did: Stopped when nothing else held the
// daemon, else the forwards other owners keep and the sessions holding it.
type DownResult struct {
	Stopped  bool
	Forwards []Forward
	Sessions int
}

// Down drops every conf owner and stops the daemon only if no forward and no
// session remains (browser-bridge.md §4). A daemon from before `down`
// existed answers "unknown op"; it predates owners and sessions too, so
// stopping it outright is exactly the old `ports down`.
func (c *Client) Down() (DownResult, error) {
	resp, err := c.request(Request{Op: OpDown})
	if err != nil {
		return DownResult{}, err
	}
	if !resp.OK {
		if strings.HasPrefix(resp.Err, "unknown op") {
			return DownResult{Stopped: true}, c.Stop()
		}
		return DownResult{}, errors.New(resp.Err)
	}
	return DownResult{Stopped: resp.Stopped, Forwards: resp.Forwards, Sessions: resp.Sessions}, nil
}

// Status is a daemon snapshot: its connection state, when that state began,
// and every forward it owns (live, or pending while reconnecting).
type Status struct {
	State    string
	Since    time.Time
	Version  string // build the daemon is running; "" from a pre-version daemon
	Sessions int    // open session connections holding the daemon
	Forwards []Forward
}

// ConfCount is how many forwards a conf owner holds: the N of `up:N` in
// `status --plain` (browser-bridge.md §8), which never counts forwards only
// a session or a ttl holds.
func (s Status) ConfCount() int {
	n := 0
	for _, f := range s.Forwards {
		if f.IsConf() {
			n++
		}
	}
	return n
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
	return Status{State: resp.State, Since: resp.Since, Version: resp.Version, Sessions: resp.Sessions, Forwards: resp.Forwards}, nil
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
