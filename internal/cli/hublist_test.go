package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/session"
)

// shortHubTimes runs the listing on test-sized clocks: the production 2s
// connect / 5s deadline / 2–30s backoff would make every unreachable-hub
// test a coffee break. Restored on cleanup; these tests do not run in
// parallel with each other.
func shortHubTimes(t *testing.T, deadline, backoff time.Duration) {
	t.Helper()
	orig := hubTimes
	hubTimes = hubTiming{connect: 1, deadline: deadline, backoffMin: backoff, backoffMax: 2 * backoff, eofGrace: 200 * time.Millisecond}
	t.Cleanup(func() { hubTimes = orig })
}

// hubRowsScript is a fake ssh body that answers the listing with fixed rows,
// whatever the remote command was.
const hubRowsScript = "printf 'web\\tsmol\\trunning\\tup:9\\napi\\tsmol\\tstopped\\t-\\n'"

func TestParseHubRows(t *testing.T) {
	text := "Welcome to the hub\n\nweb\tsmol\trunning\tup:1\napi\tsmol\tstopped\nother/x\tsmol\trunning\t-\nh@x\tsmol\t?\t-\nbad name\tsmol\t?\t-\n"
	got := parseHubRows(text)
	want := []hubRow{{"web", "smol", "running", "up:1"}, {"api", "smol", "stopped", "-"}}
	if !slices.Equal(got, want) {
		t.Errorf("parseHubRows = %v, want %v", got, want)
	}
}

func TestBlockSplitter(t *testing.T) {
	var got []string
	s := &blockSplitter{onBlock: func(b string) { got = append(got, b) }}
	// Leading newlines (a banner flushed ahead of devvm) are never a block,
	// whether they arrive alone or with the first rows; a lone "\n" after a
	// real block is the empty listing it means.
	for _, chunk := range []string{"\n", "\n\n", "\na\tsmol\trun", "ning\t-\n", "\nb\tsmol\tstopped\t-\n\n", "\n", "c\tsmol\t?\t-\n\n"} {
		if _, err := s.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"a\tsmol\trunning\t-\n", "b\tsmol\tstopped\t-\n", "", "c\tsmol\t?\t-\n"}
	if !slices.Equal(got, want) {
		t.Errorf("blocks = %q, want %q", got, want)
	}
	// One write with a banner line and the first block: one block.
	got = nil
	s = &blockSplitter{onBlock: func(b string) { got = append(got, b) }}
	if _, err := s.Write([]byte("\nweb\tsmol\trunning\t-\n\n")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"web\tsmol\trunning\t-\n"}) {
		t.Errorf("banner + block = %q", got)
	}
}

// A stream with no separator is bounded: past hubBlockMax the buffer is
// dropped, the overflow hook fires (it kills the ssh) and Write errors so
// the copy stops.
func TestBlockSplitterOverflow(t *testing.T) {
	overflows, blocks := 0, 0
	s := &blockSplitter{onBlock: func(string) { blocks++ }, onOverflow: func() { overflows++ }}
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	var err error
	for i := 0; i < 2*hubBlockMax/len(chunk) && err == nil; i++ {
		_, err = s.Write(chunk)
	}
	if err == nil || overflows != 1 || blocks != 0 || s.buf != nil {
		t.Errorf("err = %v, overflows = %d, blocks = %d, buf = %d bytes", err, overflows, blocks, len(s.buf))
	}
}

// End to end: a hub pipe that spews without a separator is killed and
// re-spawned, and the hub reads unreachable meanwhile.
func TestWatchHubPipeOverflowRespawns(t *testing.T) {
	shortHubTimes(t, 500*time.Millisecond, 50*time.Millisecond)
	a := newTestApp(t)
	log := fakeSSH(t, "head -c 2500000 /dev/zero | tr '\\0' x; exec sleep 30")
	writeHub(t, a, "h", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := newBlockWriter()
	done := make(chan error, 1)
	go func() { done <- a.watchWithHubs(ctx, out, 30*time.Millisecond) }()
	out.until(t, "with the hub unreachable", func(b string) bool { return b == "h\thub\tunreachable\t-\n" })
	deadline := time.Now().Add(3 * time.Second)
	for len(sshLines(t, log)) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the overflowing pipe was not re-spawned")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop")
	}
}

