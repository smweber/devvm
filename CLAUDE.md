# CLAUDE.md

Guidance for Claude Code working in this repo. Focuses on the non-obvious
constraints; read the package docs for the rest.

## What this is

`devvm` manages persistent dev boxes across four backends: local smolvm
microVMs (`smol`) and Hetzner Cloud servers (`hetzner`), both made by devvm
and discovered by their `devvm=true` label, never configured; boxes reached
over ssh (`remote`, a `[machines.*]` entry devvm only connects to); and
`hub`: another host running devvm, whose smol VMs this one reaches as
`HUB/NAME` (see Hubs below). `remote`, `hub` and `hetzner` share one ssh
transport (`ssh.go`). Everything the user writes is one file,
`$XDG_CONFIG_HOME/devvm/devvm.toml` (`[templates.*]` + `[machines.*]` +
`[providers.*]`, see Config below). The rule that settles most questions
(simplify.md): **devvm shapes what it creates and only connects to
everything else.** It is a **host CLI** (`cmd/devvm`) plus a **guest agent**
(`cmd/devvm-agent`) the host installs into a box only when needed (see the
agent note below). It began as a ~2,100-line bash script; comments often
explain *why* a behavior exists because it was hard-won there. smol needs
**smolvm ≥ 1.35.5** (its start and `--stream` behaviour, below):
`backend.SmolvmMinVersion`, pinned by a test. `backend.SmolReady` (one
`smolvm --version`) checks it in `create` (before its prompts) and in
`PowerStart` (every boot of a stopped VM, attach/shell/setup's included; a
hub's VM is checked by the hub's own proxied devvm), refusing below it in
one line that says to stop every VM before upgrading; a `--version` that
fails or prints no X.Y.Z is no block. Listing, exec and cp never run it.
devvm never installs or upgrades smolvm (an upgrade breaks a running VM,
and devvm cannot see unlabelled ones to stop them).

