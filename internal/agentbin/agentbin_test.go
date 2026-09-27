package agentbin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// fakeGuest is a Backend whose guest is a map of file contents. It answers
// the probes Install runs (uname, $HOME, and sumsScript's per-path loop:
// one sha256 line per path, "-" for a missing one), records every write,
// and applies the writes Install makes (the agent's install/mv, the shim's
// printf), so a second Install sees what the first one left.
type fakeGuest struct {
	backend.Backend
	files  map[string][]byte
	staged map[string][]byte // Copy targets
	writes []string
}

func newFakeGuest() *fakeGuest {
	return &fakeGuest{files: map[string][]byte{}, staged: map[string][]byte{}}
}

func (g *fakeGuest) Copy(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	g.staged[dst] = b
	g.writes = append(g.writes, "copy "+dst)
	return nil
}

func (g *fakeGuest) Run(_ context.Context, o backend.ExecOpts, argv ...string) error {
	out := o.Stdout
	if out == nil {
		out = io.Discard
	}
	joined := strings.Join(argv, " ")
	switch {
	case joined == "uname -s -m":
		fmt.Fprintln(out, "Linux x86_64")
	case joined == "sh -c echo $HOME":
		fmt.Fprintln(out, "/home/dev")
	case strings.HasPrefix(joined, "sh -c for f in "):
		list := strings.TrimPrefix(joined, "sh -c for f in ")
		for _, p := range strings.Fields(list[:strings.Index(list, ";")]) {
			if b, ok := g.files[p]; ok {
				fmt.Fprintln(out, sha256hex(b))
			} else {
				fmt.Fprintln(out, "-")
			}
		}
	case strings.Contains(joined, "install -m 0755"):
		// install -m 0755 STAGING TARGET && rm -f STAGING
		f := strings.Fields(argv[2])
		g.files[f[4]] = g.staged[f[3]]
		g.writes = append(g.writes, "install "+f[4]+" as "+o.User)
	case strings.Contains(joined, "printf"):
		// mkdir -p DIR && printf '…exec AGENT open-url…' > TMP && chmod && mv TMP SHIM
		script := argv[2]
		agent := strings.Fields(script[strings.Index(script, "exec ")+5:])[0]
		f := strings.Fields(script)
		shim := f[len(f)-1]
		g.files[shim] = []byte(shimScript(agent))
		g.writes = append(g.writes, "shim "+shim+" as "+o.User)
	case strings.Contains(joined, "mkdir -p") && !strings.Contains(joined, "printf"):
	case strings.Contains(joined, "chmod 0755") && strings.Contains(joined, " mv "):
		f := strings.Fields(argv[2])
		g.files[f[len(f)-1]] = g.staged[f[1]]
		g.writes = append(g.writes, "mv "+f[len(f)-1])
	default:
		return fmt.Errorf("fake guest: unexpected command %q", joined)
	}
	return nil
}

