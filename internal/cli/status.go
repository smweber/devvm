package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// statusGroups is the display order for `status`: one section per backend.
var statusGroups = []struct{ backend, title string }{
	{config.BackendSmol, "smol"},
	{config.BackendRemoteManaged, "remote-managed"},
	{config.BackendRemoteUnmanaged, "remote-unmanaged"},
	{config.BackendHub, "hub"}, // every hub's own row followed by its machines, whatever backend the hub reports for them
}

// smolLifecycle is the derived-state track shown in the verbose view; the live
// state is highlighted. Remote backends have no dormant/stopped split (they
// always report reachable), so they get no track.
var smolLifecycle = []string{"dormant", "running", "stopped"}

// statusRow is one machine's resolved status for rendering.
type statusRow struct {
	name    string
	backend string
	state   string // dormant | running | stopped | reachable | unreachable | broken conf | ?
	hub     string // the hub this row belongs to (its own row included); "" for a local machine
	exists  bool
	running bool
	mem     int // MiB (smol conf spec)
	disk    int // GiB (smol conf spec)
	host    string
	fwds    fwdSummary
	note    string // e.g. "unregistered"
	m       *config.Machine
}

// fwdSummary is what status knows about a machine's forward daemon.
type fwdSummary struct {
	daemon bool      // a daemon answered
	state  string    // session.StateUp | session.StateReconnecting
	n      int       // forwards it owns (live or pending)
	since  time.Time // when state began
}

// runStatusAll lists every machine (registry ∪ live smol ∪ the hubs'
// listings), grouped by backend. verbose adds a lifecycle track, live smol
// resource sizes, and per-machine forward detail. local stops at this host.
func (a *App) runStatusAll(verbose, local bool) error {
	rows := a.gatherRows(statusScope{local: local})
	if len(rows) == 0 {
		fmt.Fprintln(a.Stdout, "No machines. Create one with 'devvm create'.")
		return nil
	}

	first := true
	for _, g := range statusGroups {
		var group []statusRow
		for _, r := range rows {
			if groupOf(r) == g.backend {
				group = append(group, r)
			}
		}
		if len(group) == 0 {
			continue
		}
		if !first {
			fmt.Fprintln(a.Stdout)
		}
		first = false
		fmt.Fprintln(a.Stdout, g.title)
		if g.backend == config.BackendSmol {
			a.renderSmolGroup(group, verbose)
		} else {
			a.renderRemoteGroup(group, verbose)
		}
	}
	return nil
}

// runStatusPlain emits one tab-separated row per machine — name, backend,
// state, and forward state — with no headers or grouping, so scripts can
// enumerate machines (e.g. every running smol VM) without scraping the
// human-formatted table. The token sets are the format's contract:
//
//	state:    running | stopped | dormant | reachable | unreachable | broken conf | ?
//	forwards: up:N | reconnecting:N | down | -
//
// N is the number of forwards the daemon owns; `down` means ports are
// configured but no daemon answers (the normal state of a stopped VM, so a UI
// should only flag it on a running machine); `-` means nothing is configured.
// `reachable` is what every remote reports (devvm does not probe them), and
// `?` / `broken conf` mean the backend could not be asked / the conf did not
// load. Consumers should treat any other token as "unknown", not fail.
// `--watch` re-emits this listing on devvm-made changes (see watchStatus); the
// format is the same, so a consumer parses one thing.
//
// Hubs add no column: a hub's own row is `HUB<TAB>hub<TAB>reachable|unreachable`,
// and a machine on it is `HUB/NAME` with the backend and state the hub
// reported (`unreachable` for every row of a hub that did not answer, from
// the cache) and this host's own forwards column. A consumer that wants to
// group by hub splits column 1 on `/`. --local lists this host only.
func (a *App) runStatusPlain(local bool) error {
	for _, r := range a.gatherRows(statusScope{local: local}) {
		fmt.Fprintf(a.Stdout, "%s\t%s\t%s\t%s\n", r.name, r.backend, r.state, plainForwards(r))
	}
	return nil
}

