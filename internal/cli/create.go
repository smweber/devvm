package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// createSpec is the flag- and prompt-collected input for `create`. Every field
// has a flag so create runs fully non-interactively; a terminal prompts for
// whatever's left unset, seeded from the effective template.
type createSpec struct {
	Name      string
	Backend   string
	Template  string // --template; "" means [templates.default]
	SSHHost   string
	SSHPort   int
	Identity  string
	Transport string

	// The template fields (hook, memory, disk, repos, ports, keys, harden,
	// fail2ban, user): flags here, then settled by gatherCreateSpec from
	// flag > template > built-in. Repos/Ports apply to any backend but a
	// hub (Ports are saved as this host's state, config.SavePorts, not in
	// the conf); keys and hardening are remote-only (harden is
	// remote-managed only).
	tmplFlags
	KeysGithub []string
	KeyFiles   []string

	Yes bool // skip prompts; resolve every unset field from flag > template > built-in
}

func (a *App) runCreate(s createSpec) error {
	defer config.TouchChanged(a.Paths) // wake `status --watch`
	// Name may be omitted on the command line; prompt for it before anything else
	// so `devvm create` can be run bare on a terminal.
	if err := resolveName(&s); err != nil {
		return err
	}
	if _, _, onHub, err := config.SplitHubName(s.Name); err != nil {
		return err
	} else if onHub {
		// `create HUB/NAME` is proxied by createCmd before it gets here (the
		// hub writes the conf; nothing about it lives on this host), so this
		// is reachable only from a direct call. The prompt path cannot
		// produce a HUB/NAME: resolveName validates with ValidName.
		return fmt.Errorf("create %s: %w", s.Name, backend.ErrHubProxy)
	}
	if err := config.ValidName(s.Name); err != nil {
		return err
	}
	// A registry entry refuses the name whatever it is (it would shadow the
	// new VM, config.md §2), and so does a local smol VM of that name, made
	// by devvm or not: smolvm cannot rename one.
	if config.Exists(a.Paths.Config, s.Name) {
		if ex, _ := config.Load(a.Paths.Config, s.Name); ex != nil && !ex.IsRemote() && !backend.SmolExists(s.Name) {
			return fmt.Errorf("machine '%s' has a stale conf with no VM; 'devvm delete %s' drops it first", s.Name, s.Name)
		}
		return fmt.Errorf("machine '%s' already exists", s.Name)
	}
	if backend.SmolExists(s.Name) {
		return fmt.Errorf("machine '%s' already exists (a local smol VM)", s.Name)
	}
	if err := a.gatherCreateSpec(&s); err != nil {
		return err
	}
	m, err := s.machine()
	if err != nil {
		return err
	}

	// Allocate the resource (adopt backends just probe), then register before
	// setup so resolve() sees it (and a mid-flight failure is resumable via
	// `devvm setup`).
	if err := a.provisionResource(m); err != nil {
		return err
	}
	if m.IsHub() {
		// A hub is registered, never shaped: no setup. The one extra check
		// is that its devvm is usable at all (installed, on the login shell's
		// PATH, and not older than the hub surface), before the conf exists.
		b, err := backend.For(m, a.Paths)
		if err != nil {
			return err
		}
		if err := a.checkHubVersion(context.Background(), b, m); err != nil {
			return err
		}
		// Clear forwards a same-named predecessor left for machines on it
		// (its conf removed by hand, never `delete`), before the conf
		// exists: they would come back as this hub's, and make `delete
		// HUB` refuse.
		stale, err := config.SavedPortsNames(a.Paths)
		if err != nil {
			return err
		}
		for _, display := range stale {
			if hub, _, ok, _ := config.SplitHubName(display); ok && hub == m.Name {
				if err := a.forgetHostState(display, nil); err != nil {
					return err
				}
			}
		}
		if err := m.Save(a.Paths.Config); err != nil {
			return err
		}
		a.printCreateNext(m)
		return nil
	}
	// Forwards are host state, not conf (config.md §5). Writing the list
	// always, empty included, also clears a stale file a same-named
	// predecessor left (a hand-removed conf never ran `delete`). Before the
	// conf, so a failure here registers nothing; the VM provisionResource
	// made (smol) stays, and the error names the mappings to re-add by hand.
	// Unless this is a hub running a laptop's proxied create: those
	// forwards are the laptop's, and it seeds them itself.
	if s.savesPorts() {
		if err := a.editPorts(m, func([]string) []string { return s.Ports }); err != nil {
			return fmt.Errorf("save forwards %v for '%s' (not registered): %w", s.Ports, m.Name, err)
		}
	}
	if err := m.Save(a.Paths.Config); err != nil {
		return err
	}
	if m.IsRemote() && !m.Managed() {
		// An adopted host is never shaped: setup refuses it.
		b, err := backend.For(m, a.Paths)
		if err != nil {
			return err
		}
		if err := a.checkAdoptedRemote(m, b); err != nil {
			return err
		}
		a.printCreateNext(m)
		return nil
	}
	// setup gets the template create settled (flags, prompts and all), so
	// it resolves nothing again, and treats the box as fresh: a ports file
	// a same-named predecessor left is replaced.
	t := s.template()
	if err := a.runSetup(s.Name, setupOpts{resolved: &t, fresh: true,
		tmplFlags: tmplFlags{Keys: s.Keys, NoPorts: !s.savesPorts()}}); err != nil {
		return fmt.Errorf("'%s' was created but setup failed: %w; re-run 'devvm setup %s'", s.Name, err, s.Name)
	}
	a.printCreateNext(m)
	return nil
}

