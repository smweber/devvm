package backend

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// HostWaitDelay is the exec.Cmd WaitDelay runHost sets: how long, after a
// host command is gone (exited or killed), its stdio pipes may stay open
// under another holder before they are closed and Wait returns. A killed
// ssh on a ControlMaster is the case that matters: the mux master inherits
// the client's fds, so the listing's stdout stays open for this long after
// SIGKILL. Exported so a caller with an overall budget (the hub listing)
// can subtract it from its context deadline.
const HostWaitDelay = time.Second

// runHost runs a fully-formed host command (the smolvm/ssh invocation) with
// stdio wired per opts and waits. Interactive execs pass the terminal fds
// straight through; the guest side (smolvm -it / ssh -t) owns pty allocation,
// so the host needs no local pty.
func runHost(ctx context.Context, o ExecOpts, hostArgv []string) error {
	o.stdioDefaults()
	cmd := exec.CommandContext(ctx, hostArgv[0], hostArgv[1:]...)
	cmd.Stdin = o.Stdin
	cmd.Stdout = o.Stdout
	cmd.Stderr = o.Stderr
	// When stdio is a Go writer (a buffer, a pipe) Wait also waits for the
	// copying goroutines, which end only when every holder of the write end
	// is gone. A deadline that kills ssh must not then block on a grandchild
	// that inherited the pipe (a login shell's child, a fake ssh's sleep):
	// after the process is gone, WaitDelay closes the pipes and Wait returns.
	// A process that exits normally never has anything left to drain, so this
	// changes nothing for the ordinary case.
	cmd.WaitDelay = HostWaitDelay
	return cmd.Run()
}

// quietHost runs a host command with its output captured, surfacing it only in
// the error. Transports print progress meters and temp paths (smolvm's
// "Uploading …/payload.tar" line, scp's meter) that are noise on success but
// the only diagnostic on failure.
func quietHost(hostArgv []string) error {
	cmd := exec.Command(hostArgv[0], hostArgv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// captureHost runs a host command and returns its trimmed stdout.
func captureHost(ctx context.Context, hostArgv []string) (string, error) {
	cmd := exec.CommandContext(ctx, hostArgv[0], hostArgv[1:]...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// spawnHost starts a host command in its own process group and returns a
// Session over its stdin/stdout pipes. This owns the whole child tree so a
// single group kill takes down smolvm/ssh and everything it spawned — the same
// contract devvm-mux relied on.
func spawnHost(ctx context.Context, hostArgv []string) (*Session, error) {
	cctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cctx, hostArgv[0], hostArgv[1:]...)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	return &Session{cmd: cmd, Stdin: in, Stdout: out, cancel: cancel}, nil
}

func killGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		// Negative PID targets the whole process group (Setpgid above).
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// posixQuote single-quotes a string so any POSIX shell (and fish) treats it as
// one literal argument — the Go equivalent of sq() in bin/devvm.
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// envAssignments turns opts.Env into sorted-free `KEY=VALUE` tokens to place
// after `env`. SMOLVM_GUEST is added by the smol backend, not here.
func envAssignments(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
