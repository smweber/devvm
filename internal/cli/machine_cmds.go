package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/reach"
	"github.com/smweber/devvm/internal/session"
)

func (a *App) runExec(name string, argv []string) error {
	m, b, err := a.resolveUnmanaged(name) // an unmanaged VM runs it as smolvm's default user (backend)
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
	err = b.Run(context.Background(), backend.ExecOpts{Login: true}, argv...)
	a.noteContact(m, "exec", config.TransportSSH, err)
	return err
}

// runShell opens a raw login shell (no tmux); runAttach joins the dev tmux
// session. Both dispatch to the backend's Interactive surface, with the
// transport (ssh|mosh) resolved from the flag or conf for remote machines,
// and hold the browser bridge (bridgeSession) for as long as they run. A
// remote box that let one in is reachable as of its return (noteContact).
func (a *App) runShell(name, transport string) error {
	m, it, tr, release, err := a.interactive(name, transport, true)
	if err != nil {
		return err
	}
	defer release()
	err = it.Shell(tr)
	a.noteContact(m, "shell", tr, err)
	return err
}

func (a *App) runAttach(name, transport string) error {
	m, it, tr, release, err := a.interactive(name, transport, false)
	if err != nil {
		return err
	}
	defer release()
	err = it.Attach(tr)
	a.noteContact(m, "attach", tr, err)
	return err
}

// allowUnmanaged is shell's: an unmanaged smol VM gets a plain login shell
// (the backend's), never attach's dev tmux session. bridgeSession holds
// nothing for one (bridged).
func (a *App) interactive(name, transport string, allowUnmanaged bool) (*config.Machine, backend.Interactive, string, func(), error) {
	resolve := a.resolve
	if allowUnmanaged {
		resolve = a.resolveUnmanaged
	}
	m, b, err := resolve(name)
	if err != nil {
		return nil, nil, "", nil, err
	}
	it, ok := b.(backend.Interactive)
	if !ok {
		return nil, nil, "", nil, fmt.Errorf("backend %q is not interactive", m.Backend)
	}
	tr, err := resolveTransport(m, transport)
	if err != nil {
		return nil, nil, "", nil, err
	}
	// Subscribed (acked) before the session starts, so the first URL a
	// program in it opens already reaches this host.
	return m, it, tr, a.bridgeSession(m, b), nil
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
	defer config.TouchChanged(a.Paths) // wake `status --watch`
	m, b, err := a.resolveUnmanaged(name)
	if err != nil {
		return err
	}
	if err := b.PowerStart(); err != nil {
		return err
	}
	if requireManaged(m) != nil {
		return nil // no forwards on a VM devvm did not create (nothing to resume)
	}
	// Forwards follow the VM: resume the saved ones on start.
	saved, err := a.savedPorts(m.Name)
	if err != nil {
		return err
	}
	if len(saved) > 0 {
		return a.tunnelUpWait(name, runningPollTimeout) // smolvm may still say "starting"
	}
	return nil
}

func (a *App) runStop(name string) error {
	defer config.TouchChanged(a.Paths) // wake `status --watch`
	_, b, err := a.resolveUnmanaged(name)
	if err != nil {
		return err
	}
	// Reap this machine's forwards (stop its daemon) before powering off, so no
	// dead forward squats a host port.
	if cl, derr := session.Existing(a.Paths, name); derr == nil {
		_ = cl.Stop()
	}
	return b.PowerStop()
}

func (a *App) runDelete(name string, force bool) error {
	defer config.TouchChanged(a.Paths) // wake `status --watch`
	m, b, err := a.resolveAny(name)    // delete applies to hubs and hub machines too
	var stale *errStaleConf
	if errors.As(err, &stale) {
		return a.deleteStaleConf(name)
	}
	if err != nil {
		return err
	}
	if m.IsHubMachine() {
		return a.deleteHubMachine(m, b)
	}
	if m.IsHub() {
		return a.deleteHub(m, force)
	}
	switch {
	case !m.IsRemote():
		// devvm destroys only what it made (config.md §2): a VM it merely
		// discovered is smolvm's to remove, as a config entry will be
		// `config rm`'s.
		if !m.Managed() {
			return fmt.Errorf("'%s' is a smol VM devvm did not create; remove it with 'smolvm machine rm --name %s'", name, name)
		}
		ok, err := confirm(fmt.Sprintf("Delete VM '%s' and its disk (irreversible)?", name))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("aborted")
		}
		a.reapRunFiles(name) // the daemon's exec goes before the VM does
		if err := b.PowerDelete(); err != nil {
			return err
		}
	default:
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
		a.reapRunFiles(name)
		_ = b.PowerDelete()
	}
	if err := config.Remove(a.Paths.Config, name); err != nil {
		return err
	}
	if err := a.forgetHostState(name, m); err != nil {
		return err
	}
	session.RemoveLock(a.Paths, name) // only now: see RemoveLock
	fmt.Fprintf(a.Stdout, "devvm: removed '%s'\n", name)
	return nil
}

