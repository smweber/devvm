package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

// gatherCreateSpec with Yes=true forces the non-interactive path regardless of
// any controlling terminal, so these exercise pure flag > template >
// compiled resolution without touching huh.

// newTestApp's config dir is short (shortTempDir, under /tmp): tests that
// serve a daemon socket under run/ would otherwise exceed macOS's 104-byte
// unix socket path limit whenever the test's name is long, since
// t.TempDir() there is /var/folders/…/T/TestName…/001.
func newTestApp(t *testing.T) *App {
	t.Helper()
	return &App{Paths: testPaths(shortTempDir(t))}
}

func TestResolveDefaultTemplateFillsUnsetFields(t *testing.T) {
	a := newTestApp(t)
	writeDevvmTOML(t, a, "[templates.default]\nhook = \"cmd:/opt/setup.sh\"\nmemory = 4096\n")
	s := createSpec{Name: "box", Backend: config.BackendSmol, Yes: true}
	if err := a.gatherCreateSpec(&s); err != nil {
		t.Fatalf("gatherCreateSpec: %v", err)
	}
	if s.Memory != 4096 {
		t.Errorf("memory = %d, want 4096 from [templates.default]", s.Memory)
	}
	if s.Hook != "cmd:/opt/setup.sh" {
		t.Errorf("hook = %q, want the template's", s.Hook)
	}
}

func TestResolveFlagBeatsTemplate(t *testing.T) {
	a := newTestApp(t)
	writeDevvmTOML(t, a, "[templates.default]\nhook = \"cmd:/opt/setup.sh\"\nmemory = 4096\n")
	s := createSpec{Name: "box", Backend: config.BackendSmol, tmplFlags: tmplFlags{Memory: 1024, Hook: "none"}, Yes: true}
	if err := a.gatherCreateSpec(&s); err != nil {
		t.Fatalf("gatherCreateSpec: %v", err)
	}
	if s.Memory != 1024 {
		t.Errorf("memory = %d, want 1024 (flag wins)", s.Memory)
	}
	if s.Hook != "none" {
		t.Errorf("hook = %q, want none (flag wins)", s.Hook)
	}
}

func TestResolveCompiledDefaultWhenUnset(t *testing.T) {
	a := newTestApp(t) // no devvm.toml
	s := createSpec{Name: "box", Backend: config.BackendSmol, tmplFlags: tmplFlags{Memory: 2048}, Yes: true}
	if err := a.gatherCreateSpec(&s); err != nil {
		t.Fatalf("gatherCreateSpec: %v", err)
	}
	if s.Hook != "none" || s.User != "dev" {
		t.Errorf("hook = %q, user = %q, want the built-ins none, dev", s.Hook, s.User)
	}
}

func TestResolveRequiredMemoryErrorsWithoutDefault(t *testing.T) {
	a := newTestApp(t) // no devvm.toml, no --memory
	s := createSpec{Name: "box", Backend: config.BackendSmol, Yes: true}
	if err := a.gatherCreateSpec(&s); err == nil {
		t.Fatal("expected error: smol needs memory with no flag and no template memory")
	}
}

// Remote create is gone (config.md §4): `create --backend remote|hub`
// points at `config add`, before any prompt or dial.
func TestCreateRefusesRemoteBackends(t *testing.T) {
	for _, be := range []string{"remote", "hub", "remote-managed", "remote-unmanaged"} {
		a := newTestApp(t)
		err := a.runCreate(createSpec{Name: "box", Backend: be, Yes: true})
		if err == nil || !strings.Contains(err.Error(), "config add") {
			t.Errorf("create --backend %s = %v, want a pointer at config add", be, err)
		}
	}
}

// `create NAME` refuses while [machines.NAME] exists: the entry would
// shadow the new VM.
func TestCreateRefusesEntryName(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	writeDevvmTOML(t, a, "[machines.box]\nbackend = \"remote\"\nhost = \"b\"\n")
	err := a.runCreate(createSpec{Name: "box", Yes: true, tmplFlags: tmplFlags{Memory: 1024}})
	if err == nil || !strings.Contains(err.Error(), "config rm box") {
		t.Errorf("create over an entry = %v", err)
	}
}

// `create HUB/NAME --backend remote` is refused on this host, before any
// proxying.
func TestCreateHubMachineRefusesBackendLocally(t *testing.T) {
	a := newTestApp(t)
	log := fakeSSH(t, "exit 0")
	err := runTree(t, a, "create", "h/x", "--backend", "remote")
	if err == nil || !strings.Contains(err.Error(), "config add") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Error("proxied before refusing")
	}
}

// delete's completion offers smol VMs and hub machines, never an entry.
func TestDeleteCompletionSkipsEntries(t *testing.T) {
	a := newTestApp(t)
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}})
	if err := writeEntry(a.Paths, config.NewRemote("box", true, "box")); err != nil {
		t.Fatal(err)
	}
	got, _ := a.completeDeletable(nil, nil, "")
	if strings.Join(got, ",") != "h/web" {
		t.Errorf("delete completion = %v", got)
	}
}
