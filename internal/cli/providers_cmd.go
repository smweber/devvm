package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
	"github.com/smweber/devvm/internal/provider/hetzner"
)

// `devvm providers token NAME [--clear]` (providers.md §1 "The token"): the
// one leaf of the providers noun, and what every no-token message names.
// The token is per-host credential state, so it goes to
// <State>/providers/NAME.token (0600 in a 0700 dir), never to devvm.toml.

func (a *App) providersCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "providers",
		Short: "Manage this host's cloud provider credentials (the API token)",
		Long: "Provider servers (backend hetzner) are discovered from the provider's API with\n" +
			"this host's token; [providers.NAME] in devvm.toml says where new ones go.\n" +
			"The token never goes in devvm.toml: it is kept in the state dir, 0600.",
	}
	c.AddCommand(a.providersTokenCmd())
	return c
}

func (a *App) providersTokenCmd() *cobra.Command {
	var clear bool
	c := &cobra.Command{
		Use:   "token PROVIDER [--clear]",
		Short: "Set (validated with one read-only API call) or --clear this host's API token",
		Long: "Read the token with echo off on a terminal, or one line from stdin otherwise,\n" +
			"check it with one GET /servers, and store it in the state dir (0600). A token\n" +
			"that does not work is never written. --clear removes the stored token.\n" +
			hetzner.TokenEnv + " in the environment overrides the file.\n\n" +
			"Use a separate Hetzner project for devvm boxes: a token reaches every server\n" +
			"in its project.",
		Args:      cobra.ExactArgs(1),
		ValidArgs: config.KnownProviders(),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runProvidersToken(args[0], clear)
		},
	}
	c.Flags().BoolVar(&clear, "clear", false, "remove the stored token")
	return c
}

func (a *App) runProvidersToken(name string, clear bool) error {
	if !slices.Contains(config.KnownProviders(), name) {
		return fmt.Errorf("unknown provider %q (devvm knows: %s)", name, strings.Join(config.KnownProviders(), ", "))
	}
	path := provider.TokenPath(a.Paths, name)
	if clear {
		err := os.Remove(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintf(a.Stdout, "devvm: no %s token stored on this host\n", providerTitle(name))
		case err != nil:
			return err
		default:
			fmt.Fprintf(a.Stdout, "devvm: removed %s\n", tildePath(path))
		}
		a.noteEnvOverride()
		return nil
	}
	if _, err := a.readAndSaveToken(name); err != nil {
		return err
	}
	a.noteEnvOverride()
	return nil
}

// readAndSaveToken reads a token (readToken), checks it with one GET
// /servers, and stores it: the token command's core, and the first-use
// prompt of a lifecycle command (askProviderToken). A token that does not
// work is never written. It returns the client the token made.
func (a *App) readAndSaveToken(name string) (provider.Provider, error) {
	path := provider.TokenPath(a.Paths, name)
	tok, err := a.readToken(name)
	if err != nil {
		return nil, err
	}
	cl, err := a.providerAPIFor(name, tok)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerTimes.get)
	defer cancel()
	servers, err := cl.List(ctx) // the one validating call; read-only
	if err != nil {
		return nil, fmt.Errorf("the token did not work, so nothing was saved: %w", err)
	}
	if err := writeTokenFile(path, tok); err != nil {
		return nil, err
	}
	fmt.Fprintf(a.Stdout, "devvm: saved the %s token to %s (its project has %s)\n", providerTitle(name), tildePath(path), countServers(len(servers)))
	return cl, nil
}

// askProviderToken is the first-use prompt (providers.md §1): a lifecycle
// command on a terminal with no token asks for it once, validates and
// stores it, then carries on with the client. Without a terminal it never
// prompts: the caller gets back the no-token error, which names the
// command. The env var still wins once the file exists.
func (a *App) askProviderToken(name string, noToken error) (provider.Provider, error) {
	if a.Stdin != nil || !stdinIsTTY() {
		return nil, noToken
	}
	fmt.Fprintf(a.Stderr, "devvm: this host has no %s API token yet (it is kept in the state dir, never in devvm.toml).\n"+
		"devvm: use a separate %s project for devvm boxes: a token reaches every server in its project.\n",
		providerTitle(name), providerTitle(name))
	return a.readAndSaveToken(name)
}

// noteEnvOverride says when HCLOUD_TOKEN will win over the file just
// written or removed, so a rotation that seems to do nothing is explained.
func (a *App) noteEnvOverride() {
	if strings.TrimSpace(os.Getenv(hetzner.TokenEnv)) != "" {
		fmt.Fprintf(a.Stderr, "devvm: note: %s is set in this environment and overrides the stored token\n", hetzner.TokenEnv)
	}
}

// readToken reads the token: with echo off when stdin is a terminal, else
// one line from stdin (a password manager's pipe). Never echoed, never in
// argv.
func (a *App) readToken(name string) (string, error) {
	var line string
	if a.Stdin == nil && stdinIsTTY() {
		fmt.Fprintf(a.Stderr, "%s API token (read & write; Security > API tokens in the console): ", providerTitle(name))
		b, err := term.ReadPassword(os.Stdin.Fd())
		fmt.Fprintln(a.Stderr)
		if err != nil {
			return "", fmt.Errorf("reading the token: %w", err)
		}
		line = string(b)
	} else {
		l, err := bufio.NewReader(a.stdin()).ReadString('\n')
		if err != nil && l == "" {
			return "", errors.New("no token on stdin (pipe one line, or run this on a terminal)")
		}
		line = l
	}
	tok := strings.TrimSpace(line)
	switch {
	case tok == "":
		return "", errors.New("empty token; nothing was saved")
	case strings.ContainsAny(tok, " \t\r\n"):
		return "", errors.New("a token is one word; nothing was saved")
	}
	return tok, nil
}

// writeTokenFile writes the token as one line, 0600, in a 0700 dir, via a
// temp file and a rename, so a reader never sees half a token and a
// replaced token never has a moment with a wider mode.
func writeTokenFile(path, tok string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil { // devvm's own dir: tighten one made wider
		return err
	}
	tmp, err := os.CreateTemp(dir, ".token.*") // 0600 regardless of umask
	if err != nil {
		return err
	}
	_, werr := tmp.WriteString(tok + "\n")
	if werr == nil {
		werr = tmp.Chmod(0o600)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return werr
}
