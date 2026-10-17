package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The managed marker (config.md §2): <State>/managed/NAME, holding the smol
// VM's created_at as `smolvm machine ls --json` reports it. It is how a
// local smol VM devvm made (create, later `setup --adopt`) is told apart
// from one it merely discovered. It is host state, not config, because smol
// VMs are per host; and not in the guest, because reading it there would
// need an exec, and an exec may boot a stopped VM.
//
// These are file helpers only. The marker is *interpreted* by the smol
// discovery source (backend's smol source), which compares it with the
// VM's created_at and settles Managed on the record it produces; nothing
// else reads it to decide anything (§9 item 3: a provider source reads its
// label instead).

// ManagedDir holds one marker per managed local smol VM.
func (p Paths) ManagedDir() string {
	return filepath.Join(p.State, "managed")
}

// ManagedPath is the marker for a local machine name.
func (p Paths) ManagedPath(name string) string {
	return filepath.Join(p.ManagedDir(), name)
}

// ReadManagedMarker returns the created_at a marker holds; ok is false when
// there is no marker.
func ReadManagedMarker(p Paths, name string) (createdAt string, ok bool, err error) {
	if err := ValidName(name); err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(p.ManagedPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(data)), true, nil
}

// WriteManagedMarker records that devvm owns the smol VM NAME created at
// createdAt. Atomic, 0600 in a 0700 dir, like the rest of the state dir.
// An empty createdAt is refused: a marker that cannot be checked against
// the VM would be trusted forever.
func WriteManagedMarker(p Paths, name, createdAt string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if strings.TrimSpace(createdAt) == "" {
		return errors.New("managed marker: the VM reports no created_at (devvm needs smolvm ≥ 1.16, which reports created_at; upgrade smolvm and retry)")
	}
	if err := os.MkdirAll(p.ManagedDir(), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(p.ManagedPath(name), []byte(createdAt+"\n"), 0o600)
}

// RemoveManagedMarker drops a marker (idempotent).
func RemoveManagedMarker(p Paths, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	err := os.Remove(p.ManagedPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