// groupOf is the status section a row renders in: a hub and every machine
// on it share the hub section, whatever backend the hub reports for them.
func groupOf(r statusRow) string {
	if r.hub != "" {
		return config.BackendHub
	}
	return r.backend
}

// plainForwards renders the machine-readable forward column (see runStatusPlain).
func plainForwards(r statusRow) string {
	switch {
	case r.fwds.daemon:
		return fmt.Sprintf("%s:%d", r.fwds.state, r.fwds.n)
	case r.m != nil && len(r.m.Ports) > 0:
		return "down"
	default:
		return "-"
	}
}

func (a *App) renderSmolGroup(rows []statusRow, verbose bool) {
	fmt.Fprintf(a.Stdout, "  %-16s %-12s %-8s %-8s %s\n", "NAME", "STATE", "MEM", "DISK", "FWDS")
	for _, r := range rows {
		mem, disk := r.mem, r.disk
		if verbose && r.running {
			if lm, ld, ok := a.smolLiveResources(r.name, r.m); ok {
				mem, disk = lm, ld
			}
		}
		fmt.Fprintf(a.Stdout, "  %-16s %-12s %-8s %-8s %s\n",
			r.name, r.state, memHuman(mem), diskHuman(disk), fwdsCount(r.fwds))
		if verbose {
			a.renderVerboseDetail(r)
		}
	}
}

func (a *App) renderRemoteGroup(rows []statusRow, verbose bool) {
	// HOST is sized to the widest value so a long user@address does not push
	// FWDS out of line.
	hostW := 22
	for _, r := range rows {
		if len(r.host) > hostW {
			hostW = len(r.host)
		}
	}
	fmt.Fprintf(a.Stdout, "  %-16s %-12s %-*s %s\n", "NAME", "STATE", hostW, "HOST", "FWDS")
	for _, r := range rows {
		fmt.Fprintf(a.Stdout, "  %-16s %-12s %-*s %s\n", r.name, r.state, hostW, r.host, fwdsCount(r.fwds))
		if verbose {
			a.renderVerboseDetail(r)
		}
	}
}

// renderVerboseDetail prints the lifecycle track (registered smol) and each live
// forward under a machine's row.
func (a *App) renderVerboseDetail(r statusRow) {
	if r.backend == config.BackendSmol && r.note == "" && r.hub == "" {
		fmt.Fprintf(a.Stdout, "    lifecycle: %s\n", lifecycleTrack(smolLifecycle, r.state))
	}
	if r.hub != "" && r.name != r.hub {
		// The HOST column carries the hub; the backend the hub reported for
		// the machine has nowhere else to show in the human table. A row the
		// hub did not name (this host's table or daemon only) has none.
		if r.backend == config.BackendHub {
			fmt.Fprintf(a.Stdout, "    backend: unknown (hub %s does not list it)\n", r.hub)
		} else {
			fmt.Fprintf(a.Stdout, "    backend: %s (as reported by hub %s)\n", r.backend, r.hub)
		}
	}
	if cl, err := session.Existing(a.ConfigDir, r.name); err == nil {
		if st, err := cl.Status(); err == nil && len(st.Forwards) > 0 {
			if st.Reconnecting() {
				fmt.Fprintf(a.Stdout, "    forwards: reconnecting (since %s)\n", sinceHuman(st.Since))
			} else {
				fmt.Fprintln(a.Stdout, "    forwards:")
			}
			for _, f := range st.Forwards {
				suffix := ""
				if f.Pending {
					suffix = "  (pending)"
				}
				fmt.Fprintf(a.Stdout, "      localhost:%d -> %s:%d%s\n", f.Host, r.name, f.Guest, suffix)
			}
		}
	}
}

// statusScope says how far a listing reaches. local stops at this host: no
// hub conf is read, nothing is dialed (hub.md §5: it is what a hub runs for
// *its* callers, so hubs never fan out). Otherwise every hub conf
// contributes its rows — fetched now when listings is nil, or, under
// --watch, the latest block from each held pipe.
type statusScope struct {
	local    bool
	hubs     []*config.Machine // the hub confs, when the caller already loaded them (--watch); nil loads them here
	listings hubListings
}

