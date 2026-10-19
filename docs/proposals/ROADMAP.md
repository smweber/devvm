# Roadmap

Once the single ordered plan for the design docs here; now a record, since
every planned step has shipped, plus the live lists: shared decisions, known
issues, lessons, live-test environments. The design docs (`hub.md`,
`browser-bridge.md`, `config.md`, `providers.md`) own the *what* and *why*
and carry "as shipped" notes where the code departed from them.

| Milestone | Design doc | Tags |
|-----------|------------|------|
| Hubs + browser bridge (steps 1–9) | `hub.md` v3.4, `browser-bridge.md` v2.6 | v0.1.13–v0.1.15 |
| Config redesign (steps 1–6) | `config.md` v3.1 | v0.1.16 |
| Provider backend (steps 0–4) | `providers.md` v1.5 | v0.1.16 (0–1), v0.1.17 (2–4) |
| Known issues (the real ones) | this file | v0.1.18 |
| ssh agent exec for remote boxes | `browser-bridge.md` §1, §5, §6 | v0.1.19 |
| Menu bar relaunch fix | Lessons below | v0.1.20 |

The long form (per-step live-test lists, "what shipped differently" notes)
is in git history before the commit "docs/proposals: ROADMAP trimmed to a
record and the live lists" (2026-10-19).

## Config redesign (done, v0.1.16)

Progress: all six steps landed 2026-10-17 (Opus implements and reviews,
ThinkPad live test, one `jj commit` each); v0.1.16 shipped after a
whole-redesign review with all three hosts hand-migrated.

| # | Step | Ships |
|---|------|-------|
| 1 | Config vs state | `config.Paths{Config, State}`; `run/`, `cache/`, `known_hosts`, `c-%C` move to `$XDG_STATE_HOME/devvm` by link+unlink; clients use an old-layout daemon where it is (no sibling on upgrade) |
| 2 | Forwards as host state | `<State>/ports/NAME` (`HUB@NAME`), per-name flock, one all-or-nothing seed; `ports add` saves (`--ephemeral` opts out); `UpdateHubMachine` gone |
| 3 | Discovery + managed marker | one `smolvm machine ls --json` in resolve (`created_at` is Unix seconds and survives stop/start on 1.16.1); `<State>/managed/NAME`; dormant gone; fifth `--plain` column |
| 4 | `setup` + templates | `bootstrap` → `setup` in a fixed order; `[templates.*]`; `setup --adopt NAME` (typed name); root adoption, 90s wait; `setup --host`; proxied create/setup send the template inline; `hubMinVersion = v0.1.16` |
| 5 | `devvm.toml` only | strict loader; `config.toml`, `defaults`, remote/hub `create` gone; `config {path,show,edit,check,add,rm}`; upgrade seeds + one-line pointer |
| 6 | Hand migration | ThinkPad, this guest (old tree in `~/devvm-old-config/`), `cloud` (fake smolvm emits `created_at`) |

## Provider backend (done, v0.1.16–v0.1.17)

| # | Step | Tag | Ships |
|---|------|-----|-------|
| 1 | Interface + Hetzner client | v0.1.16 | `internal/provider` (`Provider`, `KeyEnsurer`, sentinels), plain-REST `hetzner`, `fake`; fixtures from the test project |
| 0 | Status truthfulness | v0.1.16 | `internal/reach` cache, `ssh -O check` → `ssh -G` → 2s TCP probe, `__refresh`, sixth `--plain` field, `status -v` last contact |
| 2 | Token, loader, discovery | v0.1.17 | `[providers.hetzner]`, `providers token`, discovery last in resolve, `cache/provider-hetzner.list`, backend `hetzner`; daemon/`__session`/completion/`config rm` cache-only |
| 3 | Lifecycle | v0.1.17 | `create --backend hetzner` (refusals before any API call, then `shape`), `delete`, `start`/`stop` |
| 4 | Live test | v0.1.17 | full run in the test project from isolated dirs; left empty but one non-billing key `devvm-42dec2d3bc2d` |

