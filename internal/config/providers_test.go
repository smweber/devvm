package config

import (
	"strings"
	"testing"
)

func TestProvidersTable(t *testing.T) {
	p := Paths{Config: t.TempDir(), State: t.TempDir()}
	writeFile(t, p, `
[providers.hetzner]
location = "fsn1"
ssh_key = "scott-laptop"
ipv4 = false

[templates.default]
image = "debian-12"

[templates.big]
server_type = "cx43"
location = "hel1"
ipv4 = true
memory = 8192
`)
	f, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !f.HasProviders() || strings.Join(f.ProviderNames(), ",") != "hetzner" {
		t.Fatalf("providers = %+v", f.Providers)
	}
	hz := f.Providers["hetzner"]
	def := f.Templates[DefaultTemplate]
	// default: image from the template, location and ipv4 from the table,
	// server_type built-in.
	pl := def.Under(def).Resolve(DefaultTemplate).Placement(hz)
	if pl != (Placement{ServerType: DefaultServerType, Location: "fsn1", Image: "debian-12", IPv4: false, SSHKey: "scott-laptop"}) {
		t.Errorf("default placement = %+v", pl)
	}
	// big over default: its own type, location and ipv4; default's image.
	pl = f.Templates["big"].Under(def).Resolve("big").Placement(hz)
	if pl != (Placement{ServerType: "cx43", Location: "hel1", Image: "debian-12", IPv4: true, SSHKey: "scott-laptop"}) {
		t.Errorf("big placement = %+v", pl)
	}
	// No table at all: the built-ins.
	pl = TemplateSpec{}.Resolve("x").Placement(ProviderSpec{})
	if pl != (Placement{ServerType: "cx23", Location: "nbg1", Image: "ubuntu-24.04", IPv4: true}) {
		t.Errorf("built-in placement = %+v", pl)
	}
	if got := strings.Join(f.Templates["big"].ProviderFields(), ","); got != "server_type,location,ipv4" {
		t.Errorf("ProviderFields = %q", got)
	}
	if fs := (TemplateSpec{}).ProviderFields(); len(fs) != 0 {
		t.Errorf("empty spec fields = %v", fs)
	}
}

func TestNewProviderMachine(t *testing.T) {
	s := ProviderServer{ID: 42, Status: "running", State: "running", Address: "1.2.3.4", Type: "cx23", Location: "nbg1"}
	m := NewProviderMachine("web", BackendHetzner, s)
	if !m.IsProvider() || !m.IsRemote() || !m.Probeable() || !m.Managed() || m.IsHub() ||
		m.SSHHost != "dev@1.2.3.4" || m.SSHPort != 22 || m.User != "" || m.Server.ID != 42 {
		t.Fatalf("managed = %+v", m)
	}
	if dark := NewProviderMachine("dark", BackendHetzner, ProviderServer{ID: 7}); dark.SSHHost != "" {
		t.Fatalf("no address = %+v", dark)
	}
	if (&Machine{Name: "x", Backend: BackendRemote}).IsProvider() {
		t.Fatal("a remote entry is not a provider record")
	}
}
