package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
	defer config.TouchChanged(a.ConfigDir) // wake `status --watch`
	m, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	if err := b.PowerStart(); err != nil {
		return err
	}
	// Forwards follow the VM: resume configured ones on start.
	if len(m.Ports) > 0 {
		return a.tunnelUpWait(name, runningPollTimeout) // smolvm may still say "starting"
	}
	return nil
}

func (a *App) runStop(name string) error {
	defer config.TouchChanged(a.ConfigDir) // wake `status --watch`
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
	defer config.TouchChanged(a.ConfigDir) // wake `status --watch`
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
	defer config.TouchChanged(a.ConfigDir) // wake `status --watch`
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

func (a *App) runDelete(name string, force bool) error {
	defer config.TouchChanged(a.ConfigDir) // wake `status --watch`
	m, b, err := a.resolveAny(name)        // delete applies to hubs and hub machines too
	if err != nil {
		return err
	}
	if m.IsHubMachine() {
		return a.deleteHubMachine(m, b)
	}
	if m.IsHub() {
		return a.deleteHub(m, force)
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

// deleteHubMachine is `delete HUB/NAME`: the hub owns the machine's registry
// entry, so the deregistration is a proxied `delete NAME` there, first —
// a failed remote delete leaves the laptop side intact. Then this host's own
// daemon for it is stopped and its [machines.NAME] table dropped from the
// hub conf. The one confirmation is the hub's: its delete prompts through
// the forwarded terminal before destroying a real VM (and refuses with no
// terminal), so a second prompt here would only ask the same question
// twice. --force is not forwarded: it is this host's flag for deleting a
// hub conf that still has tables, and means nothing for a machine on a hub.
func (a *App) deleteHubMachine(m *config.Machine, b backend.Backend) error {
	hub, machine := m.Hub, m.HubMachineName()
	p, ok := b.(backend.Proxier)
	if !ok {
		return fmt.Errorf("%s: backend %q cannot proxy", m.Name, b.Kind())
	}
	if err := a.runProxied(m, p, []string{"delete", machine}); err != nil {
		return err
	}
	if cl, err := session.Existing(a.ConfigDir, m.Name); err == nil {
		_ = cl.Stop()
	}
	dropFromHubCache(a.ConfigDir, hub.Name, machine)
	if _, recorded := hub.Machines[machine]; recorded {
		// Step 6 wraps this read-modify-write in a flock on the hub conf: two
		// `ports add HUB/a` and `ports add HUB/b` share one file, and Save is
		// atomic but not serialized.
		delete(hub.Machines, machine)
		if err := hub.Save(a.ConfigDir); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "devvm: dropped this host's forwards for '%s'\n", m.Name)
	}
	return nil
}

// deleteHub is `delete HUB`: the hub was only ever registered, never shaped,
// so this removes one conf file and touches nothing on the host. It refuses
// while any [machines.*] table or live run/HUB@*.sock exists (hub.md §2):
// the tables are the laptop's forwards for machines that still exist on the
// hub, and a live daemon holds forwards the conf removal would orphan.
// --force stops those daemons and removes the conf anyway. The refusal runs
// before the prompt, so a scripted delete fails fast. (Step 6, which adds
// the daemons, changes nothing here: liveHubSockets already sees them.)
func (a *App) deleteHub(m *config.Machine, force bool) error {
	var held []string
	for name := range m.Machines {
		held = append(held, "[machines."+name+"]")
	}
	var live []string
	for _, display := range liveHubSockets(a.ConfigDir) {
		if hub, _, _, _ := config.SplitHubName(display); hub == m.Name {
			live = append(live, display)
			held = append(held, "forwards for "+display)
		}
	}
	if len(held) > 0 && !force {
		sort.Strings(held)
		return fmt.Errorf("hub '%s' still has %s; delete those machines first, or use --force",
			m.Name, strings.Join(held, ", "))
	}
	ok, err := confirm(fmt.Sprintf("Remove hub '%s' from the registry (leaves the host untouched)?", m.Name))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("aborted")
	}
	for _, display := range live {
		if cl, err := session.Existing(a.ConfigDir, display); err == nil {
			_ = cl.Stop()
		}
	}
	if err := config.Remove(a.ConfigDir, m.Name); err != nil {
		return err
	}
	removeHubCache(a.ConfigDir, m.Name) // derived from the conf that just went
	fmt.Fprintf(a.Stdout, "devvm: removed hub '%s'\n", m.Name)
	return nil
}
