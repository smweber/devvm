package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConf(t *testing.T, dir, name, raw string) {
	t.Helper()
	if err := os.MkdirAll(MachinesDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(MachinesDir(dir), name+".toml"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A hub conf with [machines.NAME] tables loads, validates, and survives Save
// (which strips zero-int and default lines by column-0 regex; the nested
// tables are indented and must come through untouched).
func TestHubConfRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeConf(t, dir, "h", `backend = "hub"
ssh_host = "scott@desktop"

[machines.web]
ports = ["3000", "8443:443"]

[machines.api]
`)
	h, err := Load(dir, "h")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !h.IsHub() || h.IsHubMachine() || h.Managed() || !h.IsRemote() {
		t.Errorf("hub predicates wrong: IsHub=%v IsHubMachine=%v Managed=%v IsRemote=%v",
			h.IsHub(), h.IsHubMachine(), h.Managed(), h.IsRemote())
	}
	if h.SSHPort != 22 || h.TransportName() != TransportSSH {
		t.Errorf("hub ssh defaults not applied: port=%d transport=%q", h.SSHPort, h.Transport)
	}
	if got := h.Machines["web"].Ports; len(got) != 2 || got[1] != "8443:443" {
		t.Errorf("[machines.web] ports = %v", got)
	}
	if _, ok := h.Machines["api"]; !ok {
		t.Errorf("empty [machines.api] table lost: %v", h.Machines)
	}
	if err := h.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(MachinesDir(dir), "h.toml"))
	if !strings.Contains(string(data), "[machines.web]") || !strings.Contains(string(data), `"8443:443"`) {
		t.Errorf("saved conf lost the machine table:\n%s", data)
	}
	for _, bad := range []string{"memory", "\nports", "transport", "bootstrap_hook"} {
		if strings.Contains(string(data), bad) {
			t.Errorf("hub conf should not contain %q:\n%s", bad, data)
		}
	}
	again, err := Load(dir, "h")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(again.Machines) != 2 || again.Machines["web"].Ports[0] != "3000" {
		t.Errorf("round-trip changed the tables: %v", again.Machines)
	}
}

func TestValidateHub(t *testing.T) {
	hub := func(mut func(*Machine)) Machine {
		m := Machine{Name: "h", Backend: BackendHub, SSHHost: "x"}
		mut(&m)
		return m
	}
	tests := []struct {
		name    string
		m       Machine
		wantErr string // substring; "" means valid
	}{
		{"bare hub", hub(func(m *Machine) {}), ""},
		{"no host", hub(func(m *Machine) { m.SSHHost = "" }), "ssh_host"},
		{"ports", hub(func(m *Machine) { m.Ports = []string{"3000"} }), "ports"},
		{"memory", hub(func(m *Machine) { m.Memory = 1024 }), "memory"},
		{"disk", hub(func(m *Machine) { m.Disk = 10 }), "disk"},
		{"repos", hub(func(m *Machine) { m.Repos = []string{"a/b"} }), "repos"},
		{"hook", hub(func(m *Machine) { m.BootstrapHook = "cmd:/x" }), "bootstrap_hook"},
		{"default hook ok", hub(func(m *Machine) { m.BootstrapHook = DefaultBootstrapHook }), ""},
		{"github keys", hub(func(m *Machine) { m.AuthorizedKeysGithub = []string{"a"} }), "authorized_keys_github"},
		{"harden", hub(func(m *Machine) { m.Harden = true }), "harden"},
		{"several named", hub(func(m *Machine) { m.Ports = []string{"1"}; m.Memory = 1 }), "ports, memory"},
		{"mosh ok", hub(func(m *Machine) { m.Transport = TransportMosh }), ""},
		{"tables ok", hub(func(m *Machine) { m.Machines = map[string]HubMachine{"web": {Ports: []string{"1"}}} }), ""},
		{"bad table name", hub(func(m *Machine) { m.Machines = map[string]HubMachine{"a/b": {}} }), "[machines.a/b]"},
		{"tables on a remote", Machine{Name: "r", Backend: BackendRemoteManaged, SSHHost: "x",
			Machines: map[string]HubMachine{"web": {}}}, "only apply to a hub"},
		{"tables on smol", Machine{Name: "s", Backend: BackendSmol,
			Machines: map[string]HubMachine{"web": {}}}, "only apply to a hub"},
	}
	for _, tt := range tests {
		err := tt.m.Validate()
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tt.name, err)
		case tt.wantErr != "" && err == nil:
			t.Errorf("%s: want error containing %q, got nil", tt.name, tt.wantErr)
		case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
			t.Errorf("%s: error %q does not mention %q", tt.name, err, tt.wantErr)
		}
	}
}

