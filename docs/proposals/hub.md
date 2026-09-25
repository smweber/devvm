# Proposal: hubs — reach another host's devvm machines from this one

Status: draft v3.4, 2026-09-15, revised after six independent reviews;
§2 and §7 updated 2026-09-25 to the mechanism roadmap step 6 shipped.
Order of work lives in `ROADMAP.md` and nowhere else. Companion to
`browser-bridge.md`, whose sections 3 and 4 are the authoritative statement
of leases, subscriptions, ownership and bind policy; this doc references them
and adds only what is hub-specific. Section 8 explains how the two interact.

## Problem

devvm's registry and backends assume the machine that runs `devvm` is the
machine that runs `smolvm`. A desktop Mac with spare RAM should host several
smol VMs, and a laptop should reach those VMs as first-class devvm machines:
list them, attach, forward ports, copy files, create and provision new ones.
Meanwhile, the desktop's own `devvm` must keep working on the same VMs,
whoever created them, and the desktop must not itself become a devvm-managed
box.

Two constraints from the code shape the answer:

- A smol VM is reachable only via `smolvm machine exec` on its host. There is
  no sshd or routable address in a smol guest, so the laptop cannot adopt a
  desktop VM as a `remote-*` machine.
- The **one-exec rule**: each smol VM tolerates one long-lived agent exec. If
  both the desktop's daemon and a laptop daemon spawned `devvm-agent serve` into
  the same VM, that rule is broken. So the laptop must never own an agent exec
  for a hub VM; the hub's daemon is the only owner.

## Goals

1. From the laptop: `devvm status` lists desktop VMs; `attach`, `shell`, `exec`,
   `cp-in`, `cp-out`, `ports`, `auth`, `create`, `provision`, `bootstrap`,
   `start`, `stop`, `delete` all work on them by name.
2. From the desktop: everything works exactly as today, including on VMs the
   laptop created.
