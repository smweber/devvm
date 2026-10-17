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
// precedence flag > --template > [templates.default] > built-in. On a
// terminal (and without --yes) it prompts, pre-selecting that resolved value;
// otherwise it resolves silently and errors — naming the flag — on a required
// field that has no default. So create is fully scriptable and predictable
// either way.
func (a *App) gatherCreateSpec(s *createSpec) error {
	tty := openTTY()
	if tty != nil {
		defer tty.Close()
	}
	// --yes forces the non-interactive path even on a terminal.
	interactive := tty != nil && !s.Yes

	t, err := a.resolveTemplate(s.Template, s.tmplFlags)
	if err != nil {
		return err
	}
	if err := resolveMemory(tty, interactive, s, t); err != nil {
		return err
	}
	if err := resolveDisk(tty, interactive, s, t); err != nil {
		return err
	}
	if err := resolveHook(tty, interactive, s, t); err != nil {
		return err
	}
	// The rest come from the template (flags already folded in); the form
	// below offers repos and ports for editing, pre-filled.
	s.User = t.User
	s.Repos, s.Ports = t.Repos, t.Ports
	s.KeysGithub, s.KeyFiles = t.AuthorizedKeysGithub, t.AuthorizedKeys
	s.Harden, s.Fail2ban = t.Harden, t.Fail2ban
	if !s.savesPorts() {
		s.Ports = nil // the proxying laptop's (HUB@NAME), seeded there
	}
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

// askExtras prompts the optional create-time fields, repos and ports. Each
// accepts a space-separated list and is safe to leave blank.
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
	return nil
}

// fields splits a space-separated list into a trimmed, non-empty slice (nil when
// blank, so an empty answer leaves the field unset).
func fields(s string) []string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return nil
	}
	return f
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
// CI), signalling the caller to require flags instead of prompting. A var
// so a test can hand create a terminal.
var openTTY = func() *os.File {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	return f
}
