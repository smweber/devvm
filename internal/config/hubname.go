package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Hub machines are named HUB/NAME everywhere a user sees them: the command
// line, status rows, --plain column 1, completion. nameRe forbids '/', so the
// form is unambiguous and a local `web` can never collide with `desktop/web`.
//
// Runtime files (the daemon's socket, log and lock under run/) cannot use that
// form — `run/desktop/web.sock` names a directory nothing creates — so they use
// HUB@NAME instead; '@' is outside nameRe too, so it cannot collide with a
// local name either. Both directions live here and nowhere else: session
// applies RuntimeName when it derives a path, listMachines applies
// DisplayName when it enumerates live sockets, and nothing else ever splits
// either form.
const (
	hubSep     = "/"
	runtimeSep = "@"
)

// SplitHubName parses HUB/NAME into its parts. ok is false for a bare name
// (no separator). Both parts are validated with ValidName so a malformed
// reference fails here rather than as a file path.
func SplitHubName(name string) (hub, machine string, ok bool, err error) {
	hub, machine, ok = strings.Cut(name, hubSep)
	if !ok {
		return "", "", false, nil
	}
	if err := ValidName(hub); err != nil {
		return "", "", true, fmt.Errorf("invalid hub name in %q: %w", name, err)
	}
	if err := ValidName(machine); err != nil {
		return "", "", true, fmt.Errorf("invalid machine name in %q: %w", name, err)
	}
	return hub, machine, true, nil
}

// JoinHubName renders the display form HUB/NAME.
func JoinHubName(hub, machine string) string { return hub + hubSep + machine }

// RuntimeName maps a display name to the identifier used for run/ files:
// HUB/NAME becomes HUB@NAME, a local name is returned unchanged.
func RuntimeName(display string) string {
	return strings.Replace(display, hubSep, runtimeSep, 1)
}

// DisplayName is the inverse of RuntimeName: HUB@NAME becomes HUB/NAME.
func DisplayName(runtime string) string {
	return strings.Replace(runtime, runtimeSep, hubSep, 1)
}

// LoadHubMachine returns the record for HUB/NAME: the hub's conf plus the
// machine's laptop-side state (its [machines.NAME] table, if any). It reads
// only the hub conf and never asks the hub whether the machine exists — the
// proxied command reports that itself, so a typo'd name costs an ssh timeout
// only when something is actually run against it, and `resolve` stays off the
// network. The record carries the hub's ssh fields so a Backend built from it
// can reach the hub; Backend is BackendHub and Hub points at the hub conf.
func LoadHubMachine(configDir, name string) (*Machine, error) {
	hubName, machine, ok, err := SplitHubName(name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%q is not a HUB/NAME reference", name)
	}
	hub, err := Load(configDir, hubName)
	if err != nil {
		return nil, err
	}
	if !hub.IsHub() {
		return nil, fmt.Errorf("%q is a %s machine, not a hub (so %q names nothing)", hubName, hub.Backend, name)
	}
	m := &Machine{
		Name:       name,
		Hub:        hub,
		Backend:    BackendHub,
		SSHHost:    hub.SSHHost,
		SSHPort:    hub.SSHPort,
		Identity:   hub.Identity,
		Transport:  hub.Transport,
		MoshServer: hub.MoshServer,
		Ports:      hub.Machines[machine].Ports,
	}
	m.applyDefaults()
	return m, nil
}

// LoadAny loads a local conf or, for a HUB/NAME reference, the hub-machine
// record. It is the file-level counterpart of the cli's resolve (which adds
// the unregistered-smol fallback and a backend).
func LoadAny(configDir, name string) (*Machine, error) {
	if _, _, ok, err := SplitHubName(name); err != nil {
		return nil, err
	} else if ok {
		return LoadHubMachine(configDir, name)
	}
	return Load(configDir, name)
}

// HubMachineName returns the machine's name on its hub (the part after the
// slash) for a hub-machine record.
func (m *Machine) HubMachineName() string {
	_, machine, _, _ := SplitHubName(m.Name)
	return machine
}

// hubConfLockPath is the lock serializing read-modify-writes of a hub's
// conf. It is a sibling file, not the conf itself: Save replaces the conf by
// rename, so a flock on the conf would be held on an inode the next writer
// never opens. A dotfile ending in .lock, so List (which reads *.toml)
// never sees it and `status --watch` skips it as noise.
func hubConfLockPath(configDir, hub string) string {
	return filepath.Join(MachinesDir(configDir), "."+hub+".toml.lock")
}

// UpdateHubMachine is the one read-modify-write of a hub conf's
// [machines.NAME] table (hub.md §2): under an exclusive flock it loads the
// hub conf fresh, hands fn the machine's table (the zero table if there is
// none), drops the table if fn leaves it empty, and saves. Every machine on
// a hub shares that one file, and Save is atomic but not serialized, so
// without the lock `ports add desktop/a` and `ports add desktop/b` racing
// would drop one of the two edits. fn returning an error aborts the write,
// and an fn that changes nothing writes nothing.
// The hub conf as saved is returned.
func UpdateHubMachine(configDir, hub, machine string, fn func(*HubMachine) error) (*Machine, error) {
	if err := ValidName(hub); err != nil {
		return nil, err
	}
	if err := ValidName(machine); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(MachinesDir(configDir), 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(hubConfLockPath(configDir, hub), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close() // releases the flock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("lock hub conf %s: %w", hub, err)
	}
	h, err := Load(configDir, hub)
	if err != nil {
		return nil, err
	}
	if !h.IsHub() {
		return nil, fmt.Errorf("%q is a %s machine, not a hub", hub, h.Backend)
	}
	hm := h.Machines[machine]
	before := slices.Clone(hm.Ports)
	if err := fn(&hm); err != nil {
		return nil, err
	}
	if slices.Equal(before, hm.Ports) {
		// Nothing changed: no write, so `status --watch` is not woken for
		// nothing (a repeat `ports add` of a recorded mapping).
		return h, nil
	}
	if len(hm.Ports) == 0 {
		delete(h.Machines, machine)
	} else {
		if h.Machines == nil {
			h.Machines = map[string]HubMachine{}
		}
		h.Machines[machine] = hm
	}
	if len(h.Machines) == 0 {
		h.Machines = nil
	}
	if err := h.Save(configDir); err != nil {
		return nil, err
	}
	return h, nil
}
