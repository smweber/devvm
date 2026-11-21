package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/reach"
	"github.com/smweber/devvm/internal/session"
)

func (a *App) runExec(name string, argv []string) error {
	m, b, err := a.resolve(name)
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
	return guestExit(m, err)
}

// guestExit makes exec's failure the guest command's own exit status, as
// ssh and `smolvm machine exec` pass it through: Execute exits with it
// silently (the command wrote its own stderr), where it used to print
// "exit status N" and exit 1. Over ssh (a remote box, a provider server)
// 255 is ssh's own failure, as everywhere in devvm (noteContact, the hub
// proxy's exitStatus), so it stays an error naming the box and exits 1: a
// hub running this exec for a laptop must not answer 255, which the
// laptop reads as the hub itself unreachable. A command killed by a signal
// (code -1) and anything that is not an exit status are left as they were.
func guestExit(m *config.Machine, err error) error {
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() < 0 {
		return err
	}
	if m.IsRemote() && ee.ExitCode() == 255 {
		return fmt.Errorf("cannot reach '%s' (%s): ssh exited 255", m.Name, m.SSHHost)
	}
	return &exitCode{code: ee.ExitCode()}
}

// runShell opens a raw login shell (no tmux); runAttach joins the dev tmux
// session. Both dispatch to the backend's Interactive surface, with the
// transport (ssh|mosh) resolved from the flag or conf for remote machines,
// and hold the browser bridge (bridgeSession) for as long as they run. A
// remote box that let one in is reachable as of its return (noteContact).
func (a *App) runShell(name, transport string) error {
	m, it, tr, release, err := a.interactive(name, transport)
	if err != nil {
		return err
	}
	defer release()
	err = it.Shell(tr)
	a.noteContact(m, "shell", tr, err)
	return err
}

func (a *App) runAttach(name, transport string) error {
	m, it, tr, release, err := a.interactive(name, transport)
	if err != nil {
		return err
	}
	defer release()
	err = it.Attach(tr)
	a.noteContact(m, "attach", tr, err)
	return err
}

func (a *App) interactive(name, transport string) (*config.Machine, backend.Interactive, string, func(), error) {
	m, b, err := a.resolve(name)
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
	if err := a.bootForSession(m, b); err != nil {
		return nil, nil, "", nil, err
	}
	// Subscribed (acked) before the session starts, so the first URL a
	// program in it opens already reaches this host.
	return m, it, tr, a.bridgeSession(m, b), nil
}

