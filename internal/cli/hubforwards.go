package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// Hub forwards (docs/proposals/hub.md §7, roadmap step 6). `ports … HUB/NAME`
// runs here, on this host: the forwards are this host's own, inherently per
// client, so they are recorded in the hub conf's [machines.NAME] table and
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
	m, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	if err := requireRunningForForwards(m, b, 0); err != nil {
		return err
	}
	conn, err := session.DialConn(a.ConfigDir, name)
	if err != nil {
		return err
	}
	return session.Relay(conn, a.stdin(), a.Stdout)
}

// resolveForwards is resolveLive for the ports verbs, which also act on a
// machine on a hub: their forwards are this host's (hub.md §7). A HUB/NAME
// resolves from the hub conf alone, never dialing; whether the machine
// exists and runs is the hub's answer when the daemon's __session starts.
func (a *App) resolveForwards(name string) (*config.Machine, backend.Backend, error) {
	if ok, err := hubMachineArg([]string{name}); err != nil {
		return nil, nil, err
	} else if ok {
		return a.resolveAny(name)
	}
	return a.resolveLive(name)
}

// resolveForwardsConf is resolve for the conf-only ports verbs (rm, list):
// no provisioned check, a hub machine allowed.
func (a *App) resolveForwardsConf(name string) (*config.Machine, backend.Backend, error) {
	if ok, err := hubMachineArg([]string{name}); err != nil {
		return nil, nil, err
	} else if ok {
		return a.resolveAny(name)
	}
	return a.resolve(name)
}

// editPorts rewrites a machine's configured ports. A local machine saves
// its own conf. A hub machine's ports are its [machines.NAME] table in the
// hub conf, which every machine on that hub shares, so the edit re-reads
// the table and saves under the hub conf's flock (config.UpdateHubMachine):
// two `ports add` for different machines on one hub must both land. m.Ports
// is refreshed to what was saved.
func (a *App) editPorts(m *config.Machine, edit func([]string) []string) error {
	if !m.IsHubMachine() {
		m.Ports = edit(m.Ports)
		return m.Save(a.ConfigDir)
	}
	machine := m.HubMachineName()
	h, err := config.UpdateHubMachine(a.ConfigDir, m.Hub.Name, machine, func(hm *config.HubMachine) error {
		hm.Ports = edit(hm.Ports)
		return nil
	})
	if err != nil {
		return err
	}
	m.Hub, m.Ports = h, h.Machines[machine].Ports
	return nil
}

// dialForwards dials the machine's forward daemon. A hub machine's daemon
// may fail to come up for a moment right after `start HUB/NAME` (smolvm on
// the hub can still say "starting", so its __session refuses); with wait
// > 0 that is retried until wait elapses. Dial fails fast when the daemon
// exits early, so a retry costs about one ssh round trip.
func (a *App) dialForwards(m *config.Machine, wait time.Duration) (*session.Client, error) {
	deadline := time.Now().Add(wait)
	for {
		cl, err := session.Dial(a.ConfigDir, m.Name)
		if err == nil || !m.IsHubMachine() || !time.Now().Before(deadline) {
			return cl, err
		}
		time.Sleep(4 * runningPollInterval)
	}
}

// startHubMachine is `start HUB/NAME`: the proxied start, then this host's
// own configured forwards (hub.md §3). The hub's start only knows the hub's
// conf; the laptop's forwards live in its table here.
func (a *App) startHubMachine(cmd *cobra.Command, args []string) error {
	if err := a.proxy(cmd, args); err != nil {
		return err
	}
	m, err := config.LoadHubMachine(a.ConfigDir, args[0])
	if err != nil {
		return err
	}
	if len(m.Ports) == 0 {
		return nil
	}
	return a.tunnelUpWait(args[0], runningPollTimeout)
}
