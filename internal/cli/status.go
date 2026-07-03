package cli

import (
	"fmt"
	"sort"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// runStatusAll lists every machine (registry ∪ live smol), mirroring status_all.
// The running-forwards section is added with the session daemon (Phase 3).
func (a *App) runStatusAll() error {
	fmt.Printf("%-20s %-8s %s\n", "NAME", "BACKEND", "STATE")
	fmt.Println("----------------------------------------------------")

	seen := map[string]bool{}
	names, _ := config.List(a.ConfigDir)
	for _, name := range names {
		seen[name] = true
		m, err := config.Load(a.ConfigDir, name)
		if err != nil {
			fmt.Printf("%-20s %-8s %s\n", name, "?", "broken conf")
			continue
		}
		state := a.machineState(m)
		fmt.Printf("%-20s %-8s %s\n", name, m.Backend, state)
	}

	// Live smol machines not in the registry.
	smols, _ := backend.SmolList()
	for _, sm := range smols {
		if seen[sm.Name] {
			continue
		}
		seen[sm.Name] = true
		fmt.Printf("%-20s %-8s %s\n", sm.Name, "smol", sm.State+" (unregistered)")
	}

	// Every forward currently up, across all machines, with the actual host port.
	var lines []string
	for name := range seen {
		if cl, err := session.Existing(a.ConfigDir, name); err == nil {
			fwds, _ := cl.List()
			for _, f := range fwds {
				lines = append(lines, fmt.Sprintf("  %-16s localhost:%-5d -> %s:%d", name, f.Host, name, f.Guest))
			}
		}
	}
	if len(lines) > 0 {
		fmt.Println()
		fmt.Println("Running forwards:")
		sort.Strings(lines)
		for _, l := range lines {
			fmt.Println(l)
		}
	}
	return nil
}

// machineState renders a short state string for the aggregate listing.
func (a *App) machineState(m *config.Machine) string {
	b, err := backend.For(m, a.ConfigDir)
	if err != nil {
		return "broken conf"
	}
	st, err := b.Status()
	if err != nil {
		return "?"
	}
	return st.Raw
}
