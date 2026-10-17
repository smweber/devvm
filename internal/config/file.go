package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// The loader for devvm.toml (config.md §1). Three table families,
// `[templates.*]`, `[machines.*]` and `[providers.*]` (providers.md §1),
// nothing else: an unknown top-level table is an error, not a silent no-op,
// so a later family is a loader addition that an older devvm refuses
// loudly rather than half-reads. Unknown keys inside
// a known table are errors too: a typo would otherwise drop a setting, and
// for `managed` that would quietly be the safe reading the user did not
// mean. A missing file is valid: no entries, built-in templates.

// FilePath is the user's shared config file, devvm.toml.
func (p Paths) FilePath() string { return filepath.Join(p.Config, "devvm.toml") }

// Entry is one `[machines.NAME]` table as written. Managed is a pointer so
// `managed = false` written explicitly (as `config add` always does) is told
// apart from no key, which matters only for refusing it under a hub.
type Entry struct {
	Backend        string `toml:"backend"`
	Managed        *bool  `toml:"managed"`
	Host           string `toml:"host"` // host[:port], [v6]:port, or an ssh alias
	User           string `toml:"user"` // unset: no user@, ssh config decides
	Transport      string `toml:"transport"`
	MoshServer     string `toml:"mosh_server"`
	Identity       string `toml:"identity"`
	ConnectTimeout int    `toml:"connect_timeout"` // seconds; 0 = default
}

// File is devvm.toml, parsed and validated.
type File struct {
	Path      string
	Templates map[string]TemplateSpec
	Entries   map[string]Entry
	Providers map[string]ProviderSpec
	// machines are the entries as records, built once by validate.
	machines map[string]*Machine
	defUser  string
}

type rawFile struct {
	Templates map[string]TemplateSpec `toml:"templates"`
	Machines  map[string]Entry        `toml:"machines"`
	Providers map[string]ProviderSpec `toml:"providers"`
}

// LoadFile reads and validates devvm.toml. A missing file is an empty,
// valid one.
func LoadFile(p Paths) (*File, error) {
	path := p.FilePath()
	if _, err := ResolveFile(p); err != nil {
		return nil, err // a dangling symlink is not a missing file
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ParseFile(path, nil)
	}
	if err != nil {
		return nil, err
	}
	return ParseFile(path, data)
}

// ParseFile parses and validates devvm.toml's content; path is for
// messages. Every problem is reported, not just the first, so `config
// check` names them all in one run.
func ParseFile(path string, data []byte) (*File, error) {
	var raw rawFile
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	f := &File{Path: path, Templates: raw.Templates, Entries: raw.Machines, Providers: raw.Providers}
	if f.Providers == nil {
		f.Providers = map[string]ProviderSpec{}
	}
	if f.Templates == nil {
		f.Templates = map[string]TemplateSpec{}
	}
	if f.Entries == nil {
		f.Entries = map[string]Entry{}
	}
	var errs []error
	for _, k := range md.Undecoded() {
		errs = append(errs, undecodedError(k))
	}
	errs = append(errs, f.validate()...)
	if len(errs) > 0 {
		for i, e := range errs {
			errs[i] = fmt.Errorf("%s: %w", path, e)
		}
		return nil, errors.Join(errs...)
	}
	return f, nil
}

// undecodedError names a key the loader does not know.
func undecodedError(k toml.Key) error {
	switch {
	case len(k) == 0:
		return errors.New("unknown key")
	case k[0] == "templates" && len(k) > 2:
		return fmt.Errorf("[templates.%s]: unknown key %q", k[1], strings.Join(k[2:], "."))
	case k[0] == "machines" && len(k) > 2:
		return fmt.Errorf("[machines.%s]: unknown key %q", k[1], strings.Join(k[2:], "."))
	case k[0] == "providers" && len(k) > 2:
		return fmt.Errorf("[providers.%s]: unknown key %q", k[1], strings.Join(k[2:], "."))
	case k[0] == "templates" || k[0] == "machines" || k[0] == "providers":
		return fmt.Errorf("unknown key %q (want [%s.NAME] tables)", k.String(), k[0])
	case len(k) == 1:
		return fmt.Errorf("unknown top-level key or table %q (devvm.toml has [templates.*], [machines.*] and [providers.*] only)", k[0])
	default:
		return fmt.Errorf("unknown top-level table [%s] (devvm.toml has [templates.*], [machines.*] and [providers.*] only)", k[0])
	}
}

