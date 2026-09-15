# Proposal: hubs — reach another host's devvm machines from this one

Status: draft v3.2, 2026-09-15, revised after four independent reviews.
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

Two consequences the registry code has to absorb:

- **Runtime identifier.** Socket, log and lock paths are `run/NAME.*`
  (`internal/session/protocol.go`), so a bare `desktop/web` would name a
  subdirectory nothing creates and the first daemon spawn would fail opening
  its log. The runtime identifier for a hub machine is `desktop@web`; `@` is
  outside `nameRe`, so it cannot collide with a local name. `session` derives
  it from the display name in one place and nothing else ever splits it.
- **Enumeration.** `restartDaemons` (`update`), the global `ports list`,
  `gatherRows` (`status`) and completion all walk `config.List`, which yields
  hub confs but not the machines on them. They switch to a `config.ListAll`
  that yields local names, `HUB/NAME` for every `[machines.NAME]` table in a
  hub conf, `HUB/NAME` for every name in the hub's cached listing (section
  5), **and `HUB/NAME` for every live `run/HUB@NAME.sock`**. The last source
  matters because a laptop daemon for a hub machine can exist with no entry
  at all: a browser open from `attach desktop/web` creates a
  `connection`-owned forward in it and writes nothing to the conf. `delete
  desktop/web` proxies the deregistration, then stops the local daemon and
  drops the `[machines.web]` table if present. `delete desktop` refuses while
  any `[machines.*]` table or `run/desktop@*.sock` exists unless `--force`,
  which stops those daemons and removes the conf.

### 3. Dispatch lives in `resolve`, not in a root interceptor

The first positional cannot be found before cobra parses: `cp-in -r NAME …`
puts flags first, `exec` and `keys add` disable flag parsing, `status [NAME]`
is optional, and `defaults`, `hub`, `update` have no machine at all. Every leaf
already calls `resolve`, so that is the one dispatch point:

- `resolve` returns a `hubBackend` for `HUB/NAME`.
- Leaves that must run on the hub (`attach`, `shell`, `exec`, `start`, `stop`,
  `provision`, `deprovision`, `bootstrap`, `delete`, `repos *`, `keys *`,
  `lockdown`, `create`) call `a.proxy(hub, cmd, args)`, which **rebuilds** the
  remote command line from the parsed cobra command rather than replaying
  `os.Args`: the command path, the leaf's flags that were explicitly set, and
  its positionals with `HUB/` stripped from the first. The persistent
  `--config-dir` is simply never emitted. Replaying raw argv would have to
  strip `--config-dir` by pattern, and `exec` and `keys add` disable flag
  parsing precisely so the guest command's own arguments pass through
  untouched; a guest `--config-dir` after `exec NAME --` must survive.
- Leaves that run locally (`ports`, `cp-in`, `cp-out`, `status`, `auth`) use
  the backend's small surface (section 4) and the local mechanics below.

`proxy` runs:

```
ssh [-t] HUB -- "$SHELL" -lc 'devvm CMD ARGS…'      # shellJoin-quoted
```

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

`devvm status` runs `ssh HUB "$SHELL" -lc 'devvm status --plain'` once per hub,
in parallel with local probes, and merges rows as `desktop/web`. Only the
**state** and **backend** come from the hub row. Its forwards column describes
the desktop's own forwards, which say nothing about what the laptop can reach
on `localhost`; the merged row's forwards column is computed from the laptop's
daemon for `desktop@web`, exactly as for a local machine. The result is
written to `cache/hub-desktop.list` in the config dir, **outside** `machines/`
and `run/`, so the `status --watch` fsnotify loop does not see its own cache
and re-trigger. Derived data, but not worth a second XDG directory.

The listing ssh runs with a **short connect timeout** (2s, not the default
10s): the menu bar app re-runs a plain status every time its menu opens, and a
desktop that is asleep must not turn that into a ten-second hang. On failure
the cached rows are rendered with state `unreachable`.

The hub itself gets a row (`desktop  hub  reachable|unreachable`) in the
human listing and in `--plain`, so a hub with no machines yet is still
visible, and `unreachable` has somewhere to land when the listing is empty.
`statusGroups` gains a `hub` section for it.

`status --watch` must not ssh per fsnotify event, which is exactly the polling
the watcher exists to avoid. Instead it spawns `ssh HUB … devvm status --plain
--watch` per hub, holds the pipe, and re-merges whenever either the local
snapshot or a hub block changes. A hub that is unreachable renders its cached
rows with state `unreachable`.

