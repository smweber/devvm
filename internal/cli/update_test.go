package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

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
	f.exe = filepath.Join(t.TempDir(), "devvm")
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
	if cycled := a.restartDaemons(); len(cycled) != 0 {
		t.Errorf("cycled %v with no daemon running", cycled)
	}
}
