package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
	"github.com/smweber/devvm/internal/provider/fake"
	"github.com/smweber/devvm/internal/provider/hetzner"
	"github.com/smweber/devvm/internal/reach"
)

// A real-looking ed25519 public key (ssh-keygen accepts it in known_hosts).
const testPubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl me@laptop"

// spyProvider is the fake with hooks: onCreate runs after the fake made the
// server (so a test can see what was printed by then, or plant a stale pin
// for the new address; it may change the returned copy), createErr fails
// Create outright, and deleteErr replaces Delete's answer after the
// fake deleted it (the "already gone" race).
type spyProvider struct {
	*fake.Provider
	onCreate  func(*provider.Server)
	createErr error // returned with no server, and nothing made
	deleteErr error
}

func (s *spyProvider) Create(ctx context.Context, spec provider.Spec) (*provider.Server, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	srv, err := s.Provider.Create(ctx, spec)
	if s.onCreate != nil {
		s.onCreate(srv)
	}
	return srv, err
}

func (s *spyProvider) Delete(ctx context.Context, id int64) error {
	err := s.Provider.Delete(ctx, id)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return err
}

func useSpy(a *App, p *fake.Provider) *spyProvider {
	s := &spyProvider{Provider: p}
	a.providerAPI = func(string, string) (provider.Provider, error) { return s, nil }
	return s
}

// answerConfirm stubs the y/N prompt, recording each question. askConfirm
// (like pickBackend and openTTY) is a package global: tests that stub it
// must not use t.Parallel.
func answerConfirm(t *testing.T, answer bool) *[]string {
	t.Helper()
	var asked []string
	old := askConfirm
	askConfirm = func(q string) (bool, error) { asked = append(asked, q); return answer, nil }
	t.Cleanup(func() { askConfirm = old })
	return &asked
}

func cachedRow(a *App, name string) *provider.Server {
	for _, s := range readProviderCache(a.Paths, config.BackendHetzner).servers {
		if s.Name == name {
			return &s
		}
	}
	return nil
}

// Every refusal comes before Create: a name the provider would reject, a
// name this host or the project holds, a key that would not let this host
// in, a template user later commands would not log in as, smol's sizes,
// HUB/NAME, no token. None of them bills.
func TestProviderCreateRefusals(t *testing.T) {
	a := newTestApp(t)
	a.Stdin = strings.NewReader("") // never the first-use prompt
	fakeLocalSmolvm(t, `[{"name":"vm","state":"running","created_at":1}]`)
	sshLog := fakeSSH(t, "exit 0")
	priv := filepath.Join(t.TempDir(), "id")
	os.WriteFile(priv, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nxx\n"), 0o600)
	notKey := filepath.Join(t.TempDir(), "x.pub")
	os.WriteFile(notKey, []byte("hello world\n"), 0o644)

	base := "ssh_key = \"scott\"\n\n[templates.ops]\nuser = \"alice\"\n\n[machines.web]\nbackend = \"remote\"\nhost = \"w.example\"\n"
	p := withFakeHetzner(t, a, base, false)
	p.AddKey("scott", testPubKey)
	addServer(t, p, "box", "3.3.3.3", provider.StateRunning, true)
	addServer(t, p, "prod", "4.4.4.4", provider.StateRunning, false)

	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"create", "Web_1", "--backend", "hetzner", "--yes"}, "RFC 1123"},
		{[]string{"create", "web", "--backend", "hetzner", "--yes"}, "the entry [machines.web]"},
		{[]string{"create", "vm", "--backend", "hetzner", "--yes"}, "the local smol VM 'vm'"},
		{[]string{"create", "box", "--backend", "hetzner", "--yes"}, "already exists (server"},
		{[]string{"create", "prod", "--backend", "hetzner", "--yes"}, "pick another name"},
		{[]string{"create", "new", "--backend", "hetzner", "--yes", "--template", "ops"}, "always reached as [templates.default]'s user (dev)"},
		{[]string{"create", "new", "--backend", "hetzner", "--yes", "--memory", "2048"}, "--memory and --disk size a smol VM"},
		{[]string{"create", "new", "--backend", "hetzner", "--yes", "--location", "a b"}, "invalid location"},
		{[]string{"create", "h/new", "--backend", "hetzner", "--yes"}, "never on a hub"},
		{[]string{"create", "new", "--backend", "aws", "--yes"}, "invalid backend"},
	} {
		err := runTree(t, a, tc.argv...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.argv, err, tc.want)
		}
	}
	if !strings.Contains(fmt.Sprint(runTree(t, a, "create", "box", "--backend", "hetzner", "--yes")), "'devvm setup box' re-runs setup") {
		t.Error("a managed duplicate does not name setup/delete")
	}

	// Key problems, each before any API call that bills.
	for _, tc := range []struct{ table, want string }{
		{"", "names no ssh key"},
		{"ssh_key = \"nokey\"\n", "no key of that name"},
		{"ssh_key_file = \"" + priv + "\"\n", "is a private key"},
		{"ssh_key_file = \"" + notKey + "\"\n", "is not an ssh public key"},
		{"ssh_key_file = \"/nonexistent/k.pub\"\n", "no such file"},
	} {
		writeDevvmTOML(t, a, providersTOML+tc.table)
		if err := runTree(t, a, "create", "new", "--backend", "hetzner", "--yes"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("table %q: %v, want %q", tc.table, err, tc.want)
		}
	}
	// No [providers.hetzner] at all.
	writeDevvmTOML(t, a, "[templates.default]\nuser = \"dev\"\n")
	if err := runTree(t, a, "create", "new", "--backend", "hetzner", "--yes"); err == nil || !strings.Contains(err.Error(), "needs a [providers.hetzner] table") {
		t.Errorf("no table: %v", err)
	}
	// No token, no terminal: the command named, nothing dialled.
	writeDevvmTOML(t, a, providersTOML+"ssh_key = \"scott\"\n")
	os.Remove(provider.TokenPath(a.Paths, "hetzner"))
	gets := p.CallCount("Get")
	if err := runTree(t, a, "create", "new", "--backend", "hetzner", "--yes"); err == nil || !strings.Contains(err.Error(), "devvm providers token hetzner") {
		t.Errorf("no token: %v", err)
	}
	if p.CallCount("Get") != gets {
		t.Error("no token dialled the API")
	}
	if n := p.CallCount("Create"); n != 0 {
		t.Fatalf("a refusal called Create %d times", n)
	}
	if data, _ := os.ReadFile(sshLog); len(data) != 0 {
		t.Errorf("a refusal ran ssh:\n%s", data)
	}
}

