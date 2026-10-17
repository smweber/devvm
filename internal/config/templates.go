package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Templates (config.md §1, §3): "how to shape a new box", read from the
// `[templates.*]` tables of $XDG_CONFIG_HOME/devvm/devvm.toml. `create` and
// `setup` resolve one; nothing records which template made a box, so
// editing a template never changes a running VM, and `setup NAME` with no
// --template re-runs whatever `default` says now.

// DefaultTemplate is the template `create`/`setup` use without --template,
// and the one every other template falls back to (one level, no chains).
const DefaultTemplate = "default"

// DefaultTemplateUser is the built-in `user`: the account `setup` creates
// and logs in as on a managed remote box. smol VMs always use it (the smol
// backend is wired to it), so a template's user applies to remote only.
const DefaultTemplateUser = "dev"

// TemplateSpec is one layer of template values: a `[templates.NAME]`
// table, or the flags given on the command line. Every field is a pointer so "unset" (fall back to the next
// layer) is told apart from a deliberate zero (`harden = false`, `repos =
// []`). Key names match the old per-machine confs where they existed.
type TemplateSpec struct {
	User                 *string   `toml:"user"`
	Hook                 *string   `toml:"hook"`
	Memory               *int      `toml:"memory"` // MiB, create-time only
	Disk                 *int      `toml:"disk"`   // GiB, create-time only
	Repos                *[]string `toml:"repos"`
	Ports                *[]string `toml:"ports"`
	AuthorizedKeysGithub *[]string `toml:"authorized_keys_github"`
	AuthorizedKeys       *[]string `toml:"authorized_keys"`
	Harden               *bool     `toml:"harden"`
	Fail2ban             *bool     `toml:"fail2ban"`
}

// Template is a fully resolved template: every layer applied, built-ins
// filled. Memory and Disk stay 0 when no layer set them (create then
// prompts, errors, or uses the backend's default disk).
type Template struct {
	Name                 string
	User                 string
	Hook                 string
	Memory               int
	Disk                 int
	Repos                []string
	Ports                []string
	AuthorizedKeysGithub []string
	AuthorizedKeys       []string
	Harden               bool
	Fail2ban             bool
}

// Under returns s with every unset field taken from lower: s wins. Chained
// left to right, highest precedence first:
//
//	flags.Under(named).Under(def).Resolve(name)
//
// An empty string (`hook = ""`) is unset, not a value: it falls through like
// a missing key, so a blank line in a template never masks default's hook.
func (s TemplateSpec) Under(lower TemplateSpec) TemplateSpec {
	out := s
	if blank(out.User) {
		out.User = lower.User
	}
	if blank(out.Hook) {
		out.Hook = lower.Hook
	}
	if out.Memory == nil {
		out.Memory = lower.Memory
	}
	if out.Disk == nil {
		out.Disk = lower.Disk
	}
	if out.Repos == nil {
		out.Repos = lower.Repos
	}
	if out.Ports == nil {
		out.Ports = lower.Ports
	}
	if out.AuthorizedKeysGithub == nil {
		out.AuthorizedKeysGithub = lower.AuthorizedKeysGithub
	}
	if out.AuthorizedKeys == nil {
		out.AuthorizedKeys = lower.AuthorizedKeys
	}
	if out.Harden == nil {
		out.Harden = lower.Harden
	}
	if out.Fail2ban == nil {
		out.Fail2ban = lower.Fail2ban
	}
	return out
}

// Resolve fills the built-ins under every unset field: user dev, hook
// none, everything else empty.
func (s TemplateSpec) Resolve(name string) Template {
	t := Template{Name: name, User: DefaultTemplateUser, Hook: DefaultBootstrapHook}
	if s.User != nil && *s.User != "" {
		t.User = *s.User
	}
	if s.Hook != nil && strings.TrimSpace(*s.Hook) != "" {
		t.Hook = strings.TrimSpace(*s.Hook)
	}
	if s.Memory != nil {
		t.Memory = *s.Memory
	}
	if s.Disk != nil {
		t.Disk = *s.Disk
	}
	t.Repos = deref(s.Repos)
	t.Ports = deref(s.Ports)
	t.AuthorizedKeysGithub = deref(s.AuthorizedKeysGithub)
	t.AuthorizedKeys = deref(s.AuthorizedKeys)
	if s.Harden != nil {
		t.Harden = *s.Harden
	}
	if s.Fail2ban != nil {
		t.Fail2ban = *s.Fail2ban
	}
	return t
}

