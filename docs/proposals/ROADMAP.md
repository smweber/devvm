# Roadmap: hubs + browser bridge

Status: 2026-09-15, revised after a second review (auth sequencing, callback
binding, subscription handshake, hub runtime state). This is the single
ordered plan for the two design docs in this directory. The docs own the *what* and *why*; this file owns the *order*
and what each step ships. When they disagree, this file wins for sequencing
and the design doc wins for mechanism.

- `hub.md` (v3): reach another host's smol VMs as `HUB/NAME`; the hub daemon
  is the only agent-exec owner, the laptop is a client.
- `browser-bridge.md` (v2.2): the forward daemon binds ports and emits
  guest→host URL events; a subscribed client opens the browser; `auth` stops
  running its own agent exec on smol.

Already shipped and assumed here: daemon reconnect, `status --plain --watch`,
`devvm update`, the macOS menu bar app (`contrib/macos`), v0.1.11.

## Shared decisions

Both docs build on these; neither redefines them.

1. **Forward ownership.** A daemon forward has an owner set with kinds `conf`
   (`ports add`/`up`), `connection` (a held control connection; dropped when it
   closes), and `ttl` (bridge-created for OAuth callbacks; expires). The forward
   is torn down only when the set is empty. Owners decide lifetime and nothing
   else. `ports rm` drops `conf` on a configured mapping and `ttl` on an
   unconfigured guest port; `ports down` drops every `conf` owner and stops the
   daemon only if no forward and no holder remains; `ports add` on a live
   forward adds `conf` and leaves the other owners alone. `connection` owners
   are never removable from the CLI. `up:N` in `status --plain` counts
   `conf`-owned forwards only.
2. **Bind policy is separate from ownership.** A forward request names the
   guest port, the preferred host port, and whether the host port is exact or
   may bump. Every forward binds `127.0.0.1` and, best-effort, `::1`; there is
   no per-forward family choice. A `redirect_uri` callback is always exact-port
   on the host that shows the browser and is refused, with a reply the guest
   can read, if that port is busy. A direct loopback URL may bump.
3. **Subscribe-to-open.** The daemon never opens a browser and never rewrites
   a URL. `attach`, `shell`, and `auth` hold the daemon and subscribe; the most
   recent subscriber receives the original URL plus the guest port, the kind
   (direct or redirect) and the port the daemon bound, binds its own forward if
   it sits on another host, rewrites for its own port, opens, and replies. The
   daemon relays that reply to the guest, or "not opened" on timeout or with no
   subscriber. A process run by the hub proxy (`DEVVM_NO_SUBSCRIBE=1`) holds
   but never subscribes, so the laptop's subscription is the only one for that
   session rather than merely the most recent; `subscribe` is acknowledged so a
   client can wait until it is registered before starting a login.
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
| 2 | Proxy | hub §3 | `a.proxy` (`$SHELL -lc`, `DEVVM_NO_SUBSCRIBE=1`, `--config-dir` stripped, `-t` iff TTY); `attach`/`shell`/`exec`/lifecycle/`repos`/`keys`/`create` proxied; `start HUB/NAME` proxies then brings up laptop-configured forwards locally | none |
| 3 | Merged listing + watch | hub §5 | `status` merges per-hub `--plain`: state from the hub row, forwards column from the laptop's own daemon for `HUB@NAME`; cache outside watched dirs; `--watch` holds one hub pipe per hub; completion for `HUB/` | group rows by `HUB/` prefix; `unreachable` state token |
| 4 | cp over tar stdio | hub §6 | `cp-in --from-tar -`, `cp-out --to-tar -`, laptop-side loops | drop target works for hub machines (no change if it shells out by name) |
| | **Milestone A** | | Laptop drives desktop VMs: list, attach, create, lifecycle, cp. Zero daemon changes. | |
| 5 | Daemon ownership + hold + subscribe | bridge §3–4, hub §7 | owner set on `fwd`; `hold`, acked `subscribe` ops; idle rule `forwards==0 && holders==0`; `remove`/`ports down` per decision 1; `add` takes exact-or-bump; every forward dual-stack; `up:N` counts `conf` only | none (count semantics only narrow) |
| 6 | Hub forwards | hub §7 | `__forward NAME GUEST` (process-as-closer), `hubTransport` on the laptop daemon, respawn on exit, reconnect; `update` cycles hub-machine daemons | forwards for hub machines appear like any other |
| | **Milestone B** | | `ports add desktop/web 3000` works from the laptop; both users can hold forwards to one VM. | |
| 7 | Daemon bridge + agent reply | bridge §1–2, §4 | `connection`-owned forwards for direct opens, `ttl` forwards for redirects (exact-port, TTL paused while reconnecting, ≤20, no ports <1024, opens rate-limited); `CallbackPort` moves to the daemon minus its 1455 exclusion; event carries `{url, guest, kind, bound}`, subscriber reply relayed to the guest with a timeout; `open-url` becomes request/response; `attach`/`shell` hold+subscribe; **`build.sh` + commit agent** | none |
| 8 | Thin `auth`, smol and hub | bridge §6, hub §8 | `auth` dials, holds, subscribes, runs logins where the transport has events; the private session and `--auth` **stay** for remote backends; `__subscribe NAME` prints `ready` on ack; proxied `attach`/`shell`/`auth` open `__subscribe` first and wait for `ready`; `auth desktop/web` and a login inside `attach desktop/web` open on the laptop | none |
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
5 → 6                  (Milestone B; 5 is the only shared daemon change)
5 → 7 → 8              (Milestone C; 8 also needs 2 for hub auth)
8 → 9
```

Steps 1–4 and step 5 are independent and can be built in parallel if wanted.
Step 7 touches the committed agent binaries; step 8 touches them again. Do not
interleave other agent changes between them.

## Verification that needs real hardware

- Step 2: proxying from the laptop to the desktop over Tailscale, including
  the huh form in `create` through `ssh -t`.
- Step 6: `__forward` lifecycle under desktop-side `ports down` (daemon stays
  up for the laptop's forwards, desktop `status` shows `-`), `stop`, `update`,
  and laptop sleep.
- Step 8: `gh auth login` from `attach desktop/web` opens on the laptop and
  the callback lands; the same with a desktop `attach` held on the same VM.
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
