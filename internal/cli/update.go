package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
	"github.com/spf13/cobra"
)

// releaseRepo is the GitHub repository whose releases `update` installs from.
// releaseBase is its releases root; tests point an updater at an httptest
// server instead.
const (
	releaseRepo = "smweber/devvm"
	releaseBase = "https://github.com/" + releaseRepo + "/releases"
)

// daemonGoneTimeout bounds the wait for a stopped daemon to unlink its socket
// before its replacement is spawned (see session.WaitGone).
const daemonGoneTimeout = 5 * time.Second

// reexec replaces the process image and executablePath locates the running
// binary. Vars so tests can observe the hand-off and redirect the install
// target instead of replacing the test binary.
var (
	reexec         = syscall.Exec
	executablePath = os.Executable
)

func (a *App) updateCmd() *cobra.Command {
	var (
		force, check bool
		version      string
		finishFrom   string
	)
	c := &cobra.Command{
		Use:   "update",
		Short: "Update devvm to the latest release",
		Long: "Download the latest devvm release for this host, verify it against the\n" +
			"release's SHA256SUMS, and replace the running binary in place (no sudo; the\n" +
			"install directory must be writable). The new binary then restarts every\n" +
			"running forward daemon so none keeps executing the old code; forwards come\n" +
			"back on the same host ports.\n\n" +
			"Only release builds know their version. A 'dev' build (go build / install.sh)\n" +
			"cannot be compared and refuses without --force. First-time installs still go\n" +
			"through bootstrap.sh or install.sh.",
		Example: "  devvm update\n  devvm update --check\n  devvm update --version v0.1.10",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if finishFrom != "" {
				return a.finishUpdate(finishFrom)
			}
			u := newUpdater(a.Stderr)
			return a.runUpdate(cmd.Context(), u, updateOpts{force: force, check: check, version: version})
		},
	}
	c.Flags().BoolVar(&force, "force", false, "install even if this is a dev build or already current")
	c.Flags().BoolVar(&check, "check", false, "only report the current and latest versions")
	c.Flags().StringVar(&version, "version", "", "install this release tag (vX.Y.Z) instead of the latest")
	// The freshly installed binary is re-exec'd with this flag to run the
	// post-install steps on the new code.
	c.Flags().StringVar(&finishFrom, "finish-from", "", "")
	_ = c.Flags().MarkHidden("finish-from")
	return c
}

type updateOpts struct {
	force, check bool
	version      string
}

// updater fetches release metadata and assets. base is the releases root
// (releaseBase in production).
type updater struct {
	base   string
	client *http.Client
	stderr io.Writer
}

func newUpdater(stderr io.Writer) *updater {
	return &updater{base: releaseBase, client: &http.Client{Timeout: 60 * time.Second}, stderr: stderr}
}

func (a *App) runUpdate(ctx context.Context, u *updater, o updateOpts) error {
	if runtime.GOOS == "windows" {
		return errors.New("update is not supported on windows")
	}
	current := Version
	tag := o.version
	if tag == "" {
		var err error
		if tag, err = u.latestTag(ctx); err != nil {
			return fmt.Errorf("find latest release: %w", err)
		}
	}
	if o.check {
		fmt.Fprintf(a.Stdout, "current: %s\nlatest:  %s\n", current, tag)
		return nil
	}
	switch {
	case current == "dev" && !o.force:
		return fmt.Errorf("this is a dev build with no version to compare; use --force to install %s anyway, or rebuild with install.sh", tag)
	case current != "dev" && !o.force:
		switch compareVersions(tag, current) {
		case 0:
			fmt.Fprintf(a.Stdout, "devvm %s is current\n", current)
			return nil
		case -1:
			fmt.Fprintf(a.Stdout, "devvm %s is newer than %s; use --force to downgrade\n", current, tag)
			return nil
		}
	}

	exe, err := executablePath()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	asset := "devvm-" + runtime.GOOS + "-" + runtime.GOARCH

	var sums bytes.Buffer
	if err := u.fetch(ctx, tag, "SHA256SUMS", &sums, false); err != nil {
		return fmt.Errorf("download SHA256SUMS: %w", err)
	}
	want, ok := parseSums(&sums)[asset]
	if !ok {
		return fmt.Errorf("release %s has no SHA256SUMS entry for %s", tag, asset)
	}
	var bin bytes.Buffer
	if err := u.fetch(ctx, tag, asset, &bin, true); err != nil {
		return fmt.Errorf("download %s: %w", asset, err)
	}
	if got := sha256Hex(bin.Bytes()); got != want {
		return fmt.Errorf("%s: sha256 mismatch (got %s, SHA256SUMS says %s); nothing installed", asset, got, want)
	}
	if err := installBinary(bin.Bytes(), exe); err != nil {
		return err
	}
	// Hand off to the new code for the post-install steps. exec only returns
	// on failure, in which case the binary is installed but the daemons were
	// not cycled; say so rather than pretend.
	argv := []string{exe, "--config-dir", a.ConfigDir, "update", "--finish-from", current}
	if err := reexec(exe, argv, os.Environ()); err != nil {
		return fmt.Errorf("installed %s at %s but could not re-exec it to finish (%v); run 'devvm ports down NAME' and 'devvm ports up NAME' for any running forwards", tag, exe, err)
	}
	return nil
}

// finishUpdate runs on the freshly installed binary (see the --finish-from
// hand-off): report, cycle the forward daemons, then the menu bar app.
func (a *App) finishUpdate(old string) error {
	fmt.Fprintf(a.Stdout, "updated devvm %s -> %s\n", old, Version)
	a.restartDaemons()
	a.updateMenubar()
	return nil
}

