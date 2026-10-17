package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/reach"
	"github.com/smweber/devvm/internal/session"
)

// `devvm config` (config.md §4): the noun for devvm.toml. Every config
// command reads or writes the file and this host's own state; none dials a
// box (a local `smolvm machine ls` for collision warnings is the furthest
// any of them reaches). Every VM command reads the file and none writes it,
// so a CLI edit shows up as a `git diff` in a dotfiles repo and a VM
// operation never dirties it.

func (a *App) configCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Show, check and edit devvm.toml (templates, remote boxes, hubs)",
		Long: "devvm.toml holds [templates.*] (how create/setup shape a box),\n" +
			"[machines.*] (remote boxes and hubs; local smol VMs are discovered, not\n" +
			"configured) and [providers.*] (where provider servers are made; written by hand). It is yours: hand-edit it, keep it in dotfiles. 'add' and 'rm'\n" +
			"append or remove one whole table and never touch another line; everything\n" +
			"else is 'edit'. No config command connects to a box.",
	}
	c.AddCommand(a.configPathCmd(), a.configShowCmd(), a.configEditCmd(),
		a.configCheckCmd(), a.configAddCmd(), a.configRmCmd())
	return c
}

func (a *App) configPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the path of devvm.toml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(a.Stdout, a.Paths.FilePath())
			return nil
		},
	}
}

func (a *App) configShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the effective config: templates with their fallbacks, entries, built-ins",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := config.LoadFile(a.Paths)
			if err != nil {
				return err
			}
			a.showConfig(f)
			return nil
		},
	}
}