func (f *File) validate() []error {
	var errs []error
	for _, name := range sortedKeys(f.Templates) {
		if err := ValidName(name); err != nil {
			errs = append(errs, fmt.Errorf("[templates.%s]: %w", name, err))
			continue
		}
		if err := f.Templates[name].Validate(); err != nil {
			errs = append(errs, fmt.Errorf("[templates.%s]: %w", name, err))
		}
	}
	for _, name := range sortedKeys(f.Providers) {
		if !IsProviderBackend(name) {
			errs = append(errs, fmt.Errorf("[providers.%s]: unknown provider (devvm knows: %s)", name, strings.Join(knownProviders, ", ")))
			continue
		}
		if err := f.Providers[name].validate(); err != nil {
			errs = append(errs, fmt.Errorf("[providers.%s]: %w", name, err))
		}
	}
	defUser := DefaultTemplateUser
	if u := f.Templates[DefaultTemplate].User; u != nil && *u != "" && ValidUser(*u) == nil {
		defUser = *u
	}
	f.defUser = defUser
	f.machines = map[string]*Machine{}
	for _, name := range sortedKeys(f.Entries) {
		m, err := f.Entries[name].machine(name, defUser)
		if err != nil {
			errs = append(errs, fmt.Errorf("[machines.%s]: %w", name, err))
			continue
		}
		f.machines[name] = m
	}
	return errs
}

