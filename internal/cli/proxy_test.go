package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// fakeHub registers hub h (u@h.example) and puts a fake `ssh` on PATH that
// *runs* the remote command string through a real sh — exactly what sshd
// does with it — with a fake `devvm` reachable only through the login
// shell's ~/.profile, as on the hubs. The fake devvm appends its argv,
// NUL-separated, to argvLog, exits FAKE_DEVVM_EXIT (default 0), and fails
// loudly if DEVVM_NO_SUBSCRIBE is not set in its environment. sshLog is the
// fake ssh's own argv, one line per call.
func fakeHub(t *testing.T, a *App) (sshLog, argvLog string) {
	t.Helper()
	bin := t.TempDir()
	argvLog = filepath.Join(bin, "argv.log")
	devvm := `[ -n "$DEVVM_NO_SUBSCRIBE" ] || { echo 'devvm ran without DEVVM_NO_SUBSCRIBE' >&2; exit 99; }` + "\n" +
		"printf '%s\\0' \"$@\" >> " + argvLog + "; printf '\\1' >> " + argvLog + "\n" +
		"echo \"fake devvm: $*\"\n" +
		"exit ${FAKE_DEVVM_EXIT:-0}\n"
	t.Setenv("FAKE_DEVVM_EXIT", "")
	sshLog, _ = fakeHubWith(t, a, bin, devvm)
	return sshLog, argvLog
}

// fakeHubWith is fakeHub with the hub-side devvm's script body supplied
// (installed as bin/devvm; bin may hold other fakes the script needs). It
// returns the fake ssh's log and the hub user's home, whose ~/.profile is
// what puts bin on the login shell's PATH; a test that wants a login banner
// appends an echo to it.
func fakeHubWith(t *testing.T, a *App, bin, devvmBody string) (sshLog, home string) {
	t.Helper()
	home = t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "devvm"), []byte("#!/bin/sh\n"+devvmBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("ENV", "")
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "")
	sshLog = fakeSSH(t, `sh -c "$last"`)
	// The profile restates the whole PATH, not just `bin:$PATH`: on macOS
	// `sh -l` sources /etc/profile, whose path_helper puts the system dirs
	// first, so a hub-side devvm that itself runs ssh (cp against a remote
	// `web`) would find /usr/bin/ssh ahead of the fake installed by fakeSSH.
	profile := "PATH=" + sq(bin+":"+os.Getenv("PATH")) + "\n"
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}})
	return sshLog, home
}

// runTree runs argv through the real command tree, as main does.
func runTree(t *testing.T, a *App, argv ...string) error {
	t.Helper()
	a.Stdout, a.Stderr = new(bytes.Buffer), new(bytes.Buffer)
	root := a.rootCmd()
	root.SetArgs(argv)
	root.SetOut(a.Stdout.(*bytes.Buffer))
	root.SetErr(a.Stderr.(*bytes.Buffer))
	return root.Execute()
}

// devvmCalls returns every argv the fake devvm received, in order: one
// record per call, tokens NUL-separated, records separated by \x01.
func devvmCalls(t *testing.T, argvLog string) [][]string {
	t.Helper()
	data, err := os.ReadFile(argvLog)
	if err != nil {
		return nil
	}
	var calls [][]string
	for _, rec := range strings.Split(string(data), "\x01") {
		if rec == "" {
			continue
		}
		calls = append(calls, strings.Split(strings.TrimSuffix(rec, "\x00"), "\x00"))
	}
	return calls
}

// pinTTY fixes the proxy's terminal answer for one test (both fds).
func pinTTY(t *testing.T, tty bool) {
	t.Helper()
	pinTTYs(t, tty, tty)
}

func pinTTYs(t *testing.T, in, out bool) {
	t.Helper()
	origIn, origOut := stdinIsTTY, stdoutIsTTY
	stdinIsTTY = func() bool { return in }
	stdoutIsTTY = func() bool { return out }
	t.Cleanup(func() { stdinIsTTY, stdoutIsTTY = origIn, origOut })
}

