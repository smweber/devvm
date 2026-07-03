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
