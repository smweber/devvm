# devvm

One frontend for persistent dev boxes, whatever the transport — local
[smolvm](https://smolmachines.com) microVMs (`smol`) or an existing SSH host
(`ssh`). A Go rewrite of the old `bin/devvm` bash script.

Module: `github.com/smweber/devvm`.

## Build

```sh
go build ./...            # everything
go test ./...             # unit tests (forwards, daemon, keys, config, auth)
go build -o ~/.local/bin/devvm ./cmd/devvm   # install the host CLI
```

`bootstrap.sh` (host profile) does the last step for you.

## Layout

```
cmd/devvm/          host CLI entrypoint
cmd/devvm-agent/    guest agent: serve (forwards+rpc+events) | open-url | keys
internal/cli/       cobra command tree
internal/config/    TOML machine registry (~/.config/devvm/machines/<name>.toml)
internal/backend/   Backend interface + smol.go + ssh.go
internal/session/   per-machine forward daemon (owns the one exec) + client
internal/agentrpc/  yamux stream protocol (forward / rpc / event) shared host+guest
internal/agentbin/  embedded, cross-compiled guest agent binaries (go:embed)
internal/keys/      authorized_keys logic (dedup / revoke / list), was awk
internal/auth/      login orchestration, URL bridge, callback-as-forward
internal/provision/ minimal prereqs + pluggable provisioner + ssh hardening
internal/hostbrowser/ open guest login URLs on the host (sanitized)
```

### The one-exec rule

smolvm has poor concurrency across separate `machine exec` sessions, so every
forward, rpc, and auth event for a machine rides a *single* persistent
`devvm-agent serve` exec, multiplexed with yamux. This is why the session daemon
exists (it owns that exec for the machine's lifetime) and why forwards are yamux
streams for smol. ssh has no such limit, so its forwards are native `-L` on a
ControlMaster; only the agent's rpc/events ride the exec. One-shot guest actions
(keys) are a single exec each, which is fine.

## Guest binaries

`internal/agentbin/bin/devvm-agent-linux-{amd64,arm64}` are cross-compiled and
committed, then `go:embed`-ed into the host binary so `go install …@latest`
yields a self-contained artifact. Regenerate after changing `cmd/devvm-agent`:

```sh
./build.sh
```

## Publishing for `go install`

`go install github.com/smweber/devvm/cmd/devvm@latest` needs this module at the
root of a `smweber/devvm` repo. From the dotfiles repo:

```sh
git subtree split --prefix=devvm -b devvm-split
# push devvm-split to a new github.com/smweber/devvm repo, then tag:
git tag v0.1.0 && git push --tags
```
