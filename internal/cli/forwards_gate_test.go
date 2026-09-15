package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
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
		{name: "remote backends skip the gate", b: &statusBackend{runningAfter: -1}, m: &config.Machine{Name: "r", Backend: config.BackendRemoteUnmanaged}, minCall: 0},
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

// `ports add` on a stopped smol VM is a conf edit: it must be recorded and
// must not spawn a daemon (whose first dial would exec into the stopped box).
func TestAddPortRecordsWithoutRunningVM(t *testing.T) {
	shrinkRunningPoll(t)
	var out bytes.Buffer
	a := &App{ConfigDir: t.TempDir(), Stdout: &out, Stderr: &out}
	m := &config.Machine{Name: "vm", Backend: config.BackendSmol}
	if err := m.Save(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	if err := a.addPort(m, &statusBackend{runningAfter: -1}, "8080:8080"); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(a.ConfigDir, "vm")
	if err != nil || len(saved.Ports) != 1 || saved.Ports[0] != "8080:8080" {
		t.Fatalf("conf ports = %v, %v; want [8080:8080]", saved.Ports, err)
	}
	if !strings.Contains(out.String(), "recorded 8080:8080; forwards come up on 'devvm start vm'") {
		t.Fatalf("output = %q", out.String())
	}
	// A conf edit on a stopped VM must not stall on the start-time poll.
	if b := (&statusBackend{runningAfter: -1}); true {
		_ = a.addPort(m, b, "8081:8081")
		if b.calls > 2 {
			t.Fatalf("Status polled %d times on `ports add`; want at most 2", b.calls)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(a.ConfigDir, "run")); len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if strings.Contains(strings.Join(names, ","), ".sock") {
			t.Fatalf("a daemon socket appeared for a stopped VM: %v", names)
		}
	}
}
