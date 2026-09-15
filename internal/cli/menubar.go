package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The macOS menu bar app (contrib/macos) ships as one zip per release next to
// the host binaries. It is a thin shell over this CLI and must match its
// version, so `menubar` installs the app for *this* build's version and
// `update` re-installs it whenever the CLI moves.
const (
	menubarAppName  = "DevVM"
	menubarBundleID = "com.smweber.devvm.menubar"
	menubarAsset    = "devvm-menubar.zip"
)

// menubarQuitTimeout bounds the wait for a politely quit app to exit before
// it is killed. Var so tests don't wait.
var menubarQuitTimeout = 5 * time.Second

// Vars so the flow can run under test on Linux against a temp directory:
// the OS check, where ~/Applications is, and how host tools are invoked.
var (
	menubarGOOS    = runtime.GOOS
	menubarAppsDir = func() (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Applications"), nil
	}
	menubarRun hostRunner = runHostTool
	// newMenubarUpdater builds the release client for the update-time step,
	// which has no updater handed in; tests point it at a local server.
	newMenubarUpdater = newUpdater
)

// hostRunner runs a host tool and returns its trimmed stdout; a non-zero exit
// is an error carrying the tool's stderr.
type hostRunner func(name string, args ...string) (string, error)

func runHostTool(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
			return "", fmt.Errorf("%s: %s", name, bytes.TrimSpace(ee.Stderr))
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (a *App) menubarCmd() *cobra.Command {
	var (
		force   bool
		version string
	)
	c := &cobra.Command{
		Use:   "menubar",
		Short: "Install and open the macOS menu bar app",
		Long: "Install the DevVM menu bar app (a drop target for cp-in, machine status,\n" +
			"start/stop, and forwards) into ~/Applications at this devvm's version, then\n" +
			"open it. Run it again any time to open the app; it re-downloads only when\n" +
			"the installed version differs. `devvm update` keeps it current afterwards.\n\n" +
			"The zip is fetched from the same GitHub release as the binaries and verified\n" +
			"against its .sha256 sidecar. Downloads made by devvm carry no quarantine\n" +
			"flag, so the ad-hoc signed app opens without Gatekeeper prompts. To remove\n" +
			"it, drag DevVM.app out of ~/Applications.\n\n" +
			"macOS only. A dev build has no release to match; pass --version or build the\n" +
			"app yourself from contrib/macos/build.sh.",
		Example: "  devvm menubar\n  devvm menubar --version v0.2.0\n  devvm menubar --force   # reinstall the current version",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runMenubar(cmd.Context(), newUpdater(a.Stderr), version, force)
		},
	}
	c.Flags().BoolVar(&force, "force", false, "reinstall even if the installed app already matches")
	c.Flags().StringVar(&version, "version", "", "install the app from this release tag (vX.Y.Z) instead of this devvm's version")
	return c
}

func (a *App) runMenubar(ctx context.Context, u *updater, version string, force bool) error {
	if menubarGOOS != "darwin" {
		return fmt.Errorf("the menu bar app is macOS-only")
	}
	tag := version
	if tag == "" {
		tag = currentVersion()
		if tag == "dev" {
			return fmt.Errorf("this devvm build has no release version; pass --version vX.Y.Z or build the app from contrib/macos/build.sh")
		}
	}
	if !tagRe.MatchString(tag) {
		return fmt.Errorf("invalid release tag %q (want vX.Y.Z)", tag)
	}
	m, err := a.menubarInstaller(u)
	if err != nil {
		return err
	}
	if installed := m.installedVersion(); installed != "" && compareVersions(installed, tag) == 0 && !force {
		fmt.Fprintf(a.Stdout, "%s.app %s is already installed in %s\n", menubarAppName, installed, m.appsDir)
	} else {
		if _, err := m.install(ctx, tag); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "installed %s.app %s to %s\n", menubarAppName, tag, m.appsDir)
	}
	if err := m.open(); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "opened %s.app\n", menubarAppName)
	return nil
}

// updateMenubar is update's post-install step: if the menu bar app is
// installed and on another version, re-install it at this build's version,
// relaunching only if it was running. The CLI itself updated fine by the time
// this runs, so a failure here is reported with a hint, never an exit code.
func (a *App) updateMenubar() {
	if menubarGOOS != "darwin" {
		return
	}
	m, err := a.menubarInstaller(newMenubarUpdater(a.Stderr))
	if err != nil {
		return
	}
	installed := m.installedVersion()
	if installed == "" {
		return // not installed: nothing to keep in step
	}
	tag := currentVersion()
	if tag == "dev" {
		fmt.Fprintf(a.Stdout, "%s.app %s left alone (this build has no release version)\n", menubarAppName, installed)
		return
	}
	if compareVersions(installed, tag) == 0 {
		fmt.Fprintf(a.Stdout, "%s.app already %s\n", menubarAppName, installed)
		return
	}
	wasRunning, err := m.install(context.Background(), tag)
	if err != nil {
		fmt.Fprintf(a.Stderr, "devvm: menu bar app not updated: %v\n  run 'devvm menubar' to retry\n", err)
		return
	}
	fmt.Fprintf(a.Stdout, "updated %s.app %s -> %s\n", menubarAppName, installed, tag)
	if wasRunning {
		if err := m.open(); err != nil {
			fmt.Fprintf(a.Stderr, "devvm: could not relaunch %s.app: %v\n", menubarAppName, err)
		}
	}
}

