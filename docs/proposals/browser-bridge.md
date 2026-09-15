# Proposal: a generic guest→host browser bridge and ad-hoc forwards

Status: draft v2.5, 2026-09-15. Order of work lives in `ROADMAP.md` and
nowhere else. Revised after five independent reviews. **Sections 3 and 4 are
the authoritative statement of leases, subscriptions, ownership and bind
policy**; `hub.md` and `ROADMAP.md` reference them and do not restate them.
Scope: `internal/auth`, `internal/session`, `cmd/devvm-agent`, `internal/agentrpc`,
`internal/backend`, `internal/bootstrap`, `internal/cli`.

## Problem

`devvm auth` contains a general-purpose mechanism (guest hands the host a URL;
host bridges any loopback callback port and opens the URL) but it is only
reachable through the three hard-coded logins, and only while `auth` runs:

- `BROWSER` is set only inside the auth session's `bash -lc` execs. A tool run
  from `attach`/`shell` that wants a browser just prints a URL.
- On remote backends the event channel exists only for the life of `auth`. The
  ssh forward daemon owns a ControlMaster and no agent exec.
- On smol, the daemon's agent opens URLs it receives on the default socket but
  never bridges callbacks, and nothing points `BROWSER` at the shim.
- `auth` runs its own `serve --auth` exec (`internal/auth/auth.go`). On smol
  that is a **second parallel agent exec** whenever a forward daemon is up, which
  is exactly what the one-exec rule forbids. It has worked so far because the
  two execs are mostly idle, but it is a latent violation.
- Ad-hoc forwarding is a two-step: notice a dev server on guest `:3000`, run
  `devvm ports add NAME 3000` on the host, then open `localhost:3000` yourself.

## Goals

1. Any program inside the guest can open a URL in the host browser from any
   devvm-started shell, with no `devvm auth` running.
2. A loopback URL from the guest (`http://localhost:3000/…`, an OAuth
   `redirect_uri`, codex's `:1455`) gets forwarded automatically and the guest
   is told which host port was actually bound.
3. `devvm auth` shrinks to "run this login command with `BROWSER` set" and
   loses its private agent session, fixing the one-exec violation.
4. One agent exec per machine, on smol, stays the rule.

## Non-goals

- Reverse forwards (host → guest), non-loopback binds, UDP.
- A general RPC surface for the guest to drive the host CLI.
- Changing how persistent `ports` config works.
- Native Windows hosts (the daemon already needs `flock`); WSL is covered by
  the existing `wslview` opener.

## Design

### 1. The forward daemon becomes the bridge

The per-machine daemon (`internal/session`) already owns the machine's
long-lived channel and the host-port allocator. It gains an event handler that
does what `auth.session.onOpenURL` does today:

```
on open-url request {url}:
  if no subscriber: log the URL; reply {opened: false}; stop
  classify:
      url is loopback (host ∈ {localhost,127.0.0.1,::1}) port P  → kind=direct
      url has a loopback redirect_uri with port P                 → kind=redirect
      otherwise                                                   → kind=external
  if kind != external and the subscriber is local (not a relay):
      if daemon is reconnecting:
          reply {err: "forward pending; retry when the link is back"}; stop
      direct:   bound = add(guest P, prefer P, bump ok, owner = subscriber conn)
      redirect: bound = add(guest P, exact P,          owner = ttl)
                if busy: reply {err: "callback port P is in use on the host"}; stop
  deliver {url, kind, guest: P?, bound?} to the most recent subscriber
      // guest/bound absent for external; bound absent for a relay subscriber
  wait for its reply {opened, url, host_port?}  // 15s; timeout or subscriber
  relay the reply to the guest                  // gone => opened=false
```

**The daemon never opens a browser itself and never rewrites a URL.** A
subscribed client (section 3) rewrites a bumped direct URL for the port it
will actually reach and opens it with `hostbrowser.Open` on its own host. An
`external` URL (a normal login page with no loopback callback) is opened as
is; there is nothing to forward.

**Local versus relay subscribers.** A session declares at open (section 3)
whether the browser opens on this host (`local`, the `attach`/`shell`/`auth`
process here) or the subscriber is **another daemon** reached through a
relay (`hub.md`'s `__session`, held by the laptop's daemon for that machine). For a
local subscriber the daemon binds before delivering, and `bound` is the port
the browser will reach. For a relay subscriber the daemon **binds nothing**:
the event is handed up as-is, arrives on the far daemon's `events()` channel
like any transport event, and that daemon runs this same handler, binding on
its own transport (whose hub side resolves an intermediate hub port for
itself, `hub.md` section 7) and delivering to its own local subscriber. The
relay is recursion, not a second code path. Only the host that shows the
browser needs the exact callback port, so a busy port on the hub never
refuses a login that will open on the laptop.

A `redirect_uri` is never rewritten by anyone: the provider needs it verbatim,
which is why that port must be exact on the host that shows the browser and
why an occupied port there is a refused open rather than a bumped one. The
subscriber's reply is what the daemon relays to the guest, so success means
"bound and handed to a browser", not "delivered to someone".

Locally the subscriber is the `attach`/`shell`/`auth` process on this machine;
for a hub machine it is the laptop's daemon across ssh (see `hub.md`
section 8). With no subscriber the URL is logged and the guest is told it was
not opened. This deliberately
regresses today's smol behaviour, where a URL from any guest shell opened on
the host even with no devvm session running, in exchange for closing the
unattended-open concern in section 7.