// showConfig prints f as TOML-shaped text, each value annotated with where
// it came from when that is not the table itself.
func (a *App) showConfig(f *config.File) {
	w := a.Stdout
	if _, err := os.Stat(f.Path); err != nil {
		fmt.Fprintf(w, "# %s (missing: built-in defaults only)\n", f.Path)
	} else {
		fmt.Fprintf(w, "# %s\n", f.Path)
	}
	names := append([]string{config.DefaultTemplate}, slices.DeleteFunc(sortedNames(f.Templates), func(n string) bool { return n == config.DefaultTemplate })...)
	def := f.Templates[config.DefaultTemplate]
	for _, name := range names {
		own := f.Templates[name]
		fmt.Fprintf(w, "\n[templates.%s]", name)
		if _, ok := f.Templates[name]; !ok {
			fmt.Fprint(w, "  # not in the file: built-ins")
		} else if name != config.DefaultTemplate {
			fmt.Fprint(w, "  # unset fields fall back to [templates.default]")
		}
		fmt.Fprintln(w)
		t := own.Under(def).Resolve(name)
		src := func(set, defSet bool) string {
			switch {
			case set:
				return ""
			case defSet && name != config.DefaultTemplate:
				return "from default"
			default:
				return "built-in"
			}
		}
		showLine(w, "user", strconv.Quote(t.User), src(own.User != nil && *own.User != "", def.User != nil && *def.User != ""))
		showLine(w, "hook", strconv.Quote(t.Hook), src(own.Hook != nil && strings.TrimSpace(*own.Hook) != "", def.Hook != nil && strings.TrimSpace(*def.Hook) != ""))
		showInt(w, "memory", t.Memory, src(own.Memory != nil, def.Memory != nil), "unset: create asks, or --memory")
		showInt(w, "disk", t.Disk, src(own.Disk != nil, def.Disk != nil), fmt.Sprintf("unset: create uses %d", backend.SmolDefaultDiskGiB))
		showLine(w, "repos", tomlList(t.Repos), src(own.Repos != nil, def.Repos != nil))
		showLine(w, "ports", tomlList(t.Ports), src(own.Ports != nil, def.Ports != nil))
		showLine(w, "authorized_keys_github", tomlList(t.AuthorizedKeysGithub), src(own.AuthorizedKeysGithub != nil, def.AuthorizedKeysGithub != nil))
		showLine(w, "authorized_keys", tomlList(t.AuthorizedKeys), src(own.AuthorizedKeys != nil, def.AuthorizedKeys != nil))
		showLine(w, "harden", strconv.FormatBool(t.Harden), src(own.Harden != nil, def.Harden != nil))
		showLine(w, "fail2ban", strconv.FormatBool(t.Fail2ban), src(own.Fail2ban != nil, def.Fail2ban != nil))
		if f.HasProviders() || len(own.ProviderFields()) > 0 || len(def.ProviderFields()) > 0 {
			// Placement: template > [providers.*] > built-in (shown against
			// the first provider, the only one today).
			var pn string
			var ps config.ProviderSpec
			if names := f.ProviderNames(); len(names) > 0 {
				pn, ps = names[0], f.Providers[names[0]]
			}
			pl := t.Placement(ps)
			psrc := func(ownSet, defSet, provSet bool) string {
				if s := src(ownSet, defSet); s != "built-in" {
					return s
				}
				if provSet {
					return "from [providers." + pn + "]"
				}
				return "built-in"
			}
			set := func(p *string) bool { return p != nil && strings.TrimSpace(*p) != "" }
			showLine(w, "server_type", strconv.Quote(pl.ServerType), psrc(set(own.ServerType), set(def.ServerType), false))
			showLine(w, "location", strconv.Quote(pl.Location), psrc(set(own.Location), set(def.Location), ps.Location != ""))
			showLine(w, "image", strconv.Quote(pl.Image), psrc(set(own.Image), set(def.Image), ps.Image != ""))
			showLine(w, "ipv4", strconv.FormatBool(pl.IPv4), psrc(own.IPv4 != nil, def.IPv4 != nil, ps.IPv4 != nil))
		}
	}
	for _, pn := range f.ProviderNames() {
		p := f.Providers[pn]
		fmt.Fprintf(w, "\n[providers.%s]  # the token is not here: 'devvm providers token %s'\n", pn, pn)
		pick := func(v, def string) (string, string) {
			if v != "" {
				return v, ""
			}
			return def, "built-in; a template may override"
		}
		v, n := pick(p.Location, config.DefaultLocation)
		showLine(w, "location", strconv.Quote(v), n)
		v, n = pick(p.Image, config.DefaultImage)
		showLine(w, "image", strconv.Quote(v), n)
		switch {
		case p.SSHKey != "":
			showLine(w, "ssh_key", strconv.Quote(p.SSHKey), "")
		case p.SSHKeyFile != "":
			showLine(w, "ssh_key_file", strconv.Quote(p.SSHKeyFile), "uploaded once by name if missing")
		default:
			fmt.Fprintln(w, "# ssh_key: unset (create needs ssh_key or ssh_key_file)")
		}
		if p.IPv4 != nil {
			showLine(w, "ipv4", strconv.FormatBool(*p.IPv4), "")
		} else {
			showLine(w, "ipv4", "true", "default")
		}
	}
	for _, m := range f.Machines() {
		e := f.Entries[m.Name]
		fmt.Fprintf(w, "\n%s", config.MachineTable(m.Name))
		if m.IsHub() && isThisHost(m.Name) {
			fmt.Fprint(w, "  # (this host)")
		}
		fmt.Fprintln(w)
		showLine(w, "backend", strconv.Quote(m.Backend), "")
		if m.IsRemote() && !m.IsHub() { // the posture line: never on a hub
			showLine(w, "managed", strconv.FormatBool(m.Managed()), map[bool]string{true: "", false: "default"}[e.Managed != nil])
		}
		showLine(w, "host", strconv.Quote(e.Host), "")
		switch {
		case e.User != "":
			showLine(w, "user", strconv.Quote(e.User), "")
		case m.Managed():
			u, _, _ := strings.Cut(m.SSHHost, "@")
			showLine(w, "user", strconv.Quote(u), "from [templates.default]: the user setup creates")
		default:
			fmt.Fprintln(w, "# user: unset, ~/.ssh/config decides")
		}
		showLine(w, "transport", strconv.Quote(m.TransportName()), map[bool]string{true: "", false: "default"}[e.Transport != ""])
		if e.MoshServer != "" {
			showLine(w, "mosh_server", strconv.Quote(e.MoshServer), "")
		}
		if e.Identity != "" {
			showLine(w, "identity", strconv.Quote(e.Identity), "")
		}
		ct, ctSrc := backend.SSHConnectTimeoutFor(m), ""
		switch {
		case os.Getenv("DEVVM_SSH_CONNECT_TIMEOUT") != "":
			ctSrc = "DEVVM_SSH_CONNECT_TIMEOUT"
		case e.ConnectTimeout == 0:
			ctSrc = "default"
		}
		showLine(w, "connect_timeout", strconv.Itoa(ct), ctSrc)
	}
}

