package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// The plain forward column is the menu bar app's only signal for "forwards are
// down or stuck reconnecting", so its four shapes are pinned here.
func TestPlainForwards(t *testing.T) {
	withPorts := &config.Machine{Name: "m", Ports: []string{"8080:8080"}}
	noPorts := &config.Machine{Name: "m"}
	for _, tc := range []struct {
		name string
		row  statusRow
		want string
	}{
		{"daemon up", statusRow{m: withPorts, fwds: fwdSummary{daemon: true, state: session.StateUp, n: 2}}, "up:2"},
		{"daemon reconnecting", statusRow{m: withPorts, fwds: fwdSummary{daemon: true, state: session.StateReconnecting, n: 1}}, "reconnecting:1"},
		{"daemon idle with nothing configured", statusRow{m: noPorts, fwds: fwdSummary{daemon: true, state: session.StateUp}}, "up:0"},
		{"ports configured, no daemon", statusRow{m: withPorts}, "down"},
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
	a := &App{ConfigDir: t.TempDir()}
	m := &config.Machine{Name: "vm", Backend: config.BackendSmol, Ports: []string{"80:80"}}
	if got := a.rowFor(m, smolMachines{available: false}).state; got != "?" {
		t.Errorf("smolvm missing: state = %q, want ?", got)
	}
	snap := smolMachines{available: true, state: map[string]string{"vm": "running", "other": "stopped"}}
	if r := a.rowFor(m, snap); r.state != "running" || !r.running || !r.exists || plainForwards(r) != "down" {
		t.Errorf("running vm: row = %+v, forwards %q", r, plainForwards(r))
	}
	snap.state["vm"] = "stopped"
	if got := a.rowFor(m, snap).state; got != "stopped" {
		t.Errorf("stopped vm: state = %q", got)
	}
	delete(snap.state, "vm")
	if got := a.rowFor(m, snap).state; got != "dormant" {
		t.Errorf("unknown vm: state = %q, want dormant", got)
	}
}
