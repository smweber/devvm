package auth

import (
	"errors"
	"os/exec"
	"syscall"
)

// isSignalExit reports whether a command exited due to a signal (e.g. Ctrl-C),
// which should abort the whole auth flow rather than continue to the next tool.
func isSignalExit(err error) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return true
		}
		return ee.ExitCode() >= 128
	}
	return false
}
