package cli

import (
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// Hub forwards (docs/proposals/hub.md §7, roadmap step 6). `ports … HUB/NAME`
// runs here, on this host: the forwards are this host's own, inherently per
// client, so they are saved in this host's ports file for HUB@NAME and
// carried by this host's daemon for HUB@NAME, whose hubTransport holds one
// `__session NAME` on the hub. The hub's daemon owns the only agent exec;
// this host never spawns one into a hub VM.

// sessionCmd is the hidden hub side of a hub forward: one per hub machine
// per client daemon, held for the relay's life.
func (a *App) sessionCmd() *cobra.Command {
	return &cobra.Command{
		Use:    backend.SessionCmd + " NAME",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE:   func(cmd *cobra.Command, args []string) error { return a.runSession(args[0]) },
	}
}

// runSession dials (spawning if needed) this host's daemon for NAME, prints
// the marker, and relays its stdio to that connection verbatim
// (session.Relay). The laptop opens the connection as `session {relay:
// true}` itself, so every forward it adds is owned by this process's
// connection and goes when it does.
//
// A stopped VM is refused before any dial, as `ports up` refuses it:
// session.Dial would spawn a daemon whose first transport dial execs into
// the VM, and smolvm's exec may boot a machine the user stopped on purpose.
// So a `stop web` here puts the laptop in `reconnecting` and its backoff
// retries land here, one cheap refusal each, never a boot loop.
func (a *App) runSession(name string) error {
	if ok, err := hubMachineArg([]string{name}); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%s is the hub side of hub forwards and takes a machine on this host, not %s (hubs do not chain)", backend.SessionCmd, name)
	}
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if err := requireRunningForForwards(m, b, 0); err != nil {
		return err
	}
	conn, err := session.DialConn(a.Paths, name)
	if err != nil {
		return err
	}
	return session.Relay(conn, a.stdin(), a.Stdout)
}

// resolveForwards is resolve for the ports verbs, which also act on a
// machine on a hub: their forwards are this host's (hub.md §7). A HUB/NAME
// resolves from the hub conf alone, never dialing; whether the machine
// exists and runs is the hub's answer when the daemon's __session starts.
func (a *App) resolveForwards(name string) (*config.Machine, backend.Backend, error) {
	if ok, err := hubMachineArg([]string{name}); err != nil {
		return nil, nil, err
	} else if ok {
		return a.resolveAny(name)
	}
	return a.resolve(name)
}

// savedPorts is a machine's saved forwards on this host (config.LoadPorts),
// the list `start` and `ports up` bring up. It is the only place the CLI
// reads them: the daemon never does, it gets each forward from a client.
func (a *App) savedPorts(name string) ([]string, error) {
	return config.LoadPorts(a.Paths, name)
}

// editPorts rewrites a machine's saved forwards, local or HUB/NAME alike,
// under the file's lock (config.LockPorts): two `ports add` for one machine
// must both land, as two for machines on one hub did under the old hub-conf
// flock. A machine whose conf (its hub's, for HUB/NAME) existed when the
// edit began but is gone once the lock is held was deleted meanwhile;
// `delete` removes the conf before it clears the file under this lock, so
// the edit refuses rather than leave a stray file behind. An unregistered
// smol VM has no conf to check. An edit that changes nothing writes nothing.
func (a *App) editPorts(m *config.Machine, edit func([]string) []string) error {
	conf := m.Name
	if m.IsHubMachine() {
		conf = m.Hub.Name
	}
	registered := config.Exists(a.Paths.Config, conf)
	unlock, err := config.LockPorts(a.Paths, m.Name)
	if err != nil {
		return err
	}
	defer unlock()
	if registered && !config.Exists(a.Paths.Config, conf) {
		return fmt.Errorf("'%s' was deleted while its forwards were being edited", conf)
	}
	before, err := a.savedPorts(m.Name)
	if err != nil {
		return err
	}
	after := edit(slices.Clone(before))
	if slices.Equal(before, after) {
		return nil
	}
	return config.SavePorts(a.Paths, m.Name, after)
}

// dialForwards dials the machine's forward daemon. A hub machine's daemon
// may fail to come up for a moment right after `start HUB/NAME` (smolvm on
// the hub can still say "starting", so its __session refuses); with wait
// > 0 that is retried until wait elapses. Dial fails fast when the daemon
// exits early, so a retry costs about one ssh round trip.
func (a *App) dialForwards(m *config.Machine, wait time.Duration) (*session.Client, error) {
	deadline := time.Now().Add(wait)
	for {
		cl, err := session.Dial(a.Paths, m.Name)
		if err == nil || !m.IsHubMachine() || !time.Now().Before(deadline) {
			return cl, err
		}
		time.Sleep(4 * runningPollInterval)
	}
}

// startHubMachine is `start HUB/NAME`: the proxied start, then this host's
// own saved forwards (hub.md §3). The hub's start only knows the hub's
// forwards; the laptop's are in its own ports file.
func (a *App) startHubMachine(cmd *cobra.Command, args []string) error {
	if err := a.proxy(cmd, args); err != nil {
		return err
	}
	if _, err := config.LoadHubMachine(a.Paths.Config, args[0]); err != nil {
		return err
	}
	if saved, err := a.savedPorts(args[0]); err != nil || len(saved) == 0 {
		return err
	}
	return a.tunnelUpWait(args[0], runningPollTimeout)
}
