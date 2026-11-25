package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
	"github.com/smweber/devvm/internal/reach"
	"github.com/smweber/devvm/internal/session"
)

// statusGroups is the display order for `status`: one section per backend.
// The titles are the human words (the backend tokens stay --plain's).
var statusGroups = []struct{ backend, title string }{
	{config.BackendSmol, "local (smolvm)"},
	{config.BackendRemote, "remote (ssh)"},
	{config.BackendHetzner, "hetzner"}, // provider servers: their own section, from the provider's cached listing
	{config.BackendHub, "hubs"},        // every hub's own row followed by its machines, whatever backend the hub reports for them
}

// smolLifecycle is the state track the single-machine drill-in shows; the
// live state is highlighted. A smol VM either exists or it doesn't (no
// dormant state, config.md §2).
var smolLifecycle = []string{"running", "stopped"}

// The --plain fifth column (config.md §7): who owns the machine.
const (
	ownManaged  = "managed"  // devvm made it: the devvm=true label, which every smol and provider row has
	ownShadowed = "shadowed" // a registry entry hiding a local smol VM, or an entry or smol VM hiding a provider server, of the same name
	ownNone     = "-"        // entries (devvm never shapes one) and hubs; a hub machine whose hub did not say
)

// statusRow is one machine's resolved status for rendering.
type statusRow struct {
	name        string
	backend     string
	state       string // running | stopped | starting | stopping | reachable | unreachable | ? (a hub's rows as it sent them)
	hub         string // the hub this row belongs to (its own row included); "" for a local machine
	mem         int    // MiB (smol: smolvm's own record)
	disk        int    // GiB (smol: smolvm's own record)
	cpus        int
	host        string
	fwds        fwdSummary
	saved       bool         // this host has saved forwards for it (<State>/ports/NAME): --plain's `down`
	managed     string       // the --plain fifth column: managed | shadowed | -
	note        string       // a remark for the human table, e.g. "this host"
	reach       *reach.Entry // a remote box's cached reach (--plain's sixth column); nil for every other row
	hubVerified string       // a hub machine's sixth column as its hub sent it (`verified=…`); "" when it sent none
	shadows     []string     // what this row hides, for the human note ("a local smol VM", "a hetzner server")
	stale       bool         // a hub machine whose hub did not answer: --plain says unreachable, the human table unknown
	m           *config.Machine
}

// fwdSummary is what status knows about a machine's forward daemon.
type fwdSummary struct {
	daemon bool              // a daemon answered
	state  string            // session.StateUp | session.StateReconnecting
	n      int               // forwards it owns (live or pending), any owner
	conf   int               // of those, the ones a conf owner holds: --plain's N
	since  time.Time         // when state began
	list   []session.Forward // the daemon's forwards (any owner): the human FORWARDS column and -v
}

// runStatusAll lists every machine (registry ∪ live smol ∪ the hubs'
// listings), grouped by backend, as one table (renderStatusTable). verbose
// adds the provider record, the full reach and per-machine forward detail
// under each row. local stops at this host.
func (a *App) runStatusAll(verbose, local bool) error {
	rows, err := a.gatherRows(statusScope{local: local, probe: !local})
	if bc, ok := asBrokenConf(err); ok {
		fmt.Fprintf(a.Stderr, "devvm: %v\n", bc.err)
		fmt.Fprintf(a.Stdout, "broken conf: %v\n", bc.err)
		return nil
	}
	if err != nil {
		return err
	}
	if !local {
		// The live round refreshed the reach and provider caches a running
		// `status --watch` reads, and its debounce re-resolves a symlinked
		// devvm.toml (kqueue may have dropped the relink): wake it. The menu
		// bar app runs this on every menu open, so that is the user's refresh.
		config.TouchChanged(a.Paths)
	}
	if !local {
		a.noteNoToken()
	}
	sources := a.providerSourceLines(local)
	var unlabelled []string
	if !local {
		unlabelled = a.unlabelledLines()
	}
	if len(rows) == 0 {
		fmt.Fprintln(a.Stdout, "No machines. Create one with 'devvm create'.")
		if verbose {
			for _, l := range sources {
				fmt.Fprintln(a.Stdout, l)
			}
		}
		for _, l := range unlabelled {
			fmt.Fprintln(a.Stdout, l)
		}
		return nil
	}

	var sections []statusSection
	for _, g := range statusGroups {
		sec := statusSection{title: g.title}
		for _, r := range rows {
			if groupOf(r) == g.backend {
				sec.rows = append(sec.rows, r)
			}
		}
		src, isProvider := sources[g.backend]
		if len(sec.rows) == 0 && !(verbose && isProvider) {
			continue
		}
		if isProvider {
			// The listing's age and health, once per section: the rows
			// are its, however old (providers.md §5). Without -v only
			// when something is wrong with it.
			src = strings.TrimPrefix(src, g.backend+": ")
			sec.problem = !strings.HasPrefix(src, "listed ")
			// No token is already noteNoToken's line on stderr.
			if verbose || (sec.problem && readProviderCache(a.Paths, g.backend).err != providerErrNoToken) {
				sec.remark = src
			}
		}
		sections = append(sections, sec)
	}
	a.renderStatusTable(sections, verbose)
	if len(unlabelled) > 0 {
		fmt.Fprintln(a.Stdout)
		for _, l := range unlabelled {
			fmt.Fprintln(a.Stdout, l)
		}
	}
	return nil
}