`auth.CallbackPort` moves to the daemon package **minus its `codexFixedPort`
exclusion**. It skips 1455 today only because `auth` pre-binds that port
itself before running `codex login`; with the pre-bind gone the exclusion
would leave codex's callback unbridged. Codex's fixed 1455 becomes just
another loopback `redirect_uri`, so the `localhost:1455` special case in
`hostbrowser.Sanitize` goes away too (every forward binds `::1` wherever the
host has an IPv6 loopback, section 4).

**Smol**: the daemon's existing agent exec already carries events; `drainEvents`
in `transport_smol.go` becomes the bridge handler. No new exec.

**Remote (ssh)**: **deferred**, not scheduled in `ROADMAP.md`. Until it
ships, `auth` on a remote backend keeps its private agent session and the
agent keeps `--auth`; ssh has no one-exec limit, so that is duplication, not a
violation. When it does ship: the ssh transport gains an agent exec **on the
daemon's own ControlMaster**. Today `sshBackend.Spawn` goes through `base()`, which uses the
CLI's `cm-%C` auto-master, not the daemon's `<name>.master`
(`internal/backend/ssh.go`, `base()` vs `SSHConn()`), so the backend needs a
`SpawnOn(ctx, controlPath, argv)` (or the transport builds the argv itself).
Forwards stay native `-L`; the exec carries only events.

Two failure modes, handled separately:

- **Master dies** → existing `dead()` path; the agent exec dies with it and is
  respawned by `restore()`.
- **Agent exec dies, master alive** (agent killed, guest reboot of the agent
  socket) → the transport respawns the exec with its own small backoff. Not a
  daemon-level reconnect: forwards are unaffected.

On an adopt host the daemon **never installs**. It probes for an already
installed agent (`~/.local/bin/devvm-agent`, sha match, no writes) and runs
without an event channel if absent. Consent to install stays exactly where it is
today: `auth --install-agent` or the interactive prompt in `auth`.

The transport interface grows one method:

```go
type transport interface {
    forward(hostPort, guestPort int) (io.Closer, error) // 127.0.0.1 + best-effort ::1
    events() <-chan bridgeRequest    // nil when no agent (adopt host w/o consent)
    dead() <-chan struct{}
    Close() error
}
```

`bridgeRequest` carries the event plus a reply func, so the daemon's loop can
select on it alongside `dead()` and the idle timer.

### 2. Request/response events

Today an event stream is write-and-close; the guest never learns what the host
did, and the daemon's "refusing to open" / "open this URL yourself" fallbacks go
to `<name>.log` where nobody sees them. The `open-url` event becomes a request:
the agent writes the event, then reads one JSON reply line and prints it to the
shim's stderr:

```
devvm: opened on host -> http://localhost:3001/   (guest 3000, host port bumped)
devvm: host refused non-http(s) URL: file:///…
devvm: forward pending (host is reconnecting); retry in a moment
```

This is an agent-protocol change, so `./build.sh` lands with the bridge step
in `ROADMAP.md`. The reply is backwards-compatible: an old agent that closes
the stream after writing gets its reply discarded. The reply the agent prints
is whatever the subscriber reported, so on a hub machine it names the
laptop's port.

### 3. Session leases: keep the daemon alive while something needs the bridge

