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

// runShell opens a raw login shell (no tmux); runAttach joins the dev tmux
// session. Both dispatch to the backend's Interactive surface, with the
// transport (ssh|mosh) resolved from the flag or conf for remote machines.
func (a *App) runShell(name, transport string) error {
	it, tr, err := a.interactive(name, transport)
	if err != nil {
		return err
	}
	return it.Shell(tr)
}

func (a *App) runAttach(name, transport string) error {
	it, tr, err := a.interactive(name, transport)
	if err != nil {
		return err
	}
	return it.Attach(tr)
}

func (a *App) interactive(name, transport string) (backend.Interactive, string, error) {
	m, b, err := a.resolve(name)
	if err != nil {
		return nil, "", err
	}
	it, ok := b.(backend.Interactive)
	if !ok {
		return nil, "", fmt.Errorf("backend %q is not interactive", m.Backend)
	}
	tr, err := resolveTransport(m, transport)
	if err != nil {
		return nil, "", err
	}
	return it, tr, nil
}

// resolveTransport picks the interactive transport: an explicit --transport flag
// (remote-only), else the machine's configured default. smol ignores it.
func resolveTransport(m *config.Machine, flag string) (string, error) {
	if flag == "" {
		return m.TransportName(), nil
	}
	if !m.IsRemote() {
		return "", fmt.Errorf("--transport only applies to remote machines ('%s' is %s)", m.Name, m.Backend)
	}
	switch flag {
	case config.TransportSSH, config.TransportMosh:
		return flag, nil
	default:
		return "", fmt.Errorf("invalid transport %q (want %q or %q)", flag, config.TransportSSH, config.TransportMosh)
	}
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
