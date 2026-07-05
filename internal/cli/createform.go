package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/smweber/devvm/internal/backend"
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
		if err := resolveDisk(tty, interactive, s, defaults); err != nil {
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

	// Offer the optional fields (repos, ports, keys, hardening) only on a terminal;
	// the scripted path leaves them empty and uses the dedicated subcommands.
	if interactive {
		if err := askExtras(tty, s); err != nil {
			return err
		}
	}
	return nil
}

// resolveName settles the machine name: the positional arg if given, else a
// prompt (interactive) or an error (non-interactive), so `devvm create` runs bare.
func resolveName(s *createSpec) error {
	if s.Name != "" {
		return nil
	}
	tty := openTTY()
	if tty == nil || s.Yes {
		return fmt.Errorf("a machine name is required (usage: devvm create NAME)")
	}
	defer tty.Close()
	name := ""
	if err := form(tty, huh.NewInput().
		Title("Machine name").
		Value(&name).
		Validate(func(v string) error { return config.ValidName(strings.TrimSpace(v)) })); err != nil {
		return err
	}
	s.Name = strings.TrimSpace(name)
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

// resolveDisk settles smol's disk: flag, else global default, else prompt
// (interactive) or the compiled default (non-interactive). Unlike memory, disk
// always has a built-in fallback (SmolDefaultDiskGiB), so it never errors.
func resolveDisk(tty *os.File, interactive bool, s *createSpec, d *config.Defaults) error {
	if s.Disk != 0 { // flag wins
		return nil
	}
	if interactive {
		return askDisk(tty, s, d)
	}
	if d.Disk != 0 {
		s.Disk = d.Disk
	} else {
		s.Disk = backend.SmolDefaultDiskGiB
	}
	return nil
}

func askDisk(tty *os.File, s *createSpec, d *config.Defaults) error {
	pref := backend.SmolDefaultDiskGiB
	if d.Disk != 0 {
		pref = d.Disk
	}
	val := strconv.Itoa(pref)
	if err := form(tty, huh.NewInput().
		Title("VM disk (GiB)").
		Value(&val).
		Validate(func(v string) error {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("must be an integer number of GiB")
			}
			if n < 1 {
				return fmt.Errorf("must be at least 1 GiB")
			}
			return nil
		})); err != nil {
		return err
	}
	s.Disk, _ = strconv.Atoi(strings.TrimSpace(val))
	return nil
}

// askExtras prompts the optional create-time fields. Repos and ports are offered
// for any backend; github key-seeding is remote-only; hardening is remote-managed
// only (it's a no-op elsewhere — see runBootstrap). Each accepts a space-separated
// list and is safe to leave blank.
func askExtras(tty *os.File, s *createSpec) error {
	repos := strings.Join(s.Repos, " ")
	ports := strings.Join(s.Ports, " ")
	if err := form(tty,
		huh.NewInput().Title("Repos to clone (owner/repo or URL, space-separated)").
			Placeholder("me/app me/tools").Value(&repos),
		huh.NewInput().Title("Ports to forward (HOST:GUEST or PORT, space-separated)").
			Placeholder("3000 8080:80").Value(&ports),
	); err != nil {
		return err
	}
	s.Repos = fields(repos)
	s.Ports = fields(ports)

	if s.Backend == config.BackendRemoteManaged || s.Backend == config.BackendRemoteUnmanaged {
		keys := strings.Join(s.KeysGithub, " ")
		if err := form(tty, huh.NewInput().
			Title("Seed authorized_keys from GitHub users (space-separated handles)").
			Placeholder("alice bob").Value(&keys)); err != nil {
			return err
		}
		s.KeysGithub = fields(keys)
	}
	if s.Backend == config.BackendRemoteManaged {
		if err := form(tty,
			huh.NewConfirm().Title("Harden the box (ssh lockdown)?").Value(&s.Harden),
		); err != nil {
			return err
		}
		if s.Harden {
			if err := form(tty, huh.NewConfirm().Title("Install fail2ban?").Value(&s.Fail2ban)); err != nil {
				return err
			}
		}
	}
	return nil
}

// fields splits a space-separated list into a trimmed, non-empty slice (nil when
// blank, so an empty answer leaves the conf field unset).
func fields(s string) []string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return nil
	}
	return f
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
