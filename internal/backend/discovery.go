package backend

import (
	"github.com/smweber/devvm/internal/config"
)

// Discovery (config.md §2, §9 item 2): a name with no registry entry is
// looked up in an ordered list of sources, first match wins. smol (`smolvm
// machine ls`) is the only source today; a provider backend later adds
// one (its API, filtered by a label) without touching the callers, and the
// collision rule (a registry entry shadows any discovered machine of the
// same name) extends to it unchanged.
//
// The source that produced a record settles its Managed bit (§9 item 3):
// smol compares the managed marker with the VM's created_at, a provider
// would read its label. No caller reads a marker to decide anything.

// Discovered is a record a source produced, with the backend that drives
// it (carrying what the source saw, e.g. smol's listing row).
type Discovered struct {
	Machine *config.Machine
	Backend Backend
}

// discoverer is one source. ok=false is "not mine, ask the next"; an error
// stops the search (a source that cannot answer must not let a later one
// claim the name).
type discoverer func(paths config.Paths, name string) (d Discovered, ok bool, err error)

// discoverers is the ordered source list.
var discoverers = []discoverer{discoverSmol}

// Discover asks each source in order for name; ok=false means no source
// knows it.
func Discover(paths config.Paths, name string) (Discovered, bool, error) {
	for _, find := range discoverers {
		if d, ok, err := find(paths, name); err != nil || ok {
			return d, ok, err
		}
	}
	return Discovered{}, false, nil
}

// discoverSmol is the local smol source: one `smolvm machine ls --json`,
// the row kept on the backend. A host with no smolvm has no smol VMs, which
// is "not mine", not an error: a laptop that only reaches remote boxes
// never needs smolvm.
func discoverSmol(paths config.Paths, name string) (Discovered, bool, error) {
	if !SmolAvailable() {
		return Discovered{}, false, nil
	}
	sm, ok, err := smolLookup(name)
	if err != nil || !ok {
		return Discovered{}, false, err
	}
	m := config.NewSmol(name)
	m.SetManaged(settleSmol(paths, sm, false))
	return Discovered{Machine: m, Backend: &smolBackend{m: m, paths: paths, row: &sm}}, true, nil
}

// ResolveSmolConf is the smol source for a record loaded from an old
// per-machine conf (machines/NAME.toml with backend smol; the loader goes
// in config.md step 5, and this with it). Same one listing; the VM must
// exist (there is no dormant state: a conf with no VM is not found), and
// Managed is settled as for any smol VM, plus the migration rule in
// settleSmol. ok=false means smolvm has no such VM.
func ResolveSmolConf(paths config.Paths, m *config.Machine) (Backend, bool, error) {
	if err := needSmolvm(); err != nil {
		return nil, false, err
	}
	sm, ok, err := smolLookup(m.Name)
	if err != nil || !ok {
		return nil, false, err
	}
	m.SetManaged(settleSmol(paths, sm, true))
	return &smolBackend{m: m, paths: paths, row: &sm, fromConf: true}, true, nil
}

// SmolManaged settles one row of a listing the caller already took (the
// status snapshot: one `ls` for every row, not one per row). hasConf says
// an old per-machine conf names the VM (see settleSmol).
func SmolManaged(paths config.Paths, sm SmolMachine, hasConf bool) bool {
	return settleSmol(paths, sm, hasConf)
}

// settleSmol is the smol source's Managed rule (config.md §2): managed iff
// the marker holds this VM's created_at. A VM with no created_at, or a
// marker naming another created_at (the VM deleted with raw smolvm and
// recreated under the same name), is unmanaged: a stale marker is ignored,
// never trusted, and never rewritten here.
//
// Migration aid, until the old loader goes (step 5): a VM that has no
// marker but does have a per-machine smol conf (hasConf) was made by a
// devvm from before markers existed, on one of the hosts migrated by hand,
// so it is treated as managed and its marker is written now, silently and
// idempotently. Without this every existing devvm-made VM would turn
// unmanaged on upgrade and refuse attach until a `setup --adopt` that
// lands only in step 4. A marker that exists is never overridden by a
// conf: that is exactly the recreated-under-the-same-name case.
func settleSmol(paths config.Paths, sm SmolMachine, hasConf bool) bool {
	created := string(sm.CreatedAt)
	if created == "" {
		return false
	}
	marker, ok, err := config.ReadManagedMarker(paths, sm.Name)
	if err != nil {
		return false
	}
	if ok {
		return normalizeCreatedAt(marker) == created
	}
	if !hasConf {
		return false
	}
	_ = config.WriteManagedMarker(paths, sm.Name, created) // best-effort: the conf alone decides this time
	return true
}