// --local is this host only: hub confs are skipped, their tables with them,
// and nothing is dialed. It is what a hub runs for its callers, so a hub
// with hubs of its own never fans out.
func TestStatusLocalSkipsHubs(t *testing.T) {
	a := newTestApp(t)
	log := fakeSSH(t, "exit 1")
	if err := config.NewSmol("box").Save(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}})
	if err := writeHubCache(a.ConfigDir, "h", []hubRow{{"db", "smol", "running", "-"}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a.Stdout = &out
	if err := a.runStatusPlain(true); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "box\tsmol\t?\t-\n" {
		t.Errorf("--local plain =\n%s", got)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		data, _ := os.ReadFile(log)
		t.Errorf("--local dialed the hub:\n%s", data)
	}
	// Through the tree, for the human listing and --watch too; and never
	// with a NAME, which would be a different command.
	if err := runTree(t, a, "status", "--local"); err != nil {
		t.Fatal(err)
	}
	if got := a.Stdout.(*bytes.Buffer).String(); !strings.Contains(got, "  box ") || strings.Contains(got, "hub\n") ||
		strings.Contains(got, "  h ") || strings.Contains(got, "  h/") {
		t.Errorf("status --local =\n%s", got)
	}
	if err := runTree(t, a, "status", "--local", "box"); err == nil || !strings.Contains(err.Error(), "--local") {
		t.Errorf("status --local NAME: err = %v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Error("the human --local listing dialed the hub")
	}
}

// The listing argv: `status --plain --local` under the login shell behind
// `env DEVVM_NO_SUBSCRIBE=1`, BatchMode, no -t even on a terminal, and a 2s
// connect timeout whatever DEVVM_SSH_CONNECT_TIMEOUT says.
func TestHubListingArgv(t *testing.T) {
	a := newTestApp(t)
	sshLog, argvLog := fakeHub(t, a)
	t.Setenv("DEVVM_SSH_CONNECT_TIMEOUT", "30")
	pinTTY(t, true)
	if err := runTree(t, a, "status", "--plain"); err != nil {
		t.Fatal(err)
	}
	calls := devvmCalls(t, argvLog)
	if len(calls) != 1 || !slices.Equal(calls[0], []string{"status", "--plain", "--local"}) {
		t.Errorf("hub devvm argv = %v, want [[status --plain --local]]", calls)
	}
	lines := sshLines(t, sshLog)
	if len(lines) != 1 {
		t.Fatalf("ssh calls = %d, want 1:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	// Exactly the proxy's BatchMode line (login-shell wrapper around
	// `env DEVVM_NO_SUBSCRIBE=1 devvm status --plain --local`), with the
	// listing's own connect timeout in place of the default.
	want := strings.Replace(wantSSH(a, false, "status", "--plain", "--local"), "ConnectTimeout=10", "ConnectTimeout=2", 1)
	if lines[0] != want {
		t.Errorf("ssh line:\n%s\nwant\n%s", lines[0], want)
	}
	// The fake devvm prints no rows, so the hub answered with an empty
	// listing: reachable, and the tabled machine is `?` (the hub did not
	// name it), with this host's forwards column.
	if got := a.Stdout.(*bytes.Buffer).String(); got != "h\thub\treachable\t-\nh/web\thub\t?\tdown\n" {
		t.Errorf("plain =\n%s", got)
	}
}

// Row merge: state and backend from the hub's row, the forwards column
// from this host's own daemon for HUB@NAME (the hub said up:9; ours says
// up:2), a hub row of its own, machines this host knows that the hub did
// not list, and the cache written under cache/ and nowhere else.
func TestStatusMergesHubRows(t *testing.T) {
	a := newTestApp(t)
	fakeSSH(t, hubRowsScript)
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}, "db": {"80"}})
	// This host's own daemon for h@web (serveFakeDaemon takes the runtime
	// name): two forwards, against the hub's up:9.
	serveFakeDaemon(t, a.ConfigDir, config.RuntimeName("h/web"), session.Response{
		State: session.StateUp, Forwards: []session.Forward{{Host: 3000, Guest: 3000}, {Host: 3001, Guest: 3001}},
	})
	var out bytes.Buffer
	a.Stdout = &out
	if err := a.runStatusPlain(false); err != nil {
		t.Fatal(err)
	}
	want := "h\thub\treachable\t-\n" +
		"h/api\tsmol\tstopped\t-\n" +
		"h/db\thub\t?\tdown\n" +
		"h/web\tsmol\trunning\tup:2\n"
	if out.String() != want {
		t.Errorf("plain =\n%s\nwant\n%s", out.String(), want)
	}
	// The cache is the hub's rows verbatim, in cache/; machines/ and run/
	// hold only what the test put there (a cache write must never wake
	// `status --watch`, which observes those two).
	data, err := os.ReadFile(hubCachePath(a.ConfigDir, "h"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "web\tsmol\trunning\tup:9\napi\tsmol\tstopped\t-\n" {
		t.Errorf("cache =\n%q", got)
	}
	if rel, _ := filepath.Rel(config.CacheDir(a.ConfigDir), hubCachePath(a.ConfigDir, "h")); rel != "hub-h.list" {
		t.Errorf("cache path %s is not under cache/", hubCachePath(a.ConfigDir, "h"))
	}
	for dir, want := range map[string][]string{config.MachinesDir(a.ConfigDir): {"h.toml"}, config.RuntimeDir(a.ConfigDir): {"h@web.sock"}} {
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if !slices.Equal(names, want) {
			t.Errorf("%s holds %v, want %v", dir, names, want)
		}
	}
	// The human listing puts the hub and its machines in the hub section.
	out.Reset()
	if err := a.runStatusAll(true, false); err != nil {
		t.Fatal(err)
	}
	human := out.String()
	for _, want := range []string{"hub\n", "  h  ", "reachable", "  h/web", "running", "  h/api", "stopped", "backend: smol (as reported by hub h)"} {
		if !strings.Contains(human, want) {
			t.Errorf("human listing lacks %q:\n%s", want, human)
		}
	}
	// The drill-in for one hub machine reads the same listing.
	out.Reset()
	if err := a.runStatus("h/web"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.HasPrefix(got, "h/web (smol on hub h, u@h.example)\n  state: running\n") {
		t.Errorf("status h/web =\n%s", got)
	}
}

// A hub that fails, times out, or hangs renders its cached rows
// `unreachable` — every row, whatever the cache says — within the deadline.
func TestStatusHubUnreachableWithinDeadline(t *testing.T) {
	shortHubTimes(t, 300*time.Millisecond, 50*time.Millisecond)
	cached := []hubRow{{"web", "smol", "running", "up:9"}, {"api", "smol", "stopped", "-"}}
	want := "h\thub\tunreachable\t-\n" +
		"h/api\tsmol\tunreachable\t-\n" +
		"h/web\tsmol\tunreachable\tdown\n"
	for _, tc := range []struct {
		name, body string
		within     time.Duration
	}{
		{"ssh exits 255", "exit 255", 2 * time.Second},
		{"hub devvm exits 1", "exit 1", 2 * time.Second},
		// The deadline kills ssh itself…
		{"hangs", "exec sleep 30", 2 * time.Second},
		// …and when the hung process is a grandchild still holding stdout
		// (a login shell's child), WaitDelay closes the pipe under it.
		{"hangs in a child", "sleep 30\nexit 0", 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			fakeSSH(t, tc.body)
			writeHub(t, a, "h", map[string][]string{"web": {"3000"}})
			if err := writeHubCache(a.ConfigDir, "h", cached); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			a.Stdout = &out
			start := time.Now()
			if err := a.runStatusPlain(false); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed > tc.within {
				t.Errorf("status took %s, want < %s", elapsed, tc.within)
			}
			if out.String() != want {
				t.Errorf("plain =\n%s\nwant\n%s", out.String(), want)
			}
			// A failed listing leaves the cache alone.
			if got := readHubCache(a.ConfigDir, "h"); !slices.Equal(got, cached) {
				t.Errorf("cache after failure = %v", got)
			}
			// The drill-in says so too, rather than dressing the cache up.
			out.Reset()
			if err := a.runStatus("h/web"); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); !strings.Contains(got, "state: unreachable\n  hub: h did not answer") {
				t.Errorf("status h/web while unreachable =\n%s", got)
			}
		})
	}
	// The deadline is the whole budget: with a real-sized one the pipe drain
	// after the kill (backend.HostWaitDelay, which a ControlMaster mux
	// holding the killed client's stdout consumes in full) fits inside it,
	// so a hung hub costs the deadline and not the deadline plus a second.
	for _, tc := range []struct{ name, body string }{{"hangs", "exec sleep 30"}, {"hangs in a child", "sleep 30\nexit 0"}} {
		t.Run("budget: "+tc.name, func(t *testing.T) {
			shortHubTimes(t, 1500*time.Millisecond, 50*time.Millisecond)
			a := newTestApp(t)
			fakeSSH(t, tc.body)
			writeHub(t, a, "h", nil)
			a.Stdout = new(bytes.Buffer)
			start := time.Now()
			if err := a.runStatusPlain(false); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed > hubTimes.deadline+200*time.Millisecond {
				t.Errorf("status took %s, want within the %s deadline", elapsed, hubTimes.deadline)
			}
		})
	}
	// Two hubs are asked in parallel: two hung hubs cost one deadline.
	t.Run("parallel", func(t *testing.T) {
		a := newTestApp(t)
		fakeSSH(t, "exec sleep 30")
		writeHub(t, a, "h1", nil)
		writeHub(t, a, "h2", nil)
		var out bytes.Buffer
		a.Stdout = &out
		start := time.Now()
		if err := a.runStatusPlain(false); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("two hung hubs took %s", elapsed)
		}
		if got := out.String(); got != "h1\thub\tunreachable\t-\nh2\thub\tunreachable\t-\n" {
			t.Errorf("plain =\n%s", got)
		}
	})
}

func TestHubCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got := readHubCache(dir, "h"); got != nil {
		t.Errorf("missing cache = %v", got)
	}
	rows := []hubRow{{"web", "smol", "running", "up:1"}, {"api", "remote-managed", "reachable", "-"}}
	if err := writeHubCache(dir, "h", rows); err != nil {
		t.Fatal(err)
	}
	if got := readHubCache(dir, "h"); !slices.Equal(got, rows) {
		t.Errorf("round trip = %v", got)
	}
	// Atomic: no temp file survives a write, and the file is exactly the
	// --plain grammar.
	entries, _ := os.ReadDir(config.CacheDir(dir))
	if len(entries) != 1 || entries[0].Name() != "hub-h.list" {
		t.Errorf("cache dir holds %v", entries)
	}
	data, _ := os.ReadFile(hubCachePath(dir, "h"))
	if string(data) != "web\tsmol\trunning\tup:1\napi\tremote-managed\treachable\t-\n" {
		t.Errorf("cache text = %q", data)
	}
	if !slices.Equal(cachedHubMachines(dir, "h"), []string{"web", "api"}) {
		t.Errorf("cachedHubMachines = %v", cachedHubMachines(dir, "h"))
	}
	// An empty listing is a real answer (the hub has no machines): the
	// cache becomes empty, not stale.
	if err := writeHubCache(dir, "h", nil); err != nil {
		t.Fatal(err)
	}
	if got := readHubCache(dir, "h"); got != nil {
		t.Errorf("empty listing cached as %v", got)
	}
	writeHubCache(dir, "h", rows)
	dropFromHubCache(dir, "h", "web")
	if got := cachedHubMachines(dir, "h"); !slices.Equal(got, []string{"api"}) {
		t.Errorf("after drop = %v", got)
	}
	removeHubCache(dir, "h")
	if _, err := os.Stat(hubCachePath(dir, "h")); !os.IsNotExist(err) {
		t.Error("removeHubCache left the file")
	}
	removeHubCache(dir, "h") // idempotent
}

