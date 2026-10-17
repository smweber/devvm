package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
	"github.com/smweber/devvm/internal/provider/fake"
	"github.com/smweber/devvm/internal/provider/hetzner"
)

const providersTOML = "[providers.hetzner]\nlocation = \"fsn1\"\n"

// withFakeHetzner configures [providers.hetzner] (plus extra), injects the
// fake as the API, and stores a token unless noToken. It returns the fake.
func withFakeHetzner(t *testing.T, a *App, extra string, noToken bool) *fake.Provider {
	t.Helper()
	t.Setenv("HCLOUD_TOKEN", "")
	writeDevvmTOML(t, a, providersTOML+extra)
	p := fake.New()
	a.providerAPI = func(name, token string) (provider.Provider, error) {
		if name != config.BackendHetzner || token == "" {
			t.Fatalf("providerAPI(%q, %q)", name, token)
		}
		return p, nil
	}
	if !noToken {
		if err := writeTokenFile(provider.TokenPath(a.Paths, "hetzner"), "test-token"); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func addServer(t *testing.T, p *fake.Provider, name, addr, state string, managed bool) {
	t.Helper()
	labels := map[string]string{}
	if managed {
		labels[provider.LabelManaged] = provider.LabelManagedValue
	}
	if _, err := p.Add(provider.Server{Name: name, Address: addr, State: state, Type: "cx23", Location: "nbg1", Labels: labels}); err != nil {
		t.Fatal(err)
	}
}

// The provider is resolve's last source: consulted only after the entry and
// smol sources miss, one Get, and only with a token.
func TestProviderResolveOrder(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[{"name":"vm","state":"running","created_at":1}]`)
	p := withFakeHetzner(t, a, "[templates.default]\nuser = \"ops\"\n\n[machines.web]\nbackend = \"remote\"\nhost = \"web.example\"\n", false)
	addServer(t, p, "web", "1.1.1.1", provider.StateRunning, true)
	addServer(t, p, "vm", "2.2.2.2", provider.StateRunning, true)
	addServer(t, p, "box", "3.3.3.3", provider.StateStopped, true)
	addServer(t, p, "prod", "4.4.4.4", provider.StateRunning, false)

	if m, _, err := a.resolveAny("web"); err != nil || m.Backend != config.BackendRemote {
		t.Fatalf("web = %+v, %v (the entry shadows the server)", m, err)
	}
	if m, _, err := a.resolveAny("vm"); err != nil || m.Backend != config.BackendSmol {
		t.Fatalf("vm = %+v, %v (the smol VM shadows the server)", m, err)
	}
	if n := p.CallCount("Get"); n != 0 {
		t.Fatalf("the API was asked %d times for names a local source holds", n)
	}
	m, b, err := a.resolveAny("box")
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsProvider() || m.Backend != config.BackendHetzner || !m.Managed() || !m.IsRemote() || !m.Probeable() ||
		m.SSHHost != "ops@3.3.3.3" || m.SSHPort != 22 || m.User != "" || m.Server.ID == 0 || m.Server.Status != "off" {
		t.Fatalf("box record = %+v %+v", m, m.Server)
	}
	if b.Kind() != config.BackendHetzner {
		t.Fatalf("Kind = %q", b.Kind())
	}
	if _, ok := b.(backend.SSHConnector); !ok {
		t.Fatal("a hetzner record must get the ssh backend")
	}
	if st, err := b.Status(); err != nil || st.Running || !strings.Contains(st.Raw, "stopped") {
		t.Fatalf("Status = %+v, %v (from the API row, never a probe)", st, err)
	}
	if n := p.CallCount("Get"); n != 1 {
		t.Fatalf("Get = %d, want 1", n)
	}
	if m, _, err := a.resolveAny("prod"); err != nil || m.Managed() || m.SSHHost != "4.4.4.4" {
		t.Fatalf("prod = %+v, %v (unmanaged: no user@)", m, err)
	}
	if _, _, err := a.resolveAny("nothere"); err == nil || !strings.Contains(err.Error(), "unknown machine") {
		t.Fatalf("nothere: %v", err)
	}

	// No token: the source is skipped, and named only when nothing matched.
	if err := os.Remove(provider.TokenPath(a.Paths, "hetzner")); err != nil {
		t.Fatal(err)
	}
	gets := p.CallCount("Get")
	_, _, err = a.resolveAny("box")
	if err == nil || !strings.Contains(err.Error(), "devvm providers token hetzner") {
		t.Fatalf("no token: %v", err)
	}
	if p.CallCount("Get") != gets {
		t.Fatal("no token must not dial")
	}
	// An API failure is the error, not "unknown machine".
	writeTokenFile(provider.TokenPath(a.Paths, "hetzner"), "t")
	p.Err = errors.New("hetzner: GET /servers: 503")
	if _, _, err := a.resolveAny("box"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("API error: %v", err)
	}
}

// Without [providers.*] no provider is consulted, token or not.
func TestProviderResolveNeedsTable(t *testing.T) {
	a := newTestApp(t)
	p := withFakeHetzner(t, a, "", false)
	addServer(t, p, "box", "3.3.3.3", provider.StateRunning, true)
	writeDevvmTOML(t, a, "[templates.default]\nuser = \"dev\"\n")
	if _, _, err := a.resolveAny("box"); err == nil {
		t.Fatal("resolved a provider server with no [providers.*]")
	}
	if p.CallCount("Get") != 0 {
		t.Fatal("dialled the API with no [providers.*]")
	}
}

// The daemon resolves a provider box from the last listing only: never the
// API, never the token, and a cache miss is an error naming the refresh. A
// live lookup merges its answer into that listing first.
func TestProviderDaemonCacheOnly(t *testing.T) {
	a := newTestApp(t)
	p := withFakeHetzner(t, a, "", false)
	addServer(t, p, "box", "3.3.3.3", provider.StateRunning, true)
	addServer(t, p, "late", "5.5.5.5", provider.StateRunning, true)
	a.providerRound(context.Background())
	if err := p.Delete(context.Background(), mustServer(t, p, "late").ID); err != nil {
		t.Fatal(err)
	}
	addServer(t, p, "fresh", "6.6.6.6", provider.StateRunning, true)
	// A live lookup of a new name adds its row; of a vanished one drops it.
	if _, _, err := a.resolveAny("fresh"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.resolveAny("late"); err == nil {
		t.Fatal("late resolved after its server went")
	}
	c := readProviderCache(a.Paths, "hetzner")
	names := map[string]bool{}
	for _, s := range c.servers {
		names[s.Name] = true
	}
	if !names["box"] || !names["fresh"] || names["late"] || c.fetched.IsZero() {
		t.Fatalf("merged cache = %+v", c)
	}
	before, _ := os.ReadFile(providerCachePath(a.Paths, "hetzner"))
	if !strings.Contains(string(before), "fetched=") {
		t.Fatal("a merge lost fetched")
	}

	// The daemon: a token file it would warn about proves it never reads it.
	if err := os.Chmod(provider.TokenPath(a.Paths, "hetzner"), 0o644); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	old := hetzner.Warn
	hetzner.Warn = &warn
	t.Cleanup(func() { hetzner.Warn = old })
	gets, lists := p.CallCount("Get"), p.CallCount("List")
	restore := a.cachedProviders()
	m, _, err := a.resolveAny("fresh")
	if err != nil || m.SSHHost != "dev@6.6.6.6" || !m.Managed() {
		t.Fatalf("fresh = %+v, %v", m, err)
	}
	_, _, err = a.resolveAny("uncached")
	restore()
	if err == nil || !strings.Contains(err.Error(), "no cached hetzner server uncached; run 'devvm status'") {
		t.Fatalf("cache miss: %v", err)
	}
	if err := runTree(t, a, "__daemon", "uncached"); err == nil || !strings.Contains(err.Error(), "no cached hetzner server") {
		t.Fatalf("__daemon miss: %v", err)
	}
	if p.CallCount("Get") != gets || p.CallCount("List") != lists {
		t.Fatal("the daemon path dialled the API")
	}
	if warn.Len() != 0 {
		t.Fatalf("the daemon path read the token: %q", warn.String())
	}
}

func mustServer(t *testing.T, p *fake.Provider, name string) *provider.Server {
	t.Helper()
	s, err := p.Get(context.Background(), name)
	if err != nil || s == nil {
		t.Fatalf("server %s: %v", name, err)
	}
	return s
}

// __session, config rm and guest-path completion read local sources only:
// zero API calls whatever the name.
func TestProviderLocalOnlyPaths(t *testing.T) {
	a := newTestApp(t)
	fakeSSH(t, "exit 255")
	p := withFakeHetzner(t, a, "\n[machines.web]\nbackend = \"remote\"\nhost = \"w\"\n", false)
	addServer(t, p, "box", "3.3.3.3", provider.StateRunning, true)
	a.providerRound(context.Background())
	gets := p.CallCount("Get")
	if err := runTree(t, a, "__session", "box"); err == nil || !strings.Contains(err.Error(), "a hub does not relay one") {
		t.Errorf("__session box: %v", err)
	}
	if err := runTree(t, a, "__session", "gone"); err == nil {
		t.Error("__session gone resolved")
	}
	a.completeCopyGuestPath(context.Background(), "box", "/tm")
	a.completeCopyGuestPath(context.Background(), "gone", "/tm")
	if err := runTree(t, a, "config", "rm", "web"); err != nil {
		t.Fatal(err)
	}
	if p.CallCount("Get") != gets {
		t.Fatalf("Get = %d, want %d: a local-only path dialled the API", p.CallCount("Get"), gets)
	}
}

// An entry that is a hub's own row is marked shadowed by a cached server too.
func TestProviderShadowsHubRow(t *testing.T) {
	a := newTestApp(t)
	fakeSSH(t, "exit 255")
	p := withFakeHetzner(t, a, "\n[machines.hubx]\nbackend = \"hub\"\nhost = \"h\"\n", false)
	addServer(t, p, "hubx", "7.7.7.7", provider.StateRunning, true)
	if err := runTree(t, a, "status", "--plain"); err != nil {
		t.Fatal(err)
	}
	out := a.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "hubx\thub\tunreachable\t-\tshadowed\t") || strings.Contains(out, "hubx\thetzner") {
		t.Errorf("--plain:\n%s", out)
	}
}

// shell and exec reach an unmanaged server through the tree (as its own
// login: no user@), and a server with no public address is refused.
func TestProviderUnmanagedShellExec(t *testing.T) {
	a := newTestApp(t)
	log := fakeSSH(t, "exit 0")
	p := withFakeHetzner(t, a, "", false)
	addServer(t, p, "prod", "4.4.4.4", provider.StateRunning, false)
	addServer(t, p, "dark", "", provider.StateRunning, true)
	if err := runTree(t, a, "exec", "prod", "true"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := runTree(t, a, "shell", "prod"); err != nil {
		t.Fatalf("shell: %v", err)
	}
	data, _ := os.ReadFile(log)
	if strings.Count(string(data), " 4.4.4.4 ") < 1 || strings.Contains(string(data), "@4.4.4.4") {
		t.Errorf("ssh calls:\n%s", data)
	}
	if err := runTree(t, a, "exec", "dark", "true"); err == nil || !strings.Contains(err.Error(), "no public address") {
		t.Errorf("dark: %v", err)
	}
}

// An unmanaged provider server: status, shell and exec only; the rest
// refuse with the never-promote line, and delete points at the console.
// Nothing on this host is touched by the refusals.
func TestProviderRefusals(t *testing.T) {
	a := newTestApp(t)
	fakeSSH(t, "exit 255")
	p := withFakeHetzner(t, a, "", false)
	addServer(t, p, "prod", "4.4.4.4", provider.StateRunning, false)
	addServer(t, p, "box", "3.3.3.3", provider.StateRunning, true)
	if err := config.SavePorts(a.Paths, "box", []string{"3000"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.resolveUnmanaged("prod"); err != nil {
		t.Fatalf("status/shell/exec path refused: %v", err)
	}
	for _, argv := range [][]string{
		{"attach", "prod"}, {"setup", "prod"}, {"setup", "--adopt", "prod"}, {"keys", "list", "prod"},
		{"repos", "list", "prod"}, {"ports", "up", "prod"}, {"auth", "prod"}, {"cp-in", "prod", "providers_test.go"},
	} {
		if err := runTree(t, a, argv...); err == nil || !strings.Contains(err.Error(), "never promotes a provider server") {
			t.Errorf("%v: %v", argv, err)
		}
	}
	if err := runTree(t, a, "delete", "prod"); err == nil || !strings.Contains(err.Error(), "Hetzner console") {
		t.Errorf("delete prod: %v", err)
	}
	if p.CallCount("Delete") != 0 {
		t.Error("delete prod reached the API")
	}
	if saved, _ := config.LoadPorts(a.Paths, "box"); len(saved) != 1 {
		t.Fatalf("a refusal touched box's saved forwards: %v", saved)
	}
}

// One-shot status lists the provider (one List) into the cache, a watch
// snapshot reads only the cache, and the rows carry the provider columns.
func TestProviderStatusRows(t *testing.T) {
	a := newTestApp(t)
	fakeSSH(t, "exit 255")
	fakeLocalSmolvm(t, `[{"name":"vm","state":"running","created_at":1}]`)
	p := withFakeHetzner(t, a, "\n[machines.web]\nbackend = \"remote\"\nhost = \"web.example\"\n", false)
	addServer(t, p, "box", "3.3.3.3", provider.StateRunning, true)
	addServer(t, p, "prod", "4.4.4.4", provider.StateStopped, false)
	addServer(t, p, "web", "1.1.1.1", provider.StateRunning, true)
	addServer(t, p, "vm", "2.2.2.2", provider.StateRunning, true)

	if err := runTree(t, a, "status", "--plain"); err != nil {
		t.Fatal(err)
	}
	out := a.Stdout.(*bytes.Buffer).String()
	for _, want := range []string{
		"box\thetzner\trunning\t-\tmanaged\tverified=-\n",
		"prod\thetzner\tstopped\t-\tunmanaged\tverified=-\n",
		"web\tremote\t?\t-\tshadowed\t",
		"vm\tsmol\trunning\t-\tshadowed\t-\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--plain lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "web\thetzner") || strings.Contains(out, "vm\thetzner") {
		t.Errorf("a shadowed server has a row:\n%s", out)
	}
	if p.CallCount("List") != 1 {
		t.Fatalf("List = %d, want 1", p.CallCount("List"))
	}
	data, err := os.ReadFile(filepath.Join(a.Paths.CacheDir(), "provider-hetzner.list"))
	if err != nil || !strings.Contains(string(data), "fetched=") || !strings.Contains(string(data), "box\t") {
		t.Fatalf("cache = %q, %v", data, err)
	}
	if strings.Contains(string(data), "test-token") {
		t.Fatal("the token leaked into the cache")
	}

	// A watch snapshot: the cache only.
	snap, err := a.plainSnapshot(statusScope{})
	if err != nil || !strings.Contains(snap, "box\thetzner\trunning") {
		t.Fatalf("snapshot = %q, %v", snap, err)
	}
	if p.CallCount("List") != 1 || p.CallCount("Get") != 0 {
		t.Fatal("a snapshot dialled the API")
	}

	// --local skips providers entirely.
	if err := runTree(t, a, "status", "--plain", "--local"); err != nil {
		t.Fatal(err)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); strings.Contains(out, "hetzner") {
		t.Errorf("--local listed a provider:\n%s", out)
	}
	if p.CallCount("List") != 1 {
		t.Fatal("--local listed the provider")
	}

	// The API fails: cached rows with their last state, never unreachable;
	// -v names the source and the age.
	p.Err = errors.New("hetzner: GET /servers: 503 service unavailable")
	if err := runTree(t, a, "status", "-v"); err != nil {
		t.Fatal(err)
	}
	out = a.Stdout.(*bytes.Buffer).String()
	for _, want := range []string{"hetzner: listing failed, cached ", "ago (hetzner: GET /servers: 503", "server: cx23 in nbg1, 3.3.3.3", "(stopped, still billed)", "contact: none recorded yet", "(shadows a hetzner server of the same name)"} {
		if !strings.Contains(out, want) {
			t.Errorf("-v lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "unreachable") {
		t.Errorf("a failed listing read as unreachable:\n%s", out)
	}

	// No token: the hint on stderr, the cache still shown.
	p.Err = nil
	os.Remove(provider.TokenPath(a.Paths, "hetzner"))
	if err := runTree(t, a, "status", "--plain"); err != nil {
		t.Fatal(err)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "box\thetzner\trunning") {
		t.Errorf("no token dropped the cached rows:\n%s", out)
	}
	if w := a.Stderr.(*bytes.Buffer).String(); strings.Count(w, "devvm providers token hetzner") != 1 {
		t.Errorf("hint = %q", w)
	}
	if err := runTree(t, a, "status", "-v"); err != nil {
		t.Fatal(err)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "hetzner: no token (devvm providers token hetzner), cached") {
		t.Errorf("-v no-token line missing:\n%s", out)
	}

	// status NAME on a provider box.
	writeTokenFile(provider.TokenPath(a.Paths, "hetzner"), "t")
	if err := runTree(t, a, "status", "prod"); err != nil {
		t.Fatal(err)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "prod (hetzner)") || !strings.Contains(out, "managed: no") || !strings.Contains(out, "still billed") {
		t.Errorf("status prod:\n%s", out)
	}
}

// No cache and no token: no rows, and -v still says why.
func TestProviderStatusNoCache(t *testing.T) {
	a := newTestApp(t)
	withFakeHetzner(t, a, "", true)
	if err := runTree(t, a, "status", "-v"); err != nil {
		t.Fatal(err)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "hetzner: no token (devvm providers token hetzner), nothing cached") {
		t.Errorf("-v:\n%s", out)
	}
}

func TestProviderCacheRoundTrip(t *testing.T) {
	a := newTestApp(t)
	in := providerCache{servers: []provider.Server{{ID: 7, Name: "box", State: "running", Status: "running", Address: "2a01::1", Type: "cx23", Location: "nbg1",
		Labels: map[string]string{"devvm": "true", "devvm-template": "big"}}}, err: "listing failed: x\ny"}
	if err := writeProviderCache(a.Paths, "hetzner", in); err != nil {
		t.Fatal(err)
	}
	got := readProviderCache(a.Paths, "hetzner")
	if len(got.servers) != 1 || got.servers[0].Address != "2a01::1" || got.servers[0].ID != 7 || !provider.Managed(got.servers[0]) ||
		got.servers[0].Labels["devvm-template"] != "big" || got.err != "listing failed: x y" {
		t.Fatalf("round trip = %+v", got)
	}
}

// The token command: validated before writing, 0600 in 0700, cleared.
func TestProvidersToken(t *testing.T) {
	a := newTestApp(t)
	p := withFakeHetzner(t, a, "", true)
	path := provider.TokenPath(a.Paths, "hetzner")

	p.Err = errors.New("hetzner: GET /servers: 401 unauthorized")
	a.Stdin = strings.NewReader("bad-token\n")
	if err := runTree(t, a, "providers", "token", "hetzner"); err == nil || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatalf("bad token: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a failed validation left a token file")
	}
	p.Err = nil
	a.Stdin = strings.NewReader("  good-token  \n")
	if err := runTree(t, a, "providers", "token", "hetzner"); err != nil {
		t.Fatal(err)
	}
	if out := a.Stdout.(*bytes.Buffer).String() + a.Stderr.(*bytes.Buffer).String(); strings.Contains(out, "good-token") {
		t.Fatal("the token was printed")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "good-token\n" {
		t.Fatalf("token file = %q, %v", data, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o", fi.Mode().Perm())
	}
	if p.CallCount("List") != 2 {
		t.Errorf("List = %d, want one per attempt", p.CallCount("List"))
	}
	for _, in := range []string{"", "\n", "two words\n"} {
		a.Stdin = strings.NewReader(in)
		if err := runTree(t, a, "providers", "token", "hetzner"); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	if err := runTree(t, a, "providers", "token", "aws"); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("aws: %v", err)
	}
	if err := runTree(t, a, "providers", "token", "hetzner", "--clear"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("--clear left the file")
	}
	if err := runTree(t, a, "providers", "token", "hetzner", "--clear"); err != nil {
		t.Fatalf("clearing nothing: %v", err)
	}
}

// config check: placement with no provider warns; a cached server shadowed
// by an entry warns; show covers the provider table and template fields.
func TestConfigCheckProviders(t *testing.T) {
	a := newTestApp(t)
	writeDevvmTOML(t, a, "[templates.big]\nserver_type = \"cx43\"\n")
	if err := runTree(t, a, "config", "check"); err != nil {
		t.Fatal(err)
	}
	if w := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(w, "[templates.big] sets server_type") || !strings.Contains(w, "no [providers.*]") {
		t.Errorf("warning = %q", w)
	}
	p := withFakeHetzner(t, a, "[templates.big]\nserver_type = \"cx43\"\nmemory = 8192\n\n[machines.web]\nbackend = \"remote\"\nhost = \"w\"\n", false)
	addServer(t, p, "web", "1.1.1.1", provider.StateRunning, true)
	a.providerRound(context.Background())
	if err := runTree(t, a, "config", "check"); err != nil {
		t.Fatal(err)
	}
	if w := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(w, "[machines.web]") || !strings.Contains(w, "shadows the Hetzner server 'web'") {
		t.Errorf("shadow warning = %q", w)
	}
	if err := runTree(t, a, "config", "show"); err != nil {
		t.Fatal(err)
	}
	out := a.Stdout.(*bytes.Buffer).String()
	for _, want := range []string{"[providers.hetzner]", `location = "fsn1"`, `image = "ubuntu-24.04"`, `server_type = "cx43"`, "from [providers.hetzner]"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
}

// memory/disk are ignored on a provider box, placement on anything else,
// on setup's one warning line.
func TestWarnIgnoredProviderFields(t *testing.T) {
	a := newTestApp(t)
	var w bytes.Buffer
	a.Stderr = &w
	pm := config.NewProviderMachine("box", config.BackendHetzner, "dev", true, config.ProviderServer{Address: "1.2.3.4"})
	a.warnIgnored(pm, config.Template{Name: "big", User: "dev", Memory: 8192, ServerType: "cx43"}, setupOpts{})
	if got := w.String(); !strings.Contains(got, "ignoring memory (not applicable to a Hetzner server)") {
		t.Errorf("provider: %q", got)
	}
	// On a remote box: placement the chosen template sets is named, what
	// it only inherits from default is not.
	writeDevvmTOML(t, a, "[templates.default]\nlocation = \"fsn1\"\n\n[templates.big]\nserver_type = \"cx43\"\nipv4 = false\n")
	v := false
	for _, tc := range []struct{ tmpl, want string }{
		{"big", "ignoring server_type, ipv4 (not applicable to a remote box)"},
		{config.DefaultTemplate, ""},
	} {
		w.Reset()
		a.warnIgnored(config.NewRemote("r", true, "dev@h"), config.Template{Name: tc.tmpl, User: "dev", ServerType: "cx43", Location: "fsn1", IPv4: &v}, setupOpts{})
		if got := w.String(); (tc.want == "" && got != "") || !strings.Contains(got, tc.want) {
			t.Errorf("remote, %s: %q", tc.tmpl, got)
		}
	}
}
