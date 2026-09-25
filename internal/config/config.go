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
	"syscall"

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
//	hub              another host running devvm whose machines are reached as
//	                 HUB/NAME (docs/proposals/hub.md). Reached over the ssh
//	                 transport like a remote box, but never shaped: no
//	                 bootstrap, keys, repos, ports or agent land on it.
const (
	BackendSmol            = "smol"
	BackendRemoteManaged   = "remote-managed"
	BackendRemoteUnmanaged = "remote-unmanaged"
	BackendHub             = "hub"

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

// DefaultBootstrapHook is the built-in fallback bootstrap-hook: do nothing. A
// box's own hook is chosen at create time (flag > global config.toml > this) and
// written concretely into its conf, so nothing personal is baked into the binary.
// Users who want a default (e.g. their dotfiles bootstrap) set `bootstrap-hook` in
// config.toml — see Defaults.
const DefaultBootstrapHook = "none"

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

// HubMachine is the laptop-side state for one machine on a hub: the
// `[machines.NAME]` table in the hub's conf. Ports (this host's own forwards,
// inherently per client) is the only field; everything else about the machine
// lives in the hub's registry, which is the single source of truth.
type HubMachine struct {
	Ports []string `toml:"ports,omitempty"` // "HOST:GUEST" or bare "PORT"
}

// Machine is one registered dev box. Zero values mean "unset"; applyDefaults
// fills the same defaults load_machine did (SSHPort 22, BootstrapHook).
type Machine struct {
	// Name is derived from the filename, not stored in the file. For a hub
	// machine it is the display form HUB/NAME (see hubname.go).
	Name string `toml:"-"`

	// Hub is set only on a hub-machine record (LoadHubMachine): the loaded conf
	// of the hub the machine lives on. Such a record is synthesized, never
	// written — the hub's [machines.NAME] table is the only thing on disk.
	Hub *Machine `toml:"-"`

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

	// smol backend. Memory (MiB) and Disk (GiB) are a provisioning spec: read at
	// create and (re)provision, but the live VM is the source of truth once it
	// exists — `status` reports the VM's actual sizes, and editing these here only
	// takes effect on the next provision (e.g. after a deprovision).
	Memory int `toml:"memory,omitempty"` // MiB
	Disk   int `toml:"disk,omitempty"`   // GiB

	// shared
	Repos         []string `toml:"repos,omitempty"`
	Ports         []string `toml:"ports,omitempty"` // "HOST:GUEST" or bare "PORT"
	BootstrapHook string   `toml:"bootstrap_hook,omitempty"`

	// ssh key seeding / hardening
	AuthorizedKeys       []string `toml:"authorized_keys,omitempty"`
	AuthorizedKeysGithub []string `toml:"authorized_keys_github,omitempty"`
	Harden               bool     `toml:"harden,omitempty"`
	Fail2ban             bool     `toml:"fail2ban,omitempty"`

	// hub backend only: one table per machine this host has laptop-side state
	// for, keyed by the machine's name on the hub. Kept in the hub's own conf
	// (no second registry tree) so `delete HUB` is one file and List stays a
	// plain directory read.
	Machines map[string]HubMachine `toml:"machines,omitempty"`
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

// NewHub returns a defaulted hub conf for the given ssh destination.
func NewHub(name, sshHost string) *Machine {
	m := &Machine{Name: name, Backend: BackendHub, SSHHost: sshHost}
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
// meaningless ssh_port/transport. Save strips the default-valued bootstrap_hook
// and transport lines back out, so a conf shows only what deviates from stock —
// but Load always has a concrete value in hand.
func (m *Machine) applyDefaults() {
	if m.BootstrapHook == "" {
		m.BootstrapHook = DefaultBootstrapHook
	}
	if m.IsRemote() {
		if m.SSHPort == 0 {
			m.SSHPort = 22
		}
		if m.Transport == "" {
			m.Transport = TransportSSH
		}
	}
}

// Validate enforces the invariants load_machine checked at source time.
func (m *Machine) Validate() error {
	switch m.Backend {
	case BackendSmol, BackendRemoteManaged, BackendRemoteUnmanaged, BackendHub:
	case "":
		return fmt.Errorf("machine %q has no backend set", m.Name)
	default:
		return fmt.Errorf("machine %q has unsupported backend %q", m.Name, m.Backend)
	}
	if m.IsRemote() && m.SSHHost == "" {
		return fmt.Errorf("remote machine %q needs ssh_host", m.Name)
	}
	if m.IsHub() {
		if err := m.validateHub(); err != nil {
			return err
		}
	} else if len(m.Machines) > 0 {
		return fmt.Errorf("machine %q: [machines.*] tables only apply to a hub", m.Name)
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

// validateHub rejects the fields that would shape the hub itself. A hub is
// reached like a remote box but is never bootstrapped, forwarded to, or given
// repos/keys — those verbs refuse it — so a conf carrying them is a mistake
// (most likely a remote conf whose backend was flipped by hand) rather than
// something to silently ignore. transport/mosh_server stay allowed: they steer
// the interactive hop to the hub, which is what a proxied attach rides.
func (m *Machine) validateHub() error {
	// transport (and mosh_server) are deliberately not on this list: they steer
	// the interactive hop to the hub, which a proxied attach/shell rides.
	var bad []string
	if len(m.Ports) > 0 {
		bad = append(bad, "ports")
	}
	if m.Memory != 0 {
		bad = append(bad, "memory")
	}
	if m.Disk != 0 {
		bad = append(bad, "disk")
	}
	if len(m.Repos) > 0 {
		bad = append(bad, "repos")
	}
	if m.BootstrapHook != "" && m.BootstrapHook != DefaultBootstrapHook {
		bad = append(bad, "bootstrap_hook")
	}
	if len(m.AuthorizedKeys) > 0 {
		bad = append(bad, "authorized_keys")
	}
	if len(m.AuthorizedKeysGithub) > 0 {
		bad = append(bad, "authorized_keys_github")
	}
	if m.Harden {
		bad = append(bad, "harden")
	}
	if m.Fail2ban {
		bad = append(bad, "fail2ban")
	}
	if len(bad) > 0 {
		return fmt.Errorf("hub %q is not a machine: it cannot carry %s (forwards and setup belong to the machines on it, as [machines.NAME] tables)",
			m.Name, strings.Join(bad, ", "))
	}
	for name := range m.Machines {
		if err := ValidName(name); err != nil {
			return fmt.Errorf("hub %q: [machines.%s]: %w", m.Name, name, err)
		}
	}
	return nil
}

// IsHub reports whether this conf registers a hub (another host's devvm) rather
// than a machine. A hub-machine record (Hub != nil) is not a hub.
func (m *Machine) IsHub() bool {
	return m.Backend == BackendHub && m.Hub == nil
}

// IsHubMachine reports whether this is a synthesized HUB/NAME record.
func (m *Machine) IsHubMachine() bool { return m.Hub != nil }

// Managed reports whether devvm owns this box's OS and lifecycle — it installs
// prereqs, may harden, and pins known_hosts. smol and remote-managed are managed;
// adopted remote-unmanaged hosts are not (devvm only checks them, never modifies).
func (m *Machine) Managed() bool {
	return m.Backend == BackendSmol || m.Backend == BackendRemoteManaged
}

// IsRemote reports whether the box is reached over the ssh transport (both
// remote-* backends, and a hub — its ssh fields drive the hop to it). Drives
// ssh flags, key management, mosh, and forwards. A hub-machine record carries
// the hub's ssh fields for the same reason, so it counts too.
func (m *Machine) IsRemote() bool {
	return m.Backend == BackendRemoteManaged || m.Backend == BackendRemoteUnmanaged || m.Backend == BackendHub
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
	if m.IsHubMachine() {
		// The record is derived from the hub's conf; the only persistent part
		// is its [machines.NAME] table, which callers edit on the hub conf.
		return fmt.Errorf("%s is a machine on hub %s; save the hub's conf instead", m.Name, m.Hub.Name)
	}
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
	// Both patterns are anchored at column 0: the encoder indents the keys of
	// nested [machines.NAME] tables, so those lines are never touched.
	clean := zeroIntLine.ReplaceAllString(body.String(), "")
	// Drop default-valued string lines too: applyDefaults always fills these, so
	// omitempty can't, but an omitted line reloads to the exact same value. So a
	// conf carries bootstrap_hook/transport only when they deviate from stock.
	clean = defaultStrLine.ReplaceAllString(clean, "")
	content := fmt.Sprintf("# devvm machine config for %q (tool-managed; edit freely)\n\n%s", m.Name, clean)
	return writeFileAtomic(confPath(configDir, m.Name), []byte(content), 0o644)
}

// writeFileAtomic writes via a temp file in the same directory and a rename,
// so a reader (notably `status --watch`, which re-snapshots on the write
// event) never sees a truncated or half-written conf. An existing file keeps
// its mode (a conf the user chmod'ed to 0600 stays that way); mode applies
// only to a new file. The data is fsync'ed before the rename so a crash can't
// leave a zero-length conf behind the new name (the directory entry itself is
// not fsync'ed: a power loss right after the rename may still show the old
// conf, which is a complete file either way).
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

// zeroIntLine matches a TOML scalar line whose int value is 0 (see Save).
var zeroIntLine = regexp.MustCompile(`(?m)^[a-z0-9_]+ = 0\n`)

// defaultStrLine matches the string lines whose value equals the built-in
// default, so Save omits them (Load re-fills via applyDefaults). See Save.
var defaultStrLine = regexp.MustCompile(`(?m)^(?:bootstrap_hook = "` + DefaultBootstrapHook + `"|transport = "` + TransportSSH + `")\n`)

// Exists reports whether a conf file is present for name.
func Exists(configDir, name string) bool {
	_, err := os.Stat(confPath(configDir, name))
	return err == nil
}

// Remove deletes a machine's conf file (idempotent).
func Remove(configDir, name string) error {
	// A hub's conf-edit lock goes with it (none for a machine). The conf is
	// removed while holding that lock and the lock file unlinked before it
	// is released, so a `ports add HUB/NAME` blocked on it wakes to find no
	// conf rather than re-creating one mid-delete.
	if lock, err := os.OpenFile(hubConfLockPath(configDir, name), os.O_RDWR, 0); err == nil {
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err == nil {
			defer os.Remove(hubConfLockPath(configDir, name))
		}
	}
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

// HasRepo reports whether the exact repo spec is already configured.
func (m *Machine) HasRepo(repo string) bool {
	for _, r := range m.Repos {
		if r == repo {
			return true
		}
	}
	return false
}