// Placement: template > [providers.hetzner] > built-in, flags over all;
// the labels; ipv4 = false is a v6-only server. A Create that made the
// server and then failed names it and the two ways forward, keeps it in the
// cache, and never deletes it.
func TestProviderCreatePlacementAndPartialFailure(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	sshLog := fakeSSH(t, "exit 0")
	p := withFakeHetzner(t, a, "ssh_key = \"scott\"\nipv4 = false\nimage = \"ubuntu-22.04\"\n\n[templates.big]\nserver_type = \"cx43\"\n", false)
	p.AddKey("scott", testPubKey)
	p.CreateErr = errors.New("hetzner: action start_server failed")

	err := runTree(t, a, "create", "web", "--backend", "hetzner", "--template", "big", "--image", "debian-12", "--yes")
	if err == nil {
		t.Fatal("a failed Create succeeded")
	}
	specs := p.Specs()
	if len(specs) != 1 {
		t.Fatalf("Create called %d times", len(specs))
	}
	sp := specs[0]
	if sp.Name != "web" || sp.Type != "cx43" || sp.Location != "fsn1" || sp.Image != "debian-12" || sp.SSHKey != "scott" || !sp.IPv6Only ||
		sp.Labels[provider.LabelManaged] != provider.LabelManagedValue || sp.Labels[provider.LabelTemplate] != "big" || len(sp.Labels) != 2 {
		t.Fatalf("spec = %+v", sp)
	}
	row := cachedRow(a, "web")
	if row == nil {
		t.Fatal("the created server is not in the cache")
	}
	for _, want := range []string{"'web' was created", fmt.Sprintf("server %d", row.ID), row.Address, "start_server failed", "run: devvm setup web or devvm delete web"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if !strings.Contains(row.Address, ":") {
		t.Errorf("ipv4 = false made %q", row.Address)
	}
	if p.CallCount("Delete") != 0 {
		t.Fatal("a half-made server was deleted")
	}
	if data, _ := os.ReadFile(sshLog); len(data) != 0 {
		t.Errorf("setup ran after a failed Create:\n%s", data)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "cx43 in fsn1, image debian-12, IPv6 only") {
		t.Errorf("plan = %q", out)
	}
}

// The whole create: key uploaded from ssh_key_file, the bill notice before
// Create, the row cached before setup's first ssh, a stale pin for the
// reused address dropped, then setup unchanged: the fresh server answers
// only as root, so the user is created there (root adoption), and the
// rest runs as it.
func TestProviderCreateRunsSetup(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	shrinkReachWait(t, 2*time.Second, 10*time.Millisecond)
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	keyFile := filepath.Join(t.TempDir(), "id.pub")
	os.WriteFile(keyFile, []byte(testPubKey+"\n"), 0o644)
	p := withFakeHetzner(t, a, "ssh_key_file = \""+keyFile+"\"\n", false)
	spy := useSpy(a, p)

	made := filepath.Join(t.TempDir(), "made")
	cache := providerCachePath(a.Paths, config.BackendHetzner)
	log := fakeRemote(t, `grep -q '^web' `+cache+` 2>/dev/null || echo NOCACHE >&2
case "$last" in *useradd*) touch `+made+`;; esac
case "$dest" in
  root@*) ;;
  dev@*) [ -e `+made+` ] || { echo "$dest: Permission denied (publickey)." >&2; exit 255; };;
esac`)
	var atCreate string
	spy.onCreate = func(s *provider.Server) {
		atCreate = a.Stdout.(*bytes.Buffer).String()
		// A pin a deleted server left at this address, and one for another host.
		os.MkdirAll(a.Paths.State, 0o700)
		kh := s.Address + " " + testPubKey + "\nother.example " + testPubKey + "\n"
		os.WriteFile(a.Paths.KnownHostsPath(), []byte(kh), 0o600)
	}
	if err := runTree(t, a, "create", "web", "--backend", "hetzner", "--yes"); err != nil {
		t.Fatalf("create: %v\n%s", err, a.Stderr)
	}
	if !strings.Contains(atCreate, "Creating Hetzner server 'web': cx23 in fsn1, image ubuntu-24.04, IPv4 + IPv6, ssh key devvm-") ||
		!strings.Contains(atCreate, "It bills from now until 'devvm delete web'") {
		t.Errorf("printed before Create: %q", atCreate)
	}
	if p.CallCount("EnsureSSHKey") != 1 || p.CallCount("Create") != 1 {
		t.Errorf("EnsureSSHKey %d, Create %d", p.CallCount("EnsureSSHKey"), p.CallCount("Create"))
	}
	sp := p.Specs()[0]
	if !strings.HasPrefix(sp.SSHKey, "devvm-") || sp.SSHKey != providerKeyName(testPubKey) || sp.IPv6Only || sp.Labels[provider.LabelTemplate] != "default" {
		t.Errorf("spec = %+v", sp)
	}
	row := cachedRow(a, "web")
	if row == nil || row.State != provider.StateRunning || !provider.Managed(*row) {
		t.Fatalf("cache row = %+v", row)
	}
	data, _ := os.ReadFile(log)
	got := string(data)
	if strings.Contains(a.Stderr.(*bytes.Buffer).String(), "NOCACHE") {
		t.Error("setup's ssh ran before the row was cached")
	}
	inOrder(t, got,
		" dev@"+row.Address+" 'true'", " root@"+row.Address+" 'true'", // denied, then root
		"useradd", "'dev'", // the template user, as root
		"dev@"+row.Address, "build-essential", // prereqs as it
	)
	kh, _ := os.ReadFile(a.Paths.KnownHostsPath())
	if strings.Contains(string(kh), row.Address+" ") || !strings.Contains(string(kh), "other.example") {
		t.Errorf("known_hosts after create:\n%s", kh)
	}
	if e := reach.Read(a.Paths, "web"); e.State != reach.Reachable || e.How != "setup" {
		t.Errorf("reach = %+v", e)
	}
	out := a.Stdout.(*bytes.Buffer).String()
	inOrder(t, out, "Created 'web' (server", "at "+row.Address+"; setting it up", "Machine 'web' (hetzner) is ready.")
	if p.CallCount("Delete") != 0 {
		t.Error("create deleted something")
	}

	// A setup failure leaves the server and says so.
	fakeRemote(t, `exit 255`)
	if err := runTree(t, a, "create", "web2", "--backend", "hetzner", "--yes"); err == nil ||
		!strings.Contains(err.Error(), "was created") || !strings.Contains(err.Error(), "setup failed") ||
		!strings.Contains(err.Error(), "run: devvm setup web2 or devvm delete web2") {
		t.Errorf("setup failure: %v", err)
	}
	if s, _ := p.Get(context.Background(), "web2"); s == nil || p.CallCount("Delete") != 0 {
		t.Error("a server whose setup failed was deleted")
	}
	// The key was found, not uploaded again.
	if p.CallCount("EnsureSSHKey") != 2 {
		t.Errorf("EnsureSSHKey = %d", p.CallCount("EnsureSSHKey"))
	}
}

// delete: the confirm names type and address, declining touches nothing;
// confirmed (or --yes), the server goes, then this host's state for it:
// saved forwards, reach cache, the pinned host key in both its forms (the
// bare address, and the [host]:port the user's ssh config made of it), and
// the cache row. Already gone at the provider: the same cleanup, said so.
func TestProviderDelete(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	sshWithConfig(t, "Host 192.0.2.50\n  Port 2222\n")
	p := withFakeHetzner(t, a, "", false)
	spy := useSpy(a, p)
	addServer(t, p, "web", "192.0.2.50", provider.StateRunning, true)
	addServer(t, p, "six", "2001:db8::7", provider.StateStopped, true)
	config.SavePorts(a.Paths, "web", []string{"3000"})
	config.SavePorts(a.Paths, "six", []string{"3000"})
	reach.Record(a.Paths, "web", "attach")
	os.MkdirAll(a.Paths.State, 0o700)
	kh := "192.0.2.50 " + testPubKey + "\n[192.0.2.50]:2222 " + testPubKey + "\n2001:db8::7 " + testPubKey + "\nkeep.example " + testPubKey + "\n"
	os.WriteFile(a.Paths.KnownHostsPath(), []byte(kh), 0o600)

	asked := answerConfirm(t, false)
	if err := runTree(t, a, "delete", "web"); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("declined: %v", err)
	}
	if p.CallCount("Delete") != 0 {
		t.Fatal("declined delete called the API")
	}
	if saved, _ := config.LoadPorts(a.Paths, "web"); len(saved) != 1 {
		t.Fatal("declined delete touched host state")
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "cx23 in nbg1, 192.0.2.50") {
		t.Fatalf("prompt = %q", *asked)
	}

	answerConfirm(t, true)
	if err := runTree(t, a, "delete", "web"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if p.CallCount("Delete") != 1 {
		t.Fatalf("Delete = %d", p.CallCount("Delete"))
	}
	if s, _ := p.Get(context.Background(), "web"); s != nil {
		t.Fatal("the server survived")
	}
	if saved, _ := config.LoadPorts(a.Paths, "web"); len(saved) != 0 {
		t.Errorf("saved forwards survived: %v", saved)
	}
	if e := reach.Read(a.Paths, "web"); e.State != "" && e.State != reach.Unknown {
		t.Errorf("reach survived: %+v", e)
	}
	if cachedRow(a, "web") != nil {
		t.Error("cache row survived")
	}
	data, _ := os.ReadFile(a.Paths.KnownHostsPath())
	if strings.Contains(string(data), "192.0.2.50") || !strings.Contains(string(data), "keep.example") || !strings.Contains(string(data), "2001:db8::7") {
		t.Errorf("known_hosts after delete web:\n%s", data)
	}

	// --yes, and the server already gone by the time Delete runs.
	asked = answerConfirm(t, false)
	spy.deleteErr = fmt.Errorf("hetzner: DELETE /servers/1: %w", provider.ErrNotFound)
	if err := runTree(t, a, "delete", "six", "--yes"); err != nil {
		t.Fatalf("delete six: %v", err)
	}
	if len(*asked) != 0 {
		t.Error("--yes still asked")
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "was already gone") {
		t.Errorf("out = %q", out)
	}
	if saved, _ := config.LoadPorts(a.Paths, "six"); len(saved) != 0 || cachedRow(a, "six") != nil {
		t.Error("already-gone cleanup skipped")
	}
	data, _ = os.ReadFile(a.Paths.KnownHostsPath())
	if strings.Contains(string(data), "2001:db8::7") || !strings.Contains(string(data), "keep.example") {
		t.Errorf("known_hosts after delete six:\n%s", data)
	}

	// Any other API error stops before the cleanup.
	addServer(t, p, "err", "192.0.2.51", provider.StateRunning, true)
	config.SavePorts(a.Paths, "err", []string{"3000"})
	spy.deleteErr = errors.New("hetzner: 503")
	if err := runTree(t, a, "delete", "err", "--yes"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("API error: %v", err)
	}
	if saved, _ := config.LoadPorts(a.Paths, "err"); len(saved) != 1 {
		t.Error("a failed delete cleared host state")
	}
}

// start/stop: the state resolve just fetched decides; the asked state is a
// no-op that says so (no action), otherwise Power the right way, the cache
// row updated. stop warns about billing every time. An unmanaged server
// confirms first; --yes skips it.
func TestProviderStartStop(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	fakeSSH(t, "exit 0")
	p := withFakeHetzner(t, a, "", false)
	addServer(t, p, "box", "192.0.2.70", provider.StateRunning, true)
	addServer(t, p, "prod", "192.0.2.71", provider.StateRunning, false)
	const warn = "devvm: box is stopped but still billed; devvm delete box frees it\n"
	asked := answerConfirm(t, false)

	step := func(argv []string, power int, state, out string) {
		t.Helper()
		if err := runTree(t, a, argv...); err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
		if n := p.CallCount("Power"); n != power {
			t.Fatalf("%v: Power = %d, want %d", argv, n, power)
		}
		if row := cachedRow(a, argv[1]); row == nil || row.State != state {
			t.Fatalf("%v: cache row = %+v, want %s", argv, row, state)
		}
		if got := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(got, out) {
			t.Errorf("%v: out = %q, want %q", argv, got, out)
		}
		w := a.Stderr.(*bytes.Buffer).String()
		if argv[0] == "stop" && argv[1] == "box" && !strings.Contains(w, warn) {
			t.Errorf("%v: no billing warning: %q", argv, w)
		}
		if argv[0] == "start" && strings.Contains(w, "billed") {
			t.Errorf("%v: start warned: %q", argv, w)
		}
	}
	step([]string{"stop", "box"}, 1, provider.StateStopped, "'box' is stopped")
	if s, _ := p.Get(context.Background(), "box"); s.State != provider.StateStopped {
		t.Fatal("Power went the wrong way")
	}
	step([]string{"stop", "box"}, 1, provider.StateStopped, "'box' is already stopped")
	step([]string{"start", "box"}, 2, provider.StateRunning, "'box' is running")
	step([]string{"start", "box"}, 2, provider.StateRunning, "'box' is already running")
	if len(*asked) != 0 {
		t.Fatalf("a managed box asked: %q", *asked)
	}

	// Unmanaged: declined, nothing happens; --yes, no question.
	if err := runTree(t, a, "stop", "prod"); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("declined: %v", err)
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "devvm did not make") || p.CallCount("Power") != 2 {
		t.Fatalf("asked %q, Power %d", *asked, p.CallCount("Power"))
	}
	step([]string{"stop", "prod", "--yes"}, 3, provider.StateStopped, "'prod' is stopped")
	answerConfirm(t, true)
	step([]string{"start", "prod"}, 4, provider.StateRunning, "'prod' is running")
	if s, _ := p.Get(context.Background(), "prod"); s.State != provider.StateRunning || provider.Managed(*s) {
		t.Fatalf("prod = %+v (power never relabels)", s)
	}
}