3. One registry per VM (the hub's). Nothing on the laptop can drift from it.
4. The desktop is a hub, not a machine: devvm never bootstraps, hardens,
   installs an agent on, or otherwise shapes it. It only needs `devvm` and
   `smolvm` installed.

## Non-goals

- Hub-to-hub chaining, or a hub that is itself a `remote-*` box.
- Live migration or sharing VMs between hosts.
- Replacing the per-user config with a shared one.

## Design

### 1. A hub is a conf, reached like a remote box, never shaped like one

A hub is registered as a machine-shaped conf so the existing ssh plumbing
(port, identity, isolated known_hosts, ControlMaster flags, `SSHConn()`)
applies without a second implementation:

```toml
# ~/.config/devvm/machines/desktop.toml
backend  = "hub"
ssh_host = "scott@desktop.tail-net.ts.net"
```

`backend = "hub"` is a fourth backend value. `Validate` accepts it, `IsRemote()`
is true for it (ssh transport), `Managed()` is false, and every lifecycle and
shaping verb (`provision`, `bootstrap`, `lockdown`, `keys`, `repos`, `auth`)
refuses it with "NAME is a hub, not a machine". `devvm create --backend hub
NAME --ssh-host DEST` test-connects and checks the remote `devvm` version, like
adopting a remote box does today. `delete` deregisters it.

Not in `config.toml`: `SaveDefaults` re-encodes only the `Defaults` struct, so a
`[hubs]` table there would be silently dropped by the next `defaults set`.

### 2. Hub machines are named `HUB/NAME`

`nameRe` forbids `/` in machine names, so `desktop/web` is unambiguous: the
part before the slash is a hub conf, the rest is a machine on it. This is the
canonical reference everywhere: `devvm attach desktop/web`, `devvm status`
rows, `--plain` column 1, completion. Collisions between a local `web` and a
desktop `web` cannot happen.

`resolve("desktop/web")` loads the `desktop` hub conf and returns a hub
machine record plus a `hubBackend` (section 3). It does **not** ssh to check
the machine exists: the proxied command reports that itself. So nothing is
added to the `resolveLive` hot path, and a sleeping hub costs a typo'd name one
ssh timeout only when a command is actually run against it.

Bare names never fall back to hubs. Completion of `desktop/` comes from a
per-hub listing cache (section 5).

Laptop-side state for a hub machine exists for one thing: the laptop's own
port forwards, which are inherently per client. It lives **in the hub's own
conf**, as a table keyed by machine name:

```toml
# ~/.config/devvm/machines/desktop.toml
backend  = "hub"
ssh_host = "scott@desktop.tail-net.ts.net"

[machines.web]
ports = ["3000", "8443:443"]
```

`ports` is the only per-machine field, so this is a `map[string]HubMachine`
with one slice in it. One hand-editable file per hub, no second registry
tree, and `config.List` stays truthful with no filtering: it sees `desktop`,
whose conf validates as a hub. `delete desktop` removes one file.

One file for every machine on the hub widens an existing race: today two
`ports add web …` runs race on `web.toml`, but `ports add desktop/a` and
`ports add desktop/b` used to touch different files and now share one, and
`Save` is atomic, not serialized, so the second read-modify-write drops the
first. The hub-conf rewrite therefore takes a `flock` on the conf for the
whole read-modify-write from the day `ports add HUB/NAME` ships (roadmap
step 6); the atomic rename stays, the lock is what prevents the lost update.
Local per-machine confs keep today's behaviour. As shipped
(`config.UpdateHubMachine`), the lock is a sibling file,
`machines/.HUB.toml.lock`, not the conf: the rename replaces the conf's
inode, so a flock on the conf itself would not exclude the next writer.
A table left with no ports is dropped.

Two consequences the registry code has to absorb:

- **Runtime identifier.** Socket, log and lock paths are `run/NAME.*`
  (`internal/session/protocol.go`), so a bare `desktop/web` would name a
  subdirectory nothing creates and the first daemon spawn would fail opening
  its log. The runtime identifier for a hub machine is `desktop@web`; `@` is
  outside `nameRe`, so it cannot collide with a local name. `session` derives
  it from the display name in one place and nothing else ever splits it.
- **Enumeration.** `restartDaemons` (`update`), the global `ports list`,
  `gatherRows` (`status`) and completion all walk `config.List`, which yields
  hub confs but not the machines on them. They switch to an application
  level `listMachines` in `internal/cli` (not `config`, which stays a file
  reader that never dials a socket or trusts a cache) that unions
  `config.List` with `HUB/NAME` for every `[machines.NAME]` table in a hub
  conf, `HUB/NAME` for every name in the hub's cached listing (section 5),
  **and `HUB/NAME` for every live `run/HUB@NAME.sock`**. Keeping it out of
  `config` also keeps the stale-cache handling explicit: the cache is a
  hint for completion and enumeration, and only a live proxied command says
  whether a machine exists. The last source
  matters because a laptop daemon for a hub machine can exist with no entry
  at all: a browser open from `attach desktop/web` creates a
  `connection`-owned forward in it and writes nothing to the conf. `delete
  desktop/web` proxies the deregistration, then stops the local daemon and
  drops the `[machines.web]` table if present. `delete desktop` refuses while
  any `[machines.*]` table or `run/desktop@*.sock` exists unless `--force`,
  which stops those daemons and removes the conf.

### 3. Dispatch lives in each leaf's `RunE`, with `resolve` as the guard

The first positional cannot be found before cobra parses: `cp-in -r NAME …`
puts flags first, `exec` and `keys add` disable flag parsing, `status [NAME]`
is optional, and `defaults`, `hub`, `update` have no machine at all. So there
is no root interceptor. Each proxied leaf's `RunE` checks its first positional
for `HUB/NAME` (one wrapper, `hubOr`), because that is the only place the
parsed `*cobra.Command` the rebuilt argv needs is in hand; and `resolve`,
which every leaf calls, refuses a hub machine, so a leaf that is not proxied
can never act on the hub itself with the machine's record:

- `resolveAny` returns a `hubBackend` for `HUB/NAME`; `resolve` refuses it.
- Leaves that must run on the hub (`attach`, `shell`, `exec`, `start`, `stop`,
  `provision`, `deprovision`, `bootstrap`, `delete`, `repos *`, `keys *`,
  `lockdown`, `create`) call `a.proxy(cmd, args)`, which **rebuilds** the
  remote command line from the parsed cobra command rather than replaying
  `os.Args`: the command path, the leaf's flags that were explicitly set, and
  its positionals with `HUB/` stripped from the first. The persistent
  `--config-dir` is simply never emitted. Replaying raw argv would have to
  strip `--config-dir` by pattern, and `exec` and `keys add` disable flag
  parsing precisely so the guest command's own arguments pass through
  untouched; a guest `--config-dir` after `exec NAME --` must survive.
- Leaves that run locally (`ports`, `cp-in`, `cp-out`, `status`, `auth`) use
  the backend's small surface (section 4) and the local mechanics below;
  until their step lands, `resolve`'s refusal is what they hit.

`proxy` runs:

```
ssh [-t] HUB -- "$SHELL" -lc 'devvm CMD ARGS…'      # shellJoin-quoted
```

**Inputs that mean something on the laptop are resolved there first.**
Rebuilding argv preserves bytes, not semantics: a path, the current
directory and the default key set all belong to the laptop. `keys add
HUB/NAME SPEC` runs `resolvePubkeys` locally (a file path, the bare default
`~/.ssh/id_*.pub`, `--from-github`) and proxies the resulting inline key
lines, so "add my keys" adds the laptop's keys, not the hub's. `repos add
HUB/NAME` with no REPO infers the origin from the laptop's working directory
and sends that URL; on the hub the proxied process runs in `$HOME` and has
no origin to infer. `exec` and everything after `--` stay untouched. `create
HUB/NAME --yes` resolves unset fields from the **hub's** `config.toml`,
deliberately: the hub's defaults describe the hub's VMs. Nothing else in the
proxied set reads a local file.

A login shell is required: a plain `ssh HUB devvm` runs in a non-login shell
where Homebrew's `devvm` and `smolvm` are usually not on `PATH`. This is what
`remoteCommand(Login: true)` already does for guest execs. `-t` is passed only
when the laptop's stdin is a TTY, so the menubar's non-interactive `cp-in` does
not hit `ssh -t`'s noise. The proxied command line is prefixed with
`DEVVM_NO_SUBSCRIBE=1`, so a proxied `attach`, `shell` or `auth` holds the hub
daemon but never subscribes to browser events; the laptop subscribes instead
(section 8).

`start desktop/web` proxies. Once hub forwards exist (section 7) it also runs
the laptop's own `tunnelUp`, so laptop-configured forwards come back alongside
the hub's; the hub's `start` only knows the hub's conf.

`create desktop/web …` is the one leaf whose name does not exist yet; it
proxies the same way, and the hub writes the conf. The huh form runs on the
hub through the forwarded TTY.

**Version skew.** There is no `devvm version` subcommand, all tags are `v0.x`,
and local builds report `dev`. The hub surface is a set of CLI contracts
(`--plain`, the tar flags, the `__session` line protocol), so it is versioned
the way `--plain` already is: by a floor, not by equality. `ssh HUB devvm
--version` is read once per hub (cached with the listing); a hub below the
version that introduced the surface is refused, any other mismatch is a
one-line warning, and `dev` on either side is never refused. An exact-match
rule would break hub access on every `devvm update` taken on one side first,
which the menu bar app makes routine. Nothing on the laptop ever dials the
hub daemon's socket, so its `ping` version is irrelevant here.

### 4. `hubBackend`

Implements `backend.Backend` only as far as the local leaves need:

- `Status()`/`Exists()`: from the cached listing (section 5), refreshed on a
  miss.
- `Run`: `proxy(hub, ["exec", name, "--", argv…])` for the few non-interactive
  probes `auth` and `cp` make. Guest stdin is not wired on a smol one-shot exec
  (no `-i`), so callers that need stdin use the tar path in section 6, not `Run`.
- `Spawn`: returns an error. A hub machine never gets a laptop-owned agent
  exec; that is the whole point.
- `Copy`: error; cp uses section 6.
- `Interactive`: `Shell`/`Attach` proxy.

### 5. Listing and watching

`devvm status` runs `ssh HUB "$SHELL" -lc 'devvm status --plain --local'`
once per hub, in parallel with local probes, and merges rows as
`desktop/web`. `--local` lists only the machines the hub itself runs and
skips every hub conf registered *there*: without it a hub that has hubs of
its own would fan out its own listings and watchers on every laptop query,
and two hosts registered as each other's hubs would recurse. Chaining is a
non-goal; `--local` is what makes it not happen by accident. The flag is
part of the hub surface and sits behind the version floor (section 3). Only the
**state** and **backend** come from the hub row. Its forwards column describes
the desktop's own forwards, which say nothing about what the laptop can reach
on `localhost`; the merged row's forwards column is computed from the laptop's
daemon for `desktop@web`, exactly as for a local machine. The result is
written to `cache/hub-desktop.list` in the config dir, **outside** `machines/`
and `run/`, so the `status --watch` fsnotify loop does not see its own cache
and re-trigger. Derived data, but not worth a second XDG directory.

The listing ssh runs with a **short connect timeout** (2s, not the default
10s): the menu bar app re-runs a plain status every time its menu opens, and a
desktop that is asleep must not turn that into a ten-second hang.
`ConnectTimeout` bounds only the connect and key exchange, not
authentication, the login shell, or the remote `devvm status` itself, so
the listing also runs with `BatchMode=yes` (already an `ExecOpts` field; no
prompt can hang it) under an **overall deadline** (proposed 5s) that kills
the ssh. A hub that accepts the connection and then hangs is `unreachable`
like one that never answers. On failure the cached rows are rendered with
state `unreachable`.

The hub itself gets a row (`desktop  hub  reachable|unreachable`) in the
human listing and in `--plain`, so a hub with no machines yet is still
visible, and `unreachable` has somewhere to land when the listing is empty.
`statusGroups` gains a `hub` section for it.

`status --watch` must not ssh per fsnotify event, which is exactly the polling
the watcher exists to avoid. Instead it spawns `ssh HUB … devvm status --plain
--local --watch` per hub, holds the pipe, and re-merges whenever either the local
snapshot or a hub block changes. A hub that is unreachable renders its cached
rows with state `unreachable`. When a hub pipe hits EOF (the desktop went
to sleep, sshd restarted, `devvm` was updated there) the watcher re-spawns
it with the daemon's 2–30s backoff and shows the cached rows `unreachable`
meanwhile, so a hub that wakes is picked up without restarting `--watch`;
it also re-reads the hub confs on the `machines/` fsnotify events it
already receives, so an added or deleted hub gets or loses its pipe live.

`--plain` gains no column: the hub is encoded in column 1 (`desktop/web`),
which is what the menubar already keys on. Consumers that want to group by hub
split on `/`.

### 6. File copy

cp already uses a tar stream as its wire format, but it is not "two flags":
`copyArchive` derives its base names from the local sources, and `copyOut`
emits one tar per source. So:

- `cp-in NAME --from-tar - DST`: the hub spools stdin to a private temp file
  (refusing a stream that ends before tar's end blocks), derives bases with
  `readArchive`, and hands it to the normal `copyArchive` staging. The laptop
  builds the tar exactly as today and pipes it over `ssh HUB … devvm cp-in
  web --from-tar - DST` (no `-t`): the hub-side argv carries the bare machine
  name, and the hidden forms refuse a `HUB/NAME`.
- `cp-out NAME SRC --to-tar -`: single source per invocation; the laptop loops
  sources, streams each tar back, and extracts locally with the same
  `extractArchive` and no-clobber checks as today.
- The "->" progress lines print on the laptop only; the hidden hub-side
  forms print nothing (there is no `--quiet` flag).
- **The stream starts with a marker line.** The hub side runs under the
  user's login shell, and a `.zprofile` or `.bash_profile` that echoes would
  land in front of the tar. The hub prints `devvm-tar-v1\n` before the
  archive and the laptop discards everything up to and including it. The
  `__session` line protocol (section 7) opens the same way for the same
  reason.

Budget: about 150 lines, no daemon changes, covered by the existing tar tests.

### 7. Forwards: two hops, one agent exec, one session per hub machine

The laptop's per-machine daemon gets a third transport, `hubTransport`. It
owns a ControlMaster to the hub via the existing `sshTransport` code and,
over that master, exactly **one** long-lived process per hub machine:

```
ssh -o ControlMaster=no -o ControlPath=run/HUB@web.master -o BatchMode=yes -o RequestTTY=no HUB \
    sh -c 'exec "${SHELL:-sh}" -lc "$0"' 'env DEVVM_NO_SUBSCRIBE=1 devvm __session web'
```

The master is the laptop daemon's own, one per hub machine
(`run/HUB@NAME.master`): two daemons for `desktop/a` and `desktop/b` each
start and `-O exit` their master, so a shared path would let one kill the
other's `-L`s.

`__session` on the hub refuses a stopped VM (the `ports up` guard), then
dials (spawning if needed) the hub daemon for `web`, prints the marker
line `devvm-session-v1` (section 6's reason: a login banner may come
first), and from then on relays its stdio and that one connection
**verbatim** in both directions; `__session` adds nothing but the marker
and its own lifetime. The laptop's `hubTransport` writes the session open
itself, so the connection's line protocol (`browser-bridge.md` section 3:
an `id` on every request and event, one reader per side, serialized
writes) runs end to end between the laptop daemon and the hub daemon:

```
hub → laptop:  (blank line) devvm-session-v1            // after any login banner
laptop → hub:  {"id":1,"op":"session","relay":true,"heartbeat":20}
hub → laptop:  {"id":1,"ok":true,"state":"up","version":"…","relay":true,"heartbeat":20}
               // or, hub transport down: {"id":1,"ok":false,"err":"relay session refused: …"}, then EOF
laptop → hub:  {"id":2,"op":"add","host":3000,"guest":3000}   // host = the hub's preference: the guest port
hub → laptop:  {"id":2,"ok":true,"host":3001}                 // the hub-loopback port
laptop → hub:  {"id":3,"op":"remove","guest":3000}
hub → laptop:  {"id":3,"ok":true}
laptop → hub:  {"id":4,"op":"subscribe"} / {"op":"unsubscribe"}     (section 8, step 8)
hub → laptop:  {"event":{"id":7,…}}             // subscribed bridge events
laptop → hub:  {"reply":{"id":7,…}}             // the subscriber's answer
laptop → hub:  {"id":5,"op":"ping"}             // every heartbeat interval
hub → laptop:  {"id":5,"ok":true,"state":"up",…}
```

**Heartbeat.** `heartbeat` on the open is an interval in seconds (the
laptop sends 20) at which the far side promises to send something; the
hub daemon echoes the value it enforces (capped at an hour) and closes the
relay once it has *read* nothing for three intervals (60s). The laptop
pings at the interval. Without it, a laptop that vanished without a FIN
(lid closed, network gone) left the `__session`, its sshd and login shell,
every forward the relay owned and the hub daemon itself alive until sshd's
TCP keepalive gave up, around two hours, during which a hub `ports down
web` could not stop the daemon; every laptop sleep added one more. The
deadline is on reads, so any line counts, and a ping the hub answers late
(its one session worker busy with a slow `add`) cannot trip it: the ping
was read when it arrived. Closing the connection ends `__session`'s relay,
so it exits and sshd tears the rest down; the session's forwards go with
it. On the laptop, a ping left unanswered for two intervals marks the
transport dead, the same reconnect a dead master or `__session` starts.
Only a relay that declares `heartbeat` gets a deadline: a local session
(a held `attach`, `auth`) is idle by design and its client is on the
same host, and a local open's `heartbeat` is ignored and not echoed.

The hub add is never `exact` (the hub port is an intermediate hop; an
exact callback port matters only on the laptop, section 8), and this
host's port is probed before the hub is asked for anything, so a busy
laptop port bumps without a hub round trip per try.

The open reply echoes `"relay":true`: a hub daemon older than hub
forwards ignores the request's `relay` and admits a plain session, which
would never be closed on its transport's death, so a reply without the
echo is refused by the laptop with the fix to run on the hub (restart that
daemon). A hub whose devvm has no `__session` is named with the release
it needs (`session.HubForwardsMinVersion`); the hub floor (section 3)
stays where listing, cp and proxying need it. Calls on an open relay time
out after 10s. On teardown the laptop sends no per-forward `remove`: the
relay's close drops them all on the hub, and the master's exit every
`-L`. A hub-side refusal of an `add` (no free port there) leaves that
forward pending on the laptop, retried by the ticker.

Every forward `__session` adds is **owned by its control connection**
(`browser-bridge.md` section 4). `hubTransport.forward(host, guest)` sends
`add`, reads the hub port the hub chose (which may be bumped on the hub;
nothing on the laptop cares), and binds a native `-L host:127.0.0.1:hubPort`
on the master. `127.0.0.1`, not `localhost`: the hub port is the hub
daemon's own listener, IPv4 with `::1` best-effort (bridge section 4), and
naming the family means a stranger on the hub's `[::1]:hubPort` can never
receive laptop traffic. `remove` drops this connection's ownership of one
guest port. When the process dies, every forward it owned goes with it.

**A relay session is only ever open against a live transport.** The hub
daemon refuses `session {relay: true}` while it has no transport to the VM
(an error reply, then close; bridge section 3), so a `__session` started
during a hub-side outage fails to open, the laptop transport's dial fails,
and the laptop daemon's own reconnect backoff retries until the hub is back.
Without that rule a relay opened mid-outage would get `pending` for every
`add`, and nothing would ever tell it the hub's `restore()` had finished:
the closing below fires only on transport *death*, so a laptop whose first
forward landed in that window would stay pending for good. A `pending`
reply can still reach the laptop in the race between the transport dying
and the close that follows; the transport treats it as a bind failure and
the close brings the recovery below. As shipped, the transport also
marks itself dead on that reply, and the laptop daemon records an `add`
that fails on a dead transport as pending (not failed), so the forward
comes back with the reconnect.

Why one process per machine and not a control socket forwarded over ssh:
process lifetime is the scope, with nothing to refcount across a second
protocol client, no unix-socket `-L` (which OpenSSH supports on a mux but
leaves the client socket file behind on master death, so `restore()` would
hit "address already in use" without `StreamLocalBindUnlink`), and no unix
variant of the bindability pre-probe. Why one per machine and not one per
forward: the same process carries the browser-event subscription (section
8), so the hub daemon sees one connection per laptop, and the laptop daemon's
transport has one thing to supervise.

Death handling falls out of the daemon's existing reconnect. `dead()` on
`hubTransport` fires when the master dies **or** `__session` exits. If the
hub daemon cycles while the master is alive (`update` or `stop` on the
desktop), `__session` exits, the laptop daemon goes `reconnecting`, and
`restore()` re-dials the transport (a fresh `__session`) and re-adds every
forward with the daemon's existing 2–30s backoff. The hub port a re-added
forward gets may differ; the transport re-resolves it and re-adds the `-L`
before reporting the forward up.

**A hub-side reconnect invalidates `__session`.** The hub daemon can lose
its transport to the VM while the master and `__session` are both alive,
and its own `restore()` bumps a forward whose host port was taken during
the outage; the laptop's `-L` would then point at a hub port nothing
listens on, and the protocol above has no line to say so. Rather than add
one, the hub daemon **closes every relay session when its transport dies**
(bridge section 3), so `__session` exits, `dead()` fires, and the same
reconnect as above re-adds every forward and re-resolves every hub port
once the hub is back. Three causes, one recovery path: master death, hub
daemon cycle, hub transport loss. On first add and
in `restore()` alike, a forward is acknowledged up only after the hub's
`add` succeeded **and** the `-L` is bound; a hub port is never reported
before both hold. `restore()` never dials a stopped smol VM
today; the hub side keeps that guard, because `__session` runs `session.Dial`
on the hub, which refuses to spawn a daemon for a stopped VM, so a desktop
`stop web` cannot boot-loop the VM through the laptop's reconnect.

**Ownership in the hub daemon** is `browser-bridge.md` section 4, not
restated here. What it means for hubs: a desktop `ports rm` (which drops only
`conf`) cannot kill a laptop forward, and a desktop `ports down` drops the
`conf` owners but leaves the daemon up for the laptop's forwards. The second
rule exists for this case: if `ports down` stopped the daemon outright, the
laptop transport would re-run `__session`, which respawns the daemon, and the
two would loop. The desktop's `status --plain` shows `down` meanwhile (ports
configured, none of them up), because `up:N` counts only `conf` and `-` is
reserved for "nothing configured". A held `__session` with no forwards and no
subscription still counts as a holder on the hub daemon (bridge section 3),
for the same reason: an idle-exit under a live `__session` would be the same
loop on a 60-second period. The laptop daemon idles out first (no forwards,
no subscribers), which closes `__session`, and the hub daemon idles after it.

