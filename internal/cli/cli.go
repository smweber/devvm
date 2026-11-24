// Package cli wires the devvm host command tree (cobra). Command bodies live in
// the same package, split by concern; this file holds the root command and the
// shared App context (config and state dirs + helpers) every subcommand receives.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
	"github.com/spf13/cobra"
)

// Version is stamped by release builds (-ldflags -X); "dev" for plain go build.
var Version = "dev"

// App carries process-wide context to command handlers.
type App struct {
	Paths  config.Paths
	Stdin  io.Reader // nil means os.Stdin; set by tests that drive a stdin-EOF path
	Stdout io.Writer
	Stderr io.Writer

	// providerAPI, when set, builds the provider client for a token in
	// place of the real one (tests: the fake, so every API call is
	// counted). nil is the real client (providers.go).
	providerAPI func(name, token string) (provider.Provider, error)
	// providerMode is how resolve's provider source may answer: the API
	// (the zero value) or the last listing only (cachedProviders).
	providerMode providerMode
	// providerAsk: this is a lifecycle command (create, delete, start,
	// stop), so a provider with no token on this host asks for one on a
	// terminal (providers.md §1 "First use asks") instead of being skipped.
	providerAsk bool
}

func (a *App) stdin() io.Reader {
	if a.Stdin != nil {
		return a.Stdin
	}
	return os.Stdin
}

