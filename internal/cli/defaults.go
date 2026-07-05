package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/bootstrap"
	"github.com/smweber/devvm/internal/config"
	"github.com/spf13/cobra"
)

// defaultsCmd groups the global create-time defaults kept in config.toml. Unlike
// the other multi-op nouns these are not per-machine, so no NAME positional; the
// first positional of set/unset is the KEY instead.
func (a *App) defaultsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "defaults",
		Short: "Manage global create-time defaults (config.toml)",
		Long: "Global defaults seed unset `create` fields, with precedence\n" +
			"flag > config.toml > built-in. They apply only at create time; a\n" +
			"machine's own conf is the source of truth afterward.",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "Show every default: effective value and source",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return a.runDefaultsList() },
	}

	set := &cobra.Command{
		Use:               "set KEY VALUE",
		Short:             "Set a default in config.toml (validated)",
		Args:              cobra.ExactArgs(2),
		RunE:              func(cmd *cobra.Command, args []string) error { return a.runDefaultsSet(args[0], args[1]) },
		ValidArgsFunction: completeDefaultKeyThenValue,
	}

	unset := &cobra.Command{
		Use:               "unset KEY",
		Short:             "Remove a default, reverting to the built-in",
		Args:              cobra.ExactArgs(1),
		RunE:              func(cmd *cobra.Command, args []string) error { return a.runDefaultsUnset(args[0]) },
		ValidArgsFunction: completeDefaultKey,
	}

	path := &cobra.Command{
		Use:   "path",
		Short: "Print the config.toml path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(a.Stdout, config.DefaultsPath(a.ConfigDir))
			return nil
		},
	}

	c.AddCommand(list, set, unset, path)
	return c
}

func (a *App) runDefaultsList() error {
	d, err := config.LoadDefaults(a.ConfigDir)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tVALUE\tSOURCE")
	for _, key := range config.DefaultKeys {
		val, set, err := d.Get(key)
		if err != nil {
			return err
		}
		source := "config.toml"
		if !set {
			val, source = compiledDefaultDisplay(key), "built-in"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", key, val, source)
	}
	return tw.Flush()
}

func (a *App) runDefaultsSet(key, value string) error {
	if key == "bootstrap-hook" {
		// Validate the spec here — internal/config can't import internal/bootstrap.
		if _, err := bootstrap.ParseSpec(value); err != nil {
			return err
		}
	}
	d, err := config.LoadDefaults(a.ConfigDir)
	if err != nil {
		return err
	}
	if err := d.Set(key, value); err != nil {
		return err
	}
	if err := config.SaveDefaults(a.ConfigDir, d); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "set %s = %s\n", key, value)
	return nil
}

func (a *App) runDefaultsUnset(key string) error {
	d, err := config.LoadDefaults(a.ConfigDir)
	if err != nil {
		return err
	}
	if err := d.Unset(key); err != nil {
		return err
	}
	if err := config.SaveDefaults(a.ConfigDir, d); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "unset %s (reverts to built-in)\n", key)
	return nil
}

// compiledDefaultDisplay describes the built-in fallback for a key, shown by
// `list` when there's no override. Kept here (not in config) because the memory
// default is host-derived and lives in the cli layer.
func compiledDefaultDisplay(key string) string {
	switch key {
	case "bootstrap-hook":
		return bootstrap.KindNone
	case "memory":
		return fmt.Sprintf("%d (≈half of host RAM)", suggestedMemoryMiB())
	case "disk":
		return fmt.Sprintf("%d (GiB)", backend.SmolDefaultDiskGiB)
	case "transport":
		return config.TransportSSH
	default:
		return ""
	}
}

// completeDefaultKey offers the known default keys, each with its help text.
func completeDefaultKey(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return defaultKeyCompletions(), cobra.ShellCompDirectiveNoFileComp
}

// completeDefaultKeyThenValue completes the KEY first, then suggests values for
// keys with a fixed set (transport).
func completeDefaultKeyThenValue(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return defaultKeyCompletions(), cobra.ShellCompDirectiveNoFileComp
	case 1:
		if args[0] == "transport" {
			return []string{config.TransportSSH, config.TransportMosh}, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	default:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

func defaultKeyCompletions() []string {
	out := make([]string, 0, len(config.DefaultKeys))
	for _, k := range config.DefaultKeys {
		out = append(out, k+"\t"+config.DefaultKeyHelp(k))
	}
	return out
}
