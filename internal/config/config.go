// Package config reads devvm's one config file, $XDG_CONFIG_HOME/devvm/devvm.toml
// (config.md §1): `[templates.*]` (how to shape a new box, templates.go) and
// `[machines.*]` (the boxes devvm cannot discover: remote boxes and hubs).
// Local smol VMs have no entry; they are discovered from smolvm (backend's
// discovery sources). The file is the user's, hand-edited and safe to keep
// in a dotfiles repo shared by several hosts, so nothing here writes it
// except the two structural edits of the `config` noun (edit.go). This
// host's own state lives under Paths.State.
package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Backend is who owns a machine's registry and lifecycle (config.md §1, §9).
// It is one axis; Managed is the other.
//
//	smol    a local microVM, discovered from `smolvm machine ls`; never an
//	        entry in the file. Managed is settled by discovery (the marker).
//	remote  a `[machines.*]` entry reached over ssh. `managed = true` means
//	        devvm shapes it (setup, prereqs, hardening, pinned host key);
//	        the default false is an adopted box devvm never modifies.
//	hub     a `[machines.*]` entry for another host running devvm, whose
//	        machines are HUB/NAME (docs/proposals/hub.md). Reached over ssh
//	        like a remote box, but never shaped and never managed.
//	hetzner a Hetzner Cloud server, discovered from the API (providers.md
//	        §2); never an entry. Reached over ssh like a remote box; Managed
//	        is the devvm=true label. The value is the provider's name, so a
//	        second provider is the same shape (providers.go).
//
// Code outside this package asks the record's predicates (Managed,
// IsRemote, IsHub, IsHubMachine, Probeable), never compares these strings
// (§9 item 1): a provider backend later answers the same predicates.
const (
	BackendSmol    = "smol"
	BackendRemote  = "remote"
	BackendHub     = "hub"
	BackendHetzner = "hetzner"
)

// Transport selects how an interactive connection (shell/attach) is made to a
// remote backend; it does not affect forwards (always native ssh -L) or exec.
const (
	TransportSSH  = "ssh"
	TransportMosh = "mosh"
)

// DefaultBootstrapHook is the built-in hook: do nothing, so nothing personal
// is baked into the binary. Users who want one (e.g. their dotfiles
// bootstrap.sh) set `hook` in a template (templates.go).
const DefaultBootstrapHook = "none"

// ErrNotFound is returned by Load when the file has no `[machines.NAME]`
// entry. The cli then asks discovery (backend.Discover), which finds local
// smol VMs.
var ErrNotFound = errors.New("no such entry")

// nameRe mirrors valid_name in bin/devvm: names land in file paths and process
// patterns, so keep them tame.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidName reports whether a machine name is safe to use in paths.
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid machine name %q (use letters, digits, . _ -)", name)
	}
	return nil
}

// Machine is one resolved box: a `[machines.*]` entry, a discovered smol
// VM, or a HUB/NAME record derived from its hub's entry. It is never
// written back; the file is changed only by `config add`/`rm` (edit.go).
type Machine struct {
	// Name is the entry's table name, the smol VM's name, or for a hub
	// machine the display form HUB/NAME (see hubname.go).
	Name string

	// Hub is set only on a hub-machine record (LoadHubMachine): the entry of
	// the hub the machine lives on. This host's only state for such a
	// machine is its saved-forwards file.
	Hub *Machine

	Backend string // BackendSmol | BackendRemote | BackendHub | BackendHetzner

	// Server is set only on a provider record (NewProviderMachine): what
	// the API row said, at resolve time. Never saved.
	Server *ProviderServer

	// SSHHost is the ssh destination, `[user@]host`: the entry's host
	// without its port, prefixed with the entry's user, or on a managed
	// remote entry with no user, the [templates.default] user (the account
	// `setup` creates there, config.md §1, §3). With no user at all it is
	// the bare host and ~/.ssh/config decides, as for an alias.
	SSHHost string
	// SSHPort is the port given in `host = "h:P"`, 0 when none (ssh config
	// or 22). Only a port other than 0 and 22 is passed to ssh.
	SSHPort int
	// User is the entry's own `user`, "" when the entry sets none. setup
	// uses it to tell a pinned login from the template's.
	User string

	Identity   string
	Transport  string // ssh (default) | mosh
	MoshServer string
	// ConnectTimeout is the entry's `connect_timeout` in seconds, 0 when
	// unset (10s, or DEVVM_SSH_CONNECT_TIMEOUT, which wins when set).
	ConnectTimeout int

	// managed is the record's Managed bit: a remote entry's `managed`, or a
	// smol VM's as its discovery source settled it (the managed marker
	// against the VM's created_at, config.md §2). Never stored. Unsettled
	// means unmanaged, so a record nobody settled can never be taken for one
	// devvm may install into.
	managed bool
}

