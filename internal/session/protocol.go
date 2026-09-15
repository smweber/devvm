// Package session is the host-side per-machine forward daemon and its client.
// One daemon owns the single agent exec (smol) or ControlMaster (ssh) for a
// machine's lifetime; the devvm CLI is a thin client that talks to it over a
// unix socket and exits. This replaces every nohup/pidfile/autossh/url-watcher
// mechanism from the old script with one supervised process per machine.
package session

import (
	"path/filepath"
	"time"

	"github.com/smweber/devvm/internal/config"
)

// Request is a control message from the CLI to the daemon (one JSON line).
type Request struct {
	Op    string `json:"op"`              // add | remove | list | ping | kick | stop
	Host  int    `json:"host,omitempty"`  // preferred host port (add)
	Guest int    `json:"guest,omitempty"` // guest port (add/remove)
}

// Forward is one forward the daemon owns: the actual host port and the guest
// port it maps. Pending means the transport is down and it will be re-bound on
// reconnect (the host port is the one it had, or the preferred one if added
// during the outage).
type Forward struct {
	Host    int  `json:"host"`
	Guest   int  `json:"guest"`
	Pending bool `json:"pending,omitempty"`
}

// Daemon states, reported on list/ping.
const (
	StateUp           = "up"
	StateReconnecting = "reconnecting"
)

// Response is the daemon's reply (one JSON line).
type Response struct {
	OK       bool      `json:"ok"`
	Err      string    `json:"err,omitempty"`
	Host     int       `json:"host,omitempty"`    // actual host port after any bump (add)
	Bumped   bool      `json:"bumped,omitempty"`  // preferred port was taken
	Pending  bool      `json:"pending,omitempty"` // recorded during an outage, not yet bound (add)
	State    string    `json:"state,omitempty"`   // StateUp | StateReconnecting (list/ping)
	Since    time.Time `json:"since,omitempty"`   // when State began (list/ping)
	Version  string    `json:"version,omitempty"` // build the daemon runs (list/ping)
	Forwards []Forward `json:"forwards,omitempty"`
}

// Control op names.
const (
	OpAdd    = "add"
	OpRemove = "remove"
	OpList   = "list"
	OpPing   = "ping"
	OpStop   = "stop"
	OpKick   = "kick" // retry a reconnect now instead of waiting out the backoff
)

// Runtime files are keyed by the display name run through config.RuntimeName
// (HUB/NAME -> HUB@NAME); the three helpers below are the only place a name
// becomes a path, so the mapping is applied once and nowhere else.

// socketPath is the daemon's control socket for a machine.
func socketPath(configDir, name string) string {
	return filepath.Join(config.RuntimeDir(configDir), config.RuntimeName(name)+".sock")
}

// logPath is where a spawned daemon's stderr lands.
func logPath(configDir, name string) string {
	return filepath.Join(config.RuntimeDir(configDir), config.RuntimeName(name)+".log")
}

// lockPath is the startup lock serializing daemon creation for a machine.
func lockPath(configDir, name string) string {
	return filepath.Join(config.RuntimeDir(configDir), config.RuntimeName(name)+".lock")
}
