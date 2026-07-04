// Package cli wires the devvm host command tree (cobra). Command bodies live in
// the same package, split by concern; this file holds the root command and the
// shared App context (config dir + helpers) every subcommand receives.
package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/smweber/devvm/internal/config"
	"github.com/spf13/cobra"
)

// Version is stamped by release builds (-ldflags -X); "dev" for plain go build.
var Version = "dev"

// App carries process-wide context to command handlers.
type App struct {
	ConfigDir string
	Stdout    io.Writer
	Stderr    io.Writer
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
	app := newApp()
	root := app.rootCmd()
	if err := root.Execute(); err != nil {
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
			"  remote-unmanaged  an existing host devvm adopts hands-off (over ssh)\n\n" +
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
		a.vncCmd(),
		a.authCmd(),
	)...)
	root.AddCommand(group(groupConfigure,
		a.reposCmd(),
		a.portsCmd(),
		a.keysCmd(),
		a.defaultsCmd(),
		a.statusCmd(),
	)...)
	root.AddCommand(a.daemonCmd()) // hidden; falls under "Additional Commands"
	return root
}

// completeMachines is the ValidArgsFunction for commands that take a machine
// name: it offers registered machines (and, once the smol backend is wired,
// live-but-unregistered VMs).
func (a *App) completeMachines(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names, _ := config.List(a.ConfigDir)
	return names, cobra.ShellCompDirectiveNoFileComp
}
