package cli

import (
	"os"
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
// machine (a --watch re-emit would otherwise spawn N+1 subprocesses), with
// smolvm's own sizes for a stopped VM too (config.md §3), and `down` for a
// managed VM with saved forwards and no daemon, since the list is on this
// host.
func TestSmolRowsUseSnapshot(t *testing.T) {
	a := newTestApp(t)
	log := fakeLocalSmolvm(t, `[{"name":"vm","state":"stopped","memory_mib":2048,"storage_gb":20,"cpus":4,"created_at":100},{"name":"other","state":"running","created_at":7}]`)
	if err := config.WriteManagedMarker(a.Paths, "vm", "100"); err != nil {
		t.Fatal(err)
	}
	if err := config.SavePorts(a.Paths, "vm", []string{"80:80"}); err != nil {
		t.Fatal(err)
	}
	if err := config.SavePorts(a.Paths, "other", []string{"81:81"}); err != nil {
		t.Fatal(err)
	}
	rows, err := a.gatherRows(statusScope{local: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	vm, other := rows[0], rows[1]
	if vm.state != "stopped" || plainForwards(vm) != "down" || vm.mem != 2048 || vm.disk != 20 || vm.cpus != 4 || plainManaged(vm) != "managed" {
		t.Errorf("vm = %+v, forwards %q", vm, plainForwards(vm))
	}
	// An unmanaged VM's stray ports file reads `-`: nothing may bring it up.
	if other.state != "running" || plainForwards(other) != "-" || plainManaged(other) != "unmanaged" {
		t.Errorf("other = %+v", other)
	}
	if data, _ := os.ReadFile(log); strings.Count(string(data), "machine ls --json") != 1 {
		t.Errorf("smolvm calls =\n%s\nwant one listing", data)
	}
}
