package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/agentbin"
	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	fwdsession "github.com/smweber/devvm/internal/session"
)

// Thin auth (browser-bridge.md §6, roadmap step 8), with the daemon and the
// guest stood in for: a recorder shared by the bridge and the backend, so
// the order of dial, ping, subscribe and logins is asserted as it
// happened.

type thinLog struct {
	mu  sync.Mutex
	evs []string
}

func (l *thinLog) add(format string, a ...any) {
	l.mu.Lock()
	l.evs = append(l.evs, fmt.Sprintf(format, a...))
	l.mu.Unlock()
}

func (l *thinLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.evs, "\n")
}

type fakeBridge struct {
	log     *thinLog
	shim    string
	state   string // "" reads as up
	version string
	// shimAfterKick: the shim a kick makes the daemon report (it picked up
	// an agent auth installed); "" leaves shim as it is.
	shimAfterKick string
}

func (f *fakeBridge) Ping() (fwdsession.PingInfo, error) {
	f.log.add("ping")
	state := f.state
	if state == "" {
		state = fwdsession.StateUp
	}
	return fwdsession.PingInfo{State: state, Version: f.version, Shim: f.shim}, nil
}
func (f *fakeBridge) Kick() error {
	f.log.add("kick")
	if f.shimAfterKick != "" {
		f.shim = f.shimAfterKick
	}
	return nil
}
func (f *fakeBridge) Subscribe() error { f.log.add("subscribe"); return nil }
func (f *fakeBridge) Close() error     { f.log.add("close"); return nil }
func (f *fakeBridge) Add(pref, guest int, exact bool) (int, bool, bool, error) {
	f.log.add("add %d exact=%v", guest, exact)
	return pref, false, false, nil
}

// guestBackend answers the login probes (every tool installed, no codex
// device auth) and records each run; an exec (Spawn) is recorded too, so
// a test can prove none happens.
type guestBackend struct {
	backend.Backend
	log *thinLog
}

func (b guestBackend) Run(_ context.Context, o backend.ExecOpts, argv ...string) error {
	script := argv[len(argv)-1]
	switch {
	case strings.Contains(script, "command -v"):
		b.log.add("probe %s", strings.Fields(script)[2])
	case strings.Contains(script, "--help"):
		if o.Stdout != nil {
			io.WriteString(o.Stdout, "Usage: codex login [--with-api-key]") // no --device-auth
		}
	default:
		b.log.add("login tty=%v BROWSER=%s: %s", o.TTY, o.Env["BROWSER"], script)
	}
	return nil
}

func (b guestBackend) Spawn(_ context.Context, _ backend.ExecOpts, argv ...string) (*backend.Session, error) {
	b.log.add("SPAWN %s", strings.Join(argv, " "))
	return nil, errors.New("no exec in this test")
}

// stubInstall replaces agentbin.Install, logging calls, and agentbin.Probe,
// answering st.
func stubInstall(t *testing.T, log *thinLog, err error) {
	t.Helper()
	stubAgent(t, log, err, agentbin.Missing)
}

func stubAgent(t *testing.T, log *thinLog, err error, st agentbin.State) {
	t.Helper()
	oldI, oldP := installAgent, probeAgent
	installAgent = func(_ context.Context, _ backend.Backend, _ *config.Machine, approve func() error) (agentbin.Paths, error) {
		log.add("INSTALL approve=%v", approve != nil)
		return agentbin.Paths{}, err
	}
	probeAgent = func(context.Context, backend.Backend, *config.Machine) (agentbin.Paths, agentbin.State, error) {
		log.add("PROBE")
		return agentbin.Paths{}, st, nil
	}
	t.Cleanup(func() { installAgent, probeAgent = oldI, oldP })
}

