package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// statusGroups is the display order for `status`: one section per backend.
var statusGroups = []struct{ backend, title string }{
	{config.BackendSmol, "smol"},
	{config.BackendRemoteManaged, "remote-managed"},
	{config.BackendRemoteUnmanaged, "remote-unmanaged"},
}

// smolLifecycle is the derived-state track shown in the verbose view; the live
// state is highlighted. Remote backends have no dormant/stopped split (they
// always report reachable), so they get no track.
var smolLifecycle = []string{"dormant", "running", "stopped"}

// statusRow is one machine's resolved status for rendering.
type statusRow struct {
	name    string
	backend string
	state   string // dormant | running | stopped | reachable | broken conf | ?
	exists  bool
	running bool
	mem     int // MiB (smol conf spec)
	disk    int // GiB (smol conf spec)
	host    string
	fwds    int    // live forward count
	note    string // e.g. "unregistered"
	m       *config.Machine
}

// runStatusAll lists every machine (registry ∪ live smol), grouped by backend.
// verbose adds a lifecycle track, live smol resource sizes, and per-machine
// forward detail.
func (a *App) runStatusAll(verbose bool) error {
	rows := a.gatherRows()
	if len(rows) == 0 {
		fmt.Fprintln(a.Stdout, "No machines. Create one with 'devvm create'.")
		return nil
	}

	first := true
	for _, g := range statusGroups {
		var group []statusRow
		for _, r := range rows {
			if r.backend == g.backend {
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

// runStatusPlain emits one tab-separated row per machine — name, backend, and
// derived state (running|stopped|dormant|reachable|…) — with no headers or
// grouping, so scripts can enumerate machines (e.g. every running smol VM)
// without scraping the human-formatted table.
func (a *App) runStatusPlain() error {
	for _, r := range a.gatherRows() {
		fmt.Fprintf(a.Stdout, "%s\t%s\t%s\n", r.name, r.backend, r.state)
	}
	return nil
}

func (a *App) renderSmolGroup(rows []statusRow, verbose bool) {
	fmt.Fprintf(a.Stdout, "  %-16s %-10s %-8s %-8s %s\n", "NAME", "STATE", "MEM", "DISK", "FWDS")
	for _, r := range rows {
		mem, disk := r.mem, r.disk
		if verbose && r.running {
			if lm, ld, ok := a.smolLiveResources(r.name, r.m); ok {
				mem, disk = lm, ld
			}
		}
		fmt.Fprintf(a.Stdout, "  %-16s %-10s %-8s %-8s %s\n",
			r.name, r.state, memHuman(mem), diskHuman(disk), fwdsCount(r.fwds))
		if verbose {
			a.renderVerboseDetail(r)
		}
	}
}

func (a *App) renderRemoteGroup(rows []statusRow, verbose bool) {
	fmt.Fprintf(a.Stdout, "  %-16s %-10s %-22s %s\n", "NAME", "STATE", "HOST", "FWDS")
	for _, r := range rows {
		fmt.Fprintf(a.Stdout, "  %-16s %-10s %-22s %s\n", r.name, r.state, r.host, fwdsCount(r.fwds))
		if verbose {
			a.renderVerboseDetail(r)
		}
	}
}

// renderVerboseDetail prints the lifecycle track (registered smol) and each live
// forward under a machine's row.
func (a *App) renderVerboseDetail(r statusRow) {
	if r.backend == config.BackendSmol && r.note == "" {
		fmt.Fprintf(a.Stdout, "    lifecycle: %s\n", lifecycleTrack(smolLifecycle, r.state))
	}
	if cl, err := session.Existing(a.ConfigDir, r.name); err == nil {
		if fwds, err := cl.List(); err == nil && len(fwds) > 0 {
			fmt.Fprintln(a.Stdout, "    forwards:")
			for _, f := range fwds {
				fmt.Fprintf(a.Stdout, "      localhost:%d -> %s:%d\n", f.Host, r.name, f.Guest)
			}
		}
	}
}

// gatherRows resolves every registered machine plus any live-but-unregistered
// smol VM into a statusRow.
func (a *App) gatherRows() []statusRow {
	var rows []statusRow
	seen := map[string]bool{}
	names, _ := config.List(a.ConfigDir)
	for _, name := range names {
		seen[name] = true
		m, err := config.Load(a.ConfigDir, name)
		if err != nil {
			rows = append(rows, statusRow{name: name, backend: "?", state: "broken conf"})
			continue
		}
		rows = append(rows, a.rowFor(m))
	}
	// Live smol VMs not in the registry.
	smols, _ := backend.SmolList()
	for _, sm := range smols {
		if seen[sm.Name] {
			continue
		}
		rows = append(rows, statusRow{
			name: sm.Name, backend: config.BackendSmol,
			state:  smolStateLabel(sm.State != "not created", sm.State == "running"),
			exists: sm.State != "not created", running: sm.State == "running",
			note: "unregistered",
		})
	}
	return rows
}

func (a *App) rowFor(m *config.Machine) statusRow {
	r := statusRow{name: m.Name, backend: m.Backend, m: m, mem: m.Memory, disk: m.Disk, host: m.SSHHost}
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
	if m.Backend == config.BackendSmol {
		r.state = smolStateLabel(st.Exists, st.Running)
	} else {
		r.state = "reachable"
	}
	r.fwds = a.forwardCount(m.Name)
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

// forwardCount returns how many forwards are live for a machine (0 if no daemon).
func (a *App) forwardCount(name string) int {
	cl, err := session.Existing(a.ConfigDir, name)
	if err != nil {
		return 0
	}
	fwds, err := cl.List()
	if err != nil {
		return 0
	}
	return len(fwds)
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
	m, b, err := a.resolve(name)
	if err != nil {
		return err
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
func fwdsCount(n int) string {
	if n == 0 {
		return "—"
	}
	return strconv.Itoa(n)
}