// gatherRows resolves every local machine listMachines knows plus any
// live-but-unregistered smol VM into a statusRow, then (unless local) each
// hub's block: its own row and a row per machine on it (hubRows).
//
// smolvm is listed once per call and every smol row is derived from that one
// listing (rather than the backend's per-machine Status probe): a snapshot is
// re-taken on every --watch event, and N+1 `smolvm machine ls` subprocesses
// per change would be the polling this command exists to avoid.
func (a *App) gatherRows(sc statusScope) []statusRow {
	var rows []statusRow
	seen := map[string]bool{}
	smols := smolSnapshot()
	for _, name := range a.listMachines() {
		if _, _, onHub, _ := config.SplitHubName(name); onHub {
			continue // rendered under its hub below, or not at all with --local
		}
		seen[name] = true
		m, err := config.Load(a.ConfigDir, name)
		if err != nil {
			rows = append(rows, statusRow{name: name, backend: "?", state: "broken conf"})
			continue
		}
		if m.IsHub() {
			continue // its row comes with its listing
		}
		rows = append(rows, a.rowFor(m, smols))
	}
	// Live smol VMs not in the registry. They can still have a daemon (ports
	// added by hand), so the forward column is real for them too.
	for _, sm := range smols.list {
		if seen[sm.Name] {
			continue
		}
		rows = append(rows, statusRow{
			name: sm.Name, backend: config.BackendSmol,
			state:  smolStateLabel(sm.State != "not created", sm.State == "running"),
			exists: sm.State != "not created", running: sm.State == "running",
			note: "unregistered", fwds: a.forwardSummary(sm.Name),
		})
	}
	if sc.local {
		return rows
	}
	hubs := sc.hubs
	if hubs == nil {
		hubs = a.hubConfs()
	}
	listings := sc.listings
	if listings == nil {
		listings = a.fetchHubListings(context.Background(), hubs)
	}
	for _, hub := range hubs {
		rows = append(rows, a.hubRows(hub, listings[hub.Name])...)
	}
	return rows
}