// entryUserRe is a login name for an entry's `user`. Looser than a
// template's (ValidUser): an adopted box may well be reached as root.
var entryUserRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]*$`)

// machine validates one entry and builds its record. defUser is the
// [templates.default] user, which a managed remote entry with no `user` logs
// in as: it is the account `setup` creates there.
func (e Entry) machine(name, defUser string) (*Machine, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	switch e.Backend {
	case BackendRemote, BackendHub:
	case "":
		return nil, errors.New(`backend is required ("remote" or "hub")`)
	case BackendSmol:
		return nil, errors.New("smol VMs are discovered from smolvm, not configured: remove this table")
	case BackendHetzner:
		return nil, fmt.Errorf("%s servers are discovered from the provider's API, not configured: remove this table (a server made in the console is a backend = \"remote\" entry)", e.Backend)
	case "remote-managed", "remote-unmanaged", "ssh":
		return nil, fmt.Errorf(`backend %q is the old layout: write backend = "remote" and managed = true or false`, e.Backend)
	default:
		return nil, fmt.Errorf(`unsupported backend %q (want "remote" or "hub")`, e.Backend)
	}
	if e.Backend == BackendHub && e.Managed != nil {
		return nil, errors.New("managed applies only to remote entries: a hub is never shaped")
	}
	host, port, err := ParseHost(e.Host)
	if err != nil {
		return nil, err
	}
	if e.User != "" && !entryUserRe.MatchString(e.User) {
		return nil, fmt.Errorf("invalid user %q", e.User)
	}
	switch e.Transport {
	case "", TransportSSH, TransportMosh:
	default:
		return nil, fmt.Errorf("invalid transport %q (want %q or %q)", e.Transport, TransportSSH, TransportMosh)
	}
	if e.ConnectTimeout < 0 {
		return nil, fmt.Errorf("connect_timeout %d must be a positive number of seconds", e.ConnectTimeout)
	}
	m := &Machine{
		Name: name, Backend: e.Backend, SSHPort: port, User: e.User,
		Identity: e.Identity, Transport: e.Transport, MoshServer: e.MoshServer,
		ConnectTimeout: e.ConnectTimeout,
		managed:        e.Backend == BackendRemote && e.Managed != nil && *e.Managed,
	}
	user := e.User
	if user == "" && m.managed {
		user = defUser
	}
	m.SSHHost = host
	if user != "" {
		m.SSHHost = user + "@" + host
	}
	return m, nil
}

// ParseHost splits an entry's `host` into the ssh host and port (0 when
// none). An IPv6 address needs brackets ([::1] or [::1]:22): a bare one
// cannot be told from host:port. A user@ belongs in `user`, so it is refused
// here rather than doubled up.
func ParseHost(s string) (host string, port int, err error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", 0, errors.New("host is required (host[:port] or an ssh alias)")
	case strings.Contains(s, "@"):
		return "", 0, fmt.Errorf("host %q carries a user: put it in user = \"...\"", s)
	case strings.HasPrefix(s, "-"), strings.ContainsAny(s, " \t\n/"):
		return "", 0, fmt.Errorf("invalid host %q", s)
	}
	portStr := ""
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", 0, fmt.Errorf("invalid host %q: unclosed [", s)
		}
		host, rest := s[1:end], s[end+1:]
		if net.ParseIP(host) == nil || !strings.Contains(host, ":") {
			return "", 0, fmt.Errorf("invalid host %q: brackets are for an IPv6 address", s)
		}
		if rest != "" {
			p, ok := strings.CutPrefix(rest, ":")
			if !ok || p == "" {
				return "", 0, fmt.Errorf("invalid host %q", s)
			}
			portStr = p
		}
		s = host
	} else {
		switch strings.Count(s, ":") {
		case 0:
		case 1:
			h, ps, _ := strings.Cut(s, ":")
			if h == "" || ps == "" {
				return "", 0, fmt.Errorf("invalid host %q (host[:port])", s)
			}
			s, portStr = h, ps
		default:
			return "", 0, fmt.Errorf("host %q: an IPv6 address needs brackets, [addr] or [addr]:port", s)
		}
	}
	if portStr != "" {
		n, err := strconv.Atoi(portStr)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("invalid port %q in host", portStr)
		}
		port = n
	}
	return s, port, nil
}

// Entry returns the record for a `[machines.NAME]` entry: a fresh copy, so
// a caller may change it (setup's --host does) without touching the file's.
func (f *File) Entry(name string) (*Machine, bool) {
	m, ok := f.machines[name]
	if !ok {
		return nil, false
	}
	c := *m
	return &c, true
}

// Names is every entry's name, sorted.
func (f *File) Names() []string { return sortedKeys(f.Entries) }

// Machines is every entry as a record, sorted by name.
func (f *File) Machines() []*Machine {
	var out []*Machine
	for _, n := range f.Names() {
		m, _ := f.Entry(n)
		out = append(out, m)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// EntryOf renders a record back into the entry that loads to it: the host
// with its port (bracketed for IPv6), the record's own user. Used to write a
// record's table (AppendEntry) by tests and tools that hold a record.
func EntryOf(m *Machine) Entry {
	host := m.SSHHost
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if m.SSHPort != 0 {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		host = fmt.Sprintf("%s:%d", host, m.SSHPort)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	e := Entry{Backend: m.Backend, Host: host, User: m.User, Transport: m.Transport,
		MoshServer: m.MoshServer, Identity: m.Identity, ConnectTimeout: m.ConnectTimeout}
	if m.Backend == BackendRemote {
		managed := m.Managed()
		e.Managed = &managed
	}
	return e
}

// writeFileAtomic writes via a temp file in the same directory and a rename,
// so a reader (notably `status --watch`, which re-snapshots on the write
// event) never sees a truncated or half-written file. An existing file keeps
// its mode (a file the user chmod'ed to 0600 stays that way); mode applies
// only to a new file. The data is fsync'ed before the rename so a crash can't
// leave a zero-length file behind the new name (the directory entry itself
// is not fsync'ed: a power loss right after the rename may still show the
// old file, which is a complete file either way).
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(mode); err != nil { // CreateTemp is 0600 regardless of umask
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