// RFC 1123 host names, lower case: what Hetzner takes as a server name.
func TestValidServerName(t *testing.T) {
	for _, ok := range []string{"web", "web-1", "a.b.c", "x", strings.Repeat("a", 63)} {
		if err := config.ValidServerName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Web", "web_1", "-web", "web-", "a..b", ".a", strings.Repeat("a", 64), strings.Repeat("a.", 127) + "a"} {
		if config.ValidServerName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Create returning no server: only an API answer (an HTTP error, a
// conflict) means nothing was made; anything else may have come after the
// POST was accepted, so the user is told to look.
func TestProviderCreateNoServer(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	fakeSSH(t, "exit 0")
	p := withFakeHetzner(t, a, "ssh_key = \"scott\"\n", false)
	p.AddKey("scott", testPubKey)
	spy := useSpy(a, p)
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&hetzner.APIError{Method: "POST", Path: "/servers", Status: 422, Code: "invalid_input", Message: "bad type"}, "the API refused it; no server was created"},
		{&hetzner.APIError{Method: "POST", Path: "/servers", Status: 409, Code: "uniqueness_error"}, "the API refused it"},
		{fmt.Errorf("fake: name used: %w", provider.ErrConflict), "the API refused it"},
		{errors.New("hetzner: POST /servers: dial tcp: i/o timeout"), "'web' may have been created and would bill: run 'devvm status' to list the project, then 'devvm delete web' if it exists"},
		{fmt.Errorf("hetzner: POST /servers: %w", context.DeadlineExceeded), "may have been created"},
	} {
		spy.createErr = tc.err
		err := runTree(t, a, "create", "web", "--backend", "hetzner", "--yes")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.err, err, tc.want)
		}
	}
}

