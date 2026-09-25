# Roadmap: hubs + browser bridge

Status: 2026-09-25, **steps 1–4 (Milestone A, released as v0.1.13), 5 and 6
(Milestone B) implemented and committed**;
see Progress below. Plan text revised after a sixth review (`status --local` for
hub listings so hubs never fan out, relay sessions refused while a
transport is down, `keys add`/`repos add` inputs resolved on the laptop,
a `flock` on the hub conf from the start, `hostbrowser.Open` reports
success, a literal `[::1]` callback is refused when `::1` is unbound,
tmux `set-environment` instead of `-e`, host-level subscriber priority
across a relay stated, remote `BROWSER` plumbing deferred with the ssh
agent exec, thin `auth` installs nothing; the piped-stdin test dropped). This is
the single ordered plan for the two design docs in this directory. The docs
own the *what* and *why*; this file owns the *order* and what each step
ships. When they disagree, this file wins for sequencing and the design doc
wins for mechanism. Mechanism for leases, subscriptions, ownership and bind
policy lives in `browser-bridge.md` §3–4 only.

- `hub.md` (v3.4): reach another host's smol VMs as `HUB/NAME`; the hub daemon
  is the only agent-exec owner, the laptop is a client.
- `browser-bridge.md` (v2.6): the forward daemon binds ports and emits
  guest→host URL events; a subscribed client opens the browser; `auth` stops
  running its own agent exec on smol.

Already shipped and assumed here: daemon reconnect, `status --plain --watch`,
`devvm update`, the macOS menu bar app (`contrib/macos`), v0.1.11.

## Progress

**Milestone A landed 2026-09-15**: steps 1–4 are four commits on `master`
(`hub backend + HUB/NAME`, `proxy commands on HUB/NAME to the hub`,
`merged hub listing, cache and watch`, `cp-in/cp-out for hub machines over
tar stdio`), not yet pushed, no release tagged. Each step went through the
same loop: implementation, `go build/vet/test` + `gofmt`, an independent
Opus code review (every step had findings, step 4 a blocker), the fixes, the
step's live list run from this devvm guest against the real hubs, then one
`jj commit`. Hub confs `h` (cloud) and `tp` (ThinkPad) exist in this guest's
`~/.config/devvm`; both hubs run a `dev` cross-build of step 4.

**Step 5 landed 2026-09-25** (one commit, not pushed; Opus implemented and
reviewed it, two review rounds). Its live list passed on `cloud` (ssh) and on
the ThinkPad's `web` (real smol, one agent exec throughout); `update`'s
daemon cycling was checked only through a local `--finish-from`, as no newer
release exists.

**Step 6 landed 2026-09-25 (Milestone B)**, same loop (two review rounds). Its
live list passed on the ThinkPad (`tp`, real smol) with `cloud` (`h`) for the
`calls.log` check and the old-hub refusal (a v0.1.13 hub reads "predates hub
forwards (needs v0.1.14 or newer)"). **The next release must be v0.1.14 or
later** (`session.HubForwardsMinVersion`, pinned by a test). One agent exec
served the relay throughout; reconnects took 2–4s after killing the hub
daemon, the laptop master, or the hub's agent exec. Both hubs run the step 6
cross-build. Next: step 7.

**Tag `v0.1.13` from step 4 or later, never from an earlier commit.**
`hubMinVersion` (`internal/cli/hub.go`, pinned by a test) is `v0.1.13`
because a hub must answer `status --plain --local --watch
--exit-on-stdin-eof` and the hidden `cp-in --from-tar -`/`cp-out --to-tar -`
forms, all of which arrived in steps 3–4.

What shipped differently from the plan, per step (the design docs were
updated where the mechanism changed; this is the short list):

- **1.** `delete HUB` already refuses while `[machines.*]` tables or
  `run/HUB@*.sock` exist (`--force` overrides); planned for step 6, pulled
  forward because hand-written tables exist from day one. `transport` is
  allowed on a hub conf (it steers the interactive hop of a proxied
  `attach`/`shell`; mosh currently falls back to ssh with a notice).
  Completion omits hub names for the verbs that refuse them.
- **2.** Dispatch lives in each proxied leaf's `RunE` (`hubOr` in
  `internal/cli/proxy.go`), not in `resolve` (which stays the refusal
  guard), because only `RunE` has the parsed `*cobra.Command` (hub.md §3
  updated). The prefix is `env DEVVM_NO_SUBSCRIBE=1 devvm …`: `shellJoin`
  single-quotes every token and a quoted `'VAR=1'` is a command name, not
  an assignment. **`-t` only when stdin *and* stdout are terminals**: with
  stdout redirected a remote pty turns `\n` into `\r\n` and merges stderr
  (`exec h/web -- cat f > out` was corrupted). `-o LogLevel=ERROR` on `-t`
  runs silences ssh's "Shared connection closed". `delete HUB/NAME`
  confirms once, on the hub. `keys add` accepts options-prefixed key lines.
  `contrib/fakesmol/smolvm` learned `machine exec -d` (tmux keeper).
- **3.** The 5s listing budget *includes* the 1s `cmd.WaitDelay`
  (`backend.HostWaitDelay`): after the deadline kills ssh, the ControlMaster
  mux holds the client's stdout for the full drain. A hidden
  `status --exit-on-stdin-eof` flag makes the hub-side watcher exit when
  the laptop's dies (otherwise one orphan per laptop watcher restart, alive
  until its next write). `blockSplitter` is capped at 1 MiB and drops
  leading blank lines (login banners), so a hub with an *empty* registry
  reads `unreachable` in `--watch` until it has a machine. Swift `isHub`
  needs `hub == nil`, not just backend `hub`: machine rows the hub did not
  list carry backend `hub` too. Ports items stay gated off hub machines in
  the app until step 6.
