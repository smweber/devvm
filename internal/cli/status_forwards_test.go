package cli

import (
	"github.com/smweber/devvm/internal/backend"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// The plain forward column is the menu bar app's only signal for "forwards are
// down or stuck reconnecting", so its four shapes are pinned here.
func TestPlainForwards(t *testing.T) {
	// saved is "this host has a ports file for it" (config.LoadPorts).
	noPorts := &config.Machine{Name: "m"}
	for _, tc := range []struct {
		name string
		row  statusRow
		want string
	}{
		{"daemon up", statusRow{m: noPorts, saved: true, fwds: fwdSummary{daemon: true, state: session.StateUp, n: 2, conf: 2}}, "up:2"},
		{"daemon reconnecting", statusRow{m: noPorts, saved: true, fwds: fwdSummary{daemon: true, state: session.StateReconnecting, n: 1, conf: 1}}, "reconnecting:1"},
		// N counts conf owners only (browser-bridge.md §8).
		{"conf plus session forwards", statusRow{m: noPorts, saved: true, fwds: fwdSummary{daemon: true, state: session.StateUp, n: 3, conf: 1}}, "up:1"},
		// up:0 / reconnecting:0 are never emitted: a daemon with no conf
		// forward reads like no daemon at all.
		{"daemon idle with nothing configured", statusRow{m: noPorts, fwds: fwdSummary{daemon: true, state: session.StateUp}}, "-"},
		{"session-only forwards, nothing configured", statusRow{m: noPorts, fwds: fwdSummary{daemon: true, state: session.StateUp, n: 2}}, "-"},
		{"session-only forwards, ports configured", statusRow{m: noPorts, saved: true, fwds: fwdSummary{daemon: true, state: session.StateUp, n: 2}}, "down"},
		{"reconnecting with no conf forward", statusRow{m: noPorts, saved: true, fwds: fwdSummary{daemon: true, state: session.StateReconnecting, n: 1}}, "down"},
		{"ports configured, no daemon", statusRow{m: noPorts, saved: true}, "down"},
		{"nothing configured", statusRow{m: noPorts}, "-"},
		{"unregistered smol (no conf)", statusRow{}, "-"},
	} {
		if got := plainForwards(tc.row); got != tc.want {
			t.Errorf("%s: plainForwards = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestFwdsCount(t *testing.T) {
	if got := fwdsCount(fwdSummary{}); got != "—" {
		t.Errorf("no forwards = %q", got)
	}
	if got := fwdsCount(fwdSummary{daemon: true, state: session.StateUp, n: 3}); got != "3" {
		t.Errorf("up = %q", got)
	}
	got := fwdsCount(fwdSummary{daemon: true, state: session.StateReconnecting, n: 3, since: time.Now().Add(-3 * time.Minute)})
	if !strings.HasPrefix(got, "3 (reconnecting 3m") {
		t.Errorf("reconnecting = %q", got)
	}
}

func TestSinceHuman(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		ago  time.Duration
		want string
	}{
		{12 * time.Second, "12s"},
		{3*time.Minute + 20*time.Second, "3m"},
		{2*time.Hour + 5*time.Minute, "2h5m"},
	} {
		if got := sinceHuman(now.Add(-tc.ago)); got != tc.want {
			t.Errorf("sinceHuman(%s ago) = %q, want %q", tc.ago, got, tc.want)
		}
	}
}

// Smol rows come from one `smolvm machine ls` per snapshot, not a probe per
// machine (a --watch re-emit would otherwise spawn N+1 subprocesses).
func TestRowForSmolUsesSnapshot(t *testing.T) {
	a := &App{Paths: testPaths(t.TempDir())}
	m := &config.Machine{Name: "vm", Backend: config.BackendSmol}
	if err := config.SavePorts(a.Paths, "vm", []string{"80:80"}); err != nil {
		t.Fatal(err)
	}
	if got := a.rowFor(m, smolMachines{available: false}).state; got != "?" {
		t.Errorf("smolvm missing: state = %q, want ?", got)
	}
	snap := smolMachines{available: true, byName: map[string]backend.SmolMachine{
		"vm":    {Name: "vm", State: "running", MemoryMiB: 2048, StorageGB: 20, CPUs: 4, CreatedAt: "100"},
		"other": {Name: "other", State: "stopped"},
	}}
	if r := a.rowFor(m, snap); r.state != "running" || !r.running || !r.exists || plainForwards(r) != "down" {
		t.Errorf("running vm: row = %+v, forwards %q", r, plainForwards(r))
	}
	// Sizes are smolvm's, for a stopped VM too (config.md §3).
	// Stopped, with no daemon to ask: still `down`, since the saved list
	// is on this host.
	snap.byName["vm"] = backend.SmolMachine{Name: "vm", State: "stopped", MemoryMiB: 2048, StorageGB: 20, CPUs: 4, CreatedAt: "100"}
	if r := a.rowFor(m, snap); r.state != "stopped" || plainForwards(r) != "down" || r.mem != 2048 || r.disk != 20 || r.cpus != 4 {
		t.Errorf("stopped vm: row = %+v, forwards %q, want stopped/down with sizes", r, plainForwards(r))
	}
	// No dormant state: a conf whose VM is gone is a stale conf, `?`.
	delete(snap.byName, "vm")
	if r := a.rowFor(m, snap); r.state != "?" || plainManaged(r) != "-" || r.note == "" {
		t.Errorf("unknown vm: row = %+v, want ? / - with a note", r)
	}
}
