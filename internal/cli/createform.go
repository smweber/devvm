package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/smweber/devvm/internal/bootstrap"
	"github.com/smweber/devvm/internal/config"
)

// gatherCreateSpec fills any field left unset by a flag, resolving each with the
// precedence flag > config.toml global default > compiled built-in. On a terminal
// (and without --yes) it prompts, pre-selecting that resolved default; otherwise
// it resolves silently and errors — naming the flag — on a required field that
// has no default. So create is fully scriptable and predictable either way.
func (a *App) gatherCreateSpec(s *createSpec) error {
	defaults, err := config.LoadDefaults(a.ConfigDir)
	if err != nil {
		return err
	}

	tty := openTTY()
	if tty != nil {
		defer tty.Close()
	}
	// --yes forces the non-interactive path even on a terminal.
	interactive := tty != nil && !s.Yes

	// Backend is required and has no global default.
	if s.Backend == "" {
		if !interactive {
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
		if err := resolveMemory(tty, interactive, s, defaults); err != nil {
			return err
		}
	case config.BackendRemoteManaged, config.BackendRemoteUnmanaged:
		if err := resolveRemote(tty, interactive, s, defaults); err != nil {
			return err
		}
	default:
		// machine() reports an invalid backend value.
	}

	// The bootstrap-hook runs only on managed boxes (adopt hosts are never shaped),
	// so only offer it there.
	if s.Backend == config.BackendSmol || s.Backend == config.BackendRemoteManaged {
		if err := resolveBootstrapHook(tty, interactive, s, defaults); err != nil {
			return err
		}
	}
	return nil
}

// resolveMemory settles smol's required memory: flag, else global default, else
// prompt (interactive) or error (non-interactive) — smol has no universal default.
func resolveMemory(tty *os.File, interactive bool, s *createSpec, d *config.Defaults) error {
	if s.Memory != 0 { // flag wins
		return nil
	}
	if interactive {
		return askMemory(tty, s, d)
	}
	if d.Memory != 0 {
		s.Memory = d.Memory
		return nil
	}
	return fmt.Errorf("smol needs --memory MiB (or set memory in config.toml)")
}

func askMemory(tty *os.File, s *createSpec, d *config.Defaults) error {
	pref := suggestedMemoryMiB()
	if d.Memory != 0 {
		pref = d.Memory
	}
	val := strconv.Itoa(pref)
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

// resolveRemote settles the remote fields. Transport is seeded from the global
// default (both paths); ssh-host is required with no default, so it prompts
// interactively or errors otherwise.
func resolveRemote(tty *os.File, interactive bool, s *createSpec, d *config.Defaults) error {
	if s.Transport == "" {
		s.Transport = d.Transport // may stay "" → applyDefaults fills ssh
	}
	if s.SSHHost != "" { // flag given: keep the rest at their defaults
		return nil
	}
	if !interactive {
		return fmt.Errorf("remote backend needs --ssh-host")
	}
	return askRemote(tty, s)
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

// resolveBootstrapHook settles the bootstrap-hook: flag wins; otherwise prompt
// (interactive, pre-selecting the global-or-none default) or take the global
// default silently. An empty result means "none" via applyDefaults.
func resolveBootstrapHook(tty *os.File, interactive bool, s *createSpec, d *config.Defaults) error {
	if s.BootstrapHook != "" { // flag wins
		return nil
	}
	if !interactive {
		s.BootstrapHook = d.BootstrapHook // "" → applyDefaults fills "none"
		return nil
	}
	return askBootstrapHook(tty, s, d)
}

func askBootstrapHook(tty *os.File, s *createSpec, d *config.Defaults) error {
	const (
		optDefault = "default"
		optNone    = "none"
		optCustom  = "custom"
	)
	choice := optNone
	var opts []huh.Option[string]
	// Offer the config.toml default (pre-selected) only when it's set to something
	// other than "none" — otherwise it's identical to the plain "none" option.
	if d.BootstrapHook != "" && d.BootstrapHook != bootstrap.KindNone {
		opts = append(opts, huh.NewOption("default (config.toml): "+d.BootstrapHook, optDefault))
		choice = optDefault
	}
	opts = append(opts,
		huh.NewOption("none — skip the hook", optNone),
		huh.NewOption("custom — enter a url:/cmd: spec", optCustom),
	)
	if err := form(tty, huh.NewSelect[string]().Title("Bootstrap hook").Options(opts...).Value(&choice)); err != nil {
		return err
	}
	switch choice {
	case optDefault:
		s.BootstrapHook = d.BootstrapHook
	case optNone:
		s.BootstrapHook = bootstrap.KindNone
	case optCustom:
		custom := ""
		if err := form(tty, huh.NewInput().
			Title("Bootstrap-hook spec").
			Placeholder("url:<URL> [args] | cmd:<path> [args] | none").
			Value(&custom).
			Validate(func(v string) error {
				_, err := bootstrap.ParseSpec(strings.TrimSpace(v))
				return err
			})); err != nil {
			return err
		}
		s.BootstrapHook = strings.TrimSpace(custom)
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
