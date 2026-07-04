package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Defaults are user-level create-time defaults, read from
// $XDG_CONFIG_HOME/devvm/config.toml. They sit between command-line flags and
// the compiled-in defaults when `create` resolves an unset field
// (flag > config.toml > built-in). They are consulted only at create time; a
// machine's own conf is the source of truth afterward, so editing this file
// never changes what an existing box does.
//
// Only fields worth pinning globally live here; grow it key-by-key. Keep the
// key list in sync with DefaultKeys / the Get/Set switches below.
type Defaults struct {
	Provision string `toml:"provision,omitempty"`
	Memory    int    `toml:"memory,omitempty"` // MiB
	Transport string `toml:"transport,omitempty"`
}

// DefaultKeys are the settable keys, in display order. Drives `defaults list`
// and shell completion, so it is the single source of truth for what exists.
var DefaultKeys = []string{"provision", "memory", "transport"}

// DefaultKeyHelp is a one-line description of a key for completion and help.
func DefaultKeyHelp(key string) string {
	switch key {
	case "provision":
		return "provisioner spec: url:<URL> [args] | cmd:<path> [args] | none"
	case "memory":
		return "smol VM memory in MiB (>= 512)"
	case "transport":
		return "remote interactive transport: ssh | mosh"
	default:
		return ""
	}
}

// defaultsPath is the global config file (sibling to the machines/ dir).
func defaultsPath(configDir string) string {
	return filepath.Join(configDir, "config.toml")
}

// DefaultsPath is the public path to the global config file (for `defaults path`).
func DefaultsPath(configDir string) string { return defaultsPath(configDir) }

// LoadDefaults reads config.toml. A missing file yields zero Defaults (every
// compiled default applies); a malformed or invalid file is an error.
func LoadDefaults(configDir string) (*Defaults, error) {
	p := defaultsPath(configDir)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &Defaults{}, nil
		}
		return nil, err
	}
	var d Defaults
	if err := toml.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &d, nil
}

// SaveDefaults writes config.toml as commented, hand-editable TOML. Zero-valued
// ints (memory = 0) are stripped so unset keys don't linger as noise.
func SaveDefaults(configDir string, d *Defaults) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	var body strings.Builder
	if err := toml.NewEncoder(&body).Encode(d); err != nil {
		return err
	}
	clean := zeroIntLine.ReplaceAllString(body.String(), "")
	content := "# devvm global defaults (tool-managed; edit freely).\n" +
		"# These seed unset `create` fields: flag > this file > built-in default.\n\n" + clean
	return os.WriteFile(defaultsPath(configDir), []byte(content), 0o644)
}

// Validate enforces the same field invariants create would, so a bad config.toml
// (or `defaults set`) fails loudly instead of surfacing at bootstrap. Provision
// specs are validated by the caller (internal/provision, to avoid an import
// cycle) — see the defaults command.
func (d *Defaults) Validate() error {
	switch d.Transport {
	case "", TransportSSH, TransportMosh:
	default:
		return fmt.Errorf("invalid transport %q (want %q or %q)", d.Transport, TransportSSH, TransportMosh)
	}
	if d.Memory != 0 && d.Memory < 512 {
		return fmt.Errorf("memory %d too small (min 512 MiB)", d.Memory)
	}
	return nil
}

// Get returns the override for key as a display string, and whether it's set.
func (d *Defaults) Get(key string) (val string, set bool, err error) {
	switch key {
	case "provision":
		return d.Provision, d.Provision != "", nil
	case "memory":
		if d.Memory == 0 {
			return "", false, nil
		}
		return strconv.Itoa(d.Memory), true, nil
	case "transport":
		return d.Transport, d.Transport != "", nil
	default:
		return "", false, fmt.Errorf("unknown default %q", key)
	}
}

// Set applies value to key. Provision is stored verbatim — the caller validates
// the spec. A rejected value leaves d unchanged (it's applied to a trial copy
// that must pass Validate before it's committed).
func (d *Defaults) Set(key, value string) error {
	trial := *d
	switch key {
	case "provision":
		trial.Provision = value
	case "memory":
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("memory must be an integer number of MiB")
		}
		trial.Memory = n
	case "transport":
		trial.Transport = value
	default:
		return fmt.Errorf("unknown default %q (want one of: %s)", key, strings.Join(DefaultKeys, ", "))
	}
	if err := trial.Validate(); err != nil {
		return err
	}
	*d = trial
	return nil
}

// Unset clears key back to its compiled default.
func (d *Defaults) Unset(key string) error {
	switch key {
	case "provision":
		d.Provision = ""
	case "memory":
		d.Memory = 0
	case "transport":
		d.Transport = ""
	default:
		return fmt.Errorf("unknown default %q (want one of: %s)", key, strings.Join(DefaultKeys, ", "))
	}
	return nil
}
