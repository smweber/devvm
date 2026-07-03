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
			m, b, err := a.resolve(args[0])
			if err != nil {
				return err
			}
			return session.RunDaemon(context.Background(), a.ConfigDir, m, b)
		},
	}
}
