package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// parseMapping splits "HOST:GUEST" (or bare "PORT") into numeric ports. Both
// must be concrete (1-65535): port 0 would bind a random port the daemon can't
// report back (it records the preference, not the kernel's pick).
func parseMapping(mapping string) (pref, guest int, err error) {
	h, g := config.SplitPort(mapping)
	pref, err1 := strconv.Atoi(h)
	guest, err2 := strconv.Atoi(g)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("'%s' must be numeric HOST:GUEST ports", mapping)
	}
	if pref < 1 || pref > 65535 || guest < 1 || guest > 65535 {
		return 0, 0, fmt.Errorf("'%s' ports must be in 1-65535", mapping)
	}
	return pref, guest, nil
}

// findMapping returns the configured mapping equivalent to (pref, guest), so
// "8080" and "8080:8080" name the same forward.
func findMapping(m *config.Machine, pref, guest int) (string, bool) {
	for _, p := range m.Ports {
		if h, g, err := parseMapping(p); err == nil && h == pref && g == guest {
			return p, true
		}
	}
	return "", false
}

func (a *App) reportForward(name string, host, guest, pref int, bumped, pending bool) {
	switch {
	case pending:
		fmt.Fprintf(a.Stdout, "devvm: forward localhost:%d -> %s:%d pending (daemon is reconnecting; it comes up when the link is back)\n", host, name, guest)
	case bumped:
		fmt.Fprintf(a.Stdout, "devvm: forwarding localhost:%d -> %s:%d (preferred %d taken)\n", host, name, guest, pref)
	default:
		fmt.Fprintf(a.Stdout, "devvm: forwarding localhost:%d -> %s:%d\n", host, name, guest)
	}
}