The daemon idle-exits after 60s with zero forwards (`daemon.loop`), and a
machine with no configured ports never gets a daemon at all (`tunnelUp` returns
before `Dial`). Without a fix, a `BROWSER` shim in `attach` points at a bridge
that vanishes a minute later, and thin `auth` would lose its bridge mid-login if
the user takes more than a minute. So:

- **A long-lived control connection is a session, and a session holds the
  daemon.** Today every control connection is one request and a 30-second
  read deadline. A client that sends `session {relay: bool}` instead keeps
  the connection open; it may then `subscribe`, and it may `add` forwards
  owned by that connection (`connection` owners, section 4). `relay` is
  declared once, at open: it says the far end is another daemon
  (`hub.md`'s `__session`), not a browser on this host, and both the
  subscription (section 1) and the transport-loss rule below inherit it.
  The daemon counts open sessions. Idle rule becomes `forwards == 0 && sessions == 0`, in both
  `loop()` and `reconnect()`. There is no separate `hold` op: holding is what
  a session connection does by existing, subscribed or not. That matters for
  `hub.md`'s `__session`, which can sit connected with no forwards and no
  subscription; if it did not hold, the hub daemon would idle-exit under it
  and the laptop's reconnect would respawn it every 60 seconds.
- `subscribe` and `unsubscribe` are messages on a session connection.
  `subscribe` is acknowledged: the reply is sent only once the subscription
  is registered, so a client that waits for the reply knows every later
  event reaches it. A daemon that is itself a relay subscriber upstream (the
  laptop daemon on a hub, `hub.md` section 8) acknowledges a local
  `subscribe` only after its own upstream `subscribe` is acknowledged, so
  the guarantee composes across hops. Re-sending `subscribe` on an already
  subscribed session moves it to the front (the laptop daemon does this on
  every new local subscriber, `hub.md` section 8). Events go to the most
  recently registered subscriber; when it unsubscribes or disconnects, the
  next most recent one, if any, receives them.
- **Wire shape.** Today's protocol is one request, one reply, one
  connection, with no correlation. A session connection carries requests,
  their replies, events and event replies on one pipe, so every request
  carries an `id` its reply echoes, and every event carries an `id` the
  subscriber's reply names. Each side runs **one reader for the
  connection's life** and serializes its writes; a reader never blocks
  waiting for a reply, because a reply can queue behind an event (the
  laptop daemon's bridge handler sends `add` on this pipe while it is
  handling an event, and the answer to that `add` shares the pipe with the
  event's reply). A reply naming an `id` the daemon no longer holds (the
  event timed out, or the subscriber that got it is gone) is a late reply
  and is dropped. `hub.md`'s `__session` relays these lines verbatim and
  adds nothing of its own.
- **One session client.** `attach`, `shell`, `auth` and the laptop daemon's
  hub transport share one client in `internal/session` that owns dial (and
  spawn), `session`, `subscribe` with its ack, and **reconnect**: when the
  connection drops (the daemon was cycled by `update` or `stop`, or died)
  it redials with backoff, opens a new session and re-subscribes, so a held
  `attach` regains browser support without being reattached. The forwards
  the old connection owned are gone with it and come back on the next open
  (section 4). After `update` the reconnecting client is the old binary
  talking to a new daemon; the session ops are a compatibility surface
  versioned by a floor like `--plain`, and the daemon's `ping` version is
  what a client checks before assuming an op exists.
- **Transport loss closes relay sessions.** When the daemon's transport
  dies it closes every session opened with `relay: true`, dropping the
  forwards they own; local sessions and their forwards stay, and
  `restore()` re-binds those as today. The reason is in `hub.md` section 7:
  a relay's forwards are intermediate hops whose host port `restore()` may
  bump, and the far daemon has no way to learn the new port except by
  re-adding through its own reconnect.
- `DEVVM_NO_SUBSCRIBE=1` makes `attach`/`shell`/`auth` skip the daemon
  entirely: no dial, no session, no subscription. The hub proxy sets it on
  every proxied command so the laptop's subscription is the only one for that
  session (`hub.md` section 8); the laptop daemon's `__session` already holds
  the hub daemon, so the proxied process has nothing to hold.
- `attach`, `shell`, and `auth` dial the daemon (spawning it if needed), open
  a session, and subscribe for their lifetime. When the interactive session
  ends the connection closes, the session count drops, and the normal idle
  rule applies.
- A session also bounds the trust surface (section 7): the guest can only
  reach the host browser while a devvm session is active or forwards are up.

### 4. Ownership, bind policy, and ephemeral forwards

`daemon.add` has no notion of who owns a forward, and `remove` is
unconditional. Both proposals need ownership, so define it once:

```go
type fwd struct {
    host, guest int
    exact       bool               // host port may not bump, now or on restore
    closer      io.Closer
    owners      map[owner]struct{} // torn down when empty
}
// owner kinds: conf (added by `ports add`/`ports up`), connection (a held
// session connection; dropped when it closes — hub.md's __session), ttl
// (the bridge's ephemeral forwards; dropped at expiry).
```

Owners decide lifetime and nothing else. Who drops what:

- `ports rm` on a configured mapping drops `conf`; on an unconfigured guest
  port with a live forward it drops `ttl` (today it refuses anything not in
  the conf). It never drops a `connection` owner: that belongs to whoever
  holds the connection.
- `ports down` drops every `conf` owner and stops the daemon only if no
  forward and no session remains. Today it stops the daemon outright; with hub
  forwards that would loop (`hub.md` section 7).
- `update` (`restartDaemons`) today skips a daemon that holds no configured
  forward, "left to exit on its own". A daemon holding `connection` forwards
  never exits on its own, so that skip would leave old code running after a
  desktop update. It cycles every daemon that is up; a hub `__session` on the
  other side reconnects through its own daemon's backoff, a local `attach`
  reconnects and re-subscribes through the session client (section 3), and
  its direct forwards are re-created on the next open.
- A closing connection drops itself; the expiry ticker drops `ttl`.
- `ports add NAME PORT` on a live forward adds `conf` and touches no other
  owner. A `ttl` left behind expires harmlessly because `conf` keeps the
  forward alive; a `connection` owner is still someone's session.
- The forward is closed only when no owner remains. "Ephemeral" below means
  "has no `conf` owner".

**Bind policy is a property of the request, kept for the forward's life.**
`add` takes the guest port, a preferred host port, and `exact bool`, and the
forward record retains `exact`. Configured forwards and direct opens bump as
today; a `redirect_uri` callback is exact and fails with `errPortBusy` if the
port is taken, which the bridge turns into a refused open with a reply the
guest can read. Today's `ensureCallback` also binds the exact port; it just
fails silently. **`restore()` honours `exact`**: today it re-binds every
forward with bumping allowed, which would silently move a callback port that
another process grabbed during the outage while the browser still targets the
original. An exact forward whose port is unavailable after a reconnect stays
pending (visible in `ports list`) and is never bumped. Today a pending
forward is re-bound only by a later `add` for the same guest port or by the
next transport reconnect, so freeing the port would change nothing until one
of those happened; the daemon's expiry ticker (the one that drops `ttl`
owners) therefore also retries every pending exact bind while the transport
is up. A freed callback port comes back within one tick; one that never
frees expires with its `ttl` owner. **`exact` is sticky**: a request that
reuses an existing forward (below) and asks for exact sets it for the rest
of the forward's life, including after the owner that asked has expired.
Recomputing it per owner would add policy for a distinction nobody sees;
the cost is that a configured forward that once carried a callback stays
pending instead of bumping if its port is taken during a later outage, and
`ports down`/`ports up` clears that.