**Backend axes: two, not one enum.** `backend` ∈ {`smol`, `remote`, `hub`,
`hetzner`} says who owns the registry and lifecycle (smolvm, the file, a hub,
the provider's API: providers.md); `Managed()` says whether devvm made the
box and so owns its OS (installs prereqs, may harden, pins known_hosts). It is
not a setting: true for every discovered record (only a labelled VM or server
is discovered at all), false for every entry and hub. There is no `managed`
key, no marker file and no unmanaged discovered machine.
`IsRemote()` = `remote`, `hub` or a provider (reached over ssh), `IsHub()`,
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

smolvm 1.16.1 had **poor concurrency across separate `machine exec` sessions**.
So for a smol machine, every port forward and browser event rides a **single**
persistent `devvm-agent serve` exec, multiplexed with yamux. This is the whole
reason `internal/session` exists: a per-machine host daemon owns that one exec
for the machine's lifetime, and the CLI is a thin client over a unix socket.

Do **not** regress to one-exec-per-connection (the original `devvm-mux` was
created specifically to escape that). The wall was about *parallel* execs, so
a single one-shot exec is fine. smolvm 1.35.5 showed no such wall (30 parallel
execs, 25 concurrent 8 MiB streams: simplify.md, ThinkPad answers), but the
rule stays: the bridge's one guest socket and the clock sync assume one
daemon holding one exec.

ssh has no such limit: its forwards are native `ssh -L` on a ControlMaster; only
events ride the agent exec. **`authorized_keys` management is host-side**
(`internal/keys` over one plain exec — read, process on the host, atomic
write-back), so `keys` needs no agent and touches nothing on an entry but
`~/.ssh`. The agent is pulled in by the forward daemon: on smol it carries
everything (forwards, the clock's time stream, the browser events
`attach`/`shell` subscribe to); on a box reached over ssh one `devvm-agent
serve` exec rides the daemon's own ControlMaster for browser events only. Either way it is one
exec per daemon and one guest socket (`~/.devvm/agent.sock`).

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
cmd/devvm-agent/    guest agent: serve (forwards+events+time) | open-url
internal/cli/       cobra command tree + handlers (create's huh form in createform.go;
                    the `config` noun in config_cmd.go; hub proxying in proxy.go +
                    hub*.go; attach/shell's bridge in bridge.go)
internal/config/    the devvm.toml loader (file.go: [templates.*] + [machines.*], strict)
                    and its two structural writes (edit.go: append/remove one table),
                    templates, Paths (config vs state dirs), saved forwards
                    (ports.go), [providers.*] (providers.go)
internal/backend/   Backend interface + smol.go + ssh.go (remote and the hop to a hub)
                    + hub.go (HUB/NAME: proxy argv, `__session` argv; never spawns);
                    optional capabilities asserted by the cli (Refresher, RootCopier,
                    Deleter: only smol's VMs are deleted through a backend);
                    discovery.go = DiscoverSmol
internal/session/   per-machine forward daemon (owns the exec) + client + transports
                    (transport_{smol,ssh,hub}.go, transport_ssh_agent.go = a remote
                    box's agent exec); daemon_session.go = sessions and
                    relays; bridge.go = URL classify/bind/deliver (browser bridge)
internal/agentrpc/  yamux stream protocol (forward/event/time), shared host+guest
internal/agentbin/  embedded guest agent binaries (go:embed) + on-demand install
internal/provider/  cloud providers: Provider interface + hetzner/ (plain REST) + fake/
internal/keys/      authorized_keys logic (was awk); pure/text host-side, unit-tested
internal/bootstrap/ the guest steps of `setup`: EnsureUser, prereqs, the template's
                    hook, hardening + browser.go (agent + shim + /etc/profile.d/devvm.sh)
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
  VM (smolvm's `machine exec` refuses one, 1.16.1 and 1.35.5 alike; an exec
  racing a boot only waits for it) — neither does the CLI spawn a daemon
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
  A daemon for a box reached over ssh (`transport_ssh_agent.go`) also runs
  the agent exec on its own master (`<name>.master`, via
  `backend.MasterBinder.OnMaster`, never the CLI's `c-%C`), and agentbin's
  probes and install ride it too. A provider server devvm made:
  `agentbin.Install` (root, as smol). An entry with `agent = true`: the
  user-scoped `~/.local/bin` install (the key is the consent, and replaces a
  stale agent after `devvm update` by itself). Any other entry:
  `agentbin.Probe` only, never a write; with no current agent the transport
  has no shim and no events and waits for a nudge (`kick`, a new
  subscriber), say after an agent was put there by hand. The record is
  captured once, at the daemon's start (`sshAgentHooks`), so an `agent =
  true` added later counts from the next start (`ports down`, end the
  attach). The agent failing never fails
  the transport (forwards don't need it). Exec dies with the master up →
  respawned by the transport (1s doubling to 30s), forwards untouched;
  master dies → the transport is marked dead and the daemon's reconnect
  rebuilds both. No clock sync on remote (it runs its own time daemon).
  A daemon spawned only for an attach's subscription idle-exits once the
  session ends with no forwards.
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
  `cache/` (hub listings, provider listings, `reach/`), `ports/` (saved
  forwards, below), `providers/` (API tokens), the pinned `known_hosts`
  (provider servers only), and the ssh ControlMaster sockets `c-%C` (short
  for macOS's sun_path). State helpers
  are methods on `Paths`, so a config dir can't be passed where state is
  meant. `--state-dir` overrides; a non-default `--config-dir` alone keeps
  its state inside it (`cli.stateDirFor`; the layout before the split),
  so a test or second registry stays isolated; naming the default config dir
  keeps the default state dir. The daemon, and `update`'s re-exec, get both
  flags. There is no upgrade code for older layouts (simplify.md step 16):
  laptop and hubs update together, and no host still has them.
- **Saved forwards are host state** (config.md §5): `<State>/ports/NAME`, one
  mapping per line in `ports add` syntax (`GUEST` or `HOST:GUEST`; `#` and
  blank lines skipped), `HUB@NAME` for a hub machine, 0600 in a 0700 dir,
  written atomically; an emptied list removes the file; every
  read-modify-write (and `delete`'s/`config rm`'s removal) holds
  `config.LockPorts` (`ports/.NAME.lock`, never unlinked). Never in
  devvm.toml: the
  CLI alone reads it (`start`/`ports up`, and `attach`/`shell` as they
  connect, push one Add per line; `ports list`'s `configured:` block,
  `--plain`'s `down`), and **the daemon never reads it** (it keeps forwards
  in memory; clients push adds). attach/shell's push is best-effort, quiet,
  in the background on the session's own client, and never fails the
  session; a proxied attach on a hub pushes on the laptop only. So `ports
  down` holds until the next `attach`/`shell`/`start`/`ports up`. The daemon
  never reads ports from config either; it reads its devvm.toml entry once,
  at start (its record, and `connect_timeout` for its waits). **`ports add`
  is the only way to save a forward** (no template `ports`, no `--port`, no
  `--ephemeral`), `ports rm` drops a line. `create` clears a leftover file
  for the name once its VM exists (`clearLeftoverPorts`: a box removed
  outside this host leaves its file, and the first attach would raise a
  predecessor's forwards); the laptop does the same for `HUB@NAME` after a
  proxied create that exited 0 or whose output carries `wasCreated(name)`.
  `delete` clears a machine's through `forgetHostState` ("everything this
  host remembers about NAME": ports file, reach cache, and a provider
  server's pinned `known_hosts` lines); `config rm` through
  `forgetEntryState` (ports file, reach cache). The ports dir is made on
  demand (0700). The owner kind stays `conf` (a compatibility surface).
- **Resolution and ownership** (config.md §2, simplify.md steps 13–15):
  `HUB/NAME` → the hub (record derived from its entry); a name with a
  `[machines.NAME]` entry uses it (a remote box never runs smolvm; an entry
  **shadows** a discovered machine of the same name: `shadowed` in
  `--plain`, a `config check` warning, and `create NAME` refuses). Any other
  name goes to `backend.DiscoverSmol`: one `smolvm machine ls --json` (existence,
  state, sizes, labels, pinned by `backend/testdata`), the row kept on the
  backend (`SmolRow`, so `status -v` needs no guest exec). After that miss,
  resolve asks each `[providers.*]` (Providers, below). A
  devvm.toml that does not parse fails every name (an entry would win, and a
  broken file can't say whether one does). **Ownership is a label on the
  machine**: a smol VM is devvm's iff it carries `devvm=true` (`create`
  passes `smolvm machine create --label devvm=true`), a server iff it has
  the same label. An unlabelled one is invisible: no row, no completion, no
  shadowing, and a command naming an unlabelled smol VM says in one line
  that devvm did not create it and how to label it (one more `ls`, on that
  failure path only); `create X` refuses the name. No host file says who
  made a VM, and devvm reads no `created_at`.
  The daemon re-settles through `backend.Refresher` before every dial, first
  included, on a copy of the record: "still listed with the label". A VM
  gone or recreated without it reads as not running, so it is never dialled
  or installed into; reconnect waits for it as for a stopped VM, logging
  why once per change. `ports rm`/`ports list` are host-side only
  (`portsRecord`), so a stray ports file can always be listed and emptied.
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
  `create --backend hetzner` refuses everything before Create (name, collisions, key), labels the server `devvm=true` (its only label), then hands it to `shape` unchanged (never deletes it on failure);
  `delete`/`start`/`stop` act on the row resolve just fetched, merge the answer into the cache; `stop` warns it still bills. Labelled servers only: resolve's Get ignores an unlabelled server, and the cache keeps labelled rows; the live round's one List is unfiltered and counts the rest (`unlabelled=N` in the cache), so a one-shot human `status` ends with one line naming that count (a billing server that lost its label is still noticed). Adding the label by hand is the only promotion. `forgetHostState` drops the address pin too.
- **`--plain` is a compatibility surface** parsed by the Swift app:
  `name\tbackend\tstate\tforwards\tmanaged` with backend ∈ {smol, remote,
  hub, hetzner}, state ∈ {running, stopped, starting, stopping, reachable,
  unreachable, broken conf, ?} (a `hetzner` row's state is mapped from the API's status,
  never `unreachable`), forwards ∈ {up:N, reconnecting:N, down, -} and
  managed ∈ {managed, shadowed, -}. A hub's rows are passed through as it
  sent them (`parseHubRows` maps nothing). The fifth column is appended (≥4
  fields must keep parsing): `managed` is a smol VM or server devvm made
  (its label: an unlabelled one has no row), `shadowed` an entry hiding a
  discovered machine of the same name, `-` an entry or a hub; a hub
  machine's is what its hub sent (`-` if it sent none). A sixth column,
  appended the same way, is `verified=<unix>:<how>` (or `verified=-`) on a
  remote row (a hub machine carries its hub's through) and `-` on every other. A
  devvm.toml that does not parse is one row `-\t-\tbroken conf\t-\t-\t-` (exit 0;
  `--watch` keeps watching until it parses). There is no `dormant`.
  Compatibility surfaces too: the `ports list` line (`guest N -> localhost:M [(pending)] [owner=K+K] [exact] [ttl=9m42s]`,
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
  menu bar app along (a failed app update exits non-zero, naming `devvm
  menubar`, so the app shows it instead of relaunching the old bundle). `menubar` installs/opens the
  app at this build's version (`.sha256` sidecar, `ditto` so the ad-hoc
  signature survives). Both need a real version: `-X cli.Version` is stamped by
  install.sh/release.sh from `git describe`; a plain `go build` (or a tagless
  clone) is `dev` and they refuse without `--force`/`--version`. `install()`
  quits the app with SIGTERM (SIGKILL if it lingers; never an Apple event,
  which needs TCC consent and can hang), waits until it has exited, and only
  then `open -a`s the new bundle (retried once if nothing starts), and it
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
  `hub` (`smol` and `hetzner` are errors: those are discovered); `agent`
  (remote only; an error under a hub): `true` is the consent for the
  daemon's user-scoped agent install, file-only (no `config add` flag;
  `config show` prints it); `host` (`host[:port]`, `[v6]:port`, or an ssh
  alias; a `user@` there is an error); `user` (unset = no `user@`, ssh
  config decides); `transport`, `mosh_server`, `identity`,
  `connect_timeout`. Retired keys are load errors naming the fix, never
  silently ignored: an entry's `managed` (delete it; `agent = true` for the
  bridge), a template's `user` and `ports` (`ports add`). **VM commands
  never write the file; only `config add`/`config rm` do**, and only
  structurally: `add` appends one whole table at EOF (always writing
  `backend`), `rm` cuts one table's line range (its keys, glued
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
  checks hook specs and warns on collisions. `config rm` owns
  this host's state: it **plans** the cut first (`PlanRemoveEntry`: every
  refusal happens before any daemon is touched), refuses while a daemon
  for the entry (or `NAME/*` for a hub) holds live forwards unless
  `--force`, stops those daemons, commits the edit (refused if the file
  changed since the plan), then clears saved forwards, reach cache, the
  hub cache and locks, carrying on past failures ("removed [machines.X];
  but: …"). An entry is never pinned in devvm's `known_hosts` (its host key
  is the user's `~/.ssh/known_hosts`), so there is no pin to clear. `config
  add` of a hub clears nothing: forwards already saved for its machines
  stay. A hub entry named exactly `os.Hostname()` is marked `(this host)` in
  `status`/`config show`: cosmetic only.
- **Connect surface**: `attach` (persistent dev tmux) and `shell` (raw login
  shell), both backend-dispatched via the `Interactive` interface and taking
  `--transport ssh|mosh`. There is no `ssh`/`mosh` command. `create` makes a
  local smol VM or (`--backend hetzner`) a provider server, flags-first, huh
  prompts on a TTY seeded from the template: a remote box or hub is `config
  add` (its huh form when `--host` is missing on a TTY), and devvm never
  shapes it. `--yes` skips prompts and resolves every unset field from flag >
  template > built-in (erroring on a required field with no default).
  Multi-op nouns are grouped: `ports {add,rm,list,up,down}`,
  `keys {add,rm,list,dedupe}`, `config {path,show,edit,check,add,rm}` and
  `providers token`. NAME is always the **first positional of the leaf**
  (`devvm ports add NAME PORT`) so completion and "first arg = machine" hold
  at any depth. Single-action verbs (create/start/stop/delete/attach/…)
  stay flat, but `--help` clusters them into Lifecycle/Connect/Configure/Config
  groups in an explicit lifecycle order via `cobra.EnableCommandSorting=false` +
  `AddGroup`/`GroupID` (see `rootCmd`). Removed by simplify.md, and not to
  come back as such: `auth` (a login is typed inside `attach`, below),
  `repos` (a template's `repos` are setup's last step; anything else is `git
  clone` in the box), `lockdown` (setup's harden step), `setup --adopt`,
  `setup --host`, `config add --managed`, `ports add --ephemeral`; and by
  the post-review cleanup, the hidden no-op `delete --force`.
- **Keys**: `internal/keys` is pure text logic (SHA256 fingerprint computed in-Go,
  matches `ssh-keygen`). It runs **host-side** — read `~/.ssh/authorized_keys`,
  process on the host, write back atomically (`umask 077` + temp + `mv`, content
  over stdin so nothing is shell-interpolated). No agent, so it works on any
  entry with zero footprint. `internal/keys` preserves options/comments verbatim.
- **Hubs** (`docs/proposals/hub.md`): a hub is a `[machines.HUB]` entry with
  `backend = "hub"`, reached like a remote box and never shaped like one (it
  needs only `devvm` and `smolvm`; registering one dials nothing, so its
  version is checked only by the proxied verbs that need the floor). Its VMs
  are addressed `HUB/NAME` and have **no entry of their own** on the laptop:
  the record is derived from the hub's entry, and the laptop's own state for
  one (its saved forwards, `<State>/ports/HUB@NAME`) is host state like any
  machine's, so the hub's registry is the only one. `HUB/NAME` is only ever
  the hub's (labelled) smol VM. `delete HUB/NAME` still proxies (with `--yes`
  when given, else the hub's own prompt runs over `-t`); the hub entry itself
  goes with `config rm HUB`. The merged
  listing hides a hub's `remote` row whose name is also an entry here (the
  direct path is right; a hub is not a jump host: that is `ProxyJump` on the
  `host` alias). Runtime files (daemon socket, log, lock)
  use `HUB@NAME`, because `run/HUB/NAME.sock` would name a directory; only
  `config.RuntimeName` and `DisplayName` convert between the forms. Commands
  **proxy**: the cli rebuilds
  the leaf's argv with the hub-side name and runs `env DEVVM_NO_SUBSCRIBE=1
  devvm …` over the hub's ssh. The env var keeps a proxied attach/shell
  from subscribing on the hub, since the laptop subscribes through its own
  daemon instead. A `-t` run's login-shell wrapper catches SIGINT
  (`TTYLoginShellArgv`), so a ^C, which the hub's pty delivers, comes back
  as 130 rather than ssh's 255 for an exit-signal ("cannot reach hub"). Forwards: the laptop's daemon for `HUB@NAME` runs one hidden
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
  `start` does (`bootForSession`; smolvm's `machine exec` refuses a stopped
  VM, it never boots one; no lock: smolvm 1.35.5 lists a booting VM as
  `unreachable` and answers a second `machine start` "already running",
  rc 0, without disturbing the boot, so two starts both wait for
  "running"), then open the bridge: on a hub, a
  `__session` refusal for a VM not running within the 5s makes the laptop
  proxy `start NAME` (no `-t`) and open once more (`hubBootForSession`);
  past the 5s the retry adopts it once the hub-side attach has started it.
  Listings fan
  out as `status --plain --local --watch` on each hub (`--local`, so hubs never
  chain) and are cached per hub. The hub floor is **v0.2.0**, the release
  that carries simplify.md: `hubMinVersion`,
  `session.HubForwardsMinVersion` and `HubBridgeMinVersion` are all it,
  pinned by tests, and named in the one-line refusals for a hub too old
  (no `setup`/`--inline-template`, no `__session`, a daemon without relays
  or `Bridge`). None is compared to a hub's version, and nothing else shims
  an older hub: laptop and hubs update together.
- **Browser bridge** (`docs/proposals/browser-bridge.md`): guest programs get
  `BROWSER` = the `devvm-open-url` shim, which hands the URL to the agent; the
  agent sends it up the daemon's one exec as an `open-url` event. The **daemon**
  classifies it (external / direct loopback / OAuth redirect), binds the forward
  a loopback URL needs (connection-owned, or `ttl`-owned and exact for a
  redirect), and delivers it to its **most recent subscriber**. That is a session
  held by `attach`/`shell`, which opens the URL on its own host (or prints
  it there if it has no opener); its answer goes back to the guest. The daemon
  never opens a browser itself. With no subscriber, the shim prints the URL in the
  guest. The shim exits 0 only when the URL was opened or printed on the
  host (1 when refused, unattached or unanswered), so gh says it failed. Opens are rate-limited (10/min) and bridge forwards are capped per
  machine. A remote box has events while its daemon knows a current agent
  (`eventCarrier.hasEvents`, which is also what a hub echoes as `Bridge` for
  it): its `events()` channel is fixed for the transport's life and fed only
  while the agent exec is up. `attach`/`shell` subscribe on remote boxes
  too, waiting at most 5s (`remoteBridgeWait`; a late bridge is adopted, so
  a dead box doesn't cost the daemon's come-up first); a daemon that
  reports no shim gets the session released with one line (naming `agent =
  true` on an entry). Remote `BROWSER` comes from tmux (`attach` sets it in
  the `dev` session, guarded on the shim being executable) and, on a
  provider server devvm made, profile.d: bare `shell` sends no remote command
  and sshd drops `SetEnv`, so on an entry it is the user's to export
  (`~/.local/bin/devvm-open-url`).
- **Logins** (no `auth` command, simplify.md decision 1): `gh auth login`
  and `codex login` typed inside `attach` open on the laptop through the
  bridge. codex's fixed `:1455` callback needs nothing of its own: the
  daemon's URL classifier (`session/bridge.go`) binds a `redirect_uri`
  callback as an exact `ttl`-owned forward. `DEVVM_NO_SUBSCRIBE` and the
  daemon's `kick` stay for proxied attach/shell. Lost with `auth`: picking
  `codex login --device-auth`, and chaining `gh auth setup-git`.
- **Agent install** (`internal/agentbin`): idempotent by sha256, arch-matched,
  `$HOME`-resolved (never hardcode `/home/dev`). A box devvm made (smol, a
  provider server) gets a root-owned `/usr/local/bin` install; an entry with
  `agent = true` gets a **user-scoped `~/.local/bin`** install from its
  daemon, never as root; any other entry only gets `agentbin.Probe` (same
  probes, no writes), so devvm never writes to an entry unasked. The posture
  is keyed on `Managed()` and the entry's `Agent`. Every record that reaches
  an install is one of those two (an unlabelled VM is never resolved), so
  there is no install gate or approve callback. `Install` returns the paths
  to use (agent and shim). On every box devvm made, `setup` installs agent +
  shim and writes `/etc/profile.d/devvm.sh` (`bootstrap.Browser`), so
  profile.d never names a missing file; `create`/`setup` **fail** if that
  install fails; an entry never gets profile.d. smol
  `shell`/`attach` also carry `BROWSER` in their exec env (attach copies it
  into the `dev` tmux session), dropped if the shim is missing.
- **Root adoption (fresh provider servers)**: `setup` on a provider server
  first waits for ssh, **bounded** (`setupReachWait`, 90s: a just-made VM or
  a cloud-init sshd bounce; only "unreachable" is retried). It tries
  `dev@address` (`config.DefaultUser`: every box devvm makes has the user
  `dev`, templates have none); on *permission denied* (a BatchMode stderr
  match: exit 255 alone is also "unreachable") it tries `root@address`, and
  a box that answers only as root gets `bootstrap.EnsureUser(dev)` (NOPASSWD
  sudo, root's `authorized_keys`), then setup continues as `dev`, so the
  keys/hook/repos never land in `/root`; it re-probes as `dev` first.
  **Nothing is written**: every later command logs in as `dev`. A changed
  host key or "Too many authentication failures" is fatal at once, and any
  exit but 255 means the host answered. A stopped smol VM is started (as
  `start` does) before setup's first exec. Only `setup` ever tries root, and
  only on a provider server. This is what establishes the invariant
  `rootWrap` assumes (login user = unprivileged user with sudo). `Harden`
  pins `AllowUsers` to the actual connection user, opens ssh over tcp and
  mosh's `60000:61000/udp`, and refuses to run as root (it would write
  `PermitRootLogin no` + `AllowUsers root` — a lockout). devvm's own apt
  calls wait for the dpkg lock (`-o DPkg::Lock::Timeout`) and its `apt-get
  update` retries (the lock timeout does not cover the lists lock); a
  hook's own apt is the hook's business.
- **Vocabulary — don't re-conflate these.** `create` allocates the resource (the
  VM/disk, or the server) with devvm's `devvm=true` label; `delete` destroys
  it. **`setup`** (was `bootstrap`) shapes a box devvm made and is the single
  place devvm touches its contents, in a fixed order: ensure user → prereqs
  (incl. tmux, gh, mosh-server) → agent + shim + profile.d → **keys →
  harden** (a provider server) → **share auth** → **`hook`** → repos. The
  security steps sit before user code and the network, so a failed hook or
  clone never leaves a billing server open. **Share auth**
  (`cli/shareauth.go`, share-auth.md) copies this host's `gh` login into
  the box when the template's `share_auth` lists `"github"`: a working
  guest login (`gh api user`) is left alone; else `gh auth token
  --hostname github.com` on the host (`hostGH`, a var for tests; an
  expiring login, `refreshable` in `gh auth status --json hosts`, is
  refused) goes over the install exec's **stdin only** (never argv, env,
  a file or a log) to `gh auth login --with-token` + `setup-git` +
  `git_protocol https`, as the login user. Every failure is one warning,
  never a failed setup (a login that went in with `setup-git` or
  `git_protocol` failing says so: the next setup leaves it alone). On a
  hub machine "this host" is the hub (its own login); only the list
  crosses, as the hidden `--share-auth`. The **`hook`** (template key) is
  the *user's* step inside it, often a dotfiles script itself named
  `bootstrap.sh`. So
  `create` = make the box exist; `setup` = shape it; `hook` = the user's
  part of shaping. A failed clone does not end setup: every repo is tried
  and setup exits **3** at the end, "setup finished, but: failed to clone
  …" (`setupIncomplete`, `exitSetupIncomplete`); `create` reports that as
  created-with-a-problem, and a hub passes the 3 back to the laptop as is.
  An `owner/repo` spec while the guest's `gh` has no working login is a
  skip, not a failure: the clone script exits **86** (`ghNoLoginExit`), the
  spec is named in one line with the fix (`share_auth = ["github"]` first,
  `gh auth login` in the box second), and
  setup exits 0; URL specs are still tried with plain `git`. The script runs
  as `bash -c` under `ExecOpts.Login`, since a login bash at SHLVL=1 with
  `set -e` turns any `exit N` into 1 through Ubuntu's stock
  `~/.bash_logout`. There is no promotion: an unlabelled smol VM or server
  becomes devvm's only by labelling it by hand (`smolvm machine update
  --name NAME --label devvm=true`, the provider's console). An entry is
  never shaped (`setup` on one refuses in one line). `config add`/`config
  rm` = the entry exists here or not, independent of the box.
- **Lifecycle states**: a machine exists or it doesn't. There is **no dormant
  state** and no `provision`/`deprovision` (config.md §2): a smol VM is
  running or stopped, a remote box is reachable. Rebuilding is `delete` then
  `create`.
- **Templates** (`internal/config/templates.go`, config.md §3): `[templates.*]`
  in devvm.toml, beside `[machines.*]`. Fields: `hook` (`url:`/`cmd:`/`none`,
  built-in **`none`**: nothing personal baked into the binary),
  `memory`/`disk` (create-time, smol), `repos`, `share_auth` (tools whose
  host login setup copies in: `"github"` only, any other a load error;
  `[]` in a named template turns default's off), `authorized_keys[_github]`
  and `harden`/`fail2ban` (provider servers), `server_type`/`location`/
  `image`/`ipv4` (create-time, provider). No `user` (always `dev`) and no
  `ports` (`ports add`): both are load errors. Fields that don't apply are
  ignored with one warning line. Unset fields fall back to
  `[templates.default]`, **one level**. Precedence for create/setup: flag >
  `--template T` > `[templates.default]` > built-in. `config show` prints each
  template resolved, annotated with where each value came from. Nothing records which template made a box: `setup
  NAME` re-runs whatever `default` says now. The template flags are
  `--hook`/`--memory`/`--disk` (visible on create) and a hidden `--repo`
  and `--share-auth`; keys, harden and fail2ban have no flag. A proxied
  `create`/`setup HUB/NAME --template T` resolves T on the laptop and
  sends what a hub's smol VM takes, hook, memory, disk, repos and
  share_auth, as those flags plus a hidden `--inline-template`
  (`templateArgv`), so the hub's file never matters;
  a create with no `--backend` also sends `--backend=smol`. Nothing about
  forwards crosses (the laptop clears its own `HUB@NAME` file after a
  create, above). A hub with no `setup`, `--inline-template` or
  `--share-auth` maps to one line naming `devvm update`
  (`runProxiedShaping`; `--share-auth` ships in the v0.2.0 floor itself).
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
  `realpath`). cp-in stages in `/var/tmp` (`/tmp` was tmpfs-shadowed under
  `machine cp` on smolvm 1.16.1; not on 1.35.5, and `/var/tmp` still works).
  cp-out streams the guest's tar over exec stdout on every backend: on smol
  a `machine exec -i --stream` (no tty, so byte-clean; `-i` for its empty
  stdin, below), because smolvm caps a plain exec's stdout at 11 MiB
  (1.35.5 fails past it with an explicit error) and does not cap a
  `--stream` or `-i` one.
  Extraction guards for cp-out live in `cp_tar.go` and are tested with
  hostile archives — keep them all-or-nothing. cp-in/cp-out run under
  `interruptible`: a signal kills the host command via the context, the
  spool is removed on the way out, and Execute re-raises the signal.
- **stdin on a smol one-shot exec**: `execArgv` passes `-i` exactly when the
  caller set `ExecOpts.Stdin` (decided before runHost defaults a nil one to
  the terminal); smolvm 1.35.5 delivers the input whole and EOF when the
  host closes it, and an empty reader is EOF at once. Without `-i` smolvm
  never reads the host's stdin. A caller that must keep the terminal out of
  an ssh exec sets an empty reader, which on smol is `-i` and an instant
  EOF. Guest files are written from stdin (`cat > tmp && mv`: the agentbin
  shim, `/etc/profile.d/devvm.sh`, `authorized_keys`), nothing in them
  shell-interpolated.

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
