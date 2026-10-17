package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func writeFile(t *testing.T, p Paths, body string) {
	t.Helper()
	if err := os.MkdirAll(p.Config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.FilePath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The templates table family is read; any other top-level table (step 5's
// [machines.*], or something newer) is ignored without error.
func TestLoadTemplates(t *testing.T) {
	p := Paths{Config: t.TempDir(), State: t.TempDir()}
	if ts, err := LoadTemplates(p); err != nil || len(ts) != 0 {
		t.Fatalf("missing file: %v, %v", ts, err)
	}
	writeFile(t, p, `
[templates.default]
hook = "url:https://x/b.sh --yes"
memory = 4096
disk = 20

[templates.devvm]
memory = 8192
repos = ["smweber/devvm"]
ports = ["3000", "5173"]
authorized_keys_github = ["smweber"]
harden = false

[machines.cloud]
backend = "remote"
host = "2.29.47.196"

[providers.hetzner]
token_env = "X"
`)
	ts, err := LoadTemplates(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || *ts["devvm"].Memory != 8192 || *ts["default"].Hook != "url:https://x/b.sh --yes" {
		t.Fatalf("templates = %+v", ts)
	}
	if h := ts["devvm"].Harden; h == nil || *h {
		t.Errorf("harden = false must read as set: %v", h)
	}
	if ts["devvm"].Disk != nil || ts["devvm"].User != nil {
		t.Error("unset fields must stay unset")
	}
}

func TestLoadTemplatesRejects(t *testing.T) {
	for body, want := range map[string]string{
		"[templates.x]\nmemroy = 1\n":       "unknown template key",
		"[templates.x]\nmemory = 100\n":     "too small",
		"[templates.x]\nuser = \"root\"\n":  "root",
		"[templates.x]\nuser = \"A B\"\n":   "invalid user",
		"[templates.\"a/b\"]\nmemory = 1\n": "invalid",
		"[templates.x\n":                    "parse",
	} {
		p := Paths{Config: t.TempDir()}
		writeFile(t, p, body)
		if _, err := LoadTemplates(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
}

// Unset fields fall back to [templates.default], one level, then to the
// built-ins; a named template that does not exist is an error, a missing
// default is not.
func TestTemplateFallbackChain(t *testing.T) {
	p := Paths{Config: t.TempDir()}
	writeFile(t, p, `
[templates.default]
user = "alice"
hook = "cmd:/d"
memory = 4096
repos = ["o/default"]
harden = true

[templates.web]
memory = 8192
repos = []
harden = false
`)
	named, def, err := TemplateLayers(p, "web")
	if err != nil {
		t.Fatal(err)
	}
	got := named.Under(def).Resolve("web")
	want := Template{Name: "web", User: "alice", Hook: "cmd:/d", Memory: 8192, Repos: []string{}, Harden: false}
	if got.Name != want.Name || got.User != want.User || got.Hook != want.Hook || got.Memory != want.Memory ||
		len(got.Repos) != 0 || got.Harden || got.Disk != 0 {
		t.Errorf("web = %+v, want %+v", got, want)
	}
	// Default alone, and the built-ins under an empty file.
	_, def, _ = TemplateLayers(p, "")
	if d := def.Resolve(DefaultTemplate); d.Memory != 4096 || !slices.Equal(d.Repos, []string{"o/default"}) || !d.Harden {
		t.Errorf("default = %+v", d)
	}
	if b := (TemplateSpec{}).Resolve(DefaultTemplate); b.User != "dev" || b.Hook != "none" || b.Memory != 0 {
		t.Errorf("built-ins = %+v", b)
	}
	if _, _, err := TemplateLayers(p, "nope"); err == nil || !strings.Contains(err.Error(), "default, web") {
		t.Errorf("unknown template: %v", err)
	}
	empty := Paths{Config: t.TempDir()}
	if _, _, err := TemplateLayers(empty, ""); err != nil {
		t.Errorf("no file, default: %v", err)
	}
	// config.toml sits under the file's layers.
	d := &Defaults{BootstrapHook: "cmd:/old", Memory: 1024, Disk: 9}
	m := (TemplateSpec{}).Under(def).Under(DefaultsSpec(d)).Resolve("x")
	if m.Hook != "cmd:/d" || m.Memory != 4096 || m.Disk != 9 {
		t.Errorf("with config.toml = %+v", m)
	}
}

// Spec round-trips a resolved template with every field set, so nothing
// below it can fill a gap.
func TestTemplateSpecRoundTrip(t *testing.T) {
	in := Template{Name: "x", User: "u", Hook: "none", Repos: []string{"a"}}
	out := in.Spec().Under(TemplateSpec{Memory: ptr(4096), Harden: ptr(true)}).Resolve("x")
	if out.Memory != 0 || out.Harden || out.User != "u" || !slices.Equal(out.Repos, []string{"a"}) {
		t.Errorf("round trip = %+v", out)
	}
}

// An empty string is unset: it falls through like a missing key.
func TestTemplateEmptyStringIsUnset(t *testing.T) {
	p := Paths{Config: t.TempDir()}
	writeFile(t, p, "[templates.default]\nhook = \"cmd:/d\"\nuser = \"alice\"\n[templates.x]\nhook = \"\"\nuser = \"\"\n")
	named, def, err := TemplateLayers(p, "x")
	if err != nil {
		t.Fatal(err)
	}
	if got := named.Under(def).Resolve("x"); got.Hook != "cmd:/d" || got.User != "alice" {
		t.Errorf("x = %+v, want default's hook and user", got)
	}
}

func ptr[T any](v T) *T { return &v }