// The create deadline: the server exists, the wait for its start runs
// out, and it comes back with the error. It is cached, named with the two
// commands, and never deleted.
func TestProviderCreateDeadline(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	sshLog := fakeSSH(t, "exit 0")
	p := withFakeHetzner(t, a, "ssh_key = \"scott\"\n", false)
	p.AddKey("scott", testPubKey)
	p.CreateBlocks = true
	old := providerTimes
	providerTimes.create = 50 * time.Millisecond
	t.Cleanup(func() { providerTimes = old })
	err := runTree(t, a, "create", "web", "--backend", "hetzner", "--yes")
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") || !strings.Contains(err.Error(), "'web' was created") ||
		!strings.Contains(err.Error(), "run: devvm setup web or devvm delete web") {
		t.Fatalf("deadline: %v", err)
	}
	if cachedRow(a, "web") == nil || p.CallCount("Delete") != 0 {
		t.Error("the server was lost track of, or deleted")
	}
	if data, _ := os.ReadFile(sshLog); len(data) != 0 {
		t.Errorf("setup ran:\n%s", data)
	}
}

// A failure between Create and setup names the server like every other
// post-Create error.
func TestProviderCreateBackendErr(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	fakeSSH(t, "exit 0")
	p := withFakeHetzner(t, a, "ssh_key = \"scott\"\n", false)
	p.AddKey("scott", testPubKey)
	old := providerBackendFor
	providerBackendFor = func(*config.Machine, config.Paths) (backend.Backend, error) { return nil, errors.New("no backend") }
	t.Cleanup(func() { providerBackendFor = old })
	err := runTree(t, a, "create", "web", "--backend", "hetzner", "--yes")
	row := cachedRow(a, "web")
	if err == nil || row == nil || !strings.Contains(err.Error(), fmt.Sprintf("'web' was created (server %d, %s)", row.ID, row.Address)) ||
		!strings.Contains(err.Error(), "no backend") || !strings.Contains(err.Error(), "run: devvm setup web or devvm delete web") {
		t.Fatalf("err = %v", err)
	}
}

