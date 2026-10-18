# CLAUDE.md

Guidance for Claude Code working in this repo. Focuses on the non-obvious
constraints; read the package docs for the rest.

## What this is

`devvm` manages persistent dev boxes across three backends: local smolvm microVMs
(`smol`, discovered from smolvm, never configured), boxes reached over ssh
(`remote`, a `[machines.*]` entry that is either managed or adopted), and
`hub`: another host running devvm, whose machines (usually smol VMs) this one
reaches as `HUB/NAME` (see Hubs below). `remote` and `hub` share one ssh
transport (`ssh.go`). Everything the user writes is one file,
`$XDG_CONFIG_HOME/devvm/devvm.toml` (`[templates.*]` + `[machines.*]`, see
Config below). It is a **host CLI** (`cmd/devvm`) plus a **guest agent**
(`cmd/devvm-agent`) the host installs into a box only when needed (see the
agent note below). It began as a ~2,100-line bash script; comments often
explain *why* a behavior exists because it was hard-won there.

**Backend axes: two, not one enum.** `backend` ∈ {`smol`, `remote`, `hub`,
`hetzner`} says who owns the registry and lifecycle (smolvm, the file, a hub,
the provider's API: providers.md); `Managed()` says whether devvm owns the OS (installs prereqs,
may harden, pins known_hosts). On a `remote` entry it is the entry's `managed`
(default **false**: a forgotten line is the safe reading); on smol it is
settled by discovery from the managed marker (below); a hub is never managed.
`IsRemote()` = `remote` or `hub` (reached over ssh), `IsHub()`,
`IsHubMachine()`, `Probeable()` (a remote box whose state is its reach cache)
are methods too. Code decides on these predicates, **never by comparing the
backend string** in cli/session/bootstrap/agentbin/reach (config.md §9 item
1). A `transport` field (`ssh`|`mosh`) steers only the interactive connection
on remote backends; forwards are always native `ssh -L`.

## Build & test

```sh
go build ./...
go test ./...     # everything is VM-independent — no smolvm or ssh host needed
gofmt -l .
./build.sh        # rebuild + re-embed the guest agents (see below)
./install.sh      # build for this host and install to ~/.local/bin (or $1)
./release.sh      # cross-compile host release binaries into dist/
```

Two Go versions, two meanings — don't conflate them:

- **`go.mod`'s `go` directive** is the **floor** — the *minimum* Go the module
  compiles with (currently `1.23.0`). It ships to consumers (`go install …@latest`
  needs at least this), so keep it low and bump it only when the code actually
  needs a newer language/stdlib feature.
