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

// Shell opens a raw interactive login shell (no tmux). transport is ignored:
// smol is reached via smolvm exec, never ssh/mosh.
//
// The `cd "$HOME"` matters: smolvm exec starts in / and a bash login shell does
// not chdir home on its own (sudo -H only sets $HOME), so without it the prompt
// opens in /. The outer `bash -c` just cds and execs the real `bash -l`, so
// login semantics are unchanged — only the starting directory is fixed.
func (b *smolBackend) Shell(string) error {
	if err := needSmolvm(); err != nil {
		return err
	}
	return b.Run(context.Background(), ExecOpts{TTY: true},
		"bash", "-c", `cd "$HOME" 2>/dev/null; exec bash -l`)
}

// Attach joins the dev tmux session, starting a persistent keeper first if
// needed. smolvm tears down an interactive exec context (and its daemonized
// children) after tmux detaches, so the tmux server must live in its own
// detached exec context — otherwise successive `devvm attach` calls can't
// re-attach. transport is ignored (see Shell). Ports smol_ensure_tmux / smol_shell.
func (b *smolBackend) Attach(string) error {
	if err := needSmolvm(); err != nil {
		return err
	}
	if err := RequireGuestTmux(b, b.m); err != nil {
		return err
	}
	if err := b.ensureTmux(); err != nil {
		return err
	}
	return b.Run(context.Background(), ExecOpts{TTY: true, Login: true},
		"tmux", "-S", tmuxSocket, "attach-session", "-t", "dev")
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