func showLine(w io.Writer, key, val, note string) {
	line := key + " = " + val
	if note != "" {
		line = fmt.Sprintf("%-40s # %s", line, note)
	}
	fmt.Fprintln(w, line)
}

func showInt(w io.Writer, key string, v int, note, unset string) {
	if v == 0 {
		fmt.Fprintf(w, "# %s: %s\n", key, unset)
		return
	}
	showLine(w, key, strconv.Itoa(v), note)
}

func tomlList(vs []string) string {
	q := make([]string, len(vs))
	for i, v := range vs {
		q[i] = strconv.Quote(v)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (a *App) configCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Parse and validate devvm.toml offline; non-zero on error (a dotfiles hook)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runConfigCheck() },
	}
}

// runConfigCheck validates the file as every command will read it, plus
// what only the cli can check (each template's hook spec and port
// mappings, resolved over default), and warns about entries that shadow a
// local smol VM.
func (a *App) runConfigCheck() error {
	f, err := config.LoadFile(a.Paths)
	if err != nil {
		return err
	}
	if err := checkTemplates(f); err != nil {
		return err
	}
	// One local smol listing for every collision check below.
	var smols smolMachines
	if len(f.Entries) > 0 || f.HasProviders() {
		smols = smolSnapshot()
	}
	for _, w := range collisionsIn(f, smols) {
		fmt.Fprintf(a.Stderr, "devvm: warning: %s\n", w)
	}
	for _, w := range a.providerWarnings(f, smols) {
		fmt.Fprintf(a.Stderr, "devvm: warning: %s\n", w)
	}
	if _, err := os.Stat(f.Path); err != nil {
		fmt.Fprintf(a.Stdout, "%s: missing, which is valid (no entries, built-in templates)\n", f.Path)
		return nil
	}
	if f.HasProviders() {
		fmt.Fprintf(a.Stdout, "%s: ok (%d machine entries, %d templates, providers: %s)\n", f.Path, len(f.Entries), len(f.Templates), strings.Join(f.ProviderNames(), ", "))
		return nil
	}
	fmt.Fprintf(a.Stdout, "%s: ok (%d machine entries, %d templates)\n", f.Path, len(f.Entries), len(f.Templates))
	return nil
}

// checkTemplates resolves every template over default and checks it as
// create/setup would (validateTemplate).
func checkTemplates(f *config.File) error {
	var errs []error
	def := f.Templates[config.DefaultTemplate]
	for _, name := range sortedNames(f.Templates) {
		if err := validateTemplate(f.Templates[name].Under(def).Resolve(name)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.Path, err))
		}
	}
	return errors.Join(errs...)
}

// collisions names every entry hiding a local smol VM of the same name
// (config.md §2): the entry wins for every command. One local listing; no
// smolvm (a laptop that only reaches remote boxes) means none.
func (a *App) collisions(f *config.File) []string {
	if len(f.Entries) == 0 {
		return nil
	}
	return collisionsIn(f, smolSnapshot())
}

// collisionsIn is collisions over a listing already taken.
func collisionsIn(f *config.File, smols smolMachines) []string {
	var out []string
	for _, sm := range smols.list {
		if _, ok := f.Entries[sm.Name]; ok {
			out = append(out, fmt.Sprintf("%s shadows the local smol VM '%s'; rename the entry, or remove the VM with 'smolvm machine rm --name %s'", config.MachineTable(sm.Name), sm.Name, sm.Name))
		}
	}
	return out
}

