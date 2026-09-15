# Roadmap: hubs + browser bridge

Status: 2026-09-15, revised after a fifth review (ids and one reader on
session connections, `relay` declared at session open, one session client
with reconnect, relay sessions closed on transport loss, sticky `exact`,
pending exact binds retried on the ticker, agent+shim as one install run
by the smol transport, listing under `BatchMode` and a deadline; **two
delivery tracks**, hub and bridge, joined at step 8, with the relay-only
rules moved from step 5 to step 6; a per-step Testing section). This is
the single ordered plan for the two design docs in this directory. The docs
own the *what* and *why*; this file owns the *order* and what each step
ships. When they disagree, this file wins for sequencing and the design doc
wins for mechanism. Mechanism for leases, subscriptions, ownership and bind
policy lives in `browser-bridge.md` §3–4 only.

- `hub.md` (v3.3): reach another host's smol VMs as `HUB/NAME`; the hub daemon
  is the only agent-exec owner, the laptop is a client.
- `browser-bridge.md` (v2.5): the forward daemon binds ports and emits
  guest→host URL events; a subscribed client opens the browser; `auth` stops
  running its own agent exec on smol.

Already shipped and assumed here: daemon reconnect, `status --plain --watch`,
`devvm update`, the macOS menu bar app (`contrib/macos`), v0.1.11.

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
   acknowledged (a relaying daemon acks after its upstream ack); one session
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
| 2 | Proxy | hub §3 | `a.proxy` builds the remote argv from the parsed command (`$SHELL -lc`, `DEVVM_NO_SUBSCRIBE=1`, `-t` iff TTY, nothing after `--` touched); `attach`/`shell`/`exec`/lifecycle/`repos`/`keys`/`create` proxied | none |
| 3 | Merged listing + watch | hub §5 | `status` merges per-hub `--plain`: state and backend from the hub row, forwards column from the laptop's own daemon for `HUB@NAME`; a row for the hub itself (`hub` group); the listing runs with a 2s connect timeout, `BatchMode=yes` and an overall deadline; cache in `cache/` outside watched dirs, `listMachines` reads it; `--watch` holds one hub pipe per hub, re-spawns it with backoff on EOF, re-reads hub confs on fsnotify; completion for `HUB/` | group rows by `HUB/` prefix; `unreachable` state token; `hub` backend rows |
| 4 | cp over tar stdio | hub §6 | `cp-in --from-tar -`, `cp-out --to-tar -` (marker line before the stream), laptop-side loops | drop target works for hub machines (no change if it shells out by name) |
| | **Milestone A** | | Laptop drives desktop VMs: list, attach, create, lifecycle, cp. Zero daemon changes. | |
| 5 | Daemon ownership + sessions | bridge §3–4 | owner set and `exact` on `fwd`; `session` connections (long-lived; `id` on every request/event, one reader per side, serialized writes; `add`/`remove` owned by the connection, acked `subscribe`/`unsubscribe`, re-subscribe moves to front; no hub vocabulary yet: `relay` arrives in step 6); the session client in `internal/session` (dial, session, acked subscribe, reconnect with backoff); idle rule `forwards==0 && sessions==0`; `ports rm`/`ports down` per bridge §4; `restartDaemons` cycles every up daemon; `add` takes exact-or-bump, `restore()` honours it, `exact` sticky on reuse, a ticker retries pending exact binds (the same ticker expires `ttl` owners in step 7); every forward dual-stack with `::1` best-effort (ssh: one `localhost:` spec, IPv4 pre-probe decides busy); `up:N` counts `conf` only, never `up:0` | none (`down`/`-` already parsed) |
| 6 | Hub forwards | hub §7 | `session {relay}` on the daemon (declared at open; the daemon closes relay sessions when its transport dies, bridge §3), `__session NAME` (one per hub machine: marker line, then a `session {relay:true}` relayed verbatim, forwards owned by its connection), `hubTransport` on the laptop daemon (one `__session` on its master; guest-port contract; `-L` to `127.0.0.1:hubPort`; a `pending` add reply is a bind failure; up only after hub `add` and `-L` both hold; `dead()` on master or process death; re-resolves the hub port in `restore()`); `start HUB/NAME` brings up laptop-configured forwards after proxying; `listMachines` also enumerates live `run/HUB@*.sock`; `update` cycles hub-machine daemons | forwards for hub machines appear like any other |
| | **Milestone B** | | `ports add desktop/web 3000` works from the laptop; both users can hold forwards to one VM. | |
| 7 | Daemon bridge + agent reply | bridge §1–2, §4 | `events()` on the transport interface; kinds external/direct/redirect; `connection`-owned forwards for direct opens, `ttl` forwards for redirects (exact-port, wall-clock TTL, ≤20, no ports <1024, opens rate-limited); no bind for relay subscribers (a no-op until step 6 lands); `CallbackPort` moves to the daemon minus its 1455 exclusion; event carries `{url, kind, guest?, bound?}`, subscriber reply relayed to the guest with a timeout; `open-url` becomes request/response; agent+shim become one `agentbin.Install` op, run by the smol transport before it spawns the agent exec; `attach`/`shell` open a session and subscribe through the session client; **`build.sh` + commit agent** | none |
| 8 | Thin `auth`, smol and hub | bridge §6, hub §8 | `auth` dials, subscribes, runs logins where the transport has events; the private session and `--auth` **stay** for remote backends; `__session` carries `subscribe`/`unsubscribe` and streams events/replies; `hubTransport.events()` is fed by them; the laptop daemon subscribes on the hub while it has local subscribers, re-subscribes on each new one, and acks a local subscribe only after the hub's ack; `attach`/`shell`/`auth desktop/web` = dial the laptop daemon, subscribe, proxy with `DEVVM_NO_SUBSCRIBE=1`; `hubBackend.Run` carries `BROWSER` in the proxied exec argv; agent/shim install skipped for hub machines | none |
| | **Milestone C** | | One exec per smol VM in every flow. Any browser open from any smol or hub devvm session lands on the host you are sitting at. | |
| 9 | `BROWSER` plumbing | bridge §5 | agent+shim installed at `bootstrap` on managed boxes; `profile.d` (guarded); tmux `new-session -e`; smol `Env`; README | none |

