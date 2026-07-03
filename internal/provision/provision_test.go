package provision

import (
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

func TestParseSpec(t *testing.T) {
	tests := []struct {
		in     string
		kind   string
		target string
		nargs  int
		err    bool
	}{
		{"none", KindNone, "", 0, false},
		{"url:https://x/b.sh --profile agent-vm --yes", KindURL, "https://x/b.sh", 3, false},
		{"cmd:/opt/setup.sh --fast", KindCmd, "/opt/setup.sh", 1, false},
		{"url:", "", "", 0, true},
		{"bogus:x", "", "", 0, true},
		{"noscheme", "", "", 0, true},
	}
	for _, tt := range tests {
		s, err := ParseSpec(tt.in)
		if (err != nil) != tt.err {
			t.Errorf("%q: err=%v wantErr=%v", tt.in, err, tt.err)
			continue
		}
		if tt.err {
			continue
		}
		if s.Kind != tt.kind || s.Target != tt.target || len(s.Args) != tt.nargs {
			t.Errorf("%q: got %+v", tt.in, s)
		}
	}
}

func TestParseSpecDefault(t *testing.T) {
	// Empty spec falls back to the default bootstrap.sh url provisioner.
	s, err := ParseSpec("")
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != KindURL || !strings.Contains(s.Target, "bootstrap.sh") {
		t.Errorf("default spec = %+v", s)
	}
	if !strings.Contains(config.DefaultProvision, "agent-vm") {
		t.Errorf("default provision lost agent-vm profile: %q", config.DefaultProvision)
	}
}