**One forwarding contract.** A client of the laptop daemon always asks for a
**guest** port, exactly as it does for a local machine; it never sees or
names a hub port. The same contract serves `ports add desktop/web 3000` and a
browser open relayed from the hub (section 8): both are `add(guest P, …)` on
the laptop daemon.

The hub daemon owns the only agent exec into the VM. The laptop never spawns
one. Both users can hold forwards to the same VM at once.

### 8. `auth` and the browser bridge

The login must run on the hub (only the hub's daemon can see the guest's
events), but the browser must open on the laptop. The same holds for a login
started inside a proxied `attach` or `shell`. The laptop daemon is the relay,
and `attach desktop/web` is **the local path with a different transport**:

```
laptop CLI:     session.Dial("desktop@web") — spawns the laptop daemon if
                needed; its hubTransport comes up with a `__session` on the
                hub (section 7). Subscribe to that daemon, exactly as
                `attach web` subscribes to its own. Then proxy the leaf with
                DEVVM_NO_SUBSCRIBE=1 (and -t when interactive).
laptop daemon:  while it has at least one local subscriber, it sends
                `subscribe` down `__session` (re-sent on every new local
                subscriber, so the hub's most-recent rule tracks the session
                that started last); `unsubscribe` when the last one leaves.
hub daemon:     sees `__session` as a relay subscriber: binds nothing,
                delivers `{url, kind, guest?}`, waits for the reply.
laptop daemon:  the event arrives on `hubTransport.events()`, the same
                channel a smol transport feeds. The bridge handler runs
                unchanged: classify, bind on its own transport (which is
                `add` over `__session`, then `-L`), deliver to its local
                subscriber with `bound`, relay the reply down `__session`.
laptop CLI:     opens the URL, replies. Its reply is what the guest sees, so
                `devvm: opened on host -> …` names the laptop's port.
```

