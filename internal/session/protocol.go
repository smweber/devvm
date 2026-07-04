// Package session is the host-side per-machine forward daemon and its client.
// One daemon owns the single agent exec (smol) or ControlMaster (ssh) for a
// machine's lifetime; the devvm CLI is a thin client that talks to it over a
// unix socket and exits. This replaces every nohup/pidfile/autossh/url-watcher
// mechanism from the old script with one supervised process per machine.
package session

import (
	"path/filepath"

	"github.com/smweber/devvm/internal/config"
)

// Request is a control message from the CLI to the daemon (one JSON line).
type Request struct {
	Op    string `json:"op"`              // add | remove | list | ping | stop
	Host  int    `json:"host,omitempty"`  // preferred host port (add)
	Guest int    `json:"guest,omitempty"` // guest port (add/remove)
}

// Forward is one live forward: the actual host port and the guest port it maps.
type Forward struct {
	Host  int `json:"host"`
	Guest int `json:"guest"`
}

// Response is the daemon's reply (one JSON line).
type Response struct {
	OK       bool      `json:"ok"`
	Err      string    `json:"err,omitempty"`
	Host     int       `json:"host,omitempty"`   // actual host port after any bump (add)
	Bumped   bool      `json:"bumped,omitempty"` // preferred port was taken
	Forwards []Forward `json:"forwards,omitempty"`
}

// Control op names.
const (
	OpAdd    = "add"
	OpRemove = "remove"
	OpList   = "list"
	OpPing   = "ping"
	OpStop   = "stop"
)

// socketPath is the daemon's control socket for a machine.
func socketPath(configDir, name string) string {
	return filepath.Join(config.RuntimeDir(configDir), name+".sock")
}

// logPath is where a spawned daemon's stderr lands.
func logPath(configDir, name string) string {
	return filepath.Join(config.RuntimeDir(configDir), name+".log")
}

// lockPath is the startup lock serializing daemon creation for a machine.
func lockPath(configDir, name string) string {
	return filepath.Join(config.RuntimeDir(configDir), name+".lock")
}
