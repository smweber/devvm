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
	f.StringVar(&s.Provision, "provision", "", "provisioner: url:/cmd:/none (default: none, or config.toml)")
	f.BoolVarP(&s.Yes, "yes", "y", false, "don't prompt; resolve unset fields from flags, config.toml, then built-in defaults")
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

// reposCmd groups per-machine git repo management. NAME is the first positional
// of each leaf, keeping "first arg = machine" consistent across the whole CLI.
func (a *App) reposCmd() *cobra.Command {
	c := &cobra.Command{Use: "repos", Short: "Manage a machine's git repositories"}

	add := &cobra.Command{
		Use:   "add NAME [REPO...]",
		Short: "Add repo(s) to the conf and clone them (owner/repo, https://, ssh://)",
		Long: "Add repositories to a machine and clone them into ~/src.\n\n" +
			"REPO may be GitHub \"owner/repo\" shorthand (cloned via gh) or any git\n" +
			"URL (https://, ssh://, git@host:path — cloned via git). With no REPO and\n" +
			"a terminal it prompts, prefilling from the current directory's git origin.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			noClone, _ := cmd.Flags().GetBool("no-clone")
			return a.runReposAdd(args[0], args[1:], !noClone)
		},
		ValidArgsFunction: a.completeMachines,
	}
	add.Flags().Bool("no-clone", false, "record in the conf only; don't clone now")

	rm := &cobra.Command{
		Use:   "rm NAME REPO",
		Short: "Remove a repo from the conf (leaves the guest checkout)",
		Args:  cobra.ExactArgs(2),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runReposRm(args[0], args[1]) },
	}

	list := &cobra.Command{
		Use:   "list NAME",
		Short: "List a machine's configured repos",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runReposList(args[0]) },
	}
	a.machineArg(list)

	clone := &cobra.Command{
		Use:   "clone NAME",
		Short: "Clone all of a machine's configured repos",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runReposClone(args[0]) },
	}
	a.machineArg(clone)

	c.AddCommand(add, rm, list, clone)
	return c
}

// portsCmd groups per-machine port forwarding: add/rm/list mutate the conf,
// up/down drive the live forwards.
func (a *App) portsCmd() *cobra.Command {
	c := &cobra.Command{Use: "ports", Short: "Manage a machine's port forwards"}

	add := &cobra.Command{
		Use:   "add NAME HOST:GUEST",
		Short: "Forward host port HOST -> guest GUEST (auto-bumps on conflict)",
		Args:  cobra.ExactArgs(2),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runPort(args[0], args[1]) },
	}
	rm := &cobra.Command{
		Use:   "rm NAME HOST:GUEST",
		Short: "Remove a configured forward + tear it down",
		Args:  cobra.ExactArgs(2),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runUnport(args[0], args[1]) },
	}
	list := &cobra.Command{
		Use:   "list NAME",
		Short: "List configured + live forwards",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runPortsList(args[0]) },
	}
	a.machineArg(list)
	up := &cobra.Command{
		Use:   "up NAME",
		Short: "Bring up all configured forwards",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.tunnelUp(args[0]) },
	}
	a.machineArg(up)
	down := &cobra.Command{
		Use:   "down NAME",
		Short: "Tear down live forwards",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.tunnelDown(args[0]) },
	}
	a.machineArg(down)

	c.AddCommand(add, rm, list, up, down)
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

// keysCmd groups authorized_keys management for remote machines (host-side over
// one exec; see internal/keys). NAME is the first positional of each leaf.
func (a *App) keysCmd() *cobra.Command {
	c := &cobra.Command{Use: "keys", Short: "Manage a machine's authorized SSH keys"}

	add := &cobra.Command{
		Use:                "add NAME [KEY|--from-github USER]",
		Short:              "Add a client pubkey (remote machines)",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true, // pass --from-github through untouched
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runAuthorizeKey(args[0], args[1:])
		},
	}
	list := &cobra.Command{
		Use:   "list NAME",
		Short: "List authorized keys (remote machines)",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runKeys(args[0]) },
	}
	a.machineArg(list)
	rm := &cobra.Command{
		Use:   "rm NAME PATTERN",
		Short: "Remove one key by fingerprint, comment, or key-line substring",
		Args:  cobra.ExactArgs(2),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runRevokeKey(args[0], args[1]) },
	}
	dedupe := &cobra.Command{
		Use:   "dedupe NAME",
		Short: "Remove duplicate authorized keys (remote machines)",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runCleanupKeys(args[0]) },
	}
	a.machineArg(dedupe)

	c.AddCommand(add, list, rm, dedupe)
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
