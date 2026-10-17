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

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/reach"
)

// Prober is a backend whose reachability can be probed: the two remote-*
// backends. A smol VM's state is smolvm's listing and a hub machine's is
// its hub's, so neither is one.
type Prober interface {
	// Probe establishes the box's reach now, writes it to the reach cache
	// and returns it. Bounded by ctx; a probe the deadline cuts short
	// before it tested anything for its full dial bound claims nothing
	// and returns the cached entry unchanged.
	Probe(ctx context.Context) reach.Entry
}

// ProbeDialTimeout bounds the probe's TCP connect by default. Not
// DEVVM_SSH_CONNECT_TIMEOUT: that one is sized for ssh's banner exchange
// on a slow host, while a SYN-ACK usually comes within a couple of
// seconds. An entry's own connect_timeout (a box behind a relayed or
// waking first hop, e.g. Tailscale's DERP) replaces it, capped at
// ProbeBudget: see probeDialTimeout.
const ProbeDialTimeout = 2 * time.Second

// probeDialTimeout is the dial bound for one box: its entry's
// connect_timeout when it sets one, capped at ProbeBudget (the round's
// own deadline, which bounds the dial anyway), else ProbeDialTimeout.
func probeDialTimeout(m *config.Machine) time.Duration {
	if m == nil || m.ConnectTimeout <= 0 {
		return ProbeDialTimeout
	}
	return min(time.Duration(m.ConnectTimeout)*time.Second, ProbeBudget)
}

// ProbeBudget is the whole budget of one box's probe when nothing bounds
// it already (the single-machine `status NAME` through Status); the
// listing's round has its own deadline across every box.
const ProbeBudget = 5 * time.Second

// probeDial is the TCP connect, a var so tests can inject a dialer that
// never answers instead of relying on an unroutable address.
var probeDial = func(ctx context.Context, addr string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

// Probe is providers.md §5's probe, and deliberately never a full ssh: no
// ssh connection, no auth, nothing run on the box, so it costs
// milliseconds against a live master and at most the dial bound
// (probeDialTimeout) otherwise, and it cannot hang on a prompt or a slow login shell. (`ssh
// -G` is local but may run a `Match exec` from the user's ssh config; it
// is bounded by ctx like the rest.) In order:
//
//  1. `ssh -O check` against the box's ControlMasters (the daemon's, which
//     it holds whenever the box has forwards, then the shared one
//     interactive calls use). Local, and a live master means the box
//     answered ssh's last keepalive.
//  2. `ssh -G` to resolve the alias as ssh would (HostName, Port); an alias
//     with ProxyJump/ProxyCommand is not dialled, since the dial would test
//     the wrong host, and stays `?`.
//  3. A TCP connect to the resolved host:port. Refused, unroutable or
//     unresolvable is `unreachable`; a connect that timed out is `?` with
//     the reason (How = probe), since a slow box (a Tailscale peer still
//     on a relay) is not a dead one. That `?` replaces what was cached:
//     the box was tested, and it did not answer in time.
//
// What it writes is only what it established. A probe the round's
// deadline cut short (the master check or `-G` killed, or a dial that had
// less than its dial bound to answer, as a box that got a late slot
// does) tested nothing and leaves the cache as it was. A `?` (not
// probeable) never overwrites a state a command or a master check
// recorded: for an alias behind ProxyJump that record is the only
// evidence there is, and "not probed" says nothing against it.
func (b *sshBackend) Probe(ctx context.Context) reach.Entry {
	e, ok := b.probe(ctx)
	if !ok {
		return reach.Read(b.paths, b.m.Name)
	}
	if e.State == reach.Unknown && e.How != reach.HowProbe {
		if prev := reach.Read(b.paths, b.m.Name); prev.State != reach.Unknown {
			return prev
		}
	}
	_ = reach.Write(b.paths, b.m.Name, e) // derived data; a failed write is not a failed probe
	return e
}

// probe returns the entry it established, and false when ctx ended it
// before it established anything.
func (b *sshBackend) probe(ctx context.Context) (reach.Entry, bool) {
	if b.masterAlive(ctx) {
		return reach.Entry{State: reach.Reachable, Verified: time.Now(), How: reach.HowMaster}, true
	}
	if ctx.Err() != nil {
		return reach.Entry{}, false
	}
	host, port, proxy, err := b.resolveDest(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return reach.Entry{}, false
		}
		return reach.Entry{State: reach.Unknown, Reason: "ssh -G failed: " + err.Error()}, true
	}
	if proxy != "" {
		return reach.Entry{State: reach.Unknown, Reason: proxy}, true
	}
	start, bound := time.Now(), probeDialTimeout(b.m)
	if err := probeDial(ctx, net.JoinHostPort(host, port), bound); err != nil {
		if ctx.Err() != nil && time.Since(start) < bound {
			return reach.Entry{}, false // cut short by the round, not timed out by the box
		}
		if dialTimedOut(err) {
			return reach.Entry{State: reach.Unknown, How: reach.HowProbe, Reason: dialReason(err, host)}, true
		}
		return reach.Entry{State: reach.Unreachable, Verified: time.Now(), How: reach.HowProbe, Reason: dialReason(err, host)}, true
	}
	return reach.Entry{State: reach.Reachable, Verified: time.Now(), How: reach.HowProbe}, true
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

// dialTimedOut is a connect that got no answer in time, as opposed to
// one refused or unroutable.
func dialTimedOut(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
}

// dialReason is a failed connect in the words `status -v` shows.
func dialReason(err error, host string) string {
	var dns *net.DNSError
	switch {
	case dialTimedOut(err):
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
