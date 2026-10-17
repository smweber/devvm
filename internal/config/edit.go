package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The only two writes devvm ever makes to devvm.toml (config.md §4): `config
// add` appends one whole `[machines.NAME]` table at the end, `config rm`
// deletes one table's line range. Neither re-encodes the file, so the
// user's comments, ordering and layout everywhere else survive byte for
// byte; everything else (templates, transport, keys) is `config edit`'s.
// Both re-parse the result and check that exactly the intended entry
// changed before an atomic rename, so a scanner fooled by an odd layout (a
// header-like line inside a multi-line string) refuses instead of
// corrupting the file.

// ErrBrokenFile wraps a parse failure of the file an edit starts from:
// the structural edits refuse to touch a file they cannot read.
var ErrBrokenFile = errors.New("devvm.toml does not parse; fix it with 'devvm config edit' first")

// AppendEntry adds `[machines.NAME]` at the end of devvm.toml, creating the
// file if it is missing. It refuses a name the file already has. backend is
// always written, and managed too on a remote entry, so the file states the
// posture rather than leaving it to a default.
func AppendEntry(p Paths, name string, e Entry) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if e.Backend == BackendRemote && e.Managed == nil {
		f := false
		e.Managed = &f
	}
	path, data, err := readForEdit(p)
	if err != nil {
		return err
	}
	before, err := ParseFile(path, data)
	if err != nil {
		return fmt.Errorf("%w:\n%v", ErrBrokenFile, err)
	}
	if _, ok := before.Entries[name]; ok {
		return fmt.Errorf("%s already has %s", path, MachineTable(name))
	}
	// Match the file's line ending: a file saved with CRLF (an editor on
	// Windows, a git checkout with autocrlf) stays CRLF throughout.
	nl := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		nl = "\r\n"
	}
	var b bytes.Buffer
	b.Write(data)
	if len(data) > 0 {
		if data[len(data)-1] != '\n' {
			b.WriteString(nl)
		}
		b.WriteString(nl)
	}
	b.WriteString(strings.ReplaceAll(renderEntry(name, e), "\n", nl))
	after, err := ParseFile(path, b.Bytes())
	if err != nil {
		return err // the new table itself is invalid (a bad host, say)
	}
	if !sameExcept(before, after, name) {
		return fmt.Errorf("appending %s would change other tables in %s; edit it with 'devvm config edit'", MachineTable(name), path)
	}
	if _, ok := after.Entries[name]; !ok {
		return fmt.Errorf("appending %s to %s did not add it; edit it with 'devvm config edit'", MachineTable(name), path)
	}
	return writeConfigFile(path, b.Bytes())
}

// RemoveEntry deletes the `[machines.NAME]` table: its header line through
// its last key, the comment lines glued below that, and the blank gap
// before whatever follows (the next header, or EOF). Conservatively kept:
// a comment block right above the next header (it describes that table), a
// comment after a blank line (not clearly this table's), and a comment
// above NAME's own header. ErrNotFound when the file has no such entry.
func RemoveEntry(p Paths, name string) error {
	e, err := PlanRemoveEntry(p, name)
	if err != nil {
		return err
	}
	return e.Commit()
}

// Edit is a planned write of devvm.toml: the new content, checked, not yet
// written. `config rm` plans first and commits only after it has stopped
// the entry's daemons, so a layout the cut refuses never costs a daemon.
type Edit struct {
	path       string
	from, data []byte
}

// PlanRemoveEntry is RemoveEntry up to, not including, the write: every
// refusal (no such entry, a broken file, a form the line cut cannot remove,
// a cut that would change another table) happens here.
func PlanRemoveEntry(p Paths, name string) (*Edit, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	path, data, err := readForEdit(p)
	if err != nil {
		return nil, err
	}
	before, err := ParseFile(path, data)
	if err != nil {
		return nil, fmt.Errorf("%w:\n%v", ErrBrokenFile, err)
	}
	if _, ok := before.Entries[name]; !ok {
		return nil, fmt.Errorf("%w: %s in %s", ErrNotFound, MachineTable(name), path)
	}
	out, err := cutTable(data, name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w; remove it with 'devvm config edit'", path, err)
	}
	after, err := ParseFile(path, out)
	if err != nil || !sameExcept(before, after, name) {
		return nil, fmt.Errorf("%s: removing %s would change other tables; remove it with 'devvm config edit'", path, MachineTable(name))
	}
	if _, ok := after.Entries[name]; ok {
		return nil, fmt.Errorf("%s: %s is defined in a form this edit cannot remove; remove it with 'devvm config edit'", path, MachineTable(name))
	}
	return &Edit{path: path, from: data, data: out}, nil
}

