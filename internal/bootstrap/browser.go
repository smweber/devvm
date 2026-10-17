package bootstrap

import (
	"context"
	"fmt"
	"strings"

	"github.com/smweber/devvm/internal/agentbin"
	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// ProfilePath is the guest file that exports BROWSER to login shells that
// devvm did not start itself (cron, a login from inside the box, an exec
// with `bash -l`).
const ProfilePath = "/etc/profile.d/devvm.sh"

// installAgent is agentbin.Install, a var so a test can stand in for it.
var installAgent = agentbin.Install

// Browser installs the agent and its BROWSER shim on a managed smol box and
// writes ProfilePath pointing at the shim (browser-bridge.md §5). The
// install happens here, at setup, and not only lazily when the daemon's
// transport first spawns the agent exec, so profile.d never names a file
// that does not exist yet. One-shot execs only, in sequence: nothing here
// runs beside another exec.
//
// Every other backend is left alone: remote boxes have no agent exec to
// answer the shim yet (their plumbing is deferred with it), an adopt host
// never gets a profile.d write, and a hub machine is set up by its
// hub, which runs this itself.
func Browser(ctx context.Context, b backend.Backend, m *config.Machine) error {
	if m.IsRemote() || !m.Managed() {
		return nil
	}
	p, err := installAgent(ctx, b, m, nil)
	if err != nil {
		return fmt.Errorf("install devvm-agent and browser shim: %w", err)
	}
	return b.Run(ctx, backend.ExecOpts{User: "root"}, "sh", "-c", writeProfileScript(p.Shim))
}

// profileScript is ProfilePath's content. The guard keeps it harmless: a
// BROWSER the user (or devvm's own exec Env) already set wins, and a
// missing shim exports nothing, since a BROWSER naming no file makes gh fail
// outright instead of printing the URL. if/fi rather than `a && b`, so the
// file never leaves a false status behind in the shell that sources it.
// sh/bash only; zsh and fish get BROWSER from the tmux session environment
// on attach, or set it themselves.
func profileScript(shim string) string {
	q := sq(shim)
	return "# Written by devvm setup: BROWSER opens URLs on the devvm host.\n" +
		`if [ -z "${BROWSER:-}" ] && [ -x ` + q + " ]; then\n" +
		"    export BROWSER=" + q + "\n" +
		"fi\n"
}

// writeProfileScript writes profileScript to ProfilePath as root, staged
// and renamed so a login never sources half a file. The lines go to printf
// as arguments, not as its format and not over stdin (a one-shot smol exec
// has none), the same way agentbin writes the shim.
func writeProfileScript(shim string) string {
	lines := strings.Split(strings.TrimSuffix(profileScript(shim), "\n"), "\n")
	q := make([]string, len(lines))
	for i, l := range lines {
		q[i] = sq(l)
	}
	tmp := ProfilePath + ".new"
	return fmt.Sprintf(`mkdir -p /etc/profile.d && printf '%%s\n' %s > %s && chmod 0644 %s && mv %s %s`,
		strings.Join(q, " "), tmp, tmp, tmp, ProfilePath)
}

// sq single-quotes s for a POSIX shell.
func sq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
