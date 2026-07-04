package cli

import (
	"context"
	"fmt"

	"github.com/smweber/devvm/internal/auth"
	"github.com/smweber/devvm/internal/config"
)

func (a *App) runAuth(name, tool string, installAgent bool) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	tools, err := auth.Tools(tool)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "devvm: starting requested auth flow for '%s' (%s)\n", name, tool)
	return auth.Authenticate(context.Background(), b, m, tools, a.agentInstallApprover(m, installAgent))
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
