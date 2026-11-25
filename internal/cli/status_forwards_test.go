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

// The FORWARDS cell lists guest ports, a bumped host port after an arrow,
// at most four; ↻ while reconnecting; a count of saved ones with no daemon.
func TestForwardsCell(t *testing.T) {
	a := newTestApp(t)
	fw := func(g, h int) session.Forward { return session.Forward{Guest: g, Host: h} }
	for _, tc := range []struct {
		name string
		f    fwdSummary
		want string
	}{
		{"none", fwdSummary{}, ""},
		{"sorted, bumped", fwdSummary{daemon: true, state: session.StateUp, list: []session.Forward{fw(5173, 5173), fw(3000, 3001)}}, "3000→3001 5173"},
		{"capped", fwdSummary{daemon: true, state: session.StateUp, list: []session.Forward{fw(1, 1), fw(2, 2), fw(3, 3), fw(4, 4), fw(5, 5), fw(6, 6)}}, "1 2 3 4 +2"},
		{"reconnecting", fwdSummary{daemon: true, state: session.StateReconnecting, list: []session.Forward{fw(8080, 8080)}}, "↻ 8080"},
	} {
		if got := a.forwardsCell(statusRow{name: "m", fwds: tc.f}).text; got != tc.want {
			t.Errorf("%s: forwardsCell = %q, want %q", tc.name, got, tc.want)
		}
	}
	if err := config.SavePorts(a.Paths, "m", []string{"3000", "8080"}); err != nil {
		t.Fatal(err)
	}
	if got := a.forwardsCell(statusRow{name: "m"}).text; got != "2 saved" {
		t.Errorf("saved = %q", got)
	}
}

// Every section shares one set of column widths, so a long name or host in
// one section lines its columns up with every other's.
func TestStatusTableAligned(t *testing.T) {
	a := newTestApp(t)
	var out strings.Builder
	a.Stdout = &out
	hub := &config.Machine{Name: "tp", Backend: config.BackendHub}
	a.renderStatusTable([]statusSection{
		{title: "local (smolvm)", rows: []statusRow{{name: "dev", backend: config.BackendSmol, state: "running", cpus: 4, mem: 8192, disk: 40}}},
		{title: "remote (ssh)", rows: []statusRow{{name: "a-rather-long-name", backend: config.BackendRemote, state: "?", host: "2.29.47.196"}}},
		{title: "hubs", rows: []statusRow{
			{name: "tp", backend: config.BackendHub, hub: "tp", state: "reachable", host: "devvmtest@192.168.1.196", m: hub},
			{name: "tp/raw", backend: config.BackendSmol, hub: "tp", state: "stopped", host: "devvmtest@192.168.1.196"},
			{name: "tp/web", backend: config.BackendSmol, hub: "tp", state: "unreachable", stale: true},
		}},
	}, false)
	lines := strings.Split(out.String(), "\n")
	want := []string{
		"local (smolvm)",
		"  NAME                STATE        DETAIL                     FORWARDS  ADDRESS",
		"  dev                 ● running    4 cpu · 8G ram · 40G disk",
		"",
		"remote (ssh)",
		"  a-rather-long-name  ? unknown                                         2.29.47.196",
		"",
		"hubs",
		"  tp                  ● reachable  2 machines                           devvmtest@192.168.1.196",
		"  ├ tp/raw            ○ stopped    vm",
		"  └ tp/web            · unknown    vm",
		"",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("table =\n%s\nwant\n%s", out.String(), strings.Join(want, "\n"))
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
// VM with saved forwards and no daemon, since the list is on this host.
func TestSmolRowsUseSnapshot(t *testing.T) {
	a := newTestApp(t)
	log := fakeLocalSmolvm(t, `[{"name":"vm","state":"stopped","memory_mib":2048,"storage_gb":20,"cpus":4,"labels":{"devvm":"true"}},{"name":"other","state":"running","created_at":7}]`)
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
	// A VM devvm did not create (no label) has no row, whatever this host
	// still has saved for it.
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	vm := rows[0]
	if vm.state != "stopped" || plainForwards(vm) != "down" || vm.mem != 2048 || vm.disk != 20 || vm.cpus != 4 || plainManaged(vm) != "managed" {
		t.Errorf("vm = %+v, forwards %q", vm, plainForwards(vm))
	}
	if data, _ := os.ReadFile(log); strings.Count(string(data), "machine ls --json") != 1 {
		t.Errorf("smolvm calls =\n%s\nwant one listing", data)
	}
}
