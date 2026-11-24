package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/reach"
	"github.com/smweber/devvm/internal/session"
)

func TestConfigPath(t *testing.T) {
	a := newTestApp(t)
	if err := runTree(t, a, "config", "path"); err != nil {
		t.Fatal(err)
	}
	if got := a.Stdout.(*bytes.Buffer).String(); got != a.Paths.FilePath()+"\n" {
		t.Errorf("path = %q", got)
	}
}

// `config add` appends one table, refuses what the loader would, and
// never dials anything.
func TestConfigAdd(t *testing.T) {
	a := newTestApp(t)
	log := fakeSSH(t, "exit 0")
	fakeLocalSmolvm(t, `[{"name":"web","state":"running",`+devvmLabel+`}]`)
	head := "# my boxes\n[templates.default]\nhook = \"none\" # me\n"
	writeDevvmTOML(t, a, head)
	if err := runTree(t, a, "config", "add", "cloud", "--backend", "remote", "--host", "2.29.47.196", "--transport", "mosh", "--connect-timeout", "20"); err != nil {
		t.Fatal(err)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "devvm never modifies it. Next: 'devvm attach cloud'.") {
		t.Errorf("next step = %q", out)
	}
	if err := runTree(t, a, "config", "add", "web", "--backend", "remote", "--host", "web.example", "--user", "me", "--mosh-server", "/opt/mosh-server", "--identity", "~/.ssh/k"); err != nil {
		t.Fatal(err)
	}
	if w := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(w, "shadows the local smol VM 'web'") {
		t.Errorf("no collision warning: %q", w)
	}
	if err := runTree(t, a, "config", "add", "h", "-b", "hub", "--host", "[::1]:2222"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(a.Paths.FilePath())
	want := head + "\n[machines.cloud]\nbackend = \"remote\"\nhost = \"2.29.47.196\"\ntransport = \"mosh\"\nconnect_timeout = 20\n" +
		"\n[machines.web]\nbackend = \"remote\"\nhost = \"web.example\"\nuser = \"me\"\nmosh_server = \"/opt/mosh-server\"\nidentity = \"~/.ssh/k\"\n" +
		"\n[machines.h]\nbackend = \"hub\"\nhost = \"[::1]:2222\"\n"
	if string(data) != want {
		t.Fatalf("devvm.toml =\n%s\nwant\n%s", data, want)
	}
	cloud, err := config.Load(a.Paths, "cloud")
	if err != nil || cloud.SSHHost != "2.29.47.196" || cloud.Managed() || cloud.ConnectTimeout != 20 {
		t.Errorf("cloud = %+v, %v", cloud, err)
	}

	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"config", "add", "cloud", "--backend", "remote", "--host", "x"}, "already has [machines.cloud]"},
		{[]string{"config", "add", "x", "--backend", "smol", "--host", "x"}, "discovered from smolvm"},
		{[]string{"config", "add", "x", "--backend", "remote", "--host", "x", "--managed"}, "unknown flag: --managed"},
		{[]string{"config", "add", "x", "--host", "x"}, "needs --backend"},
		{[]string{"config", "add", "x", "--backend", "remote", "--host", "u@x"}, "put it in user"},
		{[]string{"config", "add", "x", "--backend", "remote", "--host", "x", "--transport", "telnet"}, "invalid transport"},
		{[]string{"config", "add", "a/b", "--backend", "remote", "--host", "x"}, "lives in the hub's devvm.toml"},
		{[]string{"config", "add", "a b", "--backend", "remote", "--host", "x"}, "invalid machine name"},
	} {
		if err := runTree(t, a, tc.argv...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.argv, err, tc.want)
		}
	}
	if after, _ := os.ReadFile(a.Paths.FilePath()); string(after) != want {
		t.Errorf("a refused add changed the file:\n%s", after)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Error("config add dialed a box")
	}
}

