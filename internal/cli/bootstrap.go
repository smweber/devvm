package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/provision"
)

func (a *App) runBootstrap(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if m.Unmanaged {
		return fmt.Errorf("'%s' is unmanaged; refusing to bootstrap it", name)
	}
	ctx := context.Background()
	if err := provision.Prereqs(ctx, b, m); err != nil {
		return err
	}
	if err := provision.Run(ctx, b, m); err != nil {
		return err
	}
	// Exposed backends (ssh/cloud) get key seeding and optional hardening.
	if m.IsExposed() {
		if err := a.seedAuthorizedKeys(b, m.AuthorizedKeysGithub, m.AuthorizedKeys); err != nil {
			return err
		}
		if m.Harden {
			if err := provision.Harden(ctx, b, m); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) runLockdown(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if err := requireSSH(m, "lockdown"); err != nil {
		return err
	}
	if m.Unmanaged {
		ok, err := confirm(fmt.Sprintf("'%s' is an unmanaged host — really run lockdown on it?", name))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("aborted")
		}
	}
	return provision.Harden(context.Background(), b, m)
}

// seedAuthorizedKeys pushes keys from the machine conf at bootstrap. A source
// that yields no keys (e.g. a keyless github user) warns and moves on rather
// than aborting the whole bootstrap.
func (a *App) seedAuthorizedKeys(b backend.Backend, githubUsers, files []string) error {
	push := func(lines []string) error {
		if len(lines) == 0 {
			return nil
		}
		stdin := strings.Join(lines, "\n") + "\n"
		return a.agentRun(b, strings.NewReader(stdin), a.Stdout, "keys", "add")
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
		keys, err := resolvePubkeys([]string{expandHome(f)})
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
