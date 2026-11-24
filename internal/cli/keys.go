package cli

import (
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

// Host-side authorized_keys management. The heavy logic (merge/revoke/fingerprint)
// lives in internal/keys and runs on the host; the guest is touched only to read
// and atomically rewrite the file — so no guest agent is needed for keys, and an
// entry gets nothing installed. authorized_keys holds public keys only,
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

// writeGuestAuthKeys atomically replaces ~/.ssh/authorized_keys. The content
// rides stdin (never the command line — comments/options can be attacker-shaped,
// e.g. github:USER), umask 077 fixes perms, and mv is an atomic same-dir swap
// so a dropped connection can't leave a half-written file (or lock you out).
func (a *App) writeGuestAuthKeys(b backend.Backend, lines []string) error {
	const script = "umask 077; mkdir -p ~/.ssh && cat > ~/.ssh/.authorized_keys.devvm && " +
		"mv ~/.ssh/.authorized_keys.devvm ~/.ssh/authorized_keys"
	return b.Run(context.Background(), backend.ExecOpts{Stdin: strings.NewReader(keys.Join(lines))},
		"sh", "-c", script)
}

// addGuestKeys merges incoming key lines into authorized_keys (dedup by
// material). It drops none of the guest's own keys (keys.Merge says why), so
// it needs no read of ~/.ssh/id_*.pub. Shared by `keys add` and setup
// seeding. It returns how many keys were new to the file, so `keys add`
// does not claim to have added a key that was already there.
// The read-modify-write isn't locked: two concurrent invocations (from different
// hosts, say) can lose one side's update — the atomic mv prevents corruption,
// not lost writes. Re-running is always safe.
func (a *App) addGuestKeys(b backend.Backend, incoming []string) (int, error) {
	if len(incoming) == 0 {
		return 0, nil
	}
	existing, err := a.guestAuthKeys(b)
	if err != nil {
		return 0, err
	}
	have := keys.IDs(existing)
	added := 0
	for id := range keys.IDs(incoming) {
		if !have[id] {
			added++
		}
	}
	return added, a.writeGuestAuthKeys(b, keys.Merge(existing, incoming))
}

// requireRemote gates commands that act on a guest's authorized_keys — allowed
// on every box reached over ssh (entries and provider servers), since key management is an
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
		fmt.Fprintln(a.Stdout, "(no authorized_keys)")
		return nil
	}
	for _, s := range summaries {
		fmt.Fprintln(a.Stdout, s)
	}
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
		fmt.Fprintln(a.Stdout, "devvm: no authorized_keys")
		return nil
	}
	// The host's own keys (hostProtectedPubs) are protected so you can't
	// cut the key you're connecting with.
	idx, summary, err := keys.Revoke(lines, pattern, keys.IDs(a.hostProtectedPubs(m)))
	if err != nil {
		return err
	}
	kept := append(lines[:idx:idx], lines[idx+1:]...)
	if err := a.writeGuestAuthKeys(b, kept); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "devvm: removed %s\n", summary)
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
	lines, err := hostKeyLines(spec)
	if err != nil {
		return err
	}
	added, err := a.addGuestKeys(b, lines)
	if err != nil {
		return err
	}
	if dup := len(keys.IDs(lines)) - added; dup > 0 {
		fmt.Fprintf(a.Stdout, "devvm: added %d key(s) to '%s' (%d already there)\n", added, name, dup)
		return nil
	}
	fmt.Fprintf(a.Stdout, "devvm: added %d key(s) to '%s'\n", added, name)
	return nil
}

// hostKeyLines resolves a `keys add` spec on this host into commented key
// lines, for the local leaf and for HUB/NAME alike (proxy sends the lines),
// so a key file or the bare form always means the keys of the host it was
// typed on.
func hostKeyLines(spec []string) ([]string, error) {
	lines, err := resolvePubkeys(spec)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("no ~/.ssh/id_*.pub on this host; pass github:USER, a key file or a key")
	}
	return labelKeys(lines)
}

// githubSpec is the `keys add` prefix that fetches a GitHub user's keys
// (github:USER): a spec, not a flag, so the leaf parses flags like any
// other and the prefix can never be read as a path or a key.
const githubSpec = "github:"

