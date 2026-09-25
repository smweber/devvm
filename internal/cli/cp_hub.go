package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// cp for hub machines (docs/proposals/hub.md §6, roadmap step 4). The
// archive is the wire format cp already uses, so a copy to or from HUB/NAME
// is the same archive streamed through the proxied command's stdio: the
// laptop builds it (cp-in) or extracts it (cp-out) exactly as for a local
// machine, and the hub runs a hidden form of the same leaf that takes the
// tar on stdin (`cp-in NAME --from-tar - DEST`) or writes it to stdout
// (`cp-out NAME SRC --to-tar -`) and otherwise reuses copyArchive and
// downloadArchive against its own backend. So the hub-side forms work for
// any machine the hub runs — smol through `machine cp` staging, remote over
// scp — and nothing here knows which.
//
// The stream rides a plain ssh (no -t: a pty would turn \n into \r\n and
// merge stderr into it) with BatchMode, over the same login-shell wrapper
// every proxied leaf uses. That shell is the reason for the marker line: a
// .bash_profile or .zprofile that echoes lands on stdout before the hub's
// devvm runs, so cp-out's stream starts with `devvm-tar-v1\n` and the laptop
// discards everything up to it. For cp-in the hub's stdout is discarded
// outright (nothing on it is ours); its stderr is passed through verbatim,
// which is how a refusal ("refusing to overwrite (use -f)") reads on the
// laptop, and the hub's exit status is the laptop's, silently, as for every
// proxied leaf. The `->` progress lines are printed by the laptop only; the
// hidden forms print none, so nothing is doubled.
//
// hubBackend.Copy keeps returning ErrHubProxy: the hub needs the archive,
// the destination and the flags, not a path pair, so the dispatch lives
// here at the cli level (cpInCmd/cpOutCmd) rather than behind Backend.

// tarMarker is the line the hub prints before a `cp-out --to-tar -` stream.
// Versioned so a later stream format can announce itself; the reader takes
// this exact line only.
const tarMarker = "devvm-tar-v1"

// tarMarkerLimit bounds how much a login shell may print before the marker.
// A banner is a few lines; a megabyte of stdout with no marker in it is a
// hub that is not going to send one (an old devvm printing usage, say), and
// is refused rather than buffered without end.
const tarMarkerLimit = 1 << 20

// Hidden flag names of the hub-side forms.
const (
	fromTarFlag = "from-tar"
	toTarFlag   = "to-tar"
)

// stdinSpec is the only value the hidden flags take.
const stdinSpec = "-"

// copyInSources validates cp-in's sources on the laptop and returns their
// absolute paths and the basenames they are copied as, the checks every
// cp-in makes before any transport is touched.
func copyInSources(srcs []string, dst string, o copyOpts) (abs, bases []string, err error) {
	if dst == "" {
		return nil, nil, fmt.Errorf("destination must not be empty")
	}
	if len(srcs) > 1 && !o.asDir {
		return nil, nil, fmt.Errorf("copying several sources needs a destination directory")
	}
	seen := map[string]string{}
	for _, src := range srcs {
		src, err := filepath.Abs(src)
		if err != nil {
			return nil, nil, err
		}
		info, err := os.Lstat(src)
		if err != nil {
			return nil, nil, err
		}
		if info.IsDir() && !o.recursive {
			return nil, nil, fmt.Errorf("%s is a directory; use -r to copy it", src)
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return nil, nil, fmt.Errorf("unsupported source file type: %s", src)
		}
		if filepath.Dir(src) == src {
			return nil, nil, fmt.Errorf("copying the filesystem root is not supported")
		}
		base := filepath.Base(src)
		if prev, dup := seen[base]; dup {
			return nil, nil, fmt.Errorf("%s and %s would both be copied as %s", prev, src, base)
		}
		seen[base] = src
		abs, bases = append(abs, src), append(bases, base)
	}
	return abs, bases, nil
}