func newApp() *App {
	return &App{
		Paths:  config.DefaultPaths(),
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

// stateDirFor is the state dir `--config-dir dir` alone means: the default
// one when dir is the default config dir (compared through symlinks, since
// a dotfiles-managed ~/.config/devvm is often one, and landing state in
// dotfiles by a spelling difference would be the one wrong answer), else
// dir itself, which keeps an isolated instance whole.
func stateDirFor(dir string) string {
	if sameDir(dir, config.DefaultConfigDir()) {
		return config.DefaultStateDir()
	}
	return dir
}

func sameDir(a, b string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	return filepath.Clean(a) == filepath.Clean(b) || resolve(a) == resolve(b)
}

// Execute builds the command tree and runs it. Returns the process exit code.
func Execute() int {
	// A write to a closed stdout must not kill us mid-flight. The menu bar
	// app runs `devvm update`/`devvm menubar`, which quit the app — the
	// reader of our pipes — before relaunching it; with the default
	// disposition the next Fprintf would end the process by SIGPIPE before
	// the relaunch. Ignored, the write returns EPIPE, which the Fprintfs
	// discard and `status --watch` already treats as a clean exit.
	signal.Ignore(syscall.SIGPIPE)
	app := newApp()
	root := app.rootCmd()
	if err := root.Execute(); err != nil {
		// A proxied command's exit status is the hub's devvm's own, which
		// already printed its error; pass the status through without a
		// second line.
		var pe *exitCode
		if errors.As(err, &pe) {
			return pe.code
		}
		// A signal stopped the command after its cleanup (interruptible):
		// die by it, as the default disposition would have.
		var ie *interrupted
		if errors.As(err, &ie) {
			fmt.Fprintln(os.Stderr, "devvm:", err)
			signal.Reset(ie.sig)
			_ = syscall.Kill(os.Getpid(), ie.sig)
			// The signal reaches some thread asynchronously; returning at
			// once would usually exit with the status before it lands.
			time.Sleep(time.Second)
			return 128 + int(ie.sig)
		}
		// cobra already prints usage errors; print anything else once, plainly.
		fmt.Fprintln(os.Stderr, "devvm:", err)
		return failureStatus(err)
	}
	return 0
}

// failureStatus is the exit status for an error Execute printed: 1, except
// a create/setup that finished with only clones failing
// (exitSetupIncomplete).
func failureStatus(err error) int {
	var si *setupIncomplete
	if errors.As(err, &si) {
		return exitSetupIncomplete
	}
	return 1
}

// Command groups, so `--help` clusters the surface by concern instead of one flat
// alphabetical list. Ordered lifecycle: setup → run → teardown, with the symmetric
// pairs bracketing (create/delete outer, start/stop inner).
const (
	groupLifecycle = "lifecycle"
	groupConnect   = "connect"
	groupConfigure = "configure"
	groupConfig    = "config"
	groupMaintain  = "maintain"
)

func (a *App) rootCmd() *cobra.Command {
	// Render commands in AddCommand order (grouped, lifecycle-ordered) rather than
	// alphabetically.
	cobra.EnableCommandSorting = false

	root := &cobra.Command{
		Use:   "devvm",
		Short: "One frontend for persistent dev boxes, whatever the transport",
		Long: "devvm manages persistent dev boxes across backends:\n" +
			"  smol    local, isolated smolvm microVMs, discovered from smolvm\n" +
			"  remote  a box reached over ssh, named in devvm.toml (never shaped)\n" +
			"  hub     another host running devvm; its machines are HUB/NAME\n" +
			"  hetzner a Hetzner Cloud server, discovered from the API ([providers.hetzner])\n\n" +
			"~/.config/devvm/devvm.toml holds templates for create/setup ([templates.*])\n" +
			"and the remote boxes and hubs ([machines.*]); see 'devvm config'.\n" +
			"This host's state (daemon sockets, logs, caches, saved forwards) is in\n" +
			"~/.local/state/devvm.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// A leaf that disables flag parsing (exec) gets the
			// root's own flags typed before it as arguments: apply them
			// here, as parsing would have, and the leaf strips them
			// (leafArgs).
			if cmd.DisableFlagParsing {
				set, _, err := splitRootFlags(args)
				if err != nil {
					return err
				}
				for _, kv := range set {
					if err := cmd.Root().PersistentFlags().Set(kv[0], kv[1]); err != nil {
						return err
					}
				}
			}
			// --config-dir alone, naming another dir, keeps an isolated
			// instance (a test, a second registry) self-contained, its
			// state inside it, rather than sharing this host's run/ with
			// the real one. Naming the default dir is not isolation: its
			// state stays in the default state dir, never in a config dir
			// that may be a dotfiles checkout. (The root's set, not the
			// leaf's: the same flags, and the only one a leaf that skips
			// parsing has.)
			fl := cmd.Root().PersistentFlags()
			if fl.Changed("config-dir") && !fl.Changed("state-dir") {
				a.Paths.State = stateDirFor(a.Paths.Config)
			}
			return nil
		},
	}
	root.PersistentFlags().StringVar(&a.Paths.Config, "config-dir", a.Paths.Config,
		"devvm config directory")
	root.PersistentFlags().StringVar(&a.Paths.State, "state-dir", a.Paths.State,
		"devvm state directory: daemon sockets, logs, caches (with a non-default --config-dir, defaults to it)")

	root.AddGroup(
		&cobra.Group{ID: groupLifecycle, Title: "Lifecycle:"},
		&cobra.Group{ID: groupConnect, Title: "Connect:"},
		&cobra.Group{ID: groupConfigure, Title: "Configure:"},
		&cobra.Group{ID: groupConfig, Title: "Config:"},
		&cobra.Group{ID: groupMaintain, Title: "Maintain devvm itself:"},
	)

	// checkCommandGroups panics on a GroupID with no registered group, so GroupID is
	// set only on these root-level commands (subcommand leaves stay ungrouped).
	group := func(id string, cmds ...*cobra.Command) []*cobra.Command {
		for _, c := range cmds {
			c.GroupID = id
		}
		return cmds
	}

	root.AddCommand(group(groupLifecycle,
		a.createCmd(),
		a.setupCmd(),
		a.startCmd(),
		a.stopCmd(),
		a.deleteCmd(),
	)...)
	root.AddCommand(group(groupConnect,
		a.attachCmd(),
		a.shellCmd(),
		a.execCmd(),
		a.cpInCmd(),
		a.cpOutCmd(),
	)...)
	root.AddCommand(group(groupConfigure,
		a.portsCmd(),
		a.keysCmd(),
		a.statusCmd(),
	)...)
	root.AddCommand(group(groupConfig,
		a.configCmd(),
		a.providersCmd(),
	)...)
	root.AddCommand(group(groupMaintain,
		a.updateCmd(),
		a.menubarCmd(),
	)...)
	root.AddCommand(a.daemonCmd())  // hidden; falls under "Additional Commands"
	root.AddCommand(a.sessionCmd()) // hidden: the hub side of hub forwards
	root.AddCommand(a.refreshCmd()) // hidden: the menu bar app's "probe the remotes now"
	guardNouns(root)
	return root
}

// guardNouns makes every noun under root (a command with subcommands and
// no Run of its own: ports, keys, config, providers) refuse a word it does
// not know. cobra only checks that at the root: a non-runnable noun prints
// its help and exits 0, so a script calling a removed leaf (`keys dedupe`)
// would see success. A bare noun still prints its help, exit 0.
func guardNouns(root *cobra.Command) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c == root || !c.HasSubCommands() || c.Runnable() {
			return
		}
		// Runnable now, so cobra's usage would list a bare `devvm keys
		// [flags]`; the noun's usage keeps only `devvm keys [command]`.
		c.SetUsageTemplate(strings.Replace(c.UsageTemplate(),
			"Usage:{{if .Runnable}}", "Usage:{{if and .Runnable (not .HasAvailableSubCommands)}}", 1))
		c.RunE = func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			var names []string
			for _, sub := range cmd.Commands() {
				if sub.IsAvailableCommand() {
					names = append(names, sub.Name())
				}
			}
			return fmt.Errorf("unknown command %q for %q (its commands: %s)",
				args[0], cmd.CommandPath(), strings.Join(names, ", "))
		}
	}
	walk(root)
}