// menubarInstaller installs the app bundle from a release into appsDir.
type menubarInstaller struct {
	u       *updater
	run     hostRunner
	appsDir string
	stderr  io.Writer
}

func (a *App) menubarInstaller(u *updater) (*menubarInstaller, error) {
	dir, err := menubarAppsDir()
	if err != nil {
		return nil, err
	}
	return &menubarInstaller{u: u, run: menubarRun, appsDir: dir, stderr: a.Stderr}, nil
}

func (m *menubarInstaller) appPath() string {
	return filepath.Join(m.appsDir, menubarAppName+".app")
}

// installedVersion is the bundle's CFBundleShortVersionString (build.sh stamps
// the release tag there), or "" when no app is installed.
func (m *menubarInstaller) installedVersion() string {
	plist := filepath.Join(m.appPath(), "Contents", "Info.plist")
	if _, err := os.Stat(plist); err != nil {
		return ""
	}
	v, err := m.run("/usr/libexec/PlistBuddy", "-c", "Print :CFBundleShortVersionString", plist)
	if err != nil {
		return ""
	}
	return v
}

// running matches by exact name within this user's processes only; another
// user's process called DevVM must neither read as "our app" nor be killed.
func (m *menubarInstaller) running() bool {
	_, err := m.run("pgrep", "-x", "-U", strconv.Itoa(os.Getuid()), menubarAppName)
	return err == nil
}

// quit asks the app to exit and waits; a stubborn one is killed so the
// bundle can be replaced underneath it.
func (m *menubarInstaller) quit() error {
	_, _ = m.run("osascript", "-e", fmt.Sprintf("tell application id %q to quit", menubarBundleID))
	deadline := time.Now().Add(menubarQuitTimeout)
	for time.Now().Before(deadline) {
		if !m.running() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := m.run("pkill", "-x", "-U", strconv.Itoa(os.Getuid()), menubarAppName); err != nil {
		return fmt.Errorf("quit %s.app: %w", menubarAppName, err)
	}
	return nil
}

func (m *menubarInstaller) open() error {
	if _, err := m.run("open", "-a", m.appPath()); err != nil {
		return fmt.Errorf("open %s.app: %w", menubarAppName, err)
	}
	return nil
}

// install downloads the release's app zip, verifies it, quits a running app,
// and swaps the bundle into place. Reports whether the app was running so
// the caller can decide about relaunching. Nothing is replaced until the
// download has verified.
func (m *menubarInstaller) install(ctx context.Context, tag string) (wasRunning bool, err error) {
	if err := os.MkdirAll(m.appsDir, 0o755); err != nil {
		return false, err
	}
	var sums bytes.Buffer
	if err := m.u.fetch(ctx, tag, menubarAsset+".sha256", &sums, false); err != nil {
		return false, fmt.Errorf("download %s.sha256: %w", menubarAsset, err)
	}
	want, ok := parseSums(&sums)[menubarAsset]
	if !ok {
		return false, fmt.Errorf("release %s has no checksum for %s", tag, menubarAsset)
	}
	var zip bytes.Buffer
	if err := m.u.fetch(ctx, tag, menubarAsset, &zip, true); err != nil {
		return false, fmt.Errorf("download %s: %w", menubarAsset, err)
	}
	if got := sha256Hex(zip.Bytes()); got != want {
		return false, fmt.Errorf("%s: sha256 mismatch (got %s, sidecar says %s); nothing installed", menubarAsset, got, want)
	}

	// Stage inside appsDir so the final rename is on one volume (atomic), and
	// extract with ditto: the bundle's code signature lives partly in extended
	// attributes and resource forks that archive/zip would drop.
	stage, err := os.MkdirTemp(m.appsDir, ".devvm-menubar-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(stage)
	zipPath := filepath.Join(stage, menubarAsset)
	if err := os.WriteFile(zipPath, zip.Bytes(), 0o600); err != nil {
		return false, err
	}
	extracted := filepath.Join(stage, "extracted")
	if _, err := m.run("ditto", "-x", "-k", zipPath, extracted); err != nil {
		return false, fmt.Errorf("extract %s: %w", menubarAsset, err)
	}
	newApp := filepath.Join(extracted, menubarAppName+".app")
	if _, err := os.Stat(filepath.Join(newApp, "Contents", "Info.plist")); err != nil {
		return false, fmt.Errorf("%s does not contain %s.app", menubarAsset, menubarAppName)
	}

	wasRunning = m.running()
	if wasRunning {
		if err := m.quit(); err != nil {
			return true, err
		}
	}
	// Move the old bundle aside rather than deleting it, so a failed rename
	// of the new one leaves the previous app in place instead of none.
	old := m.appPath() + ".old"
	os.RemoveAll(old) // a leftover from an earlier interrupted install
	hadOld := false
	if _, err := os.Lstat(m.appPath()); err == nil {
		if err := os.Rename(m.appPath(), old); err != nil {
			return wasRunning, fmt.Errorf("move old %s.app aside: %w", menubarAppName, err)
		}
		hadOld = true
	}
	if err := os.Rename(newApp, m.appPath()); err != nil {
		if hadOld {
			_ = os.Rename(old, m.appPath())
		}
		return wasRunning, fmt.Errorf("install %s.app: %w", menubarAppName, err)
	}
	if hadOld {
		os.RemoveAll(old)
	}
	return wasRunning, nil
}
