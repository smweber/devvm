// Package keys is the authorized_keys logic that replaces the ~50-line nested
// awk programs in bin/devvm (push/cleanup/revoke/list). It is pure and text-only
// so it can be unit-tested against the old behavior; the guest agent supplies
// the filesystem glue (reading authorized_keys and the machine's own id_*.pub).
package keys

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

// keyPrefixes are the token prefixes that mark the start of a key in a line
// (matching the awk's /^(ssh-|ecdsa-|sk-)/).
var keyPrefixes = []string{"ssh-", "ecdsa-", "sk-"}

// Key is a parsed authorized_keys entry. Identity is Type+Blob; the comment is
// not part of identity (dedup is by key material).
type Key struct {
	Line    string // the original line, verbatim
	Type    string // e.g. ssh-ed25519
	Blob    string // base64 key material
	Comment string // trailing comment (may be empty)
	Prefix  string // options...+type+blob, i.e. Line minus the comment
}

func isKeyToken(s string) bool {
	for _, p := range keyPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Parse extracts the key from a line (which may carry leading options), or
// reports ok=false for blank/comment/keyless lines.
func Parse(line string) (Key, bool) {
	if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
		return Key{}, false
	}
	fields := strings.Fields(line)
	for i := 0; i+1 < len(fields); i++ {
		if isKeyToken(fields[i]) {
			return Key{
				Line:    line,
				Type:    fields[i],
				Blob:    fields[i+1],
				Comment: strings.Join(fields[i+2:], " "),
				Prefix:  strings.Join(fields[:i+2], " "),
			}, true
		}
	}
	return Key{}, false
}

// ID is a key's identity for dedup/protection (type + material, not comment).
func (k Key) ID() string { return k.Type + " " + k.Blob }

// Fingerprint returns the SHA256 fingerprint in ssh-keygen's format
// (SHA256:<base64-no-pad>), computed in-process so no ssh-keygen is needed.
func (k Key) Fingerprint() string {
	raw, err := base64.StdEncoding.DecodeString(k.Blob)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// TypeName is the parenthesised algorithm name ssh-keygen shows (best-effort).
func (k Key) TypeName() string {
	switch {
	case k.Type == "ssh-ed25519" || strings.HasPrefix(k.Type, "sk-ssh-ed25519"):
		return "ED25519"
	case k.Type == "ssh-rsa":
		return "RSA"
	case strings.HasPrefix(k.Type, "ecdsa-") || strings.HasPrefix(k.Type, "sk-ecdsa-"):
		return "ECDSA"
	default:
		return strings.ToUpper(k.Type)
	}
}

// Summary is a human line for `keys` and for revoke pattern matching, close to
// `ssh-keygen -l` output: "SHA256:... <comment> (TYPE)".
func (k Key) Summary() string {
	comment := k.Comment
	if comment == "" {
		comment = "no comment"
	}
	return fmt.Sprintf("%s %s (%s)", k.Fingerprint(), comment, k.TypeName())
}

// Dedup removes keys whose identity is in ownIDs (the machine's own keys) and
// collapses duplicates by key material, keeping the first occurrence. If the
// first copy has no comment but a later duplicate does, the later comment is
// adopted onto the first line. Non-key lines (comments/blanks) are preserved.
// Returns the rewritten lines and how many duplicate / own lines were dropped.
func Dedup(lines []string, ownIDs map[string]bool) (kept []string, removedDup, removedOwn int) {
	type firstRef struct {
		idx     int
		comment string
		prefix  string
	}
	out := make([]string, 0, len(lines))
	first := map[string]*firstRef{}
	for _, line := range lines {
		k, ok := Parse(line)
		if !ok {
			out = append(out, line)
			continue
		}
		id := k.ID()
		if ownIDs[id] {
			removedOwn++
			continue
		}
		if ref, seen := first[id]; seen {
			removedDup++
			if ref.comment == "" && k.Comment != "" {
				out[ref.idx] = ref.prefix + " " + k.Comment
				ref.comment = k.Comment
			}
			continue
		}
		out = append(out, line)
		first[id] = &firstRef{idx: len(out) - 1, comment: k.Comment, prefix: k.Prefix}
	}
	return out, removedDup, removedOwn
}

// ErrNoMatch / ErrAmbiguous / ErrProtected classify Revoke failures.
var (
	ErrNoMatch  = fmt.Errorf("no key matches")
	ErrProtect  = fmt.Errorf("refusing to revoke a key belonging to this host")
	ErrMultiple = fmt.Errorf("pattern is ambiguous")
)

// Revoke locates the single key matching pattern (by raw line or by Summary,
// covering fingerprint/comment/substring) and returns its line index. It errors
// if nothing matches, if the match is ambiguous, or if the match is a protected
// (host-owned) key. Mirrors revoke_key's safety checks.
func Revoke(lines []string, pattern string, protected map[string]bool) (idx int, summary string, err error) {
	var (
		idxs []int
		sums []string
		ids  []string
	)
	for i, line := range lines {
		k, ok := Parse(line)
		if !ok {
			continue
		}
		s := k.Summary()
		if strings.Contains(line, pattern) || strings.Contains(s, pattern) {
			idxs = append(idxs, i)
			sums = append(sums, s)
			ids = append(ids, k.ID())
		}
	}
	switch len(idxs) {
	case 0:
		return -1, "", fmt.Errorf("%w: %s", ErrNoMatch, pattern)
	case 1:
		if protected[ids[0]] {
			return -1, "", fmt.Errorf("%w: %s", ErrProtect, sums[0])
		}
		return idxs[0], sums[0], nil
	default:
		return -1, "", fmt.Errorf("%w (%s); matches:\n  %s",
			ErrMultiple, pattern, strings.Join(sums, "\n  "))
	}
}

// List returns a Summary per key line (skipping comments/blanks).
func List(lines []string) []string {
	var out []string
	for _, line := range lines {
		if k, ok := Parse(line); ok {
			out = append(out, k.Summary())
		}
	}
	return out
}

// Split breaks authorized_keys file content into lines, dropping a trailing
// empty line from the final newline.
func Split(content string) []string {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

// Join renders lines back to file content (trailing newline).
func Join(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// IDs returns the identity set for a list of public-key lines (e.g. id_*.pub),
// used to build ownIDs / protected sets.
func IDs(pubLines []string) map[string]bool {
	m := map[string]bool{}
	for _, l := range pubLines {
		if k, ok := Parse(l); ok {
			m[k.ID()] = true
		}
	}
	return m
}
