# CLAUDE.md

Guidance for Claude Code working in this repo. Focuses on the non-obvious
constraints; read the package docs for the rest.

## What this is

`devvm` manages persistent dev boxes across three backends: local smolvm microVMs
(`smol`), a remote host devvm shapes (`remote-managed`), and an existing host it
adopts hands-off (`remote-unmanaged`). The two `remote-*` backends share one ssh
transport (`ssh.go`) and differ only in posture: `Managed()` vs adopt. Old confs
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

Go 1.23+ (`go.mod` pins the version). In some environments the toolchain is at
`/usr/local/go/bin` and not on `PATH`.

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
`~/.ssh`. The agent is now pulled in only by smol forwards and `auth`.

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
internal/cli/       cobra command tree + handlers (create's huh form in createform.go)
internal/config/    TOML machine registry (~/.config/devvm/machines/<name>.toml)
internal/backend/   Backend interface + smol.go + ssh.go (both remote-* backends)
internal/session/   per-machine forward daemon (owns the exec) + client + transports
internal/agentrpc/  yamux stream protocol (forward/rpc/event), shared host+guest
internal/agentbin/  embedded guest agent binaries (go:embed) + on-demand install
internal/keys/      authorized_keys logic (was awk); pure/text host-side, unit-tested
internal/auth/      login orchestration, URL bridge, callback-as-forward
internal/bootstrap/ prereqs (install on managed / check on adopt) + bootstrap-hook + hardening
internal/hostbrowser/ open guest login URLs on the host (sanitized)
```

## Architecture notes

- **Session daemon**: spawned via the hidden `devvm __daemon NAME` command,
  detached. Owns the smol agent exec (yamux) or the ssh ControlMaster, allocates
  host ports (bumping on conflict), and idle-exits when it has no forwards.
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
- **Auth**: the guest pushes `open-url` events over the agent channel; OAuth
  loopback callbacks are bridged as ordinary forwards (no `nc`/`curl`; codex's
  `:1455` needs no VM restart).
- **Agent install** (`internal/agentbin`): idempotent by sha256, arch-matched,
  `$HOME`-resolved (never hardcode `/home/dev`). Managed boxes get a root-owned
  `/usr/local/bin` install; adopt hosts get a **user-scoped `~/.local/bin`**
  install, gated behind explicit consent (`auth --install-agent` or a prompt) so
  devvm never writes to an adopted box unasked. `Install` returns the path to use.
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

## Releases

Tag `vX.Y.Z` and push — `.github/workflows/release.yml` builds and publishes the
host binaries. Manual equivalent: `./release.sh && gh release create vX.Y.Z
dist/devvm-* dist/SHA256SUMS`.

## Conventions

- Match the surrounding style; comments explain the *why* (the constraints
  above), as carried over from the original bash.
- End commit messages with a `Co-Authored-By:` trailer when pairing.