// providerWarnings is config check's word on providers, offline: a
// template setting provider placement with no [providers.*] table to use it
// (it applies to nothing), and a provider server in the last listing whose
// name an entry or a local smol VM holds (that one wins for every command,
// providers.md §2). The listing is the cache, never the API.
func (a *App) providerWarnings(f *config.File, smols smolMachines) []string {
	var out []string
	if !f.HasProviders() {
		for _, name := range sortedNames(f.Templates) {
			if fs := f.Templates[name].ProviderFields(); len(fs) > 0 {
				out = append(out, fmt.Sprintf("[templates.%s] sets %s, which apply only to provider servers, and no [providers.*] table is configured", name, strings.Join(fs, ", ")))
			}
		}
		return out
	}
	cached := a.providerCachedNames(f)
	for _, pn := range sortedNames(cached) {
		for _, n := range cached[pn] {
			if owner := localOwner(f, smols, n); owner != "" {
				out = append(out, fmt.Sprintf("%s shadows the %s server '%s' (from its last listing); rename one of them", owner, providerTitle(pn), n))
			}
		}
	}
	return out
}

func (a *App) configEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit",
		Short: "Open devvm.toml in $VISUAL/$EDITOR, then check it (keeps a .bak if it fails)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runConfigEdit() },
	}
}

// runConfigEdit runs the editor on the file and checks the result. A
// failing check leaves the file as edited (the user's work is not thrown
// away) and keeps the pre-edit content beside it as devvm.toml.bak.
func (a *App) runConfigEdit() error {
	if !stdinIsTTY() || !stdoutIsTTY() {
		return errors.New("config edit needs a terminal; edit the file directly, then run 'devvm config check'")
	}
	if err := os.MkdirAll(a.Paths.Config, 0o755); err != nil {
		return err
	}
	// The editor gets devvm.toml (a link stays a link); the .bak goes
	// beside the real file, with its mode, so it lands in the dotfiles
	// checkout next to what it backs up.
	path := a.Paths.FilePath()
	real, err := config.ResolveFile(a.Paths)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(real); err == nil {
		mode = fi.Mode().Perm()
	}
	before, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	// Through sh so an EDITOR with arguments ("code --wait") works; the
	// path is "$1", never interpolated.
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor %q: %w", editor, err)
	}
	after, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	defer config.TouchChanged(a.Paths)
	if existed && bytes.Equal(before, after) {
		fmt.Fprintln(a.Stdout, "devvm: no changes")
		return nil
	}
	cerr := a.runConfigCheck()
	if cerr == nil {
		return nil
	}
	if !existed {
		return fmt.Errorf("%w\n%s does not check (it did not exist before this edit); fix it with 'devvm config edit'", cerr, path)
	}
	bak := real + ".bak"
	_ = os.Remove(bak) // so WriteFile's mode applies to a fresh file
	if err := os.WriteFile(bak, before, mode); err != nil {
		return fmt.Errorf("%w\n(and saving the previous version to %s failed: %v)", cerr, bak, err)
	}
	return fmt.Errorf("%w\n%s does not check; it is saved as edited, and the version before this edit is in %s", cerr, path, bak)
}

// configAddOpts is `config add`'s input.
type configAddOpts struct {
	backend, host, user, transport, moshServer, identity string
	managed                                              bool
	connectTimeout                                       int
}

