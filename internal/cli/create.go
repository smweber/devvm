package cli

import (
	"errors"
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
	Backend  string // smol, or a provider (hetzner); "" asks, else smol
	Template string // --template; "" means [templates.default]

	// A provider box's placement flags, over the template's and the
	// [providers.NAME] table's (gatherProviderSpec).
	ServerType string
	Location   string
	Image      string

	// The template fields' flags (hook, memory, disk, repos).
	tmplFlags
	// tmpl is the template gatherCreateSpec resolved (flag > template >
	// built-in), patched by the prompts: what setup shapes with.
	tmpl config.Template

	Yes bool // skip prompts; resolve every unset field from flag > template > built-in
}

func (a *App) runCreate(s createSpec) error {
	defer config.TouchChanged(a.Paths) // wake `status --watch`
	if err := checkCreateBackend(s.Backend); err != nil {
		return err
	}
	// Backend first (providers.md §3): asked only on a terminal with a
	// provider configured; otherwise create makes a smol VM, as it always did.
	if err := a.askBackend(&s); err != nil {
		return err
	}
	if s.Backend == "" {
		s.Backend = config.BackendSmol
	}
	if s.Backend == config.BackendSmol {
		if err := s.refuseSmolPlacement(); err != nil {
			return err
		}
		// smolvm present and at the floor, before any prompt: a form filled
		// in for a create that cannot run is wasted.
		if err := backend.SmolReady(); err != nil {
			return err
		}
	}
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
	if config.IsProviderBackend(s.Backend) {
		return a.runCreateProvider(s, f)
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
	if s.tmpl.Memory < 512 {
		return fmt.Errorf("smol needs --memory >= 512 (MiB)")
	}

	// Allocate the VM, labelled as devvm's; setup then shapes it (a
	// mid-flight failure is resumable with `devvm setup`).
	if err := a.provisionResource(s.Name, s.tmpl.Memory, s.tmpl.Disk); err != nil {
		return err
	}
	// setup gets the template create settled (flags, prompts and all), so
	// it resolves nothing again.
	err = a.runSetup(s.Name, setupOpts{resolved: &s.tmpl})
	var si *setupIncomplete
	if err != nil && !errors.As(err, &si) {
		return fmt.Errorf("%s but setup failed: %w; re-run 'devvm setup %s'", wasCreated(s.Name), err, s.Name)
	}
	a.printCreateNext(s.Name, config.BackendSmol)
	if si != nil {
		return fmt.Errorf("%s; %w", wasCreated(s.Name), err) // created, with a problem
	}
	return nil
}

// refuseSmolPlacement refuses a provider's placement flags on a smol
// create: a smol VM has none, and ignoring them would make a typo'd
// --backend look like it worked.
func (s createSpec) refuseSmolPlacement() error {
	var set []string
	for _, f := range [][2]string{{"--server-type", s.ServerType}, {"--location", s.Location}, {"--image", s.Image}} {
		if f[1] != "" {
			set = append(set, f[0])
		}
	}
	if len(set) == 0 {
		return nil
	}
	return fmt.Errorf("%s place a provider server (--backend %s); a smol VM has no placement",
		strings.Join(set, ", "), strings.Join(config.KnownProviders(), "|"))
}

// checkCreateBackend refuses what create does not allocate, on this host,
// before a `create HUB/NAME` is proxied too: smol and the providers only.
// Remote create is gone (config.md §4): a remote box is an entry in
// devvm.toml, and shaping it is setup's.
func checkCreateBackend(b string) error {
	switch b {
	case "", config.BackendSmol, config.BackendHetzner:
		return nil
	case config.BackendRemote, config.BackendHub:
		return fmt.Errorf("create allocates local smol VMs only; add a remote box or hub with 'devvm config add NAME --backend remote|hub --host H'")
	default:
		return fmt.Errorf("invalid backend %q (create makes smol VMs and %s servers)", b, strings.Join(config.KnownProviders(), ", "))
	}
}

// provisionResource allocates the smol VM, leaving it running: create's
// allocation step. SmolCreate (create with devvm's label, then start). A
// provider box is allocated by runCreateProvider instead.
func (a *App) provisionResource(name string, memory, disk int) error {
	fmt.Fprintf(a.Stdout, "Using %d MiB RAM, %d GiB disk.\n", memory, disk)
	if err := backend.SmolCreate(name, memory, disk); err != nil {
		return err
	}
	a.clearLeftoverPorts(name)
	return nil
}

// wasCreated leads every error of a smol create that comes after its VM
// exists (setup, a clone). A laptop proxying `create HUB/NAME`
// reads it in the hub's output: it means the VM was made, so the laptop
// clears its own leftover HUB@NAME forwards even though the hub's create
// failed. No refusal says it.
func wasCreated(name string) string { return "'" + name + "' was created" }

// clearLeftoverPorts drops the saved forwards a same-named predecessor
// left on this host (a VM removed with raw smolvm, a server deleted in the
// provider console: nothing here saw it go) once a new box of the name
// exists, so its first attach does not raise them. create's one rule about
// forwards; for HUB/NAME the laptop runs it after the proxied create. A
// failure is one line: the box exists all the same.
func (a *App) clearLeftoverPorts(name string) {
	err := func() error {
		unlock, err := config.LockPorts(a.Paths, name)
		if err != nil {
			return err
		}
		defer unlock()
		return config.RemovePorts(a.Paths, name)
	}()
	if err != nil {
		fmt.Fprintf(a.Stderr, "devvm: %s: could not clear saved forwards left by an earlier %s (%v); see 'devvm ports list %s'\n", name, name, err, name)
	}
}

func (a *App) printCreateNext(name, backendName string) {
	fmt.Fprintf(a.Stdout, "\nMachine '%s' (%s) is ready.\n\nNext:\n", name, backendName)
	fmt.Fprintf(a.Stdout, "  devvm attach %s          # join the persistent dev tmux session\n", name)
	fmt.Fprintln(a.Stdout, "  (log in inside it: gh auth login, codex login; the browser opens where you attach from;")
	fmt.Fprintln(a.Stdout, "   share_auth = [\"github\"] in a template copies the gh login of the host that runs setup)")
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
