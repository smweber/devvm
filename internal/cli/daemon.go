package cli

import (
	"context"

	"github.com/smweber/devvm/internal/session"
	"github.com/spf13/cobra"
)

// daemonCmd is the hidden per-machine forward daemon entrypoint. The CLI spawns
// it detached (session.Client.spawnDaemon) when a forward is first needed.
func (a *App) daemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__daemon NAME",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// A HUB/NAME daemon is this host's for its own forwards to a hub
			// machine; its transport is a __session on the hub, never an
			// agent exec (hub.md §7). resolve refuses hub machines for every
			// other verb, so they are let through here explicitly.
			m, b, err := a.resolveForwardsConf(args[0])
			if err != nil {
				return err
			}
			return session.RunDaemon(context.Background(), a.ConfigDir, m, b, Version)
		},
	}
}
