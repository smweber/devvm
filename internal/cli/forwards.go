package cli

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// parseMapping splits "HOST:GUEST" (or bare "PORT") into numeric ports.
func parseMapping(mapping string) (pref, guest int, err error) {
	h, g := config.SplitPort(mapping)
	pref, err1 := strconv.Atoi(h)
	guest, err2 := strconv.Atoi(g)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("'%s' must be numeric HOST:GUEST ports", mapping)
	}
	return pref, guest, nil
}

func reportForward(name string, host, guest, pref int, bumped bool) {
	if bumped {
		fmt.Printf("devvm: forwarding localhost:%d -> %s:%d (preferred %d taken)\n", host, name, guest, pref)
	} else {
		fmt.Printf("devvm: forwarding localhost:%d -> %s:%d\n", host, name, guest)
	}
}

func (a *App) runPort(name, mapping string) error {
	m, _, err := a.resolve(name)
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
	if !m.HasPort(mapping) {
		m.Ports = append(m.Ports, mapping)
		if err := m.Save(a.ConfigDir); err != nil {
			return err
		}
	}
	reportForward(name, host, guest, pref, bumped)
	return nil
}

func (a *App) runUnport(name, mapping string) error {
	m, _, err := a.resolve(name)
	if err != nil {
		return err
	}
	if !m.HasPort(mapping) {
		return fmt.Errorf("no forward '%s' configured for '%s' (have: %v)", mapping, name, m.Ports)
	}
	// Drop the mapping from the conf.
	kept := m.Ports[:0]
	for _, p := range m.Ports {
		if p != mapping {
			kept = append(kept, p)
		}
	}
	m.Ports = kept
	if err := m.Save(a.ConfigDir); err != nil {
		return err
	}
	// Tear down the live forward if a daemon is running.
	if _, guest, err := parseMapping(mapping); err == nil {
		if cl, derr := session.Existing(a.ConfigDir, name); derr == nil {
			_ = cl.Remove(guest)
		}
	}
	fmt.Printf("devvm: removed forward %s from '%s'\n", mapping, name)
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
	if _, _, err := a.resolve(name); err != nil {
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
	m, _, err := a.resolve(name)
	if err != nil {
		return err
	}
	if len(m.Ports) == 0 {
		fmt.Printf("devvm: no ports configured; add one with 'devvm ports add %s HOST:GUEST'\n", name)
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
		reportForward(name, host, guest, pref, bumped)
	}
	return nil
}

func (a *App) runVNC(name string) error {
	m, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	ex, ok := b.(interface {
		VNC(func() error) error
	})
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
	fmt.Println("  forwards:")
	for _, f := range fwds {
		fmt.Printf("    guest %-5d -> localhost:%d\n", f.Guest, f.Host)
	}
}
