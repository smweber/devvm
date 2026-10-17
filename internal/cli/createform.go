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
// precedence flag > --template > [templates.default] > config.toml (deprecated)
// > built-in. On a terminal (and without --yes) it prompts, pre-selecting that
// resolved value; otherwise it resolves silently and errors — naming the flag —
// on a required field that has no default. So create is fully scriptable and
// predictable either way.
func (a *App) gatherCreateSpec(s *createSpec) error {
	defaults, err := config.LoadDefaults(a.Paths.Config)
	if err != nil {
		return err
	}

	tty := openTTY()
	if tty != nil {
		defer tty.Close()
	}
	// --yes forces the non-interactive path even on a terminal.
	interactive := tty != nil && !s.Yes

	// Backend: smol unless asked; the form's first question on a terminal.
	if s.Backend == "" {
		s.Backend = config.BackendSmol
		if interactive {
			if err := form(tty, huh.NewSelect[string]().
				Title("Backend").
				Options(
					huh.NewOption("smol — new local microVM", config.BackendSmol),
					huh.NewOption("remote-managed — shape a remote host", config.BackendRemoteManaged),
					huh.NewOption("remote-unmanaged — adopt an existing host", config.BackendRemoteUnmanaged),
					huh.NewOption("hub — another host running devvm; reach its machines as NAME/…", config.BackendHub),
				).Value(&s.Backend)); err != nil {
				return err
			}
		}
	}

	// The template shapes only boxes devvm sets up: an adopted host is never
	// shaped and a hub is not a machine, so neither reads one (a hub conf
	// carrying memory or repos would be refused).
	shaped := s.Backend == config.BackendSmol || s.Backend == config.BackendRemoteManaged
	var t config.Template
	if shaped {
		if t, err = a.resolveTemplate(s.Template, s.tmplFlags, nil); err != nil {
			return err
		}
	} else if s.Template != "" || s.Inline {
		return fmt.Errorf("--template applies only to boxes devvm sets up (smol, remote-managed), not %s", s.Backend)
	}

	switch s.Backend {
	case config.BackendSmol:
		if err := resolveMemory(tty, interactive, s, t); err != nil {
			return err
		}
		if err := resolveDisk(tty, interactive, s, t); err != nil {
			return err
		}
	case config.BackendRemoteManaged, config.BackendRemoteUnmanaged, config.BackendHub:
		// A hub is reached over the same ssh fields as a remote box.
		if err := resolveRemote(tty, interactive, s, defaults); err != nil {
			return err
		}
	default:
		// machine() reports an invalid backend value.
	}

	if shaped {
		if err := resolveHook(tty, interactive, s, t); err != nil {
			return err
		}
		// The rest come from the template (flags already folded in); the
		// form below offers them for editing, pre-filled.
		s.User = t.User
		s.Repos, s.Ports = t.Repos, t.Ports
		s.KeysGithub, s.KeyFiles = t.AuthorizedKeysGithub, t.AuthorizedKeys
		s.Harden, s.Fail2ban = t.Harden, t.Fail2ban
	}

	// Offer the optional fields (repos, ports, keys, hardening) only on a terminal;
	// the scripted path takes the template's. A hub has none of them: they
	// belong to the machines on it.
	if !s.savesPorts() {
		s.Ports = nil // the proxying laptop's (HUB@NAME), seeded there
	}
	if interactive && s.Backend != config.BackendHub {
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

// resolveMemory settles smol's required memory: flag, else the template, else
// prompt (interactive) or error (non-interactive) — smol has no universal default.
func resolveMemory(tty *os.File, interactive bool, s *createSpec, t config.Template) error {
	if s.Memory != 0 { // flag wins
		return nil
	}
	if interactive {
		return askMemory(tty, s, t)
	}
	if t.Memory != 0 {
		s.Memory = t.Memory
		return nil
	}
	return fmt.Errorf("smol needs --memory MiB (or memory in the template, [templates.%s] in devvm.toml)", t.Name)
}

func askMemory(tty *os.File, s *createSpec, t config.Template) error {
	pref := suggestedMemoryMiB()
	if t.Memory != 0 {
		pref = t.Memory
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

// resolveDisk settles smol's disk: flag, else the template, else prompt
// (interactive) or the compiled default (non-interactive). Unlike memory, disk
// always has a built-in fallback (SmolDefaultDiskGiB), so it never errors.
func resolveDisk(tty *os.File, interactive bool, s *createSpec, t config.Template) error {
	if s.Disk != 0 { // flag wins
		return nil
	}
	if interactive {
		return askDisk(tty, s, t)
	}
	if t.Disk != 0 {
		s.Disk = t.Disk
	} else {
		s.Disk = backend.SmolDefaultDiskGiB
	}
	return nil
}

func askDisk(tty *os.File, s *createSpec, t config.Template) error {
	pref := backend.SmolDefaultDiskGiB
	if t.Disk != 0 {
		pref = t.Disk
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
// only (setup ignores it elsewhere — see shape). Each accepts a space-separated
// list and is safe to leave blank.
func askExtras(tty *os.File, s *createSpec) error {
	repos := strings.Join(s.Repos, " ")
	ports := strings.Join(s.Ports, " ")
	inputs := []huh.Field{huh.NewInput().Title("Repos to clone (owner/repo or URL, space-separated)").
		Placeholder("me/app me/tools").Value(&repos)}
	// No ports prompt on a hub running a laptop's create: the forwards are
	// the laptop's, which seeds them from its own --port.
	if s.savesPorts() {
		inputs = append(inputs, huh.NewInput().Title("Ports to forward (HOST:GUEST or PORT, space-separated)").
			Placeholder("3000 8080:80").Value(&ports))
	}
	if err := form(tty, inputs...); err != nil {
		return err
	}
	s.Repos = fields(repos)
	if s.savesPorts() {
		s.Ports = fields(ports)
	}

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

// resolveHook settles the hook: flag wins; otherwise prompt (interactive,
// pre-selecting the template's) or take the template's silently.
func resolveHook(tty *os.File, interactive bool, s *createSpec, t config.Template) error {
	if s.Hook != "" { // flag wins
		return nil
	}
	if !interactive {
		s.Hook = t.Hook
		return nil
	}
	return askHook(tty, s, t)
}

func askHook(tty *os.File, s *createSpec, t config.Template) error {
	const (
		optDefault = "default"
		optNone    = "none"
		optCustom  = "custom"
	)
	choice := optNone
	var opts []huh.Option[string]
	// Offer the template's hook (pre-selected) only when it's something
	// other than "none" — otherwise it's identical to the plain "none" option.
	if t.Hook != "" && t.Hook != bootstrap.KindNone {
		opts = append(opts, huh.NewOption("template "+t.Name+": "+t.Hook, optDefault))
		choice = optDefault
	}
	opts = append(opts,
		huh.NewOption("none — skip the hook", optNone),
		huh.NewOption("custom — enter a url:/cmd: spec", optCustom),
	)
	if err := form(tty, huh.NewSelect[string]().Title("Hook").Options(opts...).Value(&choice)); err != nil {
		return err
	}
	switch choice {
	case optDefault:
		s.Hook = t.Hook
	case optNone:
		s.Hook = bootstrap.KindNone
	case optCustom:
		custom := ""
		if err := form(tty, huh.NewInput().
			Title("Hook spec").
			Placeholder("url:<URL> [args] | cmd:<path> [args] | none").
			Value(&custom).
			Validate(func(v string) error {
				_, err := bootstrap.ParseSpec(strings.TrimSpace(v))
				return err
			})); err != nil {
			return err
		}
		s.Hook = strings.TrimSpace(custom)
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
