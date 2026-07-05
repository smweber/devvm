package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/smweber/devvm/internal/backend"
)

// ownerRepoRe matches the "owner/repo" GitHub shorthand: no scheme, no host,
// exactly one slash. Anything with a scheme (https://, ssh://) or an scp-like
// "user@host:path" is treated as a full URL and cloned with plain git.
var ownerRepoRe = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)

func (a *App) runReposList(name string) error {
	m, _, err := a.resolve(name)
	if err != nil {
		return err
	}
	if len(m.Repos) == 0 {
		fmt.Fprintf(a.Stdout, "devvm: no repos configured; add one with 'devvm repos add %s owner/repo'\n", name)
		return nil
	}
	for _, r := range m.Repos {
		fmt.Fprintf(a.Stdout, "  %s\n", r)
	}
	return nil
}

// runReposAdd records repo spec(s) in the conf and (unless noClone) clones them
// on the guest. With no repo arg it prompts, prefilling from the current
// directory's git origin so a host-side checkout is one keystroke to add.
func (a *App) runReposAdd(name string, repos []string, clone bool) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		def := normalizeRepo(originRemote())
		prompt := "Repo (owner/repo or URL): "
		if def != "" {
			prompt = fmt.Sprintf("Repo (owner/repo or URL) [%s]: ", def)
		}
		in, err := promptTTY(prompt)
		if err != nil {
			return err
		}
		if in == "" {
			in = def
		}
		if in == "" {
			return fmt.Errorf("a repo is required")
		}
		repos = []string{in}
	}

	var added []string
	for _, r := range repos {
		r = normalizeRepo(strings.TrimSpace(r))
		if r == "" || m.HasRepo(r) {
			continue
		}
		m.Repos = append(m.Repos, r)
		added = append(added, r)
	}
	if len(added) == 0 {
		fmt.Fprintf(a.Stdout, "devvm: nothing to add (already configured)\n")
		return nil
	}
	if err := m.Save(a.ConfigDir); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "devvm: added %d repo(s) to '%s'\n", len(added), name)
	if !clone {
		return nil
	}
	// The conf edit above is fine on a dormant box; cloning needs a live one.
	if err := requireProvisioned(m, b); err != nil {
		return err
	}
	return a.cloneRepos(b, added)
}

func (a *App) runReposRm(name, repo string) error {
	m, _, err := a.resolve(name)
	if err != nil {
		return err
	}
	repo = normalizeRepo(strings.TrimSpace(repo))
	if !m.HasRepo(repo) {
		return fmt.Errorf("no repo '%s' configured for '%s' (have: %v)", repo, name, m.Repos)
	}
	kept := m.Repos[:0]
	for _, r := range m.Repos {
		if r != repo {
			kept = append(kept, r)
		}
	}
	m.Repos = kept
	if err := m.Save(a.ConfigDir); err != nil {
		return err
	}
	// Leave the guest checkout in place — removing a repo from the conf is a
	// registry edit, not a destructive delete of the user's working tree.
	fmt.Fprintf(a.Stdout, "devvm: removed repo %s from '%s' (guest checkout left in place)\n", repo, name)
	return nil
}

func (a *App) runReposClone(name string) error {
	m, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	if len(m.Repos) == 0 {
		fmt.Fprintf(a.Stdout, "devvm: no repos configured; add one with 'devvm repos add %s owner/repo'\n", name)
		return nil
	}
	return a.cloneRepos(b, m.Repos)
}

// cloneRepos clones each spec into ~/src on the guest, skipping any that already
// exist. GitHub "owner/repo" shorthand goes through gh (login token, private
// repos); everything else (https://, ssh://, git@host:path) is a plain git
// clone so non-GitHub hosts work too. Runs in a login shell so gh (Homebrew) is
// on PATH.
func (a *App) cloneRepos(b backend.Backend, repos []string) error {
	// $1 is the untrusted repo spec; it's only ever a positional arg to git/gh,
	// never interpolated into the script text.
	const script = `set -e
mkdir -p "$HOME/src"; cd "$HOME/src"
repo="$1"
dir="${repo##*/}"; dir="${dir%.git}"
if [ -d "$dir" ]; then echo "devvm: $dir already cloned"; exit 0; fi
case "$repo" in
  *://*|*@*:*) exec git clone "$repo" ;;
  */*)         exec gh repo clone "$repo" ;;
  *)           echo "devvm: unrecognized repo '$repo'" >&2; exit 1 ;;
esac`
	// Clone every repo, warning-and-continuing past a failure so one bad spec (or
	// an already-in-flight gh auth hiccup) doesn't strand the repos after it. The
	// guest script already treats an existing checkout as success (exit 0), so a
	// failure here is a real one worth surfacing but not aborting on.
	var failed []string
	for _, repo := range repos {
		if err := b.Run(context.Background(), backend.ExecOpts{Login: true},
			"bash", "-lc", script, "_", repo); err != nil {
			fmt.Fprintf(a.Stderr, "devvm: failed to clone %s: %v (continuing)\n", repo, err)
			failed = append(failed, repo)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed to clone %d repo(s): %s", len(failed), strings.Join(failed, ", "))
	}
	return nil
}

// githubURLRe matches an https or scp-like github.com remote so we can fold it
// down to "owner/repo" shorthand; non-GitHub remotes are stored verbatim.
var githubURLRe = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([\w.-]+/[\w.-]+?)(?:\.git)?$`)

// normalizeRepo tidies a spec for storage: a github.com URL becomes the
// "owner/repo" shorthand (matching what gh prefers and what a bare add types);
// a bare "owner/repo" is kept; any other URL is returned unchanged.
func normalizeRepo(s string) string {
	s = strings.TrimSpace(s)
	if m := githubURLRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

// originRemote returns the current directory's git origin URL, or "" if we're
// not in a work tree. Used to prefill `repos add` from a host-side checkout.
func originRemote() string {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}