// template is the settled spec as the template setup shapes with.
func (s createSpec) template() config.Template {
	name := s.Template
	if name == "" {
		name = config.DefaultTemplate
	}
	user := s.User
	if user == "" {
		user = config.DefaultTemplateUser
	}
	hook := s.Hook
	if hook == "" {
		hook = config.DefaultBootstrapHook
	}
	return config.Template{
		Name: name, User: user, Hook: hook, Memory: s.Memory, Disk: s.Disk,
		Repos: s.Repos, Ports: s.Ports,
		AuthorizedKeysGithub: s.KeysGithub, AuthorizedKeys: s.KeyFiles,
		Harden: s.Harden, Fail2ban: s.Fail2ban,
	}
}

// provisionResource allocates the backend resource, leaving it running: create's
// allocation step. smol calls SmolCreate (create + start) and writes the
// managed marker; adopt/remote backends just probe reachability — the future
// hetzner backend adds API-backed create here.
func (a *App) provisionResource(m *config.Machine) error {
	if m.IsRemote() {
		return a.probeRemote(m)
	}
	if !backend.SmolAvailable() {
		return fmt.Errorf("smolvm is not installed; run bootstrap.sh on the host")
	}
	disk := m.Disk
	if disk <= 0 {
		disk = backend.SmolDefaultDiskGiB
	}
	fmt.Fprintf(a.Stdout, "Using %d MiB RAM, %d GiB disk.\n", m.Memory, disk)
	if err := backend.SmolCreate(m.Name, m.Memory, m.Disk); err != nil {
		return err
	}
	// A failure leaves the VM made but unmanaged, which the error says;
	// nothing is installed into it.
	if err := a.writeManagedMarker(m.Name); err != nil {
		return fmt.Errorf("created VM '%s' but could not mark it managed: %w; %s", m.Name, err, backend.AdoptHint(m.Name))
	}
	return nil
}

// writeManagedMarker records that devvm owns the smol VM NAME (config.md
// §2): its created_at, read back from smolvm, so a VM later deleted and
// recreated under the same name behind devvm's back is never mistaken for
// this one. create and `setup --adopt` are its only callers.
func (a *App) writeManagedMarker(name string) error {
	sm, ok, err := backend.SmolLookup(name)
	if err == nil && !ok {
		err = fmt.Errorf("smolvm does not list it")
	}
	if err == nil {
		err = config.WriteManagedMarker(a.Paths, name, string(sm.CreatedAt))
	}
	return err
}

