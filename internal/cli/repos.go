package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/smweber/devvm/internal/backend"
)

// ownerRepoRe matches the "owner/repo" GitHub shorthand: no scheme, no host,
// exactly one slash. Anything with a scheme (https://, ssh://) or an scp-like
// "user@host:path" is treated as a full URL and cloned with plain git.
var ownerRepoRe = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)

// runReposList lists the git checkouts in the guest's ~/src (config.md §3):
// repos are not tracked anywhere, so the guest is the only one that knows.
// One exec, so it needs a running box.
func (a *App) runReposList(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	// An exec would boot a stopped smol VM, behind any daemon it has.
	if !m.IsRemote() {
		if st, err := b.Status(); err == nil && !st.Running {
			return fmt.Errorf("'%s' is stopped; 'devvm start %s' first (repos list reads the guest's ~/src)", name, name)
		}
	}
	const script = `cd "$HOME/src" 2>/dev/null || exit 0
for d in */; do
  d="${d%/}"
  [ -e "$d/.git" ] || continue
  url=$(git -C "$d" remote get-url origin 2>/dev/null)
  printf '%s\t%s\n' "$d" "$url"
done`
	var out bytes.Buffer
	if err := b.Run(context.Background(), backend.ExecOpts{Login: true, Stdout: &out}, "bash", "-c", script); err != nil {
		return err
	}
	if strings.TrimSpace(out.String()) == "" {
		fmt.Fprintf(a.Stdout, "devvm: no git checkouts in ~/src; clone one with 'devvm repos add %s owner/repo'\n", name)
		return nil
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		dir, url, _ := strings.Cut(line, "\t")
		if url == "" {
			fmt.Fprintf(a.Stdout, "  %s\n", dir)
		} else {
			fmt.Fprintf(a.Stdout, "  %-24s %s\n", dir, normalizeRepo(url))
		}
	}
	return nil
}

// runReposAdd clones repo spec(s) into ~/src on the guest; nothing is
// recorded (config.md §3). With no repo arg it prompts, prefilling from the
// current directory's git origin so a host-side checkout is one keystroke
// to add.
func (a *App) runReposAdd(name string, repos []string) error {
	_, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		in, err := promptRepo()
		if err != nil {
			return err
		}
		repos = []string{in}
	}
	var specs []string
	for _, r := range repos {
		if r = normalizeRepo(strings.TrimSpace(r)); r != "" && !slices.Contains(specs, r) {
			specs = append(specs, r)
		}
	}
	if len(specs) == 0 {
		return fmt.Errorf("no repo given")
	}
	return a.cloneRepos(b, specs)
}

// promptRepo asks for one repo on the terminal, prefilling the current
// directory's git origin. Shared by the local leaf and the hub proxy.
func promptRepo() (string, error) {
	def := normalizeRepo(originRemote())
	prompt := "Repo (owner/repo or URL): "
	if def != "" {
		prompt = fmt.Sprintf("Repo (owner/repo or URL) [%s]: ", def)
	}
	in, err := promptTTY(prompt)
	if err != nil {
		return "", err
	}
	if in == "" {
		in = def
	}
	if in == "" {
		return "", fmt.Errorf("a repo is required")
	}
	return in, nil
}

// cloneRepos clones each spec into ~/src on the guest, skipping any that already
// exist. GitHub "owner/repo" shorthand goes through gh (login token, private
// repos); everything else (https://, ssh://, git@host:path) is a plain git
// clone so non-GitHub hosts work too. Runs in a login shell so gh (Homebrew) is
// on PATH. When jj is installed on the guest, a fresh checkout is initialized as
// a jj colocated repo — it stays a normal git repo, just jj-ready.
func (a *App) cloneRepos(b backend.Backend, repos []string) error {
	// $1 is the untrusted repo spec; it's only ever a positional arg to git/gh,
	// never interpolated into the script text.
	const script = `set -e
mkdir -p "$HOME/src"; cd "$HOME/src"
repo="$1"
dir="${repo##*/}"; dir="${dir%.git}"
if [ -d "$dir" ]; then echo "devvm: $dir already cloned"; exit 0; fi
case "$repo" in
  *://*|*@*:*) git clone "$repo" ;;
  */*)         gh repo clone "$repo" ;;
  *)           echo "devvm: unrecognized repo '$repo'" >&2; exit 1 ;;
esac
# jj-colocate the fresh checkout when jj is on the guest, so it's jj-ready while
# staying a normal git repo. Best-effort: never fail the clone over it.
if command -v jj >/dev/null 2>&1 && [ -d "$dir/.git" ] && [ ! -d "$dir/.jj" ]; then
  if ( cd "$dir" && jj git init --colocate >/dev/null 2>&1 ); then
    echo "devvm: initialized jj (colocated) in $dir"
  else
    echo "devvm: jj colocate skipped for $dir" >&2
  fi
fi`
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
// down to "owner/repo" shorthand; non-GitHub remotes are kept verbatim.
var githubURLRe = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([\w.-]+/[\w.-]+?)(?:\.git)?$`)

// normalizeRepo tidies a spec: a github.com URL becomes the
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
