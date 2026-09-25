package cli

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

// testMainEnv makes the test binary run the real CLI instead of the tests.
// TestHubCopyRoundTrip installs it as the hub's `devvm`, so the hidden
// hub-side forms under test are this very build re-exec'ed, not a script
// imitating them.
const testMainEnv = "DEVVM_TEST_MAIN"

func TestMain(m *testing.M) {
	// session.Dial re-execs os.Executable() as `__daemon NAME`, which under
	// `go test` is this binary: without this, a test that reaches a Dial
	// (ports on a hub machine, start HUB/NAME) would re-run the whole suite
	// as the "daemon", which reaches the same Dial again: a fork bomb
	// (roadmap Lessons). Refuse, so Dial fails fast; tests that want a
	// daemon serve one on the socket first (serveOwnerDaemon).
	if len(os.Args) > 1 && os.Args[1] == "__daemon" {
		fmt.Fprintln(os.Stderr, "devvm: the cli test binary runs no forward daemon")
		os.Exit(1)
	}
	if os.Getenv(testMainEnv) == "1" {
		os.Exit(Execute())
	}
	os.Exit(m.Run())
}

const marker = tarMarker + "\n"

// hubTarStream is a hub-side devvm script that writes $FAKE_STREAM's bytes
// to stdout behind a login banner and a trailing line, exiting
// FAKE_DEVVM_EXIT: the shape of a real `cp-out --to-tar -` run under a
// chatty login shell, with the archive under the test's control.
const hubTarStream = `[ -n "$DEVVM_NO_SUBSCRIBE" ] || { echo 'devvm ran without DEVVM_NO_SUBSCRIBE' >&2; exit 99; }` + "\n" +
	"echo 'Welcome to the hub'\n" +
	"cat \"$FAKE_STREAM\"\n" +
	"echo 'bye from .zlogout'\n" +
	"exit ${FAKE_DEVVM_EXIT:-0}\n"