There is no laptop-side relay code and no hub-side helper beyond `__session`:
the daemon's bridge handler is recursive across hops because a relay
subscriber is just another daemon. `__session` registers as a **relay**, so
the hub daemon binds nothing for it (`browser-bridge.md` section 1): the
exact-port requirement for a callback applies on the laptop, where the
browser is, and a busy desktop port never refuses a laptop login.

**Ordering.** `session.Dial` returns only once the laptop daemon is
listening, which is after its transport is up, which is after `__session`
has been accepted by the hub daemon; and the laptop CLI's own `subscribe` is
acknowledged before it proxies the leaf, where the laptop daemon sends that
acknowledgment **only after the hub daemon has acknowledged the relayed
`subscribe`** (bridge section 3). So the laptop is subscribed on the hub
before the login can emit anything. `DEVVM_NO_SUBSCRIBE=1` keeps the
proxied process from subscribing on the hub itself (it does not dial the hub
daemon at all); without it the proxied process would be the most recent
subscriber and the URL would open on the desktop.

**Recovery** is the laptop daemon's reconnect, nothing new. If the master
drops (laptop sleep), the hub daemon cycles (`update`, `stop` on the
desktop), or the hub daemon loses the VM and closes its relay sessions
(section 7), `__session` dies, the laptop daemon goes `reconnecting`, and
`restore()` brings back the forwards and the subscription together. The
local `attach` never notices beyond a one-line notice in the daemon log.
If instead the **laptop** daemon is cycled (a laptop `update`), the attach's
own session client reconnects and re-subscribes (bridge section 3), and
the laptop daemon's fresh `hubTransport` re-subscribes on the hub for it.
While the laptop is unsubscribed, events go to whichever subscriber the hub
daemon still has: a desktop `attach` on the same VM would open the URL there,
or with none, the guest is told "not opened" and the shim prints the URL for
pasting. A login started inside the gap therefore either opens on the desktop
or prints its URL; neither is silent.