func sq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// wantSSH renders the exact ssh argv line the proxy must produce for a
// devvm argv: sshBackend.base()'s ControlMaster flags, BatchMode iff no
// terminal, -t iff one, the hub's destination, then the login-shell wrapper
// around `env DEVVM_NO_SUBSCRIBE=1 devvm …`, every token single-quoted.
func wantSSH(a *App, tty bool, devvmArgv ...string) string {
	line := "-o ConnectTimeout=10 -o ControlMaster=auto -o ControlPath=" + filepath.Join(a.ConfigDir, "cm-%C") + " -o ControlPersist=60"
	if tty {
		line += " -t -o LogLevel=ERROR" // Quiet: no "Shared connection … closed." after a pty run
	} else {
		line += " -o BatchMode=yes -o RequestTTY=no"
	}
	return line + " u@h.example " + wantRemote(devvmArgv...)
}

// wantRemote is the remote half of a proxied ssh line: the login-shell
// wrapper around `env DEVVM_NO_SUBSCRIBE=1 devvm …`, every token quoted.
func wantRemote(devvmArgv ...string) string {
	inner := append([]string{"env", "DEVVM_NO_SUBSCRIBE=1", "devvm"}, devvmArgv...)
	q := make([]string, len(inner))
	for i, tok := range inner {
		q[i] = sq(tok)
	}
	return sq("sh") + " " + sq("-c") + " " + sq(`exec "${SHELL:-sh}" -lc "$0"`) + " " + sq(strings.Join(q, " "))
}

