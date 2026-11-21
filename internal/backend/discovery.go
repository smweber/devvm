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
// smol reads the VM's devvm=true label, as the provider source reads the
// server's.

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
	m := SmolRecord(sm)
	return Discovered{Machine: m, Backend: &smolBackend{m: m, paths: paths, row: &sm}}, true, nil
}

// SmolRecord is the record the smol source makes of one listing row,
// Managed settled: devvm's label on the VM (devvm=true, set by `create`).
// The status snapshot uses it for every row of the one `ls` it took (not
// one listing per row).
func SmolRecord(sm SmolMachine) *config.Machine {
	m := &config.Machine{Name: sm.Name, Backend: config.BackendSmol}
	m.SetManaged(sm.Labelled())
	return m
}
