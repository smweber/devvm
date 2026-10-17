package config

import (
	"os"
	"testing"
)

func TestManagedMarkerRoundTrip(t *testing.T) {
	p := Paths{Config: t.TempDir(), State: t.TempDir()}
	if _, ok, err := ReadManagedMarker(p, "web"); ok || err != nil {
		t.Fatalf("no marker: ok=%v err=%v", ok, err)
	}
	if err := WriteManagedMarker(p, "web", ""); err == nil {
		t.Error("a marker with no created_at was written")
	}
	if err := WriteManagedMarker(p, "web", "1789490152"); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := ReadManagedMarker(p, "web"); !ok || err != nil || got != "1789490152" {
		t.Errorf("read = %q, %v, %v", got, ok, err)
	}
	if fi, err := os.Stat(p.ManagedPath("web")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("marker mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	if err := RemoveManagedMarker(p, "web"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveManagedMarker(p, "web"); err != nil {
		t.Errorf("second remove: %v", err)
	}
	if _, ok, _ := ReadManagedMarker(p, "web"); ok {
		t.Error("marker survived its removal")
	}
	if err := WriteManagedMarker(p, "../x", "1"); err == nil {
		t.Error("a marker name escaped the managed dir")
	}
}
