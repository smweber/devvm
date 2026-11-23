# Roadmap

Once the single ordered plan for the design docs here; now a record, since
every planned step has shipped, plus the live lists: shared decisions, known
issues, lessons, live-test environments. The design docs (`hub.md`,
`browser-bridge.md`, `config.md`, `providers.md`, `simplify.md`,
`share-auth.md`) own the *what* and *why* and carry "as shipped" notes
where the code departed from them.

| Milestone | Design doc | Tags |
|-----------|------------|------|
| Hubs + browser bridge (steps 1–9) | `hub.md` v3.4, `browser-bridge.md` v2.6 | v0.1.13–v0.1.15 |
| Config redesign (steps 1–6) | `config.md` v3.1 | v0.1.16 |
| Provider backend (steps 0–4) | `providers.md` v1.5 | v0.1.16 (0–1), v0.1.17 (2–4) |
| Known issues (the real ones) | this file | v0.1.18 |
| ssh agent exec for remote boxes | `browser-bridge.md` §1, §5, §6 | v0.1.19 |
| Menu bar relaunch fix | Lessons below | v0.1.20 |
| Simplify (phases 0–5, steps 1–18) | `simplify.md` v1.3 (implemented, "As shipped") | v0.2.0 |
| Post-review cleanup C1–C13 | `simplify.md` "As shipped" | done 2026-11-22 (ThinkPad: 250 MiB stream cp-out, concurrent starts, hub; both sides v0.2.0) |
| Share auth (steps 0–5) | `share-auth.md` (implemented, "As shipped") | v0.2.0 (live 2026-11-23: ThinkPad smol VM + Hetzner) |
| smolvm floor: `create` and every start refuse smolvm below 1.35.5 | Known issues and Lessons below | v0.2.1 |

The long form (per-step live-test lists, "what shipped differently" notes)
is in git history before the commit "docs/proposals: ROADMAP trimmed to a
record and the live lists" (2026-10-19).

## Simplify (done, v0.2.0)

Design: `simplify.md` v1.3 (now implemented; its "As shipped" section lists
what departed from the text), then `share-auth.md`. One `jj commit` per
numbered step, tests green at each; a live test per phase, because laptop
and hubs must update together from phase 3 on. The release that carries it
is v0.2.0, which is also the hub floor (`hubMinVersion`,
`session.HubForwardsMinVersion`, `HubBridgeMinVersion`).