// resolvePubkeys turns a key spec into public-key lines: github:USER, an
// inline key (its words joined), a file path, or (empty) all local id_*.pub.
func resolvePubkeys(spec []string) ([]string, error) {
	switch {
	case len(spec) == 0:
		return localPubkeys(), nil
	case strings.HasPrefix(spec[0], githubSpec):
		if len(spec) > 1 {
			return nil, fmt.Errorf("keys add takes one github:USER, file or key, not %q too", strings.Join(spec[1:], " "))
		}
		return githubKeys(strings.TrimPrefix(spec[0], githubSpec))
	case isInlineKey(spec[0]):
		return []string{strings.Join(spec, " ")}, nil
	default:
		if len(spec) > 1 {
			return nil, fmt.Errorf("keys add takes one github:USER, file or key, not %q too", strings.Join(spec[1:], " "))
		}
		data, err := os.ReadFile(spec[0])
		if err != nil {
			return nil, fmt.Errorf("not github:USER, a key or a public-key file: %s", spec[0])
		}
		// Blank and # lines are skipped: an authorized_keys-shaped file
		// may carry them, and only key lines are added.
		var out []string
		for _, l := range strings.Split(string(data), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				out = append(out, l)
			}
		}
		return out, nil
	}
}

// isInlineKey reports whether s starts an authorized_keys line: a key type
// first, or options then a key type (`no-pty ssh-ed25519 AAAA… c`) — the
// hub proxy sends whole lines read from a file, options included, as one
// token. A path or a GitHub user never has a key type in either slot.
func isInlineKey(s string) bool {
	fields := strings.Fields(s)
	for i := 0; i < len(fields) && i < 2; i++ {
		for _, p := range []string{"ssh-", "ecdsa-", "sk-"} {
			if strings.HasPrefix(fields[i], p) {
				return true
			}
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

// githubBase is where github:USER's keys come from, a var so a test can
// serve them.
var githubBase = "https://github.com"

// githubUserRe matches a GitHub username (alphanumeric and hyphens). The user
// string lands in a URL path, so this also blocks path traversal into other
// github.com endpoints.
var githubUserRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

func githubKeys(user string) ([]string, error) {
	if !githubUserRe.MatchString(user) {
		return nil, fmt.Errorf("invalid GitHub user %q", user)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(githubBase + "/" + user + ".keys")
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
		// The endpoint drops each key's title, and the API gives titles
		// only to a token with admin:public_key, so the comment says
		// where the key came from; `keys list` prints the fingerprint
		// GitHub's settings page shows beside each title. A copy already
		// in authorized_keys keeps its own comment (keys.Merge).
		out = append(out, l+" "+user+"@github")
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no public keys for GitHub user %q", user)
	}
	return out, nil
}

// hostProtectedPubs returns the host's own public keys (id_*.pub +
// IDENTITY.pub, and on a provider server its table's ssh_key_file, the key
// create gave it, which need not be named id_*), whose identities the guest
// must never revoke: on a hardened box that is a lockout. A file that is
// missing or does not parse adds nothing; rm still runs.
func (a *App) hostProtectedPubs(m *config.Machine) []string {
	pubs := localPubkeys()
	if m.Identity != "" {
		p := config.ExpandHome(m.Identity) + ".pub"
		if data, err := os.ReadFile(p); err == nil {
			pubs = append(pubs, strings.TrimSpace(string(data)))
		}
	}
	if m.IsProvider() {
		// A provider record's backend is its [providers.NAME] table.
		if f, err := config.LoadFile(a.Paths); err == nil {
			if spec := f.Providers[m.Backend]; spec.SSHKeyFile != "" {
				if line, err := providerKeyFile(m.Backend, config.Placement{SSHKeyFile: spec.SSHKeyFile}); err == nil {
					pubs = append(pubs, line)
				}
			}
		}
	}
	return pubs
}

// labelKeys gives every key a comment, so `keys list` and `keys rm` can
// name it: a commented key passes through verbatim, and an uncommented one
// gets this host's short hostname, the host it was added from. Options on
// the line are kept. A line that is not a key is refused, never written.
func labelKeys(lines []string) ([]string, error) {
	host := shortHostname()
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		k, ok := keys.Parse(line)
		if !ok {
			return nil, fmt.Errorf("invalid public key: %s", line)
		}
		if k.Comment != "" {
			out = append(out, k.Line)
			continue
		}
		// Prefix keeps any options on the line, not just type+blob.
		out = append(out, k.Prefix+" "+host)
	}
	return out, nil
}

// shortHostname is this host's name up to the first dot (macOS reports
// NAME.local), or "unknown".
func shortHostname() string {
	host, err := hostname()
	if err != nil || host == "" {
		return "unknown"
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	return host
}