// rootFlags are the root's persistent flags, each taking a value.
var rootFlags = []string{"config-dir", "state-dir"}

// splitRootFlags peels the root's flags (--config-dir X, --state-dir=Y)
// off the front of a leaf's raw args. cobra hands a DisableFlagParsing
// leaf everything after its own name unparsed, including root flags typed
// before it (`devvm --config-dir X exec NAME -- cmd`); only the front is
// read, so CMD's own flags after NAME are never touched.
func splitRootFlags(args []string) (set [][2]string, rest []string, err error) {
	for len(args) > 0 {
		k, v, eq := strings.Cut(strings.TrimPrefix(args[0], "--"), "=")
		if !strings.HasPrefix(args[0], "--") || !slices.Contains(rootFlags, k) {
			break
		}
		if eq {
			args = args[1:]
		} else {
			if len(args) < 2 {
				return nil, nil, fmt.Errorf("flag needs an argument: --%s", k)
			}
			v, args = args[1], args[2:]
		}
		set = append(set, [2]string{k, v})
	}
	return set, args, nil
}

// wantsHelp reports whether a DisableFlagParsing leaf's args (after
// leafArgs) ask for its help: --help or -h where NAME would be. cobra
// parses no flag for such a leaf, so without this `devvm exec --help` was
// an argument-count error (or NAME "--help"); a --help after NAME is the
// guest command's (`devvm exec NAME -- --help`) and is passed through.
func wantsHelp(args []string) bool {
	return len(args) > 0 && (args[0] == "--help" || args[0] == "-h")
}

// leafArgs is a DisableFlagParsing leaf's args without the root flags the
// root's PersistentPreRunE already applied.
func leafArgs(args []string) []string {
	_, rest, _ := splitRootFlags(args)
	return rest
}

// tildePath shortens a path under $HOME to ~/….
func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rest, ok := strings.CutPrefix(p, home+"/"); ok {
			return "~/" + rest
		}
	}
	return p
}

// completeMachines is the ValidArgsFunction for the verbs that go through
// resolve: every name listMachines knows except hubs themselves, which those
// verbs always refuse (HUB/NAME machines stay: they are what the verb acts
// on). status and delete, which do take a hub, use completeAnyMachine.
func (a *App) completeMachines(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var names []string
	for _, n := range a.listMachines() {
		if m, err := config.Load(a.Paths, n); err == nil && m.IsHub() {
			continue // a HUB/NAME never loads as a local conf, so it stays
		}
		names = append(names, n)
	}
	return withPrefix(names, toComplete), cobra.ShellCompDirectiveNoFileComp
}

// completeDeletable is delete's: smol VMs and hub machines, never a
// devvm.toml entry (delete refuses those and points at `config rm`).
func (a *App) completeDeletable(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var names []string
	for _, n := range a.listMachines() {
		if config.Exists(a.Paths, n) {
			continue
		}
		names = append(names, n)
	}
	return withPrefix(names, toComplete), cobra.ShellCompDirectiveNoFileComp
}

// completeAnyMachine offers the full listing, hubs included.
func (a *App) completeAnyMachine(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return withPrefix(a.listMachines(), toComplete), cobra.ShellCompDirectiveNoFileComp
}

// withPrefix keeps the candidates that start with what was typed. cobra
// filters only static ValidArgs, not a ValidArgsFunction's answer, and the
// shell scripts do their own filtering — except that `devvm __complete
// attach h/` is also read directly (the menu bar app, the roadmap's live
// check), so "the hub's machines" must be what comes back for `h/`. The
// names come from listMachines: confs, saved forwards, the cache — never a
// dial, so completion of a sleeping hub costs nothing.
func withPrefix(names []string, prefix string) []string {
	if prefix == "" {
		return names
	}
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out
}
