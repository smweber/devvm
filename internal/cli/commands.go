package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/smweber/devvm/internal/auth"
	"github.com/smweber/devvm/internal/config"
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
		Use:   "create [NAME]",
		Short: "Create a local smol VM or a Hetzner server, then set it up from a template",
		Long: "Create a local smol VM (or, with --backend hetzner, a Hetzner Cloud server,\n" +
			"which bills until 'devvm delete') and shape it with 'setup'. NAME and any\n" +
			"unset fields are prompted for on a terminal, seeded from the template; pass\n" +
			"them as flags/args to run non-interactively. Precedence: flag > --template >\n" +
			"[templates.default] > built-in; a server's placement also falls back to\n" +
			"[providers.hetzner] in devvm.toml, which names its ssh key.\n\n" +
			"Remote boxes and hubs are not created: add one to devvm.toml with\n" +
			"'devvm config add NAME --backend remote|hub --host H', then 'devvm setup NAME'\n" +
			"for a managed box.",
		Args: cobra.MaximumNArgs(1),
		// `create HUB/NAME` is the one proxied leaf whose name exists nowhere
		// yet: the hub makes the VM, and with no --yes its huh form runs
		// there through the forwarded terminal. Without --template, --yes
		// resolves unset fields from the hub's own templates, deliberately:
		// they describe its VMs (hub.md §3). With --template the laptop
		// resolves it and sends every field explicitly (proxy.go).
		RunE: func(cmd *cobra.Command, args []string) error {
			// Refused here, before a HUB/NAME is proxied: the hub would
			// refuse it too, a round trip later.
			if err := checkCreateBackend(s.Backend); err != nil {
				return err
			}
			// A provider server is never on a hub (providers.md §6): there
			// is no exec to own there, so nothing for HUB/NAME to address.
			if len(args) > 0 && config.IsProviderBackend(s.Backend) {
				if _, _, onHub, _ := config.SplitHubName(args[0]); onHub {
					return fmt.Errorf("create %s --backend %s: a %s server is never on a hub; run 'devvm create NAME --backend %s' here",
						args[0], s.Backend, providerTitle(s.Backend), s.Backend)
				}
			}
			return a.hubOr(func(cmd *cobra.Command, args []string) error {
				if len(args) > 0 {
					s.Name = args[0]
				}
				s.noteSet(cmd.Flags())
				return a.runCreate(s)
			})(cmd, args)
		},
	}
	f := c.Flags()
	f.StringVarP(&s.Backend, "backend", "b", "", "smol (default) or hetzner (a cloud server; asked on a terminal when [providers.*] is set)")
	f.StringVar(&s.ServerType, "server-type", "", "hetzner: server type (template server_type; default "+config.DefaultServerType+")")
	f.StringVar(&s.Location, "location", "", "hetzner: location (template, then [providers.hetzner]; default "+config.DefaultLocation+")")
	f.StringVar(&s.Image, "image", "", "hetzner: image name or id (template, then [providers.hetzner]; default "+config.DefaultImage+")")
	f.StringVarP(&s.Template, flagTemplate, "t", "", "template to create from (default: default)")
	addTemplateFlags(f, &s.tmplFlags, flagMemory, flagDisk, flagHook)
	// The pre-template name of --hook, kept so scripts keep working.
	f.StringVar(&s.Hook, "bootstrap-hook", "", "deprecated: use --hook")
	_ = f.MarkHidden("bootstrap-hook")
	f.BoolVarP(&s.Yes, "yes", "y", false, "don't prompt; resolve unset fields from flags, the template, then built-in defaults")
	_ = c.RegisterFlagCompletionFunc(flagTemplate, a.completeTemplates)
	return c
}

func (a *App) shellCmd() *cobra.Command {
	var transport string
	c := &cobra.Command{
		Use:   "shell NAME",
		Short: "Open a raw login shell (no tmux)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if ok, err := hubMachineArg(args); err != nil {
				return err
			} else if ok {
				return a.proxyInteractive(cmd, args, transport)
			}
			return a.runShell(args[0], transport)
		},
	}
	c.Flags().StringVar(&transport, "transport", "", "remote transport: ssh|mosh (default: the entry's transport)")
	a.machineArg(c)
	return c
}

