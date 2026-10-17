package reach

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
)

func testPaths(t *testing.T) config.Paths {
	d := t.TempDir()
	return config.Paths{Config: filepath.Join(d, "c"), State: filepath.Join(d, "s")}
}

func TestReadMissingAndGarbled(t *testing.T) {
	p := testPaths(t)
	if e := Read(p, "box"); e.State != Unknown || !e.Verified.IsZero() || e.Plain() != "verified=-" {
		t.Errorf("missing file = %+v", e)
	}
	if err := os.MkdirAll(Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"\x00\x01garbage", "state=sideways\nverified=soon\n", "state=reachable\nverified=-5\n"} {
		if err := os.WriteFile(Path(p, "box"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		e := Read(p, "box")
		if !e.Verified.IsZero() || (e.State != Unknown && e.State != Reachable) {
			t.Errorf("garbled %q = %+v", text, e)
		}
		if e.Plain() != "verified=-" {
			t.Errorf("garbled %q plain = %s", text, e.Plain())
		}
	}
}

func TestWriteRoundTripAndSince(t *testing.T) {
	p := testPaths(t)
	t0 := time.Unix(1760700000, 0)
	if err := Write(p, "box", Entry{State: Unreachable, Verified: t0, How: HowProbe, Reason: "connect\ntimed out"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(Path(p, "box"))
	want := "state=unreachable\nverified=1760700000\nsince=1760700000\nhow=probe\nreason=connect timed out\n"
	if string(data) != want {
		t.Errorf("file =\n%s\nwant\n%s", data, want)
	}
	// The same state again keeps its since; a new state starts its own.
	t1 := t0.Add(time.Hour)
	if err := Write(p, "box", Entry{State: Unreachable, Verified: t1, How: HowProbe, Reason: "connection refused"}); err != nil {
		t.Fatal(err)
	}
	if e := Read(p, "box"); !e.Since.Equal(t0) || !e.Verified.Equal(t1) || e.Reason != "connection refused" {
		t.Errorf("repeat failure = %+v", e)
	}
	t2 := t1.Add(time.Minute)
	if err := Write(p, "box", Entry{State: Reachable, Verified: t2, How: HowMaster}); err != nil {
		t.Fatal(err)
	}
	e := Read(p, "box")
	if !e.Since.Equal(t2) || e.Reason != "" || e.Plain() != "verified=1760703660:master" {
		t.Errorf("recovered = %+v (%s)", e, e.Plain())
	}
	// No temp file is left beside it.
	entries, _ := os.ReadDir(Dir(p))
	if len(entries) != 1 {
		t.Errorf("reach dir holds %d entries", len(entries))
	}
	if err := Remove(p, "box"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(p, "box"); err != nil {
		t.Errorf("removing a missing entry: %v", err)
	}
	if Read(p, "box").State != Unknown {
		t.Error("entry survived Remove")
	}
}

func TestRecord(t *testing.T) {
	p := testPaths(t)
	Record(p, "box", "attach")
	e := Read(p, "box")
	if e.State != Reachable || e.How != "attach" || time.Since(e.Verified) > time.Minute {
		t.Errorf("recorded = %+v", e)
	}
}

func TestDescribe(t *testing.T) {
	now := time.Date(2026, 10, 17, 14, 30, 0, 0, time.Local)
	for _, tc := range []struct {
		e    Entry
		want string
	}{
		{Entry{State: Reachable, Verified: now.Add(-12 * time.Second), How: HowMaster}, "reachable, verified 12s ago (master)"},
		{Entry{State: Reachable, Verified: now.Add(-3 * time.Hour), How: "attach"}, "reachable, verified 3h ago (attach)"},
		{Entry{State: Unreachable, Verified: now, Since: time.Date(2026, 10, 17, 14, 2, 0, 0, time.Local), Reason: "connect timed out"}, "unreachable since 14:02 (connect timed out)"},
		{Entry{State: Unreachable, Verified: now, Since: time.Date(2026, 10, 15, 9, 5, 0, 0, time.Local)}, "unreachable since Oct 15 09:05"},
		{Entry{State: Unknown, Reason: "ProxyJump"}, "not probed (ProxyJump)"},
		{Entry{State: Unknown}, "not probed yet"},
	} {
		if got := tc.e.Describe(now); got != tc.want {
			t.Errorf("Describe(%+v) = %q, want %q", tc.e, got, tc.want)
		}
	}
	if !strings.HasPrefix((Entry{State: Reachable, Verified: now, How: "probe"}).Plain(), "verified=") {
		t.Error("plain lacks its key")
	}
}