Facts providers.md doesn't carry: the API puts a top-level `location` on
servers, no `datacenter`; `Create` may return a server *and* an error, and
the server bills, so callers handle both; `Power(off)` is a hard poweroff
(graceful can hang; keep); the shared ControlMaster has
`ServerAliveInterval=30/CountMax=3` so a box that died during sleep can't
read `reachable (master)`; the Swift app never calls `__refresh` (its
one-shot `status` on menu open probes).

Follow-ups (not scheduled): v6-only live test (no v6 route here); mosh with
a bare v6 host; a throwaway key for the next fixture recording (today's
hold the user's real pubkey and a released IP: not secret, but
persistent); `cp-in` ~4× slower than `cp-out`; a default `server_type` in
`[providers.hetzner]` (user, 2026-10-17: provider-specific, so it reads
oddly in `[templates.default]`; today a template field or `--server-type`,
built-in `cx23`); a live test from a second host.

## After the config redesign (done)

1. Known issues, the real ones (v0.1.18): a killed daemon orphaning its
   ControlMaster, SIGINT leaking the cp spool, the cold-boot bridge retry.
2. The ssh agent exec for remote boxes (v0.1.19; browser-bridge.md §1, §5,
   §6 "As shipped"): one `devvm-agent serve` on the daemon's own master.
3. Provider backend (above).
4. Mac tests: still owed (Live-test environments).

## Hubs + browser bridge (done, v0.1.13–v0.1.15)

| # | Step | Tag | Ships |
|---|------|-----|-------|
| 1 | Hub backend + `HUB/NAME` | v0.1.13 | `backend = "hub"`, runtime identifier `HUB@NAME`, `listMachines`, shaping verbs refuse the hub, min-version check |
| 2 | Proxy | v0.1.13 | leaf argv with the hub-side name, `env DEVVM_NO_SUBSCRIBE=1 devvm …` over the hub's ssh, `-t` iff stdin and stdout are TTYs, laptop-side inputs resolved first |
| 3 | Merged listing + watch | v0.1.13 | `status --plain --local`, per-hub fan-out with cache and deadline, one hub pipe per hub, `--exit-on-stdin-eof` |
| 4 | cp over tar stdio | v0.1.13 | `cp-in --from-tar -` / `cp-out --to-tar -`, structural completeness check |
| 5 | Ownership + sessions | v0.1.14 | owner kinds `conf`/`connection`/`ttl`, sticky exact-or-bump, acked `subscribe`, `ports down` op, dual-stack binds |
| 6 | Hub forwards | v0.1.14 | relay sessions, `__session NAME` as a pure pipe, `hubTransport`, relay heartbeat |
| 7 | Daemon bridge + agent reply | v0.1.15 | URL classify, connection-/ttl-owned bridge forwards, 10/min, 20-forward cap, `open-url` reply, one `agentbin.Install` |
| 8 | Thin `auth`, smol and hub | v0.1.15 | `auth` rides the daemon session, echo `bridge`/`shim`, `auth HUB/NAME` proxies the leaf |
| 9 | `BROWSER` plumbing (smol) | v0.1.15 | agent + shim at setup, guarded profile.d, `tmux set-environment`, guarded exec `Env` |

Pinned by tests: `hubMinVersion` (v0.1.13, v0.1.16 since config step 4),
`session.HubForwardsMinVersion = v0.1.14`, `session.HubBridgeMinVersion =
v0.1.15`.

## Shared decisions

1. **Forward ownership** (bridge §4): owner set with kinds `conf`,
   `connection`, `ttl`; torn down when empty; owners decide lifetime and
   nothing else. `up:N` counts `conf` only and `up:0` is never emitted.
