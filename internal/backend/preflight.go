package backend

import (
	"context"
	"fmt"
	"io"

	"github.com/smweber/devvm/internal/config"
)

// RequireGuestTmux verifies tmux exists in the guest before a command that
// depends on it (shell/ssh/mosh all attach a tmux session named "dev"). tmux is
// not part of devvm's own prereqs — the pluggable provisioner installs it — so a
// box provisioned with `provision = "none"`, or a bootstrap that skipped tmux,
// otherwise fails deep in the keeper / `new-session` path with a confusing
// symptom. Fail early, naming the likely cause and a concrete fix.
func RequireGuestTmux(b Backend, m *config.Machine) error {
	// Login shell so a brew-installed tmux (/home/linuxbrew/.linuxbrew/bin, off
	// the bare non-login PATH) is found — matching every other tmux invocation.
	if b.Run(context.Background(), ExecOpts{Login: true, Stdout: io.Discard, Stderr: io.Discard},
		"tmux", "-V") == nil {
		return nil
	}
	return fmt.Errorf(
		"tmux not found in %q: attaching a dev session needs tmux, but the provisioner "+
			"(provision=%q) didn't install it\n"+
			"  install it in the guest, e.g.:  devvm exec %s -- sudo apt-get install -y tmux",
		m.Name, m.Provision, m.Name)
}