// A pin a [machines.*] entry at the same address still uses is kept, and a
// host-key mismatch at setup then says which entry and how to clear it.
func TestProviderCreatePinHeldByEntry(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	shrinkReachWait(t, time.Second, 10*time.Millisecond)
	p := withFakeHetzner(t, a, "ssh_key = \"scott\"\n\n[machines.old]\nbackend = \"remote\"\nmanaged = true\nhost = \"192.0.2.250\"\n", false)
	p.AddKey("scott", testPubKey)
	spy := useSpy(a, p)
	spy.onCreate = func(s *provider.Server) { s.Address = "192.0.2.250" }
	os.MkdirAll(a.Paths.State, 0o700)
	os.WriteFile(a.Paths.KnownHostsPath(), []byte("192.0.2.250 "+testPubKey+"\n"), 0o600)
	fakeRemote(t, `case "$last" in "'true'") echo "@@@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @@@" >&2; exit 255;; esac`)
	err := runTree(t, a, "create", "web", "--backend", "hetzner", "--yes")
	if err == nil || !strings.Contains(err.Error(), "kept for the entry [machines.old]") ||
		!strings.Contains(err.Error(), "ssh-keygen -R 192.0.2.250 -f "+a.Paths.KnownHostsPath()) {
		t.Fatalf("err = %v", err)
	}
	if kh, _ := os.ReadFile(a.Paths.KnownHostsPath()); !strings.Contains(string(kh), "192.0.2.250") {
		t.Error("the entry's pin was dropped")
	}
}