2. **Bind policy is a request property kept for the forward's life** (bridge
   §4): exact-or-bump, honoured by `restore()`, sticky once any owner asked
   for exact; every forward dual-stack with `::1` best-effort. A
   `redirect_uri` callback is exact on the host that shows the browser.
3. **Subscribe-to-open** (bridge §1, §3): the daemon never opens a browser
   and never rewrites a URL; a session holds the daemon; relay sessions bind
   nothing and die with the transport; most recent subscriber of that daemon
   wins; the hub proxy runs commands with `DEVVM_NO_SUBSCRIBE=1`. Clients
   always name guest ports.
4. **One exec per smol VM, always.** Nothing on any host spawns a second
   `devvm-agent serve` into a VM. A remote box gets one too, on its
   daemon's own ssh master; `auth` never spawns its own anywhere.
5. **`--plain` changes ship with their Swift change** in the same commit.
6. ~~Hub-machine state lives in the hub's conf.~~ **Superseded by config.md
   §5:** a client's saved forwards for any machine are host state,
   `<State>/ports/NAME`, and `<State>/ports/HUB@NAME` for a hub machine,
   whose record is derived from the hub's entry.
7. **VM commands never write config; `config` commands never touch a box**
   (config §4). devvm modifies only boxes it manages: made by `create` or
   handed over with `setup --adopt` (config §2–3).
8. **Two axes, not one enum** (config §1): `backend` says who owns the
   registry and lifecycle, `managed` whether devvm owns the OS; code decides
   on the predicates, never the backend string.
9. **Smol VMs are discovered, never configured** (config §2): an entry
   shadows a smol VM of the same name, and the managed marker
   (`created_at`) settles whether devvm made it.
10. **No dormant state** (config §2): a machine exists or it doesn't;
    rebuilding is `delete` then `create`.

## Known issues (pre-existing, found by live runs, not fixed)

None open; the cold-boot bridge retry was fixed 2026-10-18 (attach/shell
start a stopped VM first, see Lessons).

Cosmetic or accepted:

- A bare `devvm exec NAME …` with no daemon up (no forwards, no
  subscriber) still sees a stale guest clock after a host suspend: the
  daemon syncs the clock only while it holds the agent exec.
- An external `devvm exec` storm (~7,800 calls in two hours from a client's
  hook) OOM-killed the fake-smolvm cloud hub. Not devvm's to fix: every hub
  exec is a login shell by design, and a rate limit would only make the
  external client's calls fail (decided 2026-10-17).
- The remote huh form can take >10s to first paint through a cold
  ControlMaster.
- The ssh transport notices a dead ControlMaster only on its 30s
  `checkInterval`, so a forward reads `up` for up to 30s after `ssh -O exit`.
- A hub `stop`→`start` waits out the laptop's reconnect backoff (≤30s).
- A locally stamped build with a suffix (`…-dirty-step6`) sorts as a
  prerelease and refuses a hub as older than `hubMinVersion`.
- The redirect TTL's 10-minute expiry was never live-tested. The host
  browser is opened by the subscribing `attach`/`shell` process, so its
  PATH decides the opener.
- Killing a laptop-side `cp-in HUB/NAME` after the archive is fully written
  does not abort it (no pty, no SIGINT propagation; "nothing created" holds
  only for an early kill). The laptop then says "interrupted after upload;
  the hub may still complete the copy".
- A process backgrounded in a detached `exec` did *not* die with that exec
  on the ThinkPad, contrary to the Environments note of the long form.
- Two hosts' daemons for one remote box (a laptop's own, and a hub's for
  the same box as `HUB/NAME`) each run an agent exec as the same login
  user, and both listen on `~/.devvm/agent.sock`: the one that bound last
  (a respawn rebinds) gets every shim's URL, delivered to *its* daemon's
  subscriber. Seen live 2026-10-18; per-host sockets would need the shim
  to choose, so it is left as "the most recent wins".
