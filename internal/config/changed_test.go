package config

import (
	"os"
	"testing"
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
	TouchChanged(dir)
	second, _ := os.ReadFile(ChangedPath(dir))
	if string(second) == string(first) {
		t.Fatal("second touch should rewrite the marker with a new timestamp")
	}
}
