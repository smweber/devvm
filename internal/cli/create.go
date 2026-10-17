package cli

import (
	"fmt"
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
	Name     string
	Backend  string // smol, the only one devvm allocates (config.md §9 item 4)
	Template string // --template; "" means [templates.default]

	// The template fields (hook, memory, disk, repos, ports, user): flags
	// here, then settled by gatherCreateSpec from flag > template >
	// built-in. Ports are saved as this host's state (config.SavePorts).
	tmplFlags
	KeysGithub []string
	KeyFiles   []string

	Yes bool // skip prompts; resolve every unset field from flag > template > built-in
}

func (a *App) runCreate(s createSpec) error {
	defer config.TouchChanged(a.Paths) // wake `status --watch`
	if err := checkCreateBackend(s.Backend); err != nil {
		return err
	}
	s.Backend = config.BackendSmol
	// Name may be omitted on the command line; prompt for it before anything else
	// so `devvm create` can be run bare on a terminal.
	if err := resolveName(&s); err != nil {
		return err
	}
	if _, _, onHub, err := config.SplitHubName(s.Name); err != nil {
		return err
	} else if onHub {
		// `create HUB/NAME` is proxied by createCmd before it gets here (the
		// hub makes the VM; nothing about it lives on this host), so this
		// is reachable only from a direct call. The prompt path cannot
		// produce a HUB/NAME: resolveName validates with ValidName.
		return fmt.Errorf("create %s: %w", s.Name, backend.ErrHubProxy)
	}
	if err := config.ValidName(s.Name); err != nil {
		return err
	}
	// A config entry refuses the name (it would shadow the new VM, config.md
	// §2), and so does a local smol VM of that name, made by devvm or not:
	// smolvm cannot rename one. A file that does not parse cannot say, so it
	// refuses too.
	f, err := config.LoadFile(a.Paths)
	if err != nil {
		return err
	}
	if _, ok := f.Entry(s.Name); ok {
		return fmt.Errorf("'%s' is an entry in %s ([machines.%s]); a smol VM of that name would be shadowed by it: pick another name, or 'devvm config rm %s' first",
			s.Name, f.Path, s.Name, s.Name)
	}
	if backend.SmolExists(s.Name) {
		return fmt.Errorf("machine '%s' already exists (a local smol VM)", s.Name)
	}
	if err := a.gatherCreateSpec(&s); err != nil {
		return err
	}
	if s.Memory < 512 {
		return fmt.Errorf("smol needs --memory >= 512 (MiB)")
	}

	// Allocate the VM and write its managed marker; setup then shapes it
	// (a mid-flight failure is resumable with `devvm setup`).
	if err := a.provisionResource(s.Name, s.Memory, s.Disk); err != nil {
		return err
	}
	// setup gets the template create settled (flags, prompts and all), so
	// it resolves nothing again, and treats the box as fresh: a ports file
	// a same-named predecessor left is replaced.
	t := s.template()
	if err := a.runSetup(s.Name, setupOpts{resolved: &t, fresh: true,
		tmplFlags: tmplFlags{Keys: s.Keys, NoPorts: !s.savesPorts()}}); err != nil {
		return fmt.Errorf("'%s' was created but setup failed: %w; re-run 'devvm setup %s'", s.Name, err, s.Name)
	}
	a.printCreateNext(s.Name)
	return nil
}

// checkCreateBackend refuses anything but smol, on this host, before a
// `create HUB/NAME` is proxied too. Remote create is gone (config.md §4): a
// remote box is an entry in devvm.toml, and shaping it is setup's.
func checkCreateBackend(b string) error {
	switch b {
	case "", config.BackendSmol:
		return nil
	case config.BackendRemote, config.BackendHub, "remote-managed", "remote-unmanaged":
		return fmt.Errorf("create allocates local smol VMs only; add a remote box or hub with 'devvm config add NAME --backend remote|hub --host H', then 'devvm setup NAME' for a managed one")
	default:
		return fmt.Errorf("invalid backend %q (create makes smol VMs)", b)
	}
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

// provisionResource allocates the smol VM, leaving it running: create's
// allocation step. SmolCreate (create + start), then the managed marker.
// A provider backend adds its API-backed create here (config.md §9).
func (a *App) provisionResource(name string, memory, disk int) error {
	if !backend.SmolAvailable() {
		return fmt.Errorf("smolvm is not installed; run bootstrap.sh on the host")
	}
	if disk <= 0 {
		disk = backend.SmolDefaultDiskGiB
	}
	fmt.Fprintf(a.Stdout, "Using %d MiB RAM, %d GiB disk.\n", memory, disk)
	if err := backend.SmolCreate(name, memory, disk); err != nil {
		return err
	}
	// A failure leaves the VM made but unmanaged, which the error says;
	// nothing is installed into it.
	if err := a.writeManagedMarker(name); err != nil {
		return fmt.Errorf("created VM '%s' but could not mark it managed: %w; %s", name, err, backend.AdoptHint(name))
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

func (a *App) printCreateNext(name string) {
	fmt.Fprintf(a.Stdout, "\nMachine '%s' (smol) is ready.\n\nNext:\n", name)
	fmt.Fprintf(a.Stdout, "  devvm auth %s            # log in to github, codex, and claude\n", name)
	fmt.Fprintf(a.Stdout, "  devvm repos add %s owner/repo  # clone a repo into ~/src\n", name)
	fmt.Fprintf(a.Stdout, "  devvm attach %s          # join the persistent dev tmux session\n", name)
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
