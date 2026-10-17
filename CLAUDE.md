# CLAUDE.md

Guidance for Claude Code working in this repo. Focuses on the non-obvious
constraints; read the package docs for the rest.

## What this is

`devvm` manages persistent dev boxes across three backends: local smolvm microVMs
(`smol`), a remote host devvm shapes (`remote-managed`), and an existing host it
adopts hands-off (`remote-unmanaged`), plus `hub`: another host running devvm,
whose machines (usually smol VMs) this one reaches as `HUB/NAME` (see Hubs below). The two
`remote-*` backends share one ssh transport (`ssh.go`) and differ only in
posture: `Managed()` vs adopt. Old confs
with `backend = "ssh"` (+ optional `unmanaged`) migrate on load. It is a **host
CLI** (`cmd/devvm`) plus a **guest agent** (`cmd/devvm-agent`) the host installs
into a box only when needed (see the agent note below). It began as a ~2,100-line
bash script; comments often explain *why* a behavior exists because it was
hard-won there.

**Backend axes.** `Managed()` = smol || remote-managed (devvm owns the OS:
installs prereqs, may harden, pins known_hosts). `IsRemote()` = both `remote-*`
(reached over ssh). The old per-machine `unmanaged` bool is gone — the backend
value carries it. A `transport` field (`ssh`|`mosh`) steers only the interactive
connection on remote backends; forwards are always native `ssh -L`.

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
                    hub proxying in proxy.go + hub*.go; attach/shell's bridge in bridge.go)
internal/config/    TOML machine registry (~/.config/devvm/machines/<name>.toml) + Paths
                    (config vs state dirs) + MigrateState
internal/backend/   Backend interface + smol.go + ssh.go (both remote-* backends)
                    + hub.go (HUB/NAME: proxy argv, `__session` argv; never spawns)
internal/session/   per-machine forward daemon (owns the exec) + client + transports
                    (transport_{smol,ssh,hub}.go); daemon_session.go = sessions and
                    relays; bridge.go = URL classify/bind/deliver (browser bridge)
internal/agentrpc/  yamux stream protocol (forward/rpc/event), shared host+guest
internal/agentbin/  embedded guest agent binaries (go:embed) + on-demand install
internal/keys/      authorized_keys logic (was awk); pure/text host-side, unit-tested
internal/auth/      login orchestration, URL bridge, callback-as-forward
internal/bootstrap/ prereqs (install on managed / check on adopt) + bootstrap-hook + hardening
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
  for one (`requireRunningForForwards`; `start` alone waits for "running").
  Shutdown closes the transport **before** unlinking the socket (`WaitGone`
  relies on that) and unlinks only its own (`ownsSocket`: inode check *plus* a
  dial — ext4/APFS reuse inode numbers, tmpfs doesn't, which hid the bug
  locally). Binds run outside `d.mu`: an `ssh -O forward` against a wedged
  master must not freeze `list`/`ping`.
- **Config vs state** (`config.Paths{Config, State}`): the config dir
  (`$XDG_CONFIG_HOME/devvm`) holds only the user's files (`machines/`,
  `config.toml`); this host's own state lives in `$XDG_STATE_HOME/devvm`
  (`~/.local/state/devvm`): `run/` (daemon sockets, logs, locks, `changed`),
  `cache/` (hub listings), `ports/` (saved forwards, below), the pinned
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
  reconnecting, and `delete` reaps both layouts.
