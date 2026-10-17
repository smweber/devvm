package backend

import (
	"github.com/smweber/devvm/internal/config"
)

// Discovery (config.md §2, §9 item 2): a name with no entry is
// looked up in an ordered list of sources, first match wins. smol (`smolvm
// machine ls`) is the only source today; a provider backend later adds
// one (its API, filtered by a label) without touching the callers, and the
// collision rule (a entry shadows any discovered machine of the
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
	m := SmolRecord(paths, sm)
	return Discovered{Machine: m, Backend: &smolBackend{m: m, paths: paths, row: &sm}}, true, nil
}

// SmolRecord is the record the smol source makes of one listing row,
// Managed settled. The status snapshot uses it for every row of the one
// `ls` it took (not one listing per row).
func SmolRecord(paths config.Paths, sm SmolMachine) *config.Machine {
	m := &config.Machine{Name: sm.Name, Backend: config.BackendSmol}
	m.SetManaged(settleSmol(paths, sm))
	return m
}

// settleSmol is the smol source's Managed rule (config.md §2): managed iff
// the marker holds this VM's created_at. A VM with no created_at (a smolvm
// older than 1.16, the first that reports created_at), or a
// marker naming another created_at (the VM deleted with raw smolvm and
// recreated under the same name), is unmanaged: a stale marker is ignored,
// never trusted, and never rewritten here. A devvm-made VM from before
// markers existed has none, so it reads unmanaged until its marker is
// written: by `setup --adopt NAME`, or by hand during the migration
// (config.md §8: <State>/managed/NAME holding the VM's created_at).
func settleSmol(paths config.Paths, sm SmolMachine) bool {
	created := string(sm.CreatedAt)
	if created == "" {
		return false
	}
	marker, ok, err := config.ReadManagedMarker(paths, sm.Name)
	if err != nil || !ok {
		return false
	}
	return normalizeCreatedAt(marker) == created
}

// SeedLegacyMarkers is the one-time marker seed for the upgrade to
// devvm.toml (config.md §8), alongside config.MigratePorts: every VM an old
// per-machine smol conf names (machines/NAME.toml, `backend = "smol"`) that
// has no marker yet gets <State>/managed/NAME with its current created_at,
// so the VMs devvm made before markers existed stay managed instead of
// turning unmanaged (and refusing attach) on upgrade. One `ls --json` for
// all of them, and only when one is unmarked; a name smolvm does not list,
// a VM with no created_at (devvm needs smolvm ≥ 1.16, which reports
// created_at; an older one leaves the VM unmarked, and it reads unmanaged
// until smolvm is upgraded and the next invocation seeds it), or a host
// with no smolvm, is skipped. An existing marker is never
// rewritten, stale or not (that is the VM-recreated-behind-devvm case).
// Idempotent and best-effort: it runs on every invocation while machines/
// still holds such confs.
func SeedLegacyMarkers(paths config.Paths) {
	var unmarked []string
	for _, n := range config.LegacySmolConfs(paths) {
		if _, ok, err := config.ReadManagedMarker(paths, n); err == nil && !ok {
			unmarked = append(unmarked, n)
		}
	}
	if len(unmarked) == 0 || !SmolAvailable() {
		return
	}
	ms, err := smolList()
	if err != nil {
		return
	}
	for _, n := range unmarked {
		for _, sm := range ms {
			if sm.Name == n && sm.CreatedAt != "" {
				_ = config.WriteManagedMarker(paths, n, string(sm.CreatedAt))
			}
		}
	}
}
