package cli

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	if err := a.resolveHook(tty, interactive, s); err != nil {
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
func (a *App) resolveHook(tty *os.File, interactive bool, s *createSpec) error {
	if s.Hook != "" || !interactive { // flag wins (folded in), else the template's
		return nil
	}
	starter, err := a.askHook(tty, &s.tmpl)
	s.starter = starter
	return err
}

// starterHook is the script the form's "starter" choice writes for a user
// with no hook of their own: common tools, a tmux that shows where you
// are, a few shell lines. Nothing of it is a default: it runs only as the
// user's own file, which they edit.
//
//go:embed starter_hook.sh
var starterHook []byte

// starterHookName is where the starter goes, beside devvm.toml, and the
// file: spec that names it there.
const starterHookName = "hook.sh"

// askHook asks for the hook. starter reports that the starter was chosen:
// t then carries its script, and the caller writes the files
// (writeStarterHook) once create is past its refusals.
func (a *App) askHook(tty *os.File, t *config.Template) (starter bool, err error) {
	const (
		optDefault = "default"
		optStarter = "starter"
		optNone    = "none"
		optCustom  = "custom"
	)
	choice := optNone
	var opts []huh.Option[string]
	switch {
	// Offer the template's hook (pre-selected) only when it's something
	// other than "none" — otherwise it's identical to the plain "none" option.
	case t.Hook != "" && t.Hook != bootstrap.KindNone:
		opts = append(opts, huh.NewOption("template "+t.Name+": "+t.Hook, optDefault))
		choice = optDefault
	// No layer set a hook (a written "none" is an answer, and stays
	// pre-selected): the starter, or the hook.sh an earlier create wrote,
	// first and pre-selected, so a first box is not bare. Not on a hub
	// running a laptop's create over -t: the files would land on the hub,
	// reported as if they were here.
	case !t.HookSet && !noSubscribe():
		path := hookFilePath(a.Paths, starterHookName)
		label := "starter — an editor, search tools, a friendlier tmux (writes " + path + " for you to edit)"
		if _, err := os.Stat(path); err == nil {
			label = "your " + path
		}
		opts = append(opts, huh.NewOption(label, optStarter))
		choice = optStarter
	}
	opts = append(opts,
		huh.NewOption("none — plain Ubuntu with git, tmux and gh", optNone),
		huh.NewOption("custom — enter a file:/url:/cmd: spec", optCustom),
	)
	if err := form(tty, huh.NewSelect[string]().Title("Hook (a script that sets the box up your way)").Options(opts...).Value(&choice)); err != nil {
		return false, err
	}
	switch choice {
	case optDefault:
	case optStarter:
		return true, a.useStarterHook(t)
	case optNone:
		t.Hook = bootstrap.KindNone
	case optCustom:
		custom := ""
		// Validated as create will read it: a file: hook's script too.
		load := func(v string) error {
			c := *t
			c.Hook = strings.TrimSpace(v)
			return a.loadHookScript(&c, "")
		}
		if err := form(tty, huh.NewInput().
			Title("Hook spec").
			Placeholder("file:<script here> [args] | url:<URL> [args] | cmd:<path in the box> [args]").
			Value(&custom).
			Validate(load)); err != nil {
			return false, err
		}
		t.Hook = strings.TrimSpace(custom)
	}
	return false, a.loadHookScript(t, "")
}

// useStarterHook makes hook.sh the hook of this create, writing nothing
// yet: the script is the hook.sh already beside devvm.toml (the user's, as
// it is now), else the starter.
func (a *App) useStarterHook(t *config.Template) error {
	t.Hook = bootstrap.KindFile + ":" + starterHookName
	if _, err := os.Stat(hookFilePath(a.Paths, starterHookName)); errors.Is(err, os.ErrNotExist) {
		t.HookScript = starterHook
		return nil
	}
	return a.loadHookScript(t, "")
}

// writeStarterHook puts the chosen starter on disk, just before create
// allocates the box (past every prompt and refusal, so an abandoned create
// leaves nothing): hook.sh beside devvm.toml unless one is there. With no
// devvm.toml at all it writes one whose default template names the hook,
// so `devvm setup` and the next create use it too; a file that exists is
// the user's and is never edited (only `config add`/`config rm` do that):
// one line says what to add. Never fatal: the script is in hand already.
func (a *App) writeStarterHook() {
	path := hookFilePath(a.Paths, starterHookName)
	line := "hook = " + strconv.Quote(bootstrap.KindFile+":"+starterHookName)
	fail := func(err error) {
		fmt.Fprintf(a.Stderr, "devvm: the starter hook runs for this box, but was not saved: %v\n", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fail(err)
		return
	}
	switch err := writeNew(path, starterHook); {
	case err == nil:
		fmt.Fprintf(a.Stderr, "devvm: wrote %s: it is yours to edit ('devvm setup NAME' runs it again)\n", path)
	case !errors.Is(err, os.ErrExist):
		fail(err)
		return
	}
	toml := a.Paths.FilePath()
	// A devvm.toml that exists, a dangling link included, is left alone.
	switch err := writeNew(toml, []byte("[templates.default]\n"+line+"\n")); {
	case err == nil:
		fmt.Fprintf(a.Stderr, "devvm: wrote %s: every create and setup uses the hook now ('devvm config edit')\n", toml)
	case errors.Is(err, os.ErrExist):
		fmt.Fprintf(a.Stderr, "devvm: the hook is for this box only; to keep it, put %s under [templates.default] ('devvm config edit')\n", line)
	default:
		fail(err)
	}
}

// writeNew writes a file that must not exist yet (os.ErrExist otherwise),
// whole or not at all: a temp file linked into place, so a reader (the
// status watcher on devvm.toml) never sees half of it.
func writeNew(path string, data []byte) error {
	if _, err := os.Lstat(path); err == nil {
		return os.ErrExist
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".devvm-new-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o644)
	}
	if err != nil {
		return err
	}
	if err := os.Link(f.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return err
	}
	return nil
}

// form runs one huh group over the controlling terminal. A var so a test
// that reaches a form past its own stubs can fail it at once: over a plain
// file huh errors on Linux but blocks for good on macOS (kqueue).
var form = func(tty *os.File, fields ...huh.Field) error {
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