// A cleanup failure after a successful Delete still reports the delete and
// drops the cache row, then returns the cleanup error.
func TestProviderDeleteCleanupFails(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	fakeSSH(t, "exit 0")
	p := withFakeHetzner(t, a, "", false)
	addServer(t, p, "web", "192.0.2.80", provider.StateRunning, true)
	os.MkdirAll(a.Paths.State, 0o700)
	os.WriteFile(a.Paths.PortsDir(), []byte("not a dir"), 0o600) // LockPorts cannot make it
	err := runTree(t, a, "delete", "web", "--yes")
	if err == nil || p.CallCount("Delete") != 1 {
		t.Fatalf("err = %v, Delete = %d", err, p.CallCount("Delete"))
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "deleted Hetzner server 'web', but clearing this host's state for it failed") {
		t.Errorf("out = %q", out)
	}
	if cachedRow(a, "web") != nil {
		t.Error("the cache row survived a successful Delete")
	}
}

// smol's delete takes --yes too; without it, a declined confirm aborts.
func TestSmolDeleteYes(t *testing.T) {
	a := newTestApp(t)
	log := fakeManagedSmol(t, a, "vm")
	asked := answerConfirm(t, false)
	if err := runTree(t, a, "delete", "vm"); err == nil || !strings.Contains(err.Error(), "aborted") || len(*asked) != 1 {
		t.Fatalf("declined: %v, asked %q", err, *asked)
	}
	if err := runTree(t, a, "delete", "vm", "--yes"); err != nil {
		t.Fatalf("--yes: %v", err)
	}
	if len(*asked) != 1 {
		t.Error("--yes asked")
	}
	if data, _ := os.ReadFile(log); !strings.Contains(string(data), "machine delete") && !strings.Contains(string(data), "machine rm") {
		t.Errorf("smolvm calls:\n%s", data)
	}
}