// machine builds and validates the registry entry from the gathered spec.
func (s createSpec) machine() (*config.Machine, error) {
	var m *config.Machine
	switch s.Backend {
	case config.BackendSmol:
		if s.Memory < 512 {
			return nil, fmt.Errorf("smol needs --memory >= 512 (MiB)")
		}
		m = config.NewSmol(s.Name)
		m.Memory = s.Memory
		m.Disk = s.Disk
	case config.BackendHub:
		if s.SSHHost == "" {
			return nil, fmt.Errorf("hub backend needs --ssh-host")
		}
		m = config.NewHub(s.Name, s.SSHHost)
		if s.SSHPort != 0 {
			m.SSHPort = s.SSHPort
		}
		m.Identity = s.Identity
		if s.Transport != "" {
			m.Transport = s.Transport
		}
		// Nothing else applies to a hub: memory, disk, repos, ports, keys, the
		// hook and hardening belong to the machines on it. Rather than silently
		// dropping a flag that was passed, record everything the spec carries
		// and let Validate refuse it with the same field list a hand-edited
		// conf would get (validateHub).
		m.Memory = s.Memory
		m.Disk = s.Disk
		m.AuthorizedKeysGithub = s.KeysGithub
		m.AuthorizedKeys = s.KeyFiles
		m.Harden = s.Harden
		m.Fail2ban = s.Fail2ban
		m.LegacyPorts = s.Ports // only so validateHub names it; never saved
	case config.BackendRemoteManaged, config.BackendRemoteUnmanaged:
		if s.SSHHost == "" {
			return nil, fmt.Errorf("remote backend needs --ssh-host")
		}
		m = config.NewRemote(s.Name, s.Backend, s.SSHHost)
		if s.SSHPort != 0 {
			m.SSHPort = s.SSHPort
		}
		m.Identity = s.Identity
		if s.Transport != "" {
			m.Transport = s.Transport
		}
		m.AuthorizedKeysGithub = s.KeysGithub
		m.AuthorizedKeys = s.KeyFiles
		// Hardening runs only on managed remotes (see shape); recording it
		// on an adopt host would be a silent no-op, so keep it managed-only.
		if s.Backend == config.BackendRemoteManaged {
			m.Harden = s.Harden
			m.Fail2ban = s.Fail2ban
		}
	default:
		return nil, fmt.Errorf("invalid backend %q (want smol|remote-managed|remote-unmanaged|hub)", s.Backend)
	}
	// The old conf records what create used (hook, repos, keys, harden):
	// setup's conf layer replays it until step 5 drops per-machine confs.
	m.Repos = s.Repos
	if s.Hook != "" {
		m.BootstrapHook = s.Hook
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// probeRemote confirms the host answers over ssh before we save its config, so
// adopting a typo'd or unreachable host fails loudly up front. Read-only. It
// does not wait or try root: config.md step 5 removes remote create, and
// `setup`'s bounded wait and root adoption cover a box that is not ready yet.
func (a *App) probeRemote(m *config.Machine) error {
	b, err := backend.For(m, a.Paths)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "devvm: checking ssh to %s...\n", m.SSHHost)
	if err := b.Run(context.Background(), backend.ExecOpts{Stdout: io.Discard, Stderr: io.Discard}, "true"); err != nil {
		return fmt.Errorf("cannot reach %q over ssh: %w", m.SSHHost, err)
	}
	return nil
}

func (a *App) printCreateNext(m *config.Machine) {
	if m.IsHub() {
		fmt.Fprintf(a.Stdout, "\nHub '%s' is registered (%s).\n\nIts machines are addressed as %s/NAME:\n", m.Name, m.SSHHost, m.Name)
		fmt.Fprintf(a.Stdout, "  %-24s # lists the hub and its machines\n", "devvm status")
		fmt.Fprintf(a.Stdout, "  %-24s # join a machine's dev tmux session\n", "devvm attach "+m.Name+"/NAME")
		return
	}
	fmt.Fprintf(a.Stdout, "\nMachine '%s' (%s) is ready.\n\nNext:\n", m.Name, m.Backend)
	if m.IsRemote() {
		fmt.Fprintf(a.Stdout, "  devvm keys add %s        # add a client key if needed\n", m.Name)
	}
	fmt.Fprintf(a.Stdout, "  devvm auth %s            # log in to github, codex, and claude\n", m.Name)
	fmt.Fprintf(a.Stdout, "  devvm repos add %s owner/repo  # add + clone a repo\n", m.Name)
	fmt.Fprintf(a.Stdout, "  devvm attach %s          # join the persistent dev tmux session\n", m.Name)
}

// suggestedMemoryMiB is half of host RAM, clamped to [1024, 2048].
func suggestedMemoryMiB() int {
	total := hostMemoryMiB()
	half := total / 2
	switch {
	case half > 2048:
		return 2048
	case half < 1024:
		return 1024
	default:
		return half
	}
}

func hostMemoryMiB() int {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err == nil {
			if bytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
				return int(bytes / 1024 / 1024)
			}
		}
		return 2048
	}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 2048
	}
	for _, line := range strings.Split(string(data), "\n") {
		if kb, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			fields := strings.Fields(kb)
			if len(fields) > 0 {
				if n, err := strconv.Atoi(fields[0]); err == nil {
					return n / 1024
				}
			}
		}
	}
	return 2048
}
