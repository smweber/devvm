# Roadmap: hubs + browser bridge

Status: 2026-09-15, revised after a third review (relay subscriptions, exact
ports across restore, dual-stack semantics, one forwarding contract). This is
the single ordered plan for the two design docs in this directory. The docs
own the *what* and *why*; this file owns the *order* and what each step
ships. When they disagree, this file wins for sequencing and the design doc
wins for mechanism. Mechanism for leases, subscriptions, ownership and bind
policy lives in `browser-bridge.md` §3–4 only.

- `hub.md` (v3.1): reach another host's smol VMs as `HUB/NAME`; the hub daemon
  is the only agent-exec owner, the laptop is a client.
- `browser-bridge.md` (v2.3): the forward daemon binds ports and emits
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
   nothing else. `up:N` in `status --plain` counts `conf` only.
2. **Bind policy is a request property kept for the forward's life** (bridge
   §4): exact-or-bump, honoured by `restore()` too; every forward dual-stack,
   with an occupied `[::1]:P` counting as busy. A `redirect_uri` callback is
   exact on the host that shows the browser and nowhere else.
3. **Subscribe-to-open** (bridge §1, §3): the daemon never opens a browser
   and never rewrites a URL; `subscribe` implies `hold` and is acknowledged;
   a subscriber is local or a relay, and the daemon binds nothing for a relay;
   the hub proxy runs commands with `DEVVM_NO_SUBSCRIBE=1`. One forwarding
   contract (hub §7): clients always name guest ports.
4. **One exec per smol VM, always.** Nothing on any host spawns a second
   `devvm-agent serve` into a VM. The laptop never spawns one into a hub VM;
   `auth` stops spawning its own on smol. Remote boxes keep `auth`'s private
   session (ssh has no one-exec limit) until the ssh agent exec ships.
5. **`--plain` changes ship with their Swift change** in the same PR.

## Steps

Each step is one PR: build, tests, Opus review, then the next. "Swift" notes
what `contrib/macos` must change in the same PR, if anything.

| # | Step | From | Ships | Swift |
|---|------|------|-------|-------|
| 1 | Hub backend + `HUB/NAME` | hub §1–3 | `backend = "hub"` confs, `create --backend hub`, name parsing in `resolve`, runtime identifier `HUB@NAME` for socket/log/lock, `config.ListAll` (local + `hubs/`) for status/update/`ports list`/completion, `delete` cleanup of `hubs/`, shaping verbs refuse hubs, exact-version check | none |
| 2 | Proxy | hub §3 | `a.proxy` builds the remote argv from the parsed command (`$SHELL -lc`, `DEVVM_NO_SUBSCRIBE=1`, `-t` iff TTY, nothing after `--` touched); `attach`/`shell`/`exec`/lifecycle/`repos`/`keys`/`create` proxied | none |
| 3 | Merged listing + watch | hub §5 | `status` merges per-hub `--plain`: state from the hub row, forwards column from the laptop's own daemon for `HUB@NAME`; cache outside watched dirs; `--watch` holds one hub pipe per hub; completion for `HUB/` | group rows by `HUB/` prefix; `unreachable` state token |
| 4 | cp over tar stdio | hub §6 | `cp-in --from-tar -`, `cp-out --to-tar -`, laptop-side loops | drop target works for hub machines (no change if it shells out by name) |
| | **Milestone A** | | Laptop drives desktop VMs: list, attach, create, lifecycle, cp. Zero daemon changes. | |
| 5 | Daemon ownership + hold + subscribe | bridge §3–4 | owner set and `exact` on `fwd`; `hold` and acked `subscribe {relay}` (subscribe implies hold); idle rule `forwards==0 && holders==0`; `ports rm`/`ports down` per bridge §4; `add` takes exact-or-bump and `restore()` honours it; every forward dual-stack with `[::1]` in-use counting as busy; `up:N` counts `conf` only | none (count semantics only narrow) |
| 6 | Hub forwards | hub §7 | `__forward NAME GUEST` (process-as-closer), `hubTransport` on the laptop daemon (guest-port contract, re-resolves the hub port on respawn), reconnect; `start HUB/NAME` brings up laptop-configured forwards after proxying; `config.ListAll` also enumerates live `run/HUB@*.sock`; `update` cycles hub-machine daemons | forwards for hub machines appear like any other |
| | **Milestone B** | | `ports add desktop/web 3000` works from the laptop; both users can hold forwards to one VM. | |
| 7 | Daemon bridge + agent reply | bridge §1–2, §4 | kinds external/direct/redirect; `connection`-owned forwards for direct opens, `ttl` forwards for redirects (exact-port, TTL paused while reconnecting, ≤20, no ports <1024, opens rate-limited); no bind for relay subscribers; `CallbackPort` moves to the daemon minus its 1455 exclusion; event carries `{url, kind, guest?, bound?}`, subscriber reply relayed to the guest with a timeout; `open-url` becomes request/response; `attach`/`shell` subscribe; **`build.sh` + commit agent** | none |
| 8 | Thin `auth`, smol and hub | bridge §6, hub §8 | `auth` dials, subscribes, runs logins where the transport has events; the private session and `--auth` **stay** for remote backends; `__subscribe NAME` registers as a relay, prints `ready` on ack, and is re-run with backoff when it dies; proxied `attach`/`shell`/`auth` open `__subscribe` first and wait for `ready`; `auth desktop/web` and a login inside `attach desktop/web` open on the laptop | none |
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

Step 5 is independent of steps 1–4 and can be built in parallel with them.
Step 7 touches the committed agent binaries; step 8 touches them again. Do not
interleave other agent changes between them.

## Verification that needs real hardware

- Step 2: proxying from the laptop to the desktop over Tailscale, including
  the huh form in `create` through `ssh -t`.
- Step 6: `__forward` lifecycle under desktop-side `ports down` (daemon stays
  up for the laptop's forwards, desktop `status` shows `-`), `stop`, `update`,
  and laptop sleep.
- Step 8: `gh auth login` from `attach desktop/web` opens on the laptop and
  the callback lands; the same with a desktop `attach` held on the same VM;
  the same after a desktop `update` cycled the hub daemon mid-session
  (`__subscribe` re-run); with desktop port 1455 deliberately occupied.
- macOS-specific: `smolvm machine create` and `open` from an sshd session on
  the desktop; daemon spawned there survives ssh logout.

Everything else runs under `go test ./...` with a fake `ssh` on `PATH` and the
existing daemon test harness.

## Open questions carried from the design docs

- Listing cache location: `$XDG_STATE_HOME/devvm/` (proposed) vs config dir.
- Ephemeral TTL: 10 minutes for `redirect_uri` callbacks. Direct dev-server
  opens are `connection`-owned and last as long as the session that opened
  them, so they carry no TTL.
- Most-recent-subscriber is the whole tie-break for opens; no per-subscriber
  hints in v1.
- A per-machine `open_urls = "always"` knob to restore unattended opening is
  a ten-line escape hatch if a real case turns up; not in v1.