// deleteStaleConf is `delete` of an old per-machine smol conf whose VM is
// gone (what used to be "dormant"): nothing to destroy, so no prompt; the
// conf and this host's state for the name go.
func (a *App) deleteStaleConf(name string) error {
	a.reapRunFiles(name)
	if err := config.Remove(a.Paths.Config, name); err != nil {
		return err
	}
	if err := a.forgetHostState(name, nil); err != nil {
		return err
	}
	session.RemoveLock(a.Paths, name) // only now: see RemoveLock
	fmt.Fprintf(a.Stdout, "devvm: removed stale registry entry for '%s' (it had no VM)\n", name)
	return nil
}

// forgetHostState removes what this host remembers about a machine besides
// its registry entry and run files (reapRunFiles): its saved forwards
// (<State>/ports/NAME), its managed marker (<State>/managed/NAME), its
// reach cache (<State>/cache/reach/NAME) and its pinned host key in the isolated known_hosts. It is the one place `delete`
// clears host state, so a later machine of the same name starts clean
// (config.md §9 item 6: a server recreated under the same name and address
// has a new host key). Run after the machine and its conf are gone, so a
// failed delete keeps its state, and under the ports file's lock, so a
// `ports add` that was waiting on it finds the conf gone and refuses
// (editPorts) instead of writing the file back. A ports file left by a
// failure here is cleared by the next `create` of the name; a marker is
// rewritten by it.
//
// m is the deleted record, for its pin; nil when there is none to drop (a
// smol VM, a hub machine, a stale conf). See forgetHostKey.
func (a *App) forgetHostState(name string, m *config.Machine) error {
	unlock, err := config.LockPorts(a.Paths, name)
	if err != nil {
		return fmt.Errorf("%s: clear saved forwards: %w", name, err)
	}
	defer unlock()
	if err := config.RemovePorts(a.Paths, name); err != nil {
		return fmt.Errorf("%s: clear saved forwards: %w", name, err)
	}
	if _, _, onHub, _ := config.SplitHubName(name); !onHub {
		if err := config.RemoveManagedMarker(a.Paths, name); err != nil {
			return fmt.Errorf("%s: clear managed marker: %w", name, err)
		}
	}
	if err := reach.Remove(a.Paths, name); err != nil {
		return fmt.Errorf("%s: clear reach cache: %w", name, err)
	}
	if m != nil {
		if err := a.forgetHostKey(m); err != nil {
			return fmt.Errorf("%s: clear pinned host key: %w", name, err)
		}
	}
	return nil
}

// forgetHostKey drops the host key devvm pinned for m in its isolated
// known_hosts (only managed ssh boxes are pinned, sshFlags). The entry is
// keyed by the host ssh actually connects to, so it is asked (`ssh -G`
// resolves an alias in the user's ssh config, and a non-22 port), and
// `ssh-keygen -R` removes it, hashed or not. It is kept while another
// registered machine still reaches the same host and port: known_hosts is
// per host, not per machine name, and that machine's pin is the same line.
func (a *App) forgetHostKey(m *config.Machine) error {
	if !m.IsRemote() || !m.Managed() || m.SSHHost == "" {
		return nil
	}
	kh := a.Paths.KnownHostsPath()
	if _, err := os.Stat(kh); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	key := knownHostsKey(m)
	names, _ := config.List(a.Paths.Config)
	for _, n := range names {
		if n == m.Name {
			continue
		}
		if o, err := config.Load(a.Paths.Config, n); err == nil && o.IsRemote() && o.Managed() && knownHostsKey(o) == key {
			return nil
		}
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		return nil // nothing could have pinned it without ssh either
	}
	if out, err := exec.Command("ssh-keygen", "-R", key, "-f", kh).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh-keygen -R %s: %v: %s", key, err, strings.TrimSpace(string(out)))
	}
	_ = os.Remove(kh + ".old") // ssh-keygen's backup; the state dir keeps no history
	return nil
}

