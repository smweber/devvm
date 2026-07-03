package config

import (
	"os"
	"path/filepath"
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
