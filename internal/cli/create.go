package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

func (a *App) runCreate(name string, memory int) error {
	if err := config.ValidName(name); err != nil {
		return err
	}
	if !backend.SmolAvailable() {
		return fmt.Errorf("smolvm is not installed; run bootstrap.sh on the host")
	}
	if config.Exists(a.ConfigDir, name) || backend.SmolExists(name) {
		return fmt.Errorf("machine '%s' already exists", name)
	}

	mem, err := chooseMemoryMiB(memory)
	if err != nil {
		return err
	}
	fmt.Printf("Using %d MiB RAM.\n", mem)
	if err := backend.SmolCreate(name, mem); err != nil {
		return err
	}

	// Register before provisioning so resolve() sees it.
	m := config.NewSmol(name)
	m.Memory = mem
	if err := m.Save(a.ConfigDir); err != nil {
		return err
	}

	if err := a.runBootstrap(name); err != nil {
		return err
	}

	fmt.Printf(`
Machine '%s' is ready.

Next:
  devvm auth %s        # log in to github, codex, and claude
  devvm repos %s       # after adding repos to the machine conf
  devvm attach %s      # join the persistent dev tmux session
`, name, name, name, name)
	return nil
}

// chooseMemoryMiB resolves the requested MiB or prompts with a suggestion.
func chooseMemoryMiB(requested int) (int, error) {
	if requested == 0 {
		suggested := suggestedMemoryMiB()
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return 0, fmt.Errorf("no terminal available; pass --memory MiB")
		}
		defer tty.Close()
		fmt.Fprintf(tty, "VM memory in MiB [%d]: ", suggested)
		line, _ := bufio.NewReader(tty).ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			requested = suggested
		} else {
			n, err := strconv.Atoi(line)
			if err != nil {
				return 0, fmt.Errorf("memory must be an integer number of MiB")
			}
			requested = n
		}
	}
	if requested < 512 {
		return 0, fmt.Errorf("memory must be at least 512 MiB")
	}
	return requested, nil
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