// bootForSession is attach/shell on a stopped smol VM: `start` first (wait
// for "running"), and only then the bridge, which brings the saved
// forwards up behind it as on any connect (pushSaved), and the session's
// own execs. smolvm 1.16.1's `machine exec` refuses a stopped VM rather
// than booting it, so the session would fail outright (attach as "tmux not
// found"); started here, the VM is running when the bridge opens, so the
// session is bridged like any other, and no daemon exec ever starts
// against a VM still booting. Remote boxes are never booted (a hub
// machine's start is proxied: hubBootForSession).
func (a *App) bootForSession(m *config.Machine, b backend.Backend) error {
	if m.IsRemote() {
		return nil
	}
	booted, err := a.startStopped(m, b)
	if booted {
		config.TouchChanged(a.Paths) // wake `status --watch`
	}
	return err
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

// runStart powers a machine on and brings its saved forwards up.
func (a *App) runStart(name string) error {
	defer config.TouchChanged(a.Paths) // wake `status --watch`
	a.providerAsk = true
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if m.IsProvider() {
		return a.powerProviderBox(m, true)
	}
	if err := a.powerStartWait(m, b); err != nil {
		return err
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

// powerStartWait is `start`'s PowerStart, under lockStart for a smol VM (a
// concurrent attach starting it must not make smolvm kill one boot for
// the other: startStopped), then a wait for "running": smolvm may return
// before it, and whoever runs next (a proxied attach's __session, a
// script's exec) needs it running. A failed PowerStart is forgiven when
// the VM runs anyway.
func (a *App) powerStartWait(m *config.Machine, b backend.Backend) error {
	if !m.IsRemote() {
		unlock, err := lockStart(a.Paths, m.Name)
		if err != nil {
			return err
		}
		defer unlock()
	}
	if err := b.PowerStart(); err != nil {
		if m.IsRemote() || waitRunning(m, b) != nil {
			return err
		}
		return nil
	}
	return waitRunning(m, b)
}

func (a *App) runStop(name string) error {
	defer config.TouchChanged(a.Paths) // wake `status --watch`
	a.providerAsk = true
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if m.IsProvider() {
		return a.powerProviderBox(m, false) // it reaps the daemon itself
	}
	// Reap this machine's forwards (stop its daemon) before powering off, so no
	// dead forward squats a host port.
	if cl, derr := session.Existing(a.Paths, name); derr == nil {
		_ = cl.Stop()
	}
	return b.PowerStop()
}

// runDelete destroys a smol VM or provider server devvm made, or proxies
// `delete NAME` to a hub for HUB/NAME. It never removes a config entry: an
// entry is the user's line in devvm.toml, and the one command that removes
// it, and this host's state for it, is `config rm` (config.md §4). force
// is accepted and ignored, for scripts written when `delete HUB --force`
// existed; yes skips the confirm.
func (a *App) runDelete(name string, _ bool, yes bool) error {
	defer config.TouchChanged(a.Paths) // wake `status --watch`
	a.providerAsk = true
	m, b, err := a.resolveAny(name) // delete applies to hub machines too
	if err != nil {
		return err
	}
	if m.IsHubMachine() {
		return a.deleteHubMachine(m, b, yes)
	}
	// A provider server: devvm sees only the servers it made (the label),
	// so every one it resolves is its to delete.
	if m.IsProvider() {
		return a.deleteProviderBox(m, yes)
	}
	if m.IsRemote() {
		what := "a remote box"
		if m.IsHub() {
			what = "a hub"
		}
		return fmt.Errorf("'%s' is %s entry in devvm.toml; 'devvm config rm %s' removes the entry and this host's state for it (the box itself is never touched)", name, what, name)
	}
	// A smol VM resolve found is one devvm made (its label): devvm destroys
	// only what it made, and one it did not is not found at all.
	if !yes {
		ok, err := confirmYes(fmt.Sprintf("Delete VM '%s' and its disk (irreversible)?", name))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("aborted")
		}
	}
	a.reapRunFiles(name) // the daemon's exec goes before the VM does
	if err := b.PowerDelete(); err != nil {
		return err
	}
	if err := a.forgetHostState(name, nil); err != nil {
		return err
	}
	session.RemoveLock(a.Paths, name) // only now: see RemoveLock
	fmt.Fprintf(a.Stdout, "devvm: removed '%s'\n", name)
	return nil
}

// forgetHostState removes what this host remembers about a deleted smol VM,
// provider server or hub machine besides its run files (reapRunFiles): its
// saved forwards (<State>/ports/NAME), its reach cache
// (<State>/cache/reach/NAME) and its pinned host key in the isolated
// known_hosts. It is the one place `delete`
// clears host state, so a later machine of the same name starts clean
// (config.md §9 item 6: a server recreated under the same name and address
// has a new host key). Run after the machine is gone, so a failed delete
// keeps its state, and under the ports file's lock, so a `ports add` that
// was waiting on it finds the machine gone and refuses (editPorts) instead
// of writing the file back. A ports file left by a failure here is cleared
// by the next `create` of the name. A config entry's state goes through
// forgetEntryState (`config rm`).
//
// m is the deleted record, for its pin; nil when there is none to drop (a
// smol VM, a hub machine). See forgetHostKey.
func (a *App) forgetHostState(name string, m *config.Machine) error {
	unlock, err := config.LockPorts(a.Paths, name)
	if err != nil {
		return fmt.Errorf("%s: clear saved forwards: %w", name, err)
	}
	defer unlock()
	if err := config.RemovePorts(a.Paths, name); err != nil {
		return fmt.Errorf("%s: clear saved forwards: %w", name, err)
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
// known_hosts (only a provider server devvm made is pinned, sshFlags). The
// line is keyed by the host ssh actually connects to, so it is asked (`ssh
// -G` resolves an alias in the user's ssh config, and a non-22 port), and
// `ssh-keygen -R` removes it, hashed or not, rewriting the file through a
// temp file.
//
// A provider box is reached at its bare address on port 22, so ssh pinned
// it under the address (v4 or v6, unbracketed); that line is dropped too
// should the user's ssh config resolve the address to something else.
// Hetzner reuses addresses, and `delete` and `create` both clear it so the
// next server there is pinned fresh rather than refused.
func (a *App) forgetHostKey(m *config.Machine) error {
	if !m.IsRemote() || !m.Managed() || m.SSHHost == "" {
		return nil
	}
	kh := a.Paths.KnownHostsPath()
	if _, err := os.Stat(kh); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	key := knownHostsKey(m)
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		return nil // nothing could have pinned it without ssh either
	}
	keys := []string{key}
	if m.IsProvider() && m.Server.Address != "" && m.Server.Address != key {
		keys = append(keys, m.Server.Address)
	}
	for _, k := range keys {
		if out, err := exec.Command("ssh-keygen", "-R", k, "-f", kh).CombinedOutput(); err != nil {
			return fmt.Errorf("ssh-keygen -R %s: %v: %s", k, err, strings.TrimSpace(string(out)))
		}
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
// If -G fails, the destination's own host part and the entry's port stand in.
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
// its run files (session.Reap) as its entry goes. Failing that is
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
// devvm.toml is never written. The one confirmation is the hub's: its
// delete prompts through the forwarded terminal before destroying a real VM
// (and refuses with no terminal), so a second prompt here would only ask the
// same question twice. --yes given here goes to the hub's delete, and only
// then: without a terminal (no -t) the hub cannot ask.
func (a *App) deleteHubMachine(m *config.Machine, b backend.Backend, yes bool) error {
	hub, machine := m.Hub, m.HubMachineName()
	p, ok := b.(backend.Proxier)
	if !ok {
		return fmt.Errorf("%s: backend %q cannot proxy", m.Name, b.Kind())
	}
	argv := []string{"delete", machine}
	if yes {
		argv = append(argv, "--yes")
	}
	if err := a.runProxied(m, p, argv); err != nil {
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