- **Saved forwards are host state** (config.md §5): `<State>/ports/NAME`, one
  mapping per line in `ports add` syntax (`GUEST` or `HOST:GUEST`; `#` and
  blank lines skipped), `HUB@NAME` for a hub machine, 0600 in a 0700 dir,
  written atomically; an emptied list removes the file; every
  read-modify-write (and `delete`'s removal) holds `config.LockPorts`
  (`ports/.NAME.lock`, never unlinked). Never in a conf: the
  CLI alone reads it (`start`/`ports up` push one Add per line, `ports list`'s
  `configured:` block, `--plain`'s `down`), and **the daemon never reads it**
  (it keeps forwards in memory; clients push adds). `ports add` saves
  (`--ephemeral` doesn't), `ports rm` drops a line, and `delete` clears it
  through `forgetHostState`, the one helper for "everything this host
  remembers about NAME". `config.MigratePorts` seeds the dir **once per
  host** (only while `ports/` doesn't exist) from old confs' `ports` and hub
  confs' `[machines.NAME]` tables (decoded leniently, not via `Load`), never
  writing a conf; per-dir rather than per-file so a list dropped here can't
  be re-seeded. It is all or nothing: any failure leaves no `ports/`, every
  ports entry point returns the error, and the next call retries (nothing
  else creates the dir). Those conf fields are
  otherwise dead (decoded so old confs parse; `validateHub` still refuses
  `ports` on a hub). The owner kind stays `conf` (a compatibility surface).
- **Change marker**: every state-changing command and daemon transition calls
  `config.TouchChanged` (a *write* to `run/changed`, because kqueue ignores
  utimes); `status --plain --watch` (fsnotify on config's `machines/` + state's
  `run/`) re-emits on it, so a write elsewhere in state (a `ports/` file) is
  seen only through the marker. Rule: a new state-changing command gets `defer
  config.TouchChanged(a.Paths)`. `Machine.Save` is atomic (temp + rename) so
  the watcher never reads a half-written conf.
- **`--plain` is a compatibility surface** parsed by the Swift app:
  `name\tbackend\tstate\tforwards` with state ∈ {running, stopped, dormant,
  reachable, broken conf, ?} and forwards ∈ {up:N, reconnecting:N, down, -}; so
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
  ssh/scp/mosh call gets `ConnectTimeout`, default 10) and
  `DEVVM_COMPLETE_TIMEOUT` (duration; guest path completion, default 2s; the
  app sets 10s for cold ControlMasters).
- **Config** is hand-editable TOML; keep it that way (don't hide state in an
  opaque DB). No legacy sourced-bash reader. BurntSushi's `omitempty` doesn't drop
  zero ints, so `Save` strips `key = 0` lines and defaults are backend-scoped.
- **Connect surface**: `attach` (persistent dev tmux) and `shell` (raw login
  shell), both backend-dispatched via the `Interactive` interface and taking
  `--transport ssh|mosh`. There is no `ssh`/`mosh` command. `create` is
  backend-agnostic (flags-first, huh prompts on a TTY); it adopts remote hosts by
  test-connecting before saving the conf. `--yes` skips prompts and resolves every
  unset field from flag > `config.toml` > built-in (erroring on a required field
  with no default). Multi-op nouns are grouped:
  `repos {add,rm,list,clone}`, `ports {add,rm,list,up,down}`,
  `keys {add,rm,list,dedupe}`, and `defaults {list,set,unset,path}` (global, so its
  leaves take a KEY not a NAME). NAME is always the **first positional of the leaf**
  (`devvm repos add NAME REPO`) so completion and "first arg = machine" hold at any
  depth. Single-action verbs (create/provision/start/stop/deprovision/delete/attach/…)
  stay flat, but `--help` clusters them into Lifecycle/Connect/Configure groups in an
  explicit lifecycle order via `cobra.EnableCommandSorting=false` + `AddGroup`/`GroupID`
  (see `rootCmd`). `repos add` records
  in the conf and clones immediately (`--no-clone` to skip); `owner/repo` shorthand
  clones via gh, any URL via plain git, so non-GitHub hosts work.
- **Keys**: `internal/keys` is pure text logic (SHA256 fingerprint computed in-Go,
  matches `ssh-keygen`). It runs **host-side** — read `~/.ssh/authorized_keys`,
  process on the host, write back atomically (`umask 077` + temp + `mv`, content
  over stdin so nothing is shell-interpolated). No agent, so it works on adopt
  hosts with zero footprint. `internal/keys` preserves options/comments verbatim.
- **Hubs** (`docs/proposals/hub.md`): a hub is a conf with `backend = "hub"`,
  reached like a remote box and never shaped like one (it needs only `devvm` and
  `smolvm`). Its VMs are addressed `HUB/NAME` and have **no conf of their own**
  on the laptop: the record is derived from the hub conf, and the laptop's own
  state for one (its saved forwards, `<State>/ports/HUB@NAME`) is host state
  like any machine's; the hub conf is never written after `create`, so the
  hub's registry is the only one. Runtime files (daemon socket, log, lock)
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
  then adopts a late one, retrying transient failures in the background. It
  never retries into a VM that was not running (`__session` refuses one),
  locally or on a hub: the daemon's exec startups would overlap the attach's
  own while it boots the VM, so that session gets a one-line notice and no
  bridge. Listings fan
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
  devvm never writes to an adopted box unasked. `Install` returns the paths to use
  (agent and shim). On smol, `bootstrap` installs agent + shim and writes
  `/etc/profile.d/devvm.sh` (`bootstrap.Browser`), so profile.d never names a
  missing file; smol `create`/`bootstrap` **fail** if that install fails. smol
  `shell`/`attach` also carry `BROWSER` in their exec env (attach copies it
  into the `dev` tmux session), dropped if the shim is missing.
- **Root adoption (fresh cloud VMs)**: `bootstrap` on a remote-managed box whose
  ssh login is root first runs `bootstrap.EnsureDevUser` — creates `dev`
  (NOPASSWD sudo, copies root's `authorized_keys`) and rewrites the conf's
  `ssh_host` to `dev@…` — so the hook/keys/repos never land in `/root`. This is
  what establishes the invariant `rootWrap` assumes (login user = unprivileged
  dev with sudo). `Harden` pins `AllowUsers` to the actual connection user and
  refuses to run as root (it would write `PermitRootLogin no` + `AllowUsers
  root` — a lockout).
- **Vocabulary — don't re-conflate these.** `provision` means **allocate a
  resource** (the VM/disk); `deprovision` frees it while keeping the registry entry.
  The **`bootstrap-hook`** (conf `bootstrap_hook`, was `provision`) is the *user
  setup step* the `bootstrap` phase runs (prereqs + hardening + hook). So
  `provision` = make the box exist; `bootstrap-hook` = shape it.
- **Lifecycle states** are **derived, not stored**: a machine is *dormant* when its
  conf exists but the backend resource doesn't (`config.Exists ∧ ¬backend.Exists()`).
  `provision` allocates a dormant box (+ bootstrap); `deprovision` returns a live box
  to dormant (destroys disk, keeps conf); `delete` on a dormant box just deregisters.
  `resolveLive`/`requireProvisioned` (`resolve.go`) guard the commands that need a
  live box so they fail with a clear next step. Remote backends report
  `Exists()==true` always, so provision/deprovision no-op there **until the hetzner
  backend** adds real API-backed create/destroy.
- **Bootstrap-hook**: may be `url:`, `cmd:`, or `none`; the built-in default is
  **`none`** (nothing personal baked into the binary). Each box's hook is resolved
  once at create time (flag > `config.toml` `bootstrap_hook` > built-in) and written
  concretely into its conf, so editing global defaults never changes what an existing
  box re-runs on `bootstrap`. A `url:` hook (e.g. a dotfiles `bootstrap.sh`) is a
  per-user choice set via `defaults set bootstrap-hook …`.
- **Global defaults** (`internal/config/defaults.go`): create-time defaults in
  `$XDG_CONFIG_HOME/devvm/config.toml` (`bootstrap_hook`, `memory`, `transport`),
  sibling to `machines/`. Consulted **only** at create (to seed prompts and the
  non-interactive path), never at runtime. Managed via `devvm defaults` (its CLI keys
  use hyphens, e.g. `bootstrap-hook`; the TOML uses snake_case `bootstrap_hook`).

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
  cp`). Extraction guards for cp-out live in `cp_tar.go` and are tested with
  hostile archives — keep them all-or-nothing.

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