// providerSourceLines is each configured provider's `status -v` line
// (providerSourceLine), keyed by provider; none with --local, which skips
// providers. Read from the caches only.
func (a *App) providerSourceLines(local bool) map[string]string {
	out := map[string]string{}
	if local {
		return out
	}
	f, err := config.LoadFile(a.Paths)
	if err != nil {
		return out
	}
	now := time.Now()
	for _, pn := range f.ProviderNames() {
		out[pn] = providerSourceLine(pn, readProviderCache(a.Paths, pn), now)
	}
	return out
}

// runStatusPlain emits one tab-separated row per machine — name, backend,
// state, forward state, ownership and last contact — with no headers or grouping, so
// scripts can enumerate machines (e.g. every running smol VM) without
// scraping the human-formatted table. The token sets are the format's
// contract:
//
//	state:    running | stopped | starting | stopping | reachable | unreachable | broken conf | ?
//	forwards: up:N | reconnecting:N | down | -
//	managed:  managed | shadowed | -
//	verified: verified=<unix>:<how> | verified=- | -
//
// The fifth column was appended (config.md §7), so a consumer reading four
// fields still works. `managed` is a smol VM or provider server devvm made
// (its devvm=true label; one without it has no row at all); `shadowed` is
// an entry hiding a local smol VM of the same name (the entry wins for
// every command); `-` is an entry (remote or hub: devvm never shapes one).
// A hub machine's column is what its hub sent, passed through.
//
// N is the number of configured (`conf`-owned) forwards the daemon holds; a
// forward only a session or a ttl holds is not counted (browser-bridge.md
// §8: a login's callback would otherwise flicker the menubar badge), and
// N is never 0: with no conf-owned forward the column reads as if no daemon
// answered. `down` means ports are configured but none is up (the normal
// state of a stopped VM, so a UI should only flag it on a running machine);
// `-` means nothing is configured.
// A remote box (backend `remote`) is `reachable` when a
// live ControlMaster answered or its last probe connected, `unreachable`
// when its last probe failed, `?` when it was never probed or cannot be (an
// alias behind ProxyJump/ProxyCommand): its reach cache, which this
// listing refreshes with a live round first (providers.md §5). The sixth
// column, appended like the fifth, is when and how that was last
// established: `verified=<unix seconds>:<how>`, how ∈ {master, probe, or
// the command that talked to the box: attach, shell, exec, setup}, or
// `verified=-` for a remote box with nothing established; a hub machine
// carries its hub's sixth column through (a newer hub's `verified=…`), and
// every other row has `-`. `?` otherwise means the backend could not be
// asked. A devvm.toml that does not parse renders no machine rows (an
// entry shadows a smol VM, and a broken file cannot say which) but the one
// row brokenConfRow, `-` everywhere but state `broken conf`, with the parse
// error on stderr and a zero exit: the menu bar app shows the state rather
// than a failed listing. A hub's broken row never becomes a HUB/NAME row
// (`-` is no valid name, so parseHubRows drops it). Consumers should treat any other token as "unknown", not fail.
// `--watch` re-emits this listing on devvm-made changes (see watchStatus); the
// format is the same, so a consumer parses one thing.
//
// Hubs add no column: a hub's own row is `HUB<TAB>hub<TAB>reachable|unreachable`,
// and a machine on it is `HUB/NAME` with the backend and state the hub
// reported (a hub's remote box that is also an
// entry here is not repeated, since the direct path is the right one) (`unreachable` for every row of a hub that did not answer, from
// the cache) and this host's own forwards column. A consumer that wants to
// group by hub splits column 1 on `/`. --local lists this host only.
//
// A provider server (backend `hetzner`) is a row from the provider's last
// listing (cache/provider-NAME.list, refreshed by the live round first):
// state running | stopped | starting | stopping | ? mapped from the
// provider's own status, the fifth column managed (only a server with the
// devvm=true label is listed at all), the sixth its last contact as for a
// remote box. A provider
// that cannot be listed (no token, an API error) keeps its cached rows with
// their last state; it never reads `unreachable`, which would say the box
// is down. A server whose name an entry or a local smol VM holds has no row
// (the other row reads `shadowed`). --local lists no provider.
func (a *App) runStatusPlain(local bool) error {
	rows, err := a.gatherRows(statusScope{local: local, probe: !local})
	if bc, ok := asBrokenConf(err); ok {
		fmt.Fprintf(a.Stderr, "devvm: %v\n", bc.err)
		fmt.Fprint(a.Stdout, brokenConfRow)
		return nil
	}
	if err != nil {
		return err
	}
	if !local {
		// The live round refreshed the reach and provider caches a running
		// `status --watch` reads, and its debounce re-resolves a symlinked
		// devvm.toml (kqueue may have dropped the relink): wake it. The menu
		// bar app runs this on every menu open, so that is the user's refresh.
		config.TouchChanged(a.Paths)
	}
	if !local {
		a.noteNoToken()
	}
	for _, r := range rows {
		fmt.Fprint(a.Stdout, plainLine(r))
	}
	return nil
}

