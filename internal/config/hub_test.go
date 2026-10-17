package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConf writes an old per-machine conf (machines/NAME.toml), which only
// the one-time saved-forwards seed still reads.
func writeConf(t *testing.T, dir, name, raw string) {
	t.Helper()
	md := LegacyMachinesDir(Paths{Config: dir})
	if err := os.MkdirAll(md, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(md, name+".toml"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHubNames(t *testing.T) {
	for _, tt := range []struct {
		in           string
		hub, machine string
		ok, bad      bool
	}{
		{in: "web"},
		{in: "h/web", hub: "h", machine: "web", ok: true},
		{in: "desktop.lan/api-2", hub: "desktop.lan", machine: "api-2", ok: true},
		{in: "h/", ok: true, bad: true},
		{in: "/web", ok: true, bad: true},
		{in: "h/a/b", ok: true, bad: true}, // the machine part may not contain '/'
		{in: "h@web", ok: false},           // the runtime form is never a display name
	} {
		hub, machine, ok, err := SplitHubName(tt.in)
		if ok != tt.ok || (err != nil) != tt.bad {
			t.Errorf("SplitHubName(%q) = ok:%v err:%v; want ok:%v bad:%v", tt.in, ok, err, tt.ok, tt.bad)
			continue
		}
		if !tt.bad && (hub != tt.hub || machine != tt.machine) {
			t.Errorf("SplitHubName(%q) = %q,%q want %q,%q", tt.in, hub, machine, tt.hub, tt.machine)
		}
	}
	// Both directions of the runtime identifier, and a local name untouched.
	if got := RuntimeName("h/web"); got != "h@web" {
		t.Errorf("RuntimeName = %q, want h@web", got)
	}
	if got := DisplayName("h@web"); got != "h/web" {
		t.Errorf("DisplayName = %q, want h/web", got)
	}
	if got := RuntimeName("web"); got != "web" {
		t.Errorf("RuntimeName(local) = %q", got)
	}
	if got := DisplayName(RuntimeName("h/web")); got != "h/web" {
		t.Errorf("round trip = %q", got)
	}
	if got := JoinHubName("h", "web"); got != "h/web" {
		t.Errorf("JoinHubName = %q", got)
	}
	// '@' and '/' are both outside nameRe, so neither form collides with a
	// local machine name.
	for _, n := range []string{"h@web", "h/web"} {
		if ValidName(n) == nil {
			t.Errorf("ValidName(%q) should fail", n)
		}
	}
}

func TestLoadHubMachine(t *testing.T) {
	p := Paths{Config: t.TempDir(), State: t.TempDir()}
	writeFile(t, p, `
[machines.h]
backend = "hub"
host = "host:2222"
user = "u"
identity = "~/.ssh/k"
connect_timeout = 30

[machines.r]
backend = "remote"
host = "r"
`)
	m, err := LoadHubMachine(p, "h/web")
	if err != nil {
		t.Fatalf("LoadHubMachine: %v", err)
	}
	if m.Name != "h/web" || !m.IsHubMachine() || m.IsHub() || m.Hub == nil || m.Hub.Name != "h" {
		t.Errorf("record shape wrong: %+v", m)
	}
	if m.HubMachineName() != "web" {
		t.Errorf("HubMachineName = %q", m.HubMachineName())
	}
	if m.SSHHost != "u@host" || m.SSHPort != 2222 || m.Identity != "~/.ssh/k" || m.ConnectTimeout != 30 {
		t.Errorf("hub ssh fields not carried: %+v", m)
	}
	if m.Managed() || m.Probeable() {
		t.Error("a hub machine is neither managed here nor probed")
	}
	if _, err := LoadHubMachine(p, "r/web"); err == nil || !strings.Contains(err.Error(), "not a hub") {
		t.Errorf("non-hub entry as hub: err=%v", err)
	}
	if _, err := LoadHubMachine(p, "ghost/web"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing hub entry: %v", err)
	}
	if _, err := LoadHubMachine(p, "web"); err == nil {
		t.Error("bare name is not a hub reference")
	}
	// LoadAny dispatches on the form.
	if any, err := LoadAny(p, "h/web"); err != nil || !any.IsHubMachine() {
		t.Errorf("LoadAny(h/web): %+v %v", any, err)
	}
	if any, err := LoadAny(p, "r"); err != nil || any.IsHubMachine() || any.Backend != BackendRemote {
		t.Errorf("LoadAny(r): %+v %v", any, err)
	}
	if _, err := LoadAny(p, "h/"); err == nil {
		t.Error("LoadAny(h/) should fail validation")
	}
}

// Probeable is remote entries only: not a hub, not a machine on one, not a
// smol VM. A method on the record (config.md §9 item 1), preserved across
// the loader change.
func TestProbeable(t *testing.T) {
	hub := NewHub("h", "u@h")
	for _, tc := range []struct {
		m    *Machine
		want bool
	}{
		{NewRemote("a", true, "u@a"), true},
		{NewRemote("b", false, "u@b"), true},
		{hub, false},
		{&Machine{Name: "h/web", Backend: BackendHub, Hub: hub}, false},
		{&Machine{Name: "vm", Backend: BackendSmol}, false},
	} {
		if got := tc.m.Probeable(); got != tc.want {
			t.Errorf("%s (%s): Probeable = %v, want %v", tc.m.Name, tc.m.Backend, got, tc.want)
		}
	}
}