- **`.go-version`** is the **exact build toolchain** (currently `1.26.4`) — the
  single source read by *both* `mise` locally (via `idiomatic_version_file_enable_tools`
  in `mise.toml`; mise can't parse `go.mod`) and CI's `setup-go` (`go-version-file:
  .go-version`). Because both compile with this identical version, the committed
  guest agents (`internal/agentbin`) are byte-reproducible. Bump the build
  toolchain by editing `.go-version` alone.

The release workflow re-runs `build.sh` and fails if a fresh build differs from
the committed `internal/agentbin/bin/*`, catching any toolchain drift before a
release ships. `mise.toml` needs `mise trust` once per clone. In some
environments the toolchain is at `/usr/local/go/bin` and not on `PATH`.

## The one-exec rule — the load-bearing constraint

smolvm has **poor concurrency across separate `machine exec` sessions**. So for a
smol machine, every port forward and auth event rides a **single** persistent
`devvm-agent serve` exec, multiplexed with yamux. This is the whole reason
`internal/session` exists: a per-machine host daemon owns that one exec for the
machine's lifetime, and the CLI is a thin client over a unix socket.

Do **not** regress to one-exec-per-connection (the original `devvm-mux` was
created specifically to escape that). The wall is about *parallel* execs, so a
single one-shot exec is fine.

ssh has no such limit: its forwards are native `ssh -L` on a ControlMaster; only
rpc/events ride the agent exec. **`authorized_keys` management is host-side**
(`internal/keys` over one plain exec — read, process on the host, atomic
write-back), so `keys` needs no agent and touches nothing on an adopt host but
`~/.ssh`. The agent is pulled in only on smol (forwards, `auth`, and the browser bridge that `attach`/`shell` subscribe to) and by `auth` on remote boxes.

A machine on a hub is the same rule one host removed: **the laptop never owns an
agent exec for a hub VM**. The hub's daemon holds the one exec; the laptop's
daemon for it is a client of that daemon over a relay (see Hubs).

## Two binaries, two targets — don't conflate them

- **`devvm-agent` (guest)** runs *inside* the box, which is **always Linux**
  (`agentbin.Install` probes `uname -s` and fails early otherwise; agent-free
  commands still work on any remote). `build.sh` builds `linux/{amd64,arm64}`
  only — never macOS. These are committed
  under `internal/agentbin/bin/` and `go:embed`-ed into the host binary.
- **`devvm` (host)** runs on the user's machine — macOS **or** Linux.
  `release.sh` cross-compiles `darwin`+`linux` × `arm64`+`amd64`.

**After changing `cmd/devvm-agent`, re-run `./build.sh` and commit the updated
`internal/agentbin/bin/*`** — otherwise the host binary embeds a stale agent.

## Layout

```
cmd/devvm/          host CLI entrypoint
cmd/devvm-agent/    guest agent: serve (forwards+rpc+events) | open-url
internal/cli/       cobra command tree + handlers (create's huh form in createform.go;
                    the `config` noun in config_cmd.go; hub proxying in proxy.go +
                    hub*.go; attach/shell's bridge in bridge.go)
internal/config/    the devvm.toml loader (file.go: [templates.*] + [machines.*], strict)
                    and its two structural writes (edit.go: append/remove one table),
                    templates, Paths (config vs state dirs) + MigrateState, saved
                    forwards (ports.go), managed markers (managed.go)
internal/backend/   Backend interface + smol.go + ssh.go (remote and the hop to a hub)
                    + hub.go (HUB/NAME: proxy argv, `__session` argv; never spawns)
internal/session/   per-machine forward daemon (owns the exec) + client + transports
                    (transport_{smol,ssh,hub}.go); daemon_session.go = sessions and
                    relays; bridge.go = URL classify/bind/deliver (browser bridge)
internal/agentrpc/  yamux stream protocol (forward/rpc/event), shared host+guest
internal/agentbin/  embedded guest agent binaries (go:embed) + on-demand install
internal/provider/  cloud providers: Provider interface + hetzner/ (plain REST) + fake/
internal/keys/      authorized_keys logic (was awk); pure/text host-side, unit-tested
internal/auth/      login orchestration, URL bridge, callback-as-forward
internal/bootstrap/ the guest steps of `setup`: EnsureUser, prereqs (install on managed /
                    check on adopt), the template's hook, hardening
                    + browser.go (smol: agent + shim + /etc/profile.d/devvm.sh)
internal/hostbrowser/ open guest login URLs on the host (sanitized)
contrib/macos/      Swift menu bar app: a thin shell over the CLI (floating drop shelf,
                    one tile per live machine = cp-in; menu + shelf fed by
                    `status --plain --watch`); built only by its build.sh
                    on macOS; release.yml attaches the zip + .sha256 sidecar
.github/workflows/  ci.yml (Go on ubuntu + Go tests and the Swift build on macos, every
                    push); release.yml (tag push)
```

## Architecture notes

- **Session daemon**: spawned via the hidden `devvm __daemon NAME` command,
  detached. Owns the smol agent exec (yamux), the ssh ControlMaster, or (hub
  machines) a relay to the hub's daemon; allocates
  host ports (bumping on conflict), and idle-exits when it has no forwards.
  When the transport dies (laptop sleep outlives ssh's keepalives) it
  **reconnects instead of exiting**: backoff 2–30s, forwards stay in the map
  closer-less as *pending* and come back on the same host ports; a `kick` op
  (sent by `ports up`/`start`) resets the backoff. It never dials a stopped smol
  VM (`smolvm machine exec` may boot it) — neither does the CLI spawn a daemon
  for one (`requireRunningForForwards`; `start`, `setup` and attach/shell,
  which start a stopped VM first, wait for "running").
  Shutdown closes the transport **before** unlinking the socket (`WaitGone`
  relies on that) and unlinks only its own (`ownsSocket`: inode check *plus* a
  dial — ext4/APFS reuse inode numbers, tmpfs doesn't, which hid the bug
  locally). Binds run outside `d.mu`: an `ssh -O forward` against a wedged
  master must not freeze `list`/`ping`. SIGTERM/INT/HUP run the same
  shutdown as a client stop (later signals are absorbed); SIGKILL orphans
  the ssh master, which the next start `-O exit`s and never adopts; a
  SIGKILLed smol daemon's exec is not covered.
  A smol daemon is also its guest's time sync: the guest runs no time
  daemon and stays behind after the host suspends. The daemon pushes its
  clock over the agent exec (a `time` stream) on every dial, on a `kick`,
  on a new subscriber and every 60s; the agent steps the guest's clock
  when the drift is ≥ 2s, with `settimeofday` as root and `sudo -n date`
  otherwise. Nothing is installed, and it runs only while a daemon holds
  the exec (a hub VM's is the hub daemon's; a bare `exec` with no daemon
  gets no sync).
- **Config vs state** (`config.Paths{Config, State}`): the config dir
  (`$XDG_CONFIG_HOME/devvm`) holds only the user's file, `devvm.toml` (safe
  to symlink into dotfiles); this host's own state lives in `$XDG_STATE_HOME/devvm`
  (`~/.local/state/devvm`): `run/` (daemon sockets, logs, locks, `changed`),
  `cache/` (hub listings, `reach/`), `ports/` (saved forwards, below), `managed/` (the
  managed markers, below), the pinned
  `known_hosts`, and the ssh ControlMaster sockets `c-%C` (short for macOS's
  sun_path). State helpers
  are methods on `Paths`, so a config dir can't be passed where state is
  meant. `--state-dir` overrides; a non-default `--config-dir` alone means the
  pre-split layout (state inside it), so a test or second registry stays
  isolated. The daemon is spawned with both flags. `config.MigrateState`
  (root `PersistentPreRun`, every invocation) moves an older build's state
  out of the config dir by link+unlink, never replacing a file, and leaves
  every run file of a machine whose old socket exists. Session clients
  (`Dial`/`Existing`) use such an old-layout daemon where it is, never
  spawning a sibling (one-exec rule); `update` cycles it unless it is
  reconnecting, and `delete`/`config rm` reap both layouts.
- **Saved forwards are host state** (config.md §5): `<State>/ports/NAME`, one
  mapping per line in `ports add` syntax (`GUEST` or `HOST:GUEST`; `#` and
  blank lines skipped), `HUB@NAME` for a hub machine, 0600 in a 0700 dir,
  written atomically; an emptied list removes the file; every
  read-modify-write (and `delete`'s/`config rm`'s removal) holds
  `config.LockPorts` (`ports/.NAME.lock`, never unlinked). Never in
  devvm.toml: the
  CLI alone reads it (`start`/`ports up` push one Add per line, `ports list`'s
  `configured:` block, `--plain`'s `down`), and **the daemon never reads it**
  (it keeps forwards in memory; clients push adds). The daemon never reads
  ports from config either; it reads its devvm.toml entry once, at start
  (its record, and `connect_timeout` for its waits). `ports add` saves
  (`--ephemeral` doesn't), `ports rm` drops a line. `delete` clears a smol
  VM's through `forgetHostState` ("everything this host remembers about
  NAME": ports file, managed marker, reach cache, pinned `known_hosts`
  entry, kept while another entry uses the same host); `config rm` through
  `forgetEntryState`, the same minus the marker (an entry may shadow a smol
  VM, whose marker is the VM's). `config.MigratePorts` seeds the dir **once
  per host** (only while `ports/` doesn't exist) from the *old* layout's
  `machines/*.toml` `ports` and hub `[machines.NAME]` tables, read with a
  lenient decoder of those keys only (it outlived the old loader so the hand
  migration need not copy forwards); per-dir rather than per-file so a list
  dropped here can't be re-seeded. It is all or nothing: any failure leaves
  no `ports/`, every ports entry point returns the error, and the next call
  retries (nothing else creates the dir). The owner kind stays `conf` (a
  compatibility surface).
- **Resolution and the managed marker** (config.md §2): `HUB/NAME` → the hub
  (record derived from its entry); a name with a `[machines.NAME]` entry uses
  it (a remote box never runs smolvm; an entry **shadows** a local smol VM of
  the same name: `shadowed` in `--plain`, a `config check` warning, and
  `create NAME` refuses). Any other name goes to `backend.Discover`, an
  ordered list of sources, first match wins; smol is the only one: one
  `smolvm machine ls --json` (existence, state, sizes, `created_at`, pinned
  by `backend/testdata`), the row kept on the backend (`SmolRow`, so `status
  -v` needs no guest exec). A devvm.toml that does not parse fails every
  name (an entry would win, and a broken file can't say whether one does).
  **The source settles `Managed`** on the record: managed iff
  `<State>/managed/NAME` holds the VM's `created_at`; no marker, a stale one
  (VM recreated by raw smolvm) or a row with no `created_at` is unmanaged
  (`backend.SmolRecord`). `Managed()` is a field read and never execs.
  `create` and `setup --adopt` write the marker; so does the one-time
  upgrade seed `backend.SeedLegacyMarkers` (root `PersistentPreRun`, while
  `machines/` still holds `backend = "smol"` confs: an unmarked one gets its
  VM's current `created_at`, one `ls` for all, an existing marker never
  rewritten), so the VMs devvm made before markers stay managed. Discovery
  itself never writes one. The daemon
  re-settles through `backend.Refresher` before every dial, first included,
  on a copy of the record, so a VM recreated while it slept turns unmanaged:
  the dial stops before any exec and the daemon exits (`ErrUnmanaged` is
  final in reconnect; the transport's install gate is the backstop). A VM
  gone since reads as not running. `created_at` is normalized to a decimal
  integer before compare/write.
- **Unmanaged smol VMs do little**: `status`, `start`, `stop`, and
  `shell`/`exec` as smolvm's default user over a plain `machine exec` (no
  sudo, no `BROWSER`, no tmux, nothing installed: `resolveUnmanaged`).
  Everything else goes through `resolve`, whose `requireManaged` refuses in
  one line (`backend.AdoptHint`: `setup --adopt NAME`, or `smolvm machine
  rm` + `create`);
  `bridged()` requires `Managed()`; `delete` refuses and points at `smolvm
  machine rm` (as it refuses an entry and points at `config rm`). `ports rm`/`ports list` are host-side only (`portsRecord`),
  so a stray ports file can always be listed and emptied.
- **Change marker**: every state-changing command and daemon transition calls
  `config.TouchChanged` (a *write* to `run/changed`, because kqueue ignores
  utimes); `status --plain --watch` (fsnotify on the config **dir**, so
  devvm.toml renamed into place is seen; a symlinked file's target dir,
  re-resolved on every config-dir event and on every debounce so a stow/chezmoi
  relink moves the watch; kqueue can drop a relink, so a one-shot `status`
  touches the change marker on its way out, and the menu bar app runs one per
  menu open: that is the refresh, never a timer; and state's `run/`; config-dir events count only for devvm.toml
  and its target, so editor swap files cost nothing) re-emits on it, so a write elsewhere in state (a
  `ports/` file) is seen only through the marker. Rule: a new state-changing
  command gets `defer config.TouchChanged(a.Paths)`. devvm.toml writes are
  atomic (temp + rename) so the watcher never reads a half-written file. A
  file that stops parsing pauses the watch (no block, one stderr line, the
  last block stays true); one broken at start fails it, like a one-shot
  `status`, which exits non-zero on a broken file.
- **Reach cache + refresh** (`providers.md` §5, `internal/reach`): a remote row's state is
  `cache/reach/NAME`, never a live call on the snapshot path. It is refreshed by one-shot `status`, by
  `__refresh` (writes `run/refresh`; the watcher runs one round, joined if in flight) and by attach/shell/exec/setup.
  The probe is `ssh -O check`, then `ssh -G` + a TCP connect (2s, or the entry's `connect_timeout`
  capped at the 5s round): never a full ssh, never polled. A connect that times out is `?`, not
  `unreachable` (a slow Tailscale peer is not a dead one); refused/unroutable is `unreachable`.
- **Providers** (`providers.md`, `cli/providers.go`): no entry; resolve asks each `[providers.*]` (one Get) only after entry+smol miss and with a token
  (`<State>/providers/NAME.token` via `providers token`; `HCLOUD_TOKEN` wins). `status` reads `cache/provider-NAME.list`, written only by the
  live round (one-shot `status`, `__refresh`); `--local` skips providers; daemon, `__session`, completion and `config rm` read the cache only (no API, no token). Lifecycle (`cli/providers_lifecycle.go`):
  `create --backend hetzner` refuses everything before Create (name, collisions, key, user), then hands the server to `shape` unchanged (never deletes it on failure);
  `delete`/`start`/`stop` act on the row resolve just fetched, merge the answer into the cache; `stop` warns it still bills; unmanaged: start/stop confirm, delete refuses. `forgetHostState` drops the address pin too.
- **`--plain` is a compatibility surface** parsed by the Swift app:
  `name\tbackend\tstate\tforwards\tmanaged` with backend ∈ {smol, remote,
  hub, hetzner}, state ∈ {running, stopped, starting, stopping, reachable,
  unreachable, broken conf, ?} (a `hetzner` row's state is mapped from the API's status,
  never `unreachable`; its managed column is the devvm=true label), forwards ∈
  {up:N, reconnecting:N, down, -} and managed ∈ {managed, unmanaged,
  shadowed, -}. A hub older than devvm.toml sends `remote-managed`/
  `remote-unmanaged` (and maybe `broken conf`/`dormant`): the laptop maps
  the former to `remote` + the managed column (`parseHubRows`) and passes
  the rest through. The fifth column
  is appended (≥4 fields must keep parsing): `shadowed` is an entry hiding a
  local smol VM of the same name, `-` is a hub, or a hub machine from a hub
  too old to send it (never `unmanaged`, which nobody said). A sixth column,
  appended the same way, is `verified=<unix>:<how>` (or `verified=-`) on a
  remote row (a hub machine carries its hub's through) and `-` on every other. A
  devvm.toml that does not parse is one row `-\t-\tbroken conf\t-\t-\t-` (exit 0;
  `--watch` keeps watching until it parses). There is no
  `dormant` (an old hub may still send it); so
  are the `ports list` line (`guest N -> localhost:M [(pending)] [owner=K+K] [exact] [ttl=9m42s]`,
  owner kinds `conf`/`connection`/`ttl`; parsers split on whitespace and read only
  the first four tokens plus `(pending)`, so anything new goes after them), `devvm
  --version` (`devvm version vX.Y.Z`), and `update --check --plain`
  (`current\tlatest\ttrue|false`). Changing any of them means a Swift change.
- **Maintain devvm itself** (help group): `update` finds the latest tag via the
  `releases/latest` redirect (no API), verifies against SHA256SUMS, renames over
  the running binary, then re-execs the new one with hidden `--finish-from` to
  cycle live forward daemons (skipping reconnecting/same-version ones; one
  holding no conf-owned forward is stopped, not respawned, so `ports down`
  sticks and session clients respawn it themselves) and bring an installed
  menu bar app along. `menubar` installs/opens the
  app at this build's version (`.sha256` sidecar, `ditto` so the ad-hoc
  signature survives). Both need a real version: `-X cli.Version` is stamped by
  install.sh/release.sh from `git describe`; a plain `go build` (or a tagless
  clone) is `dev` and they refuse without `--force`/`--version`. `install()`
  relaunches the app *before* printing anything — when the app itself ran the
  command, quitting it closed our stdout (SIGPIPE is ignored in `Execute` for
  the same reason).
- **Env overrides**: `DEVVM_SSH_CONNECT_TIMEOUT` (seconds or duration; every
  ssh/scp/mosh call gets `ConnectTimeout`; wins over an entry's
  `connect_timeout`, which wins over the default 10) and
  `DEVVM_COMPLETE_TIMEOUT` (duration; guest path completion, default 2s; the
  app sets 10s for cold ControlMasters).
- **Config** (`devvm.toml`, config.md §1, §4) is the user's hand-editable
  TOML, meant for a dotfiles repo shared by every host; keep it that way
  (don't hide state in an opaque DB, don't re-encode it). The loader is
  strict: an unknown top-level table or key is an error (so a later family,
  `[providers.hetzner]`, landed that way), every problem
  is reported at once, and a missing file is valid (no entries, built-in
  templates). A `[machines.NAME]` entry: `backend` **mandatory**, `remote` or
  `hub` (`smol` is an error: smol VMs are discovered; the old
  `remote-managed`/`ssh` values are errors naming the fix); `managed` (remote
  only; an error under a hub); `host` (`host[:port]`, `[v6]:port`, or an ssh
  alias; a `user@` there is an error); `user` (unset = no `user@`, ssh
  config decides, except a **managed** remote entry, which logs in as
  `[templates.default]`'s user: the account `setup` creates); `transport`,
  `mosh_server`, `identity`, `connect_timeout`. **VM commands never write
  the file; only `config add`/`config rm` do**, and only structurally:
  `add` appends one whole table at EOF (always writing `backend`, and
  `managed` on remote), `rm` cuts one table's line range (its keys, glued
  trailing comments, the blank gap; a comment block above the next header
  stays). Both match the file's line ending (CRLF stays CRLF), re-parse and
  require that nothing but that entry changed before an atomic rename,
  through a symlink to its target; a **dangling** link is an error naming
  link and target, for reads and edits alike (never an empty file, never a
  file renamed over the link). Everything else is `config edit`
  (`$VISUAL`/`$EDITOR`, then check; a failing check keeps the edit and
  saves the pre-edit file as `.bak` beside the real file, with its mode).
  HUB/NAME is refused by the noun: it lives in the hub's file. No `config`
  command dials a box; `config check` (offline, non-zero on error) also
  checks hook specs/port mappings and warns on collisions. `config rm` owns
  this host's state: it **plans** the cut first (`PlanRemoveEntry`: every
  refusal happens before any daemon is touched), refuses while a daemon
  for the entry (or `NAME/*` for a hub) holds live forwards unless
  `--force`, stops those daemons, commits the edit (refused if the file
  changed since the plan), then clears saved forwards, reach cache, the
  hub cache, the pin and locks, carrying on past failures ("removed
  [machines.X]; but: …"). Never a managed marker (an entry may shadow a
  smol VM). `config add` of a hub clears nothing: forwards already there
  are what the upgrade seeded. A hub entry named exactly `os.Hostname()` is
  marked `(this host)` in `status`/`config show`: cosmetic only. With the
  old `machines/` (non-empty: an older `status --watch` made an empty one)
  or `config.toml` and no devvm.toml, every command runs the two upgrade
  seeds (forwards, markers), then prints one line pointing at the hand
  migration (config.md §8) and sees no entries; nothing else reads the old
  files.
- **Connect surface**: `attach` (persistent dev tmux) and `shell` (raw login
  shell), both backend-dispatched via the `Interactive` interface and taking
  `--transport ssh|mosh`. There is no `ssh`/`mosh` command. `create` makes a
  **local smol VM only** (flags-first, huh prompts on a TTY seeded from the
  template; `--backend` stays, default and only value `smol`, for the provider
  later): a remote box or hub is `config add` (its huh form when `--host` is
  missing on a TTY), then `setup` for a managed box. `--yes` skips prompts and
  resolves every unset field from flag > template > built-in (erroring on a
  required field with no default). Multi-op nouns are grouped:
  `repos {add,list}`, `ports {add,rm,list,up,down}`,
  `keys {add,rm,list,dedupe}`, and `config {path,show,edit,check,add,rm}`.
  NAME is always the **first positional of the leaf**
  (`devvm repos add NAME REPO`) so completion and "first arg = machine" hold at any
  depth. Single-action verbs (create/start/stop/delete/attach/…)
  stay flat, but `--help` clusters them into Lifecycle/Connect/Configure/Config
  groups in an explicit lifecycle order via `cobra.EnableCommandSorting=false` +
  `AddGroup`/`GroupID` (see `rootCmd`). Repos are not tracked: `repos add`
  clones into `~/src` (an existing checkout is skipped; `owner/repo` shorthand
  via gh, any URL via plain git), and `repos list` lists the guest's git
  checkouts (one exec, so a stopped smol VM is refused, never booted).
- **Keys**: `internal/keys` is pure text logic (SHA256 fingerprint computed in-Go,
  matches `ssh-keygen`). It runs **host-side** — read `~/.ssh/authorized_keys`,
  process on the host, write back atomically (`umask 077` + temp + `mv`, content
  over stdin so nothing is shell-interpolated). No agent, so it works on adopt
  hosts with zero footprint. `internal/keys` preserves options/comments verbatim.
- **Hubs** (`docs/proposals/hub.md`): a hub is a `[machines.HUB]` entry with
  `backend = "hub"`, reached like a remote box and never shaped like one (it
  needs only `devvm` and `smolvm`; registering one dials nothing, so its
  version is checked only by the proxied verbs that need the floor). Its VMs
  are addressed `HUB/NAME` and have **no entry of their own** on the laptop:
  the record is derived from the hub's entry, and the laptop's own state for
  one (its saved forwards, `<State>/ports/HUB@NAME`) is host state like any
  machine's, so the hub's registry is the only one. `delete HUB/NAME` still
  proxies; the hub entry itself goes with `config rm HUB`. The merged
  listing hides a hub's `remote` row whose name is also an entry here (the
  direct path is right; a hub is not a jump host: that is `ProxyJump` on the
  `host` alias). Runtime files (daemon socket, log, lock)
  use `HUB@NAME`, because `run/HUB/NAME.sock` would name a directory; only
  `config.RuntimeName` and `DisplayName` convert between the forms. Commands
  **proxy**: the cli rebuilds
  the leaf's argv with the hub-side name and runs `env DEVVM_NO_SUBSCRIBE=1
  devvm …` over the hub's ssh. The env var keeps a proxied attach/shell/auth
  from subscribing on the hub, since the laptop subscribes through its own
  daemon instead. Forwards: the laptop's daemon for `HUB@NAME` runs one hidden
  `__session NAME` over its own ControlMaster. That is a **relay** session on the
  hub daemon: its adds are owned by the connection, it is heartbeated, and it is
  refused while the hub's transport is down and closed when it dies. The laptop
  then binds an `ssh -L` to the hub port the hub reports. Browser events cross the
  same relay while the laptop has a subscriber (the hub daemon binds nothing
  for a relay; the laptop daemon does), but only if the hub's open reply
  echoed `Bridge`. `Bridge` means "speaks the relay bridge *and* its transport
  has events"; `NoEvents` means a current hub whose machine is not smol;
  neither means a hub too old. A proxied attach waits at most 5s for its bridge,
  then adopts a late one, retrying failures in the background (a refusal for
  good excepted). attach/shell on a stopped VM **start it first**, as
  `start` does (`bootForSession`; smolvm 1.16.1's `machine exec` refuses a
  stopped VM, it never boots one), then open the bridge: on a hub, a
  `__session` refusal for a VM not running within the 5s makes the laptop
  proxy `start NAME` (no `-t`) and open once more (`hubBootForSession`);
  past the 5s the retry adopts it once the hub-side attach has started it.
  Listings fan
  out as `status --plain --local --watch` on each hub (`--local`, so hubs never
  chain) and are cached per hub. Minimum hub versions are pinned by tests
  (`hubMinVersion`, `session.HubForwardsMinVersion`, `HubBridgeMinVersion`).
- **Browser bridge** (`docs/proposals/browser-bridge.md`): guest programs get
  `BROWSER` = the `devvm-open-url` shim, which hands the URL to the agent; the
  agent sends it up the daemon's one exec as an `open-url` event. The **daemon**
  classifies it (external / direct loopback / OAuth redirect), binds the forward
  a loopback URL needs (connection-owned, or `ttl`-owned and exact for a
  redirect), and delivers it to its **most recent subscriber**. That is a session
  held by `attach`/`shell`/`auth`, which opens the URL on its own host (or prints
  it there if it has no opener); its answer goes back to the guest. The daemon
  never opens a browser itself. With no subscriber, the shim prints the URL in the
  guest. Opens are rate-limited (10/min) and bridge forwards are capped per
  machine. Remote boxes have no events until the ssh agent exec ships (their
  transport's `events()` is nil).
- **Auth**: smol and hub machines use **thin auth** over the daemon's session:
  ping reads the shim path (nothing installed), subscribe, bind the fixed
  callback (codex's `:1455`) as an exact session-owned forward, then run the
  logins with `BROWSER` = the shim. So `auth` during a held attach adds no second
  agent exec. `auth HUB/NAME` does this on the laptop and proxies `auth NAME` to
  the hub, whose auth only pings. Remote boxes keep the private `--auth` agent
  exec (its own guest socket) until the ssh agent exec lands.
- **Agent install** (`internal/agentbin`): idempotent by sha256, arch-matched,
  `$HOME`-resolved (never hardcode `/home/dev`). Managed boxes get a root-owned
  `/usr/local/bin` install; adopt hosts get a **user-scoped `~/.local/bin`**
  install, gated behind explicit consent (`auth --install-agent` or a prompt) so
  devvm never writes to an adopted box unasked. The posture is keyed on
  `Managed()` alone. The smol transport's install gate **fails** when
  `!Managed()` (a detached daemon can't ask anyone), so nothing is installed
  into a smol VM devvm did not create. `Install` returns the paths to use
  (agent and shim). On smol, `setup` installs agent + shim and writes
  `/etc/profile.d/devvm.sh` (`bootstrap.Browser`), so profile.d never names a
  missing file; smol `create`/`setup` **fail** if that install fails. smol
  `shell`/`attach` also carry `BROWSER` in their exec env (attach copies it
  into the `dev` tmux session), dropped if the shim is missing.
- **Root adoption (fresh cloud VMs)**: `setup` on a managed remote box first
  waits for ssh, **bounded** (`setupReachWait`, 90s: a just-made VM or a
  cloud-init sshd bounce; only "unreachable" is retried). It tries `USER@host`
  — the entry's `user`, else the template's (default `dev`); on *permission
  denied* (a BatchMode stderr match: exit 255 alone is also "unreachable") it
  tries `root@host`, and a box that answers only as root gets
  `bootstrap.EnsureUser(USER)` (NOPASSWD sudo, root's `authorized_keys`),
  then setup continues as `USER`, so the hook/keys/repos never land in
  `/root`; it re-probes as `USER` first. **Nothing is written**: with no
  `user` in the entry every later command logs in as `[templates.default]`'s
  user, which is what setup just made. A `--template T` whose user differs
  from that login is **refused before anything runs** (it would create T's
  user and pin `AllowUsers` to it, locking every later command out): the
  fix is `user = "…"` in the entry. A changed host key or "Too many authentication
  failures" is fatal at once, and any exit but 255 means the host answered.
  An entry login that is neither root nor the template's user wins, with a
  warning; `user = "root"` on a root-only box is refused. A stopped smol VM
  is started (as `start` does) before setup's first exec. Only `setup` ever
  tries root, and only on remote + managed. This is what establishes the
  invariant `rootWrap` assumes (login user = unprivileged user with sudo).
  `setup --host ADDR` is that run's address only. `Harden` pins `AllowUsers`
  to the actual connection user and refuses to run as root (it would write
  `PermitRootLogin no` + `AllowUsers root` — a lockout); `lockdown` takes
  fail2ban from `[templates.default]`.
- **Vocabulary — don't re-conflate these.** `create` allocates the resource (the
  VM/disk) and writes the managed marker; `delete` destroys it. **`setup`**
  (was `bootstrap`) shapes a box and is the single place devvm touches its
  contents, in a fixed order: ensure user → prereqs (incl. tmux, gh,
  mosh-server) → agent + shim (+ profile.d, smol) → **`hook`** → repos → keys
  → harden → seed saved forwards. The **`hook`** (template key) is the
  *user's* step inside it, often a dotfiles script
  itself named `bootstrap.sh`. So `create` = make the box exist; `setup` =
  shape it; `hook` = the user's part of shaping. `setup --adopt NAME` is the
  only promotion of a smol VM devvm did not make: it prints the plan and needs
  the VM's **name typed** on a TTY (no TTY, no adoption; `--yes` never
  applies), then writes the marker. A remote entry with `managed = false` is
  never shaped (writing `managed = true` is the consent; `--adopt` is
  refused there). `config add`/`config rm` = the entry exists here or not,
  independent of the box.
- **Lifecycle states**: a machine exists or it doesn't. There is **no dormant
  state** and no `provision`/`deprovision` (config.md §2): a smol VM is
  running or stopped, a remote box is reachable. Rebuilding is `delete` then
  `create`.
- **Templates** (`internal/config/templates.go`, config.md §3): `[templates.*]`
  in devvm.toml, beside `[machines.*]`. Fields: `user` (default `dev`; remote
  only), `hook` (`url:`/`cmd:`/`none`, built-in **`none`**: nothing personal
  baked into the binary), `memory`/`disk` (create-time, smol), `repos`,
  `ports`, `authorized_keys[_github]`, `harden`/`fail2ban` (remote). Fields
  that don't apply are ignored with one warning line. Unset fields fall back to
  `[templates.default]`, **one level**. Precedence for create/setup: flag >
  `--template T` > `[templates.default]` > built-in. `config show` prints each
  template resolved, annotated with where each value came from. Nothing records which template made a box: `setup
  NAME` re-runs whatever `default` says now. A proxied `create`/`setup HUB/NAME
  --template T` resolves T on the laptop and sends every field as the hidden
  template flags (`tmplFlags`, `--inline-template`; key sources as `--key`
  lines, skipped for a smol hub machine), so the hub's file never matters.
  Every proxied create/setup strips `--port` and sends `--no-ports` (the hub
  saves and prompts for none); the laptop seeds its own `HUB@NAME` forwards
  after success. A hub too old for `setup`/these flags maps to one line
  naming `devvm update` (`runProxiedShaping`).
- **No global defaults file**: `config.toml` and the `defaults` noun are gone;
  their values are `[templates.default]`, edited with `config edit`.

## Testing gotchas (all real, learned the hard way)

- **yamux deadlocks over `net.Pipe`** (unbuffered/synchronous). Tests use a real
  loopback TCP socket pair — see `tcpPipe` in the agentrpc tests.
- **`agentrpc.Stdio.Close` is a no-op**: the exec is torn down by killing the
  process, not by closing the pipe. So `yamux.Session.Close()` blocks until the
  underlying conn is closed — close the conns first in tests.
- **Port allocation needs a concrete preferred port**; `0` ("any") can't be
  reported back to the caller.
- **Unix-socket tests must not rely on inode identity.** tmpfs (`/tmp` here)
  hands out inodes monotonically; ext4 (CI) and APFS reuse them at once, so a
  successor socket can share dev+ino with the one just unlinked.
- **cp**: the upload archive is built in Go (no host tar, no xattr pax headers);
  the guest needs only a busybox-compatible `tar` (no `--no-same-owner`, no
  `realpath`). Staging is `/var/tmp` (`/tmp` is tmpfs-shadowed under `machine
  cp`); smol cp-out stages its tar there too and pulls it with `machine cp`
  (`backend.Downloader`), because smolvm caps exec stdout at 11 MiB.
  Extraction guards for cp-out live in `cp_tar.go` and are tested with
  hostile archives — keep them all-or-nothing. cp-in/cp-out run under
  `interruptible`: a signal kills the host command via the context, the
  spool is removed on the way out, and Execute re-raises the signal.

## Releases

Tag `vX.Y.Z` and push — `.github/workflows/release.yml` builds the host binaries
as a **draft** release, then a macOS job builds `contrib/macos` and attaches
`devvm-menubar.zip` + `.sha256` and publishes. If the macOS job fails the release
stays a draft (`gh release edit vX.Y.Z --draft=false` ships without the app).
Manual equivalent: `./release.sh && gh release create vX.Y.Z dist/devvm-*
dist/SHA256SUMS`.

## Conventions

- Match the surrounding style; comments explain the *why* (the constraints
  above), as carried over from the original bash.
- End commit messages with a `Co-Authored-By:` trailer when pairing.