// runCopyInHub is cp-in for HUB/NAME: the laptop's own archive, streamed as
// the stdin of the hub's hidden form. The archive is handed to ssh as an
// *os.File so exec passes the descriptor straight through (any other
// reader gets a copy goroutine that Wait then waits on); ssh forwards its
// EOF, which is how the hub knows the stream is complete. Stdout from the
// hub is discarded: the hidden form prints nothing there, so anything on it
// is the login shell's.
func (a *App) runCopyInHub(ctx context.Context, name string, srcs []string, dst string, o copyOpts) error {
	abs, bases, err := copyInSources(srcs, dst, o)
	if err != nil {
		return err
	}
	m, p, err := a.resolveProxy(name)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "devvm-cp-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	archive := filepath.Join(tmp, "payload.tar")
	if err := writeArchive(archive, abs, a.Stderr); err != nil {
		return fmt.Errorf("archive source: %w", err)
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	argv := hubCopyInArgv(m.HubMachineName(), dst, o)
	eo := backend.ExecOpts{BatchMode: true, Stdin: f, Stdout: io.Discard, Stderr: a.Stderr}
	if err := a.exitStatus(m, p.Proxy(ctx, eo, argv...)); err != nil {
		return err
	}
	copyTargets(a.Stderr, bases, dst, o.asDir || len(bases) > 1, "%s -> "+name+":%s")
	return nil
}

// hubCopyInArgv is the hub-side command line for a cp-in into machine:
// `cp-in NAME --from-tar - [-r] [-f] (-t DEST | DEST)`. -t carries the
// "DEST is a directory" decision the laptop already made (from -t, a
// trailing slash or the default), so the hub does not re-derive it. -r is
// forwarded for the record only: the archive already holds whatever the
// laptop decided to copy, and the hub-side form ignores it. A DEST that
// starts with `-` goes after `--` so cobra never reads it as a flag.
func hubCopyInArgv(machine, dst string, o copyOpts) []string {
	argv := []string{"cp-in", machine, "--" + fromTarFlag, stdinSpec}
	if o.recursive {
		argv = append(argv, "-r")
	}
	if o.force {
		argv = append(argv, "-f")
	}
	switch {
	case o.asDir:
		argv = append(argv, "-t", dst)
	case strings.HasPrefix(dst, "-"):
		argv = append(argv, "--", dst)
	default:
		argv = append(argv, dst)
	}
	return argv
}

// runCopyInFromTar is the hub side of cp-in: the archive arrives on stdin
// and goes into the machine through the same staging path as a local cp-in.
// It runs for a local machine only; the laptop's loop is what put the
// stream here, so a HUB/NAME would mean a hub proxying to a hub, which is a
// non-goal.
//
// The stream is spooled to a file first, and only a complete archive —
// one that ends with tar's end-of-archive blocks (readTarStream) — is used.
// archive/tar reads a stream cut at an entry boundary as a shorter, valid
// archive, so without this check a laptop killed mid-upload could leave the
// sources it had finished sending in place; with it, nothing is uploaded,
// and the staging + all-or-nothing placement in copyArchive covers
// everything after.
//
// What lands inside the machine is placed by the guest's own `tar -xf` in
// copyArchive's script, not by extractArchive: a crafted archive that
// writes through a symlink is bounded by that tar's behaviour, which is no
// escalation — whoever can run this form can already `exec` on the machine.
// extractArchive's guards are the laptop's, for cp-out.
func (a *App) runCopyInFromTar(ctx context.Context, spec string, args []string, target string, o copyOpts) error {
	if spec != stdinSpec {
		return fmt.Errorf("--%s takes '%s' (a tar stream on stdin)", fromTarFlag, stdinSpec)
	}
	name := args[0]
	if ok, err := hubMachineArg(args); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("--%s is the hub side of cp-in and takes a machine on this host, not %s", fromTarFlag, name)
	}
	dst, asDir := "~", true
	switch {
	case target != "":
		if len(args) > 1 {
			return fmt.Errorf("--%s with -t takes no DEST positional", fromTarFlag)
		}
		dst = target
	case len(args) > 1:
		dst, asDir = args[1], strings.HasSuffix(args[1], "/")
	}
	if dst == "" {
		return fmt.Errorf("destination must not be empty")
	}
	o.asDir = asDir
	_, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "devvm-cp-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	archive := filepath.Join(tmp, "payload.tar")
	if err := spoolArchive(archive, a.stdin()); err != nil {
		return fmt.Errorf("tar stream on stdin: %w", err)
	}
	entries, err := readArchive(archive, a.Stderr)
	if err != nil {
		return fmt.Errorf("tar stream on stdin: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("the tar stream holds nothing to copy")
	}
	if len(entries) > 1 && !o.asDir {
		return fmt.Errorf("copying several sources needs a destination directory")
	}
	bases := make([]string, len(entries))
	for i, e := range entries {
		bases[i] = e.name
	}
	// No copyTargets: the laptop prints the `->` lines.
	return copyArchive(ctx, b, name, archive, bases, dst, o, a.Stderr)
}

// spoolArchive copies a tar stream to path and refuses one that ended
// before its end-of-archive blocks (see runCopyInFromTar for why).
func spoolArchive(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return requireComplete(path)
}

// errIncompleteTar is a stream that ended before tar's end-of-archive
// blocks: a sender that died, on either side.
var errIncompleteTar = errors.New("ended before its end-of-archive blocks; nothing copied")

// countReader counts the bytes read through it.
type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// readTarStream reads one archive from r and returns how many bytes it
// spans, end-of-archive blocks included. Completeness is structural, not a
// sniff of the tail: archive/tar returns io.EOF after the two zero blocks,
// but also on a stream that simply ends at an entry boundary (or after a
// single zero block), so what is required is that the bytes consumed past
// the last entry's padded end cover both blocks — which they cannot when
// an entry's data merely ends in a kilobyte of NULs (a disk image, a core)
// and the stream is cut at its boundary. Every entry's data is drained so
// the count is the physical position, which keeps this true for a sparse
// entry, whose logical size is not its physical one.
func readTarStream(r io.Reader) (int64, error) {
	cr := &countReader{r: r}
	tr := tar.NewReader(cr)
	var dataEnd int64 // physical offset where the last entry's data ended
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return 0, err
		}
		dataEnd = cr.n
	}
	paddedEnd := dataEnd + (512-dataEnd%512)%512
	if cr.n-paddedEnd < 2*512 {
		return 0, errIncompleteTar
	}
	return cr.n, nil
}

