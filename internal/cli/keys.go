package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/keys"
)

func requireSSH(m *config.Machine, cmd string) error {
	if m.Backend != config.BackendSSH {
		return fmt.Errorf("'%s' only applies to ssh machines ('%s' is %s)", cmd, m.Name, m.Backend)
	}
	return nil
}

func (a *App) runKeys(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if err := requireSSH(m, "keys"); err != nil {
		return err
	}
	return a.agentRun(b, nil, os.Stdout, "keys", "list")
}

func (a *App) runCleanupKeys(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if err := requireSSH(m, "cleanup-keys"); err != nil {
		return err
	}
	return a.agentRun(b, nil, os.Stdout, "keys", "cleanup")
}

func (a *App) runRevokeKey(name, pattern string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if err := requireSSH(m, "revoke-key"); err != nil {
		return err
	}
	// The host's own keys (id_*.pub + configured IDENTITY) are protected so you
	// can't cut the key you're connecting with.
	protected := strings.Join(hostProtectedPubs(m), "\n") + "\n"
	return a.agentRun(b, strings.NewReader(protected), os.Stdout, "keys", "revoke", pattern)
}

func (a *App) runAuthorizeKey(name string, spec []string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if err := requireSSH(m, "authorize-key"); err != nil {
		return err
	}
	if len(spec) == 0 {
		user, err := promptTTY("GitHub user ID: ")
		if err != nil {
			return err
		}
		if user == "" {
			return fmt.Errorf("GitHub user ID is required")
		}
		spec = []string{"--from-github", user}
	}
	resolved, err := resolvePubkeys(spec)
	if err != nil {
		return err
	}
	labeled, err := labelKeys(resolved)
	if err != nil {
		return err
	}
	stdin := strings.Join(labeled, "\n") + "\n"
	return a.agentRun(b, strings.NewReader(stdin), os.Stdout, "keys", "add")
}

func (a *App) runRepos(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if len(m.Repos) == 0 {
		fmt.Printf("No repositories configured for '%s'.\n", name)
		fmt.Printf("Add repos = [\"owner/repo\", ...] to the machine conf.\n")
		return nil
	}
	// Clone via a login shell so gh (Homebrew) is on PATH and uses the login token.
	script := `mkdir -p "$HOME/src"; cd "$HOME/src"; [ -d "${1##*/}" ] || gh repo clone "$1"`
	for _, repo := range m.Repos {
		if err := b.Run(context.Background(), backend.ExecOpts{Login: true},
			"bash", "-lc", script, "_", repo); err != nil {
			return err
		}
	}
	return nil
}

// resolvePubkeys turns a key spec into public-key lines, mirroring resolve_pubkeys:
// --from-github USER, a bare inline key, a file path, or (empty) all local id_*.pub.
func resolvePubkeys(spec []string) ([]string, error) {
	switch {
	case len(spec) == 0:
		return localPubkeys(), nil
	case spec[0] == "--from-github":
		if len(spec) < 2 || spec[1] == "" {
			return nil, fmt.Errorf("authorize-key --from-github needs a USER")
		}
		return githubKeys(spec[1])
	case isInlineKey(spec[0]):
		return []string{strings.Join(spec, " ")}, nil
	default:
		data, err := os.ReadFile(spec[0])
		if err != nil {
			return nil, fmt.Errorf("not a public-key file: %s", spec[0])
		}
		var out []string
		for _, l := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(l) != "" {
				out = append(out, strings.TrimSpace(l))
			}
		}
		return out, nil
	}
}

func isInlineKey(s string) bool {
	for _, p := range []string{"ssh-", "ecdsa-", "sk-"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func localPubkeys() []string {
	home, _ := os.UserHomeDir()
	matches, _ := filepath.Glob(filepath.Join(home, ".ssh", "id_*.pub"))
	var out []string
	for _, m := range matches {
		if data, err := os.ReadFile(m); err == nil {
			out = append(out, strings.TrimSpace(string(data)))
		}
	}
	return out
}

func githubKeys(user string) ([]string, error) {
	resp, err := http.Get("https://github.com/" + user + ".keys")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimSpace(l))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no public keys for GitHub user %q", user)
	}
	return out, nil
}

// expandHome expands a leading ~/ to the user's home directory.
func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// hostProtectedPubs returns the host's own public keys (id_*.pub + IDENTITY.pub),
// whose identities the guest must never revoke.
func hostProtectedPubs(m *config.Machine) []string {
	pubs := localPubkeys()
	if m.Identity != "" {
		p := expandHome(m.Identity) + ".pub"
		if data, err := os.ReadFile(p); err == nil {
			pubs = append(pubs, strings.TrimSpace(string(data)))
		}
	}
	return pubs
}

var bareEmailRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+$`)

// labelKeys prompts for a comment on each key (defaulting to the local
// hostname), rejecting a bare email, since GitHub's .keys endpoint drops titles.
func labelKeys(lines []string) ([]string, error) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("no terminal for key comments; pass an explicitly commented key")
	}
	defer tty.Close()
	r := bufio.NewReader(tty)
	var out []string
	for _, line := range lines {
		k, ok := keys.Parse(line)
		if !ok {
			return nil, fmt.Errorf("invalid public key: %s", line)
		}
		var comment string
		for {
			fmt.Fprintf(tty, "Comment for %s key [%s]: ", k.Type, host)
			text, _ := r.ReadString('\n')
			comment = strings.TrimSpace(text)
			if comment == "" {
				comment = host
			}
			if bareEmailRe.MatchString(comment) {
				fmt.Fprintln(tty, "devvm: use a hostname or other descriptive label, not just an email address")
				continue
			}
			break
		}
		out = append(out, k.Type+" "+k.Blob+" "+comment)
	}
	return out, nil
}

// promptTTY reads a single line from the controlling terminal.
func promptTTY(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("no terminal; use --from-github USER")
	}
	defer tty.Close()
	fmt.Fprint(tty, prompt)
	line, _ := bufio.NewReader(tty).ReadString('\n')
	return strings.TrimSpace(line), nil
}
