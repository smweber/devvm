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
	fakeLocalSmolvm(t, `[{"name":"web","state":"running","created_at":1}]`)
	head := "# my boxes\n[templates.default]\nuser = \"ops\" # me\n"
	writeDevvmTOML(t, a, head)
	if err := runTree(t, a, "config", "add", "cloud", "--backend", "remote", "--host", "2.29.47.196", "--managed", "--transport", "mosh", "--connect-timeout", "20"); err != nil {
		t.Fatal(err)
	}
	if out := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "devvm setup cloud") {
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
	want := head + "\n[machines.cloud]\nbackend = \"remote\"\nmanaged = true\nhost = \"2.29.47.196\"\ntransport = \"mosh\"\nconnect_timeout = 20\n" +
		"\n[machines.web]\nbackend = \"remote\"\nmanaged = false\nhost = \"web.example\"\nuser = \"me\"\nmosh_server = \"/opt/mosh-server\"\nidentity = \"~/.ssh/k\"\n" +
		"\n[machines.h]\nbackend = \"hub\"\nhost = \"[::1]:2222\"\n"
	if string(data) != want {
		t.Fatalf("devvm.toml =\n%s\nwant\n%s", data, want)
	}
	cloud, err := config.Load(a.Paths, "cloud")
	if err != nil || cloud.SSHHost != "ops@2.29.47.196" || !cloud.Managed() || cloud.ConnectTimeout != 20 {
		t.Errorf("cloud = %+v, %v", cloud, err)
	}

	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"config", "add", "cloud", "--backend", "remote", "--host", "x"}, "already has [machines.cloud]"},
		{[]string{"config", "add", "x", "--backend", "smol", "--host", "x"}, "discovered from smolvm"},
		{[]string{"config", "add", "x", "--backend", "hub", "--host", "x", "--managed"}, "--managed applies only to a remote entry"},
		{[]string{"config", "add", "x", "--backend", "hub", "--host", "x", "--managed=false"}, "--managed applies only to a remote entry"},
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
// files are reaped; a managed marker (the shadowed VM's) is never touched.
func TestConfigRmReapsLocalState(t *testing.T) {
	a := newTestApp(t)
	log := fakeSSH(t, "exit 0")
	writeDevvmTOML(t, a, "# keep me\n[templates.default]\nuser = \"ops\"\n\n[machines.box]\nbackend = \"remote\"\nhost = \"b\"\n\n# the next one\n[machines.other]\nbackend = \"remote\"\nhost = \"o\"\n")
	if err := config.SavePorts(a.Paths, "box", []string{"3000"}); err != nil {
		t.Fatal(err)
	}
	reach.Record(a.Paths, "box", "attach")
	if err := config.WriteManagedMarker(a.Paths, "box", "7"); err != nil {
		t.Fatal(err)
	}
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
	if string(data) != "# keep me\n[templates.default]\nuser = \"ops\"\n\n# the next one\n[machines.other]\nbackend = \"remote\"\nhost = \"o\"\n" {
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
	if _, ok, _ := config.ReadManagedMarker(a.Paths, "box"); !ok {
		t.Error("config rm removed a managed marker (a smol VM's, never the entry's)")
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
	fakeLocalSmolvm(t, `[{"name":"web","state":"running","created_at":1}]`)
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
		"[machines.h]\nbackend = \"hub\"\nmanaged = true\nhost = \"h\"\n":            "managed applies only to remote",
		"[providers.x]\na = 1\n":                                                     "unknown top-level table",
		"[machines.x]\nbackend = \"remote\"\nhost = \"x\"\nbogus = 1\n":              "unknown key \"bogus\"",
		"[templates.t]\nhook = \"bogus\"\n":                                          "template t",
		"[templates.default]\nports = [\"x\"]\n":                                     "template default",
		"[machines.x]\nbackend = \"remote\"\n\n[machines.y]\nbackend = \"remote\"\n": "[machines.y]",
	} {
		writeDevvmTOML(t, a, body)
		if err := runTree(t, a, "config", "check"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
}

// `config show` prints the effective templates (fallbacks annotated) and
// every entry, a hub named like this host marked as such.
func TestConfigShow(t *testing.T) {
	a := newTestApp(t)
	host, err := os.Hostname()
	if err != nil || config.ValidName(host) != nil {
		t.Skip("hostname is not a valid machine name")
	}
	writeDevvmTOML(t, a, "[templates.default]\nhook = \"cmd:/d\"\nmemory = 4096\n\n[templates.big]\nmemory = 8192\n\n"+
		"[machines."+host+"]\nbackend = \"hub\"\nhost = \"h\"\n\n[machines.cloud]\nbackend = \"remote\"\nmanaged = true\nhost = \"c:2222\"\n")
	if err := runTree(t, a, "config", "show"); err != nil {
		t.Fatal(err)
	}
	out := a.Stdout.(*bytes.Buffer).String()
	for _, want := range []string{
		"[templates.default]\n",
		`user = "dev"`, "# built-in",
		"[templates.big]  # unset fields fall back to [templates.default]",
		"memory = 8192\n",
		`hook = "cmd:/d"`, "# from default",
		"[machines." + host + "]  # (this host)",
		"[machines.cloud]\n", "managed = true\n", `host = "c:2222"`,
		"from [templates.default]: the user setup creates",
		"connect_timeout = 10", "# default",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
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

// With the old per-machine layout and no devvm.toml, every command seeds
// what devvm.toml cannot carry (saved forwards, the managed markers of the
// smol VMs devvm made), says so in one line on stderr, and carries on with
// no entries. An empty machines/ (an older `status --watch` made one) is
// not the old layout.
func TestOldLayoutPointer(t *testing.T) {
	a := newTestApp(t)
	md := config.LegacyMachinesDir(a.Paths)
	if err := os.MkdirAll(md, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeLocalSmolvm(t, `[{"name":"web","state":"running","created_at":42}]`)
	if err := runTree(t, a, "config", "path"); err != nil {
		t.Fatal(err)
	}
	if w := a.Stderr.(*bytes.Buffer).String(); w != "" {
		t.Errorf("empty machines/: stderr = %q", w)
	}
	for n, body := range map[string]string{
		"cloud": "backend = \"remote-managed\"\nssh_host = \"dev@c\"\nports = [\"8080\"]\n",
		"web":   "backend = \"smol\"\nmemory = 1024\n",
	} {
		if err := os.WriteFile(filepath.Join(md, n+".toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := runTree(t, a, "status", "--plain", "--local"); err != nil {
		t.Fatal(err)
	}
	w := a.Stderr.(*bytes.Buffer).String()
	if strings.Count(w, "\n") != 1 || !strings.Contains(w, "machines/ is the old layout (saved forwards and managed markers were seeded from it); write ") ||
		!strings.Contains(w, "devvm.toml by hand (see docs/proposals/config.md §8) and remove machines/") {
		t.Errorf("stderr = %q", w)
	}
	// The old conf is not an entry; the VM it named stays managed.
	if out := a.Stdout.(*bytes.Buffer).String(); out != "web\tsmol\trunning\t-\tmanaged\t-\n" {
		t.Errorf("status = %q", out)
	}
	if got, _ := config.LoadPorts(a.Paths, "cloud"); strings.Join(got, ",") != "8080" {
		t.Errorf("forwards not seeded: %v", got)
	}
	// Once devvm.toml exists, the pointer stops.
	writeDevvmTOML(t, a, "")
	if err := runTree(t, a, "config", "path"); err != nil {
		t.Fatal(err)
	}
	if w := a.Stderr.(*bytes.Buffer).String(); w != "" {
		t.Errorf("stderr with devvm.toml = %q", w)
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
	out.next(t, "box\tremote\t?\t-\tunmanaged\tverified=-\n")
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
