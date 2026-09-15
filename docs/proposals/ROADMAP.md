# Roadmap: hubs + browser bridge

Status: 2026-09-15, revised after a fourth review (one `__session` per hub
machine held by the laptop daemon, the laptop daemon as the relay, no `hold`
op, hub-machine state in the hub conf, best-effort `::1`, `up:0` never
emitted). This is
the single ordered plan for the two design docs in this directory. The docs
own the *what* and *why*; this file owns the *order* and what each step
ships. When they disagree, this file wins for sequencing and the design doc
wins for mechanism. Mechanism for leases, subscriptions, ownership and bind
policy lives in `browser-bridge.md` §3–4 only.

- `hub.md` (v3.2): reach another host's smol VMs as `HUB/NAME`; the hub daemon
  is the only agent-exec owner, the laptop is a client.
- `browser-bridge.md` (v2.4): the forward daemon binds ports and emits
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
   §4): exact-or-bump, honoured by `restore()` too; every forward dual-stack
   with `::1` best-effort. A `redirect_uri` callback is exact on the host
   that shows the browser and nowhere else.
3. **Subscribe-to-open** (bridge §1, §3): the daemon never opens a browser
   and never rewrites a URL; a long-lived control connection is a *session*
   and holds the daemon (no separate `hold` op); `subscribe` is a message on
   a session and is acknowledged; a subscriber is local or a relay, a relay
   is another daemon, and the daemon binds nothing for a relay; the hub proxy
   runs commands with `DEVVM_NO_SUBSCRIBE=1`, which skips the daemon. One
   forwarding contract (hub §7): clients always name guest ports.
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
| 1 | Hub backend + `HUB/NAME` | hub §1–3 | `backend = "hub"` confs with `[machines.NAME]` tables, `create --backend hub`, name parsing in `resolve`, runtime identifier `HUB@NAME` for socket/log/lock, `config.ListAll` (local + hub-conf tables) for status/update/`ports list`/completion, `delete` drops the table, shaping verbs refuse the hub itself, minimum-version check with a warning on other mismatches | none |
| 2 | Proxy | hub §3 | `a.proxy` builds the remote argv from the parsed command (`$SHELL -lc`, `DEVVM_NO_SUBSCRIBE=1`, `-t` iff TTY, nothing after `--` touched); `attach`/`shell`/`exec`/lifecycle/`repos`/`keys`/`create` proxied | none |
| 3 | Merged listing + watch | hub §5 | `status` merges per-hub `--plain`: state and backend from the hub row, forwards column from the laptop's own daemon for `HUB@NAME`; a row for the hub itself (`hub` group); 2s connect timeout for the listing; cache in `cache/` outside watched dirs, `ListAll` reads it; `--watch` holds one hub pipe per hub; completion for `HUB/` | group rows by `HUB/` prefix; `unreachable` state token; `hub` backend rows |
| 4 | cp over tar stdio | hub §6 | `cp-in --from-tar -`, `cp-out --to-tar -` (marker line before the stream), laptop-side loops | drop target works for hub machines (no change if it shells out by name) |
| | **Milestone A** | | Laptop drives desktop VMs: list, attach, create, lifecycle, cp. Zero daemon changes. | |
| 5 | Daemon ownership + sessions | bridge §3–4 | owner set and `exact` on `fwd`; `session` connections (long-lived; `add`/`remove` owned by the connection, acked `subscribe {relay}`/`unsubscribe`, re-subscribe moves to front); idle rule `forwards==0 && sessions==0`; `ports rm`/`ports down` per bridge §4; `restartDaemons` cycles every up daemon; `add` takes exact-or-bump and `restore()` honours it; every forward dual-stack with `::1` best-effort (ssh: one `localhost:` spec); `up:N` counts `conf` only, never `up:0` | none (`down`/`-` already parsed) |
| 6 | Hub forwards | hub §7 | `__session NAME` (one per hub machine: marker line, then `add`/`remove` JSON lines, forwards owned by its connection), `hubTransport` on the laptop daemon (one `__session` on its master; guest-port contract; `dead()` on master or process death; re-resolves the hub port in `restore()`); `start HUB/NAME` brings up laptop-configured forwards after proxying; `config.ListAll` also enumerates live `run/HUB@*.sock`; `update` cycles hub-machine daemons | forwards for hub machines appear like any other |
| | **Milestone B** | | `ports add desktop/web 3000` works from the laptop; both users can hold forwards to one VM. | |
| 7 | Daemon bridge + agent reply | bridge §1–2, §4 | `events()` on the transport interface; kinds external/direct/redirect; `connection`-owned forwards for direct opens, `ttl` forwards for redirects (exact-port, wall-clock TTL, ≤20, no ports <1024, opens rate-limited); no bind for relay subscribers; `CallbackPort` moves to the daemon minus its 1455 exclusion; event carries `{url, kind, guest?, bound?}`, subscriber reply relayed to the guest with a timeout; `open-url` becomes request/response; `attach`/`shell` open a session and subscribe; **`build.sh` + commit agent** | none |
| 8 | Thin `auth`, smol and hub | bridge §6, hub §8 | `auth` dials, subscribes, runs logins where the transport has events; the private session and `--auth` **stay** for remote backends; `__session` gains `subscribe {relay:true}`/`unsubscribe` and streams events/replies; `hubTransport.events()` is fed by them; the laptop daemon subscribes on the hub while it has local subscribers and re-subscribes on each new one; `attach`/`shell`/`auth desktop/web` = dial the laptop daemon, subscribe, proxy with `DEVVM_NO_SUBSCRIBE=1`; `hubBackend.Run` carries `BROWSER` in the proxied exec argv; agent/shim install skipped for hub machines | none |
| | **Milestone C** | | One exec per smol VM in every flow. Any browser open from any smol or hub devvm session lands on the host you are sitting at. | |
| 9 | `BROWSER` plumbing | bridge §5 | agent+shim installed at `bootstrap` on managed boxes; `profile.d` (guarded); tmux `new-session -e`; smol `Env`; README | none |

