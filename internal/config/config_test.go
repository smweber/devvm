package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "web-1", "a.b_c", "X9"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-a", ".a", "a/b", "a b", "a@b", "../x"} {
		if ValidName(bad) == nil {
			t.Errorf("ValidName(%q) accepted", bad)
		}
	}
}

const sampleFile = `# ~/.config/devvm/devvm.toml, shared by every host.

[templates.default]              # what create/setup use
user = "ops"
hook = "none"

[machines.thinkpad]
backend = "hub"                  # backend is mandatory on every entry
host = "192.168.1.196"
user = "devvmtest"

[machines.cloud]
backend = "remote"
managed = true
host = "2.29.47.196:2222"        # user comes from the template
transport = "mosh"
connect_timeout = 25

[machines.scottdev3]
backend = "remote"
host = "scottdev3"
mosh_server = "/home/linuxbrew/.linuxbrew/bin/mosh-server"

[machines."v6.box"]
backend = "remote"
managed = false
host = "[2001:db8::1]:22"
user = "root"
`

// A valid file: both families, the two axes, host/user resolution.
func TestLoadFile(t *testing.T) {
	p := testPaths(t)
	writeFile(t, p, sampleFile)
	f, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.Names(), ","); got != "cloud,scottdev3,thinkpad,v6.box" {
		t.Errorf("names = %s", got)
	}
	for _, tc := range []struct {
		name, backend, sshHost string
		port                   int
		managed, remote, hub   bool
	}{
		// A managed entry with no user logs in as [templates.default]'s.
		{"cloud", BackendRemote, "ops@2.29.47.196", 2222, true, true, false},
		// An adopted entry with no user has no user@: ssh config decides.
		{"scottdev3", BackendRemote, "scottdev3", 0, false, true, false},
		{"thinkpad", BackendHub, "devvmtest@192.168.1.196", 0, false, true, true},
		{"v6.box", BackendRemote, "root@2001:db8::1", 22, false, true, false},
	} {
		m, ok := f.Entry(tc.name)
		if !ok {
			t.Fatalf("%s missing", tc.name)
		}
		if m.Backend != tc.backend || m.SSHHost != tc.sshHost || m.SSHPort != tc.port ||
			m.Managed() != tc.managed || m.IsRemote() != tc.remote || m.IsHub() != tc.hub {
			t.Errorf("%s = %+v (managed %v)", tc.name, m, m.Managed())
		}
	}
	cloud, _ := f.Entry("cloud")
	if cloud.User != "" || cloud.Transport != TransportMosh || cloud.ConnectTimeout != 25 {
		t.Errorf("cloud fields = %+v", cloud)
	}
	// Entry hands out copies.
	cloud.SSHHost = "changed"
	if again, _ := f.Entry("cloud"); again.SSHHost == "changed" {
		t.Error("Entry returned the file's own record")
	}
	// The package-level readers agree.
	if m, err := Load(p, "scottdev3"); err != nil || m.MoshServer == "" {
		t.Errorf("Load = %+v, %v", m, err)
	}
	if _, err := Load(p, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load(nope) = %v, want ErrNotFound", err)
	}
	if !Exists(p, "thinkpad") || Exists(p, "nope") {
		t.Error("Exists")
	}
}

// No file is valid: no entries, built-in templates.
func TestLoadFileMissing(t *testing.T) {
	p := testPaths(t)
	f, err := LoadFile(p)
	if err != nil || len(f.Entries) != 0 || len(f.Templates) != 0 {
		t.Fatalf("missing file = %+v, %v", f, err)
	}
	if names, err := List(p); err != nil || len(names) != 0 {
		t.Errorf("List = %v, %v", names, err)
	}
	if _, err := Load(p, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load = %v", err)
	}
}