// The laptop-side loops: the exact ssh argv for cp-in and cp-out against
// HUB/NAME — never -t, even with a terminal on both fds (the stream would
// not survive a pty), BatchMode, the login-shell wrapper with
// DEVVM_NO_SUBSCRIBE=1 — and the hub-side argv that reaches devvm: the
// hidden form, -r/-f forwarded, -t carrying the laptop's directory
// decision, a dashed DEST or SRC behind `--`. The `->` lines print on the
// laptop; the hub's stdout is discarded for cp-in.
func TestHubCopyArgv(t *testing.T) {
	a := newTestApp(t)
	sshLog, argvLog := fakeHub(t, a)
	pinTTY(t, true)
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(dir, "proj")
	writeTree(t, proj, "a", "sub/b")
	out := filepath.Join(dir, "out")
	for _, tc := range []struct {
		argv, want []string
		wantErr    string
		wantLine   string // on the laptop's stderr (cp-in only: the fake hub sends no stream back)
	}{
		{argv: []string{"cp-in", "h/web", f}, want: []string{"cp-in", "web", "--from-tar", "-", "-t", "~"}, wantLine: "f -> h/web:~/\n"},
		{argv: []string{"cp-in", "-r", "-f", "h/web", proj, "/home/dev/x"}, want: []string{"cp-in", "web", "--from-tar", "-", "-r", "-f", "/home/dev/x"}, wantLine: "proj -> h/web:/home/dev/x\n"},
		{argv: []string{"cp-in", "h/web", f, "docs/"}, want: []string{"cp-in", "web", "--from-tar", "-", "-t", "docs/"}, wantLine: "f -> h/web:docs/\n"},
		{argv: []string{"cp-in", "h/web", "-t", "inbox", f}, want: []string{"cp-in", "web", "--from-tar", "-", "-t", "inbox"}, wantLine: "f -> h/web:inbox/\n"},
		{argv: []string{"cp-in", "h/web", "--", f, "-weird"}, want: []string{"cp-in", "web", "--from-tar", "-", "--", "-weird"}, wantLine: "f -> h/web:-weird\n"},
		{argv: []string{"cp-out", "h/web", "notes.txt", out + "/"}, want: []string{"cp-out", "web", "notes.txt", "--to-tar", "-"}, wantErr: "no tar stream"},
		{argv: []string{"cp-out", "-r", "-f", "h/web", "proj", out + "/"}, want: []string{"cp-out", "web", "proj", "--to-tar", "-", "-r"}, wantErr: "no tar stream"},
		{argv: []string{"cp-out", "h/web", "--", "-weird", out + "/"}, want: []string{"cp-out", "web", "--to-tar", "-", "--", "-weird"}, wantErr: "no tar stream"},
	} {
		os.Remove(sshLog)
		os.Remove(argvLog)
		err := runTree(t, a, tc.argv...)
		if tc.wantErr == "" && err != nil {
			t.Errorf("%v: %v\n%s", tc.argv, err, a.Stderr)
		} else if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%v: err = %v, want %q", tc.argv, err, tc.wantErr)
		}
		lines := sshLines(t, sshLog)
		if len(lines) != 1 || lines[0] != wantSSH(a, false, tc.want...) {
			t.Errorf("%v: ssh argv =\n%s\nwant\n%s", tc.argv, strings.Join(lines, "\n"), wantSSH(a, false, tc.want...))
		}
		if got := devvmCalls(t, argvLog); len(got) != 1 || !slices.Equal(got[0], tc.want) {
			t.Errorf("%v: devvm on the hub got %q, want %q", tc.argv, got, tc.want)
		}
		if tc.wantLine != "" {
			if got := a.Stderr.(*bytes.Buffer).String(); got != tc.wantLine {
				t.Errorf("%v: laptop stderr = %q, want %q", tc.argv, got, tc.wantLine)
			}
			if got := a.Stdout.(*bytes.Buffer).String(); got != "" {
				t.Errorf("%v: the hub's stdout reached the laptop: %q", tc.argv, got)
			}
		}
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("a cp-out with no stream created its destination")
	}
	// The hidden forms are the hub's side only: a HUB/NAME is refused, and so
	// is anything but `-`, before any dial.
	os.Remove(sshLog)
	for _, argv := range [][]string{
		{"cp-in", "h/web", "--from-tar", "-"}, {"cp-in", "web", "--from-tar", "x"},
		{"cp-out", "h/web", "f", "--to-tar", "-"}, {"cp-out", "web", "f", "--to-tar", "x"},
		{"cp-out", "web", "f", "g", "--to-tar", "-"},
	} {
		if err := runTree(t, a, argv...); err == nil {
			t.Errorf("%v: want an error", argv)
		}
	}
	for _, argv := range [][]string{{"cp-in", "web", "--from-tar="}, {"cp-out", "web", "f", "--to-tar="}} {
		if err := runTree(t, a, argv...); err == nil || !strings.Contains(err.Error(), "takes '-'") {
			t.Errorf("%v: err = %v, want the takes '-' refusal, not the normal path", argv, err)
		}
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Error("a refused hidden form dialed the hub")
	}
	// Hidden from --help.
	if err := runTree(t, a, "cp-in", "--help"); err != nil {
		t.Fatal(err)
	}
	if help := a.Stdout.(*bytes.Buffer).String(); strings.Contains(help, fromTarFlag) {
		t.Errorf("--%s shows in help:\n%s", fromTarFlag, help)
	}
}