**Every forward is dual-stack, best-effort on `::1`.** `auth.ensureCallback`
binds both `127.0.0.1:P` and `[::1]:P` because macOS resolves `localhost` to
`::1` first. Both transports' `forward` bind only IPv4 today. Every forward
now binds both: smol adds a second listener; ssh uses one spec,
`-L localhost:P:localhost:G`, which OpenSSH binds on every address
`localhost` resolves to, so there is still one `-O cancel` per forward. The
IPv4 bind decides busy-or-free exactly as today. A `::1` bind that fails for
any reason is logged and tolerated, as `ensureCallback` tolerates it now:
the forward is IPv4-only on that host. Classifying `EADDRINUSE` on `::1` as
"busy" was considered and dropped; it needs errno inspection on two
transports and a v4 unbind for a conflict (another process on `[::1]:P` but
not `127.0.0.1:P`) that the current best-effort code has never hit. With
that rule there is no per-request family choice. On ssh, "the IPv4 bind
decides" is the transport's existing `127.0.0.1` pre-probe before `-O
forward`, not ssh's own result: OpenSSH reports a `localhost:` spec as bound
when any one of its addresses bound. The accepted residual case, stated so
it is not later mistaken for an oversight: another process holds `[::1]:P`
but not `127.0.0.1:P`, on a host that resolves `localhost` to `::1` first
(macOS), and the browser's callback reaches that process instead of the
forward. When nothing listens on `::1` the browser falls back to
`127.0.0.1`, so an IPv4-only forward, new or reused, is reachable in every
other case.

Which owner a bridge open gets:

- **Direct loopback URL** (`http://localhost:3000/`): the subscribing
  client's connection. The forward lives as long as the session that opened
  it, so a dev-server tab does not die ten minutes into a held `attach`.
