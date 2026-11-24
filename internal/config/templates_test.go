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

// The templates table family is read alongside [machines.*].
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
authorized_keys_github = ["smweber"]
harden = false

[machines.cloud]
backend = "remote"
host = "2.29.47.196"
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
	if ts["devvm"].Disk != nil || ts["devvm"].Hook != nil {
		t.Error("unset fields must stay unset")
	}
}

func TestLoadTemplatesRejects(t *testing.T) {
	for body, want := range map[string]string{
		"[templates.x]\nmemroy = 1\n":                            "[templates.x]: unknown key \"memroy\"",
		"[templates.x]\nmemory = 100\n":                          "too small",
		"[templates.x]\nuser = \"dev\"\n":                        "[templates.x]: user is gone from templates (a box devvm makes always has the user dev); delete the line",
		"[templates.\"a/b\"]\nmemory = 1\n":                      "invalid",
		"[templates.x\n":                                         "parse",
		"[templates.x]\nports = [\"3000\"]\n":                    "ports is gone from templates; delete the line and save each forward with 'devvm ports add NAME PORT'",
		"[templates.x]\nshare_auth = [\"github\", \"gitlab\"]\n": "[templates.x]: share_auth: unknown tool \"gitlab\" (have: github)",
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
hook = "cmd:/d"
memory = 4096
repos = ["o/default"]
harden = true
share_auth = ["github"]

[templates.web]
memory = 8192
repos = []
harden = false
share_auth = []

[templates.api]
`)
	named, def, err := TemplateLayers(p, "web")
	if err != nil {
		t.Fatal(err)
	}
	got := named.Under(def).Resolve("web")
	want := Template{Name: "web", Hook: "cmd:/d", Memory: 8192, Repos: []string{}, Harden: false}
	if got.Name != want.Name || got.Hook != want.Hook || got.Memory != want.Memory ||
		len(got.Repos) != 0 || got.Harden || got.Disk != 0 || len(got.ShareAuth) != 0 {
		t.Errorf("web = %+v, want %+v", got, want)
	}
	// share_auth falls back to default like any field; [] turns it off.
	named, def, _ = TemplateLayers(p, "api")
	if got := named.Under(def).Resolve("api"); !slices.Equal(got.ShareAuth, []string{ShareAuthGitHub}) {
		t.Errorf("api share_auth = %q, want default's", got.ShareAuth)
	}
	// Default alone, and the built-ins under an empty file.
	_, def, _ = TemplateLayers(p, "")
	if d := def.Resolve(DefaultTemplate); d.Memory != 4096 || !slices.Equal(d.Repos, []string{"o/default"}) || !d.Harden {
		t.Errorf("default = %+v", d)
	}
	if b := (TemplateSpec{}).Resolve(DefaultTemplate); b.Hook != "none" || b.Memory != 0 {
		t.Errorf("built-ins = %+v", b)
	}
	if b := (TemplateSpec{}).Resolve(DefaultTemplate); len(b.ShareAuth) != 0 {
		t.Errorf("built-in share_auth = %q, want none", b.ShareAuth)
	}
	// harden is on unless a layer turns it off; fail2ban stays off.
	if b := (TemplateSpec{}).Resolve(DefaultTemplate); !b.Harden || b.Fail2ban {
		t.Errorf("built-in harden/fail2ban = %v/%v, want on/off", b.Harden, b.Fail2ban)
	}
	named, def, _ = TemplateLayers(p, "api")
	if got := named.Under(def).Resolve("api"); !got.Harden {
		t.Errorf("api harden = %v, want default's true", got.Harden)
	}
	if _, _, err := TemplateLayers(p, "nope"); err == nil || !strings.Contains(err.Error(), "api, default, web") {
		t.Errorf("unknown template: %v", err)
	}
	empty := Paths{Config: t.TempDir()}
	if _, _, err := TemplateLayers(empty, ""); err != nil {
		t.Errorf("no file, default: %v", err)
	}
}

// An empty string is unset: it falls through like a missing key.
func TestTemplateEmptyStringIsUnset(t *testing.T) {
	p := Paths{Config: t.TempDir()}
	writeFile(t, p, "[templates.default]\nhook = \"cmd:/d\"\n[templates.x]\nhook = \"\"\n")
	named, def, err := TemplateLayers(p, "x")
	if err != nil {
		t.Fatal(err)
	}
	if got := named.Under(def).Resolve("x"); got.Hook != "cmd:/d" {
		t.Errorf("x = %+v, want default's hook", got)
	}
}

func ptr[T any](v T) *T { return &v }
