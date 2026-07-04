package cli

import (
	"context"
	"fmt"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/bootstrap"
	"github.com/smweber/devvm/internal/config"
)

func (a *App) runBootstrap(name string) error {
	m, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	ctx := context.Background()
	// Prereqs installs on managed boxes (smol, remote-managed) and only checks on
	// adopted remote-unmanaged hosts.
	if err := bootstrap.Prereqs(ctx, b, m); err != nil {
		return err
	}
	// The bootstrap-hook shapes the OS, so it runs only on boxes devvm owns; adopted
	// hosts are left untouched.
	if m.Managed() {
		if err := bootstrap.RunHook(ctx, b, m); err != nil {
			return err
		}
	}
	// Remote backends get key seeding (opt-in via conf; harmless when empty).
	// Hardening modifies the system, so it's managed-only.
	if m.IsRemote() {
		if err := a.seedAuthorizedKeys(b, m.AuthorizedKeysGithub, m.AuthorizedKeys); err != nil {
			return err
		}
		if m.Managed() && m.Harden {
			if err := bootstrap.Harden(ctx, b, m); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) runLockdown(name string) error {
	m, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	if err := requireRemote(m, "lockdown"); err != nil {
		return err
	}
	if !m.Managed() {
		ok, err := confirm(fmt.Sprintf("'%s' is an unmanaged host — really run lockdown on it?", name))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("aborted")
		}
	}
	return bootstrap.Harden(context.Background(), b, m)
}

// seedAuthorizedKeys pushes keys from the machine conf at bootstrap. A source
// that yields no keys (e.g. a keyless github user) warns and moves on rather
// than aborting the whole bootstrap.
func (a *App) seedAuthorizedKeys(b backend.Backend, githubUsers, files []string) error {
	push := func(lines []string) error {
		return a.addGuestKeys(b, lines)
	}
	for _, u := range githubUsers {
		keys, err := githubKeys(u)
		if err != nil {
			fmt.Fprintf(a.Stderr, "devvm: %v\n", err)
			continue
		}
		if err := push(keys); err != nil {
			return err
		}
	}
	for _, f := range files {
		keys, err := resolvePubkeys([]string{config.ExpandHome(f)})
		if err != nil {
			fmt.Fprintf(a.Stderr, "devvm: %v\n", err)
			continue
		}
		if err := push(keys); err != nil {
			return err
		}
	}
	return nil
}
