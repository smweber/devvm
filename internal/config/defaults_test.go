package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsMissing(t *testing.T) {
	// No config.toml → zero Defaults, no error (every compiled default applies).
	d, err := LoadDefaults(t.TempDir())
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}
	if d.Provision != "" || d.Memory != 0 || d.Transport != "" {
		t.Errorf("missing config should be zero Defaults, got %+v", d)
	}
}

func TestSaveLoadDefaultsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := &Defaults{Provision: "url:https://x/b.sh --yes", Memory: 4096, Transport: TransportMosh}
	if err := SaveDefaults(dir, in); err != nil {
		t.Fatalf("SaveDefaults: %v", err)
	}
	out, err := LoadDefaults(dir)
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}
	if *out != *in {
		t.Errorf("round trip: got %+v want %+v", out, in)
	}
}

func TestSaveDefaultsStripsZeroMemory(t *testing.T) {
	dir := t.TempDir()
	if err := SaveDefaults(dir, &Defaults{Transport: TransportSSH}); err != nil {
		t.Fatalf("SaveDefaults: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); contains(got, "memory = 0") {
		t.Errorf("zero memory should be stripped, got:\n%s", got)
	}
}

func TestLoadDefaultsInvalid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("transport = \"telnet\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDefaults(dir); err == nil {
		t.Error("expected error for invalid transport")
	}
}

func TestDefaultsSetGetUnset(t *testing.T) {
	d := &Defaults{}
	if err := d.Set("memory", "2048"); err != nil {
		t.Fatalf("Set memory: %v", err)
	}
	if v, set, _ := d.Get("memory"); !set || v != "2048" {
		t.Errorf("Get memory = %q,%v", v, set)
	}
	if err := d.Set("memory", "100"); err == nil {
		t.Error("memory below 512 should be rejected")
	}
	if err := d.Set("transport", "mosh"); err != nil {
		t.Fatalf("Set transport: %v", err)
	}
	if err := d.Set("transport", "carrier-pigeon"); err == nil {
		t.Error("bad transport should be rejected")
	}
	if err := d.Set("bogus", "x"); err == nil {
		t.Error("unknown key should be rejected")
	}
	if err := d.Unset("memory"); err != nil {
		t.Fatalf("Unset: %v", err)
	}
	if _, set, _ := d.Get("memory"); set {
		t.Error("memory should be unset")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