// NewRemote returns a remote record for the given ssh destination
// (`[user@]host`), the shape a `[machines.*]` entry loads to. For tests and
// callers that build a record by hand; the loader builds its own.
func NewRemote(name string, managed bool, sshHost string) *Machine {
	m := &Machine{Name: name, Backend: BackendRemote, SSHHost: sshHost, managed: managed}
	if u, _, ok := strings.Cut(sshHost, "@"); ok {
		m.User = u
	}
	return m
}

// NewHub returns a hub record for the given ssh destination.
func NewHub(name, sshHost string) *Machine {
	m := &Machine{Name: name, Backend: BackendHub, SSHHost: sshHost}
	if u, _, ok := strings.Cut(sshHost, "@"); ok {
		m.User = u
	}
	return m
}

// IsHub reports whether this record is a hub (another host's devvm) rather
// than a machine. A hub-machine record (Hub != nil) is not a hub.
func (m *Machine) IsHub() bool {
	return m.Backend == BackendHub && m.Hub == nil
}

// IsHubMachine reports whether this is a synthesized HUB/NAME record.
func (m *Machine) IsHubMachine() bool { return m.Hub != nil }

// Managed reports whether devvm owns this box's OS: it installs prereqs, may
// harden, and pins known_hosts. A remote entry is managed when it says
// `managed = true` (writing that line was the consent); a smol VM only when
// its discovery source said so (SetManaged): one devvm created or was handed
// with `setup --adopt`, never one it merely found (config.md §2). A hub and
// a machine on one are never managed here (the hub holds that machine's
// marker). A plain field read: it never execs or reads a file.
func (m *Machine) Managed() bool {
	if m.Backend == BackendHub {
		return false
	}
	return m.managed
}

// SetManaged settles a record's Managed bit. Only a discovery source calls
// it on a smol record (and tests); a remote entry's comes from the file.
func (m *Machine) SetManaged(managed bool) { m.managed = managed }

// IsRemote reports whether the box is reached over the ssh transport (a
// remote entry, a provider server, and a hub: its ssh fields drive the hop
// to it). Drives ssh flags, key management, mosh, and forwards. A
// hub-machine record carries the hub's ssh fields for the same reason, so
// it counts too.
func (m *Machine) IsRemote() bool {
	return m.Backend == BackendRemote || m.Backend == BackendHub || m.IsProvider()
}

// Probeable reports whether the box's state is its reach cache, probed over
// ssh (providers.md §5): a box reached over ssh that is neither a hub (its
// state is its listing pipe's) nor a machine on one (its hub's word).
func (m *Machine) Probeable() bool {
	return m.IsRemote() && !m.IsHub() && !m.IsHubMachine()
}

// TransportName is the effective interactive transport (defaulted to ssh).
func (m *Machine) TransportName() string {
	if m.Transport == "" {
		return TransportSSH
	}
	return m.Transport
}

// Load returns the `[machines.NAME]` entry from devvm.toml. ErrNotFound
// (wrapped) when the file has none, including when there is no file; a
// file that does not parse is an error for every name, since an entry
// shadows a discovered VM and a broken file cannot say whether it does.
func Load(p Paths, name string) (*Machine, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	f, err := LoadFile(p)
	if err != nil {
		return nil, err
	}
	m, ok := f.Entry(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s in %s", ErrNotFound, MachineTable(name), f.Path)
	}
	return m, nil
}

// Exists reports whether devvm.toml has a `[machines.NAME]` entry. A file
// that does not parse has none.
func Exists(p Paths, name string) bool {
	f, err := LoadFile(p)
	if err != nil {
		return false
	}
	_, ok := f.Entry(name)
	return ok
}

// List returns the names of every `[machines.*]` entry, sorted. A missing
// file has none.
func List(p Paths) ([]string, error) {
	f, err := LoadFile(p)
	if err != nil {
		return nil, err
	}
	return f.Names(), nil
}

// OldLayout reports whether the config dir still holds the per-machine
// layout (machines/ or config.toml) with no devvm.toml beside it. Nothing
// reads the old files any more: the cli prints one line pointing at the
// hand migration (config.md §8) and carries on with no entries.
//
// An empty machines/ is not the old layout: an older `status --watch`
// created it on every start, conf or not.
func OldLayout(p Paths) bool {
	if exists(p.FilePath()) {
		return false
	}
	names, _ := legacyConfNames(p)
	return len(names) > 0 || exists(LegacyDefaultsPath(p))
}

// LegacyMachinesDir is the old per-machine conf directory, named only by
// OldLayout and the one-time seeds (MigratePorts, LegacySmolConfs).
func LegacyMachinesDir(p Paths) string { return filepath.Join(p.Config, "machines") }

// LegacyDefaultsPath is the old config.toml, named only by OldLayout.
func LegacyDefaultsPath(p Paths) string { return filepath.Join(p.Config, "config.toml") }

// SplitPort parses a "HOST:GUEST" or bare "PORT" mapping into its host
// preference and guest port, mirroring split_port in bin/devvm.
func SplitPort(mapping string) (host, guest string) {
	if h, g, ok := strings.Cut(mapping, ":"); ok {
		return h, g
	}
	return mapping, mapping
}
