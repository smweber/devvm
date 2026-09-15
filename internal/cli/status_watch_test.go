package cli

import (
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
	case <-time.After(3 * time.Second):
		t.Fatalf("no snapshot within 3s (want %q)", want)
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
	dir := t.TempDir()
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
	go func() { done <- watchStatus(ctx, dir, snapshot, out, 50*time.Millisecond, nil) }()

	out.next(t, "a\tsmol\tstopped\t-\n") // initial, unconditional

	// A conf landing in machines/ is a change.
	set("a\tsmol\tstopped\t-\nb\tremote-managed\treachable\t-\n")
	if err := os.WriteFile(filepath.Join(config.MachinesDir(dir), "b.toml"), []byte("name = 'b'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.next(t, "a\tsmol\tstopped\t-\nb\tremote-managed\treachable\t-\n")

	// The marker is a change even though nothing else on disk moved.
	set("a\tsmol\trunning\tup:2\nb\tremote-managed\treachable\t-\n")
	config.TouchChanged(dir)
	out.next(t, "a\tsmol\trunning\tup:2\nb\tremote-managed\treachable\t-\n")

	// Identical snapshot after an event: suppressed.
	config.TouchChanged(dir)
	out.none(t)

	// Lock and log churn is noise, not a change.
	set("changed but should not be seen\n")
	if err := os.WriteFile(filepath.Join(config.RuntimeDir(dir), "a.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.RuntimeDir(dir), "a.log"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.none(t)

	// A socket-like entry appearing in run/ is a change (daemon came up).
	if err := os.WriteFile(filepath.Join(config.RuntimeDir(dir), "a.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out.next(t, "changed but should not be seen\n")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watchStatus returned %v on cancel", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watchStatus did not return after cancel")
	}
}

func TestWatchStatusEmptyRegistryAndConsumerGone(t *testing.T) {
	dir := t.TempDir()
	out := newBlockWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- watchStatus(ctx, dir, func() string { return "" }, out, 50*time.Millisecond, nil) }()
	out.next(t, "") // an empty registry still yields one (blank) block
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// A consumer that has gone away (write error) ends the loop cleanly.
	pr, pw := io.Pipe()
	pr.Close()
	err := watchStatus(context.Background(), dir, func() string { return "x\n" }, pw, 50*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("closed consumer should end the watch with nil, got %v", err)
	}
}

func TestWatchRequiresPlain(t *testing.T) {
	a := &App{ConfigDir: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard}
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
	dir := t.TempDir()
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
	go func() { _ = watchStatus(ctx, dir, snapshot, out, 50*time.Millisecond, nil) }()
	out.next(t, "a\tsmol\tstopped\t-\n")

	set("after removal\n")
	if err := os.RemoveAll(config.RuntimeDir(dir)); err != nil {
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
