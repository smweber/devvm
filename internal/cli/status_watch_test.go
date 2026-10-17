package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
)

// blockWriter hands each blank-line-terminated block to a channel so tests
// can wait on "a snapshot arrived" with a bounded select.
type blockWriter struct {
	mu     sync.Mutex
	buf    strings.Builder
	blocks chan string
}

func newBlockWriter() *blockWriter { return &blockWriter{blocks: make(chan string, 16)} }

func (w *blockWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		s := w.buf.String()
		i := strings.Index(s, "\n\n")
		if i < 0 {
			// An empty snapshot is a lone "\n" block.
			if s == "\n" {
				w.buf.Reset()
				w.blocks <- ""
			}
			return len(p), nil
		}
		w.blocks <- s[:i+1]
		w.buf.Reset()
		w.buf.WriteString(s[i+2:])
	}
}

func (w *blockWriter) next(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-w.blocks:
		if got != want {
			t.Fatalf("snapshot = %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second): // generous: only reached on failure; macOS CI under -race is slow
		t.Fatalf("no snapshot within 10s (want %q)", want)
	}
}

func (w *blockWriter) none(t *testing.T) {
	t.Helper()
	select {
	case got := <-w.blocks:
		t.Fatalf("unexpected snapshot %q", got)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestWatchStatusReactsToChanges(t *testing.T) {
	dir := testPaths(t.TempDir())
	// The snapshot is injected: what matters here is *when* it is re-taken,
	// not what gatherRows would say about a machine with no backend.
	var mu sync.Mutex
	current := "a\tsmol\tstopped\t-\n"
	snapshot := func() string {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	set := func(s string) { mu.Lock(); current = s; mu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := newBlockWriter()
	done := make(chan error, 1)
	go func() { done <- watchStatus(ctx, dir, okSnap(snapshot), nil, out, 50*time.Millisecond, nil, nil) }()

	out.next(t, "a\tsmol\tstopped\t-\n") // initial, unconditional

	// devvm.toml renamed into place (how `config add` and most editors
	// write it) is a change: the config dir is watched, not the file.
	set("a\tsmol\tstopped\t-\nb\tremote\treachable\t-\n")
	if err := config.AppendEntry(dir, "b", config.Entry{Backend: config.BackendRemote, Host: "b"}); err != nil {
		t.Fatal(err)
	}
	out.next(t, "a\tsmol\tstopped\t-\nb\tremote\treachable\t-\n")
	// And again: a second rename over the first is seen too.
	set("a\tsmol\tstopped\t-\n")
	if err := config.RemoveEntry(dir, "b"); err != nil {
		t.Fatal(err)
	}
	out.next(t, "a\tsmol\tstopped\t-\n")

	// The marker is a change even though nothing else on disk moved.
	set("a\tsmol\trunning\tup:2\n")
	config.TouchChanged(dir)
	out.next(t, "a\tsmol\trunning\tup:2\n")

	// Identical snapshot after an event: suppressed.
	config.TouchChanged(dir)
	out.none(t)

	// Lock and log churn is noise, not a change.
	set("changed but should not be seen\n")
	if err := os.WriteFile(filepath.Join(dir.RuntimeDir(), "a.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir.RuntimeDir(), "a.log"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.none(t)

	// A socket-like entry appearing in run/ is a change (daemon came up).
	if err := os.WriteFile(filepath.Join(dir.RuntimeDir(), "a.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out.next(t, "changed but should not be seen\n")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watchStatus returned %v on cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watchStatus did not return after cancel")
	}
}

// A devvm.toml that stops parsing mid-watch emits the broken conf row (one
// line on stderr) and keeps watching: the next event after it parses again
// emits the listing as usual.
func TestWatchStatusBrokenFileRow(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	writeDevvmTOML(t, a, "[machines.b]\nbackend = \"remote\"\nhost = \"b\"\n")
	var errBuf syncBuffer
	a.Stderr = &errBuf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := newBlockWriter()
	snapshot := func() (string, error) { return a.plainSnapshot(statusScope{local: true}) }
	done := make(chan error, 1)
	go func() { done <- watchStatus(ctx, a.Paths, snapshot, a.Stderr, out, 50*time.Millisecond, nil, nil) }()
	out.next(t, "b\tremote\t?\t-\tunmanaged\tverified=-\n")

	writeDevvmTOML(t, a, "[machines.b\n")
	out.next(t, brokenConfRow)
	if !strings.Contains(errBuf.String(), "listing 'broken conf' until devvm.toml parses again") {
		t.Errorf("stderr = %q", errBuf.String())
	}
	writeDevvmTOML(t, a, "[machines.c]\nbackend = \"hub\"\nhost = \"c\"\n")
	out.next(t, "")
	cancel()
	<-done
}

// Broken at start: the watch opens on the broken conf row and keeps
// going until the file parses; a one-shot status prints the row (plain) or
// one line naming the error (human), the error on stderr, and exits 0.
func TestStatusBrokenFileAtStart(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	writeDevvmTOML(t, a, "[machines.b\n")
	var stdout, stderr bytes.Buffer
	a.Stdout, a.Stderr = &stdout, &stderr
	if err := a.runStatusPlain(true); err != nil {
		t.Fatalf("status --plain on a broken file = %v", err)
	}
	if stdout.String() != brokenConfRow || !strings.Contains(stderr.String(), "devvm: ") {
		t.Errorf("plain: stdout %q stderr %q", stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if err := a.runStatusAll(false, true); err != nil {
		t.Fatalf("status on a broken file = %v", err)
	}
	if l := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n"); len(l) != 1 || !strings.HasPrefix(l[0], "broken conf: ") || stderr.Len() == 0 {
		t.Errorf("human: stdout %q stderr %q", stdout.String(), stderr.String())
	}

	var errBuf syncBuffer
	a.Stderr = &errBuf
	out := newBlockWriter()
	a.Stdout = out
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.runStatusWatch(ctx, true, false) }()
	out.next(t, brokenConfRow)
	writeDevvmTOML(t, a, "[machines.b]\nbackend = \"remote\"\nhost = \"b\"\n")
	out.next(t, "b\tremote\t?\t-\tunmanaged\tverified=-\n")
	cancel()
	if err := <-done; err != nil {
		t.Errorf("watch = %v", err)
	}
}

func TestWatchStatusEmptyRegistryAndConsumerGone(t *testing.T) {
	dir := testPaths(t.TempDir())
	out := newBlockWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- watchStatus(ctx, dir, okSnap(func() string { return "" }), nil, out, 50*time.Millisecond, nil, nil)
	}()
	out.next(t, "") // an empty registry still yields one (blank) block
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// A consumer that has gone away (write error) ends the loop cleanly.
	pr, pw := io.Pipe()
	pr.Close()
	err := watchStatus(context.Background(), dir, okSnap(func() string { return "x\n" }), nil, pw, 50*time.Millisecond, nil, nil)
	if err != nil {
		t.Fatalf("closed consumer should end the watch with nil, got %v", err)
	}
}

func TestWatchRequiresPlain(t *testing.T) {
	a := &App{Paths: testPaths(t.TempDir()), Stdout: io.Discard, Stderr: io.Discard}
	root := a.rootCmd()
	root.SetArgs([]string{"status", "--watch"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--plain") {
		t.Fatalf("expected a --plain requirement error, got %v", err)
	}
}

// Removing a watched directory silently drops its watch on both inotify and
// kqueue; the watcher must re-create and re-add it rather than go blind.
func TestWatchStatusSurvivesDirRemoval(t *testing.T) {
	dir := testPaths(t.TempDir())
	var mu sync.Mutex
	current := "a\tsmol\tstopped\t-\n"
	snapshot := func() string {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	set := func(s string) { mu.Lock(); current = s; mu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := newBlockWriter()
	go func() { _ = watchStatus(ctx, dir, okSnap(snapshot), nil, out, 50*time.Millisecond, nil, nil) }()
	out.next(t, "a\tsmol\tstopped\t-\n")

	set("after removal\n")
	if err := os.RemoveAll(dir.RuntimeDir()); err != nil {
		t.Fatal(err)
	}
	out.next(t, "after removal\n") // the removal itself is a change

	// The recreated directory must be watched again: a marker write there is
	// still seen.
	set("after marker\n")
	deadline := time.Now().Add(2 * time.Second)
	for {
		config.TouchChanged(dir)
		select {
		case got := <-out.blocks:
			if got != "after marker\n" {
				t.Fatalf("snapshot = %q", got)
			}
			return
		case <-time.After(150 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("marker write in the recreated runtime dir was not observed")
			}
		}
	}
}

// devvm.toml as a symlink: an edit of its target (in another dir) is seen,
// a relink to another target moves the watch, and editor swap files in the
// config dir are not changes.
func TestWatchFollowsSymlinkRetarget(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	if err := os.MkdirAll(a.Paths.Config, 0o755); err != nil {
		t.Fatal(err)
	}
	// The targets live under a symlinked dir, as t.TempDir() does on macOS
	// (/var -> /private/var): what EvalSymlinks returns is not the path the
	// link names, and the watch must match events in the resolved form.
	root := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), root); err != nil {
		t.Fatal(err)
	}
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{dirA, dirB} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fileA, fileB := filepath.Join(dirA, "devvm.toml"), filepath.Join(dirB, "devvm.toml")
	entry := func(n string) string { return "[machines." + n + "]\nbackend = \"remote\"\nhost = \"" + n + "\"\n" }
	row := func(n string) string { return n + "\tremote\t?\t-\tunmanaged\tverified=-\n" }
	if err := os.WriteFile(fileA, []byte(entry("a")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte(entry("b")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fileA, a.Paths.FilePath()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := newBlockWriter()
	snapshot := func() (string, error) { return a.plainSnapshot(statusScope{local: true}) }
	done := make(chan error, 1)
	go func() { done <- watchStatus(ctx, a.Paths, snapshot, nil, out, 50*time.Millisecond, nil, nil) }()
	out.next(t, row("a"))

	// An edit of the target, in its own dir, through config add.
	if err := config.AppendEntry(a.Paths, "a2", config.Entry{Backend: config.BackendRemote, Host: "a2"}); err != nil {
		t.Fatal(err)
	}
	out.next(t, row("a")+row("a2"))

	// Swap files in the config dir are not changes.
	if err := os.WriteFile(filepath.Join(a.Paths.Config, ".devvm.toml.swp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.none(t)

	// Relink (as stow would: a new link renamed over the old one).
	tmp := filepath.Join(a.Paths.Config, ".devvm.toml.link")
	if err := os.Symlink(fileB, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, a.Paths.FilePath()); err != nil {
		t.Fatal(err)
	}
	out.next(t, row("b"))
	// Now B's dir is watched: an edit there is seen, one in A's is not.
	if err := os.WriteFile(fileA, []byte(entry("a")+entry("zz")), 0o644); err != nil {
		t.Fatal(err)
	}
	out.none(t)
	if err := config.AppendEntry(a.Paths, "b2", config.Entry{Backend: config.BackendRemote, Host: "b2"}); err != nil {
		t.Fatal(err)
	}
	out.next(t, row("b")+row("b2"))
	cancel()
	<-done
}