Where a desktop `attach` and a laptop `attach` are both subscribed to the same
VM, the daemon delivers each event to the **most recent** subscriber only.
That is the one remaining tie-break and it is deliberate (see open questions).
Precisely: the order is kept per subscriber **of the hub daemon**, and a
relay is one subscriber whose position is that of its newest local session.
A laptop session that starts last wins; when it ends and an older laptop
session remains, the laptop keeps its place ahead of a desktop session that
started in between, because the laptop daemon has no way to demote itself
without a per-session subscription protocol across the relay. Host-level
priority, chosen over session-level for v1 (bridge section 3).

`auth desktop/web` is thin `auth` (`browser-bridge.md` section 6) pointed at
the laptop daemon for `desktop@web`; the login commands run through
`hubBackend.Run`, which is a proxied `exec` with `BROWSER` in its argv. The
agent and shim are one install operation (bridge section 5) that the hub's
smol transport runs before it spawns the agent exec, so a hub VM whose
daemon is up has both, and `auth desktop/web` skips the install step; the
laptop never writes to a hub VM. This is not true today: only `auth` writes
the shim, and the smol transport installs the agent alone, so the shared
install lands with the bridge step, not with `bootstrap` (roadmap step 9).

This changes the bridge in one way and confirms two:

- **Change: the daemon never opens a browser itself.** Note this regresses
  one current smol behaviour: today a URL from any guest shell opens on the
  host while a daemon is up even with no devvm session. Under the new rule it
  is logged instead. That is the intended trade for the unattended-open
  concern.
