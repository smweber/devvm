package cli

import (
	"context"
	"fmt"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

func (a *App) runExec(name string, argv []string) error {
	_, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	return b.Run(context.Background(), backend.ExecOpts{Login: true}, argv...)
}

func (a *App) runShell(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if m.Backend != config.BackendSmol {
		return fmt.Errorf("'%s' is a '%s' machine; use 'devvm ssh %s'", name, m.Backend, name)
	}
	sh, ok := b.(backend.Sheller)
	if !ok {
		return fmt.Errorf("backend %q has no shell", m.Backend)
	}
	return sh.Shell()
}

func (a *App) runSSH(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if !m.IsRemote() {
		return fmt.Errorf("'%s' is a '%s' machine; use 'devvm shell %s'", name, m.Backend, name)
	}
	if err := backend.RequireGuestTmux(b, m); err != nil {
		return err
	}
	return b.Run(context.Background(), backend.ExecOpts{TTY: true, Login: true},
		"tmux", "new-session", "-A", "-s", "dev")
}

func (a *App) runStart(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if err := b.PowerStart(); err != nil {
		return err
	}
	// Forwards follow the VM: resume configured ones on start.
	if len(m.Ports) > 0 {
		return a.tunnelUp(name)
	}
	return nil
}

func (a *App) runStop(name string) error {
	_, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	// Reap this machine's forwards (stop its daemon) before powering off, so no
	// dead forward squats a host port.
	if cl, derr := session.Existing(a.ConfigDir, name); derr == nil {
		_ = cl.Stop()
	}
	return b.PowerStop()
}

func (a *App) runDelete(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	switch {
	case m.Backend == config.BackendSmol:
		ok, err := confirm(fmt.Sprintf("Delete VM '%s' and its disk (irreversible)?", name))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("aborted")
		}
		if err := b.PowerDelete(); err != nil {
			return err
		}
	case m.IsRemote():
		// Adopted (unmanaged) hosts only lose their registry entry; the box is
		// left untouched, so confirm we're not expected to tear anything down.
		if !m.Managed() {
			ok, err := confirm(fmt.Sprintf(
				"Remove registry entry for '%s' (leaves the host untouched)?", name))
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("aborted")
			}
		}
		_ = b.PowerDelete()
	}
	if err := config.Remove(a.ConfigDir, name); err != nil {
		return err
	}
	fmt.Printf("devvm: removed '%s'\n", name)
	return nil
}

func (a *App) runStatus(name string) error {
	_, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	st, err := b.Status()
	if err != nil {
		return err
	}
	fmt.Printf("Machine '%s' (%s): %s\n", st.Name, st.Backend, st.Raw)
	a.forwardReport(name)
	return nil
}

func (a *App) runMosh(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	ex, ok := b.(backend.Extras)
	if !ok {
		return fmt.Errorf("'mosh' only applies to remote machines ('%s' is %s)", name, m.Backend)
	}
	return ex.Mosh()
}
