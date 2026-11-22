package backend

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

// fakeSmolvm puts a smolvm on PATH that logs each call's argv (one line per
// call) and answers `tmux has-session` as failing until a detached keeper
// exec (`machine exec -d`) has run, when hasSession is false at the start.
func fakeSmolvm(t *testing.T, hasSession bool) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "smolvm.log")
	state := filepath.Join(dir, "session")
	if hasSession {
		if err := os.WriteFile(state, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"  *' exec -d '*) : > " + state + " ;;\n" +
		"  *has-session*) [ -e " + state + " ] || exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "smolvm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// attach copies BROWSER into the dev session's environment once the
// session exists (a has-session that succeeded, after the keeper when it
// had to start one) and before attach-session, in the attach exec itself;
// that exec also carries BROWSER in its Env.
func TestSmolAttachSetsSessionBrowser(t *testing.T) {
	for _, existing := range []bool{true, false} {
		log := fakeSmolvm(t, existing)
		b := &smolBackend{m: managedSmol("web")}
		if err := b.Attach(""); err != nil {
			t.Fatalf("existing=%v: Attach: %v", existing, err)
		}
		calls := readLines(t, log)
		last := calls[len(calls)-1]
		setEnv := strings.Index(last, `tmux -S "$1" set-environment -t dev BROWSER "$BROWSER"`)
		attach := strings.Index(last, `exec tmux -S "$1" attach-session -t dev`)
		if setEnv < 0 || attach < setEnv {
			t.Fatalf("existing=%v: attach exec %q lacks set-environment before attach-session", existing, last)
		}
		if !strings.Contains(last, " -it ") || !strings.Contains(last, " BROWSER="+ManagedShimPath+" ") {
			t.Errorf("existing=%v: attach exec %q: want -it and BROWSER in its env", existing, last)
		}
		// The call before the attach is the has-session that found it.
		prev := calls[len(calls)-2]
		if !strings.Contains(prev, "has-session -t dev") {
			t.Errorf("existing=%v: call before the attach = %q, want the has-session probe", existing, prev)
		}
		if !existing && !strings.Contains(strings.Join(calls, "\n"), " exec -d ") {
			t.Errorf("no keeper started for a missing session:\n%s", strings.Join(calls, "\n"))
		}
		for _, c := range calls[:len(calls)-1] {
			if strings.Contains(c, "set-environment") || strings.Contains(c, "attach-session") {
				t.Errorf("existing=%v: %q ran before the session was known to exist", existing, c)
			}
		}
	}
}

func TestSmolShellCarriesBrowser(t *testing.T) {
	log := fakeSmolvm(t, true)
	b := &smolBackend{m: managedSmol("web")}
	if err := b.Shell(""); err != nil {
		t.Fatal(err)
	}
	calls := readLines(t, log)
	if len(calls) != 1 {
		t.Fatalf("shell ran %d execs, want 1: %q", len(calls), calls)
	}
	if !strings.Contains(calls[0], " env SMOLVM_GUEST=1 BROWSER="+ManagedShimPath+" bash -c ") {
		t.Errorf("shell exec %q: want BROWSER in its env", calls[0])
	}
}

// fakeGuestBin is a directory holding a fake command that prints BROWSER.
func fakeGuestBin(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\nprintf '%s %s\\n' \"$*\" \"${BROWSER-<unset>}\" >> " + filepath.Join(dir, "log") + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The guest halves of shell and attach drop a BROWSER that names no
// executable shim, and pass one that does.
func TestSmolBrowserGuards(t *testing.T) {
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "devvm-open-url")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(shimDir, "gone")

	for _, tc := range []struct{ browser, want string }{{shim, shim}, {gone, "<unset>"}} {
		// shell: the outer bash -c execs `bash -l`, here a fake.
		dir := fakeGuestBin(t, "bash")
		argv := smolShellArgv()
		cmd := exec.Command("/bin/sh", argv[1:]...) // the snippet is plain sh
		cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + dir, "BROWSER=" + tc.browser}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("shell snippet: %v\n%s", err, out)
		}
		if got := readLines(t, filepath.Join(dir, "log")); len(got) != 1 || got[0] != "-l "+tc.want {
			t.Errorf("shell with BROWSER=%s: login shell saw %q, want BROWSER %s", tc.browser, got, tc.want)
		}

		// attach: set-environment only when the shim is there, attach always.
		dir = fakeGuestBin(t, "tmux")
		argv = smolAttachArgv()
		cmd = exec.Command("/bin/sh", argv[1:]...)
		cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "BROWSER=" + tc.browser}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("attach snippet: %v\n%s", err, out)
		}
		got := readLines(t, filepath.Join(dir, "log"))
		want := []string{"-S " + tmuxSocket + " attach-session -t dev " + tc.browser}
		if tc.want != "<unset>" {
			want = append([]string{"-S " + tmuxSocket + " set-environment -t dev BROWSER " + shim + " " + shim}, want...)
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("attach with BROWSER=%s: tmux calls\n%s\nwant\n%s", tc.browser, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// Remote attach carries BROWSER into the dev session only through its
// guarded set-environment (remoteAttachArgv), and remote shell sends no
// remote command at all: the login shell is honoured, and an exec env
// would be dropped by sshd anyway. Neither puts BROWSER on the ssh line
// as an environment assignment.
func TestRemoteAttachAndShellBrowser(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ssh.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for be, m := range map[string]*config.Machine{"server": managedServer("cloud", "cloud"), "entry": config.NewRemote("cloud", "dev@cloud")} {
		os.Remove(log)
		b := &sshBackend{m: m, paths: config.LegacyPaths(shortTempDir(t))}
		if err := b.Attach(config.TransportSSH); err != nil {
			t.Fatalf("%s: Attach: %v", be, err)
		}
		if err := b.Shell(config.TransportSSH); err != nil {
			t.Fatalf("%s: Shell: %v", be, err)
		}
		calls := readLines(t, log)
		if len(calls) != 3 {
			t.Fatalf("%s: %d ssh calls, want tmux -V, attach, shell: %q", be, len(calls), calls)
		}
		if !strings.Contains(calls[1], "set-environment -t dev BROWSER") || !strings.Contains(calls[1], "[ -x") {
			t.Errorf("%s: attach %q does not set a guarded BROWSER in the session", be, calls[1])
		}
		if strings.Contains(calls[1], "BROWSER=") || strings.Contains(calls[1], "'env'") {
			t.Errorf("%s: attach %q passes BROWSER as exec env", be, calls[1])
		}
		if !strings.HasSuffix(calls[2], " -t dev@cloud") {
			t.Errorf("%s: shell %q sends a remote command", be, calls[2])
		}
	}
}

// managedServer is a provider server's record with devvm's label: the one
// kind of box reached over ssh that devvm shapes (and pins).
func managedServer(name, addr string) *config.Machine {
	return config.NewProviderMachine(name, config.BackendHetzner, config.ProviderServer{Address: addr})
}

// managedSmol is a smol record its discovery source settled as managed
// (devvm made it): what every smol path that installs or shapes assumes.
func managedSmol(name string) *config.Machine {
	m := &config.Machine{Name: name, Backend: config.BackendSmol}
	m.SetManaged(true)
	return m
}

// A one-shot exec passes -i exactly when the caller set Stdin, and a
// Stream exec is --stream, so cp-out's tar (Stream, an empty Stdin) is
// `-i --stream`: the guest sees EOF at once, no tty keeps stdout
// byte-clean, and smolvm 1.35.5 caps neither an -i nor a --stream exec's
// stdout as it does a plain one's. No Stdin means no -i: smolvm then
// never reads the host's stdin, which runHost defaults to the terminal.
func TestSmolExecArgvStdin(t *testing.T) {
	b := &smolBackend{m: managedSmol("x")}
	in := strings.NewReader("")
	for _, c := range []struct {
		o     ExecOpts
		spawn bool
		want  string
	}{
		{ExecOpts{}, false, "smolvm machine exec --name x --"},
		{ExecOpts{Stdin: in}, false, "smolvm machine exec -i --name x --"},
		{ExecOpts{Stream: true}, false, "smolvm machine exec --stream --name x --"},
		{ExecOpts{Stream: true, Stdin: in}, false, "smolvm machine exec -i --stream --name x --"},
		{ExecOpts{}, true, "smolvm machine exec -i --stream --name x --"},
	} {
		got := strings.Join(b.execArgv(c.o, c.spawn, []string{"true"}), " ")
		if !strings.HasPrefix(got, c.want+" ") {
			t.Errorf("%+v spawn=%v: argv = %q, want it to start %q", c.o, c.spawn, got, c.want)
		}
		if strings.Contains(got, " -t") {
			t.Errorf("%+v: argv %q asks for a tty", c.o, got)
		}
	}
}
