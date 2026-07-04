package cli

import (
	"github.com/spf13/cobra"
)

// machineArg builds the standard "one machine name" positional spec with
// completion.
func (a *App) machineArg(cmd *cobra.Command) {
	cmd.Args = cobra.ExactArgs(1)
	cmd.ValidArgsFunction = a.completeMachines
}

func (a *App) createCmd() *cobra.Command {
	var s createSpec
	c := &cobra.Command{
		Use:   "create NAME",
		Short: "Create/adopt + bootstrap a machine (any backend)",
		Long: "Create a machine of any backend. Unset fields are prompted for on a\n" +
			"terminal; pass them as flags to run non-interactively.\n\n" +
			"  smol              a new local smolvm microVM\n" +
			"  remote-managed    a remote host devvm shapes (installs prereqs, may harden)\n" +
			"  remote-unmanaged  adopt an existing host hands-off (checks prereqs only)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s.Name = args[0]
			return a.runCreate(s)
		},
	}
	f := c.Flags()
	f.StringVarP(&s.Backend, "backend", "b", "", "smol | remote-managed | remote-unmanaged")
	f.IntVarP(&s.Memory, "memory", "m", 0, "smol: VM memory in MiB")
	f.StringVar(&s.SSHHost, "ssh-host", "", "remote: ssh destination (host or user@host)")
	f.IntVar(&s.SSHPort, "ssh-port", 0, "remote: ssh port (default 22)")
	f.StringVar(&s.Identity, "identity", "", "remote: ssh identity file")
	f.StringVar(&s.Transport, "transport", "", "remote: ssh|mosh (default ssh)")
	f.StringVar(&s.Provision, "provision", "", "provisioner: url:/cmd:/none (default from backend)")
	return c
}

func (a *App) bootstrapCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "bootstrap NAME",
		Short: "Resume/rerun guest provisioning",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runBootstrap(args[0]) },
	}
	a.machineArg(c)
	return c
}

func (a *App) shellCmd() *cobra.Command {
	var transport string
	c := &cobra.Command{
		Use:   "shell NAME",
		Short: "Open a raw login shell (no tmux)",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runShell(args[0], transport) },
	}
	c.Flags().StringVar(&transport, "transport", "", "remote transport: ssh|mosh (default from conf)")
	a.machineArg(c)
	return c
}

func (a *App) attachCmd() *cobra.Command {
	var transport string
	c := &cobra.Command{
		Use:   "attach NAME",
		Short: "Attach to the persistent dev tmux session",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runAttach(args[0], transport) },
	}
	c.Flags().StringVar(&transport, "transport", "", "remote transport: ssh|mosh (default from conf)")
	a.machineArg(c)
	return c
}

func (a *App) execCmd() *cobra.Command {
	c := &cobra.Command{
		Use:                "exec NAME CMD...",
		Short:              "Run a command as the dev user",
		Args:               cobra.MinimumNArgs(2),
		DisableFlagParsing: true, // pass CMD's own flags through untouched
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runExec(args[0], args[1:])
		},
	}
	return c
}

func (a *App) authCmd() *cobra.Command {
	var installAgent bool
	c := &cobra.Command{
		Use:       "auth NAME [github|codex|claude|all]",
		Short:     "Log in to github/codex/claude (all if omitted)",
		Args:      cobra.RangeArgs(1, 2),
		ValidArgs: []string{"github", "codex", "claude", "all"},
		RunE: func(cmd *cobra.Command, args []string) error {
			tool := "all"
			if len(args) == 2 {
				tool = args[1]
			}
			return a.runAuth(args[0], tool, installAgent)
		},
	}
	c.Flags().BoolVar(&installAgent, "install-agent", false,
		"pre-approve installing the helper agent in ~/.local/bin on an unmanaged host")
	c.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		switch len(args) {
		case 0:
			return a.completeMachines(cmd, args, toComplete)
		case 1:
			return []string{"github", "codex", "claude", "all"}, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return c
}

func (a *App) reposCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "repos NAME",
		Short: "Clone this machine's configured repos",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runRepos(args[0]) },
	}
	a.machineArg(c)
	return c
}

func (a *App) portCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "port NAME HOST:GUEST",
		Short: "Forward host port HOST -> guest GUEST (auto-bumps on conflict)",
		Args:  cobra.ExactArgs(2),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runPort(args[0], args[1]) },
	}
	return c
}

func (a *App) unportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "unport NAME HOST:GUEST",
		Short: "Remove a configured forward + tear it down",
		Args:  cobra.ExactArgs(2),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runUnport(args[0], args[1]) },
	}
	return c
}

func (a *App) tunnelCmd() *cobra.Command {
	c := &cobra.Command{
		Use:       "tunnel NAME [up|down|status]",
		Short:     "Start/stop/inspect this machine's forwards",
		Args:      cobra.RangeArgs(1, 2),
		ValidArgs: []string{"up", "down", "status"},
		RunE: func(cmd *cobra.Command, args []string) error {
			action := "up"
			if len(args) == 2 {
				action = args[1]
			}
			return a.runTunnel(args[0], action)
		},
	}
	return c
}

func (a *App) startCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "start NAME",
		Short: "Start the machine (backend-aware)",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runStart(args[0]) },
	}
	a.machineArg(c)
	return c
}

func (a *App) stopCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "stop NAME",
		Short: "Stop the machine (backend-aware)",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runStop(args[0]) },
	}
	a.machineArg(c)
	return c
}

func (a *App) deleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete the machine (backend-aware)",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runDelete(args[0]) },
	}
	a.machineArg(c)
	return c
}

func (a *App) statusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status [NAME]",
		Short: "Machine status; no NAME lists all machines",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return a.runStatusAll()
			}
			return a.runStatus(args[0])
		},
		ValidArgsFunction: a.completeMachines,
	}
	return c
}

func (a *App) vncCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "vnc NAME",
		Short: "Tunnel (if needed) + open VNC (remote machines)",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runVNC(args[0]) },
	}
	a.machineArg(c)
	return c
}

func (a *App) authorizeKeyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:                "authorize-key NAME [KEY|--from-github USER]",
		Short:              "Add a client pubkey (remote machines)",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runAuthorizeKey(args[0], args[1:])
		},
	}
	return c
}

func (a *App) keysCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "keys NAME",
		Short: "List authorized keys (remote machines)",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runKeys(args[0]) },
	}
	a.machineArg(c)
	return c
}

func (a *App) revokeKeyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "revoke-key NAME PATTERN",
		Short: "Remove one key by fingerprint, comment, or key-line substring",
		Args:  cobra.ExactArgs(2),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runRevokeKey(args[0], args[1]) },
	}
	return c
}

func (a *App) cleanupKeysCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cleanup-keys NAME",
		Short: "Remove duplicate authorized keys (remote machines)",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runCleanupKeys(args[0]) },
	}
	a.machineArg(c)
	return c
}

func (a *App) lockdownCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "lockdown NAME",
		Short: "Firewall + sshd hardening + auto-updates (remote machines)",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runLockdown(args[0]) },
	}
	a.machineArg(c)
	return c
}
