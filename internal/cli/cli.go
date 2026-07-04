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

func (a *App) rootCmd() *cobra.Command {
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

	root.AddCommand(
		a.createCmd(),
		a.bootstrapCmd(),
		a.attachCmd(),
		a.shellCmd(),
		a.execCmd(),
		a.authCmd(),
		a.reposCmd(),
		a.portsCmd(),
		a.startCmd(),
		a.stopCmd(),
		a.deleteCmd(),
		a.statusCmd(),
		a.vncCmd(),
		a.keysCmd(),
		a.defaultsCmd(),
		a.lockdownCmd(),
		a.daemonCmd(),
	)
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
