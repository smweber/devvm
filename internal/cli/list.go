package cli

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/smweber/devvm/internal/config"
)

// listMachines enumerates every machine name this host can address, for the
// commands that walk "all machines": status, update's daemon restart, the
// global `ports list`, and completion. It unions
//
//   - config.List: every local conf, hubs included (a hub has a row of its own);
//   - HUB/NAME for every [machines.NAME] table in a hub conf;
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
	local, _ := config.List(a.ConfigDir)
	for _, n := range local {
		add(n)
		m, err := config.Load(a.ConfigDir, n)
		if err != nil || !m.IsHub() {
			continue // a broken conf is still listed (status shows it as such)
		}
		for mn := range m.Machines {
			add(config.JoinHubName(n, mn))
		}
		for _, mn := range cachedHubMachines(a.ConfigDir, n) {
			add(config.JoinHubName(n, mn))
		}
	}
	for _, n := range liveHubSockets(a.ConfigDir) {
		add(n)
	}
	sort.Strings(names)
	return names
}

// cachedHubMachines returns the machine names in a hub's cached listing
// (hublist.go): column 1 of each --plain row, which the hub emits as bare
// names. A hint for enumeration and completion only; the merged listing
// says whether a machine is still there.
func cachedHubMachines(configDir, hub string) []string {
	var names []string
	for _, r := range readHubCache(configDir, hub) {
		names = append(names, r.name)
	}
	return names
}

// liveHubSockets lists HUB/NAME for every run/HUB@NAME.sock. A stale socket
// (daemon crashed) is listed too; status pings it and shows no daemon, which
// is the truthful answer. Local sockets carry no '@' and are skipped: their
// machines come from config.List.
func liveHubSockets(configDir string) []string {
	matches, _ := filepath.Glob(filepath.Join(config.RuntimeDir(configDir), "*@*.sock"))
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
