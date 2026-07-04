package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/smweber/devvm/internal/config"
)

// gatherCreateSpec fills any fields left unset by flags. On a terminal it prompts
// (via huh, bound to /dev/tty so it works even with redirected stdin); without a
// terminal it errors, naming the flag to pass — so create stays fully scriptable.
func (a *App) gatherCreateSpec(s *createSpec) error {
	tty := openTTY()
	if tty != nil {
		defer tty.Close()
	}

	if s.Backend == "" {
		if tty == nil {
			return fmt.Errorf("--backend is required (smol|remote-managed|remote-unmanaged)")
		}
		if err := form(tty, huh.NewSelect[string]().
			Title("Backend").
			Options(
				huh.NewOption("smol — new local microVM", config.BackendSmol),
				huh.NewOption("remote-managed — shape a remote host", config.BackendRemoteManaged),
				huh.NewOption("remote-unmanaged — adopt an existing host", config.BackendRemoteUnmanaged),
			).Value(&s.Backend)); err != nil {
			return err
		}
	}

	switch s.Backend {
	case config.BackendSmol:
		if s.Memory == 0 {
			if tty == nil {
				return fmt.Errorf("smol needs --memory MiB")
			}
			return askMemory(tty, s)
		}
	case config.BackendRemoteManaged, config.BackendRemoteUnmanaged:
		if s.SSHHost == "" {
			if tty == nil {
				return fmt.Errorf("remote backend needs --ssh-host")
			}
			return askRemote(tty, s)
		}
	default:
		// machine() reports an invalid backend value.
	}
	return nil
}

func askMemory(tty *os.File, s *createSpec) error {
	val := strconv.Itoa(suggestedMemoryMiB())
	if err := form(tty, huh.NewInput().
		Title("VM memory (MiB)").
		Value(&val).
		Validate(func(v string) error {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("must be an integer number of MiB")
			}
			if n < 512 {
				return fmt.Errorf("must be at least 512 MiB")
			}
			return nil
		})); err != nil {
		return err
	}
	s.Memory, _ = strconv.Atoi(strings.TrimSpace(val))
	return nil
}

func askRemote(tty *os.File, s *createSpec) error {
	port := ""
	if s.SSHPort != 0 {
		port = strconv.Itoa(s.SSHPort)
	}
	if s.Transport == "" {
		s.Transport = config.TransportSSH
	}
	if err := form(tty,
		huh.NewInput().Title("SSH host (host or user@host)").Value(&s.SSHHost).
			Validate(func(v string) error {
				if strings.TrimSpace(v) == "" {
					return fmt.Errorf("required")
				}
				return nil
			}),
		huh.NewInput().Title("SSH port").Placeholder("22").Value(&port),
		huh.NewInput().Title("SSH identity file (optional)").Value(&s.Identity),
		huh.NewSelect[string]().Title("Transport").
			Options(huh.NewOption("ssh", config.TransportSSH), huh.NewOption("mosh", config.TransportMosh)).
			Value(&s.Transport),
	); err != nil {
		return err
	}
	if p := strings.TrimSpace(port); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return fmt.Errorf("ssh port must be an integer")
		}
		s.SSHPort = n
	}
	return nil
}

// form runs one huh group over the controlling terminal.
func form(tty *os.File, fields ...huh.Field) error {
	return huh.NewForm(huh.NewGroup(fields...)).WithInput(tty).WithOutput(tty).Run()
}

// openTTY returns the controlling terminal, or nil when there isn't one (piped /
// CI), signalling the caller to require flags instead of prompting.
func openTTY() *os.File {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	return f
}