// restartDaemons cycles every running forward daemon through the same paths
// as `ports down` + `ports up`, so none keeps running the pre-update code.
// Forwards return on the host ports they had (the daemon re-requests the
// configured preferred ports; only a port taken meanwhile bumps). Errors are
// reported per machine and never abort the rest. Returns the machines cycled.
func (a *App) restartDaemons() []string {
	names, _ := config.List(a.ConfigDir)
	var cycled []string
	for _, name := range names {
		cl, err := session.Existing(a.ConfigDir, name)
		if err != nil {
			continue // no daemon: nothing running old code
		}
		if err := cl.Stop(); err != nil {
			fmt.Fprintf(a.Stderr, "devvm: %s: stop forwards: %v\n", name, err)
			continue
		}
		if !session.WaitGone(a.ConfigDir, name, daemonGoneTimeout) {
			fmt.Fprintf(a.Stderr, "devvm: %s: old forward daemon did not exit; run 'devvm ports up %s' once it has\n", name, name)
			continue
		}
		fmt.Fprintf(a.Stdout, "restarting forwards for %s\n", name)
		if err := a.tunnelUp(name); err != nil {
			fmt.Fprintf(a.Stderr, "devvm: %s: bring forwards back up: %v\n", name, err)
			continue
		}
		cycled = append(cycled, name)
	}
	return cycled
}

// updateMenubar keeps the macOS menu bar app (if installed) at the CLI's
// version. TODO(menubar): lands with the app itself — download the matching
// app asset from the same release into ~/Applications and relaunch it if it
// was running. Until then there is nothing to update.
func (a *App) updateMenubar() {}

// latestTag resolves the "latest" release without the GitHub API (no auth, no
// rate limit): GitHub answers /releases/latest with a redirect to
// /releases/tag/<tag>, so the tag is the redirect's last path segment.
func (u *updater) latestTag(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.base+"/latest", nil)
	if err != nil {
		return "", err
	}
	client := *u.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	loc := resp.Header.Get("Location")
	if resp.StatusCode/100 != 3 || loc == "" {
		return "", fmt.Errorf("expected a redirect from %s/latest, got %s", u.base, resp.Status)
	}
	tag := path.Base(loc)
	if !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("unexpected release redirect %q", loc)
	}
	return tag, nil
}

// fetch downloads one release asset into w. With progress, a single
// carriage-return line on stderr tracks the transfer when the size is known.
func (u *updater) fetch(ctx context.Context, tag, asset string, w io.Writer, progress bool) error {
	url := u.base + "/download/" + tag + "/" + asset
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	var r io.Reader = resp.Body
	if progress && resp.ContentLength > 0 {
		r = &progressReader{r: resp.Body, total: resp.ContentLength, label: asset, out: u.stderr}
		defer fmt.Fprintln(u.stderr)
	}
	_, err = io.Copy(w, r)
	return err
}

type progressReader struct {
	r           io.Reader
	total, done int64
	label       string
	out         io.Writer
	last        time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if err == io.EOF || time.Since(p.last) > 100*time.Millisecond {
		p.last = time.Now()
		fmt.Fprintf(p.out, "\rdownloading %s: %s / %s", p.label, sizeHuman(p.done), sizeHuman(p.total))
	}
	return n, err
}

func sizeHuman(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// parseSums reads `sha256sum` output (`<hex>  <name>` per line) into name -> hex.
func parseSums(r io.Reader) map[string]string {
	sums := map[string]string{}
	data, _ := io.ReadAll(r)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		// sha256sum marks binary mode with a leading '*' on the name.
		sums[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	return sums
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// installBinary replaces target with data atomically: a temp file in the same
// directory (same filesystem, so rename can't degrade to copy+unlink), the
// existing mode, then rename over the running binary — which keeps executing
// its old inode untouched. Never escalates: an unwritable directory is an error.
func installBinary(data []byte, target string) error {
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".devvm-update-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%s is not writable; re-run as the user who installed devvm there, or reinstall to a directory you own", dir)
		}
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%s is not writable; re-run as the user who installed devvm there, or reinstall to a directory you own", dir)
		}
		return err
	}
	return nil
}

// compareVersions orders two release tags (v1.2.3, 1.2.3, or git-describe
// forms like v1.2.3-4-gabcdef / v1.2.3-dirty) numerically by dotted
// component: -1 if a < b, 0 if equal, 1 if a > b. A suffix after the numbers
// marks a build past that tag, so it sorts above the bare tag.
func compareVersions(a, b string) int {
	an, as := splitVersion(a)
	bn, bs := splitVersion(b)
	for i := 0; i < len(an) || i < len(bn); i++ {
		var x, y int
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case as == bs:
		return 0
	case as == "":
		return -1
	case bs == "":
		return 1
	}
	return strings.Compare(as, bs)
}

// splitVersion separates the dotted numeric prefix from any trailing suffix.
func splitVersion(v string) ([]int, string) {
	v = strings.TrimPrefix(v, "v")
	var nums []int
	i := 0
	for i < len(v) {
		j := i
		for j < len(v) && v[j] >= '0' && v[j] <= '9' {
			j++
		}
		if j == i {
			break
		}
		n, _ := strconv.Atoi(v[i:j])
		nums = append(nums, n)
		i = j
		if i < len(v) && v[i] == '.' {
			i++
			continue
		}
		break
	}
	return nums, v[i:]
}