// Every rejection names the table and what is wrong.
func TestLoadFileRejects(t *testing.T) {
	for body, want := range map[string]string{
		"[machines.vm]\nbackend = \"smol\"\n":                                        "discovered",
		"[machines.x]\nhost = \"h\"\n":                                               "backend is required",
		"[machines.x]\nbackend = \"remote-managed\"\nhost = \"h\"\n":                 "old layout",
		"[machines.x]\nbackend = \"ssh\"\nhost = \"h\"\n":                            "old layout",
		"[machines.x]\nbackend = \"hetzner\"\nhost = \"h\"\n":                        "discovered from the provider's API",
		"[machines.x]\nbackend = \"aws\"\nhost = \"h\"\n":                            "unsupported backend",
		"[machines.h]\nbackend = \"hub\"\nmanaged = false\nhost = \"h\"\n":           "managed applies only to remote",
		"[machines.x]\nbackend = \"remote\"\n":                                       "host is required",
		"[machines.x]\nbackend = \"remote\"\nhost = \"u@h\"\n":                       "put it in user",
		"[machines.x]\nbackend = \"remote\"\nhost = \"::1\"\n":                       "needs brackets",
		"[machines.x]\nbackend = \"remote\"\nhost = \"h:99999\"\n":                   "invalid port",
		"[machines.x]\nbackend = \"remote\"\nhost = \"h\"\nuser = \"a b\"\n":         "invalid user",
		"[machines.x]\nbackend = \"remote\"\nhost = \"h\"\ntransport = \"telnet\"\n": "invalid transport",
		"[machines.x]\nbackend = \"remote\"\nhost = \"h\"\nconnect_timeout = -1\n":   "connect_timeout",
		"[machines.x]\nbackend = \"remote\"\nhost = \"h\"\nssh_host = \"h\"\n":       "[machines.x]: unknown key \"ssh_host\"",
		"[machines.x]\nbackend = \"remote\"\nhost = \"h\"\nports = [\"3000\"]\n":     "[machines.x]: unknown key \"ports\"",
		"[providers.hetzner]\ntoken_env = \"X\"\n":                                   "[providers.hetzner]: unknown key \"token_env\"",
		"[providers.aws]\nlocation = \"x\"\n":                                        "[providers.aws]: unknown provider",
		"[providers.hetzner]\nssh_key = \"a\"\nssh_key_file = \"b\"\n":               "alternatives",
		"[providers.hetzner]\nlocation = \"a b\"\n":                                  "invalid location",
		"[providers.hetzner]\nipv4 = \"yes\"\n":                                      "parse",
		"[templates.x]\nserver_type = \"cx 23\"\n":                                   "invalid server_type",
		"[nope]\nx = 1\n": "unknown top-level table [nope]",
		"memory = 4096\n": "unknown top-level key",
		"[machines.\"a/b\"]\nbackend = \"remote\"\nhost = \"h\"\n": "invalid machine name",
		"[machines.x\n": "parse",
	} {
		p := testPaths(t)
		writeFile(t, p, body)
		if _, err := LoadFile(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
	// All problems at once, so `config check` names them in one run.
	p := testPaths(t)
	writeFile(t, p, "[machines.a]\nbackend = \"smol\"\n[machines.b]\nhost = \"h\"\n")
	if _, err := LoadFile(p); err == nil || !strings.Contains(err.Error(), "[machines.a]") || !strings.Contains(err.Error(), "[machines.b]") {
		t.Errorf("both errors: %v", err)
	}
}

func TestParseHost(t *testing.T) {
	for in, want := range map[string]struct {
		host string
		port int
	}{
		"box":               {"box", 0},
		"box.example:2222":  {"box.example", 2222},
		"10.0.0.1":          {"10.0.0.1", 0},
		"[::1]":             {"::1", 0},
		"[2001:db8::1]:443": {"2001:db8::1", 443},
	} {
		h, port, err := ParseHost(in)
		if err != nil || h != want.host || port != want.port {
			t.Errorf("ParseHost(%q) = %q, %d, %v", in, h, port, err)
		}
	}
	for _, bad := range []string{"", "::1", "[box]", "[::1", "[::1]x", "h:", ":22", "-oProxyCommand=x", "a b", "u@h", "h:0"} {
		if _, _, err := ParseHost(bad); err == nil {
			t.Errorf("ParseHost(%q) accepted", bad)
		}
	}
}

// The axes are methods on the record; a hub is never managed.
func TestManagedAndRemote(t *testing.T) {
	for _, tc := range []struct {
		m                   *Machine
		managed, remote, hb bool
	}{
		{NewRemote("a", true, "dev@a"), true, true, false},
		{NewRemote("b", false, "b"), false, true, false},
		{NewHub("h", "u@h"), false, true, true},
		{&Machine{Name: "vm", Backend: BackendSmol}, false, false, false},
	} {
		if tc.m.Managed() != tc.managed || tc.m.IsRemote() != tc.remote || tc.m.IsHub() != tc.hb {
			t.Errorf("%s: managed %v remote %v hub %v", tc.m.Name, tc.m.Managed(), tc.m.IsRemote(), tc.m.IsHub())
		}
	}
	vm := &Machine{Name: "vm", Backend: BackendSmol}
	vm.SetManaged(true)
	if !vm.Managed() {
		t.Error("a settled smol record reads unmanaged")
	}
	hub := NewHub("h", "u@h")
	hub.SetManaged(true)
	if hub.Managed() {
		t.Error("a hub reads managed")
	}
}

func TestSplitPort(t *testing.T) {
	for in, want := range map[string][2]string{"3000": {"3000", "3000"}, "8080:80": {"8080", "80"}} {
		if h, g := SplitPort(in); h != want[0] || g != want[1] {
			t.Errorf("SplitPort(%q) = %q,%q", in, h, g)
		}
	}
}

// The old layout with no devvm.toml is what the cli points at; once
// devvm.toml exists it is not.
func TestOldLayout(t *testing.T) {
	p := testPaths(t)
	if OldLayout(p) {
		t.Error("empty config dir reads as the old layout")
	}
	writeConf(t, p.Config, "box", "backend = \"smol\"\n")
	if !OldLayout(p) {
		t.Error("machines/ without devvm.toml is the old layout")
	}
	// The old files are never read as entries.
	if names, err := List(p); err != nil || len(names) != 0 {
		t.Errorf("List = %v, %v", names, err)
	}
	writeFile(t, p, "")
	if OldLayout(p) {
		t.Error("devvm.toml beside machines/ still reads as the old layout")
	}
	q := testPaths(t)
	if err := os.WriteFile(LegacyDefaultsPath(q), []byte("memory = 1024\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !OldLayout(q) {
		t.Error("config.toml alone is the old layout")
	}
}

// `config add` appends one table at the end: every other byte (comments,
// layout) is untouched, backend and managed are always written, and the
// result loads to the record it was rendered from.
func TestAppendEntry(t *testing.T) {
	p := testPaths(t)
	orig := sampleFile[:len(sampleFile)-1] // no trailing newline: one is added
	writeFile(t, p, orig)
	if err := AppendEntry(p, "new", Entry{Backend: BackendRemote, Host: "n.example:2200", User: "me", Transport: TransportMosh}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p.FilePath())
	want := orig + "\n\n[machines.new]\nbackend = \"remote\"\nmanaged = false\nhost = \"n.example:2200\"\nuser = \"me\"\ntransport = \"mosh\"\n"
	if string(data) != want {
		t.Fatalf("file =\n%s\nwant\n%s", data, want)
	}
	m, err := Load(p, "new")
	if err != nil || m.SSHHost != "me@n.example" || m.SSHPort != 2200 || m.Managed() {
		t.Errorf("new = %+v, %v", m, err)
	}
	// A dotted name is quoted, a hub has no managed line.
	if err := AppendEntry(p, "a.b", Entry{Backend: BackendHub, Host: "hb"}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(p.FilePath())
	if !strings.HasSuffix(string(data), "\n\n[machines.\"a.b\"]\nbackend = \"hub\"\nhost = \"hb\"\n") {
		t.Errorf("hub table:\n%s", data)
	}
	if m, err := Load(p, "a.b"); err != nil || !m.IsHub() {
		t.Errorf("a.b = %+v, %v", m, err)
	}

	// Refusals leave the file alone.
	before, _ := os.ReadFile(p.FilePath())
	for _, tc := range []struct {
		name string
		e    Entry
		want string
	}{
		{"cloud", Entry{Backend: BackendRemote, Host: "x"}, "already has"},
		{"bad", Entry{Backend: BackendRemote, Host: "u@x"}, "put it in user"},
		{"bad", Entry{Backend: BackendSmol, Host: "x"}, "discovered"},
		{"bad/name", Entry{Backend: BackendRemote, Host: "x"}, "invalid machine name"},
	} {
		if err := AppendEntry(p, tc.name, tc.e); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("AppendEntry(%s) = %v, want %q", tc.name, err, tc.want)
		}
	}
	if after, _ := os.ReadFile(p.FilePath()); string(after) != string(before) {
		t.Error("a refused add changed the file")
	}
	// A file that does not parse is never appended to.
	writeFile(t, p, "[machines.x\n")
	if err := AppendEntry(p, "y", Entry{Backend: BackendRemote, Host: "y"}); !errors.Is(err, ErrBrokenFile) {
		t.Errorf("broken file: %v", err)
	}
}

// A missing file (and dir) is created by the first add.
func TestAppendEntryCreatesFile(t *testing.T) {
	p := Paths{Config: filepath.Join(t.TempDir(), "devvm"), State: t.TempDir()}
	if err := AppendEntry(p, "h", Entry{Backend: BackendHub, Host: "h.example", User: "u"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p.FilePath())
	if string(data) != "[machines.h]\nbackend = \"hub\"\nhost = \"h.example\"\nuser = \"u\"\n" {
		t.Errorf("new file:\n%s", data)
	}
	// Round trip through EntryOf.
	m, _ := Load(p, "h")
	if e := EntryOf(m); e.Backend != BackendHub || e.Host != "h.example" || e.User != "u" || e.Managed != nil {
		t.Errorf("EntryOf = %+v", e)
	}
}

// `config rm` removes exactly one table: its keys, its own trailing
// comments and the blank gap; a comment block above the next table and the
// comment above its own header stay; every other byte is untouched.
func TestRemoveEntry(t *testing.T) {
	p := testPaths(t)
	body := `# head

[templates.default]
user = "ops"

# the cloud box
[machines.cloud]
backend = "remote"
# a comment inside
managed = true
host = "c"
# trailing, glued to it

# describes old, after a blank: kept

[machines.next]   # same-line comment
backend = "hub"
host = "n"

# about last
[machines.last]
backend = "remote"
host = "l"
`
	writeFile(t, p, body)
	if err := RemoveEntry(p, "cloud"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p.FilePath())
	want := `# head

[templates.default]
user = "ops"

# the cloud box
# describes old, after a blank: kept

[machines.next]   # same-line comment
backend = "hub"
host = "n"

# about last
[machines.last]
backend = "remote"
host = "l"
`
	if string(data) != want {
		t.Fatalf("after rm cloud:\n%s\nwant:\n%s", data, want)
	}
	// The next table is kept whole, and its comment block above too.
	if err := RemoveEntry(p, "next"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(p.FilePath())
	if !strings.Contains(string(data), "\n# about last\n[machines.last]\n") || strings.Contains(string(data), "machines.next") {
		t.Errorf("after rm next:\n%s", data)
	}
	// The last table: the file does not end in a blank line.
	if err := RemoveEntry(p, "last"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(p.FilePath())
	if !strings.HasSuffix(string(data), "# about last\n") {
		t.Errorf("after rm last:\n%q", data)
	}
	if f, err := LoadFile(p); err != nil || len(f.Entries) != 0 || *f.Templates["default"].User != "ops" {
		t.Errorf("result = %+v, %v", f, err)
	}
	if err := RemoveEntry(p, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rm ghost = %v", err)
	}
}

// A table only reachable through an odd form (an inline table) is not cut
// by a line scan; RemoveEntry refuses rather than guess.
func TestRemoveEntryRefusesOddForms(t *testing.T) {
	p := testPaths(t)
	body := "[machines]\nx = { backend = \"remote\", host = \"h\" }\n"
	writeFile(t, p, body)
	if err := RemoveEntry(p, "x"); err == nil || !strings.Contains(err.Error(), "config edit") {
		t.Errorf("inline table: %v", err)
	}
	if data, _ := os.ReadFile(p.FilePath()); string(data) != body {
		t.Error("a refused rm changed the file")
	}
	// A header-like line inside a multi-line string must not fool the scan
	// into cutting a neighbour: the post-parse check refuses.
	body = "[templates.default]\nhook = \"\"\"\n[machines.x]\n\"\"\"\n\n[machines.x]\nbackend = \"remote\"\nhost = \"h\"\n"
	writeFile(t, p, body)
	if err := RemoveEntry(p, "x"); err == nil {
		data, _ := os.ReadFile(p.FilePath())
		t.Errorf("multi-line string: removed, leaving\n%s", data)
	}
}

// A devvm.toml that is a symlink (into a dotfiles checkout) stays one: the
// edit lands in the link's target.
func TestEditThroughSymlink(t *testing.T) {
	p := testPaths(t)
	dots := t.TempDir()
	target := filepath.Join(dots, "devvm.toml")
	if err := os.WriteFile(target, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p.FilePath()); err != nil {
		t.Fatal(err)
	}
	if err := AppendEntry(p, "r", Entry{Backend: BackendRemote, Host: "r"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(p.FilePath()); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", fi, err)
	}
	if data, _ := os.ReadFile(target); !strings.HasPrefix(string(data), "# mine\n\n[machines.r]\n") {
		t.Errorf("target =\n%s", data)
	}
	if err := RemoveEntry(p, "r"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); string(data) != "# mine\n" {
		t.Errorf("target after rm = %q", data)
	}
}

// The atomic write keeps an existing file's mode.
func TestWriteKeepsMode(t *testing.T) {
	p := testPaths(t)
	writeFile(t, p, "")
	if err := os.Chmod(p.FilePath(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendEntry(p, "r", Entry{Backend: BackendRemote, Host: "r"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p.FilePath()); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(p.Config)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".devvm.toml") {
			t.Errorf("temp file left: %s", e.Name())
		}
	}
}

// A dangling devvm.toml symlink is an error naming the link and its target,
// for reads and edits alike: never an empty file, never a regular file
// renamed over the link.
func TestDanglingSymlink(t *testing.T) {
	p := testPaths(t)
	target := filepath.Join(t.TempDir(), "gone", "devvm.toml")
	if err := os.Symlink(target, p.FilePath()); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"LoadFile":    func() error { _, err := LoadFile(p); return err }(),
		"AppendEntry": AppendEntry(p, "x", Entry{Backend: BackendRemote, Host: "x"}),
		"RemoveEntry": RemoveEntry(p, "x"),
	} {
		if err == nil || !strings.Contains(err.Error(), "is a symlink to "+target) {
			t.Errorf("%s = %v", name, err)
		}
	}
	if fi, err := os.Lstat(p.FilePath()); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v", err)
	}
}

// A CRLF file gets a CRLF table.
func TestAppendEntryCRLF(t *testing.T) {
	p := testPaths(t)
	writeFile(t, p, "# mine\r\n[templates.default]\r\nuser = \"ops\"\r\n")
	if err := AppendEntry(p, "r", Entry{Backend: BackendRemote, Host: "r"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p.FilePath())
	want := "# mine\r\n[templates.default]\r\nuser = \"ops\"\r\n\r\n[machines.r]\r\nbackend = \"remote\"\r\nmanaged = false\r\nhost = \"r\"\r\n"
	if string(data) != want {
		t.Errorf("file = %q", data)
	}
	if err := RemoveEntry(p, "r"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(p.FilePath()); string(data) != "# mine\r\n[templates.default]\r\nuser = \"ops\"\r\n" {
		t.Errorf("after rm = %q", data)
	}
}

// A planned edit is not written when the file changed meanwhile.
func TestPlannedEditRefusesConcurrentChange(t *testing.T) {
	p := testPaths(t)
	writeFile(t, p, "[machines.r]\nbackend = \"remote\"\nhost = \"r\"\n")
	e, err := PlanRemoveEntry(p, "r")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, "[machines.r]\nbackend = \"remote\"\nhost = \"r2\"\n")
	if err := e.Commit(); err == nil || !strings.Contains(err.Error(), "changed while") {
		t.Errorf("Commit = %v", err)
	}
	if data, _ := os.ReadFile(p.FilePath()); !strings.Contains(string(data), "r2") {
		t.Error("the newer file was overwritten")
	}
}
