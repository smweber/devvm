package cli

import (
	"strings"
	"testing"
)

// Commented keys must pass through labelKeys untouched — no TTY, options and
// comment preserved — so scripted `keys add 'ssh-ed25519 AAA... laptop'` works.
func TestLabelKeysCommentedPassThrough(t *testing.T) {
	lines := []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKcRCKRZai1DTb1XFzQXpaGyWXbcu1I6RsIsGqTNpO3Q laptop",
		`command="echo hi",no-agent-forwarding ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGWtNBZmSoOJ2FCbeZBUlA6DHDP7C5EJyLK6BhrBhQqQ locked-down`,
	}
	got, err := labelKeys(lines)
	if err != nil {
		t.Fatalf("labelKeys: %v", err)
	}
	for i := range lines {
		if got[i] != lines[i] {
			t.Errorf("line %d rewritten:\n got %s\nwant %s", i, got[i], lines[i])
		}
	}
}

func TestLabelKeysRejectsJunk(t *testing.T) {
	if _, err := labelKeys([]string{"Not Found"}); err == nil {
		t.Error("junk line should be rejected")
	}
}

func TestGithubKeysRejectsBadUser(t *testing.T) {
	// Validation must fire before any network I/O: these would change the
	// fetched github.com path.
	for _, u := range []string{"", "a/../b", "user/repo", "-leading", "x y"} {
		if _, err := githubKeys(u); err == nil || !strings.Contains(err.Error(), "invalid GitHub user") {
			t.Errorf("githubKeys(%q) should fail validation, got %v", u, err)
		}
	}
}

func TestParseMapping(t *testing.T) {
	tests := []struct {
		in          string
		pref, guest int
		ok          bool
	}{
		{"8080:3000", 8080, 3000, true},
		{"8080", 8080, 8080, true},
		{"0:80", 0, 0, false},
		{"80:0", 0, 0, false},
		{"65536", 0, 0, false},
		{"-1:80", 0, 0, false},
		{"http:80", 0, 0, false},
	}
	for _, tt := range tests {
		pref, guest, err := parseMapping(tt.in)
		if (err == nil) != tt.ok || pref != tt.pref || guest != tt.guest {
			t.Errorf("parseMapping(%q) = %d,%d,%v want %d,%d,ok=%v", tt.in, pref, guest, err, tt.pref, tt.guest, tt.ok)
		}
	}
}
