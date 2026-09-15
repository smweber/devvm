package session

import (
	"path/filepath"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

// Runtime files for a hub machine use HUB@NAME: `run/h/web.sock` would name a
// directory nothing creates, and the first daemon spawn would fail opening
// its log. Local names pass through unchanged.
func TestRuntimePathsUseRuntimeName(t *testing.T) {
	dir := t.TempDir()
	run := config.RuntimeDir(dir)
	for _, tt := range []struct{ name, base string }{
		{"web", "web"},
		{"h/web", "h@web"},
	} {
		if got, want := socketPath(dir, tt.name), filepath.Join(run, tt.base+".sock"); got != want {
			t.Errorf("socketPath(%q) = %q, want %q", tt.name, got, want)
		}
		if got, want := logPath(dir, tt.name), filepath.Join(run, tt.base+".log"); got != want {
			t.Errorf("logPath(%q) = %q, want %q", tt.name, got, want)
		}
		if got, want := lockPath(dir, tt.name), filepath.Join(run, tt.base+".lock"); got != want {
			t.Errorf("lockPath(%q) = %q, want %q", tt.name, got, want)
		}
	}
}
