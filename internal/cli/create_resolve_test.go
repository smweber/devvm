package cli

import (
	"testing"

	"github.com/smweber/devvm/internal/config"
)

// gatherCreateSpec with Yes=true forces the non-interactive path regardless of
// any controlling terminal, so these exercise pure flag > config.toml > compiled
// resolution without touching huh.

func newTestApp(t *testing.T) *App {
	t.Helper()
	return &App{ConfigDir: t.TempDir()}
}

func TestResolveGlobalDefaultsFillUnsetFields(t *testing.T) {
	a := newTestApp(t)
	if err := config.SaveDefaults(a.ConfigDir, &config.Defaults{
		Provision: "cmd:/opt/setup.sh",
		Memory:    4096,
	}); err != nil {
		t.Fatal(err)
	}
	s := createSpec{Name: "box", Backend: config.BackendSmol, Yes: true}
	if err := a.gatherCreateSpec(&s); err != nil {
		t.Fatalf("gatherCreateSpec: %v", err)
	}
	if s.Memory != 4096 {
		t.Errorf("memory = %d, want 4096 from config.toml", s.Memory)
	}
	if s.Provision != "cmd:/opt/setup.sh" {
		t.Errorf("provision = %q, want config.toml value", s.Provision)
	}
}

func TestResolveFlagBeatsGlobalDefault(t *testing.T) {
	a := newTestApp(t)
	if err := config.SaveDefaults(a.ConfigDir, &config.Defaults{Memory: 4096, Provision: "cmd:/opt/setup.sh"}); err != nil {
		t.Fatal(err)
	}
	s := createSpec{Name: "box", Backend: config.BackendSmol, Memory: 1024, Provision: "none", Yes: true}
	if err := a.gatherCreateSpec(&s); err != nil {
		t.Fatalf("gatherCreateSpec: %v", err)
	}
	if s.Memory != 1024 {
		t.Errorf("memory = %d, want 1024 (flag wins)", s.Memory)
	}
	if s.Provision != "none" {
		t.Errorf("provision = %q, want none (flag wins)", s.Provision)
	}
}

func TestResolveCompiledDefaultWhenUnset(t *testing.T) {
	a := newTestApp(t) // no config.toml
	s := createSpec{Name: "box", Backend: config.BackendSmol, Memory: 2048, Yes: true}
	if err := a.gatherCreateSpec(&s); err != nil {
		t.Fatalf("gatherCreateSpec: %v", err)
	}
	// gatherCreateSpec leaves provision empty; machine() applies the compiled "none".
	if s.Provision != "" {
		t.Errorf("provision = %q, want empty before defaulting", s.Provision)
	}
	m, err := s.machine()
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	if m.Provision != "none" {
		t.Errorf("machine provision = %q, want compiled none", m.Provision)
	}
}

func TestResolveRequiredMemoryErrorsWithoutDefault(t *testing.T) {
	a := newTestApp(t) // no config.toml, no --memory
	s := createSpec{Name: "box", Backend: config.BackendSmol, Yes: true}
	if err := a.gatherCreateSpec(&s); err == nil {
		t.Fatal("expected error: smol needs memory with no flag and no config.toml")
	}
}

func TestResolveRemoteTransportFromGlobal(t *testing.T) {
	a := newTestApp(t)
	if err := config.SaveDefaults(a.ConfigDir, &config.Defaults{Transport: config.TransportMosh}); err != nil {
		t.Fatal(err)
	}
	s := createSpec{Name: "box", Backend: config.BackendRemoteManaged, SSHHost: "h", Yes: true}
	if err := a.gatherCreateSpec(&s); err != nil {
		t.Fatalf("gatherCreateSpec: %v", err)
	}
	if s.Transport != config.TransportMosh {
		t.Errorf("transport = %q, want mosh from config.toml", s.Transport)
	}
}
