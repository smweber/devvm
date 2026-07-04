package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// resolve loads a machine and its backend, applying load_machine's fallback:
// an unregistered name that smolvm knows about is treated as a smol machine, so
// pre-registry VMs keep working.
func (a *App) resolve(name string) (*config.Machine, backend.Backend, error) {
	m, err := config.Load(a.ConfigDir, name)
	if errors.Is(err, config.ErrNotFound) {
		if err2 := config.ValidName(name); err2 != nil {
			return nil, nil, err2
		}
		if backend.SmolExists(name) {
			m = config.NewSmol(name)
		} else {
			return nil, nil, fmt.Errorf("unknown machine %q (no conf); run 'devvm status'", name)
		}
	} else if err != nil {
		return nil, nil, err
	}
	b, err := backend.For(m, a.ConfigDir)
	if err != nil {
		return nil, nil, err
	}
	return m, b, nil
}

// requireProvisioned errors if a registered machine has no live backend resource
// — a "dormant" box (conf exists, but the VM/host doesn't, e.g. after
// `deprovision`). It lets commands that need a running box fail with a clear next
// step instead of deep inside smolvm/exec. Remote backends report Exists()==true
// always, so this is a no-op there.
func requireProvisioned(m *config.Machine, b backend.Backend) error {
	ok, err := b.Exists()
	if err != nil {
		return err // e.g. smolvm not installed — surface the real cause, not "dormant"
	}
	if !ok {
		return fmt.Errorf("'%s' is not provisioned — run 'devvm provision %s'", m.Name, m.Name)
	}
	return nil
}

// resolveLive is resolve plus requireProvisioned, for the commands that need a
// running box. Note it adds one `smolvm machine status` probe, which lands on the
// exec/shell/forward hot path — cheap, but not free.
func (a *App) resolveLive(name string) (*config.Machine, backend.Backend, error) {
	m, b, err := a.resolve(name)
	if err != nil {
		return nil, nil, err
	}
	if err := requireProvisioned(m, b); err != nil {
		return nil, nil, err
	}
	return m, b, nil
}

var yesRe = regexp.MustCompile(`^[Yy]([Ee][Ss])?$`)

// confirm prompts on the controlling terminal, matching the old confirm().
func confirm(prompt string) (bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, errors.New("no terminal for confirmation; aborting")
	}
	defer tty.Close()
	fmt.Fprintf(tty, "%s [y/N] ", prompt)
	line, _ := bufio.NewReader(tty).ReadString('\n')
	return yesRe.MatchString(trimNL(line)), nil
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
