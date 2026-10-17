package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
)

// Saved forwards are host state, not config (config.md §5): which host port
// a guest port lands on depends on what else is bound on *this* host, so the
// list lives in <State>/ports/NAME, one mapping per line in the syntax
// `ports add` takes (`GUEST` or `HOST:GUEST`). A hub machine's file is named
// by its runtime form HUB@NAME, like its run files, since HUB/NAME would name
// a directory. Blank lines and `#` comments are skipped on read, so the file
// stays hand-editable.
//
// Only the CLI reads these files (`start`/`ports up` push one Add per line);
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
// none. The first call on a host seeds the directory from old confs
// (MigratePorts); a migration that cannot finish is this call's error.
func LoadPorts(p Paths, name string) ([]string, error) {
	if err := validPortsName(name); err != nil {
		return nil, err
	}
	if err := MigratePorts(p); err != nil {
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
// it (the old hub conf dropped an emptied table the same way); MigratePorts
// runs once per host, not per file, so the old conf's list never comes back.
// Atomic (temp + rename), so a concurrent reader sees the old list or the new
// one, never half of one; the file is 0600 in a 0700 dir, as host state.
// It never creates the ports dir itself: only a finished migration does, so
// a failed one can't be papered over by an unseeded dir.
// A read-modify-write holds LockPorts around it.
func SavePorts(p Paths, name string, ports []string) error {
	if err := validPortsName(name); err != nil {
		return err
	}
	if err := MigratePorts(p); err != nil {
		return err
	}
	if len(ports) == 0 {
		return removePorts(p, name)
	}
	return writePortsFile(p.PortsDir(), RuntimeName(name), name, ports)
}

// writePortsFile writes one ports file into an existing dir. A var so a test
// can make the migration's writes fail.
var writePortsFile = func(dir, file, display string, ports []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# devvm saved forwards for %s on this host: GUEST or HOST:GUEST per line\n", display)
	for _, port := range ports {
		b.WriteString(port + "\n")
	}
	return writeFileAtomic(filepath.Join(dir, file), []byte(b.String()), 0o600)
}

// RemovePorts drops a machine's saved forwards (idempotent). It migrates
// first like the other entry points: a removal on a host not yet migrated
// would otherwise be undone by the next read seeding the name from an old
// hub conf table.
func RemovePorts(p Paths, name string) error {
	if err := validPortsName(name); err != nil {
		return err
	}
	if err := MigratePorts(p); err != nil {
		return err
	}
	return removePorts(p, name)
}

func removePorts(p Paths, name string) error {
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
	if err := MigratePorts(p); err != nil {
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

// legacyConfNames lists the old per-machine confs, machines/NAME.toml.
func legacyConfNames(p Paths) ([]string, error) {
	entries, err := os.ReadDir(LegacyMachinesDir(p))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".toml"); ok && !e.IsDir() && ValidName(n) == nil {
			names = append(names, n)
		}
	}
	return names, nil
}

// LegacySmolConfs names every old per-machine conf with `backend = "smol"`
// (decoded leniently, like MigratePorts): the VMs a devvm from before the
// managed markers made, which the marker seed (backend.SeedLegacyMarkers)
// marks so they stay managed across the upgrade. A conf that does not
// parse is skipped: this is a best-effort seed, not the all-or-nothing
// forwards one.
func LegacySmolConfs(p Paths) []string {
	names, _ := legacyConfNames(p)
	var out []string
	for _, n := range names {
		var c legacyConf
		if _, err := toml.DecodeFile(filepath.Join(LegacyMachinesDir(p), n+".toml"), &c); err == nil && c.Backend == BackendSmol {
			out = append(out, n)
		}
	}
	return out
}

// SavedPortsNames returns the display name of every machine with a saved
// forwards file, so enumeration (listMachines, `delete HUB`, `create HUB`)
// sees a hub machine this host has forwards for even when the hub's listing
// does not.
func SavedPortsNames(p Paths) ([]string, error) {
	if err := MigratePorts(p); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p.PortsDir())
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

// legacyConf is just the fields an old conf kept saved forwards in, decoded
// with no validation: a conf the old loader refused (an unknown backend, a
// hub carrying ports) still had a list worth keeping, and the migration
// runs only once.
type legacyConf struct {
	Backend  string   `toml:"backend"`
	Ports    []string `toml:"ports"`
	Machines map[string]struct {
		Ports []string `toml:"ports"`
	} `toml:"machines"`
}

// MigratePorts is the one-time move of saved forwards out of config: when
// <State>/ports does not exist yet, it is created holding a file for every
// old per-machine conf (machines/*.toml, the layout before devvm.toml) with
// `ports` and every old hub conf `[machines.NAME]` table with `ports`. It
// outlives the old loader on purpose: it reads those two keys and nothing
// else, and it spares the hand migration (config.md §8) copying forwards
// on a host whose ports dir does not exist yet. With no machines/ it just
// creates the empty dir.
// The confs are left untouched (VM commands never write config) and their
// `ports` are never read again. Doing it once for the whole directory, not
// per machine on first read, is what keeps a dropped list dropped: `delete
// HUB/NAME` removes the file but leaves the hub conf's old table, and a
// per-name seed would bring it straight back (as would `ports rm` of the
// last mapping, which removes the file too).
//
// Because it runs once, it is all or nothing: the directory is built under
// a temp name and renamed into place only when every conf was read and
// every file written. Any failure (an unreadable machines dir, a conf that
// is not TOML, a failed write) returns an error with no ports dir created,
// so every ports command fails until it is fixed and the next call retries;
// nothing is ever seeded partly. Two racing first runs both build the same
// tree: rename(2) replaces a destination dir only while it is empty, so the
// loser's rename either fails (the winner's dir already holds a file, which
// may be a `ports add` that must not be overwritten) or replaces an empty
// dir with identical content. Either way the result is a seeded dir.
func MigratePorts(p Paths) error {
	dir := p.PortsDir()
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	names, err := legacyConfNames(p)
	if err != nil {
		return fmt.Errorf("migrate saved forwards: %w", err)
	}
	if err := os.MkdirAll(p.State, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(p.State, ".ports-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // a no-op once renamed
	if err := os.Chmod(tmp, 0o700); err != nil {
		return err
	}
	for _, n := range names {
		var c legacyConf
		path := filepath.Join(LegacyMachinesDir(p), n+".toml")
		if _, err := toml.DecodeFile(path, &c); err != nil {
			return fmt.Errorf("migrate saved forwards: parse %s: %w", path, err)
		}
		if c.Backend != BackendHub && len(c.Ports) > 0 {
			if err := writePortsFile(tmp, n, n, c.Ports); err != nil {
				return fmt.Errorf("migrate saved forwards for %s: %w", n, err)
			}
		}
		if c.Backend != BackendHub {
			continue // tables on a non-hub never named a reachable machine
		}
		for mn, hm := range c.Machines {
			if len(hm.Ports) == 0 || ValidName(mn) != nil {
				continue
			}
			display := JoinHubName(n, mn)
			if err := writePortsFile(tmp, RuntimeName(display), display, hm.Ports); err != nil {
				return fmt.Errorf("migrate saved forwards for %s: %w", display, err)
			}
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		if _, serr := os.Stat(dir); serr == nil {
			return nil // a racing first run got there first
		}
		return fmt.Errorf("migrate saved forwards: %w", err)
	}
	return nil
}