- **`redirect_uri` callback**: `ttl`, fixed at creation (proposed 10 minutes)
  and refreshed when a new open names the same port. Callbacks are single-use
  by nature; a session-length owner would pin a well-known port for no
  reason. Wall-clock, with no pause while reconnecting: a callback whose
  bridge was down for ten minutes belongs to a login that is dead anyway, so
  expiring it on the first tick after `restore()` is the right outcome and
  saves a clock. TTL rather than last-use because ssh forwards are native
  `-L` and the daemon never sees their connections.

**Reuse.** A request for a guest port that is already forwarded adds its
owner to the existing forward and returns the existing host port, **unless**
the request is exact and the existing host port differs (a configured `1455`
that got bumped to `1456`), in which case it is refused like any busy exact
bind. An exact request that matches the existing host port makes the
forward exact from then on (sticky, above); without that a callback could
reuse a bumpable forward and be moved out from under the browser on the
next reconnect.

**Limits.** Privileged ports (< 1024) are refused; at most 20 live ephemeral
forwards per machine. `session.Forward` gains `Ephemeral bool` and `Expires`;
`ports list` already prints `(ephemeral)` and now also shows the owner kind
and remaining TTL. `update` and `stop` cycle the daemon and drop ephemerals.
Documented, not fixed: they are ad hoc by definition.

### 5. `BROWSER` in the guest

The shim (`devvm-open-url`) is installed beside the agent **by the same
`agentbin.Install` call**, one idempotent operation. Today only `auth` writes
it (`installBrowserShim`), and the smol transport installs the agent alone;
folding the shim in means a smol daemon that is up has both, which is what
`hub.md` section 8 relies on for hub VMs, and what `attach` relies on the
moment it subscribes (step 7), before `bootstrap` installs anything (step
9). The shim becomes `BROWSER` wherever devvm can arrange it without
writing to an adopt host:

| Path | smol | remote-managed | remote-unmanaged |
|---|---|---|---|
| `shell` | `ExecOpts.Env` | profile.d | not set (see note) |
| `attach` | `ExecOpts.Env` + tmux `-e` | tmux `-e` + profile.d | tmux `-e` if agent installed |
| other shells (cron, another ssh client) | profile.d | profile.d | not set |

Notes:

- Bare remote `shell` sends **no remote command** on purpose so the login shell
  is honoured (`sshConnect`, `moshConnect`), and sshd drops `SetEnv` unless
  `AcceptEnv` allows it. So on remote, `BROWSER` comes from tmux or profile.d,
  never from the exec. On an adopt host without profile.d, bare `shell` does not
  get it; `attach` does.
- `attach` joins an existing tmux server, so `Env` on the attach exec does not
  reach existing panes. Use `tmux new-session -A -s dev -e BROWSER=…`
  (tmux ≥ 3.2; on older tmux fall back to profile.d only). ssh `Attach` already
  runs `new-session -A`; smol's keeper in `ensureTmux` gets the same flag.
- **Managed boxes install the agent and shim at `bootstrap`**, not lazily at
  first `auth`. Otherwise profile.d would point at a file that does not exist
  yet and `gh` would fail outright instead of printing the URL.
- `/etc/profile.d/devvm.sh` is `export BROWSER=/usr/local/bin/devvm-open-url`
  guarded by `[ -z "$BROWSER" ] && [ -x … ]`. It is sh/bash only; fish and zsh
  users on managed boxes get it via tmux `-e` or set it themselves. Documented.
- The shim already prints the URL when no agent socket answers, so a stale
  `BROWSER` is harmless.
- The `agent-auth.sock` / `--auth` distinction goes away on smol: one socket,
  owned by the daemon's agent. It stays for remote backends until the ssh
  agent exec ships.

### 6. `auth` becomes thin

Where the daemon's transport has events, meaning smol today and hub machines
through the hub daemon:

