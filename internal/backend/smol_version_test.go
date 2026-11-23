package backend

import "testing"

// The floor is deliberate: v0.2.0 relies on smolvm 1.35.5's behaviour
// (simplify.md, "ThinkPad answers"); raise it with a reason.
func TestSmolvmMinVersionPinned(t *testing.T) {
	if SmolvmMinVersion != "1.35.5" {
		t.Fatalf("SmolvmMinVersion = %q; it must be the smolvm v0.2.0 was verified on (see smol.go)", SmolvmMinVersion)
	}
}

// `smolvm --version`'s first X.Y.Z, compared numerically with the floor;
// output with none is no block.
func TestSmolvmBelowFloor(t *testing.T) {
	for _, tc := range []struct {
		out, have string
		below, ok bool
	}{
		{"smolvm 1.16.1\n", "1.16.1", true, true},
		{"smolvm 1.35.4\n", "1.35.4", true, true},
		{"smolvm 0.99.0 (abc)\n", "0.99.0", true, true},
		{"smolvm 1.35.5\n", "1.35.5", false, true},
		{"smolvm 1.35.10\n", "1.35.10", false, true},
		{"smolvm v1.36.0\n", "1.36.0", false, true},
		{"smolvm 1.36.0-rc1\n", "1.36.0", false, true},
		{"smolvm 2.0.0\n", "2.0.0", false, true},
		{"smolvm version unknown\n", "", false, false},
		{"", "", false, false},
	} {
		have, below, ok := smolvmBelowFloor(tc.out)
		if have != tc.have || below != tc.below || ok != tc.ok {
			t.Errorf("%q: (%q, %v, %v), want (%q, %v, %v)", tc.out, have, below, ok, tc.have, tc.below, tc.ok)
		}
	}
}
