package cli

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/smweber/devvm/internal/config"
)

// listMachines enumerates every machine name this host can address, for the
// commands that walk "all machines": status, update's daemon restart, the
// global `ports list`, and completion. It unions
//
//   - config.List: every local conf, hubs included (a hub has a row of its own);
//   - HUB/NAME for every saved-forwards file ports/HUB@NAME (savedHubPorts);
//   - HUB/NAME for every name in the hub's cached listing (cachedHubMachines);
//   - HUB/NAME for every live run/HUB@NAME.sock.
//
// It lives here and not in config, which stays a file reader that never dials
// a socket or trusts a cache: the last two sources are hints for enumeration
// and completion, and only a proxied command says whether a machine exists.
// The socket source matters because a laptop daemon for a hub machine can
// exist with no conf entry at all (a browser open from `attach HUB/NAME`
// creates a connection-owned forward and writes nothing).
func (a *App) listMachines() []string {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	local, _ := config.List(a.Paths.Config)
	for _, n := range local {
		add(n)
		m, err := config.Load(a.Paths.Config, n)
		if err != nil || !m.IsHub() {
			continue // a broken conf is still listed (status shows it as such)
		}
		for _, mn := range cachedHubMachines(a.Paths, n) {
			add(config.JoinHubName(n, mn))
		}
	}
	// Only for a hub that is still registered: a stray file for a deleted
	// hub would list a machine nothing can reach. Live sockets are listed
	// regardless, as before, since a running daemon is a real thing.
	for _, n := range savedHubPorts(a.Paths) {
		if hub, _, _, _ := config.SplitHubName(n); slices.Contains(local, hub) {
			add(n)
		}
	}
	for _, n := range liveHubSockets(a.Paths) {
		add(n)
	}
	sort.Strings(names)
	return names
}

// cachedHubMachines returns the machine names in a hub's cached listing
// (hublist.go): column 1 of each --plain row, which the hub emits as bare
// names. A hint for enumeration and completion only; the merged listing
// says whether a machine is still there.
func cachedHubMachines(paths config.Paths, hub string) []string {
	var names []string
	for _, r := range readHubCache(paths, hub) {
		names = append(names, r.name)
	}
	return names
}

// savedHubPorts lists HUB/NAME for every hub machine this host has saved
// forwards for. Local names are skipped: their machines come from
// config.List (a stray local file must not make a conf-less name look like
// a broken conf).
func savedHubPorts(paths config.Paths) []string {
	var names []string
	saved, _ := config.SavedPortsNames(paths) // a hint here; the ports verbs report a failed migration
	for _, n := range saved {
		if _, _, ok, err := config.SplitHubName(n); ok && err == nil {
			names = append(names, n)
		}
	}
	return names
}

// liveHubSockets lists HUB/NAME for every run/HUB@NAME.sock. A stale socket
// (daemon crashed) is listed too; status pings it and shows no daemon, which
// is the truthful answer. Local sockets carry no '@' and are skipped: their
// machines come from config.List.
func liveHubSockets(paths config.Paths) []string {
	matches, _ := filepath.Glob(filepath.Join(paths.RuntimeDir(), "*@*.sock"))
	var names []string
	for _, p := range matches {
		rt := strings.TrimSuffix(filepath.Base(p), ".sock")
		display := config.DisplayName(rt)
		if _, _, ok, err := config.SplitHubName(display); ok && err == nil {
			names = append(names, display)
		}
	}
	return names
}
