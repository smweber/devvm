package cli

import (
	"context"
	"io"

	"github.com/smweber/devvm/internal/agentbin"
	"github.com/smweber/devvm/internal/backend"
)

// agentRun installs the guest agent if needed, then runs a one-shot
// `devvm-agent <args...>` as the dev user with the given stdio. Used for keys
// operations that don't need the persistent session (a single exec is fine —
// the concurrency wall is only about parallel execs).
func (a *App) agentRun(b backend.Backend, stdin io.Reader, stdout io.Writer, args ...string) error {
	ctx := context.Background()
	if err := agentbin.Install(ctx, b); err != nil {
		return err
	}
	argv := append([]string{agentbin.GuestPath}, args...)
	return b.Run(ctx, backend.ExecOpts{
		User:   backend.DefaultUser,
		Stdin:  stdin,
		Stdout: stdout,
	}, argv...)
}
