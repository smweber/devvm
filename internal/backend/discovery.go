package backend

import (
	"github.com/smweber/devvm/internal/config"
)

// DiscoverSmol is discovery (config.md §2): a name with no entry is looked
// up among the local smol VMs, one `smolvm machine ls --json`, the row kept
// on the backend, for a VM with devvm's label only (simplify.md step 15:
// devvm does not see what it did not create; resolve names such a VM in
// its refusal). The label also settles the record's Managed bit (§9 item
// 3). ok=false is "no such VM"; a host with no smolvm has none, which is
// not an error: a laptop that only reaches remote boxes never needs
// smolvm. The providers come after it, in the cli (providers.go).
func DiscoverSmol(paths config.Paths, name string) (m *config.Machine, b Backend, ok bool, err error) {
	if !SmolAvailable() {
		return nil, nil, false, nil
	}
	sm, ok, err := smolLookup(name)
	if err != nil || !ok || !sm.Labelled() {
		return nil, nil, false, err // a VM devvm did not create is not its to see
	}
	m = &config.Machine{Name: sm.Name, Backend: config.BackendSmol}
	m.SetManaged(true) // its label (devvm=true, set by `create`)
	return m, &smolBackend{m: m, paths: paths, row: &sm}, true, nil
}