Deferred, not scheduled: the ssh agent exec for remote boxes (bridge §1,
"Remote"), and with it thin remote `auth` and the deletion of `--auth` and the
second agent socket. Hub VMs do not need it; Hetzner boxes are the only
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
  `GOOS=linux GOARCH=amd64 go build -o /tmp/devvm ./cmd/devvm && devvm cp-in -f cloud -t /home/dev/.local/bin /tmp/devvm`.
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

Below, `h` is a hub conf created in step 1's live list. Run each live
list against both hubs: `--ssh-host dev@2.29.47.196` (`cloud`, fake smol,
no KVM) and `--ssh-host devvmtest@192.168.1.196` (ThinkPad, real smol).
Where a hub-side command is named, run it on the hub as that user. Ports
quoted as `3001` are the fake's bump and read `3000` on the ThinkPad.

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
  from the first positional only, everything after `--` in `exec` and
  `keys add` is byte-identical, `-t` iff stdin is a TTY,
  `DEVVM_NO_SUBSCRIBE=1` is the prefix, shellJoin survives spaces, quotes
  and `$`.
- Live: `devvm exec h/web -- sh -c 'id; echo $SMOLVM_GUEST'` prints `dev`
  and `1`; `devvm exec h/web -- echo --config-dir x` prints it literally;
  `printf x | devvm exec h/web -- cat` prints exactly `x` (no `-t` noise);
  `devvm stop h/web` then `status` shows `stopped`, `start h/web` shows
  `running` (the fake's state file); `devvm create h/api --backend smol
  --yes -m 512 -d 1` writes `api.toml` on the hub, `delete h/api` removes
  it; `keys list h/web` and `repos list h/web` proxy; `attach h/web` lands
  in tmux `dev`, `shell h/web` in a login shell (both manual, need a TTY;
  `create h/api` without `--yes` shows the huh form through `ssh -t`).

**3. Merged listing + watch**

- Unit: row merge takes state and backend from the hub row and the
  forwards column from the laptop daemon; the hub's own row; cache
  write/read in `cache/`, never under `machines/` or `run/`; a hub whose
  ssh exits non-zero, times out, or hangs (fake `ssh` that sleeps) renders
  cached rows `unreachable` within the deadline; `--plain` rows for hub
  machines carry `h/web` in column 1 and nothing new.
- Live: `devvm status` shows `h/web` and `h`; `status --plain` first
  column is `h/web`; `cache/hub-h.list` exists; a second hub conf `h2` at
  an unroutable address (`10.255.255.1`) makes `time devvm status` return
  in about 2s with `h2  hub  unreachable`; on the hub replace `devvm` with
  `sleep 60` and `time devvm status` returns within the overall deadline
  with `h/web … unreachable`, restore; `devvm status --plain --watch` in
  the background, then `devvm stop h/web` re-emits a `stopped` row, on the
  hub `pkill -u dev -f 'status --plain --watch'` and the watcher shows
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
  `cmp`; without `-f` the second `cp-in` refuses; add `echo hello` to the
  hub's `~/.bash_profile`, `cp-out` is still byte-identical, remove it;
  `cp-in h/web … </dev/null` (the menubar's non-TTY path) prints its `->`
  lines on the laptop only.

*Milestone A is done when every live list above has passed against
`cloud`.*

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
  (hold the old one with a listener during the outage); a `pending` reply
  leaves the laptop forward pending; `listMachines` sees `run/h@web.sock`.
- Live: start the guest fixture; `devvm ports add h/web 3000` on the
  laptop, `curl localhost:3000` on the laptop is 200 and the hub's `ports
  list web` shows the same guest port on `3001` with a `connection` owner
  and no `conf`; `h.toml` has `[machines.web] ports = ["3000"]`;
  `run/h@web.sock` exists; on the hub `ports rm web 3000` refuses and
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
  timeout and disconnect reply `opened:false`; rate limit, the 20-forward
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
  URL; 15 opens in a minute refuse the last five; `pkill -f '__daemon
  web'` during the attach (the `update` case), then open again: it still
  opens, without reattaching; ThinkPad: `pgrep -f 'devvm-agent serve'`
  shows exactly one exec through all of it.

**8. Thin `auth`, smol and hub**

- Unit: the laptop daemon subscribes upstream on its first local
  subscriber and unsubscribes on the last; it acks a local `subscribe`
  only after the upstream ack; an event from `hubTransport.events()` runs
  the same handler, binds through `__session` then `-L`, and relays the
  local reply upstream; thin `auth` dials, subscribes, runs the login
  table, and on a remote backend still takes the private-session branch.
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

**9. `BROWSER` plumbing**

- Unit: profile.d content and guard; tmux `-e` argv on ≥ 3.2 and the
  fallback below it; the smol `ExecOpts.Env` path; adopt hosts get no
  write.
- Live: `bootstrap cloud` installs agent and shim and
  `/etc/profile.d/devvm.sh`; `devvm shell cloud` then `echo $BROWSER`;
  `attach cloud` gets it through tmux; on the hub `bootstrap web` then
  `exec web -- bash -lc 'echo $BROWSER'`; re-register `cloud` as
  remote-unmanaged (or use `scottdev3`) and confirm nothing under `/etc`
  or `/usr/local` is written and `attach` sets `BROWSER` only if the agent
  is already installed.

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