func (a *App) configAddCmd() *cobra.Command {
	var o configAddOpts
	c := &cobra.Command{
		Use:   "add NAME --backend remote|hub --host H[:P]",
		Short: "Append a [machines.NAME] entry for a remote box or a hub (connects to nothing)",
		Long: "Append one [machines.NAME] table at the end of devvm.toml, creating the file\n" +
			"if needed; nothing else in the file changes. backend (and managed, for remote)\n" +
			"is always written. Nothing is dialed: set a managed box up with\n" +
			"'devvm setup NAME' next. On a terminal with no --host, a form asks.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runConfigAdd(args[0], o, cmd.Flags().Changed("managed"))
		},
	}
	f := c.Flags()
	f.StringVarP(&o.backend, "backend", "b", "", "remote | hub")
	f.StringVar(&o.host, "host", "", "host[:port], [ipv6]:port, or an ssh alias")
	f.StringVar(&o.user, "user", "", "login user (default: none, ~/.ssh/config decides; a managed box uses the template's user)")
	f.BoolVar(&o.managed, "managed", false, "remote: devvm shapes this box (setup, prereqs, hardening)")
	f.StringVar(&o.transport, "transport", "", "interactive transport: ssh | mosh (default ssh)")
	f.StringVar(&o.moshServer, "mosh-server", "", "path to mosh-server on the box")
	f.StringVar(&o.identity, "identity", "", "ssh identity file")
	f.IntVar(&o.connectTimeout, "connect-timeout", 0, "ssh ConnectTimeout in seconds (default 10)")
	_ = c.RegisterFlagCompletionFunc("backend", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{config.BackendRemote, config.BackendHub}, cobra.ShellCompDirectiveNoFileComp
	})
	return c
}

// hostname is os.Hostname, a var so a test can pose as a host whose name
// is an FQDN.
var hostname = os.Hostname

// isThisHost reports whether an entry name names this host: the full
// hostname or its short form (up to the first dot), since macOS and many
// Linux hosts report an FQDN (mac.local, box.example.com) while the shared
// file names the host by either. Cosmetic only (config.md §1): it labels a
// hub row "(this host)", and nothing behaves differently for it.
func isThisHost(name string) bool {
	host, err := hostname()
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(name, host) {
		return true
	}
	short, _, _ := strings.Cut(host, ".")
	return short != "" && strings.EqualFold(name, short)
}

// errHubNameConfig refuses HUB/NAME in the config noun: this host's file
// has no entries for a hub's machines.
func errHubNameConfig(name string) error {
	if _, _, ok, _ := config.SplitHubName(name); ok {
		return fmt.Errorf("%s: config is local to this host; HUB/NAME lives in the hub's devvm.toml", name)
	}
	return nil
}

func (a *App) runConfigAdd(name string, o configAddOpts, managedSet bool) error {
	if err := errHubNameConfig(name); err != nil {
		return err
	}
	if err := config.ValidName(name); err != nil {
		return err
	}
	if o.host == "" {
		tty := openTTY()
		if tty == nil {
			return errors.New("config add needs --backend and --host (no terminal to ask on)")
		}
		err := askConfigAdd(tty, &o, managedSet)
		tty.Close()
		if err != nil {
			return err
		}
	}
	switch o.backend {
	case config.BackendRemote, config.BackendHub:
	case "":
		return errors.New("config add needs --backend remote|hub")
	case config.BackendSmol:
		return fmt.Errorf("smol VMs are not configured: they are discovered from smolvm; 'devvm create %s' makes one", name)
	default:
		return fmt.Errorf("invalid backend %q (want remote or hub)", o.backend)
	}
	e := config.Entry{Backend: o.backend, Host: o.host, User: o.user, Transport: o.transport,
		MoshServer: o.moshServer, Identity: o.identity, ConnectTimeout: o.connectTimeout}
	if o.backend == config.BackendRemote {
		managed := o.managed
		e.Managed = &managed
	} else if managedSet {
		return errors.New("--managed applies only to a remote entry: a hub is never shaped")
	}
	if err := config.AppendEntry(a.Paths, name, e); err != nil {
		return err
	}
	defer config.TouchChanged(a.Paths)
	fmt.Fprintf(a.Stdout, "devvm: added %s to %s\n", config.MachineTable(name), a.Paths.FilePath())
	if f, err := config.LoadFile(a.Paths); err == nil {
		for _, w := range a.collisions(f) {
			if strings.HasPrefix(w, "[machines."+name+"]") {
				fmt.Fprintf(a.Stderr, "devvm: warning: %s\n", w)
			}
		}
	}
	switch {
	case o.backend == config.BackendHub:
		fmt.Fprintf(a.Stdout, "Its machines are %s/NAME; 'devvm status' lists them once the hub answers.\n", name)
	case o.managed:
		fmt.Fprintf(a.Stdout, "Next: 'devvm setup %s' shapes it from [templates.default] (or --template T).\n", name)
	default:
		fmt.Fprintf(a.Stdout, "devvm never modifies it (managed = false). Next: 'devvm attach %s'.\n", name)
	}
	return nil
}

