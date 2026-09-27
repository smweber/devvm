package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/smweber/devvm/internal/auth"
	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// runAuth is `auth NAME` on this host's own machine. On smol it is thin
// (browser-bridge.md §6): a session on the machine's forward daemon reports
// the shim and, subscribed, opens the logins' URLs here; nothing is
// installed and no agent exec is spawned (the daemon holds the only one).
// Remote boxes keep the private agent session (auth.Run branches).
func (a *App) runAuth(name, tool string, installAgent bool) error {
	m, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	tools, err := auth.Tools(tool)
	if err != nil {
		return err
	}
	if auth.Thin(m) {
		// No daemon is spawned for a stopped VM (its first dial would exec
		// into it), so say so here rather than as a failed dial.
		if err := requireRunningForForwards(m, b, 0); err != nil {
			return err
		}
	}
	fmt.Fprintf(a.Stderr, "devvm: starting requested auth flow for '%s' (%s)\n", name, tool)
	return auth.Run(context.Background(), b, m, tools, auth.Options{
		Approve: a.agentInstallApprover(m, installAgent),
		OpenBridge: func() (auth.Bridge, error) {
			return openAuthBridge(a.ConfigDir, m.Name, a.hostOpener())
		},
		NoSubscribe: noSubscribe(),
		Version:     Version,
		PingShim: func() (string, error) {
			cl, err := session.Existing(a.ConfigDir, m.Name)
			if err != nil {
				return "", err
			}
			return cl.Shim()
		},
	})
}

// authHubMachine is `auth HUB/NAME` (hub.md §8): the laptop half of thin
// auth. It dials this host's daemon for HUB@NAME (spawning it: its relay
// comes up on the hub), subscribes, acked only once that daemon is
// subscribed on the hub's, binds the tools' fixed callback ports here, and
// then proxies `auth NAME` to the hub with DEVVM_NO_SUBSCRIBE=1. The hub's
// auth runs the logins with a guest terminal (a proxied `exec` would give
// the guest none, and every login prompts), sets BROWSER to the shim its
// own daemon reports, and never subscribes, so every URL the logins open
// comes here through the relay. A hub too old for that is refused before
// anything runs on it: its `auth` would spawn a second agent exec. So is
// every case where this host cannot prove the hub's daemon is subscribed
// for it: this daemon reconnecting (its subscribe is refused, see
// session's errUpstreamReconnecting), no shim reported after the ack, or
// this host itself running under DEVVM_NO_SUBSCRIBE (nothing subscribes
// here, so nothing proves anything).
func (a *App) authHubMachine(cmd *cobra.Command, args []string) error {
	name := args[0]
	tool := "all"
	if len(args) == 2 {
		tool = args[1]
	}
	tools, err := auth.Tools(tool)
	if err != nil {
		return err
	}
	m, p, err := a.resolveProxy(name)
	if err != nil {
		return err
	}
	if noSubscribe() {
		return fmt.Errorf("auth %s: %s is set here, so this host cannot subscribe for the hub's logins; unset it and retry", name, backend.NoSubscribeEnv)
	}
	br, err := openAuthBridge(a.ConfigDir, m.Name, a.hostOpener())
	if err != nil {
		return fmt.Errorf("auth %s: browser bridge: %w", name, err)
	}
	defer br.Close()
	if err := br.Subscribe(); err != nil {
		// The daemon's reply leads with where the subscription stopped
		// (for attach's notice); here the refusal is the whole story.
		return fmt.Errorf("auth %s: %s", name, strings.TrimPrefix(err.Error(), session.NotUpstreamPrefix))
	}
	// Belt and braces: after the ack the hub has reported the shim its
	// own auth will use. None means this host's daemon lost its link
	// since, or the hub's machine carries no browser events (not smol);
	// either way the hub's auth would not route its logins here.
	info, err := br.Ping()
	if err != nil {
		return fmt.Errorf("auth %s: browser bridge: %w", name, err)
	}
	if info.Shim == "" {
		if info.State != session.StateUp {
			return fmt.Errorf("auth %s: the link to hub %s is %s; retry in a moment", name, m.Hub.Name, info.State)
		}
		if be := hubMachineBackend(a.ConfigDir, m); be != "" && be != config.BackendSmol {
			return fmt.Errorf("auth %s: auth on a non-smol hub machine is not supported through the hub yet (%s is %s there)", name, name, be)
		}
		// Not cached: a hub daemon new enough to pass the subscribe always
		// reports its smol transport's shim, so this is almost surely a
		// machine that is not smol, but the laptop cannot say for certain.
		return fmt.Errorf("auth %s: the hub's daemon for %s reports no browser shim (is it a smol machine?), so logins there would not open here", name, m.HubMachineName())
	}
	for _, port := range auth.CallbackPorts(tools) {
		auth.BridgeCallback(br, port)
	}
	argv, err := hubArgv(cmd, args)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "devvm: starting requested auth flow for '%s' (%s) on hub %s\n", name, tool, m.Hub.Name)
	return a.runProxied(m, p, argv)
}

// hubMachineBackend is the hub-side backend of a hub machine as the
// hub's cached listing last said (hublist.go), "" when not cached: enough
// to word a refusal, never to decide one.
func hubMachineBackend(configDir string, m *config.Machine) string {
	for _, r := range readHubCache(configDir, m.Hub.Name) {
		if r.name == m.HubMachineName() {
			return r.backend
		}
	}
	return ""
}

// agentInstallApprover returns nil for managed boxes (devvm owns them, so it
// installs the agent freely). For an adopt host it returns a gate that requires
// the --install-agent flag or an interactive yes before devvm writes the helper
// agent into the user's ~/.local/bin.
func (a *App) agentInstallApprover(m *config.Machine, installAgent bool) func() error {
	if m.Managed() {
		return nil
	}
	return func() error {
		if installAgent {
			return nil
		}
		ok, err := confirm(fmt.Sprintf(
			"devvm needs to install a helper agent in ~/.local/bin on '%s' (unmanaged host) for auth. Proceed?", m.Name))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("agent install declined; re-run with --install-agent to allow")
		}
		return nil
	}
}
