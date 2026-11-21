package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Saved forwards are host state, not config (config.md §5): which host port
// a guest port lands on depends on what else is bound on *this* host, so the
// list lives in <State>/ports/NAME, one mapping per line in the syntax
// `ports add` takes (`GUEST` or `HOST:GUEST`). A hub machine's file is named
// by its runtime form HUB@NAME, like its run files, since HUB/NAME would name
// a directory. Blank lines and `#` comments are skipped on read, so the file
// stays hand-editable.
//
// Only the CLI reads these files (`start`, `ports up` and a connecting
// `attach`/`shell` push one Add per line);
// the session daemon never does. It keeps its forwards in memory and gets
// them from its clients.

// PortsDir holds one saved-forwards file per machine.
func (p Paths) PortsDir() string {
	return filepath.Join(p.State, "ports")
}

// PortsPath is the saved-forwards file for a machine, by display name
// (HUB/NAME maps to HUB@NAME).
func (p Paths) PortsPath(name string) string {
	return filepath.Join(p.PortsDir(), RuntimeName(name))
}

// validPortsName refuses anything that would escape PortsDir: a bare name or
// HUB/NAME, each part a ValidName.
func validPortsName(name string) error {
	if _, _, ok, err := SplitHubName(name); err != nil {
		return err
	} else if ok {
		return nil
	}
	return ValidName(name)
}

// LoadPorts returns a machine's saved forwards in file order; no file is
// none.
func LoadPorts(p Paths, name string) ([]string, error) {
	if err := validPortsName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p.PortsPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parsePorts(string(data)), nil
}

func parsePorts(s string) []string {
	var ports []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ports = append(ports, line)
	}
	return ports
}

// SavePorts replaces a machine's saved forwards. An empty list removes the
// file, so a hub machine whose last forward went is no longer enumerated for
// it. Atomic (temp + rename), so a concurrent reader sees the old list or the
// new one, never half of one; the file is 0600 in a 0700 dir, as host state.
// A read-modify-write holds LockPorts around it.
func SavePorts(p Paths, name string, ports []string) error {
	if err := validPortsName(name); err != nil {
		return err
	}
	if len(ports) == 0 {
		return RemovePorts(p, name)
	}
	if err := ensurePortsDir(p); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# devvm saved forwards for %s on this host: GUEST or HOST:GUEST per line\n", name)
	for _, port := range ports {
		b.WriteString(port + "\n")
	}
	return writeFileAtomic(p.PortsPath(name), []byte(b.String()), 0o600)
}

// ensurePortsDir creates the ports dir owner-only, as host state.
func ensurePortsDir(p Paths) error {
	return os.MkdirAll(p.PortsDir(), 0o700)
}

// RemovePorts drops a machine's saved forwards (idempotent).
func RemovePorts(p Paths, name string) error {
	if err := validPortsName(name); err != nil {
		return err
	}
	err := os.Remove(p.PortsPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// LockPorts takes an exclusive flock serializing read-modify-writes of one
// machine's ports file (`ports add X 3000` racing `ports add X 4000` would
// otherwise drop one) and its removal by `delete`. The lock is a sibling
// dotfile, .NAME.lock, not the ports file: SavePorts replaces that by
// rename, so a flock on it would be held on an inode the next writer never
// opens. Lock files are never unlinked (a waiter would hold a lock nobody
// else sees); SavedPortsNames skips them. Call the returned func to release.
func LockPorts(p Paths, name string) (unlock func(), err error) {
	if err := validPortsName(name); err != nil {
		return nil, err
	}
	if err := ensurePortsDir(p); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(p.PortsDir(), "."+RuntimeName(name)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock saved forwards for %s: %w", name, err)
	}
	return func() { f.Close() }, nil // closing releases the flock
}

// SavedPortsNames returns the display name of every machine with a saved
// forwards file, so enumeration (listMachines, `delete HUB`, `create HUB`)
// sees a hub machine this host has forwards for even when the hub's listing
// does not.
func SavedPortsNames(p Paths) ([]string, error) {
	entries, err := os.ReadDir(p.PortsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // lock files, and temp files from writeFileAtomic
		}
		display := DisplayName(e.Name())
		if validPortsName(display) == nil {
			names = append(names, display)
		}
	}
	return names, nil
}