- **4.** Tar completeness is structural (`readTarStream`: the bytes the
  `tar.Reader` consumed must include the two terminator blocks); the
  original "last 1024 bytes are zero" sniff accepted a NUL-tailed entry cut
  at its boundary and would have committed a partial upload. A reader-side
  diagnosis wins over ssh's death when the laptop gives up first.
  `-o RequestTTY=no` on every BatchMode run (a `RequestTTY force` in
  `~/.ssh/config` would CRLF-mangle the stream). No `--quiet`: the hidden
  forms print nothing; hub stderr passes through verbatim, so hub-side
  refusals name `web`, not `tp/web`. `hubBackend.Copy` stays refused; the
  CLI dispatches cp itself.
- **5.** `ports down` is a new one-shot `down` op (drop every `conf`
  owner; the reply says `stopped` or which forwards/sessions remain);
  `stop` stays unconditional for `devvm stop`/`delete`/`update`, and a
  pre-step-5 daemon's "unknown op" falls back to `stop`. Events are opaque
  in step 5: `{"event":{"id":N,"data":…}}` / `{"reply":{"id":N,"data":…}}`
  (step 7 defines `data`). Session requests run in order on one worker per
  connection so the reader only routes. `reconnecting:N` counts `conf` only
  too and is never `:0`. `ports rm` on an unconfigured port also drops a
  stale `conf` owner (conf edited by hand). `update` respawns a daemon
  only if a live `conf`-owned forward maps a configured port; any other up
  daemon is stopped and left to its session clients to respawn, so a
  `ports down` held open by a session stays down. The ticker retries every
  forward pending while up (bumpable ones from their own port), not only
  exact ones, and `restore()` skips a slot an add is still binding. An add
  that finds a slot mid-bind waits for that bind's result before answering.
  A forward from a pre-owner daemon lists no owners and counts as `conf`.
  `ttl` owners exist but nothing creates or expires them yet (step 7).
