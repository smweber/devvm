# devvm

One frontend for persistent dev boxes, whatever the transport — a local
[smolvm](https://smolmachines.com) microVM or a remote host reached over ssh. A
Go rewrite of the old `bin/devvm` bash script.

Module: `github.com/smweber/devvm`.

## Backends

A machine's `backend` says who owns it and how it's reached:

| backend            | what it is                                              | transport      |
| ------------------ | ------------------------------------------------------- | -------------- |
| `smol`             | a local smolvm microVM devvm creates and shapes         | `smolvm exec`  |
| `remote-managed`   | a remote host devvm shapes (installs prereqs, may harden) | ssh / mosh   |
| `remote-unmanaged` | an existing host devvm adopts hands-off (checks only)   | ssh / mosh     |

*Managed* backends (smol, remote-managed) are devvm's to shape — it installs
prereqs, can harden, and manages known_hosts. *Unmanaged* (remote-unmanaged) is
an adopted host: devvm never modifies its OS, only checks prereqs and edits the
user's own `~/.ssh/authorized_keys`. Old confs with `backend = "ssh"` (+ optional
`unmanaged`) migrate on load. (`hetzner`, an API-provisioned managed backend over
the ssh transport, is the planned next backend.)

## Connecting

- `devvm create NAME` — create/adopt any backend. Flags drive it non-interactively
  (`--backend`, `--memory`, `--ssh-host`, `--transport`, …); a terminal prompts
  (via [huh](https://github.com/charmbracelet/huh)) for whatever's unset.
- `devvm attach NAME` — join the persistent dev tmux session.
- `devvm shell NAME` — a raw login shell, no tmux.
- Both take `--transport ssh|mosh` for remote machines (default from the conf's
  `transport` field); smol ignores it. (There is no separate `ssh`/`mosh` command.)

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
cmd/devvm-agent/    guest agent: serve (forwards+rpc+events) | open-url
internal/cli/       cobra command tree (+ create's huh form)
internal/config/    TOML machine registry (~/.config/devvm/machines/<name>.toml)
internal/backend/   Backend interface + smol.go + ssh.go (both remote-* backends)
internal/session/   per-machine forward daemon (owns the one exec) + client
internal/agentrpc/  yamux stream protocol (forward / rpc / event) shared host+guest
internal/agentbin/  embedded, cross-compiled guest agent binaries (go:embed)
internal/keys/      authorized_keys logic (dedup / revoke / list), host-side, was awk
internal/auth/      login orchestration, URL bridge, callback-as-forward
internal/provision/ prereqs (install on managed / check on adopt) + provisioner + hardening
internal/hostbrowser/ open guest login URLs on the host (sanitized)
```

### The one-exec rule

smolvm has poor concurrency across separate `machine exec` sessions, so every
forward, rpc, and auth event for a machine rides a *single* persistent
`devvm-agent serve` exec, multiplexed with yamux. This is why the session daemon
exists (it owns that exec for the machine's lifetime) and why forwards are yamux
streams for smol. ssh has no such limit, so its forwards are native `-L` on a
ControlMaster; only the agent's rpc/events ride the exec. authorized_keys
management runs host-side (`internal/keys` over one plain exec), so it needs no
agent — which is what lets `keys` work on an adopt host with zero footprint.

## Guest binaries

`internal/agentbin/bin/devvm-agent-linux-{amd64,arm64}` are cross-compiled and
committed, then `go:embed`-ed into the host binary so `go install …@latest`
yields a self-contained artifact. Guests must be **Linux**: anything needing the
agent (`auth`, smol forwards) fails early with a clear error on other OSes,
while agent-free commands (`shell`, `attach`, `exec`, `keys`, ssh forwards)
work on any remote with an sshd. Regenerate after changing `cmd/devvm-agent`:

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
