package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// spoolLeft lists what a copy left in dir (its $TMPDIR).
func spoolLeft(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	return left
}

// A signal mid-download cancels the fetch and the copy unwinds through its
// cleanup: the spool is gone, and the error is the interruption, not the
// fetch's own failure.
func TestInterruptedCopyRemovesSpool(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	sigs := make(chan os.Signal, 1)
	started := make(chan struct{})
	fetch := func(ctx context.Context, src, archive string) error {
		if err := os.WriteFile(archive, []byte("partial"), 0o600); err != nil {
			return err
		}
		close(started)
		<-ctx.Done() // a host command run under ctx is killed here
		return ctx.Err()
	}
	go func() { <-started; sigs <- syscall.SIGINT }()
	err := runInterruptible(context.Background(), sigs, func(ctx context.Context) error {
		return copyOutWith(ctx, "web", []string{"f"}, filepath.Join(t.TempDir(), "f"), copyOpts{}, io.Discard, fetch)
	})
	var ie *interrupted
	if !errors.As(err, &ie) || ie.sig != syscall.SIGINT {
		t.Fatalf("err = %v, want interrupted by SIGINT", err)
	}
	if left := spoolLeft(t, tmp); len(left) > 0 {
		t.Errorf("an interrupted copy left %v in $TMPDIR", left)
	}
}

// A copy that finished is a success whatever arrived meanwhile, and one
// that failed with no signal keeps its own error.
func TestInterruptibleOutcomes(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	sigs <- syscall.SIGINT
	if err := runInterruptible(context.Background(), sigs, func(context.Context) error { return nil }); err != nil {
		t.Errorf("finished copy with a late signal: err = %v, want nil", err)
	}
	boom := errors.New("boom")
	if err := runInterruptible(context.Background(), make(chan os.Signal), func(context.Context) error { return boom }); err != boom {
		t.Errorf("failed copy without a signal: err = %v, want %v", err, boom)
	}
	// A terminal ^C that killed the host command before the watcher took
	// it still reads as the interruption.
	sigs = make(chan os.Signal, 1)
	err := runInterruptible(context.Background(), sigs, func(context.Context) error {
		sigs <- syscall.SIGTERM
		return boom
	})
	var ie *interrupted
	if !errors.As(err, &ie) || ie.sig != syscall.SIGTERM {
		t.Errorf("err = %v, want interrupted by SIGTERM", err)
	}
}

// End to end, as the bug was found: a real devvm (this test binary
// re-exec'ed, see TestMain) copying to and from h/web is sent SIGINT
// while the hub side hangs. It dies by SIGINT, killing the ssh under it,
// and leaves nothing in $TMPDIR.
func TestCopySIGINTLeavesNoSpool(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t)
	bin := t.TempDir()
	// The hub side says it started, then hangs as a slow transfer would.
	fakeHubWith(t, a, bin, `: >"$FAKE_STARTED"; exec sleep 10`+"\n")
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"cp-in", "h/web", src, "inbox/"},
		{"cp-out", "h/web", "f", t.TempDir() + "/"},
	} {
		t.Run(argv[0], func(t *testing.T) {
			tmp := t.TempDir()
			started := filepath.Join(t.TempDir(), "started")
			cmd := exec.Command(exe, append([]string{"--config-dir", a.Paths.Config, "--state-dir", a.Paths.State}, argv...)...)
			cmd.Env = append(os.Environ(), testMainEnv+"=1", "TMPDIR="+tmp, "FAKE_STARTED="+started)
			// A file, not a buffer: the hub side's orphaned sleep inherits
			// it, and a pipe would hold Wait until the sleep ends.
			stderrPath := filepath.Join(t.TempDir(), "stderr")
			stderrFile, err := os.Create(stderrPath)
			if err != nil {
				t.Fatal(err)
			}
			defer stderrFile.Close()
			cmd.Stderr = stderrFile
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			waitFile(t, started, waited)
			if left := spoolLeft(t, tmp); len(left) == 0 {
				t.Fatal("no spool in $TMPDIR while the copy runs; the test proves nothing")
			}
			if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
				t.Fatal(err)
			}
			select {
			case <-waited:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("devvm did not exit on SIGINT")
			}
			ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ws.Signaled() || ws.Signal() != syscall.SIGINT {
				out, _ := os.ReadFile(stderrPath)
				t.Errorf("exit = %v, want death by SIGINT\n%s", cmd.ProcessState, out)
			}
			if left := spoolLeft(t, tmp); len(left) > 0 {
				t.Errorf("an interrupted %s left %v in $TMPDIR", argv[0], left)
			}
		})
	}
}

// waitFile waits until path exists, failing if the process exits first.
func waitFile(t *testing.T, path string, exited <-chan error) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-exited:
			t.Fatalf("devvm exited before the hub side started: %v", err)
		case <-deadline:
			t.Fatalf("%s never appeared", path)
		case <-tick.C:
		}
	}
}