// knownHostsKey is the known_hosts name ssh records for m. It asks `ssh -G`
// (local: it only evaluates config) with the same port option real
// connections pass (sshFlags adds -o Port only when it is neither 22 nor 0,
// so an alias's own `Port` in ssh config wins), and reads back what ssh
// would key the pin by: HostKeyAlias when set (bare, as ssh stores it),
// else the resolved hostname, bracketed with the port when that is not 22.
// If -G fails, the destination's own host part and the conf's port stand in.
func knownHostsKey(m *config.Machine) string {
	host := m.SSHHost
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	port := m.SSHPort
	if port == 0 {
		port = 22
	}
	alias := ""
	args := []string{"-G"}
	if m.SSHPort != 22 && m.SSHPort != 0 {
		args = append(args, "-o", fmt.Sprintf("Port=%d", m.SSHPort))
	}
	if out, err := exec.Command("ssh", append(args, m.SSHHost)...).Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok {
				continue
			}
			switch k {
			case "hostname":
				host = v
			case "port":
				if p, err := strconv.Atoi(v); err == nil {
					port = p
				}
			case "hostkeyalias":
				if v != "none" {
					alias = v
				}
			}
		}
	}
	if alias != "" {
		return alias
	}
	if port != 22 {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return host
}

// reapRunFiles stops a machine's forward daemon, if one runs, and removes
// its run files (session.Reap) as its registry entry goes. Failing that is
// worth a line, not the delete: the leftovers are inert files in run/.
func (a *App) reapRunFiles(name string) {
	if err := session.Reap(a.Paths, name, daemonGoneTimeout); err != nil && a.Stderr != nil {
		fmt.Fprintf(a.Stderr, "devvm: %s: %v\n", name, err)
	}
}

// deleteHubMachine is `delete HUB/NAME`: the hub owns the machine's registry
// entry, so the deregistration is a proxied `delete NAME` there, first —
// a failed remote delete leaves the laptop side intact. Then this host's own
// daemon for it is stopped and its host state (saved forwards) dropped;
// the hub conf is never written. The one confirmation is the hub's: its
// delete prompts through the forwarded terminal before destroying a real VM
// (and refuses with no terminal), so a second prompt here would only ask the
// same question twice. --force is not forwarded: it is this host's flag for
// deleting a hub this host still has forwards for, and means nothing for a
// machine on a hub.
func (a *App) deleteHubMachine(m *config.Machine, b backend.Backend) error {
	hub, machine := m.Hub, m.HubMachineName()
	p, ok := b.(backend.Proxier)
	if !ok {
		return fmt.Errorf("%s: backend %q cannot proxy", m.Name, b.Kind())
	}
	if err := a.runProxied(m, p, []string{"delete", machine}); err != nil {
		return err
	}
	a.reapRunFiles(m.Name)
	dropFromHubCache(a.Paths, hub.Name, machine)
	saved, _ := a.savedPorts(m.Name)
	if err := a.forgetHostState(m.Name, nil); err != nil {
		return err
	}
	if len(saved) > 0 {
		fmt.Fprintf(a.Stdout, "devvm: dropped this host's forwards for '%s'\n", m.Name)
	}
	return nil
}

// deleteHub is `delete HUB`: the hub was only ever registered, never shaped,
// so this removes one conf file and touches nothing on the host. It refuses
// while this host has saved forwards for a machine on it (a ports file
// HUB@NAME) or a live run/HUB@*.sock (hub.md §2): the saved forwards are
// the laptop's for machines that still exist on the hub, and a live daemon
// holds forwards the conf removal would orphan. --force stops those daemons,
// drops those saved forwards and removes the conf anyway. The refusal runs
// before the prompt, so a scripted delete fails fast. (Step 6, which adds
// the daemons, changes nothing here: liveHubSockets already sees them.)
func (a *App) deleteHub(m *config.Machine, force bool) error {
	var held, saved []string
	names, err := config.SavedPortsNames(a.Paths)
	if err != nil {
		return err
	}
	for _, display := range names {
		if hub, _, ok, _ := config.SplitHubName(display); ok && hub == m.Name {
			saved = append(saved, display)
			held = append(held, "saved forwards for "+display)
		}
	}
	var live []string
	for _, display := range liveHubSockets(a.Paths) {
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
	// Every HUB/NAME with a run file, not only the live sockets: a daemon
	// killed rather than stopped leaves its socket, and every one that ever
	// started leaves its lock and log. The hub's own name too (a refused
	// `__daemon HUB` still took its lock).
	reaped := append(session.HubRuntimeNames(a.Paths, m.Name), m.Name)
	for _, display := range reaped {
		a.reapRunFiles(display)
	}
	if err := config.Remove(a.Paths.Config, m.Name); err != nil {
		return err
	}
	// After the conf: a `ports add HUB/X` waiting on X's lock then finds
	// the hub gone and writes nothing back (editPorts).
	for _, display := range append(saved, m.Name) {
		if err := a.forgetHostState(display, nil); err != nil {
			return err
		}
	}
	for _, display := range reaped {
		session.RemoveLock(a.Paths, display) // only now: see RemoveLock
	}
	removeHubCache(a.Paths, m.Name) // derived from the conf that just went
	fmt.Fprintf(a.Stdout, "devvm: removed hub '%s'\n", m.Name)
	return nil
}