// `delete HUB/NAME` drops the machine from the cache so completion stops
// offering it before the next listing.
func TestDeleteHubMachineDropsCacheRow(t *testing.T) {
	a := newTestApp(t)
	a.Stdout = new(bytes.Buffer)
	pinTTY(t, false)
	fakeSSH(t, "exit 0")
	writeHub(t, a, "h", nil)
	if err := writeHubCache(a.ConfigDir, "h", []hubRow{{"web", "smol", "running", "-"}, {"api", "smol", "stopped", "-"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.runDelete("h/web", false); err != nil {
		t.Fatal(err)
	}
	if got := cachedHubMachines(a.ConfigDir, "h"); !slices.Equal(got, []string{"api"}) {
		t.Errorf("cache after delete h/web = %v", got)
	}
}

// Completion of `HUB/` lists the hub's machines from the cache and the
// tables, never by dialing.
func TestCompleteHubPrefix(t *testing.T) {
	a := newTestApp(t)
	log := fakeSSH(t, "exit 1")
	if err := config.NewSmol("box").Save(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	writeHub(t, a, "h", map[string][]string{"api": nil})
	if err := writeHubCache(a.ConfigDir, "h", []hubRow{{"web", "smol", "running", "-"}, {"db", "smol", "stopped", "-"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := a.completeMachines(nil, nil, "h/")
	if want := []string{"h/api", "h/db", "h/web"}; !slices.Equal(got, want) {
		t.Errorf("completeMachines(h/) = %v, want %v", got, want)
	}
	got, _ = a.completeAnyMachine(nil, nil, "h")
	if want := []string{"h", "h/api", "h/db", "h/web"}; !slices.Equal(got, want) {
		t.Errorf("completeAnyMachine(h) = %v, want %v", got, want)
	}
	// End to end, as a shell (or the menu bar app) asks.
	if err := runTree(t, a, "__complete", "attach", "h/"); err != nil {
		t.Fatal(err)
	}
	out := a.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "h/web\n") || strings.Contains(out, "box") {
		t.Errorf("__complete attach h/ =\n%s", out)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Error("completion dialed the hub")
	}
}

// until drains blocks until one satisfies ok, failing after 3s. The watch
// stream under a flapping fake pipe is not deterministic block for block,
// so the assertions are "eventually".
func (w *blockWriter) until(t *testing.T, what string, ok func(block string) bool) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case got := <-w.blocks:
			if ok(got) {
				return got
			}
		case <-deadline:
			t.Fatalf("no block %s within 3s", what)
		}
	}
}

// --watch holds one pipe per hub: a block re-emits the merged listing, EOF
// renders the hub unreachable and the pipe is re-spawned with backoff, a
// hub conf written while watching gets a pipe, a deleted one loses it.
func TestWatchHubPipes(t *testing.T) {
	shortHubTimes(t, 500*time.Millisecond, 50*time.Millisecond)
	a := newTestApp(t)
	// The first ssh prints one block, lives a moment, and exits (a hub whose
	// watcher was pkill'd); every later one prints a block and holds the
	// pipe, as a real `status --plain --watch` does.
	ctr := filepath.Join(t.TempDir(), "n")
	log := fakeSSH(t, "n=$(cat "+ctr+" 2>/dev/null || echo 0); echo $((n+1)) > "+ctr+"\n"+
		"printf 'web\\tsmol\\trunning\\tup:9\\n\\n'\n"+
		"[ \"$n\" -ge 1 ] && exec sleep 30\nsleep 0.3\nexit 0")
	writeHub(t, a, "h", map[string][]string{"web": {"3000"}})
	// A stale cache from an earlier day: the first block replaces it.
	if err := writeHubCache(a.ConfigDir, "h", []hubRow{{"old", "smol", "stopped", "-"}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := newBlockWriter()
	done := make(chan error, 1)
	go func() { done <- a.watchWithHubs(ctx, out, 30*time.Millisecond) }()

	// The first block already carries the hub's answer (the loop settles
	// on the pipes before emitting), with this host's forwards column.
	out.next(t, "h\thub\treachable\t-\nh/web\tsmol\trunning\tdown\n")
	// The pipe hit EOF: the hub and its cached rows (the block it sent, not
	// the stale cache from before) go unreachable, the hub's backend token
	// kept…
	out.until(t, "with the hub unreachable", func(b string) bool {
		return b == "h\thub\tunreachable\t-\nh/web\tsmol\tunreachable\tdown\n"
	})
	// …and the re-spawned pipe (after the backoff) brings it back.
	out.until(t, "with the hub back", func(b string) bool { return b == "h\thub\treachable\t-\nh/web\tsmol\trunning\tdown\n" })
	// Two pipes so far, both the BatchMode proxy line for `status --plain
	// --local --watch --exit-on-stdin-eof` (no -t: no terminal is ever
	// wanted on a held pipe).
	lines := sshLines(t, log)
	wantLine := strings.Replace(wantSSH(a, false, "status", "--plain", "--local", "--watch", statusExitFlag), "ConnectTimeout=10", "ConnectTimeout=1", 1)
	if len(lines) < 2 || lines[0] != wantLine || lines[1] != wantLine {
		t.Errorf("hub pipe argv:\n%s\nwant\n%s", strings.Join(lines, "\n"), wantLine)
	}
	// The cache followed the last block.
	if got := cachedHubMachines(a.ConfigDir, "h"); !slices.Equal(got, []string{"web"}) {
		t.Errorf("cache after blocks = %v", got)
	}

	// A hub conf created while watching gets a pipe of its own, no restart.
	writeHub(t, a, "h2", nil)
	out.until(t, "listing h2", func(b string) bool {
		return strings.Contains(b, "h2\thub\treachable\t-\nh2/web\tsmol\trunning\t-\n") && strings.Contains(b, "h/web\tsmol\trunning\tdown\n")
	})
	// A local change still re-emits, with the hubs' latest blocks merged.
	if err := config.NewSmol("box").Save(a.ConfigDir); err != nil {
		t.Fatal(err)
	}
	out.until(t, "with the local machine", func(b string) bool { return strings.HasPrefix(b, "box\tsmol\t?\t-\nh\thub\treachable") })

	// A deleted hub conf closes its pipe and drops its rows.
	if err := config.Remove(a.ConfigDir, "h2"); err != nil {
		t.Fatal(err)
	}
	out.until(t, "without h2", func(b string) bool { return !strings.Contains(b, "h2") && strings.Contains(b, "h/web") })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop (a hub pipe outlived its cancel)")
	}
}

// The hub side of the orphan fix: `status --plain --watch --exit-on-stdin-eof`
// ends cleanly when stdin closes; without the flag stdin is never read.
func TestWatchExitsOnStdinEOF(t *testing.T) {
	a := newTestApp(t)
	pr, pw := io.Pipe()
	a.Stdin, a.Stdout, a.Stderr = pr, newBlockWriter(), io.Discard
	done := make(chan error, 1)
	go func() {
		root := a.rootCmd()
		root.SetArgs([]string{"status", "--plain", "--watch", "--local", statusExitFlag})
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		done <- root.Execute()
	}()
	a.Stdout.(*blockWriter).next(t, "") // watching (an empty registry is one blank block)
	pw.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch on stdin EOF returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not exit on stdin EOF")
	}
	// The flag is hidden but real; without it a closed stdin is not looked at.
	pr2, pw2 := io.Pipe()
	pw2.Close()
	b := newTestApp(t)
	b.Stdin, b.Stdout = pr2, newBlockWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done2 := make(chan error, 1)
	go func() { done2 <- b.runStatusWatch(ctx, true, false) }()
	b.Stdout.(*blockWriter).next(t, "")
	select {
	case <-done2:
		t.Fatal("watch without the flag exited on a closed stdin")
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	<-done2
}

// The laptop side: the held pipe's stdin is what the hub-side watcher waits
// on. When the watcher stops, stdin is closed first and the hub-side command
// sees EOF and exits on its own (here: `cat` returns, the marker is written)
// before the grace runs out and the ssh would be killed.
func TestWatchHubPipeStdinEOF(t *testing.T) {
	shortHubTimes(t, 500*time.Millisecond, 50*time.Millisecond)
	a := newTestApp(t)
	marker := filepath.Join(t.TempDir(), "eof")
	fakeSSH(t, "printf 'web\\tsmol\\trunning\\t-\\n\\n'; cat >/dev/null; echo eof > "+marker+"; exit 0")
	writeHub(t, a, "h", nil)
	ctx, cancel := context.WithCancel(context.Background())
	out := newBlockWriter()
	done := make(chan error, 1)
	go func() { done <- a.watchWithHubs(ctx, out, 30*time.Millisecond) }()
	out.next(t, "h\thub\treachable\t-\nh/web\tsmol\trunning\t-\n")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the hub side saw EOF while the watcher was still running")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not stop")
	}
	if data, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(data)) != "eof" {
		t.Errorf("hub-side command did not exit on stdin EOF (marker: %q, %v)", data, err)
	}
	// A hub that does not know the flag fails the pipe cleanly: unreachable,
	// as any failing listing.
	fakeSSH(t, "echo 'Error: unknown flag: --exit-on-stdin-eof' >&2; exit 1")
	ctx2, cancel2 := context.WithCancel(context.Background())
	out2 := newBlockWriter()
	done2 := make(chan error, 1)
	go func() { done2 <- a.watchWithHubs(ctx2, out2, 30*time.Millisecond) }()
	out2.until(t, "with the hub unreachable", func(b string) bool { return strings.HasPrefix(b, "h\thub\tunreachable\t-\n") })
	cancel2()
	<-done2
}
