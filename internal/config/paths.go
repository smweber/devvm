package config

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultConfigDir resolves $XDG_CONFIG_HOME/devvm (or ~/.config/devvm),
// matching CONFIG_DIR in bin/devvm.
func DefaultConfigDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "devvm")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "devvm")
	}
	return filepath.Join(home, ".config", "devvm")
}

// KnownHostsPath is the isolated, TOFU-pinned known_hosts file for managed ssh
// machines (the old KNOWN_HOSTS).
func KnownHostsPath(configDir string) string {
	return filepath.Join(configDir, "known_hosts")
}

// RuntimeDir holds per-machine daemon state (control sockets, forward records).
func RuntimeDir(configDir string) string {
	return filepath.Join(configDir, "run")
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
func EnsureRuntimeDir(configDir string) error {
	dir := RuntimeDir(configDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}
