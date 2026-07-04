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
// whatever's left unset.
type createSpec struct {
	Name      string
	Backend   string
	Memory    int
	SSHHost   string
	SSHPort   int
	Identity  string
	Transport string
	Provision string
}

func (a *App) runCreate(s createSpec) error {
	if err := config.ValidName(s.Name); err != nil {
		return err
	}
	if config.Exists(a.ConfigDir, s.Name) || backend.SmolExists(s.Name) {
		return fmt.Errorf("machine '%s' already exists", s.Name)
	}
	if err := a.gatherCreateSpec(&s); err != nil {
		return err
	}
	m, err := s.machine()
	if err != nil {
		return err
	}

	// Managed backends provision a resource; adopt backends just validate we can
	// reach the host before committing the config.
	switch m.Backend {
	case config.BackendSmol:
		if !backend.SmolAvailable() {
			return fmt.Errorf("smolvm is not installed; run bootstrap.sh on the host")
		}
		fmt.Printf("Using %d MiB RAM.\n", m.Memory)
		if err := backend.SmolCreate(s.Name, m.Memory); err != nil {
			return err
		}
	default:
		if err := a.probeRemote(m); err != nil {
			return err
		}
	}

	// Register before bootstrap so resolve() sees it (and a mid-flight failure is
	// resumable via `devvm bootstrap`).
	if err := m.Save(a.ConfigDir); err != nil {
		return err
	}
	if err := a.runBootstrap(s.Name); err != nil {
		return err
	}
	a.printCreateNext(m)
	return nil
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
	default:
		return nil, fmt.Errorf("invalid backend %q (want smol|remote-managed|remote-unmanaged)", s.Backend)
	}
	if s.Provision != "" {
		m.Provision = s.Provision
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// probeRemote confirms the host answers over ssh before we save its config, so
// adopting a typo'd or unreachable host fails loudly up front. Read-only.
func (a *App) probeRemote(m *config.Machine) error {
	b, err := backend.For(m, a.ConfigDir)
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
	fmt.Printf("\nMachine '%s' (%s) is ready.\n\nNext:\n", m.Name, m.Backend)
	if m.IsRemote() {
		fmt.Printf("  devvm authorize-key %s   # add a client key if needed\n", m.Name)
	}
	fmt.Printf("  devvm auth %s            # log in to github, codex, and claude\n", m.Name)
	fmt.Printf("  devvm repos %s           # after adding repos to the machine conf\n", m.Name)
	fmt.Printf("  devvm attach %s          # join the persistent dev tmux session\n", m.Name)
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