Deferred, not scheduled: the ssh agent exec for remote boxes (bridge §1,
"Remote"), and with it thin remote `auth` and the deletion of `--auth` and the
second agent socket. Hub VMs do not need it; Hetzner boxes are the only
remaining case, and their `auth` keeps working on the private session it has
today.

## Dependencies

```
1 → 2 → 3 → 4          (Milestone A; no daemon work)
1, 5 → 6               (Milestone B; 5 is the only shared daemon change;
                        6 needs 1's hub backend and runtime identifier)
5 → 7 → 8              (Milestone C; 8 also needs 2 for the proxy and 6 for
                        the laptop-side callback forwards)
8 → 9
```

Step 5 is independent of steps 1–4 and can be built in parallel with them,
or first: it is the riskiest daemon surgery and everything after it builds
on it. Step 7 touches the committed agent binaries; step 8 does not (the
relay is host-side). Do not interleave other agent changes around step 7.

## Verification that needs real hardware

- Step 2: proxying from the laptop to the desktop over Tailscale, including
  the huh form in `create` through `ssh -t`.
- Step 3: `devvm status` with the desktop asleep returns within the short
  timeout and shows `unreachable`.
- Step 4: `cp-out` from a hub whose login shell echoes in its profile.
- Step 6: `__session` lifecycle under desktop-side `ports down` (daemon stays
  up for the laptop's forwards, desktop `status --plain` shows `down`),
  `stop` (no boot loop: the laptop's reconnect must not start the VM),
  `update`, laptop sleep, and the idle order (laptop daemon idles first, hub
  daemon after).
- Step 8: `gh auth login` from `attach desktop/web` opens on the laptop and
  the callback lands; the same with a desktop `attach` held on the same VM
  (most recent wins); the same after a desktop `update` cycled the hub daemon
  mid-session (laptop daemon reconnects and re-subscribes); with desktop port
  1455 deliberately occupied.
- macOS-specific: `smolvm machine create` and `open` from an sshd session on
  the desktop; daemon spawned there survives ssh logout.

Everything else runs under `go test ./...` with a fake `ssh` on `PATH` and the
existing daemon test harness.

## Open questions carried from the design docs

- Ephemeral TTL: 10 minutes wall-clock for `redirect_uri` callbacks. Direct
  dev-server opens are `connection`-owned and last as long as the session
  that opened them, so they carry no TTL.
- Most-recent-subscriber is the whole tie-break for opens; no per-subscriber
  hints in v1.
- A per-machine `open_urls = "always"` knob to restore unattended opening is
  a ten-line escape hatch if a real case turns up; not in v1.