// askConfigAdd is the huh form for a remote box or hub (it was create's,
// before remote create went).
func askConfigAdd(tty *os.File, o *configAddOpts, managedSet bool) error {
	if o.backend == "" {
		o.backend = config.BackendRemote
		if err := form(tty, huh.NewSelect[string]().Title("Backend").
			Options(
				huh.NewOption("remote: a box reached over ssh", config.BackendRemote),
				huh.NewOption("hub: another host running devvm; its machines are NAME/…", config.BackendHub),
			).Value(&o.backend)); err != nil {
			return err
		}
	}
	timeout := ""
	if o.connectTimeout != 0 {
		timeout = strconv.Itoa(o.connectTimeout)
	}
	if o.transport == "" {
		o.transport = config.TransportSSH
	}
	fields := []huh.Field{
		huh.NewInput().Title("Host (host[:port] or an ssh alias)").Value(&o.host).
			Validate(func(v string) error { _, _, err := config.ParseHost(v); return err }),
		huh.NewInput().Title("User (optional; blank lets ~/.ssh/config decide)").Value(&o.user),
		huh.NewInput().Title("SSH identity file (optional)").Value(&o.identity),
		huh.NewSelect[string]().Title("Transport").
			Options(huh.NewOption("ssh", config.TransportSSH), huh.NewOption("mosh", config.TransportMosh)).
			Value(&o.transport),
		huh.NewInput().Title("Connect timeout, seconds (optional)").Placeholder("10").Value(&timeout),
	}
	if o.backend == config.BackendRemote && !managedSet {
		fields = append(fields, huh.NewConfirm().
			Title("Should devvm manage it (setup installs prereqs, may harden)?").Value(&o.managed))
	}
	if err := form(tty, fields...); err != nil {
		return err
	}
	o.host, o.user = strings.TrimSpace(o.host), strings.TrimSpace(o.user)
	if o.transport == config.TransportSSH {
		o.transport = "" // the default: leave the line out
	}
	if t := strings.TrimSpace(timeout); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n <= 0 {
			return errors.New("connect timeout must be a positive number of seconds")
		}
		o.connectTimeout = n
	}
	return nil
}

func (a *App) configRmCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "rm NAME",
		Short: "Remove a [machines.NAME] entry and this host's state for it",
		Long: "Remove the [machines.NAME] table from devvm.toml (nothing else in the file\n" +
			"changes) and what this host keeps for it: its forward daemon(s), saved\n" +
			"forwards and reach cache (for a hub, those of every NAME/… machine too) and\n" +
			"a managed box's pinned host key. The box itself is not touched. Refuses\n" +
			"while a daemon holds live forwards for it, unless --force.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return a.runConfigRm(args[0], force) },
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) != 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			names, _ := config.List(a.Paths)
			return withPrefix(names, toComplete), cobra.ShellCompDirectiveNoFileComp
		},
	}
	c.Flags().BoolVar(&force, "force", false, "stop daemons holding live forwards for it (and its machines) and remove it anyway")
	return c
}

