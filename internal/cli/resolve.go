package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// resolve loads a machine and its backend for the verbs that act *on* a
// machine (attach, ports, keys, setup, auth, cp, …). It refuses a hub:
// a hub is reached like a remote box but is never shaped, so every one of
// those verbs fails here, in one place, with the same message. It also
// refuses a local smol VM devvm did not create (requireManaged): only
// status, start, stop, shell and exec reach one of those (resolveUnmanaged),
// and `delete` refuses it on its own. `status` and `delete`, which do apply
// to a hub, use resolveAny.
func (a *App) resolve(name string) (*config.Machine, backend.Backend, error) {
	m, b, err := a.resolveUnmanaged(name)
	if err != nil {
		return nil, nil, err
	}
	if err := requireManaged(m); err != nil {
		return nil, nil, err
	}
	return m, b, nil
}

// resolveUnmanaged is resolve without the managed gate, for the few verbs
// an unmanaged smol VM supports (config.md §2): status, start, stop, and
// shell/exec as smolvm's own default user. The backend itself keeps those
// hands-off (plain `machine exec`, nothing installed).
func (a *App) resolveUnmanaged(name string) (*config.Machine, backend.Backend, error) {
	m, b, err := a.resolveAny(name)
	if err != nil {
		return nil, nil, err
	}
	if err := requireMachine(m); err != nil {
		return nil, nil, err
	}
	if m.IsHubMachine() {
		// The proxied leaves never get here: their RunE dispatches HUB/NAME
		// to a.proxy before the local handler runs (proxy.go), cp-in/cp-out
		// to their tar-over-stdio loops (cp_hub.go), and ports/__daemon
		// resolve through resolveForwards (hubforwards.go: this host's own
		// forwards, over a __session, never an agent exec), and auth to
		// authHubMachine. So this is the guard for everything else, which
		// would otherwise act on the hub itself with the machine's record.
		return nil, nil, fmt.Errorf("%s: %w", name, backend.ErrHubProxy)
	}
	return m, b, nil
}

// requireMachine is the shared shaping-verb guard: the one message every verb
// prints when handed a hub instead of a machine.
func requireMachine(m *config.Machine) error {
	if m.IsHub() {
		return fmt.Errorf("%s is a hub, not a machine", m.Name)
	}
	return nil
}

// requireManaged refuses a local VM devvm did not create (config.md §2):
// attach, cp, ports, auth, setup, repos and the browser bridge all
// assume devvm's `dev` user, tmux socket and agent, none of which such a VM
// has, and none of which devvm may put there unasked. Remote boxes are
// never refused here: an adopted remote host has its own, consented,
// posture (agentInstallApprover). A machine on a hub is refused by its
// hub, which holds the marker.
func requireManaged(m *config.Machine) error {
	if m.IsRemote() || m.Managed() {
		return nil
	}
	return fmt.Errorf("'%s' is a smol VM devvm did not create; %s", m.Name, backend.AdoptHint(m.Name))
}

// resolveAny loads a machine, a hub, or a HUB/NAME hub-machine record and its
// backend, in this order (config.md §2):
//
//   - HUB/NAME goes to the hub: the record is derived from the hub's entry;
//   - a `[machines.NAME]` entry wins: a remote box or a hub runs no smolvm
//     command, and the entry shadows a local smol VM of the same name
//     (status reports that);
//   - any other name goes to discovery (backend.Discover: today, one
//     `smolvm machine ls --json`), whose source settles Managed.
//
// A devvm.toml that does not parse fails every name: an entry would win,
// and a broken file cannot say whether one does.
//
// HUB/NAME is dispatched here, not in a root interceptor: every leaf already
// calls resolve, and cobra cannot find the first positional before parsing
// (cp-in puts flags first, exec disables flag parsing). It reads the file
// only — no ssh — so a sleeping hub costs nothing until a command is
// actually run against one of its machines.
func (a *App) resolveAny(name string) (*config.Machine, backend.Backend, error) {
	if _, _, isHub, err := config.SplitHubName(name); err != nil {
		return nil, nil, err
	} else if isHub {
		m, err := config.LoadHubMachine(a.Paths, name)
		if err != nil {
			return nil, nil, err
		}
		b, err := backend.For(m, a.Paths)
		if err != nil {
			return nil, nil, err
		}
		return m, b, nil
	}
	m, err := config.Load(a.Paths, name)
	if errors.Is(err, config.ErrNotFound) {
		d, ok, err := backend.Discover(a.Paths, name)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, fmt.Errorf("unknown machine %q; run 'devvm status', 'devvm create %s' to make a smol VM, or 'devvm config add %s' for a remote box", name, name, name)
		}
		return d.Machine, d.Backend, nil
	} else if err != nil {
		return nil, nil, err
	}
	b, err := backend.For(m, a.Paths)
	if err != nil {
		return nil, nil, err
	}
	return m, b, nil
}

var yesRe = regexp.MustCompile(`^[Yy]([Ee][Ss])?$`)

// confirm prompts on the controlling terminal, matching the old confirm().
func confirm(prompt string) (bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, errors.New("no terminal for confirmation; aborting")
	}
	defer tty.Close()
	fmt.Fprintf(tty, "%s [y/N] ", prompt)
	line, _ := bufio.NewReader(tty).ReadString('\n')
	return yesRe.MatchString(trimNL(line)), nil
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
