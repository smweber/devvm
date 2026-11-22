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
// machine (attach, shell, exec, ports, keys, setup, cp, start, stop, …).
// It refuses a hub: a hub is reached like a remote box but is never
// shaped, so every one of those verbs fails here, in one place, with the
// same message. Every machine it returns is either an entry or one devvm
// made: a smol VM devvm did not create is not found at all (resolveAny).
// `status` and `delete`, which do apply to a hub, use resolveAny.
func (a *App) resolve(name string) (*config.Machine, backend.Backend, error) {
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
		// forwards, over a __session, never an agent exec). So this is the
		// guard for everything else, which would otherwise act on the hub
		// itself with the machine's record.
		return nil, nil, fmt.Errorf("%s: %w", name, backend.ErrHubProxy)
	}
	if m.IsProvider() && m.Server.Address == "" {
		return nil, nil, fmt.Errorf("'%s' is a %s server with no public address (status %s): devvm reaches it over ssh only", name, providerTitle(m.Backend), m.Server.Status)
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

// resolveAny loads a machine, a hub, or a HUB/NAME hub-machine record and its
// backend, in this order (config.md §2):
//
//   - HUB/NAME goes to the hub: the record is derived from the hub's entry;
//   - a `[machines.NAME]` entry wins: a remote box or a hub runs no smolvm
//     command, and the entry shadows a local smol VM of the same name
//     (status reports that);
//   - any other name goes to discovery (backend.DiscoverSmol: one
//     `smolvm machine ls --json`), which knows only the VMs with devvm's
//     label (a smol VM devvm did not create is invisible to it);
//   - and only then each configured provider (providerLookup: one API Get
//     per provider with a token), whose label settles Managed. An entry or
//     a smol VM therefore shadows a provider server of the same name.
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
	if err := config.ValidName(name); err != nil {
		return nil, nil, err
	}
	f, err := config.LoadFile(a.Paths)
	if err != nil {
		return nil, nil, err
	}
	m, ok := f.Entry(name)
	if !ok {
		dm, db, ok, err := backend.DiscoverSmol(a.Paths, name)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			return dm, db, nil
		}
		// Last, the providers (providers.md §2): one API call each, only
		// now that every local source has missed.
		pm, hint, err := a.providerLookup(f, name, a.providerMode)
		if err != nil {
			return nil, nil, err
		}
		if pm == nil {
			// Invisible is not unknown: a VM of that name without devvm's
			// label is the user's to remove or rename (one more listing,
			// on this failure path only).
			if backend.SmolExists(name) {
				return nil, nil, fmt.Errorf("a smol VM named %s exists, but devvm did not create it; if an older devvm made it, 'smolvm machine update --name %s --label devvm=true' makes it devvm's", name, name)
			}
			msg := fmt.Sprintf("unknown machine %q; run 'devvm status', 'devvm create %s' to make a smol VM, or 'devvm config add %s' for a remote box", name, name, name)
			if hint != "" {
				msg += " (" + hint + ")"
			}
			return nil, nil, errors.New(msg)
		}
		m = pm
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