// `config rm` removes exactly one table and owns this host's state for it:
// the daemon is stopped, saved forwards and the reach cache go, the run
// files are reaped.
func TestConfigRmReapsLocalState(t *testing.T) {
	a := newTestApp(t)
	log := fakeSSH(t, "exit 0")
	writeDevvmTOML(t, a, "# keep me\n[templates.default]\nhook = \"none\"\n\n[machines.box]\nbackend = \"remote\"\nhost = \"b\"\n\n# the next one\n[machines.other]\nbackend = \"remote\"\nhost = \"o\"\n")
	if err := config.SavePorts(a.Paths, "box", []string{"3000"}); err != nil {
		t.Fatal(err)
	}
	reach.Record(a.Paths, "box", "attach")
	// A daemon holding live forwards: refused, nothing changed.
	od := serveOwnerDaemon(t, a.Paths, "box", map[string]session.Response{
		session.OpList: {OK: true, State: session.StateUp, Forwards: []session.Forward{{Host: 3000, Guest: 3000, Owners: []string{session.OwnerConf}}}},
	}, true)
	before, _ := os.ReadFile(a.Paths.FilePath())
	err := runTree(t, a, "config", "rm", "box")
	if err == nil || !strings.Contains(err.Error(), "live forwards for box (1 forwards)") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("rm with live forwards = %v", err)
	}
	if after, _ := os.ReadFile(a.Paths.FilePath()); !bytes.Equal(before, after) {
		t.Error("a refused rm changed the file")
	}
	if len(od.requests(session.OpStop)) != 0 {
		t.Error("a refused rm stopped the daemon")
	}
	// --force stops it and removes everything this host kept.
	if err := runTree(t, a, "config", "rm", "--force", "box"); err != nil {
		t.Fatalf("config rm --force: %v\n%s", err, a.Stderr)
	}
	if len(od.requests(session.OpStop)) != 1 {
		t.Error("the daemon was not stopped")
	}
	data, _ := os.ReadFile(a.Paths.FilePath())
	if string(data) != "# keep me\n[templates.default]\nhook = \"none\"\n\n# the next one\n[machines.other]\nbackend = \"remote\"\nhost = \"o\"\n" {
		t.Errorf("devvm.toml after rm =\n%s", data)
	}
	if _, err := os.Stat(a.Paths.PortsPath("box")); !os.IsNotExist(err) {
		t.Errorf("ports file left: %v", err)
	}
	if e := reach.Read(a.Paths, "box"); e.State != reach.Unknown {
		t.Errorf("reach cache left: %+v", e)
	}
	if _, err := os.Stat(filepath.Join(a.Paths.RuntimeDir(), "box.sock")); !os.IsNotExist(err) {
		t.Errorf("daemon socket left: %v", err)
	}
	if err := runTree(t, a, "config", "rm", "box"); err == nil || !strings.Contains(err.Error(), "no such entry") {
		t.Errorf("rm of a missing entry = %v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Error("config rm dialed a box")
	}
}

// For a hub, `config rm` reaps every HUB@* daemon and saved-forwards file
// and the listing cache; another hub's are untouched. Live forwards on any
// of its machines refuse it without --force.
func TestConfigRmHub(t *testing.T) {
	a := newTestApp(t)
	fakeSSH(t, "exit 1")
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}, "api": {"80"}})
	writeHub(t, a, "other", map[string][]string{"web": {"1"}})
	if err := writeHubCache(a.Paths, "h", []hubRow{{"web", "smol", "running", "-", "-", ""}}); err != nil {
		t.Fatal(err)
	}
	od := serveOwnerDaemon(t, a.Paths, "h/web", map[string]session.Response{
		session.OpList: {OK: true, State: session.StateUp, Forwards: []session.Forward{{Host: 3000, Guest: 3000, Owners: []string{session.OwnerConf}}}},
	}, true)
	if err := runTree(t, a, "config", "rm", "h"); err == nil || !strings.Contains(err.Error(), "live forwards for h/web (1 forwards)") {
		t.Fatalf("rm hub with a live daemon = %v", err)
	}
	if !config.Exists(a.Paths, "h") {
		t.Fatal("refused rm removed the entry")
	}
	if err := runTree(t, a, "config", "rm", "h", "--force"); err != nil {
		t.Fatalf("config rm --force h: %v\n%s", err, a.Stderr)
	}
	if len(od.requests(session.OpStop)) != 1 {
		t.Error("h/web's daemon was not stopped")
	}
	if config.Exists(a.Paths, "h") || !config.Exists(a.Paths, "other") {
		t.Error("wrong entries removed")
	}
	if got, _ := config.SavedPortsNames(a.Paths); strings.Join(got, ",") != "other/web" {
		t.Errorf("saved forwards after rm h = %v", got)
	}
	if got := readHubCache(a.Paths, "h"); got != nil {
		t.Errorf("hub cache left: %v", got)
	}
	if _, err := os.Stat(filepath.Join(a.Paths.RuntimeDir(), "h@web.sock")); !os.IsNotExist(err) {
		t.Errorf("h@web socket left: %v", err)
	}
}