// brokenConfRow is the whole --plain listing while devvm.toml does not
// parse (see runStatusPlain).
const brokenConfRow = "-\t-\tbroken conf\t-\t-\t-\n"

// brokenConfError is gatherRows' error for a devvm.toml that does not
// parse: the one failure status renders (brokenConfRow) instead of
// failing.
type brokenConfError struct{ err error }

func (e brokenConfError) Error() string { return e.err.Error() }
func (e brokenConfError) Unwrap() error { return e.err }

func asBrokenConf(err error) (brokenConfError, bool) {
	var bc brokenConfError
	ok := errors.As(err, &bc)
	return bc, ok
}

// groupOf is the status section a row renders in: a hub and every machine
// on it share the hub section, whatever backend the hub reports for them.
func groupOf(r statusRow) string {
	if r.hub != "" {
		return config.BackendHub
	}
	return r.backend
}

// plainLine is one --plain row, shared by the one-shot listing and --watch.
func plainLine(r statusRow) string {
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\n", r.name, r.backend, r.state, plainForwards(r), plainManaged(r), plainVerified(r))
}

// plainVerified renders the sixth column (see runStatusPlain).
func plainVerified(r statusRow) string {
	switch {
	case r.reach != nil:
		return r.reach.Plain()
	case r.hubVerified != "":
		return r.hubVerified
	}
	return "-"
}

// plainManaged renders the fifth column (see runStatusPlain).
func plainManaged(r statusRow) string {
	if r.managed == "" {
		return ownNone
	}
	return r.managed
}

// plainForwards renders the machine-readable forward column (see runStatusPlain).
func plainForwards(r statusRow) string {
	switch {
	case r.fwds.daemon && r.fwds.conf > 0:
		return fmt.Sprintf("%s:%d", r.fwds.state, r.fwds.conf)
	case r.saved:
		return "down"
	default:
		return "-"
	}
}

// statusSection is one backend's part of the human listing: its title, a
// remark beside it (a provider listing's age or failure), and its rows.
type statusSection struct {
	title   string
	remark  string
	problem bool // the remark is a failure, not an age
	rows    []statusRow
}

// renderStatusTable prints every section as one table: the widths come
// from every row at once, so the columns line up across sections whatever
// the names, hosts or states. The column header sits under the first
// title; -v detail lines go under their row, outside the table.
func (a *App) renderStatusTable(sections []statusSection, verbose bool) {
	st := styleFor(a.Stdout)
	now := time.Now()
	table := [][]cell{{{text: "NAME", a: faint}, {text: "STATE", a: faint}, {text: "DETAIL", a: faint}, {text: "FORWARDS", a: faint}, {text: "ADDRESS", a: faint}}}
	for _, sec := range sections {
		for i, r := range sec.rows {
			table = append(table, []cell{
				nameCell(sec.rows, i),
				stateCell(r, verbose),
				detailCell(sec.rows, i, now),
				a.forwardsCell(r),
				{text: r.addressText(), a: faint},
				noteCell(r),
			})
		}
	}
	lines := renderTable(st, nil, table)
	header, lines := lines[0], lines[1:]
	for si, sec := range sections {
		if si > 0 {
			fmt.Fprintln(a.Stdout)
		}
		title := st.paint(heading, sec.title)
		if sec.remark != "" {
			ra := faint
			if sec.problem {
				ra = yellow
			}
			title += "  " + st.paint(ra, sec.remark)
		}
		fmt.Fprintln(a.Stdout, title)
		if si == 0 {
			fmt.Fprintln(a.Stdout, "  "+header)
		}
		for _, r := range sec.rows {
			fmt.Fprintln(a.Stdout, "  "+lines[0])
			lines = lines[1:]
			if verbose {
				for _, l := range a.verboseDetail(r) {
					fmt.Fprintln(a.Stdout, "    "+st.paint(faint, l))
				}
			}
		}
	}
}

