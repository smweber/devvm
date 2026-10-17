package cli

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// `devvm exec` exits with the guest command's own status, silently, as the
// proxied path does; ssh's own 255 stays "cannot reach", an ordinary error.
// The fake ssh runs the real remote string through sh, so the status is
// what the login shell's command returned.
func TestExecPassesGuestExitStatus(t *testing.T) {
	a := newTestApp(t)
	t.Setenv("HOME", t.TempDir()) // bash -l reads no profile of the real user's
	fakeSSH(t, `sh -c "$last"`)
	saveBox(t, a, "box")
	if err := runTree(t, a, "exec", "box", "--", "sh", "-c", "exit 3"); err != nil {
		var ec *exitCode
		if !errors.As(err, &ec) || ec.code != 3 {
			t.Fatalf("guest exit 3: err = %v, want exitCode 3", err)
		}
	} else {
		t.Fatal("guest exit 3 was not an error")
	}
	if err := runTree(t, a, "exec", "box", "true"); err != nil {
		t.Errorf("guest exit 0: err = %v", err)
	}
	err := runTree(t, a, "exec", "box", "--", "sh", "-c", "exit 255")
	var ec *exitCode
	if errors.As(err, &ec) || err == nil || !strings.Contains(err.Error(), "cannot reach 'box' (u@box): ssh exited 255") {
		t.Errorf("exit 255: err = %v, want cannot reach", err)
	}

	// And the process itself: Execute exits with it (this test binary
	// re-exec'ed as the CLI, see TestMain).
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "--config-dir", a.Paths.Config, "--state-dir", a.Paths.State, "exec", "box", "--", "sh", "-c", "echo oops >&2; exit 3")
	cmd.Env = append(os.Environ(), testMainEnv+"=1")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("devvm exec: %v, want exit status 3\n%s", err, out)
	}
	if string(out) != "oops\n" {
		t.Errorf("output = %q, want only the guest's stderr", out)
	}
}
