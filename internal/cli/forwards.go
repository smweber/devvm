package cli

import (
	"errors"
	"fmt"
	"strconv"

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

func (a *App) reportForward(name string, host, guest, pref int, bumped bool) {
	if bumped {
		fmt.Fprintf(a.Stdout, "devvm: forwarding localhost:%d -> %s:%d (preferred %d taken)\n", host, name, guest, pref)
	} else {
		fmt.Fprintf(a.Stdout, "devvm: forwarding localhost:%d -> %s:%d\n", host, name, guest)
	}
}

func (a *App) runPort(name, mapping string) error {
	m, _, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	pref, guest, err := parseMapping(mapping)
	if err != nil {
		return err
	}
	cl, err := session.Dial(a.ConfigDir, name)
	if err != nil {
		return err
	}
	host, bumped, err := cl.Add(pref, guest)
	if err != nil {
		return err
	}
	if _, ok := findMapping(m, pref, guest); !ok {
		m.Ports = append(m.Ports, mapping)
		if err := m.Save(a.ConfigDir); err != nil {
			return err
		}
	}
	a.reportForward(name, host, guest, pref, bumped)
	return nil
}

func (a *App) runUnport(name, mapping string) error {
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

// tunnelDown stops the machine's live forwards, if any daemon is running.
func (a *App) tunnelDown(name string) error {
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
// `tunnel up`, `start`, and vnc).
func (a *App) tunnelUp(name string) error {
	m, _, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	if len(m.Ports) == 0 {
		fmt.Fprintf(a.Stdout, "devvm: no ports configured; add one with 'devvm ports add %s HOST:GUEST'\n", name)
		return nil
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
		host, bumped, aerr := cl.Add(pref, guest)
		if aerr != nil {
			fmt.Fprintf(a.Stderr, "devvm: %v\n", aerr)
			continue
		}
		a.reportForward(name, host, guest, pref, bumped)
	}
	return nil
}

func (a *App) runVNC(name string) error {
	m, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	ex, ok := b.(backend.VNCer)
	if !ok {
		return fmt.Errorf("'vnc' only applies to remote machines ('%s' is %s)", name, m.Backend)
	}
	return ex.VNC(func() error { return a.tunnelUp(name) })
}

// forwardReport lists a machine's live forwards for `status`, or nothing if no
// daemon is running.
func (a *App) forwardReport(name string) {
	cl, err := session.Existing(a.ConfigDir, name)
	if err != nil {
		return
	}
	fwds, err := cl.List()
	if err != nil || len(fwds) == 0 {
		return
	}
	fmt.Fprintln(a.Stdout, "  forwards:")
	for _, f := range fwds {
		fmt.Fprintf(a.Stdout, "    guest %-5d -> localhost:%d\n", f.Guest, f.Host)
	}
}