// nameCell is a row's name, bold; a hub machine's is drawn as a branch of
// its hub's row, the glyph and the hub prefix faint (HUB/NAME stays whole,
// since that is what a command takes).
func nameCell(rows []statusRow, i int) cell {
	r := rows[i]
	if r.hub == "" || r.name == r.hub {
		return cell{text: r.name, a: bold}
	}
	glyph := "└ "
	if i+1 < len(rows) && rows[i+1].hub == r.hub && rows[i+1].name != r.hub {
		glyph = "├ "
	}
	hub, name, _ := strings.Cut(r.name, "/")
	return cell{lead: glyph + hub + "/", text: name, a: bold}
}

// stateCell is the STATE column: a glyph and the state, in a colour that
// says the same (green up, faint stopped, yellow in between or unknown,
// red unreachable), so it reads with colour off too.
func stateCell(r statusRow, verbose bool) cell {
	if r.stale {
		return cell{text: "· unknown", a: faint}
	}
	switch r.state {
	case "running", "reachable":
		return cell{text: "● " + r.state, a: accent}
	case "stopped":
		// providers.md §3: with -v a stopped server reads "stopped
		// (billing)", since it still bills (never in --plain).
		if verbose && r.m != nil && r.m.IsProvider() && r.state == provider.StateStopped {
			return cell{text: "○ stopped (billing)", a: yellow}
		}
		return cell{text: "○ stopped", a: faint}
	case "starting", "stopping":
		return cell{text: "◐ " + r.state, a: yellow}
	case "unreachable":
		return cell{text: "✗ unreachable", a: red}
	case "?", "":
		return cell{text: "? unknown", a: yellow}
	}
	return cell{text: "? " + r.state, a: yellow}
}

// detailCell is the DETAIL column, whatever says most about the row's
// kind of machine: a local VM's resources, a server's type and location,
// a remote box's last contact, a hub's machine count, a hub machine's
// backend as its hub reported it.
func detailCell(rows []statusRow, i int, now time.Time) cell {
	r := rows[i]
	switch {
	case r.hub != "" && r.name == r.hub:
		n := 0
		for _, o := range rows {
			if o.hub == r.hub && o.name != r.hub {
				n++
			}
		}
		text := fmt.Sprintf("%d machines", n)
		if n == 1 {
			text = "1 machine"
		}
		if r.state == "unreachable" {
			// Its rows are from the last listing and this host's own ports
			// and sockets (hubRows): say so here, not on a line of its own
			// that would split the tree.
			return cell{text: "no answer", a: faint}
		}
		if n == 0 {
			return cell{}
		}
		return cell{text: text, a: faint}
	case r.hub != "" && r.stale && r.backend == config.BackendHub:
		return cell{} // only this host names it, and the hub did not answer to say whether it does
	case r.hub != "":
		return cell{text: hubKind(r.backend), a: faint}
	case r.backend == config.BackendSmol:
		return cell{text: smolSize(r)}
	case r.m != nil && r.m.IsProvider():
		var parts []string
		for _, p := range []string{r.m.Server.Type, r.m.Server.Location} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		return cell{text: strings.Join(parts, " · ")}
	case r.reach != nil:
		return cell{text: r.reach.Brief(now), a: faint}
	}
	return cell{}
}

// hubKind is a hub machine's backend as the table says it.
func hubKind(backend string) string {
	switch backend {
	case config.BackendSmol:
		return "vm"
	case config.BackendHub:
		return "not listed by hub" // only this host's forwards name it
	}
	return backend
}

