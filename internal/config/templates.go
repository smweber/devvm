package config

import (
	"fmt"
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

// DefaultUser is the account setup creates in every box devvm makes, and
// the one devvm works as there: a smol VM (the smol backend is wired to
// it) and a provider server (its login). Templates have no user (it was
// retired: devvm stores nothing per box, so it could not remember another
// one). An entry's own `user` is unrelated: it is how that box is reached.
const DefaultUser = "dev"

// TemplateSpec is one layer of template values: a `[templates.NAME]`
// table, or the flags given on the command line. Every field is a pointer so "unset" (fall back to the next
// layer) is told apart from a deliberate zero (`harden = false`, `repos =
// []`). Key names match the old per-machine confs where they existed.
type TemplateSpec struct {
	Hook                 *string   `toml:"hook"`
	Memory               *int      `toml:"memory"` // MiB, create-time only
	Disk                 *int      `toml:"disk"`   // GiB, create-time only
	Repos                *[]string `toml:"repos"`
	AuthorizedKeysGithub *[]string `toml:"authorized_keys_github"`
	AuthorizedKeys       *[]string `toml:"authorized_keys"`
	Harden               *bool     `toml:"harden"`
	Fail2ban             *bool     `toml:"fail2ban"`
	// Provider placement (providers.md §1): create-time only, provider
	// boxes only, over the `[providers.*]` table (Template.Placement).
	ServerType *string `toml:"server_type"`
	Location   *string `toml:"location"`
	Image      *string `toml:"image"`
	IPv4       *bool   `toml:"ipv4"`
}

// Template is a fully resolved template: every layer applied, built-ins
// filled. Memory and Disk stay 0 when no layer set them (create then
// prompts, errors, or uses the backend's default disk).
type Template struct {
	Name                 string
	Hook                 string
	Memory               int
	Disk                 int
	Repos                []string
	AuthorizedKeysGithub []string
	AuthorizedKeys       []string
	Harden               bool
	Fail2ban             bool
	// The provider fields as the template layers set them, "" (nil) when
	// none did: the provider table and the built-ins go under them in
	// Placement, which needs to know the table, so they are not filled here.
	ServerType string
	Location   string
	Image      string
	IPv4       *bool
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
	if blank(out.ServerType) {
		out.ServerType = lower.ServerType
	}
	if blank(out.Location) {
		out.Location = lower.Location
	}
	if blank(out.Image) {
		out.Image = lower.Image
	}
	if out.IPv4 == nil {
		out.IPv4 = lower.IPv4
	}
	return out
}

// Resolve fills the built-ins under every unset field: hook none,
// everything else empty.
func (s TemplateSpec) Resolve(name string) Template {
	t := Template{Name: name, Hook: DefaultBootstrapHook}
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
	t.AuthorizedKeysGithub = deref(s.AuthorizedKeysGithub)
	t.AuthorizedKeys = deref(s.AuthorizedKeys)
	if s.Harden != nil {
		t.Harden = *s.Harden
	}
	if s.Fail2ban != nil {
		t.Fail2ban = *s.Fail2ban
	}
	trim := func(p *string) string {
		if p == nil {
			return ""
		}
		return strings.TrimSpace(*p)
	}
	t.ServerType, t.Location, t.Image = trim(s.ServerType), trim(s.Location), trim(s.Image)
	if s.IPv4 != nil {
		v := *s.IPv4
		t.IPv4 = &v
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

// Validate checks the fields config can check on its own. The hook spec is
// checked by the cli, which owns its parser.
func (s TemplateSpec) Validate() error {
	if s.Memory != nil && *s.Memory != 0 && *s.Memory < 512 {
		return fmt.Errorf("memory %d too small (min 512 MiB)", *s.Memory)
	}
	if s.Disk != nil && *s.Disk != 0 && *s.Disk < 1 {
		return fmt.Errorf("disk %d too small (min 1 GiB)", *s.Disk)
	}
	for _, kv := range []struct {
		key string
		v   *string
	}{{"server_type", s.ServerType}, {"location", s.Location}, {"image", s.Image}} {
		if kv.v != nil {
			if err := validPlacement(kv.key, strings.TrimSpace(*kv.v)); err != nil {
				return err
			}
		}
	}
	return nil
}
