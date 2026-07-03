# CLAUDE.md

Guidance for Claude Code working in this repo. Focuses on the non-obvious
constraints; read the package docs for the rest.

## What this is

`devvm` manages persistent dev boxes across two backends: local smolvm microVMs
(`smol`) and existing SSH hosts (`ssh`). It is a **host CLI** (`cmd/devvm`) plus
a **guest agent** (`cmd/devvm-agent`) the host installs into each box. It began
as a ~2,100-line bash script; comments often explain *why* a behavior exists
because it was hard-won there.

## Build & test

```sh
go build ./...
go test ./...     # everything is VM-independent — no smolvm or ssh host needed
gofmt -l .
./build.sh        # rebuild + re-embed the guest agents (see below)
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
created specifically to escape that). One-shot guest actions (`keys`) are a
single exec each, which is fine — the wall is about *parallel* execs.

ssh has no such limit: its forwards are native `ssh -L` on a ControlMaster; only
rpc/events ride the agent exec.

## Two binaries, two targets — don't conflate them

- **`devvm-agent` (guest)** runs *inside* the box, which is **always Linux**.
  `build.sh` builds `linux/{amd64,arm64}` only — never macOS. These are committed
  under `internal/agentbin/bin/` and `go:embed`-ed into the host binary.
- **`devvm` (host)** runs on the user's machine — macOS **or** Linux.
  `release.sh` cross-compiles `darwin`+`linux` × `arm64`+`amd64`.

**After changing `cmd/devvm-agent`, re-run `./build.sh` and commit the updated
`internal/agentbin/bin/*`** — otherwise the host binary embeds a stale agent.

## Layout

```
cmd/devvm/          host CLI entrypoint
cmd/devvm-agent/    guest agent: serve (forwards+rpc+events) | open-url | keys
internal/cli/       cobra command tree + handlers
internal/config/    TOML machine registry (~/.config/devvm/machines/<name>.toml)
internal/backend/   Backend interface + smol.go + ssh.go (replaces string dispatch)
internal/session/   per-machine forward daemon (owns the exec) + client + transports
internal/agentrpc/  yamux stream protocol (forward/rpc/event), shared host+guest
internal/agentbin/  embedded guest agent binaries (go:embed)
internal/keys/      authorized_keys logic (was awk); pure/text, unit-tested
internal/auth/      login orchestration, URL bridge, callback-as-forward
internal/provision/ minimal prereqs + pluggable provisioner + ssh hardening
internal/hostbrowser/ open guest login URLs on the host (sanitized)
```

## Architecture notes

- **Session daemon**: spawned via the hidden `devvm __daemon NAME` command,
  detached. Owns the smol agent exec (yamux) or the ssh ControlMaster, allocates
  host ports (bumping on conflict), and idle-exits when it has no forwards.
- **Config** is hand-editable TOML; keep it that way (don't hide state in an
  opaque DB). No legacy sourced-bash reader.
- **Keys**: `internal/keys` is pure text logic; the SHA256 fingerprint is
  computed in-Go and matches `ssh-keygen`. It runs inside the agent.
- **Auth**: the guest pushes `open-url` events over the agent channel; OAuth
  loopback callbacks are bridged as ordinary forwards (no `nc`/`curl`; codex's
  `:1455` needs no VM restart).
- **Provisioner**: `bootstrap.sh` is just the default `url:` provisioner, not a
  hard dependency — `provision` may be `url:`, `cmd:`, or `none`.

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