func (a *App) attachCmd() *cobra.Command {
	var transport string
	c := &cobra.Command{
		Use:   "attach NAME",
		Short: "Attach to the persistent dev tmux session",
		RunE: func(cmd *cobra.Command, args []string) error {
			if ok, err := hubMachineArg(args); err != nil {
				return err
			} else if ok {
				return a.proxyInteractive(cmd, args, transport)
			}
			return a.runAttach(args[0], transport)
		},
	}
	c.Flags().StringVar(&transport, "transport", "", "remote transport: ssh|mosh (default: the entry's transport)")
	a.machineArg(c)
	return c
}

func (a *App) execCmd() *cobra.Command {
	c := &cobra.Command{
		Use:                "exec NAME CMD...",
		Short:              "Run a command as the dev user",
		Args:               cobra.MinimumNArgs(2),
		DisableFlagParsing: true, // pass CMD's own flags through untouched
		RunE: a.hubOr(func(cmd *cobra.Command, args []string) error {
			return a.runExec(args[0], args[1:])
		}),
	}
	return c
}

func (a *App) authCmd() *cobra.Command {
	var installAgent bool
	// Selectors derive from auth.Choices() so adding a service updates the CLI
	// surface (usage, completion, uninstalled-skipping) in one place.
	choices := auth.Choices()
	argChoices := append(append([]string{}, choices...), "all")
	c := &cobra.Command{
		Use:       "auth NAME [" + strings.Join(argChoices, "|") + "]",
		Short:     "Log in to " + strings.Join(choices, "/") + " (all if omitted)",
		Args:      cobra.RangeArgs(1, 2),
		ValidArgs: argChoices,
		RunE: func(cmd *cobra.Command, args []string) error {
			if ok, err := hubMachineArg(args); err != nil {
				return err
			} else if ok {
				return a.authHubMachine(cmd, args)
			}
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
			return argChoices, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return c
}

// reposCmd groups per-machine git repo management. NAME is the first positional
// of each leaf, keeping "first arg = machine" consistent across the whole CLI.
// Repos are not tracked anywhere (config.md §3): `add` clones, `list` reads
// the guest's ~/src.
func (a *App) reposCmd() *cobra.Command {
	c := &cobra.Command{Use: "repos", Short: "Clone and list a machine's git repositories"}

	add := &cobra.Command{
		Use:   "add NAME [REPO...]",
		Short: "Clone repo(s) into ~/src (owner/repo, https://, ssh://)",
		Long: "Clone repositories into ~/src on a machine; one already there is skipped.\n\n" +
			"REPO may be GitHub \"owner/repo\" shorthand (cloned via gh) or any git\n" +
			"URL (https://, ssh://, git@host:path — cloned via git). With no REPO and\n" +
			"a terminal it prompts, prefilling from the current directory's git origin.",
		Args: cobra.MinimumNArgs(1),
		RunE: a.hubOr(func(cmd *cobra.Command, args []string) error {
			return a.runReposAdd(args[0], args[1:])
		}),
		ValidArgsFunction: a.completeMachines,
	}

	list := &cobra.Command{
		Use:   "list NAME",
		Short: "List the git checkouts in the machine's ~/src (needs it running)",
		RunE:  a.hubOr(func(cmd *cobra.Command, args []string) error { return a.runReposList(args[0]) }),
	}
	a.machineArg(list)

	c.AddCommand(add, list)
	return c
}

// portsCmd groups per-machine port forwarding: add/rm/list edit this host's
// saved forwards (<State>/ports/NAME, never the conf), up/down drive the live
// forwards.
func (a *App) portsCmd() *cobra.Command {
	c := &cobra.Command{Use: "ports", Short: "Manage a machine's port forwards"}

	var ephemeral bool
	add := &cobra.Command{
		Use:   "add NAME HOST:GUEST",
		Short: "Forward host port HOST -> guest GUEST (auto-bumps on conflict)",
		Args:  cobra.ExactArgs(2),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runPort(args[0], args[1], ephemeral) },
	}
	add.Flags().BoolVar(&ephemeral, "ephemeral", false, "forward now without saving it (lasts as long as the forward daemon)")
	rm := &cobra.Command{
		Use:   "rm NAME HOST:GUEST",
		Short: "Remove a configured forward + tear it down",
		Args:  cobra.ExactArgs(2),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runUnport(args[0], args[1]) },
	}
	list := &cobra.Command{
		Use:   "list [NAME]",
		Short: "List configured + live forwards; no NAME shows every machine",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return a.runPortsListAll()
			}
			return a.runPortsList(args[0])
		},
		ValidArgsFunction: a.completeMachines,
	}
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
	var yes bool
	c := &cobra.Command{
		Use:   "start NAME",
		Short: "Start the machine (backend-aware)",
		// A HUB/NAME start is proxied, then brings up this host's own
		// configured forwards (hub.md §3): the hub's start only knows the
		// hub's conf.
		RunE: func(cmd *cobra.Command, args []string) error {
			if ok, err := hubMachineArg(args); err != nil {
				return err
			} else if ok {
				return a.startHubMachine(cmd, args)
			}
			return a.runStart(args[0], yes)
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "don't confirm (a provider server devvm did not make)")
	a.machineArg(c)
	return c
}