// hubRows renders one hub: its own row, then a row per machine on it. In
// the human table every row of the hub shows the hub's ssh host in HOST and
// no backend column (the section is the hub's, whatever backend the hub
// reports for a machine); `-v` prints the reported backend under the row.
// The
// machine set is the union of what the hub listed (fresh, or cached when it
// did not answer) and what this host knows on its own — a [machines.NAME]
// table, a live run/HUB@NAME.sock — so a laptop-side forward for a machine
// the hub no longer lists still shows, with state `?` (the hub answered and
// did not name it; `delete HUB/NAME` clears it) and backend `hub` (nothing
// reported one). State and backend otherwise come from the hub's row, the
// forwards column from this host's own daemon for HUB@NAME, never the
// hub's. With no fresh answer every row of the hub is `unreachable`,
// whatever the cache remembers: a cached `running` would be a claim nobody
// is making.
func (a *App) hubRows(hub *config.Machine, l hubListing) []statusRow {
	state := "reachable"
	if !l.fresh {
		state = "unreachable"
	}
	rows := []statusRow{{name: hub.Name, backend: config.BackendHub, hub: hub.Name, state: state, host: hub.SSHHost, m: hub}}
	byName := map[string]hubRow{}
	names := map[string]bool{}
	for _, r := range l.rows {
		byName[r.name], names[r.name] = r, true
	}
	for n := range hub.Machines {
		names[n] = true
	}
	for _, display := range liveHubSockets(a.ConfigDir) {
		if h, n, _, _ := config.SplitHubName(display); h == hub.Name {
			names[n] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		display := config.JoinHubName(hub.Name, n)
		r := statusRow{
			name: display, hub: hub.Name, host: hub.SSHHost,
			m: &config.Machine{Name: display, Hub: hub, Backend: config.BackendHub, Ports: hub.Machines[n].Ports},
		}
		if hr, ok := byName[n]; ok {
			r.backend, r.state = hr.backend, hr.state
			r.exists, r.running = hr.state != "dormant", hr.state == "running"
		} else {
			r.backend, r.state = config.BackendHub, "?"
		}
		if !l.fresh {
			r.state = "unreachable"
		}
		r.fwds = a.forwardSummary(display)
		rows = append(rows, r)
	}
	return rows
}

// smolMachines is one `smolvm machine ls` taken for a whole status snapshot.
type smolMachines struct {
	available bool // smolvm is installed; otherwise every smol row is "?"
	list      []backend.SmolMachine
	state     map[string]string
}

func smolSnapshot() smolMachines {
	s := smolMachines{available: backend.SmolAvailable(), state: map[string]string{}}
	if !s.available {
		return s
	}
	s.list, _ = backend.SmolList()
	for _, sm := range s.list {
		s.state[sm.Name] = sm.State
	}
	return s
}

func (a *App) rowFor(m *config.Machine, smols smolMachines) statusRow {
	r := statusRow{name: m.Name, backend: m.Backend, m: m, mem: m.Memory, disk: m.Disk, host: m.SSHHost}
	if m.Backend == config.BackendSmol {
		if !smols.available {
			r.state = "?"
			return r
		}
		st, ok := smols.state[m.Name]
		r.exists, r.running = ok && st != "not created", st == "running"
		r.state = smolStateLabel(r.exists, r.running)
		r.fwds = a.forwardSummary(m.Name)
		return r
	}
	b, err := backend.For(m, a.ConfigDir)
	if err != nil {
		r.state = "broken conf"
		return r
	}
	st, err := b.Status()
	if err != nil {
		r.state = "?"
		return r
	}
	r.exists, r.running = st.Exists, st.Running
	r.state = "reachable"
	r.fwds = a.forwardSummary(m.Name)
	return r
}

// smolStateLabel maps a backend snapshot to one of the derived lifecycle states.
func smolStateLabel(exists, running bool) string {
	switch {
	case !exists:
		return "dormant"
	case running:
		return "running"
	default:
		return "stopped"
	}
}

// forwardSummary asks a machine's daemon for its state and forward count; a
// zero value means no daemon answered.
func (a *App) forwardSummary(name string) fwdSummary {
	cl, err := session.Existing(a.ConfigDir, name)
	if err != nil {
		return fwdSummary{}
	}
	st, err := cl.Status()
	if err != nil {
		return fwdSummary{}
	}
	state := st.State
	if state == "" {
		state = session.StateUp // a pre-0.1.11 daemon reports no state; it only ever ran up
	}
	return fwdSummary{daemon: true, state: state, n: len(st.Forwards), since: st.Since}
}

// smolLiveResources reads a running smol VM's actual memory (MiB) and root-fs
// size (GiB) with one quick guest exec. Best-effort: it is skipped when a session
// daemon already holds the machine's persistent exec (smolvm serializes poorly
// across parallel execs — the one-exec rule), and any error yields ok=false so
// the caller falls back to the conf spec.
func (a *App) smolLiveResources(name string, m *config.Machine) (memMiB, diskGiB int, ok bool) {
	if m == nil {
		return 0, 0, false
	}
	if _, err := session.Existing(a.ConfigDir, name); err == nil {
		return 0, 0, false // a daemon owns the exec; don't risk a parallel one
	}
	b, err := backend.For(m, a.ConfigDir)
	if err != nil {
		return 0, 0, false
	}
	const script = `printf '%s %s\n' ` +
		`"$(awk '/^MemTotal:/{printf "%d", $2/1024}' /proc/meminfo)" ` +
		`"$(df -BG / | awk 'NR==2{gsub(/G/,"",$2); print $2}')"`
	var buf bytes.Buffer
	if err := b.Run(context.Background(), backend.ExecOpts{Stdout: &buf, Stderr: io.Discard}, "sh", "-c", script); err != nil {
		return 0, 0, false
	}
	parts := strings.Fields(buf.String())
	if len(parts) != 2 {
		return 0, 0, false
	}
	mem, e1 := strconv.Atoi(parts[0])
	dsk, e2 := strconv.Atoi(parts[1])
	if e1 != nil || e2 != nil {
		return 0, 0, false
	}
	return mem, dsk, true
}

// runStatus is the single-machine drill-in: always detailed (state, lifecycle,
// live sizes for smol, and live forwards).
func (a *App) runStatus(name string) error {
	m, b, err := a.resolveAny(name) // a hub and a hub machine both have a status
	if err != nil {
		return err
	}
	if m.IsHubMachine() {
		return a.runStatusHubMachine(m)
	}
	st, err := b.Status()
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "%s (%s)\n", m.Name, m.Backend)
	if m.Backend == config.BackendSmol {
		state := smolStateLabel(st.Exists, st.Running)
		fmt.Fprintf(a.Stdout, "  lifecycle: %s\n", lifecycleTrack(smolLifecycle, state))
		mem, disk := m.Memory, m.Disk
		if st.Running {
			if lm, ld, ok := a.smolLiveResources(name, m); ok {
				mem, disk = lm, ld
			}
		}
		fmt.Fprintf(a.Stdout, "  resources: %s RAM, %s disk\n", memHuman(mem), diskHuman(disk))
	} else {
		raw := st.Raw
		if !st.Exists {
			raw = "dormant (not provisioned)"
		}
		fmt.Fprintf(a.Stdout, "  state: %s\n", raw)
	}
	a.forwardReport(name)
	return nil
}

