package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Providers (docs/proposals/providers.md §1, §2): the third table family,
// `[providers.NAME]`, holds where a provider's servers are made (location,
// image, the ssh key) and never a credential: the token is per-host state
// (<State>/providers/NAME.token). A provider server has no `[machines.*]`
// entry; it is discovered from the provider's API (the cli's provider
// source) and its record is built by NewProviderMachine from the API row,
// never stored. Sizing (`server_type`) and the same placement keys are
// template fields too, so precedence is template > provider table >
// built-in (Template.Placement).

// knownProviders are the `[providers.*]` names (and backend values) this
// build speaks. The backend value of a provider box is the provider's name
// (§7), so a second provider is one more entry here and a package beside
// internal/provider/hetzner.
var knownProviders = []string{BackendHetzner}

// IsProviderBackend reports whether b names a provider (a backend whose
// registry is a cloud API), as opposed to smol, remote or hub.
func IsProviderBackend(b string) bool { return slices.Contains(knownProviders, b) }

// KnownProviders is every provider this build speaks, sorted.
func KnownProviders() []string { return slices.Clone(knownProviders) }

// The built-in placement: what a provider box gets when neither its
// template nor its provider table says (providers.md §1). cx23 is the
// smallest shared x86 type that runs a dev box comfortably; nbg1 and
// ubuntu-24.04 are what the user's boxes run today.
const (
	DefaultServerType = "cx23"
	DefaultLocation   = "nbg1"
	DefaultImage      = "ubuntu-24.04"
)

// ProviderSpec is one `[providers.NAME]` table. Strings are unset when
// empty; IPv4 is a pointer so `ipv4 = false` is told apart from no key
// (default true: a v6-only box is unreachable from a laptop on a v4-only
// network, which is most of them).
type ProviderSpec struct {
	Location   string `toml:"location"`
	Image      string `toml:"image"`
	SSHKey     string `toml:"ssh_key"`      // a key already in the project, by name
	SSHKeyFile string `toml:"ssh_key_file"` // or a public key file, uploaded once by name
	IPv4       *bool  `toml:"ipv4"`
}

// placementRe is a location, image or server type: Hetzner's own names are
// short lowercase tokens (nbg1, cx23, ubuntu-24.04) and an image may be a
// numeric id. Kept tame since the values land in API requests and in
// `status -v`.
var placementRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validPlacement(key, v string) error {
	if v != "" && !placementRe.MatchString(v) {
		return fmt.Errorf("invalid %s %q", key, v)
	}
	return nil
}

func (s ProviderSpec) validate() error {
	var errs []error
	for _, kv := range [][2]string{{"location", s.Location}, {"image", s.Image}} {
		if err := validPlacement(kv[0], kv[1]); err != nil {
			errs = append(errs, err)
		}
	}
	if s.SSHKey != "" && s.SSHKeyFile != "" {
		errs = append(errs, errors.New("ssh_key and ssh_key_file are alternatives: set one"))
	}
	if strings.ContainsAny(s.SSHKey, "\n\t") {
		errs = append(errs, fmt.Errorf("invalid ssh_key %q", s.SSHKey))
	}
	return errors.Join(errs...)
}

// Placement is a provider box's create-time placement, every layer applied.
type Placement struct {
	ServerType string
	Location   string
	Image      string
	IPv4       bool
	SSHKey     string
	SSHKeyFile string
}

// Placement resolves t's provider fields over the provider table p and the
// built-ins: template > provider table > built-in. The key settings are the
// table's alone (a template does not pick the project's key).
func (t Template) Placement(p ProviderSpec) Placement {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if v != "" {
				return v
			}
		}
		return ""
	}
	pl := Placement{
		ServerType: pick(t.ServerType, DefaultServerType),
		Location:   pick(t.Location, p.Location, DefaultLocation),
		Image:      pick(t.Image, p.Image, DefaultImage),
		IPv4:       true,
		SSHKey:     p.SSHKey,
		SSHKeyFile: p.SSHKeyFile,
	}
	switch {
	case t.IPv4 != nil:
		pl.IPv4 = *t.IPv4
	case p.IPv4 != nil:
		pl.IPv4 = *p.IPv4
	}
	return pl
}

// ProviderFields names the provider-only fields s sets (server_type,
// location, image, ipv4): `config check` warns when a template sets one and
// no `[providers.*]` table exists, and setup names them as ignored on a box
// that is not a provider's.
func (s TemplateSpec) ProviderFields() []string {
	var out []string
	if !blank(s.ServerType) {
		out = append(out, "server_type")
	}
	if !blank(s.Location) {
		out = append(out, "location")
	}
	if !blank(s.Image) {
		out = append(out, "image")
	}
	if s.IPv4 != nil {
		out = append(out, "ipv4")
	}
	return out
}

// ProviderServer is what a provider record keeps of the API row it was
// built from, for `status` and (step 3) the lifecycle verbs: the server id
// the API's actions take, the provider's raw status word and its mapped
// --plain state, and the placement `status -v` shows. Never saved to any
// file: the API is the registry, and the next resolve reads a fresh row.
type ProviderServer struct {
	ID       int64
	Status   string // the provider's own word ("running", "off", "initializing")
	State    string // mapped to the --plain tokens (provider.State*)
	Address  string // public IPv4, or the IPv6 address the server answers on
	Type     string
	Location string
}

// NewProviderMachine builds the record for a provider server (providers.md
// §4): reached over ssh on port 22 at the API row's address, read at
// resolve time and never stored. A managed server (the devvm=true label,
// settled by the caller from the row) logs in as user, the
// [templates.default] user that `setup` creates there: the rule of a
// managed remote entry with no `user`. An unmanaged one gets no user@, as
// an adopted remote entry with no `user` does, so ~/.ssh/config (or the
// local login name) decides: devvm has no business guessing an account on
// a server it did not make.
func NewProviderMachine(name, providerName, user string, managed bool, s ProviderServer) *Machine {
	m := &Machine{Name: name, Backend: providerName, SSHPort: 22, managed: managed, Server: &s}
	m.SSHHost = s.Address
	if managed && user != "" && s.Address != "" {
		m.SSHHost = user + "@" + s.Address
	}
	return m
}

// IsProvider reports whether the record is a provider server's (built by
// NewProviderMachine from an API row).
func (m *Machine) IsProvider() bool { return IsProviderBackend(m.Backend) && m.Server != nil }

// HasProviders reports whether the file configures any provider.
func (f *File) HasProviders() bool { return len(f.Providers) > 0 }

// ProviderNames is every configured provider, sorted.
func (f *File) ProviderNames() []string { return sortedKeys(f.Providers) }

// DefaultUser is the [templates.default] user (built-in dev): the login of
// a managed remote entry with no `user`, and of a managed provider server.
func (f *File) DefaultUser() string {
	if f.defUser == "" {
		return DefaultTemplateUser
	}
	return f.defUser
}
