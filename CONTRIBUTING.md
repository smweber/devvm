# Contributing to devvm

How to build devvm, where things live, and the constraints the code is
built around. What devvm does and how to use it is in
[README.md](README.md); why it is shaped this way is in the design docs
under [docs/proposals](docs/proposals) (each carries "As shipped" notes, and
[ROADMAP.md](docs/proposals/ROADMAP.md) records what shipped when, the known
issues and the lessons). `CLAUDE.md` is the long form of this file, written
for coding agents.

## Build

```sh
go build ./...            # everything
go test ./...             # unit tests; no smolvm or ssh host needed
gofmt -l .
./install-dev.sh              # install the host CLI to ~/.local/bin, version-stamped
```

The build toolchain is the exact Go version in `.go-version` (read by `mise`
and by CI, so the committed guest agents are byte-reproducible); `go.mod`'s
`go` line is only the minimum the module compiles with.

`install.sh` is the other thing: the installer users pipe from curl, which
downloads the latest release.

`install-dev.sh` stamps the version from `git describe`; a bare `go build` yields
a `dev` build that `devvm update` and `devvm menubar` refuse (they can't tell
what release it is). After the first install, `devvm update` keeps a release
binary current.

## Layout

```
cmd/devvm/          host CLI entrypoint
cmd/devvm-agent/    guest agent: serve (forwards+events+time) | open-url
internal/cli/       cobra command tree (+ create's huh form, the `config` noun)
internal/config/    the devvm.toml loader + its two table edits, templates, host-state paths
internal/backend/   Backend interface + smol.go + ssh.go (remote boxes and the hop to a hub)
internal/session/   per-machine forward daemon (owns the one exec) + client
internal/agentrpc/  yamux stream protocol (forward / event / time) shared host+guest
internal/agentbin/  embedded, cross-compiled guest agent binaries (go:embed)
internal/keys/      authorized_keys logic (merge / revoke / list), host-side, was awk
internal/reach/     the reach cache: a remote box's last probe (ssh -O check, else a TCP connect)
internal/provider/  cloud providers: the Provider interface, Hetzner (plain REST), a fake
internal/bootstrap/ the guest steps of `setup`: user, prereqs, the template hook, hardening
internal/hostbrowser/ open guest login URLs on the host (sanitized)
contrib/macos/      the menu bar app (Swift), a thin shell over the CLI
contrib/fakesmol/   a fake `smolvm` for testing a hub on a host without KVM
docs/proposals/     the design docs, each with "As shipped" notes, and ROADMAP.md
```

## The one-exec rule

smolvm (1.16.1, where this was learned) has poor concurrency across separate
`machine exec` sessions, so every forward, clock sync and browser event for a
machine rides a *single* persistent `devvm-agent serve` exec, multiplexed
with yamux. This is why the session daemon
exists (it owns that exec for the machine's lifetime) and why forwards are yamux
streams for smol. ssh has no such limit, so its forwards are native `-L` on a
ControlMaster; only the agent's browser events ride its exec, which runs on
the daemon's own ControlMaster. authorized_keys
management runs host-side (`internal/keys` over one plain exec), so it needs no
agent — which is what lets `keys` work on any entry with zero footprint.

## Guest binaries

`internal/agentbin/bin/devvm-agent-linux-{amd64,arm64}` are cross-compiled and
committed, then `go:embed`-ed into the host binary so `go install …@latest`
yields a self-contained artifact. Guests must be **Linux**: anything needing the
agent (smol forwards, browser opens) fails early with a clear error on other OSes,
while agent-free commands (`shell`, `attach`, `exec`, `keys`, ssh forwards)
work on any remote with an sshd. Regenerate after changing `cmd/devvm-agent`:

```sh
./build.sh
```

## Releases

Tag `vX.Y.Z` and push the tag: `.github/workflows/release.yml` builds
`devvm-{darwin,linux}-{arm64,amd64}` and `SHA256SUMS` as a draft release,
a macOS job attaches `devvm-menubar.zip` and its `.sha256`, and the release
is published. That is what `devvm update` and `devvm menubar` download. The
workflow fails if a fresh `./build.sh` differs from the committed guest
agents.

`go install github.com/smweber/devvm/cmd/devvm@latest` works off the same
tag: it resolves the module from the repo named by its module path
(`go.mod`), and the build knows its version from the module.
