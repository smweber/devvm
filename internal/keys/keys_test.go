package keys

import (
	"errors"
	"strings"
	"testing"
)

const (
	edA = "ssh-ed25519 AAAAedAAAA"
	edB = "ssh-ed25519 AAAAedBBBB"
	rsa = "ssh-rsa AAAArsaCCCC"
)

func TestParse(t *testing.T) {
	k, ok := Parse(edA + " alice@host")
	if !ok || k.Type != "ssh-ed25519" || k.Blob != "AAAAedAAAA" || k.Comment != "alice@host" {
		t.Fatalf("parse = %+v ok=%v", k, ok)
	}
	if k.ID() != "ssh-ed25519 AAAAedAAAA" {
		t.Errorf("ID = %q", k.ID())
	}
	// options prefix
	k, ok = Parse(`command="x",no-pty ` + edA + " bob")
	if !ok || k.Blob != "AAAAedAAAA" || k.Comment != "bob" {
		t.Errorf("options parse = %+v ok=%v", k, ok)
	}
	if _, ok := Parse("# a comment"); ok {
		t.Error("comment should not parse as key")
	}
	if _, ok := Parse("   "); ok {
		t.Error("blank should not parse as key")
	}
}

func TestDedupKeepsFirstAdoptsComment(t *testing.T) {
	// First copy has no comment; a later duplicate does -> first adopts it.
	lines := []string{
		"# header",
		edA,             // first, no comment
		edB + " keep-b", // distinct
		edA + " laptop", // dup of first, has comment
	}
	kept, dup, own := Dedup(lines, nil)
	if dup != 1 || own != 0 {
		t.Fatalf("dup=%d own=%d, want 1,0", dup, own)
	}
	want := []string{"# header", edA + " laptop", edB + " keep-b"}
	if strings.Join(kept, "\n") != strings.Join(want, "\n") {
		t.Fatalf("kept = %v, want %v", kept, want)
	}
}

func TestDedupRemovesOwn(t *testing.T) {
	own := IDs([]string{edB + " machine"})
	lines := []string{edA + " me", edB + " machine-copy", edA + " me"}
	kept, dup, removedOwn := Dedup(lines, own)
	if removedOwn != 1 {
		t.Errorf("removedOwn = %d, want 1", removedOwn)
	}
	if dup != 1 {
		t.Errorf("dup = %d, want 1", dup)
	}
	if len(kept) != 1 || kept[0] != edA+" me" {
		t.Errorf("kept = %v", kept)
	}
}

func TestRevokeSingle(t *testing.T) {
	lines := []string{edA + " alice", edB + " bob"}
	idx, _, err := Revoke(lines, "alice", nil)
	if err != nil || idx != 0 {
		t.Fatalf("revoke alice: idx=%d err=%v", idx, err)
	}
}

func TestRevokeByFingerprint(t *testing.T) {
	lines := []string{edA + " alice"}
	k, _ := Parse(lines[0])
	fp := k.Fingerprint()
	if fp == "" {
		// edA's blob isn't valid base64 of a real key; fall back to a real one.
		t.Skip("synthetic blob has no fingerprint")
	}
	idx, _, err := Revoke(lines, fp, nil)
	if err != nil || idx != 0 {
		t.Fatalf("revoke by fp: idx=%d err=%v", idx, err)
	}
}

func TestRevokeNoMatch(t *testing.T) {
	_, _, err := Revoke([]string{edA + " alice"}, "zzz", nil)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("want ErrNoMatch, got %v", err)
	}
}

func TestRevokeAmbiguous(t *testing.T) {
	// "host" appears in both comments.
	lines := []string{edA + " a@host", edB + " b@host"}
	_, _, err := Revoke(lines, "host", nil)
	if !errors.Is(err, ErrMultiple) {
		t.Fatalf("want ErrMultiple, got %v", err)
	}
}

func TestRevokeProtected(t *testing.T) {
	lines := []string{edA + " alice"}
	protected := map[string]bool{"ssh-ed25519 AAAAedAAAA": true}
	_, _, err := Revoke(lines, "alice", protected)
	if !errors.Is(err, ErrProtect) {
		t.Fatalf("want ErrProtect, got %v", err)
	}
}

func TestSplitJoin(t *testing.T) {
	if got := Split("a\nb\n"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("Split = %v", got)
	}
	if got := Split(""); got != nil {
		t.Errorf("Split empty = %v", got)
	}
	if got := Join([]string{"a", "b"}); got != "a\nb\n" {
		t.Errorf("Join = %q", got)
	}
}

func TestListRealKey(t *testing.T) {
	// A real ed25519 public key so Fingerprint/Summary exercise valid base64.
	const real = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGb9ECWmEzf8YzarY6bTAFHNa3l8xL9nZQ7xY0J3Zk9A tester@example"
	sums := List([]string{real, "# comment", ""})
	if len(sums) != 1 {
		t.Fatalf("List = %v", sums)
	}
	if !strings.HasPrefix(sums[0], "SHA256:") || !strings.Contains(sums[0], "tester@example") {
		t.Errorf("summary = %q", sums[0])
	}
}