// A hub conf carrying ports/memory is refused at Load too, so `status` shows
// it as a broken conf rather than silently treating the hub as a machine.
func TestLoadRejectsShapedHub(t *testing.T) {
	dir := t.TempDir()
	writeConf(t, dir, "h", "backend = \"hub\"\nssh_host = \"x\"\nports = [\"3000\"]\nmemory = 1024\n")
	if _, err := Load(dir, "h"); err == nil || !strings.Contains(err.Error(), "ports, memory") {
		t.Fatalf("Load = %v, want the ports/memory rejection", err)
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
	dir := t.TempDir()
	writeConf(t, dir, "h", "backend = \"hub\"\nssh_host = \"u@host\"\nssh_port = 2222\nidentity = \"~/.ssh/k\"\n\n[machines.web]\nports = [\"3000\"]\n")
	writeConf(t, dir, "r", "backend = \"remote-unmanaged\"\nssh_host = \"r\"\n")

	m, err := LoadHubMachine(dir, "h/web")
	if err != nil {
		t.Fatalf("LoadHubMachine: %v", err)
	}
	if m.Name != "h/web" || !m.IsHubMachine() || m.IsHub() || m.Hub == nil || m.Hub.Name != "h" {
		t.Errorf("record shape wrong: %+v", m)
	}
	if m.HubMachineName() != "web" {
		t.Errorf("HubMachineName = %q", m.HubMachineName())
	}
	if m.SSHHost != "u@host" || m.SSHPort != 2222 || m.Identity != "~/.ssh/k" {
		t.Errorf("hub ssh fields not carried: %+v", m)
	}
	if len(m.Ports) != 1 || m.Ports[0] != "3000" {
		t.Errorf("table ports not carried: %v", m.Ports)
	}
	if err := m.Save(dir); err == nil {
		t.Error("Save on a hub-machine record should refuse (the table lives in the hub conf)")
	}

	// A machine with no table is still a valid reference: resolve never asks
	// the hub whether it exists.
	if m2, err := LoadHubMachine(dir, "h/api"); err != nil || len(m2.Ports) != 0 {
		t.Errorf("untabled machine: m=%+v err=%v", m2, err)
	}
	if _, err := LoadHubMachine(dir, "r/web"); err == nil || !strings.Contains(err.Error(), "not a hub") {
		t.Errorf("non-hub conf as hub: err=%v", err)
	}
	if _, err := LoadHubMachine(dir, "ghost/web"); err == nil {
		t.Error("missing hub conf should fail")
	}
	if _, err := LoadHubMachine(dir, "web"); err == nil {
		t.Error("bare name is not a hub reference")
	}
	// LoadAny dispatches on the form.
	if any, err := LoadAny(dir, "h/web"); err != nil || !any.IsHubMachine() {
		t.Errorf("LoadAny(h/web): %+v %v", any, err)
	}
	if any, err := LoadAny(dir, "r"); err != nil || any.IsHubMachine() || any.Backend != BackendRemoteUnmanaged {
		t.Errorf("LoadAny(r): %+v %v", any, err)
	}
	if _, err := LoadAny(dir, "h/"); err == nil {
		t.Error("LoadAny(h/) should fail validation")
	}
}