- **Confirms one ownership model** for forwards: owner set with kinds `conf`,
  `connection`, `ttl`. Both proposals build on it; neither redefines it.
- **Confirms skipping the ssh agent exec** step of the bridge for now. Hub VMs
  are smol VMs on the hub, so the hub daemon's smol transport already carries
  events.

Until thin hub `auth` lands (roadmap step 8), `auth desktop/web` proxies
verbatim and opens on the desktop. Usable, and clearly labelled in the output.

### 9. Trust

- The laptop trusts the hub as a host it has a shell on. Proxied commands run
  arbitrary `devvm` there as you. Nothing new beyond ssh access.
- The hub is never hardened, never gets an agent, never gets `profile.d`
  writes. Managed-box behaviour applies to the VMs on it, not to it.
- A laptop forward leaves nothing on the hub after disconnect: the owning
  process is gone, so the owner is gone.
- Things to verify by hand on macOS: `smolvm machine create` and `open` both
  work from an sshd (non-Aqua) session, and a daemon spawned there (`Setsid`)
  survives the ssh session ending. The daemon inherits that session's
  environment, not the desktop user's GUI environment.

## CLI surface

- `devvm create --backend hub NAME --ssh-host DEST`; `delete NAME` deregisters.
- Hub machines are `HUB/NAME` everywhere.
- `status` shows them merged; `--plain` encodes the hub in column 1, no new
  columns.
- `ports`, `cp-in`, `cp-out`, `status`, `auth` run locally with the mechanics
  above; every other machine verb proxies with the same argv.
- `cp-in --from-tar -` and `cp-out --to-tar -` exist for the proxy path.
- `start HUB/NAME` also brings up the laptop's own forwards; `delete` drops
  the `[machines.NAME]` table.
- `status --plain --local`: this host's machines only, no hub fan-out; what
  the laptop runs on the hub for listing and `--watch`.
- Hidden: `__session NAME` (one per hub machine, held by the laptop daemon:
  a relay session's line protocol over stdio, verbatim, behind the marker
  line). Runtime files for a hub machine use `HUB@NAME`.
- Env: `DEVVM_NO_SUBSCRIBE=1`, set by the proxy on every proxied command.

## Order of work

Lives in `ROADMAP.md`, the only place sequencing is written down.

## Open questions

- Most-recent-subscriber for browser opens is simple but means a desktop user
  who starts a login while a laptop session is held opens on the laptop. Is a
  per-subscriber "I have a terminal" hint worth it? Proposed: not in v1.