// `config check`: zero on a valid (or missing) file, non-zero naming every
// problem otherwise, including what only the cli checks (hook specs, port
// mappings); a collision with a local smol VM is a warning.
func TestConfigCheck(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[{"name":"web","state":"running",`+devvmLabel+`}]`)
	if err := runTree(t, a, "config", "check"); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "missing, which is valid") {
		t.Errorf("out = %q", out)
	}
	writeDevvmTOML(t, a, "[templates.default]\nhook = \"none\"\n\n[machines.web]\nbackend = \"remote\"\nhost = \"w\"\n")
	if err := runTree(t, a, "config", "check"); err != nil {
		t.Fatalf("valid file: %v", err)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "ok (1 machine entries, 1 templates)") {
		t.Errorf("out = %q", out)
	}
	if w := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(w, "warning: [machines.web] shadows the local smol VM 'web'") {
		t.Errorf("collision warning = %q", w)
	}
	for body, want := range map[string]string{
		"[machines.vm]\nbackend = \"smol\"\n":                                        "discovered",
		"[machines.h]\nbackend = \"hub\"\nmanaged = true\nhost = \"h\"\n":            "managed is gone",
		"[machines.r]\nbackend = \"remote\"\nmanaged = true\nhost = \"h\"\n":         "[machines.r]: managed is gone (devvm never shapes an entry); delete the line, and add agent = true for the browser bridge",
		"[machines.h]\nbackend = \"hub\"\nagent = true\nhost = \"h\"\n":              "agent applies only to remote",
		"[providers.x]\na = 1\n":                                                     "unknown provider",
		"[machines.x]\nbackend = \"remote\"\nhost = \"x\"\nbogus = 1\n":              "unknown key \"bogus\"",
		"[templates.t]\nhook = \"bogus\"\n":                                          "template t",
		"[templates.default]\nports = [\"x\"]\n":                                     "ports is gone from templates; delete the line and save each forward with 'devvm ports add NAME PORT'",
		"[templates.default]\nuser = \"ops\"\n":                                      "[templates.default]: user is gone from templates (a box devvm makes always has the user dev); delete the line",
		"[machines.x]\nbackend = \"remote\"\n\n[machines.y]\nbackend = \"remote\"\n": "[machines.y]",
	} {
		writeDevvmTOML(t, a, body)
		if err := runTree(t, a, "config", "check"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
}

// `config show` prints the effective templates (fallbacks annotated) and
// every entry, a hub named like this host marked as such. The hostname is
// faked: a real one may be an FQDN (macOS reports NAME.local), which the
// fixture would otherwise have to quote.
func TestConfigShow(t *testing.T) {
	a := newTestApp(t)
	fakeHostname(t, "mac.local")
	writeDevvmTOML(t, a, "[templates.default]\nhook = \"cmd:/d\"\nmemory = 4096\nshare_auth = [\"github\"]\n\n[templates.big]\nmemory = 8192\n\n"+
		"[machines.mac]\nbackend = \"hub\"\nhost = \"h\"\n\n[machines.cloud]\nbackend = \"remote\"\nhost = \"c:2222\"\n"+
		"\n[machines.box]\nbackend = \"remote\"\nagent = true\nhost = \"b\"\n")
	if err := runTree(t, a, "config", "show"); err != nil {
		t.Fatal(err)
	}
	out := a.Stdout.(*bytes.Buffer).String()
	for _, want := range []string{
		"[templates.default]\n",
		"harden = true", "fail2ban = false",
		"[templates.big]  # unset fields fall back to [templates.default]",
		"memory = 8192\n",
		`hook = "cmd:/d"`, "# from default",
		"share_auth = [\"github\"]\n", "share_auth = [\"github\"]                  # from default\n",
		"[machines.mac]  # (this host)",
		"[machines.cloud]\nbackend = \"remote\"\nagent = false                            # default\n", `host = "c:2222"`,
		"# user: unset, ~/.ssh/config decides",
		"connect_timeout = 10", "# default",
		"[machines.box]\nbackend = \"remote\"\nagent = true\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	// harden's built-in is on, and annotated as the built-in.
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "harden = ") && (!strings.HasPrefix(l, "harden = true ") || !strings.HasSuffix(l, "# built-in")) {
			t.Errorf("harden line = %q, want true # built-in", l)
		}
	}
}

// fakeHostname poses as a host named name for isThisHost.
func fakeHostname(t *testing.T, name string) {
	t.Helper()
	orig := hostname
	hostname = func() (string, error) { return name, nil }
	t.Cleanup(func() { hostname = orig })
}

// The (this host) hint matches the full hostname or its short form, and a
// dotted entry name (what `config add` writes for an FQDN) is shown quoted,
// so the output still reads back as the one table it is.
func TestConfigShowThisHost(t *testing.T) {
	for _, tc := range []struct {
		host, entry, header string
		hint                bool
	}{
		{"mac.local", "mac", "[machines.mac]", true},
		{"mac.local", "mac.local", `[machines."mac.local"]`, true},
		{"box", "box", "[machines.box]", true},
		{"box.example.com", "box.example", `[machines."box.example"]`, false},
		{"macbook.local", "mac", "[machines.mac]", false},
	} {
		a := newTestApp(t)
		fakeHostname(t, tc.host)
		if err := runTree(t, a, "config", "add", tc.entry, "--backend", "hub", "--host", "h"); err != nil {
			t.Fatalf("%s: add: %v", tc.entry, err)
		}
		data, err := os.ReadFile(a.Paths.FilePath())
		if err != nil || !strings.Contains(string(data), tc.header+"\n") {
			t.Errorf("%s: added %q, %v; want header %s", tc.entry, data, err, tc.header)
		}
		a.Stdout.(*bytes.Buffer).Reset()
		if err := runTree(t, a, "config", "show"); err != nil {
			t.Fatalf("%s: show: %v", tc.entry, err)
		}
		out := a.Stdout.(*bytes.Buffer).String()
		if !strings.Contains(out, tc.header) {
			t.Errorf("%s: show lacks %s:\n%s", tc.entry, tc.header, out)
		}
		if got := strings.Contains(out, tc.header+"  # (this host)"); got != tc.hint {
			t.Errorf("host %s, entry %s: hint = %v, want %v:\n%s", tc.host, tc.entry, got, tc.hint, out)
		}
		if err := runTree(t, a, "config", "rm", tc.entry); err != nil {
			t.Errorf("%s: rm: %v", tc.entry, err)
		}
	}
}

// `config edit` runs $EDITOR, then checks; a failing check keeps the edit
// and saves the version before it as devvm.toml.bak. No terminal, no edit.
func TestConfigEdit(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	orig := "[machines.box]\nbackend = \"remote\"\nhost = \"b\"\n"
	writeDevvmTOML(t, a, orig)
	pinTTYs(t, false, false)
	if err := runTree(t, a, "config", "edit"); err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Errorf("no tty: %v", err)
	}
	pinTTYs(t, true, true)
	editor := func(content string) {
		dir := t.TempDir()
		src := filepath.Join(dir, "content")
		if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(dir, "ed")
		if err := os.WriteFile(script, []byte("#!/bin/sh\ncat "+src+" > \"$1\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("VISUAL", "")
		t.Setenv("EDITOR", script)
	}
	// A broken edit: kept, with the previous version in .bak.
	editor("[machines.box]\nbackend = \"smol\"\n")
	err := runTree(t, a, "config", "edit")
	if err == nil || !strings.Contains(err.Error(), "devvm.toml.bak") {
		t.Fatalf("broken edit = %v", err)
	}
	if bak, _ := os.ReadFile(a.Paths.FilePath() + ".bak"); string(bak) != orig {
		t.Errorf(".bak = %q", bak)
	}
	if cur, _ := os.ReadFile(a.Paths.FilePath()); !strings.Contains(string(cur), "smol") {
		t.Errorf("the edit was not kept: %q", cur)
	}
	// A good edit checks clean.
	os.Remove(a.Paths.FilePath() + ".bak")
	editor(orig + "\n[machines.h]\nbackend = \"hub\"\nhost = \"h\"\n")
	if err := runTree(t, a, "config", "edit"); err != nil {
		t.Fatalf("good edit: %v", err)
	}
	if _, err := os.Stat(a.Paths.FilePath() + ".bak"); !os.IsNotExist(err) {
		t.Error("a good edit left a .bak")
	}
	if !config.Exists(a.Paths, "h") {
		t.Error("edit not saved")
	}
}

// status --watch sees devvm.toml renamed into place (config add, config rm)
// end to end, through the real snapshot.
func TestStatusWatchSeesConfigAdd(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	fakeSSH(t, "exit 1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := newBlockWriter()
	done := make(chan error, 1)
	snapshot := func() (string, error) { return a.plainSnapshot(statusScope{local: true}) }
	go func() { done <- watchStatus(ctx, a.Paths, snapshot, nil, out, 50*time.Millisecond, nil, nil) }()
	out.next(t, "")
	if err := config.AppendEntry(a.Paths, "box", config.Entry{Backend: config.BackendRemote, Host: "b"}); err != nil {
		t.Fatal(err)
	}
	out.next(t, "box\tremote\t?\t-\t-\tverified=-\n")
	if err := config.RemoveEntry(a.Paths, "box"); err != nil {
		t.Fatal(err)
	}
	out.next(t, "")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop")
	}
}

// A layout the line cut cannot remove is refused before any daemon is
// touched: the plan runs first, the reap only once the edit is sure.
func TestConfigRmRefusedLayoutKeepsDaemon(t *testing.T) {
	a := newTestApp(t)
	body := "[machines]\nbox = { backend = \"remote\", host = \"b\" }\n"
	writeDevvmTOML(t, a, body)
	od := serveOwnerDaemon(t, a.Paths, "box", map[string]session.Response{
		session.OpList: {OK: true, State: session.StateUp, Forwards: []session.Forward{{Host: 3000, Guest: 3000, Owners: []string{session.OwnerConf}}}},
	}, true)
	err := runTree(t, a, "config", "rm", "--force", "box")
	if err == nil || !strings.Contains(err.Error(), "config edit") {
		t.Fatalf("rm of an inline table = %v", err)
	}
	if n := len(od.requests(session.OpStop)); n != 0 {
		t.Errorf("a refused rm stopped the daemon (%d stops)", n)
	}
	if data, _ := os.ReadFile(a.Paths.FilePath()); string(data) != body {
		t.Errorf("file changed: %q", data)
	}
}

// Once the entry is gone, a failure clearing one piece of state does not
// stop the rest; the error says the entry was removed and what failed.
func TestConfigRmCarriesOnPastFailures(t *testing.T) {
	a := newTestApp(t)
	writeDevvmTOML(t, a, "[machines.box]\nbackend = \"remote\"\nhost = \"b\"\n")
	if err := config.SavePorts(a.Paths, "box", []string{"3000"}); err != nil {
		t.Fatal(err)
	}
	reach.Record(a.Paths, "box", "attach")
	if err := os.Chmod(a.Paths.PortsDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(a.Paths.PortsDir(), 0o700) })
	if os.WriteFile(filepath.Join(a.Paths.PortsDir(), "probe"), nil, 0o600) == nil {
		t.Skip("running as a user that can write a read-only dir (root?)")
	}
	err := runTree(t, a, "config", "rm", "box")
	if err == nil || !strings.HasPrefix(err.Error(), "removed [machines.box]; but: box: clear saved forwards") {
		t.Fatalf("err = %v", err)
	}
	if config.Exists(a.Paths, "box") {
		t.Error("entry kept")
	}
	if e := reach.Read(a.Paths, "box"); e.State != reach.Unknown {
		t.Errorf("reach cache left after an earlier failure: %+v", e)
	}
}

// Through a devvm.toml symlink the .bak lands beside the real file (in the
// dotfiles checkout), with that file's mode, and the link stays a link.
func TestConfigEditBakBesideSymlinkTarget(t *testing.T) {
	a := newTestApp(t)
	fakeLocalSmolvm(t, `[]`)
	dots := t.TempDir()
	real := filepath.Join(dots, "devvm.toml")
	orig := "[machines.box]\nbackend = \"remote\"\nhost = \"b\"\n"
	if err := os.WriteFile(real, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(a.Paths.Config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, a.Paths.FilePath()); err != nil {
		t.Fatal(err)
	}
	pinTTYs(t, true, true)
	script := filepath.Join(t.TempDir(), "ed")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '[machines.box\\n' > \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", script)
	if err := runTree(t, a, "config", "edit"); err == nil || !strings.Contains(err.Error(), real+".bak") {
		t.Fatalf("err = %v", err)
	}
	fi, err := os.Stat(real + ".bak")
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf(".bak = %v, %v", fi, err)
	}
	if data, _ := os.ReadFile(real + ".bak"); string(data) != orig {
		t.Errorf(".bak = %q", data)
	}
	if fi, err := os.Lstat(a.Paths.FilePath()); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was replaced")
	}
}