// Commit writes a planned edit, unless the file changed since it was
// planned (an editor saved meanwhile): then nothing is written.
func (e *Edit) Commit() error {
	cur, err := os.ReadFile(e.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !bytes.Equal(cur, e.from) {
		return fmt.Errorf("%s changed while it was being edited; nothing was written, run the command again", e.path)
	}
	return writeConfigFile(e.path, e.data)
}

// readForEdit returns the file to edit, through a symlink: a devvm.toml
// that is a link into a dotfiles checkout must stay one, so the edit goes
// to the link's target, renamed in its own directory. A missing file reads
// as empty; a dangling link is an error (an edit would rename a regular
// file over the link).
func readForEdit(p Paths) (string, []byte, error) {
	path, err := ResolveFile(p)
	if err != nil {
		return "", nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil, nil
	}
	return path, data, err
}

// ResolveFile is devvm.toml's real path: the file itself, or a symlink's
// target. A link whose target does not exist (a dotfiles checkout not
// cloned yet, a stow run half done) is an error naming both, not a missing
// file: reading it as empty would hide every entry, and writing it would
// replace the link.
func ResolveFile(p Paths) (string, error) {
	path := p.FilePath()
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		dest, _ := os.Readlink(path)
		return "", fmt.Errorf("%s is a symlink to %s, which does not resolve: %w", path, dest, err)
	}
	return target, nil
}

func writeConfigFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o644)
}

// sameExcept reports whether two parses differ only in the entry name.
func sameExcept(a, b *File, name string) bool {
	if !reflect.DeepEqual(a.Templates, b.Templates) {
		return false
	}
	for n, e := range a.Entries {
		if n == name {
			continue
		}
		if o, ok := b.Entries[n]; !ok || !reflect.DeepEqual(e, o) {
			return false
		}
	}
	for n := range b.Entries {
		if _, ok := a.Entries[n]; !ok && n != name {
			return false
		}
	}
	return true
}

// renderEntry is one table, in a fixed key order.
func renderEntry(name string, e Entry) string {
	var b strings.Builder
	b.WriteString(MachineTable(name) + "\n")
	fmt.Fprintf(&b, "backend = %s\n", tomlString(e.Backend))
	if e.Managed != nil {
		fmt.Fprintf(&b, "managed = %t\n", *e.Managed)
	}
	fmt.Fprintf(&b, "host = %s\n", tomlString(e.Host))
	for _, kv := range []struct{ k, v string }{
		{"user", e.User}, {"transport", e.Transport},
		{"mosh_server", e.MoshServer}, {"identity", e.Identity},
	} {
		if kv.v != "" {
			fmt.Fprintf(&b, "%s = %s\n", kv.k, tomlString(kv.v))
		}
	}
	if e.ConnectTimeout != 0 {
		fmt.Fprintf(&b, "connect_timeout = %d\n", e.ConnectTimeout)
	}
	return b.String()
}

// MachineTable is the `[machines.NAME]` header for an entry name, quoted
// as `config add` writes it: a name with a '.' (a hostname like mac.local)
// would otherwise read back as a nested table.
func MachineTable(name string) string {
	return "[machines." + tomlKey(name) + "]"
}

var bareKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// tomlKey quotes a table-name segment that is not a bare key (a machine
// name may contain '.', which would otherwise nest).
func tomlKey(k string) string {
	if bareKeyRe.MatchString(k) {
		return k
	}
	return tomlString(k)
}

