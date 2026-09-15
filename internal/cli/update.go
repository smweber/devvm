package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
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

// maxAssetSize caps a download so a broken or hostile server can't stream
// into memory forever. Release binaries are a few tens of MB.
const maxAssetSize = 256 << 20

// daemonGoneTimeout bounds the wait for a stopped daemon to release its
// transport and unlink its socket before its replacement is spawned (see
// session.WaitGone). A worst-case shutdown against a wedged ssh master is
// one bounded `ssh -O cancel` per forward plus `-O exit` plus the wait for
// a stop-interrupted dial, so this is generous rather than derived: a
// daemon still tearing down must not be declared stuck. Var so tests can
// shrink it.
var daemonGoneTimeout = 90 * time.Second

// tagRe is the only shape a release tag may take. The tag becomes a URL path
// segment, so anything else (a `../` hop into another repo's release path,
// say) is refused before it is ever used.
var tagRe = regexp.MustCompile(`^v?\d+(\.\d+)*[A-Za-z0-9.+-]*$`)

// reexec replaces the process image and executablePath locates the running
// binary. Vars so tests can observe the hand-off and redirect the install
// target instead of replacing the test binary.
var (
	reexec         = syscall.Exec
	executablePath = os.Executable
)

func (a *App) updateCmd() *cobra.Command {
	var (
		force, check, plain bool
		version             string
		finishFrom          string
	)
	c := &cobra.Command{
		Use:   "update",
		Short: "Update devvm to the latest release",
		Long: "Download the latest devvm release for this host, verify it against the\n" +
			"release's SHA256SUMS, and replace the running binary in place (no sudo; the\n" +
			"install directory must be writable). The new binary then restarts every\n" +
			"running forward daemon so none keeps executing the old code; forwards come\n" +
			"back on the same host ports, though connections through them drop for a\n" +
			"moment. A daemon that is mid-reconnect is left alone.\n\n" +
			"SHA256SUMS comes from the same release as the binary, so it catches a\n" +
			"corrupt or partial download, not a compromised release.\n\n" +
			"Release builds and `go install …@vX.Y.Z` builds know their version. A 'dev'\n" +
			"build (go build / install.sh) cannot be compared and refuses without --force.\n" +
			"First-time installs still go through your install route (bootstrap.sh or\n" +
			"install.sh).",
		Example: "  devvm update\n  devvm update --check\n  devvm update --check --plain   # current<TAB>latest<TAB>true|false\n  devvm update --version v0.1.10",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if finishFrom != "" {
				return a.finishUpdate(finishFrom)
			}
			u := newUpdater(a.Stderr)
			return a.runUpdate(cmd.Context(), u, updateOpts{force: force, check: check, plain: plain, version: version})
		},
	}
	c.Flags().BoolVar(&force, "force", false,
		"install even if this is a dev build, already current, or lives in a package manager's tree "+
			"(forcing a downgrade to a release older than this command installs fine but cannot cycle the daemons)")
	c.Flags().BoolVar(&check, "check", false, "only report the current and latest versions")
	c.Flags().BoolVar(&plain, "plain", false, "with --check: one tab-separated line for scripts")
	c.Flags().StringVar(&version, "version", "", "install this release tag (vX.Y.Z) instead of the latest")
	// The freshly installed binary is re-exec'd with this flag to run the
	// post-install steps on the new code.
	c.Flags().StringVar(&finishFrom, "finish-from", "", "")
	_ = c.Flags().MarkHidden("finish-from")
	return c
}

type updateOpts struct {
	force, check, plain bool
	version             string
}

// updater fetches release metadata and assets. base is the releases root
// (releaseBase in production).
type updater struct {
	base   string
	client *http.Client
	stderr io.Writer
}

// newUpdater builds a client with per-phase timeouts (dial, TLS, response
// headers) and no overall deadline: a whole-request timeout would cap the
// binary download and fail outright on a slow link.
func newUpdater(stderr io.Writer) *updater {
	client := &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}}
	return &updater{base: releaseBase, client: client, stderr: stderr}
}