func blank(p *string) bool { return p == nil || strings.TrimSpace(*p) == "" }

func deref(p *[]string) []string {
	if p == nil {
		return nil
	}
	return slices.Clone(*p)
}

// Spec turns a resolved template back into a layer with every field set:
// the proxied form sends exactly this, so the hub's own file never matters.
func (t Template) Spec() TemplateSpec {
	user, hook, mem, disk := t.User, t.Hook, t.Memory, t.Disk
	repos, ports := slices.Clone(t.Repos), slices.Clone(t.Ports)
	gh, files := slices.Clone(t.AuthorizedKeysGithub), slices.Clone(t.AuthorizedKeys)
	harden, f2b := t.Harden, t.Fail2ban
	return TemplateSpec{User: &user, Hook: &hook, Memory: &mem, Disk: &disk,
		Repos: &repos, Ports: &ports, AuthorizedKeysGithub: &gh, AuthorizedKeys: &files,
		Harden: &harden, Fail2ban: &f2b}
}

// LoadTemplates reads every `[templates.*]` table of devvm.toml (LoadFile:
// the whole file is validated, so a broken entry fails a template read
// too). A missing file is valid and means no templates.
func LoadTemplates(p Paths) (map[string]TemplateSpec, error) {
	f, err := LoadFile(p)
	if err != nil {
		return nil, err
	}
	return f.Templates, nil
}

// TemplateLayers returns the named template's own table and the default
// one, the two file layers of a resolution. An empty name means default
// (whose own table is then the default layer too). A named template that
// does not exist is an error; a missing default is just empty.
func TemplateLayers(p Paths, name string) (named, def TemplateSpec, err error) {
	ts, err := LoadTemplates(p)
	if err != nil {
		return TemplateSpec{}, TemplateSpec{}, err
	}
	def = ts[DefaultTemplate]
	if name == "" || name == DefaultTemplate {
		return def, def, nil
	}
	named, ok := ts[name]
	if !ok {
		have := make([]string, 0, len(ts))
		for n := range ts {
			have = append(have, n)
		}
		slices.Sort(have)
		return TemplateSpec{}, TemplateSpec{}, fmt.Errorf("no template %q in %s (have: %s)",
			name, p.FilePath(), strings.Join(have, ", "))
	}
	return named, def, nil
}

// userRe is a conservative POSIX login name: it lands in useradd, sudoers
// and sshd's AllowUsers.
var userRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ValidUser reports whether a template user is safe to create and pin.
func ValidUser(u string) error {
	if !userRe.MatchString(u) {
		return fmt.Errorf("invalid user %q (lowercase letters, digits, _ and -, starting with a letter or _)", u)
	}
	if u == "root" {
		return fmt.Errorf("user cannot be root: setup creates an unprivileged user with sudo")
	}
	return nil
}

// Validate checks the fields config can check on its own. The hook spec
// and port mappings are checked by the cli, which owns their parsers.
func (s TemplateSpec) Validate() error {
	if s.User != nil && *s.User != "" {
		if err := ValidUser(*s.User); err != nil {
			return err
		}
	}
	if s.Memory != nil && *s.Memory != 0 && *s.Memory < 512 {
		return fmt.Errorf("memory %d too small (min 512 MiB)", *s.Memory)
	}
	if s.Disk != nil && *s.Disk != 0 && *s.Disk < 1 {
		return fmt.Errorf("disk %d too small (min 1 GiB)", *s.Disk)
	}
	return nil
}
