package session

import (
	"slices"
	"testing"
)

// The daemon's spawn environment never carries the provider token
// (providers.md §8), and keeps everything else as it was.
func TestDaemonEnvStripsToken(t *testing.T) {
	in := []string{"PATH=/bin", "HCLOUD_TOKEN=secret", "HOME=/h", "HCLOUD_TOKENX=keep"}
	got := daemonEnv(in)
	if want := []string{"PATH=/bin", "HOME=/h", "HCLOUD_TOKENX=keep"}; !slices.Equal(got, want) {
		t.Fatalf("daemonEnv = %v, want %v", got, want)
	}
}
