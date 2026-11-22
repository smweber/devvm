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

// Browser installs the agent and its BROWSER shim on a box devvm made (a
// smol VM, a provider server) and writes ProfilePath pointing at the shim
// (browser-bridge.md §5). The install happens here, at setup, and not only
// lazily when the daemon's transport first spawns the agent exec, so
// profile.d never names a file that does not exist yet. One-shot execs
// only, in sequence: nothing here runs beside another exec.
//
// On a box reached over ssh profile.d is one of only two ways BROWSER
// arrives (the other is attach's tmux session environment): a bare `shell`
// sends no remote command, and sshd drops SetEnv. Only setup calls it,
// which shapes nothing but a box devvm made: an entry never gets a
// profile.d write (its agent is the user's to consent to, with agent =
// true, and lives in ~/.local/bin), and a hub machine is set up by its hub.
func Browser(ctx context.Context, b backend.Backend, m *config.Machine) error {
	p, err := installAgent(ctx, b, m)
	if err != nil {
		return fmt.Errorf("install devvm-agent and browser shim: %w", err)
	}
	return b.Run(ctx, backend.ExecOpts{User: "root", Stdin: strings.NewReader(profileScript(p.Shim))},
		"sh", "-c", writeProfileScript)
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

// writeProfileScript writes its stdin (profileScript) to ProfilePath as
// root, staged and renamed so a login never sources half a file, the same
// way agentbin writes the shim. An empty file (a transport that delivered
// no stdin) fails rather than replacing the profile.
const writeProfileScript = `mkdir -p /etc/profile.d && cat > ` + ProfilePath + `.new && [ -s ` + ProfilePath + `.new ] && chmod 0644 ` +
	ProfilePath + `.new && mv ` + ProfilePath + `.new ` + ProfilePath

// sq single-quotes s for a POSIX shell.
func sq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