// Thin auth on smol dials the daemon, reads the shim from ping, subscribes
// (acked) and only then runs the login table with BROWSER set to that shim;
// codex's fixed callback is bound as an exact forward on the session. It
// never installs anything and never spawns an exec: the daemon's one agent
// exec is the only one in the VM (the one-exec rule).
func TestThinAuthNeverInstallsOrSpawns(t *testing.T) {
	log := &thinLog{}
	stubInstall(t, log, nil)
	m := &config.Machine{Name: "web", Backend: config.BackendSmol}
	br := &fakeBridge{log: log, shim: "/usr/local/bin/devvm-open-url"}
	err := Run(context.Background(), guestBackend{log: log}, m, []string{"github", "codex"}, Options{
		OpenBridge: func() (Bridge, error) { log.add("dial"); return br, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	got := log.String()
	if strings.Contains(got, "INSTALL") || strings.Contains(got, "SPAWN") {
		t.Fatalf("thin auth installed or spawned:\n%s", got)
	}
	want := strings.Join([]string{
		"dial",
		"ping",
		"subscribe",
		"probe gh",
		"login tty=true BROWSER=/usr/local/bin/devvm-open-url: unset GH_TOKEN GITHUB_TOKEN; gh auth login && gh auth setup-git && gh config set git_protocol https --host github.com",
		"probe codex",
		"add 1455 exact=true",
		"login tty=true BROWSER=/usr/local/bin/devvm-open-url: exec codex login",
		"close",
	}, "\n")
	if got != want {
		t.Fatalf("thin auth ran:\n%s\nwant:\n%s", got, want)
	}
}

// Thin auth on a remote box is the smol path (the private session is
// gone): a daemon that reports its shim is dialed, pinged and subscribed,
// and nothing is probed, installed or spawned from here, managed or not.
func TestThinAuthRemote(t *testing.T) {
	for _, managed := range []bool{true, false} {
		log := &thinLog{}
		stubInstall(t, log, nil)
		m := config.NewRemote("box", managed, "dev@box")
		br := &fakeBridge{log: log, shim: "/home/dev/.local/bin/devvm-open-url", version: "v1"}
		err := Run(context.Background(), guestBackend{log: log}, m, []string{"github"}, Options{
			OpenBridge: func() (Bridge, error) { log.add("dial"); return br, nil },
			Approve:    func(bool) error { log.add("APPROVE"); return nil },
			Version:    "v1",
		})
		if err != nil {
			t.Fatal(err)
		}
		want := "dial\nping\nsubscribe\nprobe gh\n" +
			"login tty=true BROWSER=/home/dev/.local/bin/devvm-open-url: unset GH_TOKEN GITHUB_TOKEN; gh auth login && gh auth setup-git && gh config set git_protocol https --host github.com\nclose"
		if got := log.String(); got != want {
			t.Fatalf("managed=%v: remote auth ran:\n%s\nwant:\n%s", managed, got, want)
		}
	}
}

// An adopt host whose daemon found no current agent: auth probes (over its
// own ssh), asks for consent worded by what it found (missing, or
// outdated), installs, kicks the daemon so it looks again, and only once
// a ping reports the shim subscribes and runs the logins. Declined, nothing
// is installed and no login runs. A managed box needs no consent.
func TestThinAuthAdoptHostInstallsWithConsent(t *testing.T) {
	const shim = "/home/dev/.local/bin/devvm-open-url"
	for _, tc := range []struct {
		name    string
		managed bool
		st      agentbin.State
		decline bool
		want    string
	}{
		{"missing", false, agentbin.Missing, false,
			"dial\nping\nPROBE\nAPPROVE outdated=false\nINSTALL approve=false\nkick\nping\nsubscribe\nprobe codex\nadd 1455 exact=true\nlogin tty=true BROWSER=" + shim + ": exec codex login\nclose"},
		{"outdated", false, agentbin.Stale, false,
			"dial\nping\nPROBE\nAPPROVE outdated=true\nINSTALL approve=false\nkick\nping\nsubscribe\nprobe codex\nadd 1455 exact=true\nlogin tty=true BROWSER=" + shim + ": exec codex login\nclose"},
		{"current, daemon slow to notice", false, agentbin.Current, false,
			"dial\nping\nPROBE\nkick\nping\nsubscribe\nprobe codex\nadd 1455 exact=true\nlogin tty=true BROWSER=" + shim + ": exec codex login\nclose"},
		{"declined", false, agentbin.Missing, true,
			"dial\nping\nPROBE\nAPPROVE outdated=false\nclose"},
		{"managed", true, agentbin.Missing, false,
			"dial\nping\nPROBE\nINSTALL approve=false\nkick\nping\nsubscribe\nprobe codex\nadd 1455 exact=true\nlogin tty=true BROWSER=" + shim + ": exec codex login\nclose"},
	} {
		log := &thinLog{}
		stubAgent(t, log, nil, tc.st)
		m := config.NewRemote("box", tc.managed, "dev@box")
		br := &fakeBridge{log: log, version: "v1", shimAfterKick: shim}
		o := Options{
			OpenBridge: func() (Bridge, error) { log.add("dial"); return br, nil },
			Version:    "v1",
		}
		if !tc.managed {
			o.Approve = func(outdated bool) error {
				log.add("APPROVE outdated=%v", outdated)
				if tc.decline {
					return errors.New("agent install declined")
				}
				return nil
			}
		}
		err := Run(context.Background(), guestBackend{log: log}, m, []string{"codex"}, o)
		if (err != nil) != tc.decline {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
		if got := log.String(); got != tc.want {
			t.Errorf("%s: ran:\n%s\nwant:\n%s", tc.name, got, tc.want)
		}
	}
}

// A daemon that never reports the shim after the install is an error
// naming the wait, not a login with BROWSER unset; and a daemon from
// another build is never handed an agent (it would not look for one).
func TestThinAuthRemotePickupTimesOut(t *testing.T) {
	oldW, oldP := agentPickupWait, agentPickupPoll
	agentPickupWait, agentPickupPoll = 30*time.Millisecond, time.Millisecond
	t.Cleanup(func() { agentPickupWait, agentPickupPoll = oldW, oldP })
	log := &thinLog{}
	stubAgent(t, log, nil, agentbin.Current)
	m := config.NewRemote("box", true, "dev@box")
	br := &fakeBridge{log: log, version: "v1"}
	err := Run(context.Background(), guestBackend{log: log}, m, []string{"github"}, Options{
		OpenBridge: func() (Bridge, error) { return br, nil },
		Version:    "v1",
	})
	if err == nil || !strings.Contains(err.Error(), "did not pick up the devvm agent") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(log.String(), "login") || strings.Contains(log.String(), "subscribe") {
		t.Fatalf("ran logins without a shim:\n%s", log)
	}

	log = &thinLog{}
	stubAgent(t, log, nil, agentbin.Missing)
	br = &fakeBridge{log: log, version: "v0.1.18"}
	err = Run(context.Background(), guestBackend{log: log}, m, []string{"github"}, Options{
		OpenBridge: func() (Bridge, error) { return br, nil },
		Version:    "v1",
	})
	if err == nil || !strings.Contains(err.Error(), "runs another build of devvm") {
		t.Fatalf("another build: err = %v", err)
	}
	if got := log.String(); got != "ping\nclose" {
		t.Fatalf("another build: ran:\n%s", got)
	}
}

// Under DEVVM_NO_SUBSCRIBE (the hub side of a proxied `auth HUB/NAME`,
// whose laptop holds the subscription) the daemon is neither dialed nor
// subscribed and nothing is bound on this host; the shim comes from a
// ping of a daemon already up. Without one, BROWSER is left alone.
func TestThinAuthNoSubscribe(t *testing.T) {
	log := &thinLog{}
	stubInstall(t, log, nil)
	m := &config.Machine{Name: "web", Backend: config.BackendSmol}
	o := Options{
		NoSubscribe: true,
		OpenBridge:  func() (Bridge, error) { log.add("dial"); return nil, errors.New("must not dial") },
		PingShim:    func() (string, error) { log.add("ping-existing"); return "/usr/local/bin/devvm-open-url", nil },
	}
	if err := Run(context.Background(), guestBackend{log: log}, m, []string{"codex"}, o); err != nil {
		t.Fatal(err)
	}
	want := "ping-existing\nprobe codex\nlogin tty=true BROWSER=/usr/local/bin/devvm-open-url: exec codex login"
	if got := log.String(); got != want {
		t.Fatalf("no-subscribe auth ran:\n%s\nwant:\n%s", got, want)
	}

	log = &thinLog{}
	o.PingShim = func() (string, error) { return "", errors.New("no forward daemon running") }
	if err := Run(context.Background(), guestBackend{log: log}, m, []string{"github"}, o); err != nil {
		t.Fatal(err)
	}
	if got := log.String(); !strings.Contains(got, "login tty=true BROWSER=: ") {
		t.Fatalf("with no shim known BROWSER must be left unset:\n%s", got)
	}
}

// A daemon that reports no shim is an error naming why, read from the same
// ping: reconnecting (retry), a daemon from before thin auth (another
// build: restart it), or neither. Nothing is installed in its place and no
// login runs.
func TestThinAuthDaemonWithoutShim(t *testing.T) {
	m := &config.Machine{Name: "web", Backend: config.BackendSmol}
	for _, tc := range []struct {
		name          string
		br            fakeBridge
		want, notWant string
	}{
		{"reconnecting", fakeBridge{state: fwdsession.StateReconnecting, version: "v0.1.15"},
			"is reconnecting and reports no browser shim until its transport is back; retry", "ports down"},
		{"another build", fakeBridge{version: "v0.1.14"},
			"runs another build of devvm (v0.1.14; this is v0.1.15) and reports no browser shim; restart it ('devvm ports down web'", "retry in a moment"},
		{"same build", fakeBridge{version: "v0.1.15"},
			"the forward daemon for 'web' reports no browser shim; restart it ('devvm ports down web'", "another build"},
	} {
		log := &thinLog{}
		stubInstall(t, log, nil)
		br := tc.br
		br.log = log
		err := Run(context.Background(), guestBackend{log: log}, m, []string{"github"}, Options{
			OpenBridge: func() (Bridge, error) { return &br, nil },
			Version:    "v0.1.15",
		})
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.notWant) {
			t.Errorf("%s: err = %v\nwant it to say %q", tc.name, err, tc.want)
		}
		if got := log.String(); got != "ping\nclose" {
			t.Errorf("%s: ran:\n%s", tc.name, got)
		}
	}
}

func TestCallbackPorts(t *testing.T) {
	if got := CallbackPorts([]string{"github", "codex", "claude"}); len(got) != 1 || got[0] != codexFixedPort {
		t.Fatalf("CallbackPorts(all) = %v", got)
	}
	if got := CallbackPorts([]string{"github"}); len(got) != 0 {
		t.Fatalf("CallbackPorts(github) = %v", got)
	}
}
