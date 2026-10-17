package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Paths is where devvm keeps its files on this host, split by who owns them
// (config.md §1). Config is the user's: machines/ and config.toml, hand-edited
// and safe to keep in dotfiles. State is this host's own: daemon run files,
// the hub listing cache, the pinned known_hosts and the ssh ControlMaster
// sockets, none of which may follow a dotfiles checkout to another host. The
// helpers below are methods so a config dir can't be passed where state is
// meant: that mix-up no longer compiles.
type Paths struct {
	Config string
	State  string
}

// DefaultPaths is $XDG_CONFIG_HOME/devvm plus $XDG_STATE_HOME/devvm.
func DefaultPaths() Paths {
	return Paths{Config: DefaultConfigDir(), State: DefaultStateDir()}
}

// LegacyPaths is the layout before the split, when every state file lived in
// the config dir: still what a non-default --config-dir without --state-dir
// means (one self-contained instance, as before), and where MigrateState
// looks for state to move.
func LegacyPaths(configDir string) Paths {
	return Paths{Config: configDir, State: configDir}
}

// DefaultConfigDir resolves $XDG_CONFIG_HOME/devvm (or ~/.config/devvm),
// matching CONFIG_DIR in bin/devvm.
func DefaultConfigDir() string {
	return xdgDir("XDG_CONFIG_HOME", ".config")
}

// DefaultStateDir resolves $XDG_STATE_HOME/devvm (or ~/.local/state/devvm).
func DefaultStateDir() string {
	return xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state"))
}

func xdgDir(env, fallback string) string {
	if x := os.Getenv(env); x != "" {
		return filepath.Join(x, "devvm")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(fallback, "devvm")
	}
	return filepath.Join(home, fallback, "devvm")
}

// KnownHostsPath is the isolated, TOFU-pinned known_hosts file for managed ssh
// machines (the old KNOWN_HOSTS). Host state: a pin is this host's trust
// decision.
func (p Paths) KnownHostsPath() string {
	return filepath.Join(p.State, "known_hosts")
}

// RuntimeDir holds per-machine daemon state (control sockets, forward records).
func (p Paths) RuntimeDir() string {
	return filepath.Join(p.State, "run")
}

// ExpandHome expands a leading ~/ to the user's home directory.
func ExpandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// EnsureRuntimeDir creates the runtime dir owner-only. The control sockets in
// it drive forwards into guests, so don't rely on the umask to keep other
// local users out; Chmod also tightens a dir created 0755 by older builds.
func (p Paths) EnsureRuntimeDir() error {
	dir := p.RuntimeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// ChangedPath is the change marker under the runtime dir: `status --watch`
// watches it so state changes that leave no other trace on disk (a smol VM
// starting or stopping, a daemon flipping to reconnecting) still wake it.
func (p Paths) ChangedPath() string {
	return filepath.Join(p.RuntimeDir(), "changed")
}

// TouchChanged rewrites the change marker. Best-effort: a missing runtime dir
// or a read-only state dir is never a reason to fail the command that changed
// state. The file is truncated and written (not just utimes-touched) because
// kqueue reports a directory watch's entries on write, not on attribute change,
// so a bare touch would go unseen on macOS.
func TouchChanged(p Paths) {
	_ = p.EnsureRuntimeDir()
	_ = os.WriteFile(p.ChangedPath(), []byte(time.Now().Format(time.RFC3339Nano)+"\n"), 0o600)
}

// RefreshPath is the refresh request marker under the runtime dir
// (providers.md §5): `devvm __refresh` writes it, and a `status --watch`
// that sees it runs one live round (probe every remote box) instead of the
// cache-only snapshot every other event gets. Nothing reads its content.
func (p Paths) RefreshPath() string {
	return filepath.Join(p.RuntimeDir(), "refresh")
}

// RequestRefresh writes the refresh marker, as TouchChanged writes the
// change marker (and for the same kqueue reason, a write, not a touch).
func RequestRefresh(p Paths) error {
	if err := p.EnsureRuntimeDir(); err != nil {
		return err
	}
	return os.WriteFile(p.RefreshPath(), []byte(time.Now().Format(time.RFC3339Nano)+"\n"), 0o600)
}

// CacheDir holds derived data that is safe to lose: per-hub listing caches
// (`hub-HUB.list`, roadmap step 3) and the remote boxes' reach
// (`reach/NAME`, package reach). It is deliberately outside run/ so
// `status --watch`, which watches run/ (and machines/), never sees its own
// cache write and re-triggers.
func (p Paths) CacheDir() string {
	return filepath.Join(p.State, "cache")
}

// ControlPath is the ssh ControlMaster socket pattern for interactive and
// one-shot ssh calls (ssh expands %C to a 40-hex hash of the connection),
// host state like the rest: a master socket names a live process on this
// host. The name is kept short (c-, not the older cm-) because sun_path is
// 104 bytes on macOS and ssh binds a temp name ~17 bytes longer first:
// /Users/<name>/.local/state/devvm/c-<40> leaves room for long user names.
func (p Paths) ControlPath() string {
	return filepath.Join(p.State, "c-%C")
}