- **6.** `__session` is a pure pipe: it refuses a stopped VM, dials the
  daemon, prints `\ndevvm-session-v1\n` (the leading newline keeps a
  banner without one off the marker), then relays bytes both ways; the
  laptop's `hubTransport` writes `{"id":1,"op":"session","relay":true}`
  itself, so the hub's refusal reply reaches it verbatim (hub.md §7 has
  the exact wire). A daemon that admits a relay echoes `"relay":true`;
  without the echo (a step-5 hub daemon) or on "unknown op" the laptop
  refuses and names the hub-side restart, and a hub with no `__session`
  is named with `session.HubForwardsMinVersion` (`v0.1.14`, pinned by a
  test); `hubMinVersion` stays `v0.1.13`, forwards are checked per
  feature. The laptop daemon's master is per hub machine
  (`run/HUB@NAME.master`); `__session` runs over it with
  `ControlMaster=no`. Marker and open share one 45s deadline; later calls
  time out after 10s. Teardown sends no per-forward `remove` or `-O
  cancel` (the relay's close and the master's exit drop them all), so it
  stays bounded against a wedged link. A hub-side error on `add` counts
  as port exhaustion: a first add fails, `restore()` leaves it pending. The hub is asked for the guest port as its
  preference, never exact, and only after this host's port passed the
  IPv4 probe. A `pending` reply marks the transport dead, and `add` now
  records a forward pending (not failed) whenever the bind failed on a
  dead transport, even before `loop()` has seen the death (general fix,
  `transportDead`). `onDead` drops relay owners in the critical section
  that marks the daemon reconnecting, then closes the relays. `session.Dial`
  fails fast when the spawned daemon exits before listening (it quotes the
  last line that daemon wrote, never an earlier run's) instead of polling
  out its deadline, and a hub machine's come-up budget adds the relay's
  open deadline plus 10s (75s at the defaults). The hub-conf flock is a
  sibling `machines/.HUB.toml.lock` (the rename replaces the conf's
  inode); an edit that changes nothing writes nothing, an emptied
  `[machines.NAME]` table is dropped, `delete HUB/NAME` goes through the
  same lock, and `delete HUB` removes the conf and unlinks the lock while
  holding it. `start HUB/NAME` retries the laptop daemon's dial
  for up to 10s (smolvm on the hub can still say `starting`). `ports add
  HUB/NAME` whose daemon cannot come up records the mapping and exits
  non-zero (a stopped hub VM and an unreachable hub look alike from here).
  `listMachines`'s `run/HUB@*.sock` enumeration had already shipped in
  step 1; step 6 only adds its test. A laptop `stop HUB/NAME` (and
  `deprovision HUB/NAME`) stops the laptop daemon once the hub's command
  succeeds, as a local `stop` does, instead of leaving it redialing a
  stopped VM's `__session` every 30s; `start HUB/NAME` respawns it. A
  hub-side `stop web` still leaves the laptop daemon `reconnecting`. **Relay
  heartbeat** (pre-release fix): the relay open carries `"heartbeat":20`
  (seconds, echoed), the laptop pings at that interval, and the hub
  daemon closes a relay it has read nothing from for three intervals
  (60s), which ends the `__session` and drops its forwards; a ping
  unanswered for two intervals marks the laptop's transport dead. Local
  sessions keep no deadline (hub.md §7). `delete` (local, `HUB/NAME`, and
  every `HUB@*` on `delete HUB`) stops the daemon, waits for it to be
  gone, then removes its `run/` log and any socket or master nobody
  answers (`session.Reap`); the lock goes only after the conf does
  (unlinking a contended flock lets two starters past it), so `HUB/NAME`
  keeps its empty lock. `ssh -O exit`'s stderr no longer
  reaches the daemon log. The cli test binary's `TestMain` refuses `__daemon`, so a
  test that reaches `session.Dial` fails fast instead of re-running the
  suite as a "daemon". "The daemon binds nothing for a relay" has no code
  in step 6: `sess.relay` is recorded for step 7's bridge. Swift: the
  `isDirect` gate is gone; Ports items and the "Open localhost:PORT"
  lookups cover hub machines.

Known issues found by the live runs and **not** fixed (all pre-existing;
recorded so they are not rediscovered):

- Step 6: a hub `stop`→`start` waits out the laptop's reconnect backoff
  (up to 30s; hub-side `start` cannot kick the laptop).
- `create --backend hub` on a locally stamped build with a suffix
  (`…-dirty-step6`) refuses the hub as older than `hubMinVersion`: the
  suffix misses `describeRe` and sorts as a prerelease. Real tags are fine.

- The ssh transport notices a dead ControlMaster only on its 30s
  `checkInterval` (`transport_ssh.go`), so after `ssh -O exit` a forward
  reads `up` for up to 30s before `reconnecting`.
- `ports list NAME` prints "no ports configured" above a live list of
  `connection`-owned forwards (cosmetic).

- **`cp-out` of more than 11 MiB from a real smol VM fails**: smolvm 1.16.1
  caps `machine exec` streamed stdout at 11534336 bytes ("streaming output
  exceeded … cap; exec terminated"), and `downloadArchive`
  (`internal/cli/cp_out.go`) streams the tar over exec stdout. Fails
  all-or-nothing, locally and proxied alike. Needs a different download
  path (`machine cp` out of the guest, or chunking) before large `cp-out`
  works on smol anywhere.
- **SIGINT during a copy leaks the laptop-side spool** (`defer
  os.RemoveAll` never runs; the CLI installs no signal handler outside
  `status --watch`). Five killed 200 MB copies left 610 MB in `$TMPDIR`.
- `devvm exec` flattens the guest command's exit code to 1 (`Execute`),
  locally and proxied; `proxyExit` passes codes through for whenever that
  changes.
- `keys add` dedup strips the target's *own* `id_*.pub` from its
  `authorized_keys` (documented behaviour), so a loopback `self` machine
  breaks its own `ssh localhost`. `keys` verbs are remote-only, so `keys add
  h/web` against a smol `web` is refused by the hub, not the proxy.
- `^C` on a `-t` proxied command reports "cannot reach hub" (ssh's 255 is
  conflated with unreachable by design). The remote huh form can take more
  than 10s to first paint through a cold ControlMaster.
- Killing a laptop-side `cp-in` after the archive is fully written does not
  abort it: with no pty there is no SIGINT propagation and the hub finishes
  what it received (correct, but "nothing created" only holds for an early
  kill). Hub and VM staging dirs unwind asynchronously (gone within ~10s).

Lessons that later steps should not relearn:

- **A scratch binary that imports `internal/session` must hand
  `__daemon` to the real devvm.** `spawnDaemon` re-execs
  `os.Executable()`; under a scratch tool that is the tool itself, which
  parsed `__daemon` as a fresh run with no name and recursed into a fork
  bomb that took the 2 GB guest down twice during step 5's live run.
  `spawnDaemon` now refuses an empty name.

- **A proxied command's stdin must be an `*os.File`, never an `io.Pipe`.**
  os/exec's `awaitGoroutines` closes the parent pipes after `WaitDelay` and
  then still blocks on the stdin copy goroutine, which sits in
  `io.Pipe.Read` forever; step 3 hit this deadlock. Step 6's `__session`
  pipe and step 8's relay must follow the pattern in
  `internal/cli/hublist.go` (`os.Pipe`, write end held for the attempt).
- On both hubs only the login shell has `~/.local/bin` on `PATH`;
  `backend.LoginShellArgv` (`sh -c 'exec "${SHELL:-sh}" -lc "$0"' …`) is
  what makes every proxied command work. A `~/.bash_profile` shadows
  `~/.profile`, so a banner test must `. ~/.profile` first.
- Test scaffolding to reuse: `fakeSSH`/`fakeHub`/`fakeHubWith` in
  `internal/cli` run the *real* remote string through `sh` with `devvm` on a
  `.profile`-only `PATH` and assert exact ssh argv via `wantSSH`; the cp
  round trip re-execs the test binary as the hub's `devvm` (`TestMain` in
  `cp_hub_test.go`); `pinTTYs` fakes the stdin/stdout tty decision;
  `shortHubTimes` shortens the listing deadlines. `go test -race
  ./internal/cli` is part of the per-step check now.
- Live testing: refresh a hub's binary by `scp` to a temp path then `mv`
  (writing onto the running binary fails with ETXTBSY; the `cp-in -f`
  one-liner below does not work as written). `script -qec "devvm …"
  /dev/null <<< y` drives `[y/N]` prompts; the huh form paints unreadably
  under `script` (0×0 pty), so only its presence can be checked. Testing
  every step on both hubs paid off once (step 2: `attach`, `keys`, real VM
  timings needed the ThinkPad; the fake's missing `exec -d` needed cloud);
  from step 5 on the **ThinkPad is the default hub** and cloud is used only
  where `calls.log` must show the exact smolvm calls or where the roadmap
  names `cloud` as the remote-managed target.
- The ThinkPad VM `web` now has `python3` (guest fixture), `tmux`
  (`attach`) and a persistent `dev` tmux session. `cp-in` into it runs at
  about 8.5 MB/s over the LAN.

## Shared decisions

Both docs build on these. The mechanism behind 1–3 is written once, in
`browser-bridge.md` §3–4 (with the hub-side consequences in `hub.md` §7–8);
this list only names the decisions.

1. **Forward ownership** (bridge §4): owner set with kinds `conf`,
   `connection`, `ttl`; torn down when empty; owners decide lifetime and
   nothing else. `up:N` in `status --plain` counts `conf` only and `up:0` is
   never emitted (`down` or `-` instead, bridge §8).
2. **Bind policy is a request property kept for the forward's life** (bridge
   §4): exact-or-bump, honoured by `restore()` too, sticky once any owner
   asked for exact, pending exact binds retried on the ticker; every
   forward dual-stack with `::1` best-effort. A `redirect_uri` callback is
   exact on the host that shows the browser and nowhere else.
3. **Subscribe-to-open** (bridge §1, §3): the daemon never opens a browser
   and never rewrites a URL; a long-lived control connection is a *session*
   and holds the daemon (no separate `hold` op); a session declares `relay`
   at open, and the daemon binds nothing for a relay and closes relay
   sessions when its transport dies; every request and event carries an
   `id`, one reader per side; `subscribe` is a message on a session and is
   acknowledged (a relaying daemon acks after its upstream ack); most
   recent subscriber *of that daemon* wins, so priority across a relay is
   per host (hub §8); a daemon refuses relay sessions while its transport
   is down; one session
   client with reconnect serves `attach`/`shell`/`auth` and the hub
   transport; the hub proxy runs commands with `DEVVM_NO_SUBSCRIBE=1`, which
   skips the daemon. One forwarding contract (hub §7): clients always name
   guest ports.
4. **One exec per smol VM, always.** Nothing on any host spawns a second
   `devvm-agent serve` into a VM. The laptop never spawns one into a hub VM;
   `auth` stops spawning its own on smol. Remote boxes keep `auth`'s private
   session (ssh has no one-exec limit) until the ssh agent exec ships.
5. **`--plain` changes ship with their Swift change** in the same PR.
6. **Hub-machine state lives in the hub's conf** (hub §2): `[machines.NAME]`
   tables in `machines/HUB.toml`, `ports` the only field. No `hubs/` tree.

## Steps

Each step is one PR: build, tests, Opus review, then the next. "Swift" notes
what `contrib/macos` must change in the same PR, if anything.

| # | Step | From | Ships | Swift |
|---|------|------|-------|-------|
| 1 | Hub backend + `HUB/NAME` | hub §1–3 | `backend = "hub"` confs with `[machines.NAME]` tables, `create --backend hub`, name parsing in `resolve`, runtime identifier `HUB@NAME` for socket/log/lock, `cli.listMachines` (local + hub-conf tables; not in `config`) for status/update/`ports list`/completion, `delete` drops the table, shaping verbs refuse the hub itself, minimum-version check with a warning on other mismatches | none |
| 2 | Proxy | hub §3 | `a.proxy` builds the remote argv from the parsed command (`$SHELL -lc`, `DEVVM_NO_SUBSCRIBE=1`, `-t` iff TTY, nothing after `--` touched); laptop-side inputs resolved first (`keys add` spec to inline lines, `repos add` origin from the laptop cwd); `attach`/`shell`/`exec`/lifecycle/`repos`/`keys`/`create` proxied | none |
| 3 | Merged listing + watch | hub §5 | `status --plain --local` (this host only, no hub fan-out); `status` merges per-hub `--plain --local`: state and backend from the hub row, forwards column from the laptop's own daemon for `HUB@NAME`; a row for the hub itself (`hub` group); the listing runs with a 2s connect timeout, `BatchMode=yes` and an overall deadline; cache in `cache/` outside watched dirs, `listMachines` reads it; `--watch` holds one hub pipe per hub, re-spawns it with backoff on EOF, re-reads hub confs on fsnotify; completion for `HUB/` | group rows by `HUB/` prefix; `unreachable` state token; `hub` backend rows |
| 4 | cp over tar stdio | hub §6 | `cp-in --from-tar -`, `cp-out --to-tar -` (marker line before the stream), laptop-side loops | drop target works for hub machines (no change if it shells out by name) |
| | **Milestone A** — *done 2026-09-15* | | Laptop drives desktop VMs: list, attach, create, lifecycle, cp. Zero daemon changes. | |
| 5 | Daemon ownership + sessions | bridge §3–4 | owner set and `exact` on `fwd`; `session` connections (long-lived; `id` on every request/event, one reader per side, serialized writes; `add`/`remove` owned by the connection, acked `subscribe`/`unsubscribe`, re-subscribe moves to front; no hub vocabulary yet: `relay` arrives in step 6); the session client in `internal/session` (dial, session, acked subscribe, reconnect with backoff); idle rule `forwards==0 && sessions==0`; `ports rm`/`ports down` per bridge §4; `restartDaemons` cycles every up daemon; `add` takes exact-or-bump, `restore()` honours it, `exact` sticky on reuse, a ticker retries pending exact binds (the same ticker expires `ttl` owners in step 7); every forward dual-stack with `::1` best-effort (ssh: one `localhost:` spec, IPv4 pre-probe decides busy); `up:N` counts `conf` only, never `up:0` | none (`down`/`-` already parsed) |
| 6 | Hub forwards | hub §7 | `session {relay}` on the daemon (declared at open; the daemon closes relay sessions when its transport dies and refuses new ones until it is back, bridge §3), `__session NAME` (one per hub machine: marker line, then a `session {relay:true}` relayed verbatim, forwards owned by its connection), `hubTransport` on the laptop daemon (one `__session` on its master; guest-port contract; `-L` to `127.0.0.1:hubPort`; a `pending` add reply is a bind failure; up only after hub `add` and `-L` both hold; `dead()` on master or process death; re-resolves the hub port in `restore()`); `start HUB/NAME` brings up laptop-configured forwards after proxying; the hub-conf rewrite under `flock`; `listMachines` also enumerates live `run/HUB@*.sock`; `update` cycles hub-machine daemons | forwards for hub machines appear like any other |
| | **Milestone B** | | `ports add desktop/web 3000` works from the laptop; both users can hold forwards to one VM. | |
| 7 | Daemon bridge + agent reply | bridge §1–2, §4 | `events()` on the transport interface; kinds external/direct/redirect; `connection`-owned forwards for direct opens, `ttl` forwards for redirects (exact-port, wall-clock TTL, ≤20, no ports <1024, opens rate-limited); no bind for relay subscribers (a no-op until step 6 lands); `CallbackPort` moves to the daemon minus its 1455 exclusion; event carries `{url, kind, guest?, bound?}`, subscriber reply relayed to the guest with a timeout; `hostbrowser.Open` returns an error (refused URL, no opener, opener failed to start) and `opened:true` means the opener started; a literal `[::1]` URL is refused when the `::1` bind failed; `open-url` becomes request/response; agent+shim become one `agentbin.Install` op, run by the smol transport before it spawns the agent exec; `attach`/`shell` open a session and subscribe through the session client; **`build.sh` + commit agent** | none |
| 8 | Thin `auth`, smol and hub | bridge §6, hub §8 | `auth` dials, reads the shim path from `ping`, subscribes, runs logins where the transport has events (no `agentbin.Install` on this path); the private session and `--auth` **stay** for remote backends; `__session` carries `subscribe`/`unsubscribe` and streams events/replies; `hubTransport.events()` is fed by them; the laptop daemon subscribes on the hub while it has local subscribers, re-subscribes on each new one, and acks a local subscribe only after the hub's ack; `attach`/`shell`/`auth desktop/web` = dial the laptop daemon, subscribe, proxy with `DEVVM_NO_SUBSCRIBE=1`; `hubBackend.Run` carries `BROWSER` in the proxied exec argv; agent/shim install skipped for hub machines | none |
| | **Milestone C** | | One exec per smol VM in every flow. Any browser open from any smol or hub devvm session lands on the host you are sitting at. | |
| 9 | `BROWSER` plumbing (smol) | bridge §5 | agent+shim installed at `bootstrap` on managed smol boxes; `profile.d` (guarded); `tmux set-environment` before attach; smol `Env`; README. Remote boxes: deferred | none |

Deferred, not scheduled: the ssh agent exec for remote boxes (bridge §1,
"Remote"), and with it thin remote `auth`, `BROWSER` plumbing on remote
boxes (bridge §5), and the deletion of `--auth` and the second agent
socket. Hub VMs do not need it; Hetzner boxes are the only
remaining case, and their `auth` keeps working on the private session it has
today.

## Dependencies

```
hub track:     1 → 2 → 3 → 4      (Milestone A; no daemon work)
               1, 5 → 6           (Milestone B; 6 adds the relay rules to the
                                    daemon; needs 1's backend + runtime id)
bridge track:  5 → 7               (5 is the only shared daemon change)
join:          2, 6, 7 → 8         (Milestone C)
               8 → 9
```

Two tracks, one design: the hub track (1–4, then 6) and the bridge track
(5, then 7) share only step 5, and step 5 carries no hub vocabulary, so
either track ships and is testable on its own. **Milestone A goes first**:
four PRs with no daemon surgery that deliver most of the daily value
(list, attach, create, lifecycle, cp against another host's VMs), and a
week of using it is the cheapest check that `HUB/NAME` and the
rebuilt-argv proxy hold up before the daemon work starts. Step 5 can be
built in parallel with 1–4 by a second hand; it is the riskiest change and
everything after it builds on it. Step 7 touches the committed agent
binaries; step 8 does not (the relay is host-side). Do not interleave other
agent changes around step 7.

## Testing

Every step ships with unit tests under `go test ./...` (no VM, no ssh: the
daemon has `fakeTransport`, ssh-shaped code runs against a fake `ssh` on
`PATH`, tar and argv builders are pure) **and** a live check from the laptop
against a real second host. This section lists both per step, so a step is
"done" when its unit list is green and its live list has been run once. The
live lists are written for the environment below; the ThinkPad column says
what only a real smol hub can show.

### Environments

- **Laptop**: any host with the repo and `./install.sh` on `PATH`. The
  devvm guest this plan is being written in works; it has no browser, so a
  fake `xdg-open` on `PATH` that appends its argument to a log file is the
  "browser" (`hostbrowser` looks it up on `PATH` on Linux). The same trick
  works on any headless hub.
- **`cloud`** (Hetzner CX23, `dev@2.29.47.196`, registered here as a
  remote-managed machine): the **hub** for the hub track and the **remote
  target** for anything ssh-backend. Hetzner exposes no `/dev/kvm` on any
  plan, so `~/.local/bin/smolvm` there is **`contrib/fakesmol/smolvm`**: the
  "guest" of each fake machine is the box itself, state lives in
  `~/.local/share/fakesmol/`, every invocation is appended to `calls.log`.
  Everything devvm layers on top is real: root entry then `sudo -u dev`,
  the agent installed to `/usr/local/bin` and spawned as
  `devvm-agent serve`, yamux forwards, tmux, the shim. A fake machine
  `web` exists in the hub's own `~/.config/devvm`. Two caveats: guest port
  P *is* hub port P, so a hub-side forward of P bumps to P+1 (the laptop's
  forward is unaffected, it targets whatever the hub reports); and nothing
  smolvm-specific is exercised (exec concurrency, boot, `machine cp`
  staging, `-i --stream`). Refresh the hub's binary with
  `GOOS=linux GOARCH=amd64 go build -o /tmp/devvm ./cmd/devvm`, copy it to
  a temp path on the hub and `mv` it over `~/.local/bin/devvm` (writing
  onto the running binary fails with ETXTBSY).
  Its login shell prints nothing before a command (checked), so the
  marker-line tests add an `echo` to `~/.bash_profile` and remove it after.
- **ThinkPad** (`devvmtest@192.168.1.196`, Linux Mint 22.3, real KVM): the
  **real hub**. Everything lives under the dedicated user `devvmtest` (own
  home, own `~/.config/devvm`, own smolvm state); nothing else on the box
  is touched. Real smolvm is installed there for that user and a real smol
  machine `web` (ubuntu:24.04, 1 GiB, 10 GB, no bootstrap hook) exists;
  `~/.local/bin/devvm` is a linux/amd64 cross-build, refreshed by `scp`.
  Verified: `ports add web 3000` binds the exact port, HTTP flows through
  the yamux agent exec, exactly one `machine exec -i --stream` exists while
  forwards are up and none after `ports down`. Same live lists as `cloud`;
  the only environment for step 7's one-exec claim and for the smolvm
  caveats above. Only the login shell has `~/.local/bin` on `PATH`, which
  is what the proxy uses. Reachable from this devvm guest only while the
  ThinkPad's firewall allows port 22 from the LAN.
- **Guest fixture**: a listener inside the guest on port 3000. A process
  backgrounded inside one `exec` dies with that exec, so hold it in a
  detached exec of its own, on the hub:
  `nohup devvm exec web -- python3 -m http.server 3000 --bind 127.0.0.1 >/dev/null 2>&1 &`.
  The ubuntu:24.04 image has no `python3`, `nc` or `busybox`; install it
  once with `devvm exec web -- sudo apt-get install -y python3`. On
  `cloud` the same command runs on the box itself.

Below, `h` is a hub conf created in step 1's live list. Both hubs are
registered in this guest: `h` = `dev@2.29.47.196` (`cloud`, fake smol, no
KVM) and `tp` = `devvmtest@192.168.1.196` (ThinkPad, real smol). Steps 1–4
were run against both; **from step 5 on, run each live list against the
ThinkPad by default** and against `cloud` only where an item needs
`calls.log` to show the exact smolvm calls or names `cloud` as the
remote-managed target. Where a hub-side command is named, run it on the
hub as that user. Ports quoted as `3001` are the fake's bump and read
`3000` on the ThinkPad.

### Per step

**1. Hub backend + `HUB/NAME`**

- Unit: a `backend = "hub"` conf with `[machines.NAME]` tables loads,
  validates, round-trips through `Save`; `Validate` rejects a hub with
  `ports`/`memory`; `resolve("h/web")` yields a hub record and never
  touches the network; runtime identifier `h@web` for socket/log/lock and
  `h/web` back for display; `listMachines` unions local confs, hub tables,
  the cache, and live `run/h@*.sock`; every shaping verb refuses a hub with
  "NAME is a hub, not a machine"; version floor: `v0.0.1` refused, `dev`
  and a newer tag accepted with a warning.
- Live: `devvm create --backend hub h --ssh-host dev@2.29.47.196 --yes`
  test-connects and writes `machines/h.toml`; `devvm status` shows
  `h  hub  reachable`; `devvm bootstrap h`, `keys list h`, `repos list h`
  refuse; replace the hub's `devvm` with a script printing
  `devvm version v0.0.1` and `create` is refused, restore it; `delete h`
  removes the file.

**2. Proxy**

- Unit: the argv builder from a parsed cobra command: `--config-dir` is
  never emitted, only explicitly set leaf flags are, `HUB/` is stripped
  from the first positional only, everything after `--` in `exec` is
  byte-identical, `keys add h/web FILE` and a bare `keys add h/web` emit
  inline key lines read on the laptop (never the path), `repos add h/web`
  with no REPO emits the laptop cwd's origin, `-t` iff stdin is a TTY,
  `DEVVM_NO_SUBSCRIBE=1` is the prefix, shellJoin survives spaces, quotes
  and `$`.
- Live: `devvm exec h/web -- sh -c 'id; echo $SMOLVM_GUEST'` prints `dev`
  and `1`; `devvm exec h/web -- echo --config-dir x` prints it literally;
  `devvm exec h/web -- echo x </dev/null` prints exactly `x` (no `-t`
  noise; a one-shot smol exec has no stdin, hub §4, so nothing is piped);
  `keys add h/web ~/.ssh/id_ed25519.pub` then `keys list h/web` shows the
  laptop key's fingerprint;
  `devvm stop h/web` then `status` shows `stopped`, `start h/web` shows
  `running` (the fake's state file); `devvm create h/api --backend smol
  --yes -m 512 -d 1` writes `api.toml` on the hub, `delete h/api` removes
  it; `keys list h/web` and `repos list h/web` proxy; `attach h/web` lands
  in tmux `dev`, `shell h/web` in a login shell (both manual, need a TTY;
  `create h/api` without `--yes` shows the huh form through `ssh -t`).

**3. Merged listing + watch**

- Unit: `status --plain --local` skips hub confs and dials no hub; the
  listing argv sent to a hub carries `--local`; row merge takes state and
  backend from the hub row and the forwards column from the laptop daemon;
  the hub's own row; cache
  write/read in `cache/`, never under `machines/` or `run/`; a hub whose
  ssh exits non-zero, times out, or hangs (fake `ssh` that sleeps) renders
  cached rows `unreachable` within the deadline; `--plain` rows for hub
  machines carry `h/web` in column 1 and nothing new.
- Live: `devvm status` shows `h/web` and `h`; `status --plain` first
  column is `h/web`; `cache/hub-h.list` exists; a second hub conf `h2` at
  an unroutable address (`10.255.255.1`) makes `time devvm status` return
  in about 2s with `h2  hub  unreachable`; on the hub replace `devvm` with
  `sleep 60` and `time devvm status` returns within the overall deadline
  with `h/web … unreachable`, restore; on the hub register a hub conf at
  `10.255.255.1` and the laptop's `time devvm status` is not slowed by it
  (`--local`), remove it; `devvm status --plain --watch` in
  the background, then `devvm stop h/web` re-emits a `stopped` row, on the
  hub `pkill -u dev -f 'status --plain --local --watch'` and the watcher shows
  `unreachable` then recovers on its own, `create --backend hub h3 …`
  while watching opens a pipe for `h3` without a restart; `devvm
  __complete attach h/` lists `web`. Swift: the menu groups rows under
  `h/`, renders `unreachable`, shows the hub row.

**4. cp over tar stdio**

- Unit: `cp-in --from-tar -` and `cp-out --to-tar -` round-trip through
  the existing archive code; the marker-line reader discards arbitrary
  junk before `devvm-tar-v1\n` and rejects a stream that never shows it;
  the hostile-archive guards still apply to `cp-out`.
- Live: `devvm cp-in h/web ./f /home/dev/f` then `exec h/web -- cat
  /home/dev/f`; a directory with `-r`; `cp-out h/web /home/dev/f ./` and
  `cmp`; without `-f` the second `cp-in` refuses; add `. ~/.profile; echo
  hello` to the hub's `~/.bash_profile` (a bare `echo` shadows `~/.profile`
  and loses `~/.local/bin`), `cp-out` is still byte-identical, remove it;
  `cp-in h/web … </dev/null` (the menubar's non-TTY path) prints its `->`
  lines on the laptop only; a `cp-in` killed while the archive is still
  streaming creates nothing on the hub (killed after the stream is fully
  written, the hub finishes the copy: no pty, no SIGINT propagation);
  large `cp-out` from a real smol VM is blocked by the 11 MiB smolvm
  streaming cap (Progress, known issues).

*Milestone A is done: every live list above passed against both hubs on
2026-09-15.*

**5. Daemon ownership + sessions** (mostly unit; `fakeTransport` binds real
loopback listeners, so conflicts and dual-stack are real)

- Unit: one forward with `conf`+`connection`+`ttl` owners closes only when
  the last owner drops; `ports rm` drops `conf`, or `ttl` on an
  unconfigured port, never `connection`; `ports down` with a session open
  drops `conf` and leaves the daemon; idle rule `forwards==0 &&
  sessions==0` in `loop()` and `reconnect()`; a `session` connection with
  no forwards holds the daemon past the 60s idle; `subscribe` is acked
  only after registration, most-recent wins, disconnect falls through to
  the next; re-subscribe moves to the front; every request/event carries
  an `id` and a test interleaves an event and an `add` reply on one
  connection with a single reader on each side; a late reply for an
  unknown `id` is dropped; transport death keeps local sessions and
  `restore()` re-binds their forwards; `exact` is
  sticky through reuse and owner expiry; `restore()` leaves an exact
  forward pending when its port is held and never bumps it; the ticker
  re-binds it once the port is free; the session client reconnects and
  re-subscribes after the daemon's socket goes away and comes back; every
  forward binds `127.0.0.1` and, where the host has it, `::1`, and a held
  `[::1]:P` is tolerated; `up:N` counts `conf` only and `up:0` is never
  emitted.
- Live (`cloud` as the ssh backend, and the same on the hub against
  `web`): `ports add cloud 3000`, `ss -ltn` on the laptop shows
  `127.0.0.1:3000` and `[::1]:3000` under one `-O cancel`; `ports list`
  shows owner kind; `ports down cloud` stops the daemon when nothing else
  holds it; hold a `session` with a scratch client and `ports down` leaves
  it.

**6. Hub forwards**

- Unit: an in-process hub daemon on `fakeTransport`, a `__session` over
  `os.Pipe`, and a laptop daemon whose `hubTransport` is handed that pipe
  with a fake `-L`: `add` returns the hub port and the laptop binds only
  after it; `remove`; killing the `__session` end drops every forward it
  owned on the hub; a `session {relay: true}` is closed on hub transport
  death while a local session on the same daemon survives it; the
  laptop goes `reconnecting`, re-adds, and picks up a *changed* hub port
  (hold the old one with a listener during the outage); a `session {relay:
  true}` opened while the hub transport is down is refused, the laptop
  stays `reconnecting` and comes up on its next retry after `restore()`; a
  `pending` reply leaves the laptop forward pending; two concurrent `ports
  add` for different machines on one hub conf both land (`flock`);
  `listMachines` sees `run/h@web.sock`.
- Live: start the guest fixture; `devvm ports add h/web 3000` on the
  laptop, `curl localhost:3000` on the laptop is 200 and the hub's `ports
  list web` shows the same guest port on `3001` with a `connection` owner
  and no `conf`; `h.toml` has `[machines.web] ports = ["3000"]`;
  `run/h@web.sock` exists; on the hub `ports rm web 3000` leaves the
  `connection`-held forward up (refusing only when the hub conf does not
  list 3000) and
  `ports down web` leaves the daemon; hub `devvm stop web` puts the laptop
  in `reconnecting` and `calls.log` shows no `machine exec` storm and the
  state file stays `stopped`; hub `start web` brings the laptop's 3000
  back; hub `pkill -f '__daemon web'` (the `update` case) and laptop `ssh
  -O exit` on the master (the sleep case) both reconnect; hub-side
  transport loss: `pkill -f 'devvm-agent serve'` on the hub while a
  listener sits on hub 3001, the hub re-binds on 3002 and laptop 3000
  still answers; `ports rm h/web 3000` clears both sides; with nothing
  held the laptop daemon exits first and the hub's after; `delete h`
  refuses while `[machines.web]` or `run/h@*.sock` exists and `--force`
  cleans up; `start h/web` brings laptop forwards up; laptop `status
  --plain` shows `h/web	smol	running	up:1`. ThinkPad: the same, plus
  a desktop `attach web` held throughout to prove one agent exec serves
  both.

*Milestone B is done when the step-6 live list passes against `cloud`.*

**7. Daemon bridge + agent reply** (needs a daemon with events: the hub's
`web` run *from the hub*, or a smol VM on the ThinkPad; a fake `xdg-open`
on the host's `PATH` logs what "opened")

- Unit: classify direct/redirect/external, including a `redirect_uri` on
  `::1` and on a non-loopback host; direct binds `connection`-owned with
  bump, redirect binds exact `ttl`-owned or replies "in use"; relay
  subscribers get the event unbound; no subscriber logs and replies
  `opened:false`; reconnecting replies "forward pending"; subscriber
  timeout and disconnect reply `opened:false`; `hostbrowser.Open` with no
  opener on `PATH`, and with an opener that fails to start, returns an
  error and the subscriber replies `opened:false` and prints the URL; a
  `redirect_uri` on a literal `[::1]` with the `::1` bind failed replies
  "unavailable on the host" instead of a bound port; rate limit, the 20-forward
  cap, and ports below 1024 refuse with a reply; `CallbackPort` no longer
  skips 1455; an old agent that closes the stream after the event does not
  hang the daemon; `agentbin.Install` writes agent and shim in one call
  and is idempotent by sha.
- Live: `devvm attach web`, inside it `devvm-open-url
  http://localhost:3000/` logs `http://localhost:3001/` on the host (guest
  and host share ports on the fake; on the ThinkPad it is `:3000`) and the
  guest prints `devvm: opened on host -> …`; `ports list web` shows the
  `connection` forward and leaving `attach` removes it; `devvm-open-url
  'https://example.com/auth?redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fcb'`
  binds exact 1455 with a TTL, and with 1455 held on the host the guest
  prints "callback port 1455 is in use"; an external URL opens verbatim;
  with no `attach` the guest prints "not opened" and `web.log` has the
  URL; rename the fake `xdg-open` away and an open inside `attach` prints
  the URL on the host terminal and "not opened on host (no browser
  opener)" in the guest, restore it; 15 opens in a minute refuse the last
  five; `pkill -f '__daemon
  web'` during the attach (the `update` case), then open again: it still
  opens, without reattaching; ThinkPad: `pgrep -f 'devvm-agent serve'`
  shows exactly one exec through all of it.

**8. Thin `auth`, smol and hub**

- Unit: the laptop daemon subscribes upstream on its first local
  subscriber and unsubscribes on the last; it acks a local `subscribe`
  only after the upstream ack; an event from `hubTransport.events()` runs
  the same handler, binds through `__session` then `-L`, and relays the
  local reply upstream; ordering across a relay is per host: laptop A,
  desktop B, laptop C subscribe, C leaves, the next event goes to A; thin
  `auth` dials, reads the shim path from `ping`, subscribes, runs the
  login table without calling `agentbin.Install`, and on a remote backend
  still takes the private-session branch.
- Live: laptop `attach h/web`, inside it `devvm-open-url
  http://localhost:3000/` logs on the **laptop** with the laptop's port and
  the guest line names that port; hold a hub-side `attach web` at the same
  time and the most recent one gets the open; `pkill -f '__daemon web'`
  on the hub mid-session, then open again: lands on the laptop; hold 1455
  on the hub and a redirect open from the laptop still works (the relay
  binds nothing there); hold 1455 on the laptop and it is refused; the hub
  daemon has exactly one subscriber while the laptop is attached
  (`DEVVM_NO_SUBSCRIBE=1` observed: the proxied `attach` never dialed);
  `auth h/web` runs `gh auth login` proxied and its URL opens on the
  laptop (cancel the login); `auth cloud` (remote backend) still works on
  the private session.

*Milestone C is done when the step-7 and step-8 live lists pass; the
one-exec claim itself is a ThinkPad check.*

**9. `BROWSER` plumbing (smol)**

- Unit: profile.d content and guard; the attach path emits
  `set-environment -t dev BROWSER=…` after the session exists and before
  `attach-session`; the smol `ExecOpts.Env` path; remote backends are
  untouched (`bootstrap cloud` writes no profile.d, `attach cloud` sends
  no `set-environment`).
- Live (on the hub, against `web`): `bootstrap web` installs agent and
  shim and `/etc/profile.d/devvm.sh`; `devvm shell web` then `echo
  $BROWSER`; `attach web`, open a new tmux window, `echo $BROWSER` is set
  there and unset in a pane that predates the attach; `exec web -- bash
  -lc 'echo $BROWSER'` (profile.d); `bootstrap cloud` and `attach cloud`
  leave `/etc/profile.d` and the tmux session environment alone.

### Only on real hardware

- macOS: `smolvm machine create` and `open` from an sshd session on the
  desktop; a daemon spawned there survives ssh logout. Needs a Mac hub.
- ThinkPad: everything marked ThinkPad above, plus `machine cp` staging in
  `/var/tmp` for `cp-in` and the `-i --stream` agent exec surviving a
  `smolvm` restart.

## Open questions carried from the design docs

- Ephemeral TTL: 10 minutes wall-clock for `redirect_uri` callbacks. Direct
  dev-server opens are `connection`-owned and last as long as the session
  that opened them, so they carry no TTL.
- Most-recent-subscriber is the whole tie-break for opens; no per-subscriber
  hints in v1.
- A per-machine `open_urls = "always"` knob to restore unattended opening is
  a ten-line escape hatch if a real case turns up; not in v1.
