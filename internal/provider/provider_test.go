package provider

import (
	"path/filepath"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

func TestManaged(t *testing.T) {
	for _, c := range []struct {
		labels map[string]string
		want   bool
	}{
		{nil, false},
		{map[string]string{"devvm": "true"}, true},
		{map[string]string{"devvm": "false"}, false},
		{map[string]string{"devvm": "TRUE"}, false},
		{map[string]string{"devvm-template": "x"}, false},
	} {
		if got := Managed(Server{Labels: c.labels}); got != c.want {
			t.Errorf("Managed(%v) = %v", c.labels, got)
		}
	}
}

func TestTokenPath(t *testing.T) {
	p := config.Paths{Config: "/c", State: "/s"}
	if got, want := TokenPath(p, "hetzner"), filepath.Join("/s", "providers", "hetzner.token"); got != want {
		t.Fatalf("TokenPath = %q, want %q", got, want)
	}
}
