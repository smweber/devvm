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

// Backend identifies who owns a machine and how it's reached.
//
//	smol             local microVM devvm creates and shapes (smolvm-exec)
//	remote-managed   a remote host devvm shapes (installs prereqs, may harden),
//	                 reached over the ssh transport — you provision the box, devvm
//	                 manages the OS. (Future: hetzner adds API-backed lifecycle.)
//	remote-unmanaged an existing host devvm adopts hands-off (checks prereqs, never
//	                 modifies the OS), reached over the ssh transport.
const (
	BackendSmol            = "smol"
	BackendRemoteManaged   = "remote-managed"
	BackendRemoteUnmanaged = "remote-unmanaged"

	// legacyBackendSSH is the pre-rename backend value; migrated on load onto the
	// remote-managed / remote-unmanaged split (see migrateLegacy).
	legacyBackendSSH = "ssh"
)

// Transport selects how an interactive connection (shell/attach) is made to a
// remote backend; it does not affect forwards (always native ssh -L) or exec.
const (
	TransportSSH  = "ssh"
	TransportMosh = "mosh"
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

	Backend string `toml:"backend"`

	// Unmanaged is the deprecated pre-rename flag; still read so old confs migrate
	// (see migrateLegacy) but never written back — the backend value carries this
	// now (remote-unmanaged vs remote-managed).
	Unmanaged bool `toml:"unmanaged,omitempty"`

	// remote backends (ssh transport)
	SSHHost    string `toml:"ssh_host,omitempty"`
	SSHPort    int    `toml:"ssh_port,omitempty"`
	Identity   string `toml:"identity,omitempty"`
	Transport  string `toml:"transport,omitempty"` // ssh (default) | mosh
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

// NewRemote returns a defaulted remote machine (managed or unmanaged) for the
// given ssh host. Callers set optional fields (identity, transport, ...) after.
func NewRemote(name, backend, sshHost string) *Machine {
	m := &Machine{Name: name, Backend: backend, SSHHost: sshHost}
	m.applyDefaults()
	return m
}

// migrateLegacy rewrites the pre-rename `backend = "ssh"` (+ optional
// `unmanaged`) form onto the remote-managed / remote-unmanaged split, so old
// hand-written confs (and shared dotfiles) keep loading unchanged. The next Save
// drops the `unmanaged` key.
func (m *Machine) migrateLegacy() {
	if m.Backend == legacyBackendSSH {
		if m.Unmanaged {
			m.Backend = BackendRemoteUnmanaged
		} else {
			m.Backend = BackendRemoteManaged
		}
	}
	m.Unmanaged = false
}

// applyDefaults fills unset fields, matching load_machine's defaulting. The ssh
// transport defaults apply only to remote backends, so a smol conf doesn't carry
// meaningless ssh_port/vnc_port/transport.
func (m *Machine) applyDefaults() {
	if m.Provision == "" {
		m.Provision = DefaultProvision
	}
	if m.IsRemote() {
		if m.SSHPort == 0 {
			m.SSHPort = 22
		}
		if m.VNCPort == 0 {
			m.VNCPort = 5901
		}
		if m.Transport == "" {
			m.Transport = TransportSSH
		}
	}
}

// Validate enforces the invariants load_machine checked at source time.
func (m *Machine) Validate() error {
	switch m.Backend {
	case BackendSmol, BackendRemoteManaged, BackendRemoteUnmanaged:
	case "":
		return fmt.Errorf("machine %q has no backend set", m.Name)
	default:
		return fmt.Errorf("machine %q has unsupported backend %q", m.Name, m.Backend)
	}
	if m.IsRemote() && m.SSHHost == "" {
		return fmt.Errorf("remote machine %q needs ssh_host", m.Name)
	}
	if !m.IsRemote() && m.Transport != "" {
		return fmt.Errorf("machine %q: transport only applies to remote backends", m.Name)
	}
	switch m.Transport {
	case "", TransportSSH, TransportMosh:
	default:
		return fmt.Errorf("machine %q: invalid transport %q (want %q or %q)",
			m.Name, m.Transport, TransportSSH, TransportMosh)
	}
	return nil
}

// Managed reports whether devvm owns this box's OS and lifecycle — it installs
// prereqs, may harden, and pins known_hosts. smol and remote-managed are managed;
// adopted remote-unmanaged hosts are not (devvm only checks them, never modifies).
func (m *Machine) Managed() bool {
	return m.Backend == BackendSmol || m.Backend == BackendRemoteManaged
}

// IsRemote reports whether the box is reached over the ssh transport (both
// remote-* backends). Drives ssh flags, key management, mosh/vnc, and forwards.
func (m *Machine) IsRemote() bool {
	return m.Backend == BackendRemoteManaged || m.Backend == BackendRemoteUnmanaged
}

// TransportName is the effective interactive transport (defaulted to ssh).
func (m *Machine) TransportName() string {
	if m.Transport == "" {
		return TransportSSH
	}
	return m.Transport
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
	m.migrateLegacy()
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
	var body strings.Builder
	if err := toml.NewEncoder(&body).Encode(m); err != nil {
		return err
	}
	// BurntSushi keeps zero-valued ints even with omitempty, which would litter a
	// conf with `memory = 0` (remote) or `ssh_port = 0` (smol). None of our int
	// fields mean anything at 0 (Load re-defaults them), so drop those lines.
	clean := zeroIntLine.ReplaceAllString(body.String(), "")
	content := fmt.Sprintf("# devvm machine config for %q (tool-managed; edit freely)\n\n%s", m.Name, clean)
	return os.WriteFile(confPath(configDir, m.Name), []byte(content), 0o644)
}

// zeroIntLine matches a TOML scalar line whose int value is 0 (see Save).
var zeroIntLine = regexp.MustCompile(`(?m)^[a-z0-9_]+ = 0\n`)

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