func (a *App) stopCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "stop NAME",
		Short: "Stop the machine (backend-aware; a stopped Hetzner server still bills)",
		RunE:  a.hubOrStopForwards(func(cmd *cobra.Command, args []string) error { return a.runStop(args[0], yes) }),
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "don't confirm (a provider server devvm did not make)")
	a.machineArg(c)
	return c
}

func (a *App) deleteCmd() *cobra.Command {
	var force, yes bool
	c := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a smol VM or Hetzner server devvm made, or HUB/NAME (an entry is 'config rm')",
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runDelete(args[0], force, yes) },
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "don't confirm")
	c.Flags().BoolVar(&force, "force", false, "deprecated: no effect (a hub entry is removed with 'devvm config rm --force')")
	_ = c.Flags().MarkHidden("force")
	a.machineArg(c)
	c.ValidArgsFunction = a.completeDeletable
	return c
}

func (a *App) statusCmd() *cobra.Command {
	var verbose, plain, watch, local, exitOnEOF bool
	c := &cobra.Command{
		Use:   "status [NAME]",
		Short: "Machine status, grouped by backend; no NAME lists all machines",
		Long: "Show machine status. Without NAME, lists every machine grouped by backend\n" +
			"(smol, remote, hub)\n" +
			"with a live forward count. -v adds a lifecycle track, live smol resource\n" +
			"sizes, and per-machine forward detail. With NAME, always shows full detail.\n" +
			"A remote box is probed first (a live ssh master, else a TCP connect: never a\n" +
			"login) and is reachable, unreachable, or ? when never probed or behind a\n" +
			"ProxyJump; -v says when and how it was last reached.\n" +
			"Machines on a hub are listed as HUB/NAME with the state the hub reports\n" +
			"(each hub is asked in parallel, with a short timeout; a hub that does not\n" +
			"answer shows its last listing as 'unreachable'). --local lists this host\n" +
			"only: no hub is read or dialed, and remote boxes show their last probe.\n" +
			"--plain emits one tab-separated\n" +
			"'name<TAB>backend<TAB>state<TAB>forwards<TAB>managed<TAB>verified' row per machine (no\n" +
			"headers or grouping) for scripts: backend is smol, remote or hub; state is\n" +
			"one of running, stopped, reachable, unreachable or ?; forwards is up:N,\n" +
			"reconnecting:N, down (ports configured, no daemon) or - (none configured);\n" +
			"managed is managed, unmanaged (a smol VM devvm did not create, or an\n" +
			"adopted remote host), shadowed (a devvm.toml entry hiding a local smol VM of\n" +
			"the same name) or - (hubs, and machines on a hub too old to say); verified\n" +
			"is verified=<unix>:<how> or verified=- for a remote box, - otherwise.\n" +
			"Consumers should treat any other token as unknown. A devvm.toml that does\n" +
			"not parse fails status (non-zero); under --watch it pauses the stream until\n" +
			"the file parses again.\n" +
			"--plain --watch keeps running and re-emits the whole listing, blank-line\n" +
			"separated, whenever devvm changes a machine's state or a hub reports one\n" +
			"(no polling; remote boxes are probed at start and on 'devvm __refresh').\n" +
			"Changes made behind devvm's back — 'smolvm machine stop' run\n" +
			"directly, a VM crash — are not observed; re-run a plain status on demand\n" +
			"for those.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if watch && !plain {
				return fmt.Errorf("--watch requires --plain")
			}
			if watch && len(args) > 0 {
				return fmt.Errorf("--watch lists every machine; it does not take a NAME")
			}
			if local && len(args) > 0 {
				return fmt.Errorf("--local lists every machine on this host; it does not take a NAME")
			}
			if watch {
				return a.runStatusWatch(cmd.Context(), local, exitOnEOF)
			}
			if plain {
				return a.runStatusPlain(local)
			}
			if len(args) == 0 {
				return a.runStatusAll(verbose, local)
			}
			return a.runStatus(args[0])
		},
		ValidArgsFunction: a.completeAnyMachine, // status takes a hub too
	}
	c.Flags().BoolVarP(&verbose, "verbose", "v", false, "expand lifecycle, live resources, and forwards")
	c.Flags().BoolVar(&plain, "plain", false, "machine-readable: 'name<TAB>backend<TAB>state<TAB>forwards<TAB>managed<TAB>verified' per line")
	c.Flags().BoolVar(&watch, "watch", false, "with --plain: re-emit the listing on every devvm-made change (event-driven, not polled)")
	c.Flags().BoolVar(&local, "local", false, "this host only: skip every hub (what a hub runs for its own callers)")
	// Hidden: the laptop's hub watch pipe sets it (hublist.go). Opt-in only,
	// so the menu bar app's own watch child (stdin /dev/null: EOF at once)
	// and an interactive user are untouched.
	c.Flags().BoolVar(&exitOnEOF, strings.TrimPrefix(statusExitFlag, "--"), false, "with --watch: exit when stdin reaches EOF (for a watcher held over ssh)")
	_ = c.Flags().MarkHidden(strings.TrimPrefix(statusExitFlag, "--"))
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
		RunE: a.hubOr(func(cmd *cobra.Command, args []string) error {
			return a.runAuthorizeKey(args[0], args[1:])
		}),
	}
	list := &cobra.Command{
		Use:   "list NAME",
		Short: "List authorized keys (remote machines)",
		RunE:  a.hubOr(func(cmd *cobra.Command, args []string) error { return a.runKeys(args[0]) }),
	}
	a.machineArg(list)
	rm := &cobra.Command{
		Use:   "rm NAME PATTERN",
		Short: "Remove one key by fingerprint, comment, or key-line substring",
		Args:  cobra.ExactArgs(2),
		RunE:  a.hubOr(func(cmd *cobra.Command, args []string) error { return a.runRevokeKey(args[0], args[1]) }),
	}
	dedupe := &cobra.Command{
		Use:   "dedupe NAME",
		Short: "Remove duplicate authorized keys (remote machines)",
		RunE:  a.hubOr(func(cmd *cobra.Command, args []string) error { return a.runCleanupKeys(args[0]) }),
	}
	a.machineArg(dedupe)

	c.AddCommand(add, list, rm, dedupe)
	return c
}

func (a *App) lockdownCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "lockdown NAME",
		Short: "Firewall + sshd hardening + auto-updates (remote machines)",
		RunE:  a.hubOr(func(cmd *cobra.Command, args []string) error { return a.runLockdown(args[0]) }),
	}
	a.machineArg(c)
	return c
}
