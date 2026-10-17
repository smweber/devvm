package backend

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// tmuxSocket is the guest-side socket for the persistent dev session.
const tmuxSocket = "/home/" + DefaultUser + "/.devvm/tmux.sock"

// ManagedShimPath is the BROWSER shim (devvm-open-url) on a box devvm owns:
// agentbin.Install puts it beside the root-owned agent (a test there pins
// the two together). On smol it is installed by bootstrap and again by the
// daemon's transport before it spawns the agent exec (browser-bridge.md §5).
const ManagedShimPath = "/usr/local/bin/devvm-open-url"

// browserEnv is the ExecOpts.Env of smol's shell and attach execs: BROWSER
// pointed at the shim, so a login tool hands its URL to the daemon's bridge
// (or, with no daemon answering, the shim prints it). The guest side of
// each exec drops it again when the shim is not there (a box neither
// bootstrapped nor served since step 9): a BROWSER naming a missing file
// makes gh fail outright instead of printing the URL.
func browserEnv() map[string]string { return map[string]string{"BROWSER": ManagedShimPath} }

// smolShellArgv is Shell's guest command. The outer `bash -c` drops a
// BROWSER that names no executable (browserEnv), cds home, and execs the
// real `bash -l`.
func smolShellArgv() []string {
	return []string{"bash", "-c", `[ -x "$BROWSER" ] || unset BROWSER; cd "$HOME" 2>/dev/null; exec bash -l`}
}

// smolAttachArgv is Attach's guest command, run once the dev session exists
// (ensureTmux). Env on this exec reaches only the tmux client, never a
// pane: the server is older (the keeper's) and `new-session -e` sets the
// environment only of a session it creates. So it first copies BROWSER into
// the dev session's environment, which every new window and pane inherits
// (a shell already running in an old pane keeps its own; documented), and
// only then attaches. One exec for both, so the order holds by
// construction and the attach costs no extra smolvm exec. Skipped when the
// shim is missing, like smolShellArgv.
func smolAttachArgv() []string {
	return []string{"sh", "-c",
		`[ -x "$BROWSER" ] && tmux -S "$1" set-environment -t dev BROWSER "$BROWSER"; exec tmux -S "$1" attach-session -t dev`,
		"_", tmuxSocket}
}

// Shell opens a raw interactive login shell (no tmux). transport is ignored:
// smol is reached via smolvm exec, never ssh/mosh.
//
// The `cd "$HOME"` matters: smolvm exec starts in / and a bash login shell does
// not chdir home on its own (sudo -H only sets $HOME), so without it the prompt
// opens in /. The outer `bash -c` just cds and execs the real `bash -l`, so
// login semantics are unchanged — only the starting directory is fixed.
// BROWSER rides the exec's Env (browserEnv); the login shell's profile.d
// leaves it alone (its guard skips a BROWSER already set).
//
// An unmanaged VM gets smolvm's own default user's login shell (bash if it
// has one, else sh) and nothing of devvm's: no BROWSER, no shim, no tmux.
func (b *smolBackend) Shell(string) error {
	if err := needSmolvm(); err != nil {
		return err
	}
	if !b.m.Managed() {
		return b.Run(context.Background(), ExecOpts{TTY: true}, unmanagedShellArgv()...)
	}
	return b.Run(context.Background(), ExecOpts{TTY: true, Env: browserEnv()}, smolShellArgv()...)
}

// unmanagedShellArgv is Shell's guest command on a VM devvm did not create:
// home, then a login shell, assuming nothing but sh.
func unmanagedShellArgv() []string {
	return []string{"sh", "-c", `cd "$HOME" 2>/dev/null; command -v bash >/dev/null 2>&1 && exec bash -l; exec sh -l`}
}

// Attach joins the dev tmux session, starting a persistent keeper first if
// needed. smolvm tears down an interactive exec context (and its daemonized
// children) after tmux detaches, so the tmux server must live in its own
// detached exec context — otherwise successive `devvm attach` calls can't
// re-attach. transport is ignored (see Shell). Ports smol_ensure_tmux / smol_shell.
// The attach exec also puts BROWSER into the session environment first
// (smolAttachArgv).
func (b *smolBackend) Attach(string) error {
	if err := needSmolvm(); err != nil {
		return err
	}
	if !b.m.Managed() {
		// The cli refuses first (resolve); this keeps the backend from
		// starting a keeper as a `dev` user an unmanaged VM need not have.
		return ErrUnmanaged
	}
	if err := RequireGuestTmux(b, b.m); err != nil {
		return err
	}
	if err := b.ensureTmux(); err != nil {
		return err
	}
	return b.Run(context.Background(), ExecOpts{TTY: true, Login: true, Env: browserEnv()}, smolAttachArgv()...)
}

func (b *smolBackend) hasSession() bool {
	// Login: true so tmux is found via the guest's login PATH. On brew-based
	// boxes tmux lives in /home/linuxbrew/.linuxbrew/bin, which is absent from
	// the bare non-login PATH — without a login shell the probe fails with
	// rc=127 and every session looks dead, so ensureTmux never converges.
	return b.Run(context.Background(), ExecOpts{Login: true, Stdout: io.Discard, Stderr: io.Discard},
		"tmux", "-S", tmuxSocket, "has-session", "-t", "dev") == nil
}

func (b *smolBackend) ensureTmux() error {
	if b.hasSession() {
		return nil
	}
	fmt.Fprintln(os.Stderr, "devvm: starting persistent tmux session 'dev'...")
	keeper := `
		socket="$1"
		install -d -m 700 "$(dirname "$socket")"
		rm -f "$socket"
		cd "$HOME" 2>/dev/null || true
		tmux -S "$socket" new-session -d -s dev
		exec tmux -S "$socket" wait-for devvm-keeper-stop`
	// Detached exec (-d) in its own context, running as the dev user.
	//
	// The leading `hostname` re-assert mirrors guestArgv (smol.go): this exec
	// bypasses guestArgv, and smolvm resets the runtime hostname to "container"
	// every boot. The tmux *server* born here is the parent of every shell you
	// attach to, and bash caches \h at startup — so the hostname must be correct
	// before `tmux new-session`, not just on the later attach exec. Runs as root
	// (exec enters as root) before the sudo drop. The keeper's `cd "$HOME"` sets
	// the session start dir, which tmux uses as the default-path for new panes.
	args := []string{
		"machine", "exec", "-d", "--name", b.m.Name, "-e", "TERM=xterm-256color", "--",
		"sh", "-c", `hostname "$1" 2>/dev/null; shift; exec "$@"`, "_", b.m.Name,
		"sudo", "-u", DefaultUser, "-H", "env", "SMOLVM_GUEST=1",
		"bash", "-lc", keeper, "_", tmuxSocket,
	}
	if err := exec.Command("smolvm", args...).Run(); err != nil {
		return err
	}
	for i := 0; i < 10; i++ {
		if b.hasSession() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("persistent tmux session failed to start in '%s'", b.m.Name)
}
