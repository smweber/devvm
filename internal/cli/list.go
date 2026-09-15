package cli

import (
	"bufio"
	"os"
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

// hubCachePath is the per-hub listing cache: the hub's `status --plain
// --local` rows, written by the merged listing (roadmap step 3). It sits in
// cache/, outside the two directories `status --watch` observes.
func hubCachePath(configDir, hub string) string {
	return filepath.Join(config.CacheDir(configDir), "hub-"+hub+".list")
}

// cachedHubMachines returns the machine names in a hub's cached listing:
// column 1 of each --plain row, which the hub emits as bare names. Nothing
// writes the cache before roadmap step 3, so today this reads nothing; it is
// here so listMachines has its four sources from the start and step 3 only
// has to write the file.
func cachedHubMachines(configDir, hub string) []string {
	f, err := os.Open(hubCachePath(configDir, hub))
	if err != nil {
		return nil
	}
	defer f.Close()
	var names []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, _, _ := strings.Cut(sc.Text(), "\t")
		// ValidName also rejects a HUB/ prefix or an @ form: hubs never nest,
		// and a hub's --local rows carry bare names.
		if config.ValidName(name) == nil {
			names = append(names, name)
		}
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
