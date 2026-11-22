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

	var err error
	if s.tmpl, err = a.resolveTemplate(s.Template, s.tmplFlags); err != nil {
		return err
	}
	if err := resolveMemory(tty, interactive, s); err != nil {
		return err
	}
	if err := resolveDisk(tty, interactive, s); err != nil {
		return err
	}
	if err := resolveHook(tty, interactive, s); err != nil {
		return err
	}
	// The rest stay the template's (flags already folded in); the form
	// below offers repos for editing, pre-filled.
	if interactive {
		if err := askExtras(tty, &s.tmpl); err != nil {
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
func resolveMemory(tty *os.File, interactive bool, s *createSpec) error {
	if s.Memory != 0 { // flag wins (resolveTemplate folded it in)
		return nil
	}
	if interactive {
		return askMemory(tty, &s.tmpl)
	}
	if s.tmpl.Memory != 0 {
		return nil
	}
	return fmt.Errorf("smol needs --memory MiB (or memory in the template, [templates.%s] in devvm.toml)", s.tmpl.Name)
}

func askMemory(tty *os.File, t *config.Template) error {
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
	t.Memory, _ = strconv.Atoi(strings.TrimSpace(val))
	return nil
}

// resolveDisk settles smol's disk: flag, else the template, else prompt
// (interactive) or the compiled default (non-interactive). Unlike memory, disk
// always has a built-in fallback (SmolDefaultDiskGiB), so it never errors.
func resolveDisk(tty *os.File, interactive bool, s *createSpec) error {
	if s.Disk != 0 { // flag wins (resolveTemplate folded it in)
		return nil
	}
	if interactive {
		return askDisk(tty, &s.tmpl)
	}
	if s.tmpl.Disk == 0 {
		s.tmpl.Disk = backend.SmolDefaultDiskGiB
	}
	return nil
}

func askDisk(tty *os.File, t *config.Template) error {
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
	t.Disk, _ = strconv.Atoi(strings.TrimSpace(val))
	return nil
}

// askExtras prompts the optional create-time field, repos: a
// space-separated list, safe to leave blank. (Forwards are not asked:
// 'devvm ports add' saves one.)
func askExtras(tty *os.File, t *config.Template) error {
	repos := strings.Join(t.Repos, " ")
	if err := form(tty, huh.NewInput().Title("Repos to clone (owner/repo or URL, space-separated)").
		Placeholder("me/app me/tools").Value(&repos)); err != nil {
		return err
	}
	t.Repos = fields(repos)
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
// pre-selecting the template's) or keep the template's silently.
func resolveHook(tty *os.File, interactive bool, s *createSpec) error {
	if s.Hook != "" || !interactive { // flag wins (folded in), else the template's
		return nil
	}
	return askHook(tty, &s.tmpl)
}

func askHook(tty *os.File, t *config.Template) error {
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
	case optNone:
		t.Hook = bootstrap.KindNone
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
		t.Hook = strings.TrimSpace(custom)
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