// smolSize is a local VM's resources, smolvm's own record: "4 cpu · 8G ram
// · 40G disk", any part it does not report left out.
func smolSize(r statusRow) string {
	var parts []string
	if r.cpus > 0 {
		parts = append(parts, fmt.Sprintf("%d cpu", r.cpus))
	}
	if r.mem > 0 {
		if r.mem%1024 == 0 {
			parts = append(parts, fmt.Sprintf("%dG ram", r.mem/1024))
		} else {
			parts = append(parts, fmt.Sprintf("%dM ram", r.mem))
		}
	}
	if r.disk > 0 {
		parts = append(parts, fmt.Sprintf("%dG disk", r.disk))
	}
	return strings.Join(parts, " · ")
}

// forwardsCell is the FORWARDS column: the guest ports the daemon holds
// (cyan), marked ↻ in yellow while it reconnects; with no daemon, how many
// this host has saved (faint: `ports up` or an attach raises them).
func (a *App) forwardsCell(r statusRow) cell {
	if len(r.fwds.list) > 0 {
		ports := forwardPorts(r.fwds.list)
		if r.fwds.state == session.StateReconnecting {
			return cell{text: "↻ " + ports, a: yellow}
		}
		return cell{text: ports, a: cyan}
	}
	if saved, _ := a.savedPorts(r.name); len(saved) > 0 {
		return cell{text: fmt.Sprintf("%d saved", len(saved)), a: faint}
	}
	return cell{}
}

// forwardPorts lists forwards by guest port, "3000" when the host port is
// the same and "3000→3001" when it was bumped, at most four then "+N".
func forwardPorts(fs []session.Forward) string {
	fs = slices.Clone(fs)
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].Guest != fs[j].Guest {
			return fs[i].Guest < fs[j].Guest
		}
		return fs[i].Host < fs[j].Host
	})
	const most = 4
	var parts []string
	for i, f := range fs {
		if i == most {
			parts = append(parts, fmt.Sprintf("+%d", len(fs)-most))
			break
		}
		p := strconv.Itoa(f.Guest)
		if f.Host != f.Guest {
			p += "→" + strconv.Itoa(f.Host)
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " ")
}

// addressText is the ADDRESS column: how a box is reached over ssh; a hub
// machine leaves it to its hub's row, a local VM has none.
func (r statusRow) addressText() string {
	if r.hub != "" && r.name != r.hub {
		return ""
	}
	if r.host == "-" {
		return ""
	}
	return r.host
}

// noteCell is the human table's trailing remark: an entry shadowing a VM
// (yellow: something is hidden), the hub entry naming this host.
func noteCell(r statusRow) cell {
	switch {
	case r.note != "":
		return cell{text: "(" + r.note + ")", a: faint}
	case r.managed == ownShadowed:
		what := strings.Join(r.shadows, " and ")
		if what == "" {
			what = "a local smol VM"
		}
		return cell{text: "(shadows " + what + " of the same name)", a: yellow}
	}
	return cell{}
}

// verboseDetail is what -v prints under a machine's row: a server's
// provider record, a remote box's full reach, and each live forward. The row's own cells already carry a VM's sizes
// and a hub machine's backend.
func (a *App) verboseDetail(r statusRow) []string {
	var out []string
	if r.m != nil && r.m.IsProvider() {
		s := r.m.Server
		out = append(out, fmt.Sprintf("server: %s in %s, %s (id %d, %s status %s)",
			dash(s.Type), dash(s.Location), dash(s.Address), s.ID, r.backend, dash(s.Status)))
		if r.reach != nil {
			// Not probed: the state is the API's; this is the last time a
			// command (attach, shell, exec, setup) got through.
			out = append(out, "contact: "+providerContact(*r.reach))
		}
	} else if r.reach != nil {
		out = append(out, "reach: "+r.reach.Describe(time.Now()))
	}
	f := r.fwds
	if len(f.list) > 0 {
		if f.state == session.StateReconnecting {
			out = append(out, fmt.Sprintf("forwards: reconnecting (since %s)", sinceHuman(f.since)))
		} else {
			out = append(out, "forwards:")
		}
		for _, fw := range f.list {
			out = append(out, fmt.Sprintf("  localhost:%d -> %s:%d%s", fw.Host, r.name, fw.Guest, forwardSuffix(fw)))
		}
	}
	return out
}

