package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

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
}

func (f *fakeBridge) Ping() (fwdsession.PingInfo, error) {
	f.log.add("ping")
	state := f.state
	if state == "" {
		state = fwdsession.StateUp
	}
	return fwdsession.PingInfo{State: state, Version: f.version, Shim: f.shim}, nil
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

// stubInstall replaces agentbin.Install, counting calls.
func stubInstall(t *testing.T, log *thinLog, err error) {
	t.Helper()
	old := installAgent
	installAgent = func(context.Context, backend.Backend, *config.Machine, func() error) (agentbin.Paths, error) {
		log.add("INSTALL")
		return agentbin.Paths{}, err
	}
	t.Cleanup(func() { installAgent = old })
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

// A remote backend still takes the private-session branch: the consent-
// gated install (here refused), never the daemon.
func TestAuthRemoteTakesPrivateSession(t *testing.T) {
	log := &thinLog{}
	stubErr := errors.New("stub: install refused")
	stubInstall(t, log, stubErr)
	for _, be := range []string{"managed", "unmanaged"} {
		m := config.NewRemote("box", be == "managed", "")
		err := Run(context.Background(), guestBackend{log: log}, m, []string{"github"}, Options{
			OpenBridge: func() (Bridge, error) { log.add("dial"); return nil, errors.New("no daemon") },
		})
		if !errors.Is(err, stubErr) {
			t.Fatalf("%s: err = %v, want the private session's install", be, err)
		}
	}
	if got := log.String(); got != "INSTALL\nINSTALL" {
		t.Fatalf("remote auth ran:\n%s", got)
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
