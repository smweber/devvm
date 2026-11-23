package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// hostGH runs this host's gh and returns its stdout, also on failure. Its
// error carries gh's own stderr (which never holds the token). A var so a
// test fakes the host's login instead of reading a real one.
var hostGH = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if msg := strings.TrimSpace(stderr.String()); err != nil && msg != "" {
		err = fmt.Errorf("%w: %s", err, lastLine(msg))
	}
	return out, err
}

// shareAuth is setup's share-auth step (share-auth.md §2): for each tool
// in the template's share_auth, copy this host's login into the box. It
// runs after harden, so a problem here never costs a box its hardening,
// and before the hook, so a hook that clones private dotfiles has the
// login (and runs with it, §6). Every failure is one warning line and
// setup goes on. Only a box devvm made gets here (shape refuses an entry),
// and for a hub machine "this host" is the hub: its own login (§5).
func (a *App) shareAuth(ctx context.Context, m *config.Machine, b backend.Backend, tools []string) {
	for _, tool := range tools {
		if tool != config.ShareAuthGitHub {
			continue // the loader and the flag layer refuse any other name
		}
		if err := a.shareGitHub(ctx, m, b); err != nil {
			fmt.Fprintf(a.Stderr, "devvm: share_auth github: %v (setup continues)\n", err)
		}
	}
}

// ghLoginScript installs the GitHub login it reads on stdin, as the login
// user: the token is never in argv, the environment, a file on either
// host, or a log. GH_TOKEN/GITHUB_TOKEN would make `gh auth login` refuse
// (and would shadow the stored login), so they go first.
// --insecure-storage (gh 2.26+) only when this gh knows it: a box has no
// keyring, and an older gh stores the token in hosts.yml anyway.
// The probe reads /dev/null, so only `gh auth login` sees the token pipe.
// setup-git makes git use gh's login, git_protocol https makes gh's own
// clones use it; either failing after the login exits ghPartialExit,
// naming the command last.
const ghLoginScript = `unset GH_TOKEN GITHUB_TOKEN
set -e
if gh auth login --help </dev/null 2>&1 | grep -q -- --insecure-storage; then
  gh auth login --hostname github.com --with-token --insecure-storage
else
  gh auth login --hostname github.com --with-token
fi
gh auth setup-git || { echo 'gh auth setup-git' >&2; exit 87; }
gh config set git_protocol https --host github.com || { echo 'gh config set git_protocol https --host github.com' >&2; exit 87; }`

// ghPartialExit is ghLoginScript's exit when the login went in but a
// command after it failed. The next setup's guest check then sees a
// working login and leaves it alone, so the command is the user's to run.
const ghPartialExit = 87

// shareGitHub copies this host's gh login for github.com into the box.
func (a *App) shareGitHub(ctx context.Context, m *config.Machine, b backend.Backend) error {
	// 1. A working login in the box wins, whoever's it is: a login of the
	// box's own (gh auth login in attach), or an earlier copy still alive.
	// A login shell, as the repos step checks gh.
	if b.Run(ctx, backend.ExecOpts{Login: true, Stdout: io.Discard, Stderr: io.Discard}, "gh", "api", "user") == nil {
		fmt.Fprintf(a.Stderr, "devvm: gh in '%s' already has a working GitHub login; share_auth leaves it alone\n", m.Name)
		return nil
	}
	// 2. This host's token. gh honours GH_TOKEN/GITHUB_TOKEN here, so a
	// PAT the user exported is what is shared.
	out, err := hostGH(ctx, "auth", "token", "--hostname", "github.com")
	token := strings.TrimSpace(string(out))
	if err != nil || token == "" {
		why := "no login"
		if errors.Is(err, exec.ErrNotFound) {
			why = "no gh"
		} else if err != nil {
			why = "gh auth token: " + err.Error()
		}
		return fmt.Errorf("nothing to share from this host (%s); run 'gh auth login' here and 'devvm setup %s' again, or 'gh auth login' inside the box", why, m.Name)
	}
	// 3. An expiring login is refreshed on this host, which ends every copy.
	if expiringGHLogin(ctx) {
		return fmt.Errorf("this host's GitHub login expires and is refreshed here, which would end the copy; run 'gh auth login --expiring=false' here, then 'devvm setup %s' again", m.Name)
	}
	// 4. The install: one exec, the token on its stdin only (smol passes
	// -i for it).
	var msg bytes.Buffer
	if err := b.Run(ctx, backend.ExecOpts{Stdin: strings.NewReader(token + "\n"), Stdout: &msg, Stderr: &msg},
		"sh", "-c", ghLoginScript); err != nil {
		last := lastLine(strings.ReplaceAll(msg.String(), token, "[token]"))
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == ghPartialExit {
			return fmt.Errorf("'%s' is logged in, but '%s' failed: run it in the box (setup leaves a working login alone)", m.Name, last)
		}
		detail := ""
		if last != "" {
			detail = ": " + last
		}
		return fmt.Errorf("installing the login in '%s' failed (%v)%s", m.Name, err, detail)
	}
	from := "this host"
	if h, err := hostname(); err == nil && h != "" {
		from = h
	}
	fmt.Fprintf(a.Stderr, "devvm: '%s' has %s's GitHub login (gh, and git over https)\n", m.Name, from)
	return nil
}

// expiringGHLogin reports whether this host's active github.com login is
// one gh refreshes (gh 2.103+ `--expiring`): a copy dies at the host's
// next refresh. A gh too old for --json cannot hold one, so anything that
// does not parse reads as not expiring. gh's exit status is not read: it
// is non-zero whenever any account has a problem.
func expiringGHLogin(ctx context.Context) bool {
	out, _ := hostGH(ctx, "auth", "status", "--json", "hosts")
	var st struct {
		Hosts map[string][]struct {
			Active      bool `json:"active"`
			Refreshable bool `json:"refreshable"`
		} `json:"hosts"`
	}
	if json.Unmarshal(out, &st) != nil {
		return false
	}
	for _, h := range st.Hosts["github.com"] {
		if h.Active && h.Refreshable {
			return true
		}
	}
	return false
}

// lastLine is s's last non-empty line.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
