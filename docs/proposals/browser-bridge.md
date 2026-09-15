# Proposal: a generic guest→host browser bridge and ad-hoc forwards

Status: draft v2.2, 2026-09-15. Order of work lives in `ROADMAP.md` and
nowhere else. Revised after two independent reviews and aligned with `hub.md`
(client-side browser opening, shared forward ownership, exact-port callbacks).
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
  if daemon is reconnecting and url (or its redirect_uri) is loopback:
      reply {err: "forward pending; retry when the link is back"}; stop
  if url is loopback (host ∈ {localhost,127.0.0.1,::1}) with port P:
      kind = direct
      bound = add(guest P, prefer P, bump ok, owner = subscriber's connection)
  else if url has redirect_uri that is loopback with port P:
      kind = redirect
      bound = add(guest P, exact P, owner = ttl)
      if busy: reply {err: "callback port P is in use on the host"}; stop
  deliver {url, guest: P, kind, bound} to the most recent subscriber
  wait for its reply {opened, url, host_port}   // 15s; timeout or subscriber
  relay the reply to the guest                  // gone => opened=false
```

**The daemon never opens a browser itself and never rewrites a URL.** A
client that holds the daemon (section 3) may also subscribe to events; the
most recently subscribed client rewrites a bumped direct URL for the port it
will actually reach and opens it with `hostbrowser.Open` on its own host. For
a local subscriber that port is `bound`; a laptop subscriber on a hub machine
first adds its own forward to `bound` and rewrites for that (`hub.md` section
8). A `redirect_uri` is never rewritten by anyone: the provider needs it
verbatim, which is why that port must be exact on the host that shows the
browser and why an occupied port is a refused open rather than a bumped one.
The subscriber's reply is what the daemon relays to the guest, so success
means "bound and handed to a browser", not "delivered to someone".

Locally the subscriber is the `attach`/`shell`/`auth` process on this machine;
for a hub machine it is the laptop's subscription across ssh (see `hub.md`
section 8). With no subscriber the URL is logged and the guest is told it was not opened. This deliberately
regresses today's smol behaviour, where a URL from any guest shell opened on
the host even with no devvm session running, in exchange for closing the
unattended-open concern in section 7.

`auth.CallbackPort` moves to the daemon package **minus its `codexFixedPort`
exclusion**. It skips 1455 today only because `auth` pre-binds that port
itself before running `codex login`; with the pre-bind gone the exclusion
would leave codex's callback unbridged. Codex's fixed 1455 becomes just
another loopback `redirect_uri`, so the `localhost:1455` special case in
`hostbrowser.Sanitize` goes away too (every forward binds `::1`, section 4).

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

- New control op `hold`: the client keeps the control connection open; the
  daemon counts open holders. Idle rule becomes `forwards == 0 && holders == 0`,
  in both `loop()` and `reconnect()`. A held connection may also `subscribe`
  to events (section 1). `subscribe` is acknowledged: its reply is sent only
  once the subscription is registered, so a client that waits for the reply
  knows every later event reaches it. `hub.md`'s `__subscribe` helper prints
  `ready` on that ack and the laptop starts the login only after it.
- `DEVVM_NO_SUBSCRIBE=1` makes `attach`/`shell`/`auth` hold without
  subscribing. The hub proxy sets it on every proxied command so the laptop's
  subscription is the only one for that session (`hub.md` section 8).
- `attach`, `shell`, and `auth` dial the daemon (spawning it if needed) and
  hold for their lifetime. When the interactive session ends the hold drops and
  the normal idle rule applies.
- The hold also bounds the trust surface (section 7): the guest can only reach
  the host browser while a devvm session is active or forwards are up.

### 4. Ownership, bind policy, and ephemeral forwards

`daemon.add` has no notion of who owns a forward, and `remove` is
unconditional. Both proposals need ownership, so define it once:

```go
type fwd struct {
    host, guest int
    closer      io.Closer
    owners      map[owner]struct{} // torn down when empty
}
// owner kinds: conf (added by `ports add`/`ports up`), connection (a held
// control connection; dropped when it closes — hub.md's __forward), ttl
// (the bridge's ephemeral forwards; dropped at expiry).
```

Owners decide lifetime and nothing else. Who drops what:

- `ports rm` on a configured mapping drops `conf`; on an unconfigured guest
  port with a live forward it drops `ttl` (today it refuses anything not in
  the conf). It never drops a `connection` owner: that belongs to whoever
  holds the connection.
- `ports down` drops every `conf` owner and stops the daemon only if no
  forward and no holder remains. Today it stops the daemon outright; with hub
  forwards that would loop (`hub.md` section 7).
- A closing connection drops itself; the expiry ticker drops `ttl`.
- `ports add NAME PORT` on a live forward adds `conf` and touches no other
  owner. A `ttl` left behind expires harmlessly because `conf` keeps the
  forward alive; a `connection` owner is still someone's session.
- The forward is closed only when no owner remains. "Ephemeral" below means
  "has no `conf` owner".

**Bind policy is a property of the request, not the owner.** `add` takes the
guest port, a preferred host port, and `exact bool`. Configured forwards and
direct opens bump as today; a `redirect_uri` callback is exact and fails with
`errPortBusy` if the port is taken, which the bridge turns into a refused open
with a reply the guest can read. Today's `ensureCallback` also binds the exact
port; it just fails silently.

**Every forward is dual-stack.** `auth.ensureCallback` binds both
`127.0.0.1:P` and `[::1]:P` because macOS resolves `localhost` to `::1`. Both
transports' `forward` bind only IPv4 today. Rather than track address families
per request, every forward binds both (smol: two listeners; ssh: two `-L`
specs), with `::1` best-effort so a host without an IPv6 loopback still works.
That removes the family axis entirely: reusing an existing forward can never
hand a callback an IPv4-only bind.

Which owner a bridge open gets:

- **Direct loopback URL** (`http://localhost:3000/`): the subscribing
  client's connection. The forward lives as long as the session that opened
  it, so a dev-server tab does not die ten minutes into a held `attach`.
- **`redirect_uri` callback**: `ttl`, fixed at creation (proposed 10 minutes)
  and refreshed when a new open names the same port. Callbacks are single-use
  by nature; a session-length owner would pin a well-known port for no
  reason. The TTL is measured in connected time: it pauses while the daemon
  is reconnecting, so a long outage does not expire everything on the first
  tick after `restore()`. TTL rather than last-use because ssh forwards are
  native `-L` and the daemon never sees their connections.

**Reuse.** A request for a guest port that is already forwarded adds its
owner to the existing forward and returns the existing host port, **unless**
the request is exact and the existing host port differs (a configured `1455`
that got bumped to `1456`), in which case it is refused like any busy exact
bind.

**Limits.** Privileged ports (< 1024) are refused; at most 20 live ephemeral
forwards per machine. `session.Forward` gains `Ephemeral bool` and `Expires`;
`ports list` already prints `(ephemeral)` and now also shows the owner kind
and remaining TTL. `update` and `stop` cycle the daemon and drop ephemerals.
Documented, not fixed: they are ad hoc by definition.

### 5. `BROWSER` in the guest

The shim (`devvm-open-url`) is installed beside the agent. It becomes
`BROWSER` wherever devvm can arrange it without writing to an adopt host:

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
    agentPath := agentbin.Install(...)         // consent gate unchanged (adopt hosts)
    shim := installBrowserShim(...)
    cl := session.Dial(configDir, name)        // spawn daemon if needed
    release := cl.Hold(); defer release()      // keep the bridge alive across logins
    cl.Subscribe()                             // unless DEVVM_NO_SUBSCRIBE=1
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
menubar badge with it. Decision: **N counts configured forwards only.**
Ephemerals stay visible in `ports list`. If the menubar later wants to offer
"make persistent", add a fifth `ephemeral:N` token then; not now.

## CLI surface

No new verbs. Visible changes:

- `ports list`: ephemeral forwards labelled with owner kind and remaining TTL.
- `ports add NAME PORT` on an ephemeral forward promotes it in place.
- `ports rm NAME PORT` can remove a daemon-only `ttl` forward.
- `ports down` leaves the daemon up while other owners or holders remain.
- `attach`/`shell`/`auth` keep the daemon alive for their lifetime and
  subscribe unless `DEVVM_NO_SUBSCRIBE=1`.
- `auth --install-agent` unchanged and remains the only consent path.
- Guest side: `devvm-open-url URL|PORT` prints what the host did.

## Order of work

Lives in `ROADMAP.md`, the only place sequencing is written down.

## Open questions

- Ephemeral TTL: 10 minutes proposed for `redirect_uri` callbacks. Direct
  opens are session-owned and carry no TTL, so there is only one knob.
- Should `start` hold the daemon too, or only interactive commands? Proposed:
  no. `start` brings up configured forwards, and those already keep the daemon
  alive.
- tmux `-e` needs 3.2 (2021). Do we care about older guests? Proposed: fall
  back silently to profile.d.
