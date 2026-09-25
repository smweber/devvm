// Package session is the host-side per-machine forward daemon and its client.
// One daemon owns the single agent exec (smol) or ControlMaster (ssh) for a
// machine's lifetime; the devvm CLI is a thin client that talks to it over a
// unix socket and exits. This replaces every nohup/pidfile/autossh/url-watcher
// mechanism from the old script with one supervised process per machine.
package session

import (
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/smweber/devvm/internal/config"
)

// Request is a control message from a client to the daemon (one JSON line).
//
// A connection is either one-shot (one request, one reply, closed: every CLI
// verb) or, when its first line is `session`, a long-lived session
// (browser-bridge.md §3): requests, their replies, events and event replies
// share the one pipe, so every request carries an ID its reply echoes and
// every event carries an ID the subscriber's reply names. A session line
// with Reply set is a subscriber's answer to an event, not a request.
type Request struct {
	ID    int64  `json:"id,omitempty"`    // echoed on the reply; required on a session
	Op    string `json:"op,omitempty"`    // add | remove | list | ping | kick | stop | down | session | subscribe | unsubscribe
	Host  int    `json:"host,omitempty"`  // preferred host port (add)
	Guest int    `json:"guest,omitempty"` // guest port (add/remove)
	// Exact (add): the host port may not bump, now or on any later restore;
	// a busy port fails the add instead. Sticky on the forward once set.
	Exact bool `json:"exact,omitempty"`
	// Owner (one-shot remove): which owner to drop, OwnerConf (the default)
	// or OwnerTTL. A connection owner is dropped only by its own session.
	Owner string `json:"owner,omitempty"`
	// Reply (session): the subscriber's answer to the event with this ID.
	Reply *EventReply `json:"reply,omitempty"`
	// Relay (session open only): the far end is another daemon relaying
	// through `__session` (hub.md §7), not a client on this host. Declared
	// once, at open, and kept for the session's life: a relay session is
	// refused while the transport is down and closed when it dies
	// (browser-bridge.md §3), and the bridge binds nothing for a relay
	// subscriber (roadmap step 7).
	Relay bool `json:"relay,omitempty"`
}

// Forward is one forward the daemon owns: the actual host port and the guest
// port it maps. Pending means it is not bound right now: the transport is
// down and it will be re-bound on reconnect (the host port is the one it
// had, or the preferred one if added during the outage), or it is exact and
// its port is taken, and the retry ticker re-binds it once the port frees.
type Forward struct {
	Host    int  `json:"host"`
	Guest   int  `json:"guest"`
	Pending bool `json:"pending,omitempty"`
	Exact   bool `json:"exact,omitempty"`
	// Owners are the owner kinds holding the forward (OwnerConf,
	// OwnerConnection, OwnerTTL), sorted and deduplicated. Empty only from
	// a daemon older than owners, whose forwards were all configured ones
	// (see IsConf).
	Owners []string `json:"owners,omitempty"`
}

// IsConf reports whether a conf owner holds the forward. A forward from a
// pre-owner daemon lists no owners and counts as configured: that is what
// every forward it held was, bar an old `auth` callback nobody counted
// separately either.
func (f Forward) IsConf() bool {
	if len(f.Owners) == 0 {
		return true
	}
	for _, o := range f.Owners {
		if o == OwnerConf {
			return true
		}
	}
	return false
}

// HasOwner reports whether an owner of this kind holds the forward.
func (f Forward) HasOwner(kind string) bool {
	for _, o := range f.Owners {
		if o == kind {
			return true
		}
	}
	return false
}

// Owner kinds (browser-bridge.md §4). Owners decide a forward's lifetime and
// nothing else; the forward is closed when the last one drops.
const (
	OwnerConf       = "conf"       // `ports add`/`ports up`; dropped by `ports rm`/`ports down`
	OwnerConnection = "connection" // a session connection's add; dropped when it closes
	OwnerTTL        = "ttl"        // the bridge's ephemeral callbacks; dropped at expiry
)

// Daemon states, reported on list/ping.
const (
	StateUp           = "up"
	StateReconnecting = "reconnecting"
)

// Response is the daemon's reply (one JSON line).
type Response struct {
	ID       int64     `json:"id,omitempty"` // the request's ID, echoed
	OK       bool      `json:"ok"`
	Err      string    `json:"err,omitempty"`
	Host     int       `json:"host,omitempty"`     // actual host port after any bump (add)
	Bumped   bool      `json:"bumped,omitempty"`   // preferred port was taken
	Pending  bool      `json:"pending,omitempty"`  // recorded during an outage, not yet bound (add)
	State    string    `json:"state,omitempty"`    // StateUp | StateReconnecting (list/ping/session)
	Since    time.Time `json:"since,omitempty"`    // when State began (list/ping)
	Version  string    `json:"version,omitempty"`  // build the daemon runs (list/ping/session)
	Sessions int       `json:"sessions,omitempty"` // open session connections (list/ping/down)
	// Relay (session open reply): the daemon admitted the session as a
	// relay. Echoed so the far side can tell a daemon that knows relays
	// from one older than hub forwards, which would ignore the request's
	// Relay and admit a plain local session without its rules.
	Relay    bool      `json:"relay,omitempty"`
	Stopped  bool      `json:"stopped,omitempty"`  // down: nothing else held the daemon, so it is exiting
	Forwards []Forward `json:"forwards,omitempty"` // list; remove/down: what survives
	// Event (session, daemon to client): a line carrying only this is an
	// event for a subscriber, not a reply; the subscriber answers with a
	// Request whose Reply names Event.ID.
	Event *Event `json:"event,omitempty"`
}

// Event is something the daemon hands its most recent subscriber. The
// payload is opaque here: the browser bridge (roadmap step 7) defines it.
type Event struct {
	ID   int64           `json:"id"`
	Data json.RawMessage `json:"data,omitempty"`
}

// EventReply is a subscriber's answer to one Event. A reply whose ID the
// daemon no longer waits on (timed out, or its subscriber is gone) is late
// and dropped.
type EventReply struct {
	ID   int64           `json:"id"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Control op names.
const (
	OpAdd    = "add"
	OpRemove = "remove"
	OpList   = "list"
	OpPing   = "ping"
	OpStop   = "stop"
	OpKick   = "kick" // retry a reconnect now instead of waiting out the backoff
	// OpDown drops every conf owner and stops the daemon only if no forward
	// and no session remains (`ports down`). OpStop still stops outright
	// (`devvm stop`, `delete`, `update`).
	OpDown = "down"
	// OpSession turns the connection into a long-lived session that holds
	// the daemon for as long as it stays open; add/remove on it are owned by
	// the connection. subscribe/unsubscribe are only valid on a session.
	OpSession     = "session"
	OpSubscribe   = "subscribe"
	OpUnsubscribe = "unsubscribe"
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
