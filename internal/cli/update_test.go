package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// A stamp that is not a release tag (a bare hash from `git describe
// --always` on a tagless clone, a dirty dev build) must not be compared or
// turned into a download URL.
func TestCurrentVersionRequiresATag(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	for stamp, want := range map[string]string{
		"v0.1.10":            "v0.1.10",
		"v0.1.10-3-gabc1234": "v0.1.10-3-gabc1234",
		"83f409a":            "dev",
		"83f409a-dirty":      "dev",
		"dev":                "dev",
		"0.1.10":             "dev",
	} {
		Version = stamp
		if got := currentVersion(); got != want {
			t.Errorf("Version=%q: currentVersion() = %q, want %q", stamp, got, want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v0.1.10", "v0.1.9", 1},
		{"v0.1.9", "v0.1.10", -1},
		{"v0.1.10", "v0.1.10", 0},
		{"0.1.10", "v0.1.10", 0},
		{"v1.0.0", "v0.9.9", 1},
		{"v1.0", "v1.0.0", 0},
		{"v0.1.10-3-gabcdef", "v0.1.10", 1}, // a build past the tag
		{"v0.1.10", "v0.1.10-dirty", -1},
		{"v0.1.11", "v0.1.10-3-gabcdef", 1},
		{"v0.1.10-10-gabcdef", "v0.1.10-9-gfedcba", 1}, // commit counts compare numerically
		{"v1.0.0-rc1", "v1.0.0", -1},                   // a prerelease sorts below its release
		{"v1.0.0-rc1", "v1.0.0-rc2", -1},
		{"v1.0.0-rc1", "v1.0.0-2-gabcdef", -1},
	} {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseSums(t *testing.T) {
	in := "abc123  devvm-darwin-arm64\nDEF456 *devvm-linux-amd64\n\nmalformed line here\n"
	got := parseSums(strings.NewReader(in))
	if got["devvm-darwin-arm64"] != "abc123" || got["devvm-linux-amd64"] != "def456" || len(got) != 2 {
		t.Errorf("parseSums = %v", got)
	}
}

// fakeRelease serves a releases tree the way GitHub does: /latest redirects to
// the tag page, assets live under /download/<tag>/<name>.
type fakeRelease struct {
	tag    string
	assets map[string][]byte
}

func (f *fakeRelease) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+f.tag, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/releases/download/")
		tag, name, _ := strings.Cut(rest, "/")
		data, ok := f.assets[name]
		if tag != f.tag || !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func testUpdater(s *httptest.Server) *updater {
	return &updater{base: s.URL + "/releases", client: s.Client(), stderr: io.Discard}
}

func TestLatestTag(t *testing.T) {
	s := (&fakeRelease{tag: "v0.2.0"}).server(t)
	tag, err := testUpdater(s).latestTag(context.Background())
	if err != nil || tag != "v0.2.0" {
		t.Fatalf("latestTag = %q, %v", tag, err)
	}
	// No redirect (e.g. an error page) must not be mistaken for a tag.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hi")) }))
	defer plain.Close()
	if _, err := testUpdater(plain).latestTag(context.Background()); err == nil {
		t.Fatal("expected an error without a redirect")
	}
}

func TestInstallBinaryReplacesAtomicallyKeepingMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "devvm")
	if err := os.WriteFile(target, []byte("old"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := installBinary([]byte("new"), target); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	info, _ := os.Stat(target)
	if string(data) != "new" || info.Mode().Perm() != 0o750 {
		t.Errorf("target = %q mode %v", data, info.Mode())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestInstallBinaryUnwritableDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "devvm")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	err := installBinary([]byte("new"), target)
	if err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("err = %v, want a not-writable message", err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "old" {
		t.Errorf("target changed: %q", data)
	}
}

// updateFixture stands up a release server and a fake installed binary, and
// stubs reexec to record the hand-off. Version is set so the run compares as
// an upgrade.
type updateFixture struct {
	app    *App
	u      *updater
	exe    string
	stdout bytes.Buffer
	exec   [][]string
}

func newUpdateFixture(t *testing.T, tag string, tamper bool) *updateFixture {
	t.Helper()
	bin := []byte("#!/bin/sh\necho new-binary\n")
	sumOf := sha256Hex(bin)
	if tamper {
		sumOf = strings.Repeat("0", 64)
	}
	asset := "devvm-" + runtime.GOOS + "-" + runtime.GOARCH
	s := (&fakeRelease{tag: tag, assets: map[string][]byte{
		asset:        bin,
		"SHA256SUMS": []byte(sumOf + "  " + asset + "\n"),
	}}).server(t)

	// The install target comes from executablePath, redirected to a temp file
	// so the test binary is never the thing replaced.
	f := &updateFixture{u: testUpdater(s)}
	f.app = &App{ConfigDir: t.TempDir(), Stdout: &f.stdout, Stderr: io.Discard}
	f.exe = filepath.Join(shortTempDir(t), "devvm")
	if err := os.WriteFile(f.exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExe := executablePath
	executablePath = func() (string, error) { return f.exe, nil }
	oldReexec := reexec
	reexec = func(argv0 string, argv, env []string) error {
		f.exec = append(f.exec, append([]string{argv0}, argv...))
		return nil
	}
	oldVersion := Version
	Version = "v0.1.0"
	t.Cleanup(func() { executablePath, reexec, Version = oldExe, oldReexec, oldVersion })
	return f
}

func TestRunUpdateInstallsVerifiesAndReexecs(t *testing.T) {
	f := newUpdateFixture(t, "v0.2.0", false)
	if err := f.app.runUpdate(context.Background(), f.u, updateOpts{}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f.exe)
	if !strings.Contains(string(data), "new-binary") {
		t.Errorf("binary not replaced: %q", data)
	}
	if len(f.exec) != 1 {
		t.Fatalf("reexec calls = %v", f.exec)
	}
	argv := f.exec[0]
	if argv[0] != f.exe || argv[1] != f.exe {
		t.Errorf("reexec target = %v", argv[:2])
	}
	want := []string{"--config-dir", f.app.ConfigDir, "update", "--finish-from", "v0.1.0"}
	if got := strings.Join(argv[2:], " "); got != strings.Join(want, " ") {
		t.Errorf("reexec args = %q, want %q", got, strings.Join(want, " "))
	}
}

func TestRunUpdateRefusesOnChecksumMismatch(t *testing.T) {
	f := newUpdateFixture(t, "v0.2.0", true)
	err := f.app.runUpdate(context.Background(), f.u, updateOpts{})
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	data, _ := os.ReadFile(f.exe)
	if string(data) != "old-binary" || len(f.exec) != 0 {
		t.Errorf("binary touched (%q) or reexec'd (%v) despite mismatch", data, f.exec)
	}
}

func TestRunUpdateAlreadyCurrentAndCheck(t *testing.T) {
	f := newUpdateFixture(t, "v0.1.0", false)
	if err := f.app.runUpdate(context.Background(), f.u, updateOpts{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.stdout.String(), "is current") || len(f.exec) != 0 {
		t.Errorf("stdout = %q, exec = %v", f.stdout.String(), f.exec)
	}
	f.stdout.Reset()
	if err := f.app.runUpdate(context.Background(), f.u, updateOpts{check: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.stdout.String(), "latest:  v0.1.0") {
		t.Errorf("check output = %q", f.stdout.String())
	}
	data, _ := os.ReadFile(f.exe)
	if string(data) != "old-binary" {
		t.Errorf("--check modified the binary: %q", data)
	}
}

func TestRunUpdateDevBuildNeedsForce(t *testing.T) {
	f := newUpdateFixture(t, "v0.2.0", false)
	Version = "dev"
	err := f.app.runUpdate(context.Background(), f.u, updateOpts{})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v", err)
	}
	if err := f.app.runUpdate(context.Background(), f.u, updateOpts{force: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.exec) != 1 {
		t.Errorf("expected a reexec after --force, got %v", f.exec)
	}
}

func TestRunUpdatePinnedVersionMissingAsset(t *testing.T) {
	f := newUpdateFixture(t, "v0.2.0", false)
	err := f.app.runUpdate(context.Background(), f.u, updateOpts{version: "v0.3.0"})
	if err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("err = %v", err)
	}
}

func TestRestartDaemonsSkipsMachinesWithoutDaemon(t *testing.T) {
	a := &App{ConfigDir: t.TempDir(), Stdout: io.Discard, Stderr: io.Discard}
	m := &config.Machine{Name: "box", Backend: config.BackendRemoteUnmanaged, SSHHost: "dev@example", Ports: []string{"8080:8080"}}
	if err := m.Save(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	if res := a.restartDaemons(); len(res.cycled)+len(res.skipped)+len(res.failed) != 0 {
		t.Errorf("touched %+v with no daemon running", res)
	}
}

func TestLatestTagRejectsUnsafeRedirect(t *testing.T) {
	for _, loc := range []string{"/releases/tag/..", "/releases/tag/v1%2F..%2Fx", "/releases/tag/main"} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", loc)
			w.WriteHeader(http.StatusFound)
		}))
		_, err := testUpdater(s).latestTag(context.Background())
		s.Close()
		if err == nil {
			t.Errorf("redirect to %q accepted", loc)
		}
	}
}

func TestRunUpdateRejectsUnsafeVersionFlag(t *testing.T) {
	f := newUpdateFixture(t, "v0.2.0", false)
	for _, v := range []string{"../../evil/repo/releases/download/v1", "v1/x", "latest"} {
		err := f.app.runUpdate(context.Background(), f.u, updateOpts{version: v})
		if err == nil || !strings.Contains(err.Error(), "invalid release tag") {
			t.Errorf("--version %q: err = %v", v, err)
		}
	}
	if len(f.exec) != 0 {
		t.Errorf("reexec'd after a rejected tag: %v", f.exec)
	}
}

func TestRunUpdateCheckPlain(t *testing.T) {
	f := newUpdateFixture(t, "v0.2.0", false)
	if err := f.app.runUpdate(context.Background(), f.u, updateOpts{check: true, plain: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.stdout.String(); got != "v0.1.0\tv0.2.0\ttrue\n" {
		t.Errorf("plain check = %q", got)
	}
	f.stdout.Reset()
	Version = "dev"
	if err := f.app.runUpdate(context.Background(), f.u, updateOpts{check: true, plain: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.stdout.String(); got != "dev\tv0.2.0\tfalse\n" {
		t.Errorf("plain check for a dev build = %q", got)
	}
}

func TestRunUpdateRefusesPackageManagerTree(t *testing.T) {
	f := newUpdateFixture(t, "v0.2.0", false)
	keg := filepath.Join(t.TempDir(), "Cellar", "devvm", "0.1.0", "bin")
	if err := os.MkdirAll(keg, 0o755); err != nil {
		t.Fatal(err)
	}
	f.exe = filepath.Join(keg, "devvm")
	if err := os.WriteFile(f.exe, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := f.app.runUpdate(context.Background(), f.u, updateOpts{})
	if err == nil || !strings.Contains(err.Error(), "Homebrew") {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(f.exe); string(data) != "old-binary" || len(f.exec) != 0 {
		t.Errorf("keg binary touched (%q) or reexec'd (%v)", data, f.exec)
	}
	if err := f.app.runUpdate(context.Background(), f.u, updateOpts{force: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.exec) != 1 {
		t.Errorf("expected --force to install, got %v", f.exec)
	}
}

func TestRunUpdateReplacesSymlinkTarget(t *testing.T) {
	f := newUpdateFixture(t, "v0.2.0", false)
	link := filepath.Join(t.TempDir(), "devvm")
	if err := os.Symlink(f.exe, link); err != nil {
		t.Fatal(err)
	}
	executablePath = func() (string, error) { return link, nil }
	if err := f.app.runUpdate(context.Background(), f.u, updateOpts{}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink was replaced by a file: %v %v", fi, err)
	}
	if data, _ := os.ReadFile(f.exe); !strings.Contains(string(data), "new-binary") {
		t.Errorf("target not replaced: %q", data)
	}
	if !strings.Contains(f.stdout.String(), "replacing the target") {
		t.Errorf("resolved path not reported: %q", f.stdout.String())
	}
	if len(f.exec) != 1 || f.exec[0][0] != f.exe {
		t.Errorf("reexec should target the resolved binary: %v", f.exec)
	}
}

// fakeDaemon answers the control protocol on a machine's socket the way a
// daemon in a given state would, so restartDaemons can be exercised without
// spawning anything. stop records whether it was told to stop; it never exits.
type fakeDaemon struct {
	stops int
}

func serveFakeDaemon(t *testing.T, configDir, name string, resp session.Response) *fakeDaemon {
	t.Helper()
	if err := config.EnsureRuntimeDir(configDir); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(config.RuntimeDir(configDir), name+".sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	fd := &fakeDaemon{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					return
				}
				var req session.Request
				_ = json.Unmarshal([]byte(line), &req)
				r := resp
				r.OK = true
				if req.Op == session.OpStop {
					fd.stops++
				}
				b, _ := json.Marshal(r)
				c.Write(append(b, '\n'))
			}(c)
		}
	}()
	return fd
}

func TestRestartDaemonsSkipsAndFails(t *testing.T) {
	old := daemonGoneTimeout
	daemonGoneTimeout = 200 * time.Millisecond
	t.Cleanup(func() { daemonGoneTimeout = old })
	var out bytes.Buffer
	a := &App{ConfigDir: shortTempDir(t), Stdout: &out, Stderr: io.Discard}
	for _, name := range []string{"reconn", "unconf", "stuck", "fresh"} {
		m := &config.Machine{Name: name, Backend: config.BackendRemoteUnmanaged, SSHHost: "dev@example", Ports: []string{"8080:8080"}}
		if err := m.Save(a.ConfigDir); err != nil {
			t.Fatal(err)
		}
	}
	fwd := []session.Forward{{Host: 8080, Guest: 8080}}
	reconn := serveFakeDaemon(t, a.ConfigDir, "reconn", session.Response{State: session.StateReconnecting, Forwards: fwd})
	unconf := serveFakeDaemon(t, a.ConfigDir, "unconf", session.Response{State: session.StateUp, Forwards: []session.Forward{{Host: 1455, Guest: 1455}}})
	stuck := serveFakeDaemon(t, a.ConfigDir, "stuck", session.Response{State: session.StateUp, Version: "v0.0.1", Forwards: fwd})
	fresh := serveFakeDaemon(t, a.ConfigDir, "fresh", session.Response{State: session.StateUp, Version: Version, Forwards: fwd})
	oldVersion := Version
	Version = "v9.9.9"
	t.Cleanup(func() { Version = oldVersion })
	fresh2 := serveFakeDaemon(t, a.ConfigDir, "fresh2", session.Response{State: session.StateUp, Version: "v9.9.9", Forwards: fwd})
	if err := (&config.Machine{Name: "fresh2", Backend: config.BackendRemoteUnmanaged, SSHHost: "dev@example", Ports: []string{"8080:8080"}}).Save(a.ConfigDir); err != nil {
		t.Fatal(err)
	}

	res := a.restartDaemons()
	if reconn.stops != 0 || unconf.stops != 0 || fresh2.stops != 0 {
		t.Errorf("stopped a daemon that should have been left alone: reconn=%d unconf=%d fresh2=%d", reconn.stops, unconf.stops, fresh2.stops)
	}
	// "fresh" was created before Version changed, so it reports the old build
	// and is cycled like "stuck"; neither ever exits.
	if stuck.stops != 1 || fresh.stops != 1 {
		t.Errorf("stops: stuck=%d fresh=%d, want 1 each", stuck.stops, fresh.stops)
	}
	if strings.Join(res.failed, ",") != "fresh,stuck" {
		t.Errorf("failed = %v, want the two daemons that never exited", res.failed)
	}
	if len(res.skipped) != 3 || len(res.cycled) != 0 {
		t.Errorf("skipped = %v cycled = %v", res.skipped, res.cycled)
	}
	for _, want := range []string{"reconn: forward daemon is reconnecting", "unconf: forward daemon holds no configured ports", "fresh2: forward daemon already runs v9.9.9"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	// And the finish step turns those failures into a non-zero exit.
	err := a.finishUpdate("v0.0.1")
	if err == nil || !strings.Contains(err.Error(), "fresh, stuck") {
		t.Errorf("finishUpdate err = %v", err)
	}
}

// TestFinishFromNeverDownloads runs the real command with --finish-from: the
// updater is never built, so no release server (or network) is involved.
func TestFinishFromNeverDownloads(t *testing.T) {
	var out bytes.Buffer
	a := &App{ConfigDir: shortTempDir(t), Stdout: &out, Stderr: io.Discard}
	root := a.rootCmd()
	root.SetArgs([]string{"--config-dir", a.ConfigDir, "update", "--finish-from", "v0.0.1"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "updated devvm v0.0.1 -> "+Version) {
		t.Errorf("output = %q", out.String())
	}
}

// shortTempDir is t.TempDir() moved under /tmp: macOS caps a unix socket path
// at 104 bytes and the default temp root (/var/folders/…/T/TestName…) blows
// it ("bind: invalid argument"). Resolved through EvalSymlinks because /tmp
// and /var are symlinks on macOS and tests compare paths.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "devvm-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir
}