| # | Phase | Steps | Live test | State |
|---|-------|-------|-----------|-------|
| 0 | ThinkPad live checks (`simplify.md`) | – | ThinkPad, smolvm upgrade | done 2026-11-20 (smolvm 1.35.5; answers in simplify.md Unknowns) |
| 1 | `setup` never skips keys and hardening; mosh UDP in harden | 1–3 | throwaway Hetzner server: create with a repo that cannot clone, harden applied, mosh connects | done 2026-11-20 (live on Hetzner; mosh checked from the ThinkPad) |
| 2 | Remove `auth`, `repos`, `lockdown`; `agent = true` | 4–8 | an entry with `agent = true` (not `scottdev3`: the user adds that line) | done 2026-11-20 (live: ThinkPad host as an `agent = true` entry) |
| 3 | Ports out of templates/create/setup; no `--ephemeral`; restore on attach | 9–11 | ThinkPad hub + local | done 2026-11-21 (ThinkPad on the same build) |
| 4 | No "managed": labels, no adopt, no managed entries; Swift | 12–15 | ThinkPad (labels), Hetzner (unlabelled count) | done 2026-11-21 (Swift built by CI's macOS job at v0.2.0) |
| 5 | Delete upgrade paths, shrink hub flags, docs | 16–18 | ThinkPad hub at the new minimum | done 2026-11-21 (ThinkPad hub and laptop both v0.2.0) |
| 6 | `share-auth.md` | its 0–5 | ThinkPad smol VM + Hetzner | done 2026-11-23 (live: ThinkPad smol VM via a piped GH_TOKEN, hub path, Hetzner with harden) |

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
v0.1.15`; all three v0.2.0 since simplify step 16.

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
   `devvm-agent serve` into a VM. A box reached over ssh gets one too, on
   its daemon's own ssh master. (smolvm 1.35.5 showed no parallel-exec
   wall; the rule stays for the one guest socket and the clock sync.)
5. **`--plain` changes ship with their Swift change** in the same commit.
6. ~~Hub-machine state lives in the hub's conf.~~ **Superseded by config.md
   §5:** a client's saved forwards for any machine are host state,
   `<State>/ports/NAME`, and `<State>/ports/HUB@NAME` for a hub machine,
   whose record is derived from the hub's entry.
7. **VM commands never write config; `config` commands never touch a box**
   (config §4). devvm shapes only what it creates (simplify.md): a smol VM
   or provider server made by `create`, carrying the `devvm=true` label.
   An entry is never shaped; the one thing devvm may write there beyond
   `~/.ssh` is its agent, and only under `agent = true`.
8. **Two axes, not one enum** (config §1): `backend` says who owns the
   registry and lifecycle, `Managed()` whether devvm made the box (true for
   every discovered record, false for entries and hubs: not a setting);
   code decides on the predicates, never the backend string.
9. **Smol VMs and provider servers are discovered, never configured**
   (config §2, simplify.md steps 13–15): ownership is the `devvm=true`
   label on the machine, an unlabelled one is invisible, and an entry
   shadows a discovered machine of the same name.
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
  an agent appears there: the echo is read once, at the open.
- On an entry whose key is not yet in `~/.ssh/known_hosts` (entries are
  never pinned by devvm), the first `attach`/`shell` gets the "bridge
  unavailable (… exit status 255)" line: the daemon's BatchMode master
  refuses the unknown key before the session's own ssh asks about it.
  The next attach is bridged.
- On a provider server whose root agent install fails (sudo refused before
  `setup` ran), each of the daemon's retries copies the agent again
  (~3 MiB), backing off to one try per five minutes.
- A daemon started before `agent = true` was added to an entry keeps the
  record it captured until it restarts (`ports down`, end the attach): the
  key counts from the next start.
- A smolvm older than 1.35.5 is refused at create and at every start
  since v0.2.1 (`backend.SmolReady`: one `smolvm --version`, one line
  naming the stop-first upgrade); a `--version` that fails or is
  unparseable is no block, and listing, exec and cp never check.
- `<State>/managed/` (the markers before phase 4) is left on disk, unread.
- `~/.devvm/agent.sock` outlives the agent on a remote box (the exec dies,
  the socket file stays until the next agent rebinds it).
- The Swift changes of phase 4 (and step 16's `dormant` removal) compile
  (CI's macOS job, v0.2.0) but were never run on a Mac: see "Mac tests the
  user still owes".

## Lessons (don't relearn)

- **smolvm's `machine exec` never boots a stopped VM** (1.16.1, 2026-10-18;
  unchanged on 1.35.5, 2026-11-20): it fails at once (`machine 'raw' is not
  running`), so attach/shell on a stopped VM used to fail (attach as a
  misleading "tmux not found"). Execs overlapping a boot are only slow.
  attach/shell now start the VM first.
- **smolvm 1.35.5 facts** (simplify.md, ThinkPad answers, 2026-11-20): a
  booting VM lists `unreachable` for ~0.3s, then `running`; a VM made but
  never started lists `created`; a second `machine start` during a boot
  answers "already running" and no longer kills the first (on 1.16.1 a
  booting VM listed `stopped` and the second start killed it, which is why
  this host's starts were serialized by `lockStart`, dropped in the
  post-review cleanup with 1.35.5 as the floor). `/tmp` is not tmpfs-shadowed under `machine cp` (staging in
  `/var/tmp` still works). Parallel execs are fine (30 short, 25
  concurrent 8 MiB `-i` streams). A plain exec's stdout is still capped at
  11534336 bytes, now with an explicit error; `-i` and `--stream` execs are
  not capped (so smol cp-out streams over `--stream`). stdin on a one-shot `exec -i` arrives whole and EOF reaches
  the guest when the host closes the pipe. `machine update --label` works
  on a running VM (not in 1.16.1). Stop every VM before upgrading smolvm.
- **A login bash at SHLVL=1 with `set -e` turns any `exit N` into 1**
  (2026-11-20): Ubuntu's stock `~/.bash_logout` runs `clear_console`, which
  fails without a console. A script whose exit code matters (the clone
  script's 86) runs as `bash -c` under `ExecOpts.Login`.
- **`DPkg::Lock::Timeout` does not cover `apt-get update`'s lists lock**
  (2026-11-20): devvm's installs wait for the dpkg lock, and its `apt-get
  update` retries.
- **macOS kqueue coalesces a relink into nothing** (2026-10-17): a symlink
  renamed over `devvm.toml` under load merges into one dir event, the old
  target's fd stays watched, and no Create/Remove fires. `status --watch`
  re-resolves a symlinked `devvm.toml` on every debounce for that reason,
  and a one-shot `status` touches the change marker on its way out, so the
  menu bar app's status per menu open is the nudge (no timer). A
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
  (ETXTBSY otherwise). This guest's linuxbrew `mosh` wrapper stalls in its
  fake-proxy ssh step: test mosh from the ThinkPad. A scripted proxied
  attach under `script` detaches only with ^B and d sent separately, and
  late. `script -qec "devvm …" /dev/null <<< y` drives
  `[y/N]` prompts; the huh form is unreadable under `script`. The
  **ThinkPad is the default live hub**; `cloud` (fake smolvm) only where
  `calls.log` must show the exact smolvm calls or a plain ssh target is
  needed.
- **Reusable test scaffolding**: `fakeSSH`/`fakeHub`/`fakeHubWith` in
  `internal/cli` run the real remote string through `sh` and assert exact
  ssh argv; the cp round trip re-execs the test binary as the hub's `devvm`
  (`TestMain` in `cp_hub_test.go`); `pinTTYs` fakes the TTY decision; the
  cli test binary's `TestMain` refuses `__daemon`.
- **Unix sockets and inodes**: tmpfs hands out inodes monotonically, ext4
  and APFS reuse them; `ownsSocket` needs the dial, not just the inode.
- **This dev guest is itself a devvm smol box** (2026-10-18): it has
  `/usr/local/bin/devvm-open-url`, so a test expecting the root shim to
  be missing fails here. Test the absence with a temp `$HOME` (an entry's
  `~/.local/bin` path) and assert only the root path's text.
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
  KVM, smolvm 1.35.5 since 2026-11-20), **intermittently offline**. Dedicated user
  `devvmtest`; nothing outside that home is touched. The only place
  smolvm-specific behaviour (exec concurrency, boot, `machine cp` staging,
  the 11 MiB cap) can be seen. VMs: `web` (ubuntu:24.04, 1 GiB,
  10 GB, with `python3`, `tmux`, `gh` and a persistent `dev` tmux session)
  and `raw` (ubuntu), both labelled `devvm=true`; there are no unmanaged
  smol VMs to test any more. Guest fixture: `nohup devvm exec web -- python3 -m http.server
  3000 --bind 127.0.0.1 >/dev/null 2>&1 &` on the hub.
- **`cloud`** (Hetzner CX23, `dev@2.29.47.196`; a `remote` entry `cloud`,
  never shaped since phase 4, and a hub entry `h` here): no `/dev/kvm`, so its `smolvm` is
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
  column's Swift change and the rest of simplify phase 4's (no `dormant`,
  `managed`/`shadowed`); a Mac as a smol hub (`smolvm machine create` and
  `open` from an sshd session, a daemon spawned there surviving logout);
  the v0.1.20 menu bar relaunch after "Update devvm".
