package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/keys"
)

// Host-side authorized_keys management. The heavy logic (dedup/revoke/fingerprint)
// lives in internal/keys and runs on the host; the guest is touched only to read
// and atomically rewrite the file — so no guest agent is needed for keys, and an
// adopted host gets nothing installed. authorized_keys holds public keys only,
// so nothing secret crosses the wire.

// guestAuthKeys reads the connecting user's ~/.ssh/authorized_keys (empty if
// absent). The path is fixed (not user data), so there's no injection surface.
func (a *App) guestAuthKeys(b backend.Backend) ([]string, error) {
	var buf bytes.Buffer
	if err := b.Run(context.Background(), backend.ExecOpts{Stdout: &buf},
		"sh", "-c", "cat ~/.ssh/authorized_keys 2>/dev/null"); err != nil {
		return nil, fmt.Errorf("read authorized_keys: %w", err)
	}
	return keys.Split(buf.String()), nil
}

// guestOwnIDs returns the identities of the guest's own keys (~/.ssh/id_*.pub),
// which dedup strips and revoke protects. A missing glob is simply "none".
func (a *App) guestOwnIDs(b backend.Backend) map[string]bool {
	var buf bytes.Buffer
	_ = b.Run(context.Background(), backend.ExecOpts{Stdout: &buf},
		"sh", "-c", "cat ~/.ssh/id_*.pub 2>/dev/null")
	return keys.IDs(keys.Split(buf.String()))
}

// writeGuestAuthKeys atomically replaces ~/.ssh/authorized_keys. The content
// rides stdin (never the command line — comments/options can be attacker-shaped,
// e.g. --from-github), umask 077 fixes perms, and mv is an atomic same-dir swap
// so a dropped connection can't leave a half-written file (or lock you out).
func (a *App) writeGuestAuthKeys(b backend.Backend, lines []string) error {
	const script = "umask 077; mkdir -p ~/.ssh && cat > ~/.ssh/.authorized_keys.devvm && " +
		"mv ~/.ssh/.authorized_keys.devvm ~/.ssh/authorized_keys"
	return b.Run(context.Background(), backend.ExecOpts{Stdin: strings.NewReader(keys.Join(lines))},
		"sh", "-c", script)
}

// addGuestKeys merges incoming key lines into authorized_keys (dedup by material,
// dropping the guest's own keys). Shared by authorize-key and bootstrap seeding.
func (a *App) addGuestKeys(b backend.Backend, incoming []string) error {
	if len(incoming) == 0 {
		return nil
	}
	existing, err := a.guestAuthKeys(b)
	if err != nil {
		return err
	}
	kept, _, _ := keys.Dedup(append(existing, incoming...), a.guestOwnIDs(b))
	return a.writeGuestAuthKeys(b, kept)
}

// requireRemote gates commands that act on a guest's authorized_keys — allowed
// on both remote backends (managed and adopted), since key management is an
// explicit, user-scoped, non-root action, not OS shaping. smol has no ssh
// authorized_keys surface (it's reached via smolvm exec).
func requireRemote(m *config.Machine, cmd string) error {
	if !m.IsRemote() {
		return fmt.Errorf("'%s' only applies to remote machines ('%s' is %s)", cmd, m.Name, m.Backend)
	}
	return nil
}

func (a *App) runKeys(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if err := requireRemote(m, "keys list"); err != nil {
		return err
	}
	lines, err := a.guestAuthKeys(b)
	if err != nil {
		return err
	}
	summaries := keys.List(lines)
	if len(summaries) == 0 {
		fmt.Fprintln(os.Stdout, "(no authorized_keys)")
		return nil
	}
	for _, s := range summaries {
		fmt.Fprintln(os.Stdout, s)
	}
	return nil
}

func (a *App) runCleanupKeys(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if err := requireRemote(m, "keys dedupe"); err != nil {
		return err
	}
	lines, err := a.guestAuthKeys(b)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		fmt.Fprintln(os.Stdout, "devvm: no authorized_keys")
		return nil
	}
	kept, dup, own := keys.Dedup(lines, a.guestOwnIDs(b))
	if err := a.writeGuestAuthKeys(b, kept); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "devvm: removed %d duplicate and %d machine-owned key line(s)\n", dup, own)
	return nil
}

func (a *App) runRevokeKey(name, pattern string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if err := requireRemote(m, "keys rm"); err != nil {
		return err
	}
	lines, err := a.guestAuthKeys(b)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		fmt.Fprintln(os.Stdout, "devvm: no authorized_keys")
		return nil
	}
	// The host's own keys (id_*.pub + configured IDENTITY) are protected so you
	// can't cut the key you're connecting with.
	idx, summary, err := keys.Revoke(lines, pattern, keys.IDs(hostProtectedPubs(m)))
	if err != nil {
		return err
	}
	kept := append(lines[:idx:idx], lines[idx+1:]...)
	if err := a.writeGuestAuthKeys(b, kept); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "devvm: removed %s\n", summary)
	return nil
}

func (a *App) runAuthorizeKey(name string, spec []string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	if err := requireRemote(m, "keys add"); err != nil {
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
	if err := a.addGuestKeys(b, labeled); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "devvm: added %d key(s) to '%s'\n", len(labeled), name)
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
			return nil, fmt.Errorf("keys add --from-github needs a USER")
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

// githubUserRe matches a GitHub username (alphanumeric and hyphens). The user
// string lands in a URL path, so this also blocks path traversal into other
// github.com endpoints.
var githubUserRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

func githubKeys(user string) ([]string, error) {
	if !githubUserRe.MatchString(user) {
		return nil, fmt.Errorf("invalid GitHub user %q", user)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get("https://github.com/" + user + ".keys")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching keys for GitHub user %q: %s", user, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(body), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		// The endpoint returns only key lines; anything else (an error page,
		// say) must never be appended to authorized_keys.
		if _, ok := keys.Parse(l); !ok {
			return nil, fmt.Errorf("unexpected non-key line from github.com/%s.keys: %q", user, l)
		}
		out = append(out, l)
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

// labelKeys ensures every key carries a comment: already-commented keys pass
// through verbatim (options and all), and uncommented ones prompt (defaulting
// to the local hostname), rejecting a bare email, since GitHub's .keys endpoint
// drops titles. The TTY is only opened if a prompt is actually needed, so a
// commented key works non-interactively.
func labelKeys(lines []string) ([]string, error) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	var tty *os.File
	var r *bufio.Reader
	var out []string
	for _, line := range lines {
		k, ok := keys.Parse(line)
		if !ok {
			return nil, fmt.Errorf("invalid public key: %s", line)
		}
		if k.Comment != "" {
			out = append(out, k.Line)
			continue
		}
		if tty == nil {
			tty, err = os.OpenFile("/dev/tty", os.O_RDWR, 0)
			if err != nil {
				return nil, fmt.Errorf("no terminal for key comments; pass an explicitly commented key")
			}
			defer tty.Close()
			r = bufio.NewReader(tty)
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
		// Prefix keeps any options on the line, not just type+blob.
		out = append(out, k.Prefix+" "+comment)
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
