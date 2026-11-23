# devvm

One frontend for persistent dev boxes, whatever the transport — a local
[smolvm](https://smolmachines.com) microVM, a Hetzner Cloud server, or a
remote host reached over ssh. A Go rewrite of the old `bin/devvm` bash script.

Module: `github.com/smweber/devvm`.

## Backends

devvm shapes what it creates and only connects to everything else:

| backend   | what it is                                                        | where it lives                     | transport     |
| --------- | ----------------------------------------------------------------- | ---------------------------------- | ------------- |
| `smol`    | a local smolvm microVM devvm created                              | discovered by its `devvm=true` label | `smolvm exec` |
| `hetzner` | a Hetzner Cloud server devvm created                              | discovered by its `devvm=true` label | ssh / mosh    |
| `remote`  | any other box you reach over ssh                                  | an entry in `devvm.toml`           | ssh / mosh    |
| `hub`     | another host running devvm; its smol VMs are `HUB/NAME`           | an entry in `devvm.toml`           | ssh           |

`create` makes a smol VM or a server and labels it; `setup` shapes it (user,
prereqs, agent, keys and hardening on a server, this host's `gh` login if the
template shares it, your hook, repos). A smol VM
or server without the label is invisible to devvm: label it by hand to hand
it over (`smolvm machine update --name NAME --label devvm=true`, or the
provider's console). An entry is never shaped: devvm connects to it
(`attach`, `shell`, `exec`, `ports`, `cp-in`/`cp-out`, `keys`), and the only
thing it may write there beyond `~/.ssh` is its agent, when the entry says
`agent = true`.

The `smol` backend needs **smolvm 1.35.5 or newer** (devvm relies on how it
answers a second start and on uncapped `--stream` execs; nothing checks the
version).

Everything you configure is one file, `~/.config/devvm/devvm.toml`, safe to
keep in dotfiles: `[templates.*]` (how `create`/`setup` shape a box),
`[machines.*]` (remote boxes and hubs) and `[providers.*]`:

```toml
[templates.default]
hook = "url:https://example.com/bootstrap.sh --yes"
memory = 4096
repos = ["owner/repo"]
share_auth = ["github"]  # setup copies this host's gh login into the box

[machines.cloud]
backend = "remote"
host = "203.0.113.5"    # host[:port] or an ssh alias
user = "dev"            # unset: ~/.ssh/config decides
agent = true            # let devvm install its agent in ~/.local/bin (browser opens)

[machines.desk]
backend = "hub"
host = "desk.tailnet"
user = "me"
```

Add and remove entries with `devvm config add NAME --backend remote|hub --host
H[:P] [--user U]` and `devvm config rm NAME` (both touch only that table);
`devvm config edit|check|show|path` cover the rest (`agent = true` is set with
`config edit`). No `config` command connects to a box. A Hetzner project is
`[providers.hetzner]` plus `devvm providers token hetzner`; see
[docs/proposals/providers.md](docs/proposals/providers.md).

## Connecting

- `devvm create NAME` — create a local smol VM (or, with `--backend hetzner`,
  a server) and shape it from a template. Flags drive it non-interactively
  (`--template`, `--memory`, `--disk`, `--hook`, `--yes`); a terminal prompts
  (via [huh](https://github.com/charmbracelet/huh)) for whatever's unset.
  `devvm setup NAME` re-runs the shaping. A failed clone does not stop it:
  setup tries every repo and exits 3 ("setup finished, but …"). An
  `owner/repo` is cloned with the guest's `gh`. With `share_auth =
  ["github"]` in the template, setup first copies this host's `gh` login
  (`gh auth token`) into the box over the exec's stdin, unless the box
  already has a working one; that token has your login's full scopes and
  sits in the box's `~/.config/gh/hosts.yml`, cloud servers included
  (`share_auth = []` in a template opts out). An expiring login (`gh auth
  login --expiring=false` avoids one) is not copied. Without a login in
  the box, `owner/repo` specs are skipped with a hint; `gh auth login`
  inside `attach` works too. On a hub machine the login is the hub's. A remote box is `devvm
  config add`.
- `devvm attach NAME` — join the persistent dev tmux session.
- `devvm shell NAME` — a raw login shell, no tmux.
- `devvm ports add NAME PORT` (or `HOST:GUEST`) — forward a guest port to
  this host and remember it here; saved forwards come back on every
  `attach`, `shell`, `start` and `ports up` (`ports down` holds until then).
  A loopback URL a guest program opens is forwarded by itself while a
  session is attached (below); `ports add` is for ports worth keeping.
- Both take `--transport ssh|mosh` for remote machines (default from the
  entry's `transport` field); smol ignores it. (There is no separate `ssh`/`mosh` command.)
- **Browser opens.** While `attach`/`shell` is open on a machine, a
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
  the URL instead. Logins work the same way: `gh auth login` or `codex
  login` inside `attach` opens on this host. On a box reached over ssh the
  machine's forward daemon runs the agent over its own ssh connection; a
  server devvm made gets agent, shim and profile.d at `setup`, and an entry
  gets the agent and shim in `~/.local/bin` from its daemon only when it
  says `agent = true` (a key added later counts once the daemon restarts:
  `devvm ports down NAME`). On an entry `BROWSER` comes from `attach`'s tmux
  session; a bare `shell` is your login shell untouched (export
  `BROWSER=~/.local/bin/devvm-open-url` yourself if you want it there).
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
installs go through `install.sh`. If the menu
bar app is installed, `update` brings it to the same version too.

## Upgrading to v0.2.0

v0.2.0 carries the simplification in
[docs/proposals/simplify.md](docs/proposals/simplify.md): no `auth`, `repos`
or `lockdown` commands, no "managed" entries, ownership by label, and no
upgrade code for older layouts.

In this order, on the laptop and on every hub:

1. A host still on the layout from before v0.1.16
   (`~/.config/devvm/machines/`) first goes through v0.1.21 (`devvm update
   --version v0.1.21`, then any command once) and still writes `devvm.toml`
   by hand ([config.md §8](docs/proposals/config.md)): only v0.1.16–v0.1.21
   carry the upgrade seeds and the pointer line.
2. Stop every VM, then upgrade smolvm (1.35.5 has `machine update
   --label`), and label each VM devvm made once: `smolvm machine update
   --name NAME --label devvm=true`. An unlabelled VM is invisible to v0.2.0:
   `devvm update` does not cycle its daemon and `devvm stop` refuses it.
3. In `devvm.toml`, delete any `managed = …` line, and any `user` or
   `ports` line in a template: each is now an error naming the fix. An
   entry that was managed and had no `user` line logged in as the
   template's user; an unset `user` now means `~/.ssh/config` decides, so
   add `user = "dev"` (or whatever that template's user was). Add `agent =
   true` to an entry whose browser opens you want. Save forwards a
   template used to carry with `devvm ports add`.
4. Update devvm on this host and on every hub together: v0.2.0 is the
   oldest hub a v0.2.0 laptop speaks to. A v0.1.x laptop proxying
   `create`/`setup` to a v0.2.0 hub fails with a misleading "hub … runs
   devvm older than v0.1.16" or a raw "unknown flag" line. If you updated
   devvm before labelling, a smol VM's daemon from the old build is still
   running: after labelling, `devvm ports down NAME && devvm ports up NAME`.
5. On a box that used to be a managed entry: ssh to it once by hand so its
   host key lands in `~/.ssh/known_hosts`, and delete
   `/etc/profile.d/devvm.sh` and the root-owned `devvm-agent` and
   `devvm-open-url` in `/usr/local/bin` there (nothing updates them now).
6. A Hetzner server devvm made carries the label already; `status` names
   how many in the project don't. `~/.local/state/devvm/managed/` is no
   longer read and can be removed.

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
go test ./...             # unit tests (forwards, daemon, keys, config, setup)
./install.sh              # install the host CLI to ~/.local/bin, version-stamped
```

`install.sh` stamps the version from `git describe`; a bare `go build` yields
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
internal/keys/      authorized_keys logic (dedup / revoke / list), host-side, was awk
internal/provider/  cloud providers: the Provider interface, Hetzner (plain REST), a fake
internal/bootstrap/ the guest steps of `setup`: user, prereqs, the template hook, hardening
internal/hostbrowser/ open guest login URLs on the host (sanitized)
```

### The one-exec rule

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

## Publishing for `go install`

`go install github.com/smweber/devvm/cmd/devvm@latest` resolves the module from
the repo named by its module path (`go.mod`), at the repo root. Publishing is
just tagging a release:

```sh
git tag v0.1.0 && git push --tags
```