- A relay opened while a hub's remote machine had no current agent there
  keeps `NoEvents` (no bridge) until the laptop's daemon cycles, even after
  `auth` on the hub installs one: the echo is read once, at the open.
- On an adopt host whose key is not yet in `~/.ssh/known_hosts` (adopt
  entries are not pinned), the first `attach`/`shell` gets the "bridge
  unavailable (… exit status 255)" line: the daemon's BatchMode master
  refuses the unknown key before the session's own ssh asks about it.
  The next attach is bridged.
- On a managed remote box whose agent install fails (sudo refused before
  `setup`), each of the daemon's retries copies the agent again (~3 MiB),
  backing off to one try per five minutes.

## Lessons (don't relearn)

- **smolvm 1.16.1's `machine exec` never boots a stopped VM** (2026-10-18,
  ThinkPad): it fails at once (`machine 'raw' is not running`), so
  attach/shell on a stopped VM used to fail (attach as a misleading "tmux
  not found"). Execs overlapping a boot are only slow: 50 parallel probes
  during `devvm start` all succeeded once the VM ran (~1s, the early ones
  waiting up to 1s), and `start` + `exec` + the daemon's serve exec
  overlapped twice with no wedge. attach/shell now start the VM first.
- **A smol VM whose start is in flight lists as `stopped`** (1.16.1 never
  says `starting`), and a second `smolvm machine start` kills the first
  one's boot ("Killing orphaned _boot-vm process"; the first start fails
  with SIGTERM, the second boots it). Seen with `devvm start raw &` and
  `devvm shell raw` 0.2s later. This host's starts of one VM are serialized
  (`lockStart`, `run/.start-NAME.lock`); `machine start` on a running VM is
  a harmless "already running".
- **macOS kqueue coalesces a relink into nothing** (2026-10-17): a symlink
  renamed over `devvm.toml` under load merges into one dir event, the old
  target's fd stays watched, and no Create/Remove fires. `status --watch`
  re-resolves a symlinked `devvm.toml` on a 2s timer for that reason. A
  test that "passed once on macOS" after a kqueue change proved nothing.
- **A scratch binary that imports `internal/session` must hand `__daemon`
  to the real devvm.** `spawnDaemon` re-execs `os.Executable()`; under a
  scratch tool that was a fork bomb that took the 2 GB guest down twice.
  `spawnDaemon` now refuses an empty name.
- **A proxied command's stdin must be an `*os.File`, never an `io.Pipe`.**
  os/exec's `awaitGoroutines` blocks forever on the stdin copy after
  `WaitDelay`. Follow `internal/cli/hublist.go` (`os.Pipe`, write end held).
- **Only the login shell has `~/.local/bin` on `PATH`** on both hubs;
  `backend.LoginShellArgv` is what makes every proxied command work. A
  `~/.bash_profile` shadows `~/.profile`.
- **Tar completeness must be structural** (the bytes the reader consumed
  include the two terminator blocks); a "trailing zeros" sniff accepted a
  cut archive.
- **`-t` only when stdin *and* stdout are terminals**: a remote pty turns
  `\n` into `\r\n` and merges stderr. `-o RequestTTY=no` on every BatchMode
  run.
- **The memory-capped guest** (`GOMEMLIMIT`) surfaced real leaks: test
  daemons that never closed transports hit the fd limit, and an in-process
  relay's `*os.File` was finalized while in use. Keep those fixes; in
  late-dial tests wait on signals, never sleep.
- **Live testing**: refresh a hub's binary by `scp` to a temp path then `mv`
  (ETXTBSY otherwise). `script -qec "devvm …" /dev/null <<< y` drives
  `[y/N]` prompts; the huh form is unreadable under `script`. The
  **ThinkPad is the default live hub**; `cloud` (fake smolvm) only where
  `calls.log` must show the exact smolvm calls or a managed remote ssh
  target is needed.