// statusScope says how far a listing reaches. local stops at this host: no
// hub is read, nothing is dialed (hub.md §5: it is what a hub runs for
// *its* callers, so hubs never fan out). Otherwise every hub entry
// contributes its rows — fetched now when listings is nil, or, under
// --watch, the latest block from each held pipe.
//
// probe runs a live round first (liveRound: the remote boxes probed, every
// provider listed; the one-shot listing: a human typed it and wants fresh),
// in parallel with the hubs' listings so they all cost one deadline. Without it, as on every --watch
// snapshot, a remote row is its reach cache and nothing is dialled.
type statusScope struct {
	local    bool
	probe    bool
	hubs     []*config.Machine // the hub entries, when the caller already loaded them (--watch); nil loads them here
	listings hubListings
}

// gatherRows resolves every `[machines.*]` entry plus every local smol VM
// into a statusRow, then (unless local) each hub's block: its own row and a
// row per machine on it (hubRows). A devvm.toml that does not parse is the
// error, and no rows: see runStatusPlain.
//
// smolvm is listed once per call and every smol row is derived from that one
// listing (rather than the backend's per-machine Status probe): a snapshot is
// re-taken on every --watch event, and N+1 `smolvm machine ls` subprocesses
// per change would be the polling this command exists to avoid. The same
// listing says which VMs are devvm's (their label, the smol source's rule)
// and carries their sizes.
func (a *App) gatherRows(sc statusScope) ([]statusRow, error) {
	f, err := config.LoadFile(a.Paths)
	if err != nil {
		return nil, brokenConfError{err}
	}
	if sc.probe {
		probed := make(chan struct{})
		go func() { defer close(probed); a.liveRound(context.Background()) }()
		if !sc.local && sc.listings == nil {
			if sc.hubs == nil {
				sc.hubs = a.hubConfs()
			}
			sc.listings = a.fetchHubListings(context.Background(), sc.hubs)
		}
		<-probed
	}
	var rows []statusRow
	smols := smolSnapshot()
	for _, m := range f.Machines() {
		if m.IsHub() {
			continue // its row comes with its listing
		}
		rows = append(rows, a.rowFor(m, smols))
	}
	// Local smol VMs with no entry: discovered (config.md §2), devvm's
	// own only (smolSnapshot). One shadowed by an entry has no row of its
	// own.
	for _, sm := range smols.list {
		if _, ok := f.Entries[sm.Name]; ok {
			continue
		}
		r := smolRow(statusRow{name: sm.Name, backend: config.BackendSmol, managed: ownManaged}, sm)
		r.fwds = a.forwardSummary(sm.Name)
		r.saved = a.hasSavedPorts(sm.Name)
		rows = append(rows, r)
	}
	if sc.local {
		return rows, nil
	}
	rows = a.providerRows(f, smols, rows)
	hubs := sc.hubs
	if hubs == nil {
		hubs = a.hubConfs()
	}
	listings := sc.listings
	if listings == nil {
		listings = a.fetchHubListings(context.Background(), hubs)
	}
	for _, hub := range hubs {
		hr := a.hubRows(hub, listings[hub.Name], f.Entries)
		smols.shadow(&hr[0]) // the hub's own row: an entry like any other
		if isThisHost(hub.Name) {
			// Cosmetic only (config.md §1): the shared file names this host
			// as a hub; nothing behaves differently for it.
			hr[0].note = "this host"
		}
		rows = append(rows, hr...)
	}
	markShadowsProviders(a.providerCachedNames(f), rows)
	return rows, nil
}

// providerRows appends a row per server in each configured provider's
// cached listing (never a live call: gatherRows' probe is what refreshes
// the cache first). A server whose name an entry or a local smol VM holds
// gets no row: that one wins for every command (providers.md §2), and its
// own row says `shadowed` (markShadowsProviders).
func (a *App) providerRows(f *config.File, smols smolMachines, rows []statusRow) []statusRow {
	for _, pn := range f.ProviderNames() {
		for _, s := range readProviderCache(a.Paths, pn).servers {
			if _, ok := f.Entries[s.Name]; ok {
				continue
			}
			if _, ok := smols.byName[s.Name]; ok {
				continue
			}
			m := providerRecord(pn, s)
			r := statusRow{name: s.Name, backend: pn, state: s.State, host: dash(s.Address), m: m,
				managed: ownManaged}
			if r.state == "" {
				r.state = provider.StateUnknown
			}
			e := reach.Read(a.Paths, s.Name)
			r.reach = &e
			r.fwds = a.forwardSummary(s.Name)
			r.saved = a.hasSavedPorts(s.Name)
			rows = append(rows, r)
		}
	}
	return rows
}

