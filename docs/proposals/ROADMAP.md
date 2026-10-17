# Roadmap

The single ordered plan for the design docs in this directory. The docs own
the *what* and *why*; this file owns the *order*, what each step shipped, and
the issues and lessons the live runs left behind. When they disagree, this
file wins for sequencing and the design doc wins for mechanism.

Status, 2026-10-17: the **hubs + browser bridge** plan (`hub.md` v3.4,
`browser-bridge.md` v2.6) landed in full as v0.1.13–v0.1.15. The **config
redesign** (`config.md` v3.1) landed in full; the three hosts are migrated; v0.1.16 is the release. The
**provider backend** (`providers.md` v1.5) landed in full the same day,
steps 0–1 in v0.1.16 and steps 2–4 after it. Next: the known issues below,
then the ssh agent exec for remote boxes.

The long form of this file (per-step live-test lists and the detailed
"what shipped differently" notes for hub steps 1–9) is in git history before
the trim commit of 2026-10-17.

## Config redesign (in progress)

`config.md` "Implementation order" owns the sequencing of its six steps; this
records what landed. Loop per step: Opus implements, Opus reviews, live test
on the ThinkPad, one `jj commit`, **no release before step 6**.

- **Step 1 landed 2026-10-17**: `config.Paths{Config, State}`; `run/`,
  `cache/`, `known_hosts` and the ssh ControlMaster sockets (`c-%C`) move to
  `$XDG_STATE_HOME/devvm`. Migration moves files with link+unlink (never
  replacing one) and leaves a live daemon's run files alone; session clients
  find an old-layout daemon where it is, so an `install.sh` upgrade never
  spawns a sibling daemon (the review's blocker). Live-tested on the ThinkPad
  with a v0.1.15 daemon holding forwards.
- **Step 2 landed 2026-10-17**: saved forwards are `<State>/ports/NAME`
  (`HUB@NAME` for hub machines), edited under a per-name flock, seeded once
  from every conf by an all-or-nothing migration that never writes a conf.
  `ports add` saves by default (`--ephemeral` opts out). `UpdateHubMachine`
  and the hub-conf flock are gone (shared decision 6 superseded; hub.md §2
  is stale on this). `delete` clears host state through `forgetHostState`.
- **Step 3 landed 2026-10-17**: one `smolvm machine ls --json` snapshot in
  resolve (`created_at` is Unix seconds and survives stop/start, confirmed on
  smolvm 1.16.1); managed marker `<State>/managed/NAME`; unmanaged smol VMs
  limited to status/start/stop/shell/exec; `provision`/`deprovision`/dormant
  removed; fifth `--plain` column (`managed|unmanaged|shadowed|-`) with its
  Swift change built blind. Live-tested on the ThinkPad against the devvm-made
  `web` (managed via its conf) and a raw alpine VM `raw` (unmanaged; kept
  there as the `setup --adopt` test subject), one agent exec throughout.
- **Step 4 landed 2026-10-17**: `bootstrap` → `setup`; `[templates.*]`
  from `devvm.toml` with one-level fallback to `default` (precedence flag >
  `--template` > the box's old conf > `default` > `config.toml` > built-in);
  fixed order user → prereqs (+tmux, gh, mosh) → agent/shim → hook → repos →
  keys → harden → forwards; `setup --adopt NAME` on a discovered VM prints
  its plan and requires the VM's name typed on a terminal (`--yes` is no
  bypass); root adoption with a 90s bounded reachability wait and
  `EnsureUser(name)`; `setup --host`; proxied `create`/`setup` send the
  template inline and keep forwards on the laptop; `hubMinVersion` is
  v0.1.16. Live on the ThinkPad: the refusals, a typed-name adoption of a
  raw Ubuntu VM (`dev` user, prereqs, agent, `BROWSER`, forwards after),
  idempotent re-run. The remote-box run on `cloud` passed once the box
  recovered (prereqs including gh and mosh, the hook, nothing written to
  config).
- **Step 5 landed 2026-10-17**: `devvm.toml` is the only config file
  (`[templates.*]` + `[machines.*]` with `backend ∈ remote|hub` mandatory,
  `managed` default false, `host`/`user`/`transport`/`mosh_server`/
  `identity`/`connect_timeout`; unknown tables and keys rejected); the old
  `machines/*.toml` loader, `config.toml`, `defaults`, remote/hub `create`,
  `repos rm/clone`, `--no-clone` and the root-adoption conf write are gone.
  `config {path,show,edit,check,add,rm}`: `add` appends one table, `rm` cuts
  one table's lines, both re-parse and deep-compare before an atomic rename
  and refuse layouts they can't edit safely; `config rm` plans the edit,
  then stops local daemons (refusing on live forwards without `--force`),
  commits, and clears ports/reach/pin/cache. `delete` on an entry points at
  `config rm`. First run on the new build seeds managed markers and saved
  forwards from the old tree and prints a one-line pointer until `machines/`
  is removed. Old-hub rows (`remote-managed`/`remote-unmanaged`, four
  fields, `dormant`) are normalized. Live on the ThinkPad: pointer, seeds,
  `machines/` removed with both VMs still managed, `config add/check/show/rm`
  round trip, `delete` refusal.
- **Step 6, hand migration**: ThinkPad done (only a smol conf; `machines/`
  moved to `~/devvm-machines.bak`). This devvm guest done (`devvm.toml` with
  the default template and `cloud`/`h`/`scottdev3`/`tp`; old tree in
  `~/devvm-old-config/`). `cloud` done (`devvm.toml` with its adopted
  `scottdev3`; the fake smolvm now emits `created_at`, so its `web` is
  managed). **All three hosts run the step 5 build on the new layout.**
  Release v0.1.16 after a whole-redesign review.

## Provider backend (done, 2026-10-17)

`providers.md` "Implementation order" owns its five steps. Steps **0** (status
truthfulness: `run/refresh` + hidden `__refresh`, remote boxes probed by a
ControlMaster check then a 2s TCP connect, `cache/reach/NAME` with
verified-at, `status -v` last-contact text, sixth `--plain` field, the
`menuWillOpen` Swift line) and **1** (the `Provider` interface and the
Hetzner REST client under `internal/provider`, a fake, tests against
responses recorded from the user's **test project**, no CLI) depend on
nothing in the config redesign, so they run now, in the jj workspace
`providers` (`../devvm-providers`), with the usual loop (Opus implements,
fresh Opus reviews, one commit per step, no push). Order: **1 first** (no
shared files), then 0. Step 0 touches `status.go`, `status_watch.go` and
`backend/ssh.go`, which config steps 3–5 also touch: it is rebased onto
master when it lands and the config session is told. The token for the
test project is at `~/.local/state/devvm/providers/hetzner.token` (0600,
the path §1 names); the client reads `HCLOUD_TOKEN` first, then that file.
Steps **2–4** (token command, discovery, lifecycle, live test) wait for the
redesign: they build on resolve's source list, templates, `setup` and
`config check`.

- **Step 1 landed 2026-10-17** (`providers` bookmark; onto master with step
  0): `internal/provider` (`Provider` interface as §7, `KeyEnsurer`,
  `ErrConflict`/`ErrNotFound` sentinels, `Managed`, state tokens),
  `internal/provider/hetzner` (plain REST, pagination, action polling that
  rejects unknown statuses, 30s/16 MB limits, errors with status+code+message,
  no retry, `Token()` = `HCLOUD_TOKEN` then the state-dir file,
  `EnsureSSHKey` resolving a 409 by fingerprint), `internal/provider/fake`
  (same rules, `CallCount`, `CreateErr`). Fixtures recorded once against
  the user's test project (two throwaway cx23 + a key, deleted and the
  project verified empty); two hand-written (429, action error). Real API
  drift: servers carry top-level `location`, no `datacenter`. `Create` may
  return a server *and* an error (it bills): callers must handle both.
  `Spec.IPv6Only` (zero value = v4, §1's default). `Power(off)` is hard
  poweroff (graceful can hang; keep). Follow-up: the next recording should
  use a throwaway key (fixtures hold the user's real pubkey + a released IP;
  not secret, but persistent).
- **Step 0 landed 2026-10-17** (same bookmark): `internal/reach` cache
  (`<State>/cache/reach/NAME`, `key=value`, atomic; garbled → `?`),
  `backend/ssh_reach.go` probe (daemon master `run/NAME.master` → shared
  `c-%C` via `ssh -O check` → `ssh -G` → 2s TCP; ProxyJump/ProxyCommand ⇒
  `?`; a `?` or a deadline cut never overwrites a recorded state),
  `cli/reach.go` (round: 16 parallel, 5s; `noteContact` from exec/shell/
  attach/bootstrap; hidden `__refresh` writes `run/refresh`),
  `status`: one-shot probes, `--local`/`--watch` read the cache, the watch
  runs a round at start and on `run/refresh` only (tested: `TouchChanged`
  runs zero rounds), sixth `--plain` field `verified=<unix>:<how>` or `-`
  (hub rows pass a newer hub's through), `-v` last-contact text,
  `config.Machine.Probeable()` (a method, per config.md §9). Shared
  ControlMaster gains `ServerAliveInterval=30/CountMax=3` so a box that
  died during sleep can't show a stale `reachable (master)`. Swift (blind):
  `?` remote stays live with glyph "◌"; the app does **not** call
  `__refresh` (its one-shot `status` on menu open already probes;
  providers.md v1.3). Reviews: Opus ×2 per step, Fable design review at the
  boundary (ship). **Rebase onto master after config step 4**: contact hook
  → end of `setup`'s `shape` (how=`setup`); `remoteConfs()` → the new
  loader's remote entries (step 5); Status.swift live check →
  `backend != "smol" && !isHub` when the column becomes `remote` (step 5).
- **Step 2 built 2026-10-17** (`providers` bookmark; lands on master after
  v0.1.16 is tagged): `[providers.hetzner]` in the loader (placement keys;
  templates gain `server_type`/`location`/`image`/`ipv4`, template >
  provider table > built-in), `providers token hetzner [--clear]`,
  `BackendHetzner` on `sshBackend`, `config.NewProviderMachine` (record
  never saved; `Machine.Server` carries id/status/address), discovery last
  in resolve's source order (one `Get`, only with a table and a token),
  provider listing in the live round with `cache/provider-hetzner.list`
  (flock; live lookups merge rows in), `--plain` backend `hetzner` with the
  §5 state mapping, shadowing by entry/smol/hub row, refusal matrix (§8),
  lifecycle verbs refusing until step 3. Two Opus review rounds; the second
  made the daemon/`__session`/`config rm`/completion cache-only and stripped
  `HCLOUD_TOKEN` from the daemon env. Swift: unlabelled hetzner rows are
  not usable; `starting`/`stopping` glyph (blind).
- **Step 3 landed 2026-10-17**: `create NAME --backend hetzner` (name
  checks, refusals before any API call, key by name or uploaded file as
  `devvm-<12 hex>`, bill notice, 5m-deadline `Create`, cache merge, stale
  pin dropped, then `shape`), `delete` (confirm, `Delete`, `forgetHostState`
  incl. the known_hosts pin, cache row), `start`/`stop` (no-op paths, Power,
  unmanaged confirm, billing warning, `start` waits for sshd), proxied
  create pinned to `--backend=smol`, root flags before raw leaves, ControlPath
  length check, interrupt handling in create/setup. Two Opus reviews plus the
  live-test fixes.
- **Step 4 done 2026-10-17**: live run in the user's test project from an
  isolated config/state dir: create 118s (root adoption + prereqs), exec,
  ports (curl through the forward), cp-in/out, stop/start, every unmanaged
  refusal after removing the label via the API, delete clears the pin,
  recreate at the reused address, interrupted create still listed and
  deletable; project verified empty (one non-billing ssh key
  `devvm-42dec2d3bc2d` left). Skipped: v6-only (no v6 route here), browser
  shim on remote (deferred), interactive confirms (unit-tested). Not tested
  from a second host.
- Follow-ups (not scheduled): v6-only live test; mosh with a bare v6 host;
  the next fixture recording should use a throwaway key; `cp-in` ~4× slower
  than `cp-out`; let `[providers.hetzner]` carry a default `server_type`
  (user, 2026-10-17: it is provider-specific, so it reads oddly in
  `[templates.default]`; today it is a template field or `--server-type`,
  built-in `cx23`). Notes for step 3 from review: put a
  deadline on `Create`; `stop` should `Get` first and skip an already-off
  server; step 2 warns on a token file mode wider than 0600.

## After the config redesign (agreed order)

1. **Known issues** (below), the real ones first: killed daemon orphaning
   its ControlMaster, SIGINT leaking the cp spool, the cold-boot bridge
   retry.
2. **The ssh agent exec for remote boxes** (bridge §1 "Remote"): thin remote
   `auth`, `BROWSER` plumbing on remote boxes (bridge §5), deletion of
   `--auth` and the second agent socket. Deferred from the bridge plan
   because hub VMs never needed it; a provider backend does.
3. ~~Provider backend~~ done (above).
4. **Mac tests** when at the laptop: the v0.1.14 menubar changes (drop shelf,
   Tab completion, save panel), the step 3 fifth-column Swift change, and a
   Mac as a smol hub (`smolvm machine create` and `open` from an sshd
   session; a daemon spawned there surviving logout). Also cp-out path
   completion into a directory (written blind): cycle to a directory, then
   "/" goes in (no doubled slash) and Right arrow accepts so the next Tab
   does; a unique directory is still entered by the next Tab.

## Hubs + browser bridge (done)

| # | Step | Shipped in | Ships |
|---|------|-----------|-------|
| 1 | Hub backend + `HUB/NAME` | v0.1.13 | `backend = "hub"` confs, runtime identifier `HUB@NAME`, `listMachines`, shaping verbs refuse the hub itself, min-version check |
| 2 | Proxy | v0.1.13 | leaf argv rebuilt with the hub-side name, `env DEVVM_NO_SUBSCRIBE=1 devvm …` over the hub's ssh, `-t` iff stdin and stdout are TTYs, laptop-side inputs (`keys add`, `repos add`) resolved first |
| 3 | Merged listing + watch | v0.1.13 | `status --plain --local`, per-hub fan-out with cache and deadline, `--watch` holds one hub pipe per hub, hidden `--exit-on-stdin-eof` |
| 4 | cp over tar stdio | v0.1.13 | `cp-in --from-tar -` / `cp-out --to-tar -`, structural tar completeness check |
| 5 | Daemon ownership + sessions | v0.1.14 | owner kinds `conf`/`connection`/`ttl`, exact-or-bump sticky bind policy, long-lived sessions with acked `subscribe`, `ports down` op, dual-stack binds, `up:N` counts `conf` only |
| 6 | Hub forwards | v0.1.14 | relay sessions, `__session NAME` as a pure pipe, `hubTransport` (`-L` to the hub port the hub reports), relay heartbeat, `HubForwardsMinVersion = v0.1.14` |
| 7 | Daemon bridge + agent reply | v0.1.15 | URL classify (external/direct/redirect), connection- and ttl-owned bridge forwards, rate limit 10/min, 20-forward cap, `open-url` request/response, one `agentbin.Install` op |
| 8 | Thin `auth`, smol and hub | v0.1.15 | `auth` rides the daemon session (no second exec), capability echo `bridge`/`shim`, `auth HUB/NAME` proxies the leaf, `HubBridgeMinVersion = v0.1.15` |
| 9 | `BROWSER` plumbing (smol) | v0.1.15 | agent + shim at bootstrap, guarded `/etc/profile.d/devvm.sh`, `tmux set-environment` on attach, guarded exec `Env` |

Mechanism lives in the design docs; CLAUDE.md carries the operational rules
(one exec, relay, bridge, thin auth). Minimum hub versions are pinned by
tests: `hubMinVersion = v0.1.13`, `session.HubForwardsMinVersion = v0.1.14`,
`session.HubBridgeMinVersion = v0.1.15`.

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
   `devvm-agent serve` into a VM. Remote boxes keep `auth`'s private session
   until the ssh agent exec ships.
5. **`--plain` changes ship with their Swift change** in the same commit.
6. ~~Hub-machine state lives in the hub's conf.~~ **Superseded by config
   step 2:** a client's saved forwards for any machine, `HUB/NAME` included,
   are host state in `$XDG_STATE_HOME/devvm/ports/`.
7. **VM commands never write config; `config` commands never touch a box**
   (config §4). devvm modifies only boxes it manages: made by `create` or
   handed over with `setup --adopt` (config §2–3).

## Known issues (pre-existing, found by live runs, not fixed)

- **Cold-boot bridge retry**: `attach`/`shell` on a stopped VM, local or
  `HUB/NAME`, gets no bridge for that session, only a notice, because the
  daemon's exec startups would overlap the attach's own tmux execs while
  the VM boots. Needs a live test on real smol before changing.

Cosmetic or accepted:

- A bare `devvm exec NAME …` with no daemon up (no forwards, no
  subscriber) still sees a stale guest clock after a host suspend: the
  daemon syncs the clock only while it holds the agent exec.
- An external `devvm exec` storm (~7,800 calls in two hours from a client's
  hook) OOM-killed the fake-smolvm cloud hub. Not devvm's to fix: every hub
  exec is a login shell by design, and a rate limit would only make the
  external client's calls fail (decided 2026-10-17).
- `keys add` dedup strips the target's *own* `id_*.pub`, so a loopback
  `self` machine breaks its own `ssh localhost`.
- `^C` on a `-t` proxied command reports "cannot reach hub" (ssh's 255 is
  read as unreachable by design). The remote huh form can take >10s to first
  paint through a cold ControlMaster.
- The ssh transport notices a dead ControlMaster only on its 30s
  `checkInterval`, so a forward reads `up` for up to 30s after `ssh -O exit`.
- A hub `stop`→`start` waits out the laptop's reconnect backoff (≤30s).
- A locally stamped build with a suffix (`…-dirty-step6`) sorts as a
  prerelease and refuses a hub as older than `hubMinVersion`.
- `ports list NAME` prints "no ports configured" above a live list of
  `connection`-owned forwards.
- `devvm-open-url` exits 0 even when the open was refused (the reply text
  says so); the redirect TTL's 10-minute expiry was never live-tested. The
  host browser is opened by the subscribing `attach`/`shell` process, so its
  PATH decides the opener.
- Cancelling a private-session `auth cloud` logs a yamux ping warning and
  prints both "opening on host" and "opened on host".
- Killing a laptop-side `cp-in HUB/NAME` after the archive is fully written
  does not abort it (no pty, no SIGINT propagation; "nothing created" holds
  only for an early kill). The laptop then says "interrupted after upload;
  the hub may still complete the copy".
- A process backgrounded in a detached `exec` did *not* die with that exec
  on the ThinkPad, contrary to the Environments note of the long form.

## Lessons (don't relearn)

- **macOS kqueue coalesces a relink into nothing** (2026-10-17): a symlink
  created and renamed over `devvm.toml` under load merges into one dir
  event; fsnotify's listing then finds nothing new and the old target's fd
  stays watched, so no Create/Remove ever fires. `status --watch`
  re-resolves a symlinked `devvm.toml` on a 2s timer for that reason. A
  test that "passed once on macOS" after a kqueue change proved nothing.
- **A scratch binary that imports `internal/session` must hand `__daemon`
  to the real devvm.** `spawnDaemon` re-execs `os.Executable()`; under a
  scratch tool that recursed into a fork bomb that took the 2 GB guest down
  twice. `spawnDaemon` now refuses an empty name.
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
  relay's `*os.File` was finalized while in use. Keep those fixes; wait on
  signals, never sleep, in late-dial tests.
- **Live testing**: refresh a hub's binary by `scp` to a temp path then `mv`
  (ETXTBSY otherwise). `script -qec "devvm …" /dev/null <<< y` drives
  `[y/N]` prompts; the huh form is unreadable under `script`. From hub step 5
  on the **ThinkPad is the default live hub**; `cloud` (fake smolvm) only
  where `calls.log` must show the exact smolvm calls or a remote-managed ssh
  target is named.
- **Reusable test scaffolding**: `fakeSSH`/`fakeHub`/`fakeHubWith` in
  `internal/cli` run the real remote string through `sh` and assert exact
  ssh argv; the cp round trip re-execs the test binary as the hub's `devvm`
  (`TestMain` in `cp_hub_test.go`); `pinTTYs` fakes the TTY decision; the
  cli test binary's `TestMain` refuses `__daemon`.
- **Unix sockets and inodes**: tmpfs hands out inodes monotonically, ext4
  and APFS reuse them; `ownsSocket` needs the dial, not just the inode.

## Live-test environments

- **This devvm guest** is the "laptop": no browser, so a fake `xdg-open` on
  `PATH` that logs its argument stands in for one. ~6 GB RAM, no swap: run
  go with `GOMAXPROCS=2 GOMEMLIMIT=1GiB GOFLAGS=-p=1`, one command at a time.
- **`cloud`** (Hetzner CX23, `dev@2.29.47.196`, registered here as
  remote-managed; hub conf `h`): no `/dev/kvm`, so its `smolvm` is
  `contrib/fakesmol/smolvm` (the "guest" is the box; every call appended to
  `~/.local/share/fakesmol/calls.log`; guest port P bumps to P+1 on the hub).
  It also holds the user's machine `scottdev3`: never touch it.
- **ThinkPad** (`devvmtest@192.168.1.196`, Linux Mint, real KVM; hub conf
  `tp`): real smolvm 1.16.1 and a real smol machine `web` (ubuntu:24.04,
  1 GiB, 10 GB) under the dedicated user `devvmtest`; nothing outside that
  home is touched. The only place smolvm-specific behaviour (exec
  concurrency, boot, `machine cp` staging, the 11 MiB cap) can be seen.
  `web` has `python3`, `tmux`, `gh` and a persistent `dev` tmux session.
  Guest fixture: `nohup devvm exec web -- python3 -m http.server 3000
  --bind 127.0.0.1 >/dev/null 2>&1 &` on the hub.