`--plain` gains no column: the hub is encoded in column 1 (`desktop/web`),
which is what the menubar already keys on. Consumers that want to group by hub
split on `/`.

### 6. File copy

cp already uses a tar stream as its wire format, but it is not "two flags":
`copyArchive` derives its base names from the local sources, and `copyOut`
emits one tar per source. So:

- `cp-in NAME --from-tar - DST`: the hub spools stdin to the staging file and
  derives bases with `readArchive`. The laptop builds the tar exactly as today
  and pipes it over `ssh HUB … devvm cp-in desktop/web --from-tar - DST` (no
  `-t`).
- `cp-out NAME SRC --to-tar -`: single source per invocation; the laptop loops
  sources, streams each tar back, and extracts locally with the same
  `extractArchive` and no-clobber checks as today.
- The "->" progress lines print on the laptop only; the hub side runs with
  `--quiet`.
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
ssh -o ControlPath=… HUB "$SHELL" -lc 'devvm __session web'
```

`__session` on the hub dials (spawning if needed) the hub daemon for `web`
and holds one long-lived control connection to it. Its stdio is a JSON-lines
protocol, opened with the marker line from section 6:

```
laptop → hub:  {"op":"add","guest":3000,"pref":3000,"exact":false}
hub → laptop:  {"ok":true,"host":3001}          // the hub-loopback port
laptop → hub:  {"op":"remove","guest":3000}
laptop → hub:  {"op":"subscribe"} / {"op":"unsubscribe"}     (section 8)
hub → laptop:  {"event":{…}}                    // subscribed bridge events
laptop → hub:  {"reply":{…}}                    // the subscriber's answer
```

Every forward `__session` adds is **owned by its control connection**
(`browser-bridge.md` section 4). `hubTransport.forward(host, guest)` sends
`add`, reads the hub port the hub chose (which may be bumped on the hub;
nothing on the laptop cares), and binds a native `-L host:localhost:hubPort`
on the master. `remove` drops this connection's ownership of one guest port.
When the process dies, every forward it owned goes with it.

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
before reporting the forward up. `restore()` never dials a stopped smol VM
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
acknowledged before it proxies the leaf. So the laptop is subscribed on the
hub before the login can emit anything. `DEVVM_NO_SUBSCRIBE=1` keeps the
proxied process from subscribing on the hub itself (it does not dial the hub
daemon at all); without it the proxied process would be the most recent
subscriber and the URL would open on the desktop.

**Recovery** is the laptop daemon's reconnect, nothing new. If the master
drops (laptop sleep) or the hub daemon cycles (`update`, `stop` on the
desktop), `__session` dies, the laptop daemon goes `reconnecting`, and
`restore()` brings back the forwards and the subscription together. The
local `attach` never notices beyond a one-line notice in the daemon log.
While the laptop is unsubscribed, events go to whichever subscriber the hub
daemon still has: a desktop `attach` on the same VM would open the URL there,
or with none, the guest is told "not opened" and the shim prints the URL for
pasting. A login started inside the gap therefore either opens on the desktop
or prints its URL; neither is silent.

Where a desktop `attach` and a laptop `attach` are both subscribed to the same
VM, the daemon delivers each event to the **most recent** subscriber only.
That is the one remaining tie-break and it is deliberate (see open questions).

`auth desktop/web` is thin `auth` (`browser-bridge.md` section 6) pointed at
the laptop daemon for `desktop@web`; the login commands run through
`hubBackend.Run`, which is a proxied `exec` with `BROWSER` in its argv. The
agent and shim are already on a hub VM (the hub's daemon installed them), so
the install step is skipped for hub machines.

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

Until the bridge lands (roadmap step 7), `auth desktop/web` proxies verbatim
and opens on the desktop. Usable, and clearly labelled in the output.

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
- Hidden: `__session NAME` (one per hub machine, held by the laptop daemon:
  add/remove owned forwards, subscribe, events out / replies in). Runtime
  files for a hub machine use `HUB@NAME`.
- Env: `DEVVM_NO_SUBSCRIBE=1`, set by the proxy on every proxied command.

## Order of work

Lives in `ROADMAP.md`, the only place sequencing is written down.

## Open questions

- Most-recent-subscriber for browser opens is simple but means a desktop user
  who starts a login while a laptop session is held opens on the laptop. Is a
  per-subscriber "I have a terminal" hint worth it? Proposed: not in v1.
