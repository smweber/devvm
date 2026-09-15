# Proposal: hubs — reach another host's devvm machines from this one

Status: draft v3, 2026-09-15, revised after two independent reviews. Order of
work lives in `ROADMAP.md` and nowhere else. Companion
to `browser-bridge.md`; section 8 explains how the two interact.

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
port forwards, which are inherently per client. It lives under
`hubs/desktop/web.toml` (not `machines/`, so `config.List` and everything that
walks the registry never sees a half-machine) and holds only `ports`.

Two consequences the registry code has to absorb:

- **Runtime identifier.** Socket, log and lock paths are `run/NAME.*`
  (`internal/session/protocol.go`), so a bare `desktop/web` would name a
  subdirectory nothing creates and the first daemon spawn would fail opening
  its log. The runtime identifier for a hub machine is `desktop@web`; `@` is
  outside `nameRe`, so it cannot collide with a local name. `session` derives
  it from the display name in one place and nothing else ever splits it.
- **Enumeration.** `restartDaemons` (`update`), the global `ports list`,
  `gatherRows` (`status`) and completion all walk `config.List`, which sees
  only `machines/`. They switch to a `config.ListAll` that yields local names
  plus `HUB/NAME` for every conf under `hubs/`, so a laptop daemon for a hub
  machine is cycled by `update` and shown by `ports list` like any other.
  `delete desktop/web` proxies the deregistration, then stops the local daemon
  and removes `hubs/desktop/web.toml`. `delete desktop` refuses while
  `hubs/desktop/` is non-empty unless `--force`, which stops those daemons and
  removes the tree.

### 3. Dispatch lives in `resolve`, not in a root interceptor

The first positional cannot be found before cobra parses: `cp-in -r NAME …`
puts flags first, `exec` and `keys add` disable flag parsing, `status [NAME]`
is optional, and `defaults`, `hub`, `update` have no machine at all. Every leaf
already calls `resolve`, so that is the one dispatch point:

- `resolve` returns a `hubBackend` for `HUB/NAME`.
- Leaves that must run on the hub (`attach`, `shell`, `exec`, `start`, `stop`,
  `provision`, `deprovision`, `bootstrap`, `delete`, `repos *`, `keys *`,
  `lockdown`, `create`) call `a.proxy(hub, argv)`, which re-runs the laptop's
  own argv on the hub with `HUB/` stripped from the name and the persistent
  `--config-dir` flag removed.
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

`start desktop/web` proxies and then runs the laptop's own `tunnelUp`, so
laptop-configured forwards come back alongside the hub's. The hub's `start`
only knows the hub's conf.

`create desktop/web …` is the one leaf whose name does not exist yet; it
proxies the same way, and the hub writes the conf. The huh form runs on the
hub through the forwarded TTY.

**Version skew.** There is no `devvm version` subcommand, all tags are `v0.x`,
and local builds report `dev`. So: compare exact versions from `ssh HUB devvm
--version` once per hub (cached with the listing) and refuse on mismatch unless
either side is `dev`. The hub daemon's `ping` reply already carries `Version`
for the forward path.

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
**state** comes from the hub row. Its forwards column describes the desktop's
own forwards, which say nothing about what the laptop can reach on
`localhost`; the merged row's forwards column is computed from the laptop's
daemon for `desktop@web`, exactly as for a local machine. The result is
written to `<state>/hub-desktop.list` **outside** `machines/` and `run/`, so the
`status --watch` fsnotify loop does not see its own cache and re-trigger.

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

Budget: about 150 lines, no daemon changes, covered by the existing tar tests.

### 7. Forwards: two hops, one agent exec, process lifetime as scope

The laptop's per-machine daemon gets a third transport, `hubTransport`. It owns
a ControlMaster to the hub via the existing `sshTransport` code, and for each
forward runs, over that master:

```
ssh -o ControlPath=… HUB "$SHELL" -lc 'devvm __forward web GUEST'
```

`__forward` on the hub dials (spawning if needed) the hub daemon for `web`,
adds a forward **owned by this process**, prints the hub-loopback host port on
one line, and then blocks until its stdin sees EOF. The laptop daemon reads the
port, adds a native `-L host:localhost:hubPort` on its master, and keeps the
`__forward` process handle as the forward's closer: closing stdin ends the
process, which ends the hub-side forward.