```go
func Authenticate(ctx, b, m, tools, approve) error {
    agentPath, shim := agentbin.Install(...)   // agent + shim, one op; consent gate unchanged (adopt hosts)
    cl := session.Dial(configDir, name)        // the session client (section 3): spawn, session, reconnect
    release := cl.Subscribe(); defer release() // acked; bridge alive across logins
                                               // DEVVM_NO_SUBSCRIBE=1: skip the daemon
    for tool := range tools { s.login(tool) }  // BROWSER=shim, as today
}
```

On a remote backend `auth` keeps its private session unchanged until the ssh
agent exec lands (section 1, Remote). `Authenticate` branches on whether the
daemon reports an event channel; the private-session code is deleted with that
step, not before.

`auth` keeps: the login table, the `installed` probe, `isSignalExit`, and the
codex device-auth preference. It drops: its yamux session, event loop, callback
map, `stream.go`, and `CallbackPort` (moved to the daemon).

### 7. Trust boundary

Today's smol daemon already opens any http(s) URL the guest sends, unattended.
This proposal extends that to ssh, makes it permanent while a session is held or
forwards are up, and lets any guest process request a loopback bind. A
compromised guest process could pop browser tabs and point the host browser at a
host-loopback origin it controls. Bounds:

- Only `127.0.0.1`/`::1` are ever bound, never `0.0.0.0`. Unchanged.
- The bump allocator never displaces an existing host listener. Unchanged.
- `hostbrowser.Open` refuses non-http(s) and option-like URLs. Unchanged.
- Privileged ports refused; 20 live ephemerals max; ephemerals expire.
- Opens rate-limited (proposed 10 per minute per machine; excess are logged and
  replied to the guest as refused).
- URLs open only while a client is subscribed, i.e. while someone is actually
  in a devvm session on the machine that will show the browser. With nothing
  held and no forwards, the daemon exits and the shim degrades to printing.

What a forward can *reach* is unchanged; this widens *who* can request one.

### 8. `status --plain` and the menubar app

`up:N` counts every daemon forward today. If ephemerals count, a machine with
no configured ports would flicker `-` → `up:1` → `-` on every login, and the
menubar badge with it. Decision: **N counts configured forwards only, and
`up:0` is never emitted.** The app renders `up:0` as "0 forwards up", so a
daemon with zero `conf` forwards reports `down` when the conf lists ports
(they are configured and not up) and `-` when it lists none, the same two
tokens a machine with no daemon gets. That keeps the contract's meaning of
every token and needs no Swift change. Ephemerals stay visible in
`ports list`. If the menubar later wants to offer "make persistent", add a
fifth `ephemeral:N` token then; not now.

One visible side effect: `status -v` skips the live-resource probe whenever
a daemon exists (it must not open a second exec), and with sessions holding
daemons through every `attach`, that probe will usually be skipped and the
conf sizes shown instead. Cosmetic; noted so it is not later mistaken for a
bug. Routing the probe through the daemon's agent channel is a small follow-up
if it matters.

## CLI surface

No new verbs. Visible changes:

- `ports list`: ephemeral forwards labelled with owner kind and remaining
  TTL; an exact forward whose port is taken after a reconnect shows `pending`.
- `ports add NAME PORT` on an ephemeral forward promotes it in place.
- `ports rm NAME PORT` can remove a daemon-only `ttl` forward.
- `ports down` leaves the daemon up while other owners or sessions remain.
- `attach`/`shell`/`auth` open a session and subscribe unless
  `DEVVM_NO_SUBSCRIBE=1`, in which case they do not touch the daemon. They
  reconnect and re-subscribe on their own if the daemon is cycled under
  them.
- `update` cycles every daemon that is up, including ones holding only
  session-owned forwards.
- `auth --install-agent` unchanged and remains the only consent path.
- Guest side: `devvm-open-url URL|PORT` prints what the host did.

## Order of work

Lives in `ROADMAP.md`, the only place sequencing is written down.

## Open questions

- Ephemeral TTL: 10 minutes proposed for `redirect_uri` callbacks. Direct
  opens are session-owned and carry no TTL, so there is only one knob.
- Should `start` open a session too, or only interactive commands? Proposed:
  no. `start` brings up configured forwards, and those already keep the daemon
  alive.
- tmux `-e` needs 3.2 (2021). Do we care about older guests? Proposed: fall
  back silently to profile.d.
