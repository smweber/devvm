package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveDropsNoise verifies confs stay clean: smol carries no ssh transport
// fields, and no backend carries a spurious zero-valued int (BurntSushi keeps
// those despite omitempty).
func TestSaveDropsNoise(t *testing.T) {
	dir := t.TempDir()

	sm := NewSmol("s")
	sm.Memory = 1024
	if err := sm.Save(dir); err != nil {
		t.Fatal(err)
	}
	smData, _ := os.ReadFile(filepath.Join(MachinesDir(dir), "s.toml"))
	for _, bad := range []string{"ssh_port", "vnc_port", "transport", "memory = 0"} {
		if strings.Contains(string(smData), bad) {
			t.Errorf("smol conf should not contain %q:\n%s", bad, smData)
		}
	}
	if !strings.Contains(string(smData), "memory = 1024") {
		t.Errorf("smol conf lost memory:\n%s", smData)
	}

	rm := NewRemote("r", BackendRemoteUnmanaged, "h")
	if err := rm.Save(dir); err != nil {
		t.Fatal(err)
	}
	rmData, _ := os.ReadFile(filepath.Join(MachinesDir(dir), "r.toml"))
	if strings.Contains(string(rmData), "memory") {
		t.Errorf("remote conf should not contain memory:\n%s", rmData)
	}
}

func TestValidName(t *testing.T) {
	ok := []string{"dev", "devbox3", "a.b_c-d", "A1"}
	for _, n := range ok {
		if err := ValidName(n); err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", n, err)
		}
	}
	bad := []string{"", "-lead", ".lead", "has space", "semi;colon", "sl/ash"}
	for _, n := range bad {
		if err := ValidName(n); err == nil {
			t.Errorf("ValidName(%q) = nil, want error", n)
		}
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		m       Machine
		wantErr bool
	}{
		{"smol ok", Machine{Name: "x", Backend: BackendSmol}, false},
		{"remote-managed ok", Machine{Name: "x", Backend: BackendRemoteManaged, SSHHost: "h"}, false},
		{"remote-unmanaged ok", Machine{Name: "x", Backend: BackendRemoteUnmanaged, SSHHost: "h"}, false},
		{"remote no host", Machine{Name: "x", Backend: BackendRemoteManaged}, true},
		{"no backend", Machine{Name: "x"}, true},
		{"bad backend", Machine{Name: "x", Backend: "hetzner"}, true},
		{"mosh transport ok", Machine{Name: "x", Backend: BackendRemoteManaged, SSHHost: "h", Transport: TransportMosh}, false},
		{"transport on smol", Machine{Name: "x", Backend: BackendSmol, Transport: TransportMosh}, true},
		{"bad transport", Machine{Name: "x", Backend: BackendRemoteManaged, SSHHost: "h", Transport: "telnet"}, true},
	}
	for _, tt := range tests {
		if err := tt.m.Validate(); (err != nil) != tt.wantErr {
			t.Errorf("%s: Validate() err=%v wantErr=%v", tt.name, err, tt.wantErr)
		}
	}
}

func TestManagedAndRemote(t *testing.T) {
	tests := []struct {
		backend           string
		managed, isRemote bool
	}{
		{BackendSmol, true, false},
		{BackendRemoteManaged, true, true},
		{BackendRemoteUnmanaged, false, true},
	}
	for _, tt := range tests {
		m := &Machine{Backend: tt.backend}
		if m.Managed() != tt.managed {
			t.Errorf("%s: Managed()=%v want %v", tt.backend, m.Managed(), tt.managed)
		}
		if m.IsRemote() != tt.isRemote {
			t.Errorf("%s: IsRemote()=%v want %v", tt.backend, m.IsRemote(), tt.isRemote)
		}
	}
}

// TestMigrateLegacySSH covers loading pre-rename confs: `backend = "ssh"` maps to
// remote-unmanaged when unmanaged=true, else remote-managed, and the deprecated
// key is dropped.
func TestMigrateLegacySSH(t *testing.T) {
	tests := []struct {
		name, raw   string
		wantBackend string
	}{
		{"legacy-unmanaged", "backend = \"ssh\"\nunmanaged = true\nssh_host = \"h\"\n", BackendRemoteUnmanaged},
		{"legacy-managed", "backend = \"ssh\"\nssh_host = \"h\"\n", BackendRemoteManaged},
	}
	for _, tt := range tests {
		dir := t.TempDir()
		if err := os.MkdirAll(MachinesDir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(MachinesDir(dir), tt.name+".toml"), []byte(tt.raw), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := Load(dir, tt.name)
		if err != nil {
			t.Fatalf("%s: Load: %v", tt.name, err)
		}
		if got.Backend != tt.wantBackend {
			t.Errorf("%s: Backend=%q want %q", tt.name, got.Backend, tt.wantBackend)
		}
		if got.Unmanaged {
			t.Errorf("%s: Unmanaged should be cleared after migration", tt.name)
		}
		if got.TransportName() != TransportSSH {
			t.Errorf("%s: TransportName()=%q want ssh", tt.name, got.TransportName())
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	orig := &Machine{
		Name:                 "devbox3",
		Backend:              BackendRemoteUnmanaged,
		SSHHost:              "devbox3",
		Transport:            TransportMosh,
		Ports:                []string{"3000", "5901", "8080:80"},
		VNCPort:              5901,
		MoshServer:           "/home/linuxbrew/.linuxbrew/bin/mosh-server",
		AuthorizedKeysGithub: []string{"alice", "bob"},
	}
	if err := orig.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir, "devbox3")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Backend != BackendRemoteUnmanaged || got.SSHHost != "devbox3" {
		t.Errorf("core fields lost: %+v", got)
	}
	if got.Transport != TransportMosh {
		t.Errorf("Transport lost: %q", got.Transport)
	}
	if got.SSHPort != 22 {
		t.Errorf("SSHPort default = %d, want 22", got.SSHPort)
	}
	if got.BootstrapHook != DefaultBootstrapHook {
		t.Errorf("BootstrapHook default = %q, want default", got.BootstrapHook)
	}
	if len(got.Ports) != 3 || got.Ports[2] != "8080:80" {
		t.Errorf("Ports lost: %v", got.Ports)
	}
	if len(got.AuthorizedKeysGithub) != 2 {
		t.Errorf("AuthorizedKeysGithub lost: %v", got.AuthorizedKeysGithub)
	}
}

func TestLoadNotFound(t *testing.T) {
	if _, err := Load(t.TempDir(), "ghost"); err == nil {
		t.Fatal("Load(missing) = nil error, want ErrNotFound")
	}
}

func TestSplitPort(t *testing.T) {
	tests := []struct{ in, h, g string }{
		{"3000", "3000", "3000"},
		{"8080:80", "8080", "80"},
		{"1455:1455", "1455", "1455"},
	}
	for _, tt := range tests {
		h, g := SplitPort(tt.in)
		if h != tt.h || g != tt.g {
			t.Errorf("SplitPort(%q) = %q,%q want %q,%q", tt.in, h, g, tt.h, tt.g)
		}
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b"} {
		(&Machine{Name: n, Backend: BackendSmol}).Save(dir)
	}
	names, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 2 {
		t.Errorf("List = %v, want 2", names)
	}
}