// markShadowsProviders marks every local row (an entry, a hub's own row, a
// smol VM) whose name a provider's cached listing also has: that server is
// hidden behind it, which the fifth column says rather than hides.
func markShadowsProviders(cached map[string][]string, rows []statusRow) {
	if len(cached) == 0 {
		return
	}
	for i := range rows {
		r := &rows[i]
		if strings.Contains(r.name, "/") || (r.m != nil && r.m.IsProvider()) {
			continue
		}
		for _, pn := range sortedNames(cached) {
			if slices.Contains(cached[pn], r.name) {
				r.managed = ownShadowed
				r.shadows = append(r.shadows, "a "+pn+" server")
			}
		}
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// hubRows renders one hub: its own row, then a row per machine on it. In
// the human table the hub's row carries its ssh host and each machine's
// DETAIL the backend the hub reported for it (renderStatusTable). The
// machine set is the union of what the hub listed (fresh, or cached when it
// did not answer) and what this host knows on its own — saved forwards
// (ports/HUB@NAME), a live run/HUB@NAME.sock — so a laptop-side forward for a machine
// the hub no longer lists still shows, with state `?` (the hub answered and
// did not name it; `delete HUB/NAME` clears it) and backend `hub` (nothing
// reported one). State and backend otherwise come from the hub's row, the
// forwards column from this host's own daemon for HUB@NAME, never the
// hub's. With no fresh answer every row of the hub is `unreachable`,
// whatever the cache remembers: a cached `running` would be a claim nobody
// is making.
//
// local is this host's own entries: a hub's row for a remote box this host
// also has an entry for is hidden (config.md §7: the direct path is the
// right one, and the hub is not a jump host), unless this host has forwards
// for the HUB/NAME form. nil hides nothing.
func (a *App) hubRows(hub *config.Machine, l hubListing, local map[string]config.Entry) []statusRow {
	state := "reachable"
	if !l.fresh {
		state = "unreachable"
	}
	rows := []statusRow{{name: hub.Name, backend: config.BackendHub, hub: hub.Name, state: state, host: hub.SSHHost, m: hub, managed: ownNone}}
	byName := map[string]hubRow{}
	names := map[string]bool{}
	for _, r := range l.rows {
		// By name, and only a remote row against a remote entry: the
		// listing carries no host to compare, and a hub entry of that name
		// here is another thing entirely.
		if e, dup := local[r.name]; dup && e.Backend == config.BackendRemote && r.backend == config.BackendRemote {
			display := config.JoinHubName(hub.Name, r.name)
			if !a.hasSavedPorts(display) && !a.forwardSummary(display).daemon {
				continue
			}
		}
		byName[r.name], names[r.name] = r, true
	}
	for _, display := range append(savedHubPorts(a.Paths), liveHubSockets(a.Paths)...) {
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
			m:     &config.Machine{Name: display, Hub: hub, Backend: config.BackendHub},
			saved: a.hasSavedPorts(display),
		}
		r.managed = ownNone
		if hr, ok := byName[n]; ok {
			// The hub's own word on ownership (its label), passed through.
			r.backend, r.state, r.managed = hr.backend, hr.state, hr.managed
			// A newer hub's sixth column, passed through as the hub said
			// it (its remote boxes' last contact, from the hub's cache).
			if v, _, _ := strings.Cut(hr.rest, "\t"); strings.HasPrefix(v, "verified=") {
				r.hubVerified = v
			}
		} else {
			r.backend, r.state = config.BackendHub, "?"
		}
		if !l.fresh {
			r.state, r.stale = "unreachable", true
		}
		r.fwds = a.forwardSummary(display)
		rows = append(rows, r)
	}
	return rows
}

// smolMachines is one `smolvm machine ls` taken for a whole status
// snapshot, the VMs with devvm's label only (simplify.md step 15): a VM
// devvm did not create is invisible to it, so it has no row, no
// completion, and shadows nothing.
type smolMachines struct {
	list   []backend.SmolMachine
	byName map[string]backend.SmolMachine
}

func smolSnapshot() smolMachines {
	s := smolMachines{byName: map[string]backend.SmolMachine{}}
	if !backend.SmolAvailable() {
		return s // no smolvm: no smol rows
	}
	all, _ := backend.SmolList()
	for _, sm := range all {
		if sm.Labelled() {
			s.list = append(s.list, sm)
			s.byName[sm.Name] = sm
		}
	}
	return s
}

// shadow marks a entry's row when a local smol VM has the same
// name (config.md §2): the entry wins for every command and the VM gets no
// row of its own, so the collision is said here rather than hidden.
func (s smolMachines) shadow(r *statusRow) {
	if _, ok := s.byName[r.name]; ok {
		r.managed = ownShadowed
		r.shadows = append(r.shadows, "a local smol VM")
	}
}

// smolRow fills a row's state and sizes from the VM's listing row: sizes are
// smolvm's own (config.md §3), for running and stopped VMs alike.
func smolRow(r statusRow, sm backend.SmolMachine) statusRow {
	r.state = smolStateLabel(sm)
	r.mem, r.disk, r.cpus = sm.MemoryMiB, sm.StorageGB, sm.CPUs
	return r
}

// hasSavedPorts reports whether this host has saved forwards for name,
// which is what lets --plain say `down` for a stopped VM: the list is on
// the host, so no daemon or guest needs to answer.
func (a *App) hasSavedPorts(name string) bool {
	saved, _ := a.savedPorts(name)
	return len(saved) > 0
}

func (a *App) rowFor(m *config.Machine, smols smolMachines) statusRow {
	// An entry is never managed: devvm only connects to it (`-`).
	r := statusRow{name: m.Name, backend: m.Backend, m: m, host: m.SSHHost, saved: a.hasSavedPorts(m.Name), managed: ownNone}
	smols.shadow(&r)
	// The reach cache, never a live call: this runs on every --watch
	// snapshot (gatherRows' probe is what refreshes it first).
	e := reach.Read(a.Paths, m.Name)
	r.reach = &e
	r.state = e.State
	r.fwds = a.forwardSummary(m.Name)
	return r
}

// smolStateLabel is a listed VM's state token: running, or stopped for
// anything else smolvm reports (a VM it lists exists).
func smolStateLabel(sm backend.SmolMachine) string {
	if sm.Running() {
		return "running"
	}
	return "stopped"
}

// forwardSummary asks a machine's daemon for its state and forward count; a
// zero value means no daemon answered.
func (a *App) forwardSummary(name string) fwdSummary {
	cl, err := session.Existing(a.Paths, name)
	if err != nil {
		return fwdSummary{}
	}
	st, err := cl.Status()
	if err != nil {
		return fwdSummary{}
	}
	return fwdSummary{daemon: true, state: st.State, n: len(st.Forwards), conf: st.ConfCount(), since: st.Since, list: st.Forwards}
}

// runStatus is the single-machine drill-in: always detailed (state, lifecycle,
// smolvm's sizes for smol, and live forwards).
func (a *App) runStatus(name string) error {
	m, b, err := a.resolveAny(name) // a hub and a hub machine both have a status
	if err != nil {
		return err
	}
	if m.IsHubMachine() {
		return a.runStatusHubMachine(m)
	}
	if sm, ok := backend.SmolRow(b); ok {
		// The row resolve already listed: state and sizes with no second
		// `smolvm machine ls`, and no guest exec (a stopped VM has sizes
		// too).
		fmt.Fprintf(a.Stdout, "%s (%s)\n", m.Name, m.Backend)
		fmt.Fprintf(a.Stdout, "  lifecycle: %s\n", lifecycleTrack(smolLifecycle, smolStateLabel(sm)))
		fmt.Fprintf(a.Stdout, "  resources: %d CPUs, %s RAM, %s disk\n", sm.CPUs, memHuman(sm.MemoryMiB), diskHuman(sm.StorageGB))
	} else {
		// An entry or a provider server: no smolvm call (a shadowed VM shows in the
		// listing's fifth column, not here).
		st, err := b.Status()
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "%s (%s)\n", m.Name, m.Backend)
		fmt.Fprintf(a.Stdout, "  state: %s\n", st.Raw)
		if m.IsProvider() {
			s := m.Server
			fmt.Fprintf(a.Stdout, "  server: %s in %s, %s\n", dash(s.Type), dash(s.Location), dash(s.Address))
			if s.State == provider.StateStopped {
				fmt.Fprintf(a.Stdout, "  billing: stopped but still billed by %s\n", providerTitle(m.Backend))
			}
			fmt.Fprintf(a.Stdout, "  contact: %s\n", providerContact(reach.Read(a.Paths, m.Name)))
		}
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
	for _, r := range a.hubRows(m.Hub, l, nil) {
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

// providerContact is a provider box's last-contact text: the reach cache's
// words once a command got through, else "none recorded yet" (it is never
// probed, so "not probed" would read as a gap to fill).
func providerContact(e reach.Entry) string {
	if e.Verified.IsZero() {
		return "none recorded yet"
	}
	return e.Describe(time.Now())
}
