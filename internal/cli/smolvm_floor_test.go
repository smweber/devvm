package cli

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/backend"
)

// Below the floor, create and every start of a smol VM (start, and
// attach/shell/setup booting a stopped one) refuse in the one line, at
// once (no wait for a boot that never began), before smolvm creates or
// starts anything and before create prompts or prints. At the floor, start
// goes through. status and ports never ask for the version.
func TestSmolvmFloor(t *testing.T) {
	a := newTestApp(t)
	rows := `[{"name":"web","state":"stopped",` + devvmLabel + `},{"name":"up","state":"running",` + devvmLabel + `}]`
	log := fakeLocalSmolvm(t, rows, "1.16.1")
	oldTTY := openTTY
	t.Cleanup(func() { openTTY = oldTTY })
	ttys := 0
	openTTY = func() *os.File { ttys++; return nil }
	want := (&backend.SmolvmTooOld{Version: "1.16.1"}).Error()
	for _, argv := range [][]string{
		{"start", "web"}, {"start", "up"}, {"attach", "web"}, {"shell", "web"}, {"setup", "web"},
		{"create"}, {"create", "vm"}, {"create", "vm", "--memory", "1024", "--yes"},
	} {
		t0 := time.Now()
		err := runTree(t, a, argv...)
		if err == nil || err.Error() != want {
			t.Errorf("%v: err = %v\nwant %s", argv, err, want)
		}
		if d := time.Since(t0); d > 5*time.Second {
			t.Errorf("%v: refused after %s, want at once", argv, d)
		}
		if argv[0] == "create" && (ttys > 0 || a.Stdout.(*bytes.Buffer).Len() > 0) {
			t.Errorf("%v: prompted (%d) or printed %q before the refusal", argv, ttys, a.Stdout)
		}
	}
	if calls := smolvmCalls(t, log); len(calls) > 0 {
		t.Errorf("refused verbs ran smolvm: %q", calls)
	}

	log = fakeLocalSmolvm(t, rows, "1.35.5")
	for _, argv := range [][]string{{"status"}, {"status", "--plain", "--local"}, {"status", "web"}, {"ports", "list", "web"}} {
		if err := runTree(t, a, argv...); err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
	}
	if data, _ := os.ReadFile(log); strings.Contains(string(data), "--version") {
		t.Errorf("status/ports ran smolvm --version:\n%s", data)
	}
	if err := runTree(t, a, "start", "up"); err != nil {
		t.Fatalf("start at the floor: %v", err)
	}
	if calls := smolvmCalls(t, log); !slices.Equal(calls, []string{"machine start --name up"}) {
		t.Errorf("start at the floor ran %q", calls)
	}
}