// The marker-line reader: junk before the marker (a banner, CRLF, a line
// longer than bufio's buffer, binary, near-misses of the marker) is
// discarded, the archive is exactly the tar's bytes however much follows
// it, a stream with no marker is refused, and a stream cut before the
// end-of-archive blocks — at an entry boundary, where archive/tar alone
// would read a shorter valid archive — is refused too.
func TestReceiveTar(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, "proj/a", "proj/sub/b")
	archive := filepath.Join(t.TempDir(), "want.tar")
	if err := writeArchive(archive, []string{filepath.Join(src, "proj")}, io.Discard); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	junk := "Welcome!\r\n" + strings.Repeat("x", 10000) + "\n\x00\xff binary \n" + tarMarker + " not quite\n " + marker
	for _, tc := range []struct {
		name    string
		stream  string
		wantErr string
	}{
		{name: "clean", stream: marker + string(want)},
		{name: "junk before and after", stream: junk + marker + string(want) + "bye from .zlogout\n"},
		{name: "no marker", stream: junk + string(want), wantErr: "no tar stream"},
		{name: "empty", stream: "", wantErr: "no tar stream"},
		{name: "too much junk", stream: strings.Repeat("y", tarMarkerLimit+1) + "\n" + marker + string(want), wantErr: "first"},
		{name: "cut at an entry boundary", stream: marker + string(want[:len(want)-2*512]), wantErr: "end-of-archive"},
		{name: "cut mid-entry", stream: marker + string(want[:600]), wantErr: "tar stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "got.tar")
			err := receiveTar(strings.NewReader(tc.stream), path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("archive differs: got %d bytes, want %d", len(got), len(want))
			}
		})
	}
}

