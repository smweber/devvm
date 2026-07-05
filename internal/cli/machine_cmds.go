package cli

import (
	"context"
	"fmt"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

func (a *App) runExec(name string, argv []string) error {
	_, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	// Flag parsing is disabled so CMD's own flags pass through, which means a
	// conventional `devvm exec NAME -- cmd` separator lands in argv. smol survives
	// it (exec "$@" eats the --) but ssh renders `bash -lc "-- cmd"` and fails, so
	// strip one leading -- for uniform behavior across backends.
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
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
	m, b, err := a.resolveLive(name)
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
	m, b, err := a.resolveLive(name)
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
	_, b, err := a.resolveLive(name)
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

// runProvision allocates the resource for a dormant machine (conf exists, no VM)
// and bootstraps it — the inverse of deprovision, and the resource half of create.
func (a *App) runProvision(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if m.IsRemote() {
		fmt.Fprintf(a.Stderr,
			"devvm: '%s' is a remote host; devvm does not manage its resource lifecycle yet (no-op).\n", name)
		return nil
	}
	ok, err := b.Exists()
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("'%s' is already provisioned (use 'start' to power it on)", name)
	}
	// A hand-edited conf can omit memory (Save strips zero ints, Load doesn't
	// re-default it), which would allocate with --mem 0. Re-check as create does.
	if m.Backend == config.BackendSmol && m.Memory < 512 {
		return fmt.Errorf("smol machine '%s' needs memory >= 512 (MiB) in its conf (got %d)", name, m.Memory)
	}
	if err := a.provisionResource(m); err != nil {
		return err
	}
	if err := a.runBootstrap(name); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "devvm: provisioned '%s'\n", name)
	return nil
}

// runDeprovision destroys a machine's resource (disk/VM) but keeps its registry
// entry, so it can be rebuilt later with `provision`. It's the middle rung between
// stop (resource kept) and delete (entry gone too).
func (a *App) runDeprovision(name string, yes bool) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if m.IsRemote() {
		fmt.Fprintf(a.Stderr,
			"devvm: '%s' is a remote host; devvm does not manage its resource lifecycle yet (no-op).\n", name)
		return nil
	}
	ok, err := b.Exists()
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(a.Stdout, "devvm: '%s' is already dormant (not provisioned)\n", name)
		return nil
	}
	if !yes {
		confirmed, cerr := confirm(fmt.Sprintf("Destroy '%s' disk but keep its registry entry?", name))
		if cerr != nil {
			return cerr
		}
		if !confirmed {
			return fmt.Errorf("aborted")
		}
	}
	// Reap forwards (stop the daemon) before tearing down the resource, so no dead
	// forward squats a host port — same as stop.
	if cl, derr := session.Existing(a.ConfigDir, name); derr == nil {
		_ = cl.Stop()
	}
	if err := b.PowerDelete(); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout,
		"devvm: deprovisioned '%s' (registry entry kept; 'devvm provision %s' to rebuild)\n", name, name)
	return nil
}

func (a *App) runDelete(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	// A dormant machine (conf but no resource) has nothing to destroy — just drop
	// the registry entry, no scary confirm. On an Exists() error, fall through to
	// the normal path so the real cause surfaces (don't silently remove a conf for
	// a resource we couldn't probe).
	if ok, exErr := b.Exists(); exErr == nil && !ok {
		if err := config.Remove(a.ConfigDir, name); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "devvm: removed registry entry for '%s' (was dormant)\n", name)
		return nil
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
	fmt.Fprintf(a.Stdout, "devvm: removed '%s'\n", name)
	return nil
}