- **Reusable test scaffolding**: `fakeSSH`/`fakeHub`/`fakeHubWith` in
  `internal/cli` run the real remote string through `sh` and assert exact
  ssh argv; the cp round trip re-execs the test binary as the hub's `devvm`
  (`TestMain` in `cp_hub_test.go`); `pinTTYs` fakes the TTY decision; the
  cli test binary's `TestMain` refuses `__daemon`.
- **Unix sockets and inodes**: tmpfs hands out inodes monotonically, ext4
  and APFS reuse them; `ownsSocket` needs the dial, not just the inode.
- **This dev guest is itself a devvm smol box** (2026-10-18): it has
  `/usr/local/bin/devvm-open-url`, so a test expecting the managed shim to
  be missing fails here. Test the absence with a temp `$HOME` (the adopt
  path) and assert only the managed path's text.
- **`ControlMaster=no` with a missing ControlPath is a fresh connection,
  not an error**: ssh connects on its own. Anything run on the daemon's
  master (`OnMaster`) checks the master is alive before a respawn and
  passes `BatchMode=yes` so the fallback can never prompt.
- **Quit a Mac app with a signal and wait for it to exit** (2026-10-19,
  v0.1.18's "Update devvm" never relaunched the app). An `osascript` quit
  from an unsigned CLI needs automation (TCC) consent: refused, or hung on
  a prompt; and `open -a` on an app still exiting only activates the dying
  instance. Now SIGTERM, wait until gone, then `open -a` and wait for it
  (v0.1.20).

## Live-test environments

- **This devvm guest** is the "laptop": no browser, so a fake `xdg-open` on
  `PATH` that logs its argument stands in for one. ~6 GB RAM, no swap: run
  go with `GOMAXPROCS=2 GOMEMLIMIT=1GiB GOFLAGS=-p=1`, one command at a time.
- **ThinkPad**, hub entry `tp` (`devvmtest@192.168.1.196`, Linux Mint, real
  KVM, smolvm 1.16.1), **intermittently offline**. Dedicated user
  `devvmtest`; nothing outside that home is touched. The only place
  smolvm-specific behaviour (exec concurrency, boot, `machine cp` staging,
  the 11 MiB cap) can be seen. VMs: `web` (managed; ubuntu:24.04, 1 GiB,
  10 GB, with `python3`, `tmux`, `gh` and a persistent `dev` tmux session)
  and `raw` (alpine, unmanaged: the `setup --adopt` and unmanaged-path test
  subject). Guest fixture: `nohup devvm exec web -- python3 -m http.server
  3000 --bind 127.0.0.1 >/dev/null 2>&1 &` on the hub.
- **`cloud`** (Hetzner CX23, `dev@2.29.47.196`; a managed `remote` entry
  `cloud` and a hub entry `h` here): no `/dev/kvm`, so its `smolvm` is
  `contrib/fakesmol/smolvm` (the "guest" is the box; every call appended to
  `~/.local/share/fakesmol/calls.log`; guest port P bumps to P+1 on the
  hub). It also holds the user's machine `scottdev3`: never touch it.
- **Hetzner test project**, for throwaway servers only: `create --backend
  hetzner`, test, `delete` after, and leave the project empty (verify it).
  The token is at `<State>/providers/hetzner.token` (0600; `HCLOUD_TOKEN`
  wins): never print it.
- **Mac tests the user still owes** (at the laptop): cp-out path completion
  into a directory (written blind: cycle to a directory, then "/" goes in
  with no doubled slash and Right arrow accepts so the next Tab does; a
  unique directory is still entered by the next Tab); the v0.1.14 menu bar
  changes (drop shelf, Tab completion, save panel); the fifth `--plain`
  column's Swift change; a Mac as a smol hub (`smolvm machine create` and
  `open` from an sshd session, a daemon spawned there surviving logout);
  the v0.1.20 menu bar relaunch after "Update devvm".