Why a process and not a forwarded control socket: process lifetime is the
scope, with nothing to refcount across a second protocol client, no
unix-socket `-L` (which OpenSSH supports on a mux but leaves the client socket
file behind on master death, so `restore()` would hit "address already in
use" without `StreamLocalBindUnlink`), and no unix variant of the bindability
pre-probe. Death handling falls out: if the hub daemon cycles while the master
is alive (`update` or `stop` on the desktop), `__forward` exits, the
laptop transport sees the process die, and it re-runs `__forward` with the
daemon's existing backoff. If the master dies, the daemon goes `reconnecting`
and `restore()` re-runs everything.

**Ownership in the hub daemon.** Forwards today are keyed by guest port with no
owner, and `remove` is unconditional, so a desktop `ports rm` would silently
kill the laptop's forward. The daemon change is small and is the same one the
browser bridge needs: a forward has a set of owners (`conf`, or a control
connection), `add` from a held connection adds that connection as owner,
`remove` from the CLI drops the `conf` owner, a connection closing drops
itself, and the forward is torn down when the set is empty. This is the single
ownership model both proposals share (see section 8); ephemeral TTL forwards in
the bridge are just a third owner kind.

`ports down` on the desktop drops the `conf` owners and stops the daemon only
when no forward and no holder remains. Today it stops the daemon outright;
under this design that would make the laptop transport re-run `__forward`,
which respawns the daemon, so the desktop could never get it down and the two
would loop. With the rule the daemon stays for the laptop's forwards and the
desktop's `status --plain` shows `-`, because `up:N` counts only `conf`.

The hub daemon owns the only agent exec into the VM. The laptop never spawns
one. Both users can hold forwards to the same VM at once.

### 8. `auth` and the browser bridge

The login must run on the hub (only the hub's daemon can bridge callback
ports), but the browser must open on the laptop. The same holds for a login
started inside a proxied `attach` or `shell`. So every proxied interactive
leaf (`attach`, `shell`, `auth`) on a hub machine runs as:

```
laptop: run `ssh HUB … devvm __subscribe web` over the master. It holds the
        hub daemon, subscribes, prints one `ready` line once the daemon has
        acknowledged the subscription, then streams events on stdout and
        reads reply lines on stdin.
laptop: wait for `ready`, then proxy the leaf with DEVVM_NO_SUBSCRIBE=1 (and
        -t when interactive).
laptop: on each event, add a `connection`-owned forward in the laptop's own
        daemon for desktop@web (exact port for a redirect, bump allowed for a
        direct URL) to the hub-bound port, rewrite the URL for the laptop
        port, open it, and write the reply back.
```

Ordering cannot be left to timing. If the proxied process subscribed too, it
would either be the most recent subscriber (laptop subscribes first) and open
on the desktop, or the laptop would race the login's first URL (laptop
subscribes second). `DEVVM_NO_SUBSCRIBE=1` makes the proxied process hold
without subscribing, and the `ready` line guarantees the laptop is subscribed
before the login can emit anything.

The event carries the original URL, the guest port, the kind (direct or
redirect) and the hub-bound port, never a URL rewritten for the hub's port:
the laptop needs the guest port to know what it is forwarding and the kind to
know whether its own port must be exact. Its reply is what the guest sees, so
`devvm: opened on host -> …` names the laptop's port. If the laptop's
`__subscribe` dies mid-session the hub daemon has no subscriber and replies
"not opened"; the shim prints the URL and the user pastes it.

Where a desktop `attach` and a laptop `attach` are both subscribed to the same
VM, the daemon delivers each event to the **most recent** subscriber only.
That is the one remaining tie-break and it is deliberate (see open questions).

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

Until the bridge's client-side opening lands, `auth desktop/web` proxies
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
- `start HUB/NAME` also brings up the laptop's own forwards; `delete` cleans
  `hubs/`.
- Hidden: `__forward NAME GUEST`, later `__subscribe NAME` (prints `ready`,
  then events out / replies in). Runtime files for a hub machine use
  `HUB@NAME`.
- Env: `DEVVM_NO_SUBSCRIBE=1`, set by the proxy on every proxied command.

## Order of work

Lives in `ROADMAP.md`, the only place sequencing is written down.

## Open questions

- Should the listing cache live in `$XDG_STATE_HOME/devvm/` (new) or in the
  config dir under a name the watcher ignores? Proposed: state dir, since it is
  derived data, not config.
- Most-recent-subscriber for browser opens is simple but means a desktop user
  who starts a login while a laptop session is held opens on the laptop. Is a
  per-subscriber "I have a terminal" hint worth it? Proposed: not in v1.