// askBackend asks only on a terminal, without --yes and with a provider
// configured; otherwise create stays smol (askBackend leaves it unset).
func TestAskBackend(t *testing.T) {
	a := newTestApp(t)
	tty, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	oldTTY, oldPick := openTTY, pickBackend
	t.Cleanup(func() { openTTY, pickBackend = oldTTY, oldPick })
	haveTTY := true
	openTTY = func() *os.File {
		if !haveTTY {
			return nil
		}
		f, _ := os.Open(tty.Name())
		return f
	}
	var asked [][]string
	pickBackend = func(_ *os.File, providers []string) (string, error) {
		asked = append(asked, providers)
		return config.BackendHetzner, nil
	}
	check := func(s createSpec, wantBackend string, wantAsked int) {
		t.Helper()
		if err := a.askBackend(&s); err != nil {
			t.Fatal(err)
		}
		if s.Backend != wantBackend || len(asked) != wantAsked {
			t.Fatalf("backend %q asked %d, want %q %d", s.Backend, len(asked), wantBackend, wantAsked)
		}
	}
	writeDevvmTOML(t, a, "[templates.default]\nuser = \"dev\"\n")
	check(createSpec{}, "", 0) // no provider table
	writeDevvmTOML(t, a, providersTOML)
	check(createSpec{Yes: true}, "", 0)
	check(createSpec{Backend: config.BackendSmol}, config.BackendSmol, 0)
	haveTTY = false
	check(createSpec{}, "", 0)
	haveTTY = true
	check(createSpec{}, config.BackendHetzner, 1)
	if len(asked[0]) != 1 || asked[0][0] != config.BackendHetzner {
		t.Errorf("offered %v", asked[0])
	}
}

// The placement flags are a provider's: a smol create refuses them, local
// or proxied, before anything runs.
func TestSmolCreateRefusesPlacement(t *testing.T) {
	a := newTestApp(t)
	smolLog := fakeLocalSmolvm(t, `[]`)
	sshLog := fakeSSH(t, "exit 0")
	for _, argv := range [][]string{
		{"create", "vm", "--yes", "--location", "fsn1"},
		{"create", "vm", "--backend", "smol", "--yes", "--server-type", "cx23"},
		{"create", "h/vm", "--yes", "--image", "debian-12"},
	} {
		if err := runTree(t, a, argv...); err == nil || !strings.Contains(err.Error(), "a smol VM has no placement") {
			t.Errorf("%v: %v", argv, err)
		}
	}
	for _, log := range []string{smolLog, sshLog} {
		if data, _ := os.ReadFile(log); strings.Contains(string(data), "create") {
			t.Errorf("ran:\n%s", data)
		}
	}
}
