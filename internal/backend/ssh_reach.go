package backend

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/smweber/devvm/internal/reach"
)

// Prober is a backend whose reachability can be probed: the two remote-*
// backends. A smol VM's state is smolvm's listing and a hub machine's is
// its hub's, so neither is one.
type Prober interface {
	// Probe establishes the box's reach now, writes it to the reach cache
	// and returns it. Bounded by ctx; a probe the deadline cuts short is
	// `unreachable` ("timed out").
	Probe(ctx context.Context) reach.Entry
}

// ProbeDialTimeout bounds the probe's TCP connect. Fixed, not
// DEVVM_SSH_CONNECT_TIMEOUT: that one is sized for ssh's banner exchange
// on a slow host, while a SYN-ACK either comes within a couple of seconds
// or the box is, for a human looking at a menu, not there.
const ProbeDialTimeout = 2 * time.Second

// ProbeBudget is the whole budget of one box's probe when nothing bounds
// it already (the single-machine `status NAME` through Status); the
// listing's round has its own deadline across every box.
const ProbeBudget = 5 * time.Second

// probeDial is the TCP connect, a var so tests can inject a dialer that
// never answers instead of relying on an unroutable address.
var probeDial = func(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: ProbeDialTimeout}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

// Probe is providers.md §5's probe, and deliberately never a full ssh: no
// handshake, no auth, no remote process, so it costs milliseconds against
// a live master and at most ProbeDialTimeout otherwise, and it cannot hang
// on a prompt or a slow login shell. In order:
//
//  1. `ssh -O check` against the box's ControlMasters (the daemon's, which
//     it holds whenever the box has forwards, then the shared one
//     interactive calls use). Local, and a live master means the box
//     answered ssh's last keepalive.
//  2. `ssh -G` to resolve the alias as ssh would (HostName, Port); an alias
//     with ProxyJump/ProxyCommand is not dialled, since the dial would test
//     the wrong host, and stays `?`.
//  3. A TCP connect to the resolved host:port.
func (b *sshBackend) Probe(ctx context.Context) reach.Entry {
	e := b.probe(ctx)
	_ = reach.Write(b.paths, b.m.Name, e) // derived data; a failed write is not a failed probe
	return e
}

func (b *sshBackend) probe(ctx context.Context) reach.Entry {
	timedOut := func() reach.Entry {
		return reach.Entry{State: reach.Unreachable, Verified: time.Now(), How: reach.HowProbe, Reason: "timed out"}
	}
	if b.masterAlive(ctx) {
		return reach.Entry{State: reach.Reachable, Verified: time.Now(), How: reach.HowMaster}
	}
	if ctx.Err() != nil {
		return timedOut()
	}
	host, port, proxy, err := b.resolveDest(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return timedOut()
		}
		return reach.Entry{State: reach.Unknown, Reason: "ssh -G failed: " + err.Error()}
	}
	if proxy != "" {
		return reach.Entry{State: reach.Unknown, Reason: proxy}
	}
	if err := probeDial(ctx, net.JoinHostPort(host, port)); err != nil {
		return reach.Entry{State: reach.Unreachable, Verified: time.Now(), How: reach.HowProbe, Reason: dialReason(err, host)}
	}
	return reach.Entry{State: reach.Reachable, Verified: time.Now(), How: reach.HowProbe}
}

// masterAlive asks each of the box's ControlMaster sockets whether a
// master answers. The daemon's is skipped when its socket is absent (no
// process for the common case of no forwards); the shared one's name is
// ssh's %C hash, so ssh is asked and finds out itself. The flags are
// sshFlags', so the %C (host, port, user) is the one the masters used.
func (b *sshBackend) masterAlive(ctx context.Context) bool {
	paths := []string{b.paths.ControlPath()}
	if daemon := b.SSHConn().ControlPath; fileExists(daemon) {
		paths = append([]string{daemon}, paths...)
	}
	for _, cp := range paths {
		args := append(b.sshFlags(), "-O", "check", "-o", "ControlPath="+cp, b.m.SSHHost)
		if probeCmd(ctx, args, io.Discard) == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
	}
	return false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// resolveDest is `ssh -G HOST` read for where ssh would connect: hostname,
// port, and the proxy option that makes a direct dial meaningless ("" when
// none). The flags go first so a conf's port wins as it does for ssh.
func (b *sshBackend) resolveDest(ctx context.Context) (host, port, proxy string, err error) {
	var out strings.Builder
	if err := probeCmd(ctx, append(b.sshFlags(), "-G", b.m.SSHHost), &out); err != nil {
		return "", "", "", err
	}
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		k, v, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		v = strings.TrimSpace(v)
		switch strings.ToLower(k) {
		case "hostname":
			host = v
		case "port":
			port = v
		case "proxyjump":
			if v != "" && v != "none" && proxy == "" {
				proxy = "ProxyJump"
			}
		case "proxycommand":
			if v != "" && v != "none" && proxy == "" {
				proxy = "ProxyCommand"
			}
		}
	}
	if host == "" {
		return "", "", "", fmt.Errorf("no hostname for %s", b.m.SSHHost)
	}
	if port == "" {
		port = "22"
	}
	return host, port, proxy, nil
}

// probeCmd runs one local ssh for the probe, its stderr dropped (a missing
// control socket is the ordinary answer, not news), bounded by ctx with
// WaitDelay so a killed ssh never holds the call on an inherited pipe.
func probeCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if err := needCmd("ssh"); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, stdout, io.Discard
	cmd.WaitDelay = HostWaitDelay
	return cmd.Run()
}

// dialReason is a failed connect in the words `status -v` shows.
func dialReason(err error, host string) string {
	var dns *net.DNSError
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "connect timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "no route to host"
	case errors.As(err, &dns):
		return "cannot resolve " + host
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return op.Err.Error()
	}
	return err.Error()
}