// sinceHuman renders how long ago t was, coarsely ("12s", "3m", "2h5m").
func sinceHuman(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func (a *App) runPort(name, mapping string) error {
	defer config.TouchChanged(a.ConfigDir) // wake `status --watch`
	m, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	return a.addPort(m, b, mapping)
}

// addPort records a mapping in the conf and, if the VM is running, brings the
// forward up. Recording comes first: adding a port is a conf edit that must
// work on a stopped box too; only the live bind needs the VM running.
func (a *App) addPort(m *config.Machine, b backend.Backend, mapping string) error {
	pref, guest, err := parseMapping(mapping)
	if err != nil {
		return err
	}
	if _, ok := findMapping(m, pref, guest); !ok {
		m.Ports = append(m.Ports, mapping)
		if err := m.Save(a.ConfigDir); err != nil {
			return err
		}
	}
	if err := requireRunningForForwards(m, b, 0); err != nil {
		fmt.Fprintf(a.Stdout, "devvm: recorded %s; forwards come up on 'devvm start %s'\n", mapping, m.Name)
		return nil
	}
	cl, err := session.Dial(a.ConfigDir, m.Name)
	if err != nil {
		return err
	}
	host, bumped, pending, err := cl.Add(pref, guest)
	if err != nil {
		return err
	}
	a.reportForward(m.Name, host, guest, pref, bumped, pending)
	return nil
}

// runUnport is `ports rm` (browser-bridge.md §4). On a configured mapping
// it drops the conf entry and the forward's conf owner; the forward itself
// goes only if no other owner holds it. On a guest port that is not in the
// conf but has a live daemon-only forward it drops that forward's ttl owner
// (a bridge callback). It never drops a connection owner: that belongs to
// whoever holds the session, and goes when the session does.
func (a *App) runUnport(name, mapping string) error {
	defer config.TouchChanged(a.ConfigDir) // wake `status --watch`
	m, _, err := a.resolve(name)
	if err != nil {
		return err
	}
	// Match by parsed ports ("8080" == "8080:8080"); an unparseable argument
	// can still remove an identical hand-edited conf entry by exact string.
	configured, guest := mapping, 0
	if pref, g, perr := parseMapping(mapping); perr == nil {
		found, ok := findMapping(m, pref, g)
		if !ok {
			return a.unportUnconfigured(name, m, mapping, g)
		}
		configured, guest = found, g
	} else if !m.HasPort(mapping) {
		return fmt.Errorf("no forward '%s' configured for '%s' (have: %v)", mapping, name, m.Ports)
	}
	// Drop the mapping from the conf.
	kept := m.Ports[:0]
	for _, p := range m.Ports {
		if p != configured {
			kept = append(kept, p)
		}
	}
	m.Ports = kept
	if err := m.Save(a.ConfigDir); err != nil {
		return err
	}
	// Drop the live forward's conf owner if a daemon is running.
	var left *session.Forward
	if guest != 0 {
		if cl, derr := session.Existing(a.ConfigDir, name); derr == nil {
			left, _ = cl.RemoveOwner(guest, session.OwnerConf)
		}
	}
	fmt.Fprintf(a.Stdout, "devvm: removed forward %s from '%s'\n", configured, name)
	if left != nil {
		fmt.Fprintf(a.Stdout, "devvm: localhost:%d stays up: still held by %s\n", left.Host, ownerLabel(*left))
	}
	return nil
}

// unportUnconfigured handles `ports rm` of a guest port the conf does not
// list: only a live forward with a ttl owner can be removed that way.
func (a *App) unportUnconfigured(name string, m *config.Machine, mapping string, guest int) error {
	notConfigured := fmt.Errorf("no forward '%s' configured for '%s' (have: %v)", mapping, name, m.Ports)
	cl, err := session.Existing(a.ConfigDir, name)
	if err != nil {
		return notConfigured
	}
	fwds, err := cl.List()
	if err != nil {
		return notConfigured
	}
	for _, f := range fwds {
		if f.Guest != guest {
			continue
		}
		// A ttl owner (a bridge callback) is what `ports rm` exists to drop
		// here. A conf owner the conf no longer names (a hand edit, or a
		// pre-owner daemon, which removes outright) is stale and goes too.
		// A connection owner is never ours to drop.
		var drop string
		switch {
		case f.HasOwner(session.OwnerTTL):
			drop = session.OwnerTTL
		case f.IsConf():
			drop = session.OwnerConf
		default:
			return fmt.Errorf("the forward for guest %d on '%s' is held by %s; it goes when that session ends", guest, name, ownerLabel(f))
		}
		left, err := cl.RemoveOwner(guest, drop)
		if err != nil {
			return err
		}
		if left != nil {
			fmt.Fprintf(a.Stdout, "devvm: dropped the %s owner of localhost:%d; it stays up: still held by %s\n", drop, left.Host, ownerLabel(*left))
			return nil
		}
		fmt.Fprintf(a.Stdout, "devvm: removed ephemeral forward localhost:%d -> %s:%d\n", f.Host, name, guest)
		return nil
	}
	return notConfigured
}

// ownerLabel names a forward's owner kinds for humans ("conf+connection").
func ownerLabel(f session.Forward) string {
	if len(f.Owners) == 0 {
		return "conf" // a pre-owner daemon: every forward it held was configured
	}
	return strings.Join(f.Owners, "+")
}

// forwardSuffix is what follows `guest N -> localhost:M` in `ports list` and
// `status -v`: `(pending)`, then the owner kinds and `exact`. The menubar
// app parses these lines (contrib/macos/Sources/Status.swift,
// parsePortsList): it needs the first four tokens and looks for a
// `(pending)` token anywhere after, so everything added stays after them.
func forwardSuffix(f session.Forward) string {
	suffix := ""
	if f.Pending {
		suffix += "  (pending)"
	}
	if len(f.Owners) > 0 {
		suffix += "  owner=" + ownerLabel(f)
	}
	if f.Exact {
		suffix += "  exact"
	}
	return suffix
}

// runPortsList shows the machine's configured forwards plus any that are live.
func (a *App) runPortsList(name string) error {
	m, _, err := a.resolve(name)
	if err != nil {
		return err
	}
	if len(m.Ports) == 0 {
		fmt.Fprintf(a.Stdout, "devvm: no ports configured; add one with 'devvm ports add %s HOST:GUEST'\n", name)
	} else {
		fmt.Fprintln(a.Stdout, "configured:")
		for _, p := range m.Ports {
			fmt.Fprintf(a.Stdout, "  %s\n", p)
		}
	}
	a.forwardReport(name)
	return nil
}

// runPortsListAll is the global forwarding overview (`ports list` with no NAME):
// a flat table of every machine's configured mappings and whether each is live,
// so a box with many forwards is scannable in one place instead of via status.
func (a *App) runPortsListAll() error {
	fmt.Fprintf(a.Stdout, "%-16s %-14s %-6s %-16s %-12s %s\n", "MACHINE", "MAPPING", "GUEST", "HOST", "STATE", "OWNER")
	any := false
	for _, name := range a.listMachines() {
		m, err := config.LoadAny(a.ConfigDir, name)
		if err != nil {
			continue
		}
		// guest port -> daemon-owned forward, if a daemon is up for this machine.
		// A pending forward (daemon reconnecting) shows its host port but not "up".
		live := map[int]session.Forward{}
		if cl, err := session.Existing(a.ConfigDir, name); err == nil {
			if fwds, err := cl.List(); err == nil {
				for _, f := range fwds {
					live[f.Guest] = f
				}
			}
		}
		for _, p := range m.Ports {
			any = true
			_, guestStr := config.SplitPort(p)
			guest, _ := strconv.Atoi(guestStr)
			host, state, own := "—", "down", "—"
			if f, ok := live[guest]; ok {
				host, state, own = fmt.Sprintf("localhost:%d", f.Host), forwardState(f), ownerLabel(f)
				delete(live, guest) // consumed; leftover live entries are ephemeral
			}
			fmt.Fprintf(a.Stdout, "%-16s %-14s %-6d %-16s %-12s %s\n", name, p, guest, host, state, own)
		}
		// Live forwards with no matching configured mapping: a session's, or
		// a bridge callback's (ttl).
		ephemeral := make([]int, 0, len(live))
		for guest := range live {
			ephemeral = append(ephemeral, guest)
		}
		sort.Ints(ephemeral)
		for _, guest := range ephemeral {
			any = true
			f := live[guest]
			fmt.Fprintf(a.Stdout, "%-16s %-14s %-6d %-16s %-12s %s\n",
				name, "(ephemeral)", guest, fmt.Sprintf("localhost:%d", f.Host), forwardState(f), ownerLabel(f))
		}
	}
	if !any {
		fmt.Fprintln(a.Stdout, "devvm: no ports configured on any machine")
	}
	return nil
}

// tunnelDown is `ports down` (browser-bridge.md §4): it drops every conf
// owner, and the daemon exits only if no forward and no session remains.
// With a session holding it (an attach, a hub's __session) the daemon stays
// and the configured forwards alone go down.
func (a *App) tunnelDown(name string) error {
	defer config.TouchChanged(a.ConfigDir) // wake `status --watch`
	if _, _, err := a.resolveLive(name); err != nil {
		return err
	}
	cl, err := session.Existing(a.ConfigDir, name)
	if errors.Is(err, session.ErrNoDaemon) {
		fmt.Fprintln(a.Stdout, "devvm: no forwards running")
		return nil
	}
	if err != nil {
		return err
	}
	res, err := cl.Down()
	if err != nil {
		return err
	}
	if res.Stopped {
		fmt.Fprintln(a.Stdout, "devvm: forwards stopped")
		return nil
	}
	var held []string
	if n := len(res.Forwards); n > 0 {
		held = append(held, plural(n, "forward", "forwards")+" other owners hold")
	}
	if res.Sessions > 0 {
		held = append(held, plural(res.Sessions, "session", "sessions"))
	}
	fmt.Fprintf(a.Stdout, "devvm: configured forwards stopped; the forward daemon stays up for %s\n", strings.Join(held, " and "))
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// tunnelUp brings up every configured forward for the machine (used by
// `tunnel up` and `start`).
func (a *App) tunnelUp(name string) error { return a.tunnelUpWait(name, 0) }

// tunnelUpWait is tunnelUp with a bound on how long to wait for a smol VM
// to report running. `start` passes runningPollTimeout because smolvm can
// return from `machine start` while the state is still "starting"; `ports
// up` passes 0 so a deliberately stopped VM fails fast instead of stalling
// ten seconds on forty `smolvm machine ls` calls.
func (a *App) tunnelUpWait(name string, wait time.Duration) error {
	defer config.TouchChanged(a.ConfigDir) // wake `status --watch`
	m, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	if len(m.Ports) == 0 {
		fmt.Fprintf(a.Stdout, "devvm: no ports configured; add one with 'devvm ports add %s HOST:GUEST'\n", name)
		return nil
	}
	if err := requireRunningForForwards(m, b, wait); err != nil {
		return err
	}
	cl, err := session.Dial(a.ConfigDir, name)
	if err != nil {
		return err
	}
	for _, mapping := range m.Ports {
		pref, guest, perr := parseMapping(mapping)
		if perr != nil {
			fmt.Fprintf(a.Stderr, "devvm: skipping %q: %v\n", mapping, perr)
			continue
		}
		host, bumped, pending, aerr := cl.Add(pref, guest)
		if aerr != nil {
			fmt.Fprintf(a.Stderr, "devvm: %v\n", aerr)
			continue
		}
		a.reportForward(name, host, guest, pref, bumped, pending)
	}
	// A daemon mid-backoff (the VM was just started, the link is back) should
	// not make `start`/`ports up` wait up to 30s for pending forwards.
	if st, err := cl.Status(); err == nil && st.Reconnecting() {
		if err := cl.Kick(); err == nil {
			fmt.Fprintln(a.Stdout, "devvm: forward daemon is reconnecting; retrying now")
		}
	}
	return nil
}

// requireRunningForForwards refuses to spawn a forward daemon for a stopped
// smol VM. resolveLive only checks the VM is provisioned; the daemon's first
// transport would exec into the machine, and smolvm's exec may boot one the
// user deliberately stopped. (The daemon guards its reconnect dials the same
// way; this catches the initial dial before a daemon exists.)
//
// With wait > 0 it polls: `devvm start` calls tunnelUp right after `smolvm
// machine start` returns, and smolvm can still report the box as starting for
// a moment. Failing there would tell the user to start a VM they just started.
// `ports add`/`ports up` pass 0 so a deliberately stopped VM answers at once.
func requireRunningForForwards(m *config.Machine, b backend.Backend, wait time.Duration) error {
	if m.Backend != config.BackendSmol {
		return nil
	}
	deadline := time.Now().Add(wait)
	for {
		st, err := b.Status()
		if err != nil || st.Running {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s is not running; start it first ('devvm start %s' also brings its forwards up)", m.Name, m.Name)
		}
		time.Sleep(runningPollInterval)
	}
}

// Poll knobs for requireRunningForForwards (vars so tests can shrink them).
var (
	runningPollTimeout  = 10 * time.Second
	runningPollInterval = 250 * time.Millisecond
)

// forwardState is the per-forward column value: "up", or "reconnecting" for
// one the daemon remembers but has not re-bound yet.
func forwardState(f session.Forward) string {
	if f.Pending {
		return session.StateReconnecting
	}
	return session.StateUp
}

// forwardReport lists a machine's daemon-owned forwards for `ports list`,
// each with its owner kinds, and the sessions holding the daemon, or nothing
// if no daemon is running. While the daemon is reconnecting the header
// says so (and for how long) and each forward is marked pending, so a stuck
// reconnect is visible rather than looking like healthy forwards.
func (a *App) forwardReport(name string) {
	cl, err := session.Existing(a.ConfigDir, name)
	if err != nil {
		return
	}
	st, err := cl.Status()
	if err != nil {
		return
	}
	if len(st.Forwards) > 0 {
		if st.Reconnecting() {
			fmt.Fprintf(a.Stdout, "  forwards: reconnecting (since %s)\n", sinceHuman(st.Since))
		} else {
			fmt.Fprintln(a.Stdout, "  forwards:")
		}
		for _, f := range st.Forwards {
			fmt.Fprintf(a.Stdout, "    guest %-5d -> localhost:%d%s\n", f.Guest, f.Host, forwardSuffix(f))
		}
	}
	if st.Sessions > 0 {
		fmt.Fprintf(a.Stdout, "  sessions: %d holding the forward daemon\n", st.Sessions)
	}
}
