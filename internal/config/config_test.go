package config

import (
	"testing"
)

func TestValidName(t *testing.T) {
	ok := []string{"dev", "scottdev3", "a.b_c-d", "A1"}
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
		{"ssh ok", Machine{Name: "x", Backend: BackendSSH, SSHHost: "h"}, false},
		{"ssh no host", Machine{Name: "x", Backend: BackendSSH}, true},
		{"no backend", Machine{Name: "x"}, true},
		{"bad backend", Machine{Name: "x", Backend: "hetzner"}, true},
	}
	for _, tt := range tests {
		if err := tt.m.Validate(); (err != nil) != tt.wantErr {
			t.Errorf("%s: Validate() err=%v wantErr=%v", tt.name, err, tt.wantErr)
		}
	}
}

func TestManagedSSH(t *testing.T) {
	if !(&Machine{Backend: BackendSSH}).ManagedSSH() {
		t.Error("managed ssh host should be managed")
	}
	if (&Machine{Backend: BackendSSH, Unmanaged: true}).ManagedSSH() {
		t.Error("unmanaged ssh host should not be managed")
	}
	if (&Machine{Backend: BackendSmol}).ManagedSSH() {
		t.Error("smol should not be managed-ssh")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	orig := &Machine{
		Name:                 "scottdev3",
		Backend:              BackendSSH,
		Unmanaged:            true,
		SSHHost:              "scottdev3",
		Ports:                []string{"3000", "5901", "8080:80"},
		VNCPort:              5901,
		MoshServer:           "/home/linuxbrew/.linuxbrew/bin/mosh-server",
		AuthorizedKeysGithub: []string{"alice", "bob"},
	}
	if err := orig.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir, "scottdev3")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Backend != BackendSSH || !got.Unmanaged || got.SSHHost != "scottdev3" {
		t.Errorf("core fields lost: %+v", got)
	}
	if got.SSHPort != 22 {
		t.Errorf("SSHPort default = %d, want 22", got.SSHPort)
	}
	if got.Provision != DefaultProvision {
		t.Errorf("Provision default = %q, want default", got.Provision)
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