// cp-out from a hub goes through the same guards as a local one: a hostile
// archive behind the marker is refused with nothing written, and so is a
// stream with no marker or one cut short; a hub-side refusal (non-zero
// exit, no marker) is the hub's exit status. The honest case extracts what
// the hub sent, banner and trailing chatter notwithstanding.
func TestCopyOutHubGuards(t *testing.T) {
	a := newTestApp(t)
	bin := t.TempDir()
	_, _ = fakeHubWith(t, a, bin, hubTarStream)
	pinTTY(t, false)
	outside := t.TempDir()
	src := t.TempDir()
	writeTree(t, src, "proj/a", "proj/sub/b")
	honest := filepath.Join(t.TempDir(), "honest.tar")
	if err := writeArchive(honest, []string{filepath.Join(src, "proj")}, io.Discard); err != nil {
		t.Fatal(err)
	}
	stream := func(t *testing.T, parts ...string) string {
		path := filepath.Join(t.TempDir(), "stream")
		if err := os.WriteFile(path, []byte(strings.Join(parts, "")), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	fileOf := func(t *testing.T, path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	dotdot := craftArchive(t, tarEntry{name: "proj/../../x", typ: tar.TypeReg, body: "x"})
	through := craftArchive(t,
		tarEntry{name: "proj/", typ: tar.TypeDir},
		tarEntry{name: "proj/link", typ: tar.TypeSymlink, link: outside},
		tarEntry{name: "proj/link/authorized_keys", typ: tar.TypeReg, body: "evil"})
	honestTar := fileOf(t, honest)
	for _, tc := range []struct {
		name    string
		stream  string
		exit    string
		wantErr string
		code    int // a proxyExit with this code
	}{
		{name: "honest", stream: marker + honestTar},
		{name: "dotdot after the marker", stream: marker + fileOf(t, dotdot), wantErr: "refusing archive entry"},
		{name: "write through a symlink", stream: marker + fileOf(t, through), wantErr: "refusing to write through symlink"},
		{name: "no marker", stream: honestTar, wantErr: "no tar stream"},
		// With chatter after it, the cut is an unexpected EOF to archive/tar;
		// alone it would be the end-of-archive check (TestReceiveTar).
		{name: "cut short", stream: marker + honestTar[:len(honestTar)-1024], wantErr: "tar stream"},
		{name: "hub refused", stream: "", exit: "1", code: 1},
		// The reader gives up at the limit and cuts the hub off; the user
		// must see the reader's reason, not what the cut did to ssh.
		{name: "endless junk", stream: strings.Repeat("y\n", 1<<20), wantErr: "line in the first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FAKE_STREAM", stream(t, tc.stream))
			t.Setenv("FAKE_DEVVM_EXIT", tc.exit)
			dst := filepath.Join(t.TempDir(), "out")
			err := runTree(t, a, "cp-out", "-r", "h/web", "proj", dst+"/")
			switch {
			case tc.code != 0:
				var pe *proxyExit
				if !errors.As(err, &pe) || pe.code != tc.code {
					t.Fatalf("err = %v, want proxyExit %d", err, tc.code)
				}
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("%v\n%s", err, a.Stderr)
			}
			if tc.wantErr != "" || tc.code != 0 {
				// extractArchive refuses at the offending entry (the symlink
				// case leaves proj/ and the link, as locally); what matters
				// is that no file was written through it or outside.
				if _, err := os.Lstat(filepath.Join(dst, "proj", "a")); !os.IsNotExist(err) {
					t.Error("a refused cp-out extracted proj/a")
				}
				if entries, _ := os.ReadDir(outside); len(entries) > 0 {
					t.Errorf("wrote outside the destination: %v", entries)
				}
				return
			}
			compareTrees(t, filepath.Join(src, "proj"), filepath.Join(dst, "proj"))
			if got := a.Stderr.(*bytes.Buffer).String(); got != "h/web:proj -> "+dst+"/\n" {
				t.Errorf("laptop stderr = %q", got)
			}
			if got := a.Stdout.(*bytes.Buffer).String(); got != "" {
				t.Errorf("the hub's stdout reached the laptop's: %q", got)
			}
		})
	}
}

// End to end: the laptop's cp-in and cp-out against h/web, through a fake
// ssh whose "hub" runs this build's real hidden forms (the test binary
// re-exec'ed, see TestMain) against a hub-registered remote machine `web`
// whose ssh and scp are the same fakes, so its guest is a temp home on this
// host. Files and a directory tree with a symlink round-trip byte for byte;
// the second cp-in without -f is refused on the hub with the hub's status
// and message; a missing source is the hub's error; a login banner before
// (and chatter after) the command changes nothing; every ssh runs without
// -t; the `->` lines appear once, from the laptop, with the hub's stderr
// on the same buffer to prove the hub printed none; and a stream that ends
// early leaves nothing on the hub.
func TestHubCopyRoundTrip(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t)
	hubCfg := t.TempDir()
	bin := t.TempDir()
	// The hub's `web` is a remote box reached by the same fake ssh, and scp
	// (its Copy) is `cp` to the path after the colon.
	scp := "#!/bin/sh\n" + `eval "src=\${$(($#-1))}"; eval "dst=\${$#}"` + "\n" + `exec cp -- "$src" "${dst#*:}"` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "scp"), []byte(scp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.NewRemote("web", config.BackendRemoteUnmanaged, "dev@web.example").Save(hubCfg); err != nil {
		t.Fatal(err)
	}
	devvm := `[ -n "$DEVVM_NO_SUBSCRIBE" ] || { echo 'devvm ran without DEVVM_NO_SUBSCRIBE' >&2; exit 99; }` + "\n" +
		testMainEnv + "=1 exec " + sq(exe) + " --config-dir " + sq(hubCfg) + " \"$@\"\n"
	sshLog, home := fakeHubWith(t, a, bin, devvm)
	pinTTY(t, true) // and still no -t anywhere
	guest := func(rel string) string { return filepath.Join(home, rel) }

	src := t.TempDir()
	f := filepath.Join(src, "f")
	if err := os.WriteFile(f, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(src, "proj")
	writeTree(t, proj, "a", "sub/b")
	if err := os.Symlink("a", filepath.Join(proj, "l")); err != nil {
		t.Fatal(err)
	}
	run := func(argv ...string) error {
		t.Helper()
		return runTree(t, a, argv...)
	}
	mustRun := func(argv ...string) {
		t.Helper()
		if err := run(argv...); err != nil {
			t.Fatalf("%v: %v\n%s", argv, err, a.Stderr)
		}
	}
	wantStderr := func(argv []string, want string) {
		t.Helper()
		if got := a.Stderr.(*bytes.Buffer).String(); got != want {
			t.Errorf("%v: stderr = %q, want %q (the hub side must print no -> lines)", argv, got, want)
		}
		if got := a.Stdout.(*bytes.Buffer).String(); got != "" {
			t.Errorf("%v: stdout = %q, want nothing", argv, got)
		}
	}

	// cp-in: a file, then a tree with -r.
	argv := []string{"cp-in", "h/web", f, "inbox/"}
	mustRun(argv...)
	wantStderr(argv, "f -> h/web:inbox/\n")
	if got, err := os.ReadFile(guest("inbox/f")); err != nil || string(got) != payload {
		t.Fatalf("inbox/f on the hub machine = %q, %v", got, err)
	}
	argv = []string{"cp-in", "-r", "h/web", proj, "inbox/"}
	mustRun(argv...)
	wantStderr(argv, "proj -> h/web:inbox/\n")
	compareTrees(t, proj, guest("inbox/proj"))

	// Exists: refused on the hub without -f, its message and status here.
	if err := os.WriteFile(f, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	argv = []string{"cp-in", "h/web", f, "inbox/"}
	err = run(argv...)
	var pe *proxyExit
	if !errors.As(err, &pe) || pe.code != 1 {
		t.Fatalf("%v: err = %v, want the hub's exit 1", argv, err)
	}
	if got := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(got, "refusing to overwrite (use -f)") || strings.Contains(got, "->") {
		t.Errorf("%v: stderr = %q", argv, got)
	}
	if got, _ := os.ReadFile(guest("inbox/f")); string(got) != payload {
		t.Errorf("a refused cp-in changed inbox/f: %q", got)
	}
	mustRun("cp-in", "-f", "h/web", f, "inbox/")
	if got, _ := os.ReadFile(guest("inbox/f")); string(got) != "v2" {
		t.Errorf("cp-in -f did not overwrite: %q", got)
	}

	// cp-out: the file and the tree come back byte for byte.
	out := filepath.Join(t.TempDir(), "out")
	argv = []string{"cp-out", "h/web", "inbox/f", out + "/"}
	mustRun(argv...)
	wantStderr(argv, "h/web:inbox/f -> "+out+"/\n")
	if got, err := os.ReadFile(filepath.Join(out, "f")); err != nil || string(got) != "v2" {
		t.Fatalf("out/f = %q, %v", got, err)
	}
	mustRun("cp-out", "-r", "h/web", "inbox/proj", out+"/")
	compareTrees(t, proj, filepath.Join(out, "proj"))
	// Exists locally: refused on the laptop, before anything is written.
	if err := run("cp-out", "h/web", "inbox/f", out+"/"); err == nil || !strings.Contains(err.Error(), "refusing to overwrite (use -f)") {
		t.Errorf("second cp-out: err = %v", err)
	}
	mustRun("cp-out", "-f", "h/web", "inbox/f", out+"/")
	// A missing source is the hub's refusal: its message, its status, no marker.
	argv = []string{"cp-out", "h/web", "inbox/missing", out + "/"}
	err = run(argv...)
	if !errors.As(err, &pe) || pe.code != 1 {
		t.Fatalf("%v: err = %v, want the hub's exit 1", argv, err)
	}
	if got := a.Stderr.(*bytes.Buffer).String(); !strings.Contains(got, "missing") {
		t.Errorf("%v: stderr = %q", argv, got)
	}
	if _, err := os.Lstat(filepath.Join(out, "missing")); !os.IsNotExist(err) {
		t.Error("a refused cp-out wrote out/missing")
	}

	// A login shell that prints before every command: cp-out is still byte
	// for byte, cp-in still lands, and none of it reaches the laptop's stdout.
	// Appended to fakeHubWith's profile (which restates the whole PATH; see
	// its macOS note) rather than replacing it.
	profile := filepath.Join(home, ".profile")
	pf, err := os.OpenFile(profile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pf.WriteString("echo hello from .profile\n"); err != nil {
		t.Fatal(err)
	}
	if err := pf.Close(); err != nil {
		t.Fatal(err)
	}
	out2 := filepath.Join(t.TempDir(), "out2")
	argv = []string{"cp-out", "h/web", "inbox/f", out2 + "/"}
	mustRun(argv...)
	wantStderr(argv, "h/web:inbox/f -> "+out2+"/\n")
	if got, _ := os.ReadFile(filepath.Join(out2, "f")); string(got) != "v2" {
		t.Errorf("cp-out behind a banner: %q", got)
	}
	argv = []string{"cp-in", "h/web", f, "inbox2/"}
	mustRun(argv...)
	wantStderr(argv, "f -> h/web:inbox2/\n")
	if got, _ := os.ReadFile(guest("inbox2/f")); string(got) != "v2" {
		t.Errorf("cp-in behind a banner: %q", got)
	}

	// Every laptop→hub ssh ran without a pty and in BatchMode.
	for _, line := range sshLines(t, sshLog) {
		if strings.Contains(line, " u@h.example ") && (strings.Contains(line, " -t ") || !strings.Contains(line, "BatchMode=yes")) {
			t.Errorf("a cp ssh got a pty or no BatchMode: %s", line)
		}
	}

	// The hub side with a stream that ends early (the laptop died): refused
	// before anything is uploaded, so the machine sees no staging and DEST
	// is not created. Run in-process as the hub's own devvm would.
	full := filepath.Join(t.TempDir(), "full.tar")
	if err := writeArchive(full, []string{f}, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	before := len(sshLines(t, sshLog))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH")) // the fake scp, as the login shell had it
	hub := &App{ConfigDir: hubCfg}
	// Cut at the trailer, and cut at an entry boundary behind a NUL-tailed
	// entry (the tail alone looks like a trailer): both refused before any
	// exec reaches the machine.
	var nulTail bytes.Buffer
	tw := tar.NewWriter(&nulTail)
	for _, body := range []string{"abc" + strings.Repeat("\x00", 4096), "x"} {
		if err := tw.WriteHeader(&tar.Header{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, body)
		tw.Flush()
	}
	boundary := nulTail.Len() - 512 - 512 // before the second entry's header and its one data block
	for _, cut := range [][]byte{data[:len(data)-1024], nulTail.Bytes()[:boundary]} {
		hub.Stdin = bytes.NewReader(cut)
		err = runTree(t, hub, "cp-in", "web", "--from-tar", "-", "-t", "inbox3/")
		if err == nil || !strings.Contains(err.Error(), "end-of-archive") {
			t.Fatalf("hub-side cp-in on a cut stream: err = %v", err)
		}
	}
	if _, err := os.Stat(guest("inbox3")); !os.IsNotExist(err) {
		t.Error("a cut stream created DEST on the machine")
	}
	if after := len(sshLines(t, sshLog)); after != before {
		t.Errorf("a cut stream reached the machine: %d exec(s)", after-before)
	}
	// The complete stream through the same in-process form lands, silently.
	hub.Stdin = bytes.NewReader(data)
	if err := runTree(t, hub, "cp-in", "web", "--from-tar", "-", "-t", "inbox3/"); err != nil {
		t.Fatalf("hub-side cp-in: %v\n%s", err, hub.Stderr)
	}
	if got, _ := os.ReadFile(guest("inbox3/f")); string(got) != "v2" {
		t.Errorf("hub-side cp-in: inbox3/f = %q", got)
	}
	if got := hub.Stdout.(*bytes.Buffer).String() + hub.Stderr.(*bytes.Buffer).String(); got != "" {
		t.Errorf("the hub-side form printed %q", got)
	}
	// And the hub-side cp-out form: marker first, then exactly the archive.
	hub.Stdin = strings.NewReader("")
	if err := runTree(t, hub, "cp-out", "web", "inbox3/f", "--to-tar", "-"); err != nil {
		t.Fatalf("hub-side cp-out: %v\n%s", err, hub.Stderr)
	}
	got := hub.Stdout.(*bytes.Buffer).String()
	if !strings.HasPrefix(got, marker) {
		t.Fatalf("hub-side cp-out stdout starts %q, want the marker", got[:min(len(got), 20)])
	}
	back := filepath.Join(t.TempDir(), "back.tar")
	if err := receiveTar(strings.NewReader(got), back); err != nil {
		t.Fatal(err)
	}
	entries, err := readArchive(back, io.Discard)
	if err != nil || len(entries) != 1 || entries[0].name != "f" {
		t.Errorf("hub-side cp-out archive: %+v, %v", entries, err)
	}
}

// Completeness is structural, not a sniff of the tail: an archive whose
// last entry's data ends in a run of NULs, cut at the boundary before its
// next entry, is refused on the cp-out path (receiveTar) and on the cp-in
// path (spoolArchive, which runCopyInFromTar runs before anything else);
// the same archive whole passes both, and so does one padded out to a GNU
// record with zeros. Anything else after the end blocks is refused.
func TestTarCompletenessIsStructural(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name, body string) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
		if err := tw.Flush(); err != nil { // pads the entry, so buf.Len() is the boundary
			t.Fatal(err)
		}
	}
	write("proj/blob", "abc"+strings.Repeat("\x00", 4096))
	cut := buf.Len()
	write("proj/b", "x")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	whole := buf.Bytes()
	truncated := whole[:cut]
	padded := append(append([]byte{}, whole...), make([]byte, 10240-len(whole)%10240)...)
	for _, tc := range []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{name: "whole", data: whole},
		{name: "record padded", data: padded},
		{name: "cut after the NUL-tailed entry", data: truncated, wantErr: "end-of-archive"},
		{name: "cut plus one zero block", data: append(append([]byte{}, truncated...), make([]byte, 512)...), wantErr: "end-of-archive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(what string, err error) {
				t.Helper()
				if tc.wantErr == "" && err != nil {
					t.Errorf("%s: %v", what, err)
				} else if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
					t.Errorf("%s: err = %v, want %q", what, err, tc.wantErr)
				}
			}
			check("cp-in (spoolArchive)", spoolArchive(filepath.Join(t.TempDir(), "in.tar"), bytes.NewReader(tc.data)))
			check("cp-out (receiveTar)", receiveTar(bytes.NewReader(append([]byte(marker), tc.data...)), filepath.Join(t.TempDir(), "out.tar")))
		})
	}
	trailing := filepath.Join(t.TempDir(), "trailing.tar")
	if err := spoolArchive(trailing, bytes.NewReader(append(append([]byte{}, whole...), "junk"...))); err == nil || !strings.Contains(err.Error(), "trailing bytes") {
		t.Errorf("bytes after the end blocks: err = %v", err)
	}
}

// compareTrees checks that got mirrors want: same entries, types, symlink
// targets, file contents and permission bits.
func compareTrees(t *testing.T, want, got string) {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.Walk(want, func(p string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(want, p)
		seen[rel] = true
		q := filepath.Join(got, rel)
		gi, err := os.Lstat(q)
		if err != nil {
			t.Errorf("%s: missing in copy: %v", rel, err)
			return nil
		}
		if gi.Mode().Type() != info.Mode().Type() || gi.Mode().Perm() != info.Mode().Perm() {
			t.Errorf("%s: mode %v, want %v", rel, gi.Mode(), info.Mode())
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			a, _ := os.Readlink(p)
			b, _ := os.Readlink(q)
			if a != b {
				t.Errorf("%s: link %q, want %q", rel, b, a)
			}
		case info.Mode().IsRegular():
			a, _ := os.ReadFile(p)
			b, _ := os.ReadFile(q)
			if !bytes.Equal(a, b) {
				t.Errorf("%s: content differs", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	filepath.Walk(got, func(p string, info fs.FileInfo, err error) error {
		if err == nil {
			if rel, _ := filepath.Rel(got, p); !seen[rel] {
				t.Errorf("%s: extra entry in copy", rel)
			}
		}
		return nil
	})
}