// runStatusHubMachine is the drill-in for HUB/NAME: one listing from its hub
// (the same call the merged listing makes, same cache, same deadline), its
// row picked out. A hub that does not answer says so on its own line rather
// than dressing a cached token up as the current state.
func (a *App) runStatusHubMachine(m *config.Machine) error {
	l := a.listHub(context.Background(), m.Hub)
	for _, r := range a.hubRows(m.Hub, l) {
		if r.name != m.Name {
			continue
		}
		if r.backend == config.BackendHub { // the hub did not name it, so no backend is known
			fmt.Fprintf(a.Stdout, "%s (on hub %s, %s)\n", m.Name, m.Hub.Name, m.Hub.SSHHost)
		} else {
			fmt.Fprintf(a.Stdout, "%s (%s on hub %s, %s)\n", m.Name, r.backend, m.Hub.Name, m.Hub.SSHHost)
		}
		fmt.Fprintf(a.Stdout, "  state: %s\n", r.state)
		if !l.fresh {
			fmt.Fprintf(a.Stdout, "  hub: %s did not answer within %s; the machine is from its last listing\n", m.Hub.Name, hubTimes.deadline)
		}
		a.forwardReport(m.Name)
		return nil
	}
	if !l.fresh {
		return fmt.Errorf("hub '%s' (%s) did not answer, and its last listing does not name %s", m.Hub.Name, m.Hub.SSHHost, m.HubMachineName())
	}
	return fmt.Errorf("hub '%s' lists no machine %q; run 'devvm status'", m.Hub.Name, m.HubMachineName())
}

// lifecycleTrack renders states joined by arrows with the current one bracketed.
func lifecycleTrack(states []string, current string) string {
	parts := make([]string, len(states))
	for i, s := range states {
		if s == current {
			parts[i] = "[" + s + "]"
		} else {
			parts[i] = s
		}
	}
	return strings.Join(parts, " ─ ")
}

// memHuman renders MiB as GiB when it's a clean multiple, else MiB; "—" at zero.
func memHuman(miB int) string {
	if miB <= 0 {
		return "—"
	}
	if miB%1024 == 0 {
		return fmt.Sprintf("%d GiB", miB/1024)
	}
	return fmt.Sprintf("%d MiB", miB)
}

func diskHuman(giB int) string {
	if giB <= 0 {
		return "—"
	}
	return fmt.Sprintf("%d GiB", giB)
}

// fwdsCount renders a forward count, or "—" when none are up.
// fwdsCount is the FWDS column: a count, flagged while the daemon is between
// transports so a stuck reconnect reads differently from healthy forwards.
func fwdsCount(f fwdSummary) string {
	if f.n == 0 {
		return "—"
	}
	if f.state == session.StateReconnecting {
		return fmt.Sprintf("%d (reconnecting %s)", f.n, sinceHuman(f.since))
	}
	return strconv.Itoa(f.n)
}