// One call installs agent and shim; a second is a no-op (sha match on both).
func TestInstallAgentAndShimIdempotent(t *testing.T) {
	g := newFakeGuest()
	m := &config.Machine{Name: "web", Backend: config.BackendSmol}
	p, err := Install(context.Background(), g, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Agent != "/usr/local/bin/devvm-agent" || p.Shim != "/usr/local/bin/devvm-open-url" {
		t.Fatalf("paths = %+v", p)
	}
	// smol's shell/attach Env and profile.d name this path without asking.
	if p.Shim != backend.ManagedShimPath {
		t.Fatalf("managed shim %q != backend.ManagedShimPath %q", p.Shim, backend.ManagedShimPath)
	}
	want := "copy /home/dev/.devvm-agent.new|install /usr/local/bin/devvm-agent as root|shim /usr/local/bin/devvm-open-url as root"
	if got := strings.Join(g.writes, "|"); got != want {
		t.Fatalf("writes = %s\nwant    %s", got, want)
	}
	if got := string(g.files[p.Shim]); got != "#!/bin/sh\nexec /usr/local/bin/devvm-agent open-url \"$@\"\n" {
		t.Fatalf("shim = %q", got)
	}
	g.writes = nil
	if _, err := Install(context.Background(), g, m, func() error { return errors.New("approve called") }); err != nil {
		t.Fatal(err)
	}
	if len(g.writes) != 0 {
		t.Fatalf("second install wrote %v", g.writes)
	}
}

// A box with the agent but no shim (installed by a devvm before step 7)
// gets the shim alone; on an adopt host that write is gated like any other.
func TestInstallShimOnlyGated(t *testing.T) {
	g := newFakeGuest()
	m := &config.Machine{Name: "box", Backend: config.BackendRemoteUnmanaged}
	data, _, err := binFor("Linux x86_64")
	if err != nil {
		t.Fatal(err)
	}
	g.files["/home/dev/.local/bin/devvm-agent"] = data
	if _, err := Install(context.Background(), g, m, func() error { return errors.New("declined") }); err == nil || err.Error() != "declined" {
		t.Fatalf("install without consent = %v", err)
	}
	if len(g.writes) != 0 {
		t.Fatalf("declined install wrote %v", g.writes)
	}
	approved := 0
	p, err := Install(context.Background(), g, m, func() error { approved++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if approved != 1 || strings.Join(g.writes, "|") != "shim /home/dev/.local/bin/devvm-open-url as " {
		t.Fatalf("approved %d, writes %v", approved, g.writes)
	}
	if p.Shim != "/home/dev/.local/bin/devvm-open-url" {
		t.Fatalf("paths = %+v", p)
	}
}

func TestParseSums(t *testing.T) {
	got := parseSums("abc\n-\n")
	if len(got) != 2 || got[0] != "abc" || got[1] != "-" {
		t.Fatalf("parseSums = %v", got)
	}
}

// localGuest runs Install's commands for real, under sh on this host, with
// $HOME at home: the quoting is exercised by an actual shell, not parsed.
type localGuest struct {
	backend.Backend
	home string
}

func (g localGuest) Copy(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

func (g localGuest) Run(_ context.Context, o backend.ExecOpts, argv ...string) error {
	if strings.Join(argv, " ") == "uname -s -m" {
		fmt.Fprintln(o.Stdout, "Linux x86_64") // the embedded agents are linux-only
		return nil
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "HOME="+g.home)
	cmd.Stdout, cmd.Stderr = o.Stdout, o.Stderr
	if cmd.Stdout == nil {
		cmd.Stdout = io.Discard
	}
	return cmd.Run()
}

// A guest $HOME with a space, a `%` and a quote: every path is quoted, the
// shim's bytes equal shimScript (so the sha check is idempotent), and the
// shim runs the agent with its arguments intact.
func TestInstallQuotesHostilePaths(t *testing.T) {
	home := filepath.Join(t.TempDir(), "we ird%s'home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	g := localGuest{home: home}
	m := &config.Machine{Name: "box", Backend: config.BackendRemoteUnmanaged}
	p, err := Install(context.Background(), g, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Agent != home+"/.local/bin/devvm-agent" || p.Shim != home+"/.local/bin/devvm-open-url" {
		t.Fatalf("paths = %+v", p)
	}
	b, err := os.ReadFile(p.Shim)
	if err != nil || string(b) != shimScript(p.Agent) {
		t.Fatalf("shim = %q, %v; want %q", b, err, shimScript(p.Agent))
	}
	if _, err := exec.LookPath("sha256sum"); err == nil {
		calls := 0
		if _, err := Install(context.Background(), g, m, func() error { calls++; return nil }); err != nil || calls != 0 {
			t.Fatalf("second install: approve called %d times, %v", calls, err)
		}
	}
	// Stand in for the agent, and run the shim as a login tool would.
	out := filepath.Join(home, "args")
	agent := "#!/bin/sh\nprintf '%s|' \"$@\" > '" + strings.ReplaceAll(out, "'", `'\''`) + "'\n"
	if err := os.WriteFile(p.Agent, []byte(agent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(p.Shim, "https://x/?a=1&b=%20", "two words").Run(); err != nil {
		t.Fatalf("run shim: %v", err)
	}
	if got, _ := os.ReadFile(out); string(got) != "open-url|https://x/?a=1&b=%20|two words|" {
		t.Fatalf("agent got %q", got)
	}
}

// The usual paths need no quoting, so the shim is byte for byte what
// installs before quoting wrote: no box rewrites its shim on upgrade.
func TestShimScriptUnchangedForPlainPaths(t *testing.T) {
	if got := shimScript("/usr/local/bin/devvm-agent"); got != "#!/bin/sh\nexec /usr/local/bin/devvm-agent open-url \"$@\"\n" {
		t.Fatalf("shimScript = %q", got)
	}
	if got := shq("it's a%b"); got != `'it'\''s a%b'` {
		t.Fatalf("shq = %q", got)
	}
}

// A machine on a hub is never installed into from here (hub.md §8): the
// hub's daemon installed agent and shim before its one agent exec. Refused
// before the backend is touched (a nil one would panic).
func TestInstallRefusesHubMachine(t *testing.T) {
	hub := config.NewHub("h", "u@h.example")
	m := &config.Machine{Name: "h/web", Backend: config.BackendHub, Hub: hub}
	if _, err := Install(context.Background(), nil, m, nil); !errors.Is(err, ErrHubMachine) {
		t.Fatalf("Install on a hub machine = %v, want ErrHubMachine", err)
	}
}
