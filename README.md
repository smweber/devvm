# devvm

One frontend for persistent dev boxes, whatever the transport — a local
[smolvm](https://smolmachines.com) microVM or a remote host reached over ssh. A
Go rewrite of the old `bin/devvm` bash script.

Module: `github.com/smweber/devvm`.

## Backends

A machine's `backend` says who owns its registry and lifecycle; `managed`
says whether devvm may shape its OS:

| backend  | what it is                                                      | transport     |
| -------- | --------------------------------------------------------------- | ------------- |
| `smol`   | a local smolvm microVM, discovered from smolvm (never configured) | `smolvm exec` |
| `remote` | a box reached over ssh, an entry in `devvm.toml`                 | ssh / mosh    |
| `hub`    | another host running devvm; its machines are `HUB/NAME`          | ssh           |

A *managed* box is devvm's to shape: it installs prereqs, can harden, and pins
known_hosts. A smol VM is managed when devvm created it (or you handed it over
with `devvm setup --adopt NAME`); a remote entry when it says `managed = true`.
An unmanaged (adopted) box is never modified: devvm only edits your own
`~/.ssh/authorized_keys` on it.

Everything you configure is one file, `~/.config/devvm/devvm.toml`, safe to
keep in dotfiles: `[templates.*]` (how `create`/`setup` shape a box) and
`[machines.*]` (remote boxes and hubs):

```toml
[templates.default]
hook = "url:https://example.com/bootstrap.sh --yes"
memory = 4096

[machines.cloud]
backend = "remote"
managed = true          # devvm shapes it; the default false means hands-off
host = "203.0.113.5"    # host[:port] or an ssh alias; user from the template

[machines.desk]
backend = "hub"
host = "desk.tailnet"
user = "me"
```

Add and remove entries with `devvm config add NAME --backend remote|hub --host
H[:P] [--user U] [--managed]` and `devvm config rm NAME` (both touch only that
table); `devvm config edit|check|show|path` cover the rest. No `config` command
connects to a box. (`hetzner`, a provider backend whose VMs are discovered from
its API, is the planned next backend.)

## Connecting

- `devvm create NAME` — create a local smol VM and shape it from a template.
  Flags drive it non-interactively (`--template`, `--memory`, `--disk`,
  `--hook`, `--yes`); a terminal prompts (via
  [huh](https://github.com/charmbracelet/huh)) for whatever's unset. A remote
  box is `devvm config add` then, for a managed one, `devvm setup NAME`.
- `devvm attach NAME` — join the persistent dev tmux session.
- `devvm shell NAME` — a raw login shell, no tmux.
- Both take `--transport ssh|mosh` for remote machines (default from the
  entry's `transport` field); smol ignores it. (There is no separate `ssh`/`mosh` command.)
- **Browser opens (smol).** While `attach`/`shell` is open on a smol machine, a
  URL a guest program opens through `$BROWSER` opens in the host's browser,
  and a loopback URL (a dev server, an OAuth callback) gets its forward
  bound first. `BROWSER` points at `/usr/local/bin/devvm-open-url`, which
  `setup` installs with the agent; `shell` and `attach` set it, `attach`
  also in the `dev` tmux session's environment (new windows and panes get
  it; a shell started before `setup` installed the shim needs `export
  BROWSER=/usr/local/bin/devvm-open-url` once), and
  `/etc/profile.d/devvm.sh` sets it for other sh/bash login shells (cron,
  and every `devvm exec`, which runs under a login shell) unless `BROWSER` is already set or the shim is
  missing. zsh and fish don't read profile.d: they get it inside `attach`,
  or set it yourself. When no devvm on the host answers, the shim prints
  the URL instead. Remote machines are not wired up yet: there `BROWSER` is
  left alone and only `auth` bridges its own logins.
- `devvm cp-in NAME SOURCE [DEST]` — copy a local file into a machine; add `-r`
  for directories. DEST is a guest path, relative to the login user's home
  unless absolute, and defaults to the home directory. A DEST ending in `/`
  is a directory to copy into, created if missing. Several sources take
  `-t DIR`. Existing files are never overwritten unless `-f` is given; a copy
  that would overwrite anything is refused before a single file is written.
  Copies run as the normal guest user on both smol and remote backends and
  need only `tar` in the guest (the upload archive is built in Go).
  With shell completion enabled, SOURCE completes local paths and DEST queries
  guest paths for registered machines (two-second timeout, no SSH password
  prompts; `DEVVM_COMPLETE_TIMEOUT=10s` raises it). Directory suggestions end
  in `/` so you can keep completing inside. Every ssh invocation uses a
  10-second connect timeout; `DEVVM_SSH_CONNECT_TIMEOUT=30` (or `30s`) raises
  it for slow hosts.
- `devvm cp-out NAME SOURCE [DEST]` — copy a guest file onto the host; add `-r`
  for directories. SOURCE is relative to the guest user's home unless absolute;
  DEST is a local path, defaulting to the current directory, with the same
  trailing-`/`, `-t`, and `-f` rules as cp-in. Source completion queries the
  guest, while destination completion uses local paths. Downloads run as the
  normal guest user and need only `tar` in the guest.

```sh
devvm cp-in myvm ./notes.txt                  # -> ~/notes.txt
devvm cp-in myvm ./notes.txt docs/            # into ~/docs, created if missing
devvm cp-in -r myvm ./project /home/dev/project
devvm cp-in myvm -t docs/ a.png b.png         # several sources
devvm cp-out myvm notes.txt                   # -> ./notes.txt
devvm cp-out -r myvm project ./backup/
```

## Updating

`devvm update` replaces the running binary with the latest GitHub release
(verified against the release's `SHA256SUMS`, installed in place with no sudo)
and then restarts every running forward daemon so none keeps executing the old
code. `--check` only reports (`--check --plain` prints
`current<TAB>latest<TAB>true|false` for scripts); `--version vX.Y.Z` pins a
release. A `dev` build has no version to compare and needs `--force`. First-time
installs still go through `install.sh` (or your bootstrap script). If the menu
bar app is installed, `update` brings it to the same version too.

## Upgrading from v0.1.15

v0.1.16 replaces the per-machine confs (`~/.config/devvm/machines/*.toml`) and
`config.toml` with one hand-edited `devvm.toml`, and moves state out of the
config dir.

1. Run any devvm command once (`devvm status` will do). It moves the state
   files, seeds your saved forwards and the managed markers of the smol VMs
   devvm made from the old confs, and prints a one-line pointer at the rest.
2. Write `~/.config/devvm/devvm.toml` by hand: the remote boxes and hubs you
   use, plus any templates. Local smol VMs need no entry; they are discovered.
   See [docs/proposals/config.md §8](docs/proposals/config.md) for the
   old-to-new mapping.
3. Remove `~/.config/devvm/machines/` and `~/.config/devvm/config.toml`.

Forward daemons started by v0.1.15 keep working where they are; `devvm update`
cycles them. A hub needs v0.1.16 too for `create HUB/NAME` and
`setup HUB/NAME`.

## Menu bar app (macOS)

`devvm menubar` installs the DevVM menu bar app into `~/Applications` at your
CLI's version and opens it (run it again any time to open it). The app is a
thin shell over the CLI: a floating drop shelf (opened from the menu) has a
tile per running machine and runs `cp-in` for files dropped on one, the menu
lists machines and their forwards (fed by
`status --plain --watch`), and each machine offers start/stop, ports up/down,
its forwarded ports, and copy in/out. It never spawns a terminal. The zip
comes from the same release as the binaries, verified against its `.sha256`
sidecar; downloads made by devvm carry no quarantine flag, so the ad-hoc
signed app opens without Gatekeeper prompts. Sources and a local build script
live in [`contrib/macos`](contrib/macos/README.md). Remove it by dragging
`DevVM.app` out of `~/Applications`.

## Build

```sh
go build ./...            # everything
go test ./...             # unit tests (forwards, daemon, keys, config, auth)
./install.sh              # install the host CLI to ~/.local/bin, version-stamped
```

`install.sh` stamps the version from `git describe`; a bare `go build` yields
a `dev` build that `devvm update` and `devvm menubar` refuse (they can't tell
what release it is). `bootstrap.sh` (host profile) installs a release binary.

## Layout

```
cmd/devvm/          host CLI entrypoint
cmd/devvm-agent/    guest agent: serve (forwards+rpc+events) | open-url
internal/cli/       cobra command tree (+ create's huh form, the `config` noun)
internal/config/    the devvm.toml loader + its two table edits, templates, host-state paths
internal/backend/   Backend interface + smol.go + ssh.go (remote boxes and the hop to a hub)
internal/session/   per-machine forward daemon (owns the one exec) + client
internal/agentrpc/  yamux stream protocol (forward / rpc / event) shared host+guest
internal/agentbin/  embedded, cross-compiled guest agent binaries (go:embed)
internal/keys/      authorized_keys logic (dedup / revoke / list), host-side, was awk
internal/auth/      login orchestration, URL bridge, callback-as-forward
internal/bootstrap/ the guest steps of `setup`: user, prereqs, the template hook, hardening
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

`go install github.com/smweber/devvm/cmd/devvm@latest` resolves the module from
the repo named by its module path (`go.mod`), at the repo root. Publishing is
just tagging a release:

```sh
git tag v0.1.0 && git push --tags
```
