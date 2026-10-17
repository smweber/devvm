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
// machine (attach, ports, keys, bootstrap, auth, cp, …). It refuses a hub:
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
// attach, cp, ports, auth, bootstrap, repos and the browser bridge all
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

// errStaleConf is resolveAny's "not found" for an old per-machine smol conf
// whose VM does not exist (there is no dormant state any more). delete
// catches it to drop the conf; everything else just reports it.
type errStaleConf struct{ m *config.Machine }

func (e *errStaleConf) Error() string {
	return fmt.Sprintf("machine '%s' not found: no smol VM by that name (its old conf is stale; 'devvm delete %s' drops it, then 'devvm create %s')",
		e.m.Name, e.m.Name, e.m.Name)
}

// resolveAny loads a machine, a hub, or a HUB/NAME hub-machine record and its
// backend. A name with a registry entry uses that entry (a remote box or a
// hub runs no smolvm command; an entry also shadows a local smol VM of the
// same name, which status reports). Any other name goes to discovery
// (backend.Discover: today, one `smolvm machine ls --json`), whose source
// settles Managed on the record.
//
// HUB/NAME is dispatched here, not in a root interceptor: every leaf already
// calls resolve, and cobra cannot find the first positional before parsing
// (cp-in puts flags first, exec disables flag parsing). It reads the hub's
// conf only — no ssh — so a sleeping hub costs nothing until a command is
// actually run against one of its machines.
func (a *App) resolveAny(name string) (*config.Machine, backend.Backend, error) {
	if _, _, isHub, err := config.SplitHubName(name); err != nil {
		return nil, nil, err
	} else if isHub {
		m, err := config.LoadHubMachine(a.Paths.Config, name)
		if err != nil {
			return nil, nil, err
		}
		b, err := backend.For(m, a.Paths)
		if err != nil {
			return nil, nil, err
		}
		return m, b, nil
	}
	m, err := config.Load(a.Paths.Config, name)
	if errors.Is(err, config.ErrNotFound) {
		if err2 := config.ValidName(name); err2 != nil {
			return nil, nil, err2
		}
		d, ok, err := backend.Discover(a.Paths, name)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, fmt.Errorf("unknown machine %q; run 'devvm status', or 'devvm create %s' to make it", name, name)
		}
		return d.Machine, d.Backend, nil
	} else if err != nil {
		return nil, nil, err
	}
	if !m.IsRemote() {
		// An old per-machine smol conf (step 5 drops them): the VM is still
		// smolvm's, so the smol source decides whether it exists and settles
		// Managed, with the conf as its migration hint.
		b, ok, err := backend.ResolveSmolConf(a.Paths, m)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, &errStaleConf{m: m}
		}
		return m, b, nil
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