// runConfigRm removes an entry and owns this host's state for it (config.md
// §4): `delete` refuses an entry and points here, so this is the one
// command that forgets a remote box or a hub. Order: refuse first (live
// forwards), then stop the daemons, then edit the file, then clear the
// state files, so a failed edit leaves the state where it was and a
// `ports add` waiting on a ports lock finds the entry gone (editPorts).
func (a *App) runConfigRm(name string, force bool) error {
	if err := errHubNameConfig(name); err != nil {
		return err
	}
	defer config.TouchChanged(a.Paths)
	m, err := config.Load(a.Paths, name)
	if err != nil {
		return err
	}
	// Plan the edit before touching anything: a layout the line cut cannot
	// remove (an inline table, dotted keys) is refused here, with every
	// daemon still running.
	edit, err := config.PlanRemoveEntry(a.Paths, name)
	if err != nil {
		return err
	}
	// Every runtime name this entry owns: its own, and for a hub each
	// HUB/NAME with a run file (a daemon killed rather than stopped leaves
	// its socket; every one that started leaves its lock and log).
	runtime := []string{name}
	if m.IsHub() {
		runtime = append(session.HubRuntimeNames(a.Paths, name), name)
	}
	if !force {
		var held []string
		for _, n := range runtime {
			if f := a.forwardSummary(n); f.daemon && f.n > 0 {
				held = append(held, fmt.Sprintf("%s (%d forwards)", n, f.n))
			}
		}
		if len(held) > 0 {
			slices.Sort(held)
			return fmt.Errorf("live forwards for %s; stop them with 'devvm ports down' or 'devvm stop', or use --force", strings.Join(held, ", "))
		}
	}
	// A wedged daemon (it answers nothing, so forwardSummary saw none) is
	// not refused here: reapRunFiles stops it as well as it can and says so
	// on stderr, and the entry goes anyway, since nothing here can make
	// it answer.
	for _, n := range runtime {
		a.reapRunFiles(n)
	}
	if err := edit.Commit(); err != nil {
		return err
	}
	// The entry is gone: clear every piece of this host's state for it,
	// carrying on past a failure so one stuck file does not strand the
	// rest, and report them together.
	var failed []string
	note := func(err error) {
		if err != nil {
			failed = append(failed, strings.ReplaceAll(err.Error(), "\n", "; "))
		}
	}
	forget := []string{name}
	if m.IsHub() {
		saved, err := config.SavedPortsNames(a.Paths)
		note(err)
		for _, display := range saved {
			if hub, _, ok, _ := config.SplitHubName(display); ok && hub == name {
				forget = append(forget, display)
			}
		}
	}
	for _, display := range forget {
		pin := m
		if display != name {
			pin = nil
		}
		note(a.forgetEntryState(display, pin))
	}
	for _, n := range runtime {
		// Only once the name no longer resolves (see RemoveLock): an entry
		// may have shadowed a local smol VM of the same name, which
		// resolves now and keeps its lock.
		// Local sources only (a provider server from the cache): an rm
		// never dials the API.
		restore := a.cachedProviders()
		_, _, err := a.resolveAny(n)
		restore()
		if err != nil {
			session.RemoveLock(a.Paths, n)
		}
	}
	if m.IsHub() {
		removeHubCache(a.Paths, name) // derived from the entry that just went
	}
	if len(failed) > 0 {
		return fmt.Errorf("removed %s; but: %s", config.MachineTable(name), strings.Join(failed, "; "))
	}
	fmt.Fprintf(a.Stdout, "devvm: removed %s from %s\n", config.MachineTable(name), a.Paths.FilePath())
	return nil
}

// forgetEntryState is forgetHostState for a config entry: saved forwards,
// reach cache and pinned host key, but never a managed marker. An entry's
// name may shadow a local smol VM, and that VM's marker is the VM's, not
// the entry's: removing the entry must leave the VM as managed as it was.
func (a *App) forgetEntryState(name string, m *config.Machine) error {
	var errs []error
	if unlock, err := config.LockPorts(a.Paths, name); err != nil {
		errs = append(errs, fmt.Errorf("%s: clear saved forwards: %w", name, err))
	} else {
		if err := config.RemovePorts(a.Paths, name); err != nil {
			errs = append(errs, fmt.Errorf("%s: clear saved forwards: %w", name, err))
		}
		unlock()
	}
	if err := reach.Remove(a.Paths, name); err != nil {
		errs = append(errs, fmt.Errorf("%s: clear reach cache: %w", name, err))
	}
	if m != nil {
		if err := a.forgetHostKey(m); err != nil {
			errs = append(errs, fmt.Errorf("%s: clear pinned host key: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