// currentVersion is the running build's version: the release stamp, else the
// module version Go embeds for `go install …@vX.Y.Z` builds, else "dev".
func currentVersion() string {
	// install.sh/release.sh stamp `git describe --tags --always --dirty`, so
	// a clone with no reachable tag yields a bare hash: not a version, and
	// it would parse as one ("83f409a" > "v0.1.11"). Every real tag starts
	// with v, which is the reliable discriminator.
	if strings.HasPrefix(Version, "v") && tagRe.MatchString(Version) {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && tagRe.MatchString(bi.Main.Version) {
		return bi.Main.Version
	}
	return "dev"
}

func (a *App) runUpdate(ctx context.Context, u *updater, o updateOpts) error {
	current := currentVersion()
	tag := o.version
	if tag == "" {
		var err error
		if tag, err = u.latestTag(ctx); err != nil {
			return fmt.Errorf("find latest release: %w", err)
		}
	} else if !tagRe.MatchString(tag) {
		return fmt.Errorf("invalid release tag %q (want vX.Y.Z)", tag)
	}
	available := current != "dev" && compareVersions(tag, current) > 0
	if o.check {
		if o.plain {
			fmt.Fprintf(a.Stdout, "%s\t%s\t%t\n", current, tag, available)
		} else {
			fmt.Fprintf(a.Stdout, "current: %s\nlatest:  %s\n", current, tag)
		}
		return nil
	}
	switch {
	case current == "dev" && !o.force:
		return fmt.Errorf("this is a dev build with no version to compare; use --force to install %s anyway, or reinstall the way you first did", tag)
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

	invoked, err := executablePath()
	if err != nil {
		return fmt.Errorf("locate the running binary: %w", err)
	}
	exe, err := filepath.EvalSymlinks(invoked)
	if err != nil {
		return fmt.Errorf("locate the running binary: %w", err)
	}
	if exe != invoked {
		fmt.Fprintf(a.Stdout, "devvm is %s -> %s; replacing the target\n", invoked, exe)
	}
	if mgr := packageManagerFor(exe); mgr != "" && !o.force {
		return fmt.Errorf("%s is installed by %s; update it there, or use --force to overwrite it in place", exe, mgr)
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

// packageManagerFor names the package manager whose tree exe lives in, or ""
// if it looks hand-installed. Overwriting a keg or store path works for a
// moment and then the manager's next upgrade reverts it or errors.
func packageManagerFor(exe string) string {
	switch {
	case strings.Contains(exe, "/Cellar/"), strings.Contains(exe, "/linuxbrew/"):
		return "Homebrew"
	case strings.Contains(exe, "/nix/store/"):
		return "Nix"
	}
	return ""
}

// finishUpdate runs on the freshly installed binary (see the --finish-from
// hand-off): report, cycle the forward daemons, then the menu bar app. It
// exits non-zero if any daemon failed to come back, so a scripted update
// can't mistake half-restarted forwards for success.
func (a *App) finishUpdate(old string) error {
	fmt.Fprintf(a.Stdout, "updated devvm %s -> %s\n", old, Version)
	res := a.restartDaemons()
	a.updateMenubar()
	if len(res.failed) > 0 {
		return fmt.Errorf("forwards not restarted for %s; run 'devvm ports up NAME' for each", strings.Join(res.failed, ", "))
	}
	return nil
}

// restartResult is what restartDaemons did per machine.
type restartResult struct {
	cycled, skipped, failed []string
}

// restartDaemons cycles every running forward daemon through the same paths
// as `ports down` + `ports up`, so none keeps running the pre-update code.
// Forwards return on the host ports they had (the daemon re-requests the
// configured preferred ports; only a port taken meanwhile bumps). Left alone,
// with a note: a daemon mid-reconnect (its forwards are pending anyway and a
// stop would race the dial), one holding only forwards that aren't in the
// conf (an `auth` callback bridge — `ports up` would not bring them back),
// and one already on this build. Errors are reported per machine and never
// abort the rest.
func (a *App) restartDaemons() restartResult {
	var res restartResult
	names, _ := config.List(a.ConfigDir)
	for _, name := range names {
		cl, err := session.Existing(a.ConfigDir, name)
		if err != nil {
			continue // no daemon: nothing running old code
		}
		st, err := cl.Status()
		if err != nil {
			fmt.Fprintf(a.Stderr, "devvm: %s: query forwards: %v\n", name, err)
			res.failed = append(res.failed, name)
			continue
		}
		skip := func(why string) {
			fmt.Fprintf(a.Stdout, "%s: %s\n", name, why)
			res.skipped = append(res.skipped, name)
		}
		switch {
		case st.Version == Version && Version != "dev":
			skip("forward daemon already runs " + Version)
			continue
		case st.Reconnecting():
			skip("forward daemon is reconnecting; not restarted, it picks up the new binary next time it is started")
			continue
		case !anyConfiguredForward(a.ConfigDir, name, st.Forwards):
			skip("forward daemon holds no configured ports (idle, or an auth callback); left to exit on its own")
			continue
		}
		if err := cl.Stop(); err != nil {
			fmt.Fprintf(a.Stderr, "devvm: %s: stop forwards: %v\n", name, err)
			res.failed = append(res.failed, name)
			continue
		}
		if !session.WaitGone(a.ConfigDir, name, daemonGoneTimeout) {
			fmt.Fprintf(a.Stderr, "devvm: %s: old forward daemon did not exit within %s\n", name, daemonGoneTimeout)
			res.failed = append(res.failed, name)
			continue
		}
		fmt.Fprintf(a.Stdout, "restarting forwards for %s\n", name)
		if err := a.tunnelUp(name); err != nil {
			fmt.Fprintf(a.Stderr, "devvm: %s: bring forwards back up: %v\n", name, err)
			res.failed = append(res.failed, name)
			continue
		}
		if cl, err := session.Existing(a.ConfigDir, name); err == nil {
			if st, err := cl.Status(); err == nil && st.Version != Version {
				fmt.Fprintf(a.Stderr, "devvm: %s: new forward daemon reports %q, expected %q\n", name, st.Version, Version)
				res.failed = append(res.failed, name)
				continue
			}
		}
		res.cycled = append(res.cycled, name)
	}
	return res
}

// anyConfiguredForward reports whether at least one of the daemon's forwards
// maps a guest port listed in the machine's conf — i.e. whether `ports up`
// would recreate anything after a stop.
func anyConfiguredForward(configDir, name string, fwds []session.Forward) bool {
	m, err := config.Load(configDir, name)
	if err != nil {
		return false
	}
	configured := map[int]bool{}
	for _, mapping := range m.Ports {
		if _, guest, err := parseMapping(mapping); err == nil {
			configured[guest] = true
		}
	}
	for _, f := range fwds {
		if configured[f.Guest] {
			return true
		}
	}
	return false
}

// latestTag resolves the "latest" release without the GitHub API (no auth, no
// rate limit): GitHub answers /releases/latest with a redirect to
// /releases/tag/<tag>, so the tag is the redirect's last path segment. The
// redirect is never followed; only its last segment is used, and only if it
// is shaped like a tag.
func (u *updater) latestTag(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
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
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	loc := resp.Header.Get("Location")
	if resp.StatusCode/100 != 3 || loc == "" {
		return "", fmt.Errorf("expected a redirect from %s/latest, got %s", u.base, resp.Status)
	}
	tag := path.Base(loc)
	if !strings.HasPrefix(tag, "v") || !tagRe.MatchString(tag) {
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
	var r io.Reader = io.LimitReader(resp.Body, maxAssetSize+1)
	if progress && resp.ContentLength > 0 {
		r = &progressReader{r: r, total: resp.ContentLength, label: asset, out: u.stderr}
		defer fmt.Fprintln(u.stderr)
	}
	n, err := io.Copy(w, r)
	if err != nil {
		return err
	}
	if n > maxAssetSize {
		return fmt.Errorf("%s exceeds %d bytes", asset, maxAssetSize)
	}
	return nil
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
// existing mode, fsync'd, then rename over the running binary — which keeps
// executing its old inode untouched. Never escalates: an unwritable directory
// is an error.
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
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return fail(err)
	}
	// Without the fsync a crash right after the rename could leave an empty
	// devvm with the old inode already gone.
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%s is not writable; re-run as the user who installed devvm there, or reinstall to a directory you own", dir)
		}
		return err
	}
	return nil
}

// describeRe matches git-describe's "commits past a tag" suffix.
var describeRe = regexp.MustCompile(`^-(\d+)-g[0-9a-fA-F]+(-dirty)?$`)

// compareVersions orders two release tags numerically by dotted component:
// -1 if a < b, 0 if equal, 1 if a > b. Suffixes after the numbers break ties:
// a git-describe suffix (-N-g<hash>, or -dirty on an exact tag) is a build
// PAST the tag and sorts above it, with N compared numerically; any other
// suffix (-rc1, -beta) is a prerelease and sorts BELOW the bare tag, so a
// user on v1.2.0-rc1 is offered v1.2.0.
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
			return cmpInt(x, y)
		}
	}
	ar, aN := suffixRank(as)
	br, bN := suffixRank(bs)
	if ar != br {
		return cmpInt(ar, br)
	}
	if ar == 1 {
		return cmpInt(aN, bN)
	}
	return strings.Compare(as, bs)
}

// suffixRank classifies a version suffix: 0 for none (the release itself),
// 1 for a git-describe build past it (with its commit count), -1 for a
// prerelease.
func suffixRank(s string) (rank, commits int) {
	switch {
	case s == "":
		return 0, 0
	case s == "-dirty":
		return 1, 0
	}
	if m := describeRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return 1, n
	}
	return -1, 0
}

func cmpInt(x, y int) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
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
