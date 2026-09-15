package cli

import (
	"errors"
	"fmt"
	"strconv"
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
	if err := requireRunningForForwards(m, b); err != nil {
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
			return fmt.Errorf("no forward '%s' configured for '%s' (have: %v)", mapping, name, m.Ports)
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
	// Tear down the live forward if a daemon is running.
	if guest != 0 {
		if cl, derr := session.Existing(a.ConfigDir, name); derr == nil {
			_ = cl.Remove(guest)
		}
	}
	fmt.Fprintf(a.Stdout, "devvm: removed forward %s from '%s'\n", configured, name)
	return nil
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
	names, _ := config.List(a.ConfigDir)
	fmt.Fprintf(a.Stdout, "%-16s %-14s %-6s %-16s %s\n", "MACHINE", "MAPPING", "GUEST", "HOST", "STATE")
	any := false
	for _, name := range names {
		m, err := config.Load(a.ConfigDir, name)
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
			host, state := "—", "down"
			if f, ok := live[guest]; ok {
				host, state = fmt.Sprintf("localhost:%d", f.Host), forwardState(f)
				delete(live, guest) // consumed; leftover live entries are ephemeral
			}
			fmt.Fprintf(a.Stdout, "%-16s %-14s %-6d %-16s %s\n", name, p, guest, host, state)
		}
		// Live forwards with no matching configured mapping (added ad hoc via up).
		for guest, f := range live {
			any = true
			fmt.Fprintf(a.Stdout, "%-16s %-14s %-6d %-16s %s\n",
				name, "(ephemeral)", guest, fmt.Sprintf("localhost:%d", f.Host), forwardState(f))
		}
	}
	if !any {
		fmt.Fprintln(a.Stdout, "devvm: no ports configured on any machine")
	}
	return nil
}

// tunnelDown stops the machine's live forwards, if any daemon is running.
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
	if err := cl.Stop(); err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, "devvm: forwards stopped")
	return nil
}

// tunnelUp brings up every configured forward for the machine (used by
// `tunnel up` and `start`).
func (a *App) tunnelUp(name string) error {
	defer config.TouchChanged(a.ConfigDir) // wake `status --watch`
	m, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	if len(m.Ports) == 0 {
		fmt.Fprintf(a.Stdout, "devvm: no ports configured; add one with 'devvm ports add %s HOST:GUEST'\n", name)
		return nil
	}
	if err := requireRunningForForwards(m, b); err != nil {
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
// It polls briefly: `devvm start` calls tunnelUp right after `smolvm machine
// start` returns, and smolvm can still report the box as starting for a
// moment. Failing there would tell the user to start a VM they just started.
func requireRunningForForwards(m *config.Machine, b backend.Backend) error {
	if m.Backend != config.BackendSmol {
		return nil
	}
	deadline := time.Now().Add(runningPollTimeout)
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

// forwardReport lists a machine's daemon-owned forwards for `ports list`, or
// nothing if no daemon is running. While the daemon is reconnecting the header
// says so (and for how long) and each forward is marked pending, so a stuck
// reconnect is visible rather than looking like healthy forwards.
func (a *App) forwardReport(name string) {
	cl, err := session.Existing(a.ConfigDir, name)
	if err != nil {
		return
	}
	st, err := cl.Status()
	if err != nil || len(st.Forwards) == 0 {
		return
	}
	if st.Reconnecting() {
		fmt.Fprintf(a.Stdout, "  forwards: reconnecting (since %s)\n", sinceHuman(st.Since))
	} else {
		fmt.Fprintln(a.Stdout, "  forwards:")
	}
	for _, f := range st.Forwards {
		suffix := ""
		if f.Pending {
			suffix = "  (pending)"
		}
		fmt.Fprintf(a.Stdout, "    guest %-5d -> localhost:%d%s\n", f.Guest, f.Host, suffix)
	}
}
