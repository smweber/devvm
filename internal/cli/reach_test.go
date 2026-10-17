package cli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/reach"
)

// reachSSH is a fake ssh for the probe: no live master (`-O check` exits
// 255), and `-G` resolves every alias to 127.0.0.1 on the port in the
// returned file. Any other call (an exec, a shell) exits $FAKE_SSH_EXIT,
// 0 by default.
func reachSSH(t *testing.T, port int) (log, portFile string) {
	t.Helper()
	portFile = filepath.Join(t.TempDir(), "port")
	setPort(t, portFile, port)
	log = fakeSSH(t, "case \" $* \" in\n"+
		"*\" -O check \"*) exit 255;;\n"+
		"*\" -G \"*) printf 'hostname 127.0.0.1\\nport %s\\n' \"$(cat "+portFile+")\"; exit 0;;\n"+
		"esac\nexit ${FAKE_SSH_EXIT:-0}")
	return log, portFile
}

func setPort(t *testing.T, file string, port int) {
	t.Helper()
	if err := os.WriteFile(file, []byte(strconv.Itoa(port)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func listener(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func saveBox(t *testing.T, a *App, name string) {
	t.Helper()
	if err := writeEntry(a.Paths, config.NewRemote(name, true, "u@"+name)); err != nil {
		t.Fatal(err)
	}
}

var verifiedProbe = regexp.MustCompile(`^box\tremote\treachable\t-\tmanaged\tverified=\d+:probe\n$`)

// One-shot `status --plain` probes every remote box before printing; a
// --local one reads the cache and runs no ssh at all.
func TestStatusPlainProbesRemotes(t *testing.T) {
	a := newTestApp(t)
	log, _ := reachSSH(t, listener(t))
	saveBox(t, a, "box")
	if err := runTree(t, a, "status", "--plain"); err != nil {
		t.Fatal(err)
	}
	if got := a.Stdout.(*bytes.Buffer).String(); !verifiedProbe.MatchString(got) {
		t.Errorf("plain = %q", got)
	}
	if e := reach.Read(a.Paths, "box"); e.State != reach.Reachable || e.How != reach.HowProbe {
		t.Errorf("cache = %+v", e)
	}
	// The cache is under cache/, never run/ or the config dir (the watched dirs).
	if _, err := os.Stat(reach.Path(a.Paths, "box")); err != nil {
		t.Error(err)
	}

	os.Remove(log)
	if err := reach.Write(a.Paths, "box", reach.Entry{State: reach.Unreachable, Verified: time.Unix(123, 0), How: "probe", Reason: "connect timed out"}); err != nil {
		t.Fatal(err)
	}
	if err := runTree(t, a, "status", "--plain", "--local"); err != nil {
		t.Fatal(err)
	}
	if got := a.Stdout.(*bytes.Buffer).String(); got != "box\tremote\tunreachable\t-\tmanaged\tverified=123:probe\n" {
		t.Errorf("--local plain = %q", got)
	}
	if lines := sshLines(t, log); len(lines) != 0 {
		t.Errorf("--local ran ssh:\n%s", strings.Join(lines, "\n"))
	}
}

// `status -v` says when and how each remote box was last reached.
func TestStatusVerboseReach(t *testing.T) {
	a := newTestApp(t)
	fakeSSH(t, "exit 1")
	now := time.Now()
	for name, e := range map[string]reach.Entry{
		"up":    {State: reach.Reachable, Verified: now.Add(-12 * time.Second), How: reach.HowMaster},
		"down":  {State: reach.Unreachable, Verified: now, Since: time.Date(now.Year(), now.Month(), now.Day(), 0, 2, 0, 0, time.Local), How: reach.HowProbe, Reason: "connect timed out"},
		"proxy": {State: reach.Unknown, Reason: "ProxyJump"},
	} {
		saveBox(t, a, name)
		if err := reach.Write(a.Paths, name, e); err != nil {
			t.Fatal(err)
		}
	}
	saveBox(t, a, "never")
	var out bytes.Buffer
	a.Stdout = &out
	if err := a.runStatusAll(true, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"    reach: reachable, verified 1", "s ago (master)\n", // 12s or 13s: the cache keeps whole seconds
		"    reach: unreachable since 00:02 (connect timed out)\n",
		"    reach: not probed (ProxyJump)\n",
		"    reach: not probed yet\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status -v lacks %q:\n%s", want, out.String())
		}
	}
}

func TestForgetHostStateDropsReach(t *testing.T) {
	a := newTestApp(t)
	reach.Record(a.Paths, "box", "attach")
	if err := a.forgetHostState("box", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(reach.Path(a.Paths, "box")); !os.IsNotExist(err) {
		t.Errorf("reach cache survived: %v", err)
	}
}

// A command that got through to the box records it; ssh's own failure
// (255) records nothing, the remote command's failure still counts.
func TestExecRecordsContact(t *testing.T) {
	a := newTestApp(t)
	reachSSH(t, 1)
	saveBox(t, a, "box")
	t.Setenv("FAKE_SSH_EXIT", "255")
	if err := runTree(t, a, "exec", "box", "true"); err == nil {
		t.Fatal("exit 255 was not an error")
	}
	if e := reach.Read(a.Paths, "box"); e.State != reach.Unknown {
		t.Errorf("ssh's failure recorded %+v", e)
	}
	t.Setenv("FAKE_SSH_EXIT", "1")
	_ = runTree(t, a, "exec", "box", "false")
	if e := reach.Read(a.Paths, "box"); e.State != reach.Reachable || e.How != "exec" {
		t.Errorf("after a remote failure: %+v", e)
	}
	if _, err := os.Stat(a.Paths.ChangedPath()); err != nil {
		t.Errorf("no change marker: %v", err)
	}
}

func TestRefreshCommandWritesMarker(t *testing.T) {
	a := newTestApp(t)
	if err := runTree(t, a, "__refresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Paths.RefreshPath()); err != nil {
		t.Error(err)
	}
}

// A refresh request runs exactly one round; a burst inside the debounce
// window is one round; a request while a round is in flight joins it; an
// ordinary change re-snapshots without a round.
func TestWatchRefreshRunsOneRound(t *testing.T) {
	dir := testPaths(t.TempDir())
	var rounds atomic.Int32
	var mu sync.Mutex
	snap := "before\n"
	gate := make(chan struct{}, 1) // a round blocks until released
	started := make(chan struct{}, 4)
	refresh := func(ctx context.Context) {
		rounds.Add(1)
		started <- struct{}{}
		select {
		case <-gate:
		case <-ctx.Done():
		}
		mu.Lock()
		snap = "after " + strconv.Itoa(int(rounds.Load())) + "\n"
		mu.Unlock()
	}
	snapshot := func() string { mu.Lock(); defer mu.Unlock(); return snap }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := newBlockWriter()
	go func() { _ = watchStatus(ctx, dir, okSnap(snapshot), nil, out, 50*time.Millisecond, nil, refresh) }()
	out.next(t, "before\n")

	// An ordinary change: no round.
	config.TouchChanged(dir)
	out.none(t)
	if n := rounds.Load(); n != 0 {
		t.Fatalf("run/changed ran %d rounds", n)
	}

	// A burst of requests: one round, and a second request while it is in
	// flight joins it.
	for range 3 {
		if err := config.RequestRefresh(dir); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("no round started")
	}
	if err := config.RequestRefresh(dir); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // past the debounce, round still held
	gate <- struct{}{}
	out.next(t, "after 1\n")
	out.none(t)
	if n := rounds.Load(); n != 1 {
		t.Errorf("rounds = %d, want 1", n)
	}

	// A later request is a new round.
	gate <- struct{}{}
	if err := config.RequestRefresh(dir); err != nil {
		t.Fatal(err)
	}
	out.next(t, "after 2\n")
}

// The merged watch probes on start (inside the first-block wait) and then
// only on a refresh request: a daemon touching run/ costs no ssh.
func TestWatchProbesOnStartAndRefresh(t *testing.T) {
	a := newTestApp(t)
	log, portFile := reachSSH(t, listener(t))
	saveBox(t, a, "box")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := newBlockWriter()
	done := make(chan error, 1)
	go func() { done <- a.watchWithHubs(ctx, out, 30*time.Millisecond) }()
	select {
	case got := <-out.blocks:
		if !verifiedProbe.MatchString(got) {
			t.Fatalf("first block = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no first block")
	}
	calls := len(sshLines(t, log))
	if calls == 0 {
		t.Fatal("the opening round ran no ssh")
	}
	config.TouchChanged(a.Paths)
	out.none(t)
	if n := len(sshLines(t, log)); n != calls {
		t.Errorf("run/changed probed: %d ssh calls, was %d", n, calls)
	}
	// The box goes away behind the watcher's back (nothing listens on the
	// port it resolves to now); only the refresh notices.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	setPort(t, portFile, ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	if err := config.RequestRefresh(a.Paths); err != nil {
		t.Fatal(err)
	}
	out.until(t, "the box unreachable", func(b string) bool {
		return strings.HasPrefix(b, "box\tremote\tunreachable\t-\tmanaged\tverified=")
	})
	cancel()
	<-done
}

// noteContact writes only on evidence that ssh got through: success, or a
// remote command's own exit status. ssh's 255 and any mosh failure write
// nothing, nor does a contact with a box that is not probeable.
func TestNoteContact(t *testing.T) {
	a := newTestApp(t)
	exitErr := func(code int) error {
		err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
		if err == nil {
			t.Fatal("no exit error")
		}
		return err
	}
	box := config.NewRemote("box", true, "u@box")
	for _, tc := range []struct {
		transport string
		err       error
	}{
		{config.TransportSSH, exitErr(255)},
		{config.TransportMosh, exitErr(1)},
		{config.TransportSSH, errors.New("ssh is not installed on this host")},
	} {
		a.noteContact(box, "shell", tc.transport, tc.err)
		if e := reach.Read(a.Paths, "box"); e.State != reach.Unknown {
			t.Errorf("%s %v recorded %+v", tc.transport, tc.err, e)
		}
	}
	a.noteContact(&config.Machine{Name: "vm", Backend: config.BackendSmol}, "shell", config.TransportSSH, nil)
	if _, err := os.Stat(reach.Path(a.Paths, "vm")); !os.IsNotExist(err) {
		t.Errorf("a smol VM got a reach file: %v", err)
	}
	a.noteContact(box, "attach", config.TransportMosh, nil)
	if e := reach.Read(a.Paths, "box"); e.State != reach.Reachable || e.How != "attach" {
		t.Errorf("mosh success = %+v", e)
	}
	a.noteContact(box, "shell", config.TransportSSH, exitErr(3))
	if e := reach.Read(a.Paths, "box"); e.State != reach.Reachable || e.How != "shell" {
		t.Errorf("remote exit 3 = %+v", e)
	}
}

// shell and attach that got through record their contact (setup's is
// asserted in TestSetupRemoteOrder, which already drives a managed box).
func TestCommandsRecordContact(t *testing.T) {
	for _, argv := range [][]string{{"shell", "box"}, {"attach", "box"}} {
		t.Run(argv[0], func(t *testing.T) {
			a := newTestApp(t)
			reachSSH(t, 1)
			if err := writeEntry(a.Paths, config.NewRemote("box", false, "u@box")); err != nil {
				t.Fatal(err)
			}
			if err := runTree(t, a, argv...); err != nil {
				t.Fatalf("%v: %v\n%s", argv, err, a.Stderr.(*bytes.Buffer).String())
			}
			if e := reach.Read(a.Paths, "box"); e.State != reach.Reachable || e.How != argv[0] {
				t.Errorf("%v recorded %+v", argv, e)
			}
		})
	}
}

// A round's deadline cuts short a probe in flight and leaves a box with no
// slot unprobed: both keep their cached entries, and the round returns
// within its deadline (plus the kill's drain).
func TestReachRoundDeadlineKeepsCache(t *testing.T) {
	a := newTestApp(t)
	orig := reachTimes
	reachTimes = reachTiming{deadline: 300 * time.Millisecond, parallel: 1}
	t.Cleanup(func() { reachTimes = orig })
	log := fakeSSH(t, "exec sleep 5") // every master check hangs
	recorded := reach.Entry{State: reach.Reachable, Verified: time.Unix(1760000000, 0), How: "attach"}
	for _, n := range []string{"one", "two"} {
		saveBox(t, a, n)
		if err := reach.Write(a.Paths, n, recorded); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	a.reachRound(context.Background(), a.remoteConfs())
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("round took %s", d)
	}
	for _, n := range []string{"one", "two"} {
		if e := reach.Read(a.Paths, n); e.State != reach.Reachable || e.How != "attach" || !e.Verified.Equal(recorded.Verified) {
			t.Errorf("%s: cache = %+v", n, e)
		}
	}
	if n := len(sshLines(t, log)); n != 1 {
		t.Errorf("ssh calls = %d, want 1 (one slot, one master check cut short)", n)
	}
}

// A hub machine is its hub's row: never probed here, its hub's sixth
// column passed through.
func TestHubRowVerifiedPassesThrough(t *testing.T) {
	a := newTestApp(t)
	log := fakeSSH(t, "printf 'box\\tremote-managed\\treachable\\t-\\tmanaged\\tverified=123:probe\\nweb\\tsmol\\trunning\\t-\\tmanaged\\t-\\nold\\tsmol\\tstopped\\t-\\n'")
	writeHub(t, a, "h", nil)
	var out bytes.Buffer
	a.Stdout = &out
	if err := a.runStatusPlain(false); err != nil {
		t.Fatal(err)
	}
	want := "h\thub\treachable\t-\t-\t-\n" +
		"h/box\tremote\treachable\t-\tmanaged\tverified=123:probe\n" +
		"h/old\tsmol\tstopped\t-\t-\t-\n" +
		"h/web\tsmol\trunning\t-\tmanaged\t-\n"
	if out.String() != want {
		t.Errorf("plain =\n%s\nwant\n%s", out.String(), want)
	}
	for _, l := range sshLines(t, log) {
		if strings.Contains(l, "-O check") || strings.Contains(l, " -G ") {
			t.Errorf("a hub row was probed: %s", l)
		}
	}
	if entries, _ := os.ReadDir(reach.Dir(a.Paths)); len(entries) != 0 {
		t.Errorf("reach cache holds %d entries", len(entries))
	}
}

// The central §5 promise, end to end through the merged watch: one round
// at start, none for any number of ordinary `run/changed` touches, and
// exactly one for a `devvm __refresh`. A round is counted by its `ssh -G`
// (one box, one resolve per round; the fake has no master).
func TestWatchRoundsOnlyOnRefresh(t *testing.T) {
	a := newTestApp(t)
	log, _ := reachSSH(t, listener(t))
	saveBox(t, a, "box")
	rounds := func() int {
		n := 0
		for _, l := range sshLines(t, log) {
			if strings.Contains(l, " -G ") {
				n++
			}
		}
		return n
	}
	waitRounds := func(want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for rounds() < want {
			if time.Now().After(deadline) {
				t.Fatalf("rounds = %d, want %d", rounds(), want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := newBlockWriter()
	done := make(chan error, 1)
	go func() { done <- a.watchWithHubs(ctx, out, 30*time.Millisecond) }()
	select {
	case <-out.blocks:
	case <-time.After(3 * time.Second):
		t.Fatal("no first block")
	}
	if n := rounds(); n != 1 {
		t.Fatalf("opening rounds = %d, want 1", n)
	}
	for range 3 {
		config.TouchChanged(a.Paths)
		time.Sleep(50 * time.Millisecond) // past the debounce each time: three separate snapshots
	}
	out.none(t)
	if n := rounds(); n != 1 {
		t.Fatalf("after run/changed: rounds = %d, want still 1", n)
	}
	// The real command, from a second App on the same state dir (as a
	// separate devvm process would be).
	other := &App{Paths: a.Paths}
	if err := runTree(t, other, "__refresh"); err != nil {
		t.Fatal(err)
	}
	waitRounds(2)
	time.Sleep(300 * time.Millisecond) // a second round would have started by now
	if n := rounds(); n != 2 {
		t.Errorf("after __refresh: rounds = %d, want 2 (opening + one)", n)
	}
	cancel()
	<-done
}