// requireComplete errors unless the tar at path is a complete archive
// (readTarStream) followed by nothing but zeros: GNU and busybox tar pad
// the archive out to a 10 KiB record, which is the one thing that may
// legitimately follow the end blocks; anything else is not one archive.
func requireComplete(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	consumed, err := readTarStream(f)
	if err != nil {
		return err
	}
	if _, err := f.Seek(consumed, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := f.Read(buf)
		if bytes.ContainsFunc(buf[:n], func(r rune) bool { return r != 0 }) {
			return fmt.Errorf("trailing bytes after the end of the archive; nothing copied")
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// runCopyOutHub is cp-out for HUB/NAME: the same checks and extraction as a
// local cp-out (copyOutWith), fetching each source's archive from the hub's
// hidden form instead of a guest exec. Everything the local path refuses —
// a crafted archive, an overwrite without -f — is refused here the same way,
// because the bytes go through readArchive and extractArchive unchanged.
func (a *App) runCopyOutHub(ctx context.Context, name string, srcs []string, dst string, o copyOpts) error {
	m, p, err := a.resolveProxy(name)
	if err != nil {
		return err
	}
	fetch := func(ctx context.Context, src, archive string) error {
		return a.fetchHubTar(ctx, m, p, hubCopyOutArgv(m.HubMachineName(), src, o), archive)
	}
	return copyOutWith(ctx, name, srcs, dst, o, a.Stderr, fetch)
}

// hubCopyOutArgv is the hub-side command line for one cp-out source:
// `cp-out NAME SRC --to-tar - [-r]`. -f is not forwarded: it governs
// overwriting on the laptop, and the hub-side form has no destination. A
// SRC that starts with `-` goes after `--`, behind the flags.
func hubCopyOutArgv(machine, src string, o copyOpts) []string {
	argv := []string{"cp-out", machine}
	dashed := strings.HasPrefix(src, "-")
	if !dashed {
		argv = append(argv, src)
	}
	argv = append(argv, "--"+toTarFlag, stdinSpec)
	if o.recursive {
		argv = append(argv, "-r")
	}
	if dashed {
		argv = append(argv, "--", src)
	}
	return argv
}

// fetchHubTar runs one proxied `cp-out … --to-tar -` and writes the archive
// it streams to path. The ssh runs in a goroutine with an io.Pipe as its
// stdout, closed (with ssh's exit, if any) once the run returns — exec
// never closes a caller's writer — while this goroutine reads the stream to
// its end.
//
// Which error the user sees: the hub's exit status when the stream ran to
// its end and the hub exited non-zero — a refusal there (no such file, a
// directory without -r) ends the stream with no marker, and the hub
// already printed why. Otherwise the reader's own diagnosis. When the
// reader gives up mid-stream (no marker within the limit, a bad archive)
// the hub is given one more limit's worth of output to finish on its own,
// so a refusal that was about to arrive still counts; a hub that keeps
// sending is cut off — closing our end fails ssh's next write — and
// whatever that does to ssh's exit (a signal, EPIPE) is a consequence of
// the cut, never the diagnosis.
func (a *App) fetchHubTar(ctx context.Context, m *config.Machine, p backend.Proxier, argv []string, path string) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		eo := backend.ExecOpts{BatchMode: true, Stdin: strings.NewReader(""), Stdout: pw, Stderr: a.Stderr}
		err := p.Proxy(ctx, eo, argv...)
		pw.CloseWithError(err)
		done <- err
	}()
	readErr := receiveTar(pr, path)
	finished := readErr == nil // receiveTar drains to EOF on success
	if readErr != nil {
		n, _ := io.CopyN(io.Discard, pr, tarMarkerLimit)
		finished = n < tarMarkerLimit
	}
	pr.Close()
	err := <-done
	var ee *exec.ExitError
	if readErr != nil && !(finished && errors.As(err, &ee) && ee.ExitCode() > 0) {
		return readErr
	}
	if err != nil {
		return a.exitStatus(m, err)
	}
	return nil
}

// receiveTar reads a `cp-out --to-tar -` stream from r into path: whatever
// precedes the marker line is discarded, the archive is copied up to and
// including tar's end-of-archive blocks, and whatever follows is drained
// and ignored. The tar's own end bounds the archive, not the process's
// exit, so a login shell that prints after the command too (zsh runs
// .zlogout for `zsh -lc`) cannot append to it. readTarStream over a
// TeeReader reads exactly the archive's bytes — archive/tar never seeks or
// reads ahead on a plain reader — and refuses a stream that ends before
// the end blocks, so the file holds one complete archive or nothing.
func receiveTar(r io.Reader, path string) error {
	br := bufio.NewReader(r)
	if err := skipToMarker(br); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := readTarStream(io.TeeReader(br, f)); err != nil {
		f.Close()
		return fmt.Errorf("tar stream from the hub: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, br)
	return nil
}

// skipToMarker discards lines until the marker line, refusing a stream
// that ends first or that exceeds tarMarkerLimit without it. The reader is
// session.SkipToMarker, shared with the hub forwards' `__session` stream.
func skipToMarker(br *bufio.Reader) error {
	switch err := session.SkipToMarker(br, tarMarker, tarMarkerLimit); {
	case errors.Is(err, session.ErrMarkerMissing):
		return fmt.Errorf("the hub sent no tar stream (no %q line before the end of output)", tarMarker)
	case errors.Is(err, session.ErrMarkerLimit):
		return fmt.Errorf("no %q line in the first %d bytes from the hub", tarMarker, tarMarkerLimit)
	default:
		return err
	}
}

// runCopyOutToTar is the hub side of cp-out: one source, fetched from the
// machine into a temp file exactly as a local cp-out does, then written to
// stdout behind the marker line. The download completes before the marker
// is printed, so a refusal (missing SRC, a directory without -r) reaches
// the laptop as stderr and a non-zero exit with no marker, never as a
// marker followed by a broken stream. The laptop validates and extracts;
// nothing is checked here beyond what downloadArchive already does.
func (a *App) runCopyOutToTar(ctx context.Context, spec string, args []string, target string, o copyOpts) error {
	if spec != stdinSpec {
		return fmt.Errorf("--%s takes '%s' (a tar stream on stdout)", toTarFlag, stdinSpec)
	}
	if target != "" || len(args) != 2 {
		return fmt.Errorf("--%s takes exactly NAME SOURCE", toTarFlag)
	}
	name, src := args[0], args[1]
	if ok, err := hubMachineArg(args); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("--%s is the hub side of cp-out and takes a machine on this host, not %s", toTarFlag, name)
	}
	if src == "" {
		return fmt.Errorf("source must not be empty")
	}
	_, b, err := a.resolveLive(name)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "devvm-cp-out-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	archive := filepath.Join(tmp, "payload.tar")
	if err := downloadArchive(ctx, b, name, src, archive, o.recursive, a.Stderr); err != nil {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.WriteString(a.Stdout, tarMarker+"\n"); err != nil {
		return err
	}
	_, err = io.Copy(a.Stdout, f)
	return err
}
