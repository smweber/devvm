// Package cli wires the devvm host command tree (cobra). Command bodies live in
// the same package, split by concern; this file holds the root command and the
// shared App context (config dir + helpers) every subcommand receives.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/smweber/devvm/internal/config"
	"github.com/spf13/cobra"
)

// Version is stamped by release builds (-ldflags -X); "dev" for plain go build.
var Version = "dev"

// App carries process-wide context to command handlers.
type App struct {
	ConfigDir string
	Stdin     io.Reader // nil means os.Stdin; set by tests that drive a stdin-EOF path
	Stdout    io.Writer
	Stderr    io.Writer
}

func (a *App) stdin() io.Reader {
	if a.Stdin != nil {
		return a.Stdin
	}
	return os.Stdin
}

func newApp() *App {
	return &App{
		ConfigDir: config.DefaultConfigDir(),
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
	}
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
		var pe *proxyExit
		if errors.As(err, &pe) {
			return pe.code
		}
		// cobra already prints usage errors; print anything else once, plainly.
		fmt.Fprintln(os.Stderr, "devvm:", err)
		return 1
	}
	return 0
}

// Command groups, so `--help` clusters the surface by concern instead of one flat
// alphabetical list. Ordered lifecycle: setup → run → teardown, with the symmetric
// pairs bracketing (create/delete outer, provision/deprovision inner, start/stop).
const (
	groupLifecycle = "lifecycle"
	groupConnect   = "connect"
	groupConfigure = "configure"
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
			"  smol              local, isolated smolvm microVMs\n" +
			"  remote-managed    a remote host devvm shapes (over ssh)\n" +
			"  remote-unmanaged  an existing host devvm adopts hands-off (over ssh)\n" +
			"  hub               another host running devvm; its machines are HUB/NAME\n\n" +
			"Per-machine config lives in ~/.config/devvm/machines/<name>.toml.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&a.ConfigDir, "config-dir", a.ConfigDir,
		"devvm config directory")

	root.AddGroup(
		&cobra.Group{ID: groupLifecycle, Title: "Lifecycle:"},
		&cobra.Group{ID: groupConnect, Title: "Connect:"},
		&cobra.Group{ID: groupConfigure, Title: "Configure:"},
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
		a.provisionCmd(),
		a.bootstrapCmd(),
		a.lockdownCmd(),
		a.startCmd(),
		a.stopCmd(),
		a.deprovisionCmd(),
		a.deleteCmd(),
	)...)
	root.AddCommand(group(groupConnect,
		a.attachCmd(),
		a.shellCmd(),
		a.execCmd(),
		a.cpInCmd(),
		a.cpOutCmd(),
		a.authCmd(),
	)...)
	root.AddCommand(group(groupConfigure,
		a.reposCmd(),
		a.portsCmd(),
		a.keysCmd(),
		a.defaultsCmd(),
		a.statusCmd(),
	)...)
	root.AddCommand(group(groupMaintain,
		a.updateCmd(),
		a.menubarCmd(),
	)...)
	root.AddCommand(a.daemonCmd()) // hidden; falls under "Additional Commands"
	return root
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
		if m, err := config.Load(a.ConfigDir, n); err == nil && m.IsHub() {
			continue // a HUB/NAME never loads as a local conf, so it stays
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
// names come from listMachines: confs, hub tables, the cache — never a
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
