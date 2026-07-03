// Package config is the devvm machine registry: one TOML file per machine under
// $XDG_CONFIG_HOME/devvm/machines/<name>.toml. It replaces the sourced-bash
// confs the old bin/devvm read (load_machine / save_machine_conf), keeping the
// same field set but as validated, hand-editable TOML rather than executable
// shell.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Backend identifies which transport a machine uses.
const (
	BackendSmol = "smol"
	BackendSSH  = "ssh"
)

// Default provisioner: reproduce the old bootstrap path (bootstrap_machine),
// cloning smweber/dotfiles and running its agent-vm profile non-interactively.
const DefaultProvision = "url:https://raw.githubusercontent.com/smweber/dotfiles/master/bootstrap.sh --profile agent-vm --yes"

// ErrNotFound is returned by Load when no conf file exists for a name. Callers
// that also know about live-but-unregistered smol VMs handle that fallback.
var ErrNotFound = errors.New("machine not registered")

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

// Machine is one registered dev box. Zero values mean "unset"; applyDefaults
// fills the same defaults load_machine did (SSHPort 22, VNCPort 5901, Provision).
type Machine struct {
	// Name is derived from the filename, not stored in the file.
	Name string `toml:"-"`

	Backend   string `toml:"backend"`
	Unmanaged bool   `toml:"unmanaged,omitempty"`

	// ssh backend
	SSHHost    string `toml:"ssh_host,omitempty"`
	SSHPort    int    `toml:"ssh_port,omitempty"`
	Identity   string `toml:"identity,omitempty"`
	MoshServer string `toml:"mosh_server,omitempty"`
	VNCPort    int    `toml:"vnc_port,omitempty"`

	// smol backend
	Memory int `toml:"memory,omitempty"` // MiB

	// shared
	Repos     []string `toml:"repos,omitempty"`
	Ports     []string `toml:"ports,omitempty"` // "HOST:GUEST" or bare "PORT"
	Provision string   `toml:"provision,omitempty"`

	// ssh key seeding / hardening
	AuthorizedKeys       []string `toml:"authorized_keys,omitempty"`
	AuthorizedKeysGithub []string `toml:"authorized_keys_github,omitempty"`
	Harden               bool     `toml:"harden,omitempty"`
	Fail2ban             bool     `toml:"fail2ban,omitempty"`
}

// NewSmol returns a defaulted smol machine. Used for the "live but unregistered
// smol VM" fallback load_machine kept, so pre-registry VMs still work.
func NewSmol(name string) *Machine {
	m := &Machine{Name: name, Backend: BackendSmol}
	m.applyDefaults()
	return m
}

// applyDefaults fills unset fields, matching load_machine's defaulting.
func (m *Machine) applyDefaults() {
	if m.SSHPort == 0 {
		m.SSHPort = 22
	}
	if m.VNCPort == 0 {
		m.VNCPort = 5901
	}
	if m.Provision == "" {
		m.Provision = DefaultProvision
	}
}

// Validate enforces the invariants load_machine checked at source time.
func (m *Machine) Validate() error {
	switch m.Backend {
	case BackendSmol, BackendSSH:
	case "":
		return fmt.Errorf("machine %q has no backend set", m.Name)
	default:
		return fmt.Errorf("machine %q has unsupported backend %q", m.Name, m.Backend)
	}
	if m.Backend == BackendSSH && m.SSHHost == "" {
		return fmt.Errorf("ssh machine %q needs ssh_host", m.Name)
	}
	return nil
}

// ManagedSSH reports whether devvm manages this ssh host's lifecycle/hardening.
// Unmanaged ssh hosts (UNMANAGED=1) get no-op lifecycle and no known_hosts
// pinning, exactly as the old MANAGED_SSH derivation did.
func (m *Machine) ManagedSSH() bool {
	return m.Backend == BackendSSH && !m.Unmanaged
}

// IsExposed reports whether the backend is reachable off-host (drives key
// seeding + hardening). Local smol VMs are not; ssh (and future cloud) are.
func (m *Machine) IsExposed() bool {
	return m.Backend == BackendSSH
}

// Dir returns the machines directory under the given config dir.
func MachinesDir(configDir string) string { return filepath.Join(configDir, "machines") }

func confPath(configDir, name string) string {
	return filepath.Join(MachinesDir(configDir), name+".toml")
}

// Load reads, defaults, and validates the machine named name. Returns
// ErrNotFound (wrapped) if no conf file exists.
func Load(configDir, name string) (*Machine, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	p := confPath(configDir, name)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
		}
		return nil, err
	}
	m := &Machine{Name: name}
	if err := toml.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	m.Name = name
	m.applyDefaults()
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// Save writes the machine's conf as commented TOML. It is tool-managed but
// meant to stay hand-editable, so we lead with a header like the old confs.
func (m *Machine) Save(configDir string) error {
	if err := ValidName(m.Name); err != nil {
		return err
	}
	dir := MachinesDir(configDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# devvm machine config for %q (tool-managed; edit freely)\n\n", m.Name)
	if err := toml.NewEncoder(&b).Encode(m); err != nil {
		return err
	}
	return os.WriteFile(confPath(configDir, m.Name), []byte(b.String()), 0o644)
}

// Exists reports whether a conf file is present for name.
func Exists(configDir, name string) bool {
	_, err := os.Stat(confPath(configDir, name))
	return err == nil
}

// Remove deletes a machine's conf file (idempotent).
func Remove(configDir, name string) error {
	err := os.Remove(confPath(configDir, name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// List returns the names of all registered machines, sorted by the filesystem.
func List(configDir string) ([]string, error) {
	entries, err := os.ReadDir(MachinesDir(configDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if n, ok := strings.CutSuffix(e.Name(), ".toml"); ok {
			names = append(names, n)
		}
	}
	return names, nil
}

// SplitPort parses a "HOST:GUEST" or bare "PORT" mapping into its host
// preference and guest port, mirroring split_port in bin/devvm.
func SplitPort(mapping string) (host, guest string) {
	if h, g, ok := strings.Cut(mapping, ":"); ok {
		return h, g
	}
	return mapping, mapping
}

// HasPort reports whether the exact mapping string is already configured.
func (m *Machine) HasPort(mapping string) bool {
	for _, p := range m.Ports {
		if p == mapping {
			return true
		}
	}
	return false
}
