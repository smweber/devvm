package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestTouchChangedWritesMarker(t *testing.T) {
	dir := t.TempDir()
	TouchChanged(dir) // runtime dir does not exist yet: created, never an error
	first, err := os.ReadFile(ChangedPath(dir))
	if err != nil || len(first) == 0 {
		t.Fatalf("marker = %q, %v", first, err)
	}
	info, _ := os.Stat(ChangedPath(dir))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("marker mode = %o, want 0600", info.Mode().Perm())
	}
	// A second touch rewrites the marker (a write event is what the watcher
	// needs); the content is a timestamp, but adjacent calls may share one on
	// a coarse clock, so only check it is a valid stamp not older than the
	// first.
	TouchChanged(dir)
	second, _ := os.ReadFile(ChangedPath(dir))
	t1, err1 := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(first)))
	t2, err2 := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(second)))
	if err1 != nil || err2 != nil {
		t.Fatalf("marker is not an RFC3339Nano stamp: %q %v / %q %v", first, err1, second, err2)
	}
	if t2.Before(t1) {
		t.Fatalf("second touch %v is older than the first %v", t2, t1)
	}
}
