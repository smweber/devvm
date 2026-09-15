package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeMac stands in for the macOS tools the installer shells out to. Its
// "ditto" writes a bundle whose Info.plist holds the zip's payload as the
// version, so PlistBuddy can read it back like the real one would.
type fakeMac struct {
	running bool
	calls   []string
	appsDir string
	onOpen  func() // observe relaunch ordering
}

func (f *fakeMac) run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	switch name {
	case "pgrep":
		if f.running {
			return "123", nil
		}
		return "", fmt.Errorf("pgrep: exit status 1")
	case "osascript":
		f.running = false
		return "", nil
	case "pkill":
		f.running = false
		return "", nil
	case "open":
		f.running = true
		if f.onOpen != nil {
			f.onOpen()
		}
		return "", nil
	case "/usr/libexec/PlistBuddy":
		data, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	case "ditto":
		zip, err := os.ReadFile(args[2])
		if err != nil {
			return "", err
		}
		contents := filepath.Join(args[3], menubarAppName+".app", "Contents")
		if err := os.MkdirAll(contents, 0o755); err != nil {
			return "", err
		}
		return "", os.WriteFile(filepath.Join(contents, "Info.plist"), zip, 0o644)
	}
	return "", fmt.Errorf("unexpected tool %s", name)
}

func (f *fakeMac) installed(t *testing.T, version string) {
	t.Helper()
	contents := filepath.Join(f.appsDir, menubarAppName+".app", "Contents")
	if err := os.MkdirAll(contents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeMac) calledWith(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

type menubarFixture struct {
	app    *App
	u      *updater
	mac    *fakeMac
	stdout bytes.Buffer
	stderr bytes.Buffer
}

// newMenubarFixture serves a release whose app zip "contains" tag (see
// fakeMac.run) and points the installer at a temp ~/Applications.
func newMenubarFixture(t *testing.T, tag string, tamper bool) *menubarFixture {
	t.Helper()
	zip := []byte(tag + "\n")
	sum := sha256Hex(zip)
	if tamper {
		sum = strings.Repeat("0", 64)
	}
	s := (&fakeRelease{tag: tag, assets: map[string][]byte{
		menubarAsset:             zip,
		menubarAsset + ".sha256": []byte(sum + "  " + menubarAsset + "\n"),
	}}).server(t)

	f := &menubarFixture{u: testUpdater(s), mac: &fakeMac{appsDir: filepath.Join(t.TempDir(), "Applications")}}
	f.app = &App{ConfigDir: t.TempDir(), Stdout: &f.stdout, Stderr: &f.stderr}

	oldGOOS, oldDir, oldRun, oldQuit, oldVersion, oldUpdater := menubarGOOS, menubarAppsDir, menubarRun, menubarQuitTimeout, Version, newMenubarUpdater
	menubarGOOS = "darwin"
	newMenubarUpdater = func(io.Writer) *updater { return f.u }
	menubarAppsDir = func() (string, error) { return f.mac.appsDir, nil }
	menubarRun = f.mac.run
	menubarQuitTimeout = 50 * time.Millisecond
	Version = "v0.1.0"
	t.Cleanup(func() {
		menubarGOOS, menubarAppsDir, menubarRun, menubarQuitTimeout, Version, newMenubarUpdater = oldGOOS, oldDir, oldRun, oldQuit, oldVersion, oldUpdater
	})
	return f
}

func (f *menubarFixture) installedVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.mac.appsDir, menubarAppName+".app", "Contents", "Info.plist"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func TestMenubarRefusesOffMacOS(t *testing.T) {
	f := newMenubarFixture(t, "v0.1.0", false)
	menubarGOOS = "linux"
	err := f.app.runMenubar(context.Background(), f.u, "", false)
	if err == nil || !strings.Contains(err.Error(), "macOS-only") {
		t.Fatalf("err = %v", err)
	}
}

func TestMenubarDevBuildNeedsVersion(t *testing.T) {
	f := newMenubarFixture(t, "v0.1.0", false)
	Version = "dev"
	err := f.app.runMenubar(context.Background(), f.u, "", false)
	if err == nil || !strings.Contains(err.Error(), "--version") {
		t.Fatalf("err = %v", err)
	}
	if got := f.installedVersion(t); got != "" {
		t.Errorf("installed %q, want nothing", got)
	}
	if err := f.app.runMenubar(context.Background(), f.u, "../evil", false); err == nil || !strings.Contains(err.Error(), "invalid release tag") {
		t.Fatalf("unsafe tag err = %v", err)
	}
}

func TestMenubarInstallsVerifiesAndOpens(t *testing.T) {
	f := newMenubarFixture(t, "v0.1.0", false)
	if err := f.app.runMenubar(context.Background(), f.u, "", false); err != nil {
		t.Fatal(err)
	}
	if got := f.installedVersion(t); got != "v0.1.0" {
		t.Errorf("installed %q, want v0.1.0", got)
	}
	if !f.mac.calledWith("ditto -x -k") || !f.mac.calledWith("open -a "+filepath.Join(f.mac.appsDir, "DevVM.app")) {
		t.Errorf("calls = %v", f.mac.calls)
	}
	if f.mac.calledWith("osascript") || f.mac.calledWith("pkill") {
		t.Errorf("quit a non-running app: %v", f.mac.calls)
	}
	if entries, _ := os.ReadDir(f.mac.appsDir); len(entries) != 1 {
		t.Errorf("staging left behind: %v", entries)
	}
	if !strings.Contains(f.stdout.String(), "installed DevVM.app v0.1.0") || !strings.Contains(f.stdout.String(), "opened DevVM.app") {
		t.Errorf("stdout = %q", f.stdout.String())
	}
}

func TestMenubarCurrentVersionJustOpens(t *testing.T) {
	f := newMenubarFixture(t, "v0.1.0", false)
	f.mac.installed(t, "v0.1.0")
	if err := f.app.runMenubar(context.Background(), f.u, "", false); err != nil {
		t.Fatal(err)
	}
	if f.mac.calledWith("ditto") {
		t.Errorf("re-downloaded a current app: %v", f.mac.calls)
	}
	if !f.mac.calledWith("open -a") || !strings.Contains(f.stdout.String(), "already installed") {
		t.Errorf("calls = %v, stdout = %q", f.mac.calls, f.stdout.String())
	}

	// --force reinstalls anyway.
	f.mac.calls = nil
	if err := f.app.runMenubar(context.Background(), f.u, "", true); err != nil {
		t.Fatal(err)
	}
	if !f.mac.calledWith("ditto") {
		t.Errorf("--force did not reinstall: %v", f.mac.calls)
	}
}

func TestMenubarReplacesRunningApp(t *testing.T) {
	f := newMenubarFixture(t, "v0.2.0", false)
	f.mac.installed(t, "v0.1.0")
	f.mac.running = true
	if err := f.app.runMenubar(context.Background(), f.u, "v0.2.0", false); err != nil {
		t.Fatal(err)
	}
	if got := f.installedVersion(t); got != "v0.2.0" {
		t.Errorf("installed %q, want v0.2.0", got)
	}
	// Quit politely before the swap, then reopened.
	var quitAt, dittoAt, openAt int
	for i, c := range f.mac.calls {
		switch {
		case strings.HasPrefix(c, "osascript"):
			quitAt = i
		case strings.HasPrefix(c, "ditto"):
			dittoAt = i
		case strings.HasPrefix(c, "open"):
			openAt = i
		}
	}
	if quitAt == 0 || !(dittoAt < quitAt && quitAt < openAt) {
		t.Errorf("order: %v", f.mac.calls)
	}
	if f.mac.calledWith("pkill") {
		t.Errorf("killed an app that quit politely: %v", f.mac.calls)
	}
	// The old bundle is moved aside during the swap and cleaned up after.
	if _, err := os.Lstat(filepath.Join(f.mac.appsDir, "DevVM.app.old")); err == nil {
		t.Error("DevVM.app.old left behind after a successful install")
	}
	if !f.mac.calledWith("pgrep -x -U ") {
		t.Errorf("pgrep not scoped to this user: %v", f.mac.calls)
	}
}

// When the app itself runs `devvm menubar`/`devvm update`, quitting it
// closes our stdout; the relaunch must therefore happen before the first
// line of output, or the app never comes back.
func TestMenubarRelaunchesBeforeReporting(t *testing.T) {
	for _, viaUpdate := range []bool{false, true} {
		// The fixture pins Version to v0.1.0, which is what updateMenubar
		// installs; runMenubar takes the tag explicitly.
		f := newMenubarFixture(t, "v0.1.0", false)
		f.mac.installed(t, "v0.0.9")
		f.mac.running = true
		var order []string
		f.app.Stdout = writerFunc(func(p []byte) (int, error) {
			order = append(order, "stdout")
			return f.stdout.Write(p)
		})
		f.mac.onOpen = func() { order = append(order, "open") }
		if viaUpdate {
			f.app.updateMenubar()
		} else if err := f.app.runMenubar(context.Background(), f.u, "v0.1.0", false); err != nil {
			t.Fatal(err)
		}
		if len(order) == 0 || order[0] != "open" {
			t.Errorf("viaUpdate=%v: relaunch must precede output, got %v", viaUpdate, order)
		}
		if n := strings.Count(strings.Join(f.mac.calls, "\n"), "open -a"); n != 1 {
			t.Errorf("viaUpdate=%v: opened %d times, want 1: %v", viaUpdate, n, f.mac.calls)
		}
		if !strings.Contains(f.stdout.String(), "relaunched DevVM.app") {
			t.Errorf("viaUpdate=%v: stdout = %q", viaUpdate, f.stdout.String())
		}
	}
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func TestMenubarChecksumMismatchInstallsNothing(t *testing.T) {
	f := newMenubarFixture(t, "v0.2.0", true)
	f.mac.installed(t, "v0.1.0")
	f.mac.running = true
	err := f.app.runMenubar(context.Background(), f.u, "v0.2.0", false)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	if got := f.installedVersion(t); got != "v0.1.0" {
		t.Errorf("installed %q, want untouched v0.1.0", got)
	}
	if f.mac.calledWith("osascript") || f.mac.calledWith("ditto") {
		t.Errorf("acted before verifying: %v", f.mac.calls)
	}
}

func TestUpdateMenubarKeepsInstalledAppInStep(t *testing.T) {
	f := newMenubarFixture(t, "v0.1.0", false)

	// Not installed: nothing happens, nothing said.
	f.app.updateMenubar()
	if len(f.mac.calls) != 0 || f.stdout.Len() != 0 {
		t.Errorf("acted with no app installed: %v %q", f.mac.calls, f.stdout.String())
	}

	// Installed and stale, not running: replaced, not launched.
	f.mac.installed(t, "v0.0.9")
	f.app.updateMenubar()
	if got := f.installedVersion(t); got != "v0.1.0" {
		t.Errorf("installed %q, want v0.1.0", got)
	}
	if f.mac.calledWith("open") {
		t.Errorf("launched an app that was not running: %v", f.mac.calls)
	}
	if !strings.Contains(f.stdout.String(), "updated DevVM.app v0.0.9 -> v0.1.0") {
		t.Errorf("stdout = %q", f.stdout.String())
	}

	// Already current: left alone.
	f.mac.calls = nil
	f.app.updateMenubar()
	if f.mac.calledWith("ditto") {
		t.Errorf("reinstalled a current app: %v", f.mac.calls)
	}

	// Stale and running: replaced and relaunched.
	f.mac.installed(t, "v0.0.9")
	f.mac.running = true
	f.mac.calls = nil
	f.app.updateMenubar()
	if !f.mac.calledWith("osascript") || !f.mac.calledWith("open -a") {
		t.Errorf("calls = %v", f.mac.calls)
	}
}

func TestUpdateMenubarFailureIsReportedNotFatal(t *testing.T) {
	f := newMenubarFixture(t, "v0.1.0", true)
	f.mac.installed(t, "v0.0.9")
	f.app.updateMenubar() // no return value: must not panic or exit
	if !strings.Contains(f.stderr.String(), "menu bar app not updated") || !strings.Contains(f.stderr.String(), "devvm menubar") {
		t.Errorf("stderr = %q", f.stderr.String())
	}
	if got := f.installedVersion(t); got != "v0.0.9" {
		t.Errorf("installed %q, want untouched v0.0.9", got)
	}
	// And a dev build says so instead of guessing.
	Version = "dev"
	f.stdout.Reset()
	f.app.updateMenubar()
	if !strings.Contains(f.stdout.String(), "left alone") {
		t.Errorf("stdout = %q", f.stdout.String())
	}
}

func TestFinishUpdateRunsMenubarStep(t *testing.T) {
	f := newMenubarFixture(t, "v0.1.0", false)
	f.mac.installed(t, "v0.0.9")
	if err := f.app.finishUpdate("v0.0.9"); err != nil {
		t.Fatal(err)
	}
	if got := f.installedVersion(t); got != "v0.1.0" {
		t.Errorf("installed %q, want v0.1.0", got)
	}
}

func TestRunHostToolSurfacesStderr(t *testing.T) {
	if _, err := runHostTool("sh", "-c", "echo boom >&2; exit 3"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
	out, err := runHostTool("sh", "-c", "echo hi")
	if err != nil || out != "hi" {
		t.Errorf("out = %q, %v", out, err)
	}
}
