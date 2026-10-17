package cli

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// statusBackend answers Status() from a script: running after `runningAfter`
// calls, never if it is negative.
type statusBackend struct {
	backend.Backend
	calls        int
	runningAfter int
	statusErr    error
}

func (b *statusBackend) Status() (backend.State, error) {
	b.calls++
	if b.statusErr != nil {
		return backend.State{}, b.statusErr
	}
	return backend.State{Exists: true, Running: b.runningAfter >= 0 && b.calls > b.runningAfter}, nil
}

func shrinkRunningPoll(t *testing.T) {
	t.Helper()
	oldT, oldI := runningPollTimeout, runningPollInterval
	runningPollTimeout, runningPollInterval = 200*time.Millisecond, time.Millisecond
	t.Cleanup(func() { runningPollTimeout, runningPollInterval = oldT, oldI })
}

// `devvm start` calls tunnelUp right after smolvm returns, when the box may
// still report as starting; the gate must wait for it, not tell the user to
// start a VM they just started.
func TestRequireRunningForForwardsPolls(t *testing.T) {
	shrinkRunningPoll(t)
	smol := &config.Machine{Name: "vm", Backend: config.BackendSmol}
	for _, tc := range []struct {
		name    string
		b       *statusBackend
		m       *config.Machine
		wantErr string
		minCall int
	}{
		{name: "running on third poll", b: &statusBackend{runningAfter: 2}, m: smol, minCall: 3},
		{name: "already running", b: &statusBackend{}, m: smol, minCall: 1},
		{name: "never running", b: &statusBackend{runningAfter: -1}, m: smol, wantErr: "vm is not running; start it first", minCall: 2},
		{name: "status error is not a block", b: &statusBackend{runningAfter: -1, statusErr: errors.New("smolvm missing")}, m: smol, minCall: 1},
		{name: "remote backends skip the gate", b: &statusBackend{runningAfter: -1}, m: config.NewRemote("r", false, "r"), minCall: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireRunningForForwards(tc.m, tc.b, runningPollTimeout)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			if tc.b.calls < tc.minCall {
				t.Fatalf("Status called %d times, want at least %d", tc.b.calls, tc.minCall)
			}
			if tc.minCall == 0 && tc.b.calls != 0 {
				t.Fatalf("Status called %d times for a remote backend", tc.b.calls)
			}
		})
	}
}

// `ports add` on a stopped smol VM saves to the ports file: it must be
// recorded and must not spawn a daemon (whose first dial would exec into the
// stopped box). devvm.toml is never written.
func TestAddPortRecordsWithoutRunningVM(t *testing.T) {
	shrinkRunningPoll(t)
	var out bytes.Buffer
	a := &App{Paths: testPaths(t.TempDir()), Stdout: &out, Stderr: &out}
	m := managedSmol("vm")
	if err := a.addPort(m, &statusBackend{runningAfter: -1}, "8080:8080", false); err != nil {
		t.Fatal(err)
	}
	saved, err := config.LoadPorts(a.Paths, "vm")
	if err != nil || !slices.Equal(saved, []string{"8080:8080"}) {
		t.Fatalf("saved ports = %v, %v; want [8080:8080]", saved, err)
	}
	if _, err := os.Stat(a.Paths.FilePath()); !os.IsNotExist(err) {
		t.Errorf("ports add wrote devvm.toml: %v", err)
	}
	// "8080" is the same forward: nothing new is saved.
	if err := a.addPort(m, &statusBackend{runningAfter: -1}, "8080", false); err != nil {
		t.Fatal(err)
	}
	if saved, _ := config.LoadPorts(a.Paths, "vm"); len(saved) != 1 {
		t.Errorf("an equivalent mapping was saved twice: %v", saved)
	}
	// --ephemeral saves nothing, and with nowhere to wait it refuses a
	// stopped VM rather than claiming a recording.
	if err := a.addPort(m, &statusBackend{runningAfter: -1}, "9000", true); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("ephemeral add on a stopped VM = %v, want the not-running refusal", err)
	}
	if saved, _ := config.LoadPorts(a.Paths, "vm"); len(saved) != 1 {
		t.Errorf("--ephemeral saved: %v", saved)
	}
	if !strings.Contains(out.String(), "recorded 8080:8080; forwards come up on 'devvm start vm'") {
		t.Fatalf("output = %q", out.String())
	}
	// A conf edit on a stopped VM must not stall on the start-time poll.
	if b := (&statusBackend{runningAfter: -1}); true {
		_ = a.addPort(m, b, "8081:8081", false)
		if b.calls > 2 {
			t.Fatalf("Status polled %d times on `ports add`; want at most 2", b.calls)
		}
	}
	if entries, _ := os.ReadDir(a.Paths.RuntimeDir()); len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if strings.Contains(strings.Join(names, ","), ".sock") {
			t.Fatalf("a daemon socket appeared for a stopped VM: %v", names)
		}
	}
}