func sshLines(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// The argv builder, end to end: each proxied leaf's ssh line is exactly the
// rebuilt command, and the argv that reaches devvm on the hub — after ssh's
// shell, the wrapper's sh, and the login shell — is byte-identical to what
// was typed, HUB/ stripped from the first positional only, --config-dir
// never sent, only explicitly set leaf flags sent, `--` and everything after
// it untouched (spaces, quotes and $ included).
func TestProxyRebuildsArgv(t *testing.T) {
	a := newTestApp(t)
	sshLog, argvLog := fakeHub(t, a)
	pinTTY(t, false)
	cases := []struct {
		argv []string
		want []string
	}{
		// (--config-dir before `exec` is not a case: exec disables flag
		// parsing, so cobra hands it a root flag placed there as an argument,
		// for local machines too.)
		{[]string{"exec", "h/web", "--", "echo", "--config-dir", "x"},
			[]string{"exec", "web", "--", "echo", "--config-dir", "x"}},
		{[]string{"exec", "h/web", "sh", "-c", `echo "$HOME" it's 'a b' $X`, "h/web"},
			[]string{"exec", "web", "sh", "-c", `echo "$HOME" it's 'a b' $X`, "h/web"}},
		{[]string{"--config-dir", a.ConfigDir, "stop", "h/web"}, []string{"stop", "web"}},
		{[]string{"--config-dir", a.ConfigDir, "deprovision", "h/web", "--yes"}, []string{"deprovision", "web", "--yes"}},
		{[]string{"create", "h/api", "--backend", "smol", "--yes", "-m", "512", "-d", "1"},
			[]string{"create", "api", "--backend=smol", "--disk=1", "--memory=512", "--yes"}},
		{[]string{"repos", "add", "h/web", "--no-clone", "o/r", "https://x.example/a b.git"},
			[]string{"repos", "add", "web", "--no-clone", "o/r", "https://x.example/a b.git"}},
		{[]string{"repos", "rm", "h/web", "--", "-odd/name"}, []string{"repos", "rm", "web", "--", "-odd/name"}},
		{[]string{"keys", "rm", "h/web", "SHA256:abc"}, []string{"keys", "rm", "web", "SHA256:abc"}},
		{[]string{"bootstrap", "h/web"}, []string{"bootstrap", "web"}},
	}
	for _, tc := range cases {
		os.Remove(sshLog)
		os.Remove(argvLog)
		if err := runTree(t, a, tc.argv...); err != nil {
			t.Fatalf("%v: %v\n%s", tc.argv, err, a.Stderr)
		}
		lines := sshLines(t, sshLog)
		if len(lines) != 1 || lines[0] != wantSSH(a, false, tc.want...) {
			t.Errorf("%v: ssh argv =\n%s\nwant\n%s", tc.argv, strings.Join(lines, "\n"), wantSSH(a, false, tc.want...))
		}
		got := devvmCalls(t, argvLog)
		if len(got) != 1 || !slices.Equal(got[0], tc.want) {
			t.Errorf("%v: devvm on the hub got %q, want %q", tc.argv, got, tc.want)
		}
		if strings.Contains(strings.Join(lines, "\n"), a.ConfigDir+"'") {
			t.Errorf("%v: the laptop's config dir crossed the wire: %s", tc.argv, lines)
		}
		if !strings.Contains(a.Stdout.(*bytes.Buffer).String(), "fake devvm: ") {
			t.Errorf("%v: the hub's stdout did not reach the laptop: %q", tc.argv, a.Stdout)
		}
	}
	// State-changing verbs touch the local change marker; a read-only one
	// does not.
	os.Remove(config.ChangedPath(a.ConfigDir))
	if err := runTree(t, a, "repos", "list", "h/web"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(config.ChangedPath(a.ConfigDir)); err == nil {
		t.Error("repos list touched the change marker")
	}
	// start HUB/NAME proxies, then brings up this host's configured
	// forwards for the machine (its [machines.web] table: 3000) on this
	// host's daemon for h@web (hub.md §3).
	od := serveOwnerDaemon(t, a.ConfigDir, "h/web", map[string]session.Response{
		session.OpAdd:  {OK: true, Host: 3000},
		session.OpList: {OK: true, State: session.StateUp},
	}, false)
	os.Remove(argvLog)
	if err := runTree(t, a, "start", "h/web"); err != nil {
		t.Fatal(err)
	}
	if got := devvmCalls(t, argvLog); len(got) != 1 || !slices.Equal(got[0], []string{"start", "web"}) {
		t.Errorf("start h/web: devvm on the hub got %q", got)
	}
	if adds := od.requests(session.OpAdd); len(adds) != 1 || adds[0].Guest != 3000 || adds[0].Host != 3000 {
		t.Errorf("start h/web: laptop forwards = %+v, want one add of 3000", adds)
	}
	if _, err := os.Stat(config.ChangedPath(a.ConfigDir)); err != nil {
		t.Error("start did not touch the change marker")
	}
}

// -t iff the laptop's stdin *and* stdout are terminals: then ssh gets -t
// and no BatchMode; otherwise BatchMode and nothing piped. A redirected
// stdout (`exec h/web -- cat f > out`) must not get a remote pty.
func TestProxyTTYDecidesDashT(t *testing.T) {
	a := newTestApp(t)
	sshLog, _ := fakeHub(t, a)
	for _, tc := range []struct{ in, out, tty bool }{
		{true, true, true}, {true, false, false}, {false, true, false}, {false, false, false},
	} {
		os.Remove(sshLog)
		pinTTYs(t, tc.in, tc.out)
		if err := runTree(t, a, "stop", "h/web"); err != nil {
			t.Fatalf("in=%v out=%v: %v", tc.in, tc.out, err)
		}
		if lines := sshLines(t, sshLog); len(lines) != 1 || lines[0] != wantSSH(a, tc.tty, "stop", "web") {
			t.Errorf("in=%v out=%v: ssh argv = %q\nwant %q", tc.in, tc.out, lines, wantSSH(a, tc.tty, "stop", "web"))
		}
	}
	o := proxyExecOpts(false)
	if o.TTY || !o.BatchMode || o.Stdin == nil {
		t.Errorf("non-tty opts = %+v: want BatchMode and an empty stdin, no -t", o)
	}
	if o := proxyExecOpts(true); !o.TTY || o.BatchMode || o.Stdin != nil {
		t.Errorf("tty opts = %+v: want -t and the terminal's stdin", o)
	}
}

// keys add resolves its spec on the laptop and sends inline key lines, one
// proxied run per key, never a path; the bare form sends this host's
// ~/.ssh/id_*.pub.
func TestProxyKeysAddSendsInlineKeys(t *testing.T) {
	a := newTestApp(t)
	sshLog, argvLog := fakeHub(t, a)
	pinTTY(t, false)
	k1 := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl laptop key one"
	k2 := "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC1Dn1KEo9bFVqvtCcdXuTb2ZDvHxRq/8N0i3nqUw6t8x0N3dZzM1hSjkWxKmyO9yyy9nHR0dbb8XAP8PcHqgZYkw1x7B9gVw0FzL8sL0y0nS7Zb1n1iOZiW8xY9KfE5GsUqA1V2p9Hq0nY1mI5m5R8gQ9+8Wq5w0z1cHqDLwaxMQ== two"
	file := filepath.Join(t.TempDir(), "keys.pub")
	if err := os.WriteFile(file, []byte(k1+"\n\n"+k2+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "keys", "add", "h/web", file); err != nil {
		t.Fatalf("keys add FILE: %v\n%s", err, a.Stderr)
	}
	lines := sshLines(t, sshLog)
	want := []string{wantSSH(a, false, "keys", "add", "web", k1), wantSSH(a, false, "keys", "add", "web", k2)}
	if !slices.Equal(lines, want) {
		t.Errorf("keys add FILE ssh argv =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(strings.Join(lines, "\n"), file) {
		t.Error("the key file's path crossed the wire")
	}
	got := devvmCalls(t, argvLog)
	if len(got) != 2 || !slices.Equal(got[0], []string{"keys", "add", "web", k1}) || !slices.Equal(got[1], []string{"keys", "add", "web", k2}) {
		t.Errorf("devvm on the hub got %q", got)
	}

	// Bare form: the laptop's own id_*.pub (HOME is the fake hub's temp home).
	os.Remove(sshLog)
	os.Remove(argvLog)
	sshDir := filepath.Join(os.Getenv("HOME"), ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte(k1+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "keys", "add", "h/web"); err != nil {
		t.Fatalf("bare keys add: %v\n%s", err, a.Stderr)
	}
	if lines := sshLines(t, sshLog); len(lines) != 1 || lines[0] != wantSSH(a, false, "keys", "add", "web", k1) {
		t.Errorf("bare keys add ssh argv = %q", lines)
	}
	// A line with authorized_keys options rides whole, options first, and
	// the receiving isInlineKey (same code on the hub) still takes it.
	os.Remove(sshLog)
	opt := "no-port-forwarding,no-pty " + k1
	if err := os.WriteFile(file, []byte(opt+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "keys", "add", "h/web", file); err != nil {
		t.Fatalf("keys add with options: %v\n%s", err, a.Stderr)
	}
	if lines := sshLines(t, sshLog); len(lines) != 1 || lines[0] != wantSSH(a, false, "keys", "add", "web", opt) {
		t.Errorf("keys add with options ssh argv = %q", lines)
	}
	if got, err := resolvePubkeys([]string{opt}); err != nil || !slices.Equal(got, []string{opt}) {
		t.Errorf("the hub side would not take the options line: %q, %v", got, err)
	}
	// Inline keys pass through as typed (joined, like the local leaf).
	os.Remove(sshLog)
	if err := runTree(t, a, "keys", "add", "h/web", "ssh-ed25519", "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl", "inline"); err != nil {
		t.Fatalf("inline keys add: %v", err)
	}
	if lines := sshLines(t, sshLog); len(lines) != 1 || !strings.Contains(lines[0], sq("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl inline")) {
		t.Errorf("inline keys add ssh argv = %q", lines)
	}
	// With no key anywhere the laptop refuses before dialing.
	os.Remove(sshLog)
	os.Remove(filepath.Join(sshDir, "id_ed25519.pub"))
	if err := runTree(t, a, "keys", "add", "h/web"); err == nil || !strings.Contains(err.Error(), "id_*.pub") {
		t.Errorf("bare keys add with no keys: err = %v", err)
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Error("a refused keys add dialed the hub")
	}
}

// repos add with no REPO sends the laptop cwd's git origin, normalized as
// the local leaf would store it.
func TestProxyReposAddSendsLaptopOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	a := newTestApp(t)
	sshLog, _ := fakeHub(t, a)
	pinTTY(t, false)
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://github.com/me/app.git"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	if err := runTree(t, a, "repos", "add", "h/web"); err != nil {
		t.Fatalf("repos add: %v\n%s", err, a.Stderr)
	}
	if lines := sshLines(t, sshLog); len(lines) != 1 || lines[0] != wantSSH(a, false, "repos", "add", "web", "me/app") {
		t.Errorf("repos add ssh argv = %q", lines)
	}
	// Outside any work tree there is nothing to infer, and nothing is dialed.
	os.Remove(sshLog)
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "repos", "add", "h/web"); err == nil || !strings.Contains(err.Error(), "no git origin") {
		t.Errorf("repos add with no origin: err = %v", err)
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Error("repos add with nothing to send dialed the hub")
	}
}

// The remote command's exit status is the laptop's, silently (the hub's
// devvm printed its error); ssh's own 255 names the hub instead.
func TestProxyPropagatesExitStatus(t *testing.T) {
	a := newTestApp(t)
	_, _ = fakeHub(t, a)
	pinTTY(t, false)
	t.Setenv("FAKE_DEVVM_EXIT", "3")
	err := runTree(t, a, "exec", "h/web", "--", "false")
	var pe *proxyExit
	if !errors.As(err, &pe) || pe.code != 3 {
		t.Fatalf("err = %v, want proxyExit 3", err)
	}
	t.Setenv("FAKE_DEVVM_EXIT", "255")
	err = runTree(t, a, "stop", "h/web")
	if err == nil || !strings.Contains(err.Error(), "cannot reach hub 'h' (u@h.example)") {
		t.Errorf("255: err = %v", err)
	}
	if errors.As(err, &pe) {
		t.Error("ssh's 255 must not pass as the remote command's status")
	}
}

// attach/shell run with a terminal over the hub conf's transport; the
// --transport flag is consumed here and never forwarded.
func TestProxyInteractiveTransport(t *testing.T) {
	a := newTestApp(t)
	sshLog, argvLog := fakeHub(t, a)
	pinTTY(t, true)
	if err := runTree(t, a, "attach", "h/web"); err != nil {
		t.Fatalf("attach: %v\n%s", err, a.Stderr)
	}
	if lines := sshLines(t, sshLog); len(lines) != 1 || lines[0] != wantSSH(a, true, "attach", "web") {
		t.Errorf("attach ssh argv = %q\nwant %q", lines, wantSSH(a, true, "attach", "web"))
	}
	os.Remove(sshLog)
	if err := runTree(t, a, "shell", "h/web", "--transport", "ssh"); err != nil {
		t.Fatalf("shell: %v", err)
	}
	if lines := sshLines(t, sshLog); len(lines) != 1 || lines[0] != wantSSH(a, true, "shell", "web") {
		t.Errorf("shell --transport ssh argv = %q (the flag must not be forwarded)", lines)
	}
	if got := devvmCalls(t, argvLog); len(got) != 2 || !slices.Equal(got[1], []string{"shell", "web"}) {
		t.Errorf("devvm on the hub got %q", got)
	}
	if err := runTree(t, a, "attach", "h/web", "--transport", "nope"); err == nil {
		t.Error("an invalid transport must be refused on the laptop")
	}
	// A hub conf set to mosh still gets ssh for the proxied session (with a
	// notice on stderr; see hubBackend.ProxyInteractive), and --transport
	// mosh is accepted the same way rather than refused.
	h, _ := config.Load(a.ConfigDir, "h")
	h.Transport = config.TransportMosh
	if err := h.Save(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	os.Remove(sshLog)
	if err := runTree(t, a, "attach", "h/web"); err != nil {
		t.Fatalf("attach with a mosh hub conf: %v\n%s", err, a.Stderr)
	}
	if lines := sshLines(t, sshLog); len(lines) != 1 || lines[0] != wantSSH(a, true, "attach", "web") {
		t.Errorf("attach with a mosh hub conf: ssh argv = %q", lines)
	}
}

// The local-only leaves keep refusing hub machines without dialing: auth
// (step 8). cp proxies since step 4 (cp_hub_test.go), ports run here since
// step 6 (hubforwards_test.go).
func TestProxyLeavesLocalOnlyRefused(t *testing.T) {
	a := newTestApp(t)
	sshLog, _ := fakeHub(t, a)
	for _, argv := range [][]string{
		{"auth", "h/web"},
	} {
		err := runTree(t, a, argv...)
		if !errors.Is(err, backend.ErrHubProxy) {
			t.Errorf("%v: err = %v, want ErrHubProxy", argv, err)
		}
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		data, _ := os.ReadFile(sshLog)
		t.Errorf("a refused leaf dialed the hub:\n%s", data)
	}
	// An unknown hub, or a malformed reference, fails on the conf alone.
	for _, argv := range [][]string{{"stop", "nope/web"}, {"exec", "bad name/web", "true"}, {"create", "h/bad name", "--yes"}} {
		if err := runTree(t, a, argv...); err == nil {
			t.Errorf("%v: want an error", argv)
		}
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Error("a bad reference dialed the hub")
	}
}

func TestIsInlineKey(t *testing.T) {
	for in, want := range map[string]bool{
		"ssh-ed25519 AAAA c":                  true,
		"ecdsa-sha2-nistp256 AAAA":            true,
		"sk-ssh-ed25519@openssh.com AAAA":     true,
		"no-pty,command=\"x\" ssh-rsa AAAA c": true,
		"restrict ssh-ed25519 AAAA":           true,
		"/home/me/.ssh/id_ed25519.pub":        false,
		"--from-github":                       false,
		"alice":                               false,
		"opt1 opt2 ssh-ed25519 AAAA":          false, // options are one comma-joined token
		"":                                    false,
	} {
		if got := isInlineKey(in); got != want {
			t.Errorf("isInlineKey(%q) = %v, want %v", in, got, want)
		}
	}
}

// hubArgv never lets a malformed reference through as an empty machine
// name; it reports it (dispatch validated earlier, so this is belt and
// braces).
func TestHubArgvRejectsMalformedReference(t *testing.T) {
	a := newTestApp(t)
	root := a.rootCmd()
	cmd, _, err := root.Find([]string{"stop"})
	if err != nil {
		t.Fatal(err)
	}
	if argv, err := hubArgv(cmd, []string{"bad name/web"}); err == nil {
		t.Errorf("hubArgv(bad name/web) = %q, want an error", argv)
	}
	if argv, err := hubArgv(cmd, []string{"h/web"}); err != nil || !slices.Equal(argv, []string{"stop", "web"}) {
		t.Errorf("hubArgv(h/web) = %q, %v", argv, err)
	}
}

// A bare `repos add HUB/NAME` on a terminal goes through the local leaf's
// prompt (which needs /dev/tty; none here, so its error), never a silent
// substitution, and dials nothing.
func TestProxyReposAddPromptsOnTTY(t *testing.T) {
	a := newTestApp(t)
	sshLog, _ := fakeHub(t, a)
	pinTTY(t, true)
	err := runTree(t, a, "repos", "add", "h/web")
	if err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Errorf("err = %v, want the prompt's no-terminal error", err)
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Error("an unanswered prompt dialed the hub")
	}
}

// stop and deprovision HUB/NAME stop this host's daemon for the machine
// once the hub has done its part, as a local stop does: left running it
// would redial a stopped VM's `__session` for good. A hub-side failure
// leaves the daemon alone.
func TestHubStopStopsLaptopDaemon(t *testing.T) {
	a := newTestApp(t)
	fakeHub(t, a)
	for _, argv := range [][]string{{"stop", "h/web"}, {"deprovision", "h/web", "--yes"}} {
		od := serveOwnerDaemon(t, a.ConfigDir, "h/web", nil, true)
		t.Setenv("FAKE_DEVVM_EXIT", "1")
		var pe *proxyExit
		if err := runTree(t, a, argv...); !errors.As(err, &pe) {
			t.Fatalf("%v with the hub failing: err = %v, want its exit status", argv, err)
		}
		if n := len(od.requests(session.OpStop)); n != 0 {
			t.Errorf("%v failed on the hub but stopped this host's daemon", argv)
		}
		t.Setenv("FAKE_DEVVM_EXIT", "")
		if err := runTree(t, a, argv...); err != nil {
			t.Fatalf("%v: %v", argv, err)
		}
		if n := len(od.requests(session.OpStop)); n != 1 {
			t.Errorf("%v: this host's daemon got %d stops, want 1", argv, n)
		}
	}
}

// delete HUB/NAME reaps this host's run files for it (log, a stale socket),
// and only its own. The lock stays: the hub conf still resolves HUB/NAME,
// so a starter could race an unlinked lock (session.RemoveLock).
func TestDeleteHubMachineReapsRunFiles(t *testing.T) {
	a := newTestApp(t)
	a.Stdout, a.Stderr = new(bytes.Buffer), new(bytes.Buffer)
	pinTTY(t, false)
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "")
	writeHub(t, a, "h", nil)
	fakeSSH(t, "exit 0")
	if err := config.EnsureRuntimeDir(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	run := config.RuntimeDir(a.ConfigDir)
	for _, f := range []string{"h@web.lock", "h@web.log", "h@web.sock", "h@api.lock"} {
		if err := os.WriteFile(filepath.Join(run, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.runDelete("h/web", false); err != nil {
		t.Fatalf("delete h/web: %v", err)
	}
	for _, f := range []string{"h@web.log", "h@web.sock"} {
		if _, err := os.Stat(filepath.Join(run, f)); err == nil {
			t.Errorf("%s survived delete h/web", f)
		}
	}
	for _, f := range []string{"h@web.lock", "h@api.lock"} {
		if _, err := os.Stat(filepath.Join(run, f)); err != nil {
			t.Errorf("delete h/web removed %s", f)
		}
	}
}