// tomlString is a TOML basic string. Go's %q is not one (it emits \x and \a
// escapes TOML lacks), so escape by hand: quote, backslash, and controls.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f || r == utf8.RuneError:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// cutTable returns data without the `[machines.NAME]` table (see
// RemoveEntry for the range).
func cutTable(data []byte, name string) ([]byte, error) {
	lines := strings.SplitAfter(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	start, end := -1, len(lines)
	for i, line := range lines {
		key, ok := tableHeader(line)
		if !ok {
			continue
		}
		if start < 0 {
			if len(key) == 2 && key[0] == "machines" && key[1] == name {
				start = i
			}
			continue
		}
		end = i
		break
	}
	if start < 0 {
		return nil, fmt.Errorf("no %s header line", MachineTable(name))
	}
	// The table's own content ends at its last line that is neither blank
	// nor a comment. After it: comment lines glued to it (no blank between)
	// are its own, then the blank gap. A comment block right above the next
	// header describes that table, and a comment after a blank line is not
	// clearly anyone's: both are kept.
	content := start + 1
	for i := start + 1; i < end; i++ {
		if !isBlank(lines[i]) && !isComment(lines[i]) {
			content = i + 1
		}
	}
	keepFrom := end
	if end < len(lines) {
		for keepFrom > content && isComment(lines[keepFrom-1]) {
			keepFrom--
		}
	}
	cut := content
	for cut < keepFrom && isComment(lines[cut]) {
		cut++
	}
	for cut < keepFrom && isBlank(lines[cut]) {
		cut++
	}
	from := start
	if cut == len(lines) {
		// The table ran to EOF: drop the blank separator above it too, so
		// the file does not end in an empty line.
		for from > 0 && strings.TrimSpace(lines[from-1]) == "" {
			from--
		}
	}
	out := strings.Join(lines[:from], "") + strings.Join(lines[cut:], "")
	return []byte(out), nil
}

func isBlank(line string) bool   { return strings.TrimSpace(line) == "" }
func isComment(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "#") }

// tableHeader parses a `[a.b]` or `[[a.b]]` header line into its key
// segments. ok is false for anything else (a key/value, a comment, a
// blank line, an array value that happens to start with '[').
func tableHeader(line string) (key []string, ok bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") {
		return nil, false
	}
	inner, rest := "", ""
	if strings.HasPrefix(t, "[[") {
		i := strings.Index(t, "]]")
		if i < 0 {
			return nil, false
		}
		inner, rest = t[2:i], t[i+2:]
	} else {
		i := closingBracket(t)
		if i < 0 {
			return nil, false
		}
		inner, rest = t[1:i], t[i+1:]
	}
	if rest = strings.TrimSpace(rest); rest != "" && !strings.HasPrefix(rest, "#") {
		return nil, false
	}
	return splitKey(inner)
}

// closingBracket finds the ']' that closes a header, skipping quoted
// segments ("a]b" is a legal quoted key).
func closingBracket(t string) int {
	var quote byte
	for i := 1; i < len(t); i++ {
		c := t[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ']':
			return i
		}
	}
	return -1
}

// splitKey splits a dotted TOML key into unquoted segments.
func splitKey(s string) ([]string, bool) {
	var segs []string
	for {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, false
		}
		var seg string
		switch s[0] {
		case '"':
			end := 1
			for end < len(s) && s[end] != '"' {
				if s[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(s) {
				return nil, false
			}
			u, err := strconv.Unquote(s[:end+1])
			if err != nil {
				return nil, false
			}
			seg, s = u, s[end+1:]
		case '\'':
			end := strings.IndexByte(s[1:], '\'')
			if end < 0 {
				return nil, false
			}
			seg, s = s[1:end+1], s[end+2:]
		default:
			end := strings.IndexByte(s, '.')
			if end < 0 {
				end = len(s)
			}
			seg, s = strings.TrimSpace(s[:end]), s[end:]
			if !bareKeyRe.MatchString(seg) {
				return nil, false
			}
		}
		segs = append(segs, seg)
		s = strings.TrimSpace(s)
		if s == "" {
			return segs, true
		}
		if s[0] != '.' {
			return nil, false
		}
		s = s[1:]
	}
}
