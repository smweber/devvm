# Proposal: one shared config, discovered machines, templates

Status: draft v3, 2026-10-17, revised after two independent reviews and a
design discussion with the user (see "Review log" at the end). Not scheduled.
Once agreed, the order of work goes in `ROADMAP.md`. It follows v0.1.15
(roadmap steps 1–9).

## Problem

devvm keeps one TOML file per machine (`~/.config/devvm/machines/NAME.toml`)
plus a global `config.toml` of create-time defaults. The files mix four
kinds of data with different owners and lifetimes:

| Kind | Fields today | Real source of truth |
|---|---|---|
| Reach | `backend`, `ssh_host`, `ssh_port`, `identity`, `transport`, `mosh_server` | the conf: the only data that exists nowhere else |
| Create-time spec | `memory`, `disk`, `bootstrap_hook`, `authorized_keys*`, `harden`, `fail2ban` | used once; afterwards the VM itself |
| Reapplied intent | `repos`, `ports` | the user's wish: repos on `repos clone`, ports on every `start`/`ports up` |
| Per-host state | a hub conf's `[machines.X].ports`; `run/`, `cache/`, `known_hosts` beside the confs | this host only |

The result:

- **The files are stale by design.** `memory`/`disk` are only a spec, but
  they look like facts. Resizing a VM outside devvm makes them wrong, or they would
  lie.
- **There is a registry that smolvm already keeps.** A smol conf holds
  nothing that `smolvm machine ls` doesn't, apart from repos and ports. A
  "dormant" state and the `provision`/`deprovision` verbs exist mainly to
  reconcile the two registries.
- **The files can't be shared.** They are tool-written, so comments are lost
  on save. Sizes and ports are per host, and smol entries only make sense on
  the host that runs the VM. So they can't live in a dotfiles repo, even
  though remote boxes and personal defaults are exactly what one wants to
  carry across hosts.
- **Every state-changing command writes config.** That drives the atomic
  save, the `key = 0` stripping, the load-time migrations, and the
  config-vs-runtime split in `status --watch`.

## Goals

- One config file the user owns, commented, hand-editable and safe to keep
  in a dotfiles git repo shared across hosts.
- The VM is the source of truth for everything the VM can answer: that it
  exists, its state and size, and what's cloned in it.
- Fewer concepts: no dormant state, no per-VM registry for smol, no create
  defaults copied into each VM, no second table family for hubs.
- **VM commands never write `devvm.toml`, and `config` commands never touch
  a box.** Only `devvm config add` and `rm` write the file (§4).
- **devvm modifies only boxes it manages.** A box is managed because devvm
  created it or the user handed it over with `setup`. Discovered smol VMs and
  adopted remote boxes are hands-off (§2).

## Non-goals

- A declarative "desired state" or reconcile engine. devvm stays imperative.
  A template seeds a box once.
- Changing the hub, daemon or bridge architecture (roadmap steps 1–9). This
  proposal touches only where their inputs come from. The daemon still never
  reads config (§5).
- A general key setter for the file (`config set`). `config edit` covers it;
  revisit if editing templates by hand turns out to be a chore.
- A migration command. There is one user and three hosts; migration is done
  by hand once, with the mapping in §8.

## Design

### 1. The file

`$XDG_CONFIG_HOME/devvm/devvm.toml`, owned by the user:

```toml
# ~/.config/devvm/devvm.toml, in dotfiles, shared by every host.

[templates.default]              # what create/setup use without --template,
                                 # and what every other template falls back to
user = "dev"                     # login user devvm creates and uses on remote boxes
hook = "url:https://raw.githubusercontent.com/smweber/dotfiles/master/bootstrap.sh --profile agent-vm --yes"
memory = 4096                    # create-time only (MiB); never compared afterwards
disk = 20                        # GiB

[templates.devvm]                # unset fields fall back to [templates.default]
memory = 8192
repos = ["smweber/devvm"]
ports = ["3000", "5173"]         # seeds this host's saved forwards (§5)
authorized_keys_github = ["smweber"]
harden = true                    # ignored for smol

[machines.thinkpad]
backend = "hub"                  # backend is mandatory on every entry
host = "192.168.1.196"           # host[:port]; may be an ssh alias (ProxyJump etc.)
user = "devvmtest"

[machines.cloud]
backend = "remote"
managed = true                   # devvm shapes it: setup, prereqs, hardening
host = "2.29.47.196"             # user comes from the template ("dev")
transport = "mosh"

[machines.scottdev3]
backend = "remote"               # managed defaults to false: adopted, devvm
host = "scottdev3"               # touches only ~/.ssh (and ~/.local/bin on consent)
mosh_server = "/home/linuxbrew/.linuxbrew/bin/mosh-server"
```

There are two table families. `templates` covers create-time shaping.
`machines` covers the entries devvm can't discover: remote boxes and hubs.
**There are no smol entries and no ports.** A missing file is valid and
means no remote boxes, no hubs and built-in defaults.

**Two axes, not one enum.** `backend` is one of `smol`, `remote`, `hub` and
is **mandatory** on every entry (`config check` fails without it; nothing is
inferred). `managed` is a bool, **default `false`**, so a forgotten key is
the safe reading: devvm never modifies a box because a line is missing.
`managed` is meaningful on `remote` only in the file; on smol it is host
state (§2); `config check` errors on `managed` under a `hub` entry. These
replace today's `remote-managed`/`remote-unmanaged` values and the `Managed()`
/`IsRemote()` predicates read them directly.

**`host` and `user`, not `user@host`.** `host` is `host[:port]` or an ssh
alias (a bare IPv6 address needs brackets, `[::1]:22`). `user` is optional
per entry. **Unset means no `user@` prefix**, so `~/.ssh/config` supplies
the user for an alias, as a bare `ssh_host` does today. The template's
`user` (built-in `dev`) is used only by `setup` on a `remote` + `managed`
entry: it is the account `setup` tries first and, on a root-only box, the
account it creates (§3). Keeping the user out of the entry is what lets
`setup` handle a fresh cloud VM without writing config. An ssh alias with
`ProxyJump` in `~/.ssh/config` is the supported way to reach a box through a
jump host; a hub is not a jump host (§7).

**Tailscale.** Nothing is Tailscale-specific, but the shared file pays off
most with it: put MagicDNS names in `host` (`thinkpad`, not a LAN IP) and
one entry works from every network. Two keys help: an optional per-entry
`connect_timeout` (seconds; default 10, same as `DEVVM_SSH_CONNECT_TIMEOUT`),
because a relayed or waking first hop can exceed 10s; and `harden = false`
in the template for boxes reached over Tailscale SSH, since that path never
touches sshd, so `PermitRootLogin`/`AllowUsers`/fail2ban are moot there
(harmless, but pointless). Forwards, mosh and the bridge are unaffected.

Per-host state moves to `$XDG_STATE_HOME/devvm` (`~/.local/state/devvm`):
`run/`, `cache/`, devvm's pinned `known_hosts`, the ssh ControlMaster
sockets (`cm-%C`, today beside the confs, `backend/ssh.go:108`), saved
forwards (§5) and the smol managed markers (§2). Implementation-wise this
needs a `config.Paths{Config, State}` threaded through `backend`, `session`
and `agentbin` in place of the single `configDir` they take today; it is the
first, behaviour-neutral step. The config directory then holds only the user's
file, so it's safe to symlink into dotfiles. This also ends hub.md §5's
"cache outside the watched `machines/`" workaround. The Swift app only shells
out to `devvm` and holds no config paths, so it's unaffected.

**A hub reading its own name.** When the ThinkPad reads the shared file it
sees `[machines.thinkpad] backend = "hub"`: an entry pointing at itself. That
is harmless and nothing is special-cased for behaviour. `status` marks the
entry `(this host)` when the table name exactly equals `os.Hostname()`, as
a cosmetic hint only; a wrong or missing hint breaks nothing. Nothing is
dialled.

### 2. Machines are discovered; the name is the identity

`status` lists:

- **local smol VMs**, from `smolvm machine ls --json` (`backend.SmolList`,
  as today); the devvm name *is* the smolvm `--name`, with no mapping and no
  record;
- **`[machines.*]` entries**;
- **hub machines** `HUB/NAME`, from each hub's own listing (the step 3 merged
  listing and cache, unchanged).

Resolving a name for any other command doesn't list anything:

- `HUB/NAME` goes to the hub.
- A name with a `[machines.NAME]` entry **uses that entry**. `attach cloud`
  runs no smolvm command, and a laptop with no smolvm never needs it.
- Any other name is a local smol VM. Resolution runs **one** `smolvm
  machine ls --json` (replacing today's `machine status --name` probe: same
  subprocess count) and keeps the row: existence, state, sizes and
  `created_at`. The record's `Managed` is settled here, once, from the row
  and the marker (below). **`Managed()` never execs anything.**

**Collisions** (a `[machines.web]` entry *and* a local smol VM `web`) are not
silent. The file entry wins for commands; the VM is shadowed. `status` shows
the entry's row only and reports the collision as `shadowed` in the fifth
`--plain` column (§7); `config check` reports it too. `create web` refuses while
`[machines.web]` exists. smolvm can't rename a VM, so the fix is to rename
the config entry or delete the VM.

A name either exists or it doesn't. There is **no dormant state**:
`provision` and `deprovision` go away. `create` allocates, and `delete`
destroys the VM (smol). Rebuilding a box is `delete` then `create
--template T`.

**Managed and unmanaged smol VMs.** Symmetry with remote boxes: a smol VM
devvm did not create is treated like an adopted box. A discovered VM is
**unmanaged by default**. Exactly two things make it managed: devvm `create`,
or an explicit `setup NAME` (the user handing it over, the same consent gate
as `auth --install-agent` on an adopted box). There is therefore no code path
that installs anything into a VM the user did not give devvm.

The managed bit is host state, `$XDG_STATE_HOME/devvm/managed/NAME`,
holding the VM's `created_at` from `smolvm machine ls --json`. It cannot live
in the guest (reading it needs an exec, and an exec may boot a stopped VM) or
in the shared file (smol VMs are per host). A VM deleted with raw smolvm and
recreated under the same name has a new `created_at`, so a stale marker is
ignored, never trusted; a row without `created_at` is unmanaged. The field
is unverified in this repo's fakes (`SmolMachine` has only name and state,
`backend/smol.go:57`): pin it with a fixture first. The daemon settles
`Managed` at start and **re-reads it on every reconnect**, because the VM
may have been recreated under the same name while the daemon slept. Hub VMs
keep their marker on the hub, which enforces it for proxied commands. (If
smolvm turns out to expose a label or metadata field in that JSON, it
replaces the marker file.)

**What an unmanaged smol VM can do is deliberately small.** Today every smol
exec is hard-wired to the `dev` user (`sudo -u dev -H`, `backend/smol.go:184`),
the tmux socket lives under `/home/dev` (`smol_shell.go:13`) and the daemon's
agent spawn runs as `dev` (`session/transport_smol.go:41`). A VM devvm did
not create has no `dev` user and no sudoers entry, so none of that can be
assumed. An unmanaged smol VM supports:

- `status`, `start`, `stop`;
- `shell` and `exec`, run as smolvm's own default user over plain `machine
  exec`, with no `BROWSER`, no tmux and nothing installed;
- `setup`, which promotes it (§3);
- `delete` **refuses** and points at `smolvm machine rm`, as it refuses on a
  config entry and points at `config rm`. devvm destroys only what it made.

Everything else needs `setup` first and says so in one line: `attach`, `cp`,
`ports`, `auth` and the browser bridge. In code, `bridged(m)` (`cli/bridge.go:176`)
requires `Managed()` on smol, and the smol transport's `agentbin.Install`
gets an approve callback that **fails** when `!Managed()`, so a detached
daemon can never install anything (today it installs with no gate,
`transport_smol.go:35`). The agent install posture is keyed on `Managed()`
alone, on every backend: managed means root-owned `/usr/local/bin`;
unmanaged (adopted remote boxes, `auth --install-agent`) means consented
`~/.local/bin`.

**`delete` on a managed smol VM** removes the VM, its marker and its saved
forwards file (§5). `create` and `setup` truncate a stale ports file for
the name before seeding, so a recreated VM never inherits old forwards.

### 3. Templates and `setup`; "bootstrap" goes away

The word `bootstrap` named two different things: devvm's own needs (agent,
shim, tmux) and the user's hook, which is itself often a script called
`bootstrap.sh`. They split:

**devvm's own needs are part of `setup`, not a lazy layer.** The agent and
shim keep installing on demand as today (sha-gated, managed boxes only on
smol). Everything else devvm needs in a box, the existing prereqs
(`bootstrap/prereqs.go:21`: build tools, curl, git, sudo, the user and
sudoers, hostname) **plus tmux, gh and mosh-server**, which today come from
the hook (hence `RequireGuestTmux`), is installed once by `setup`. A box is
managed only because `setup` ran, so an "install on first attach" layer
would have nothing left to do and would run an apt beside the daemon's exec
under a waiting user. "devvm installs only on managed boxes" is then
structural: the only installer is `setup`, and `setup` is the thing that
makes a box managed. The unused `/etc/devvm-agent` touch goes.

**Shaping a box is `devvm setup NAME [--template T]`.** A template is "how
to shape a new box": the create-time spec (`memory`, `disk`), `user`,
`hook`, keys, `harden`/`fail2ban`, `repos` and `ports`. Unset fields fall
back to `[templates.default]`, one level, no chains; `config show` prints the
effective template. `setup` is the **single place devvm touches a box's contents** and the
single promotion path to managed. Its order matters and is fixed (today's
`cli/bootstrap.go:22`, with `Harden` refusing on an empty `authorized_keys`,
`harden.go:21`): ensure user → prereqs → agent + shim (+ `profile.d` on
smol) → hook → repos → keys → harden → seed saved forwards (§5).

- **`devvm create NAME [--template T] [--memory M] [--disk D]`** (smol, and
  a future Hetzner backend) allocates with the template's sizes (flags
  override), writes the managed marker, then runs `setup`. Day to day you
  type only `create`.
- **`devvm setup NAME [--template T]`** on a discovered smol VM is the
  handover, and it asks: on a TTY it prints what it is about to do (install
  prereqs and the agent as root, run the `T` hook, add keys, clone repos,
  treat `NAME` as managed from now on) and waits for `y`; without a TTY it
  refuses unless `--yes`. Then it writes the marker and shapes the VM. On a
  `remote` entry with `managed = true` it shapes without asking (writing
  `managed = true` was the consent); on an already managed smol VM it
  re-runs without asking (the idempotent path); on an unmanaged remote entry
  it refuses, as `bootstrap` does today.
- **Root-only cloud VMs.** `setup` on a managed remote entry tries
  `USER@host` first. On *permission denied* (a stderr match, BatchMode on;
  exit 255 alone can't tell it from unreachable) it tries `root@host`; if
  that works it creates `USER` with NOPASSWD sudo and root's keys
  (`EnsureDevUser`, today gated on `remote-managed` and hardcoding
  `dev`/`/home/dev`, `prereqs.go:52-79`; it becomes `EnsureUser(name)`),
  then continues as `USER`. **`setup --host ADDR`** overrides the entry's
  `host` for this one run, so a fresh cloud VM can be shaped over its public
  IP while the entry already names its tailnet address (the hook joins the
  tailnet); again nothing is written.
  Nothing is written to config. Only `setup` ever tries root, and only on
  `remote` + `managed`. Every other command uses `USER@host` and on failure
  says "run setup". `Harden` pins `AllowUsers` to `USER`.

**Nothing records which template made a box.** Editing a template never
changes a *running* VM. But `setup NAME` with no `--template` re-runs
whatever `default` says *now*. That's deliberate and simpler than today's
hook copied into each conf, but it's a change: a re-run is no longer
guaranteed to replay the original hook. Template fields apply only where
they make sense: `memory`/`disk` are ignored for remote boxes, where there's
no `create` (`config check` warns); `harden`/`fail2ban`/keys are ignored for
smol.

**Interactive definition stays.** `create` on a TTY prompts for template
fields left unset, as today. The huh form for a remote box moves to `config
add` when `--host` isn't given.

**Repos stop being tracked.**
- `repos add NAME REPO` clones into `~/src` now, using the existing
  `cloneRepos`, which already skips a directory that exists, so re-running
  `setup` is idempotent.
- `repos list NAME` lists the git checkouts under `~/src` in the guest (one
  exec), so it now needs a running box (today it reads the conf and works on
  a stopped one). `cloneRepos` needs `gh`, which is why gh is a `setup`
  prereq above.
- `repos rm`, `repos clone` and `--no-clone` go away.

**Sizes stop being stored.**
- **There is no `devvm resize`.** Resize with `smolvm machine update` or the
  provider's console. devvm stores no size, so nothing can drift. A cloud
  resize (a server-type change needs a power-off, and a disk grow is
  one-way) is deliberately left to the provider.
- smolvm's own record carries the sizes. `smolvm machine ls --json`
  reports `memory_mib`, `storage_gb` and `cpus` per machine (checked on the
  user's Mac). `SmolMachine` (`backend/smol.go:57`) gains those fields, and
  `status -v` reads them for running and stopped VMs alike. That
  replaces `smolLiveResources`' guest exec, which ran only when no daemon
  owned the exec.

### 4. Config editing is its own noun

Every VM command reads `devvm.toml`; none writes it. Every `config` command
reads and writes the file; none dials a box.

```
devvm config path                 # print the file path
devvm config show                 # effective config (file + built-ins + template fallback), annotated
devvm config edit                 # $EDITOR on the file, then check; keep a .bak on failure
devvm config check                # parse + validate, offline; non-zero on error (dotfiles hook)
devvm config add NAME --backend remote|hub --host H[:P] [--user U] [--managed] [--transport mosh]
devvm config rm NAME              # remove a [machines.NAME] table
```

**The writes are structurally safe, not clever.** `add` appends a whole
table at the end of the file, always writing `backend` and (for remote)
`managed` explicitly. `rm` deletes one `[machines.NAME]` table's line range.
Both re-parse the result before an atomic rename, so the user's comments and
layout elsewhere are never touched. Everything else (templates, `transport`,
keys) goes through `config edit`. There is no general key setter.

**`config add` does not test-connect.** v2 had it dial the box and run root
adoption; that broke "config commands only touch config". Reachability is
`setup`'s (or the first `attach`'s) problem, and root adoption lives in
`setup` (§3). `config check` is offline and never dials.

**`config rm` still owns local state.** Not dialling a box is not the same
as leaving this host's state behind: `config rm NAME` stops the local
daemon(s) for `NAME` (and `NAME@*` for a hub, as `delete HUB` reaps them
today, `machine_cmds.go:308`) and removes `state/ports/NAME` and
`state/ports/NAME@*`. It refuses while a daemon for the entry holds live
forwards, unless `--force`.

What this changes elsewhere:

- **Remote `create` is gone.** Setting up a remote box is `config add` then
  `setup`, and `create` means only "allocate a VM".
- `config.toml` and the `defaults` noun are replaced by
  `[templates.default]`, edited with `config edit`.
- A CLI edit to the file shows up as a `git diff` in the dotfiles repo. A VM
  operation never dirties it.

### 5. Saved forwards are host state, not config

Forwards are per client: which host port a guest port lands on depends on
what else is bound on *this* host. That's why the hub design already keeps a
laptop's forwards for a hub machine in the laptop's own hub conf (hub.md §2,
"inherently per client"). This proposal generalizes that to every machine
and moves it out of config:

- **Storage:** `$XDG_STATE_HOME/devvm/ports/NAME`, one mapping per line
  (`GUEST` or `HOST:GUEST`, the syntax `ports add` takes today). Hub
  machines use the runtime identifier `HUB@NAME`, as sockets and logs
  already do. A template's `ports` seed the file at `setup`.
  `ports add` / `ports rm` edit it, saving by default as today
  (`--ephemeral` for a forward that lasts only as long as the daemon).
  `delete` and `config rm` remove it; `create`/`setup` truncate a stale one.
- **Who reads it:** the CLI, exactly where it reads `m.Ports` today.
  `tunnelUpWait` (`start`, `ports up`) pushes one `Add` per line. The daemon
  keeps forwards in memory, and `restore()` re-adds them on reconnect.
  **The daemon never reads config or this file.** Nothing is polled, on any
  backend.
- **Conflicts and visibility are unchanged:** exact-or-bump, `reportForward`,
  `ports list`'s line, and the `--plain` forwards column. `down` ("saved
  forwards, daemon not up") stays computable for a stopped VM, because the
  list is on the host. The Swift app needs no change for forwards.
- **What it removes:**
  - the hub conf's `[machines.X].ports` tables, the `flock` rewrite of hub
    confs (hub.md §2), and `UpdateHubMachine`, since every machine gets one
    small file;
  - the need for any per-VM record at all.
- **Cost:** the saved list doesn't follow a VM to a second client. That's
  intended: host ports are per client, each client seeds from the same
  template, and step 7's on-demand forwards (a guest opening a loopback URL)
  cover the ad-hoc case everywhere.
- **The daemon never reads this file.** It still receives its
  `*config.Machine` at start (`cli/daemon.go:22`) and, for smol, re-reads the
  managed marker on reconnect (§2); what it never does is read *ports* from
  anywhere but its clients.

Alternatives considered:
- **A ports file inside the guest.** It needs an exec to read, which a
  stopped VM can't answer (so `down` breaks), an adopted-box exception, and
  a hub round-trip.
- **Auto-detecting listening ports.** It's smol-only without polling, and
  every stray listener grabs a host port. It could later be an opt-in on
  smol that forwards exact-only and never saves.
- **No saved forwards.** Forwards would be lost at every daemon idle-out.

### 6. What goes away

| Removed | Replaced by |
|---|---|
| `machines/*.toml`, one per box | `[machines.*]` for remote boxes and hubs; smol discovered |
| `config.toml` + `devvm defaults {list,set,unset,path}` | `[templates.default]` + `config edit` |
| `memory`, `disk`, `bootstrap_hook`, `authorized_keys*`, `harden`, `fail2ban` per box | template fields, used at create/setup |
| `backend = "remote-managed" \| "remote-unmanaged"`, `ssh_host = "u@h"` | `backend = "remote"` + `managed`, `host` + `user` |
| `bootstrap` (verb, phase, `bootstrap_hook`) | automatic prereqs on managed boxes; `setup` + template `hook` |
| per-box `repos`, `repos rm`, `repos clone`, `--no-clone` | cloned once; `repos list` reads the guest |
| per-box `ports`, hub `[machines.X].ports`, hub-conf `flock`, `UpdateHubMachine` | `state/devvm/ports/NAME` |
| `provision`, `deprovision`, dormant state, `resolveLive`/`requireProvisioned` dormant branches | `create` / `delete` |
| remote `create` path; `bootstrap`'s root-adoption `Save` | `setup` does root adoption with no config write; `config add` only writes |
| key seeding of *adopted* remotes by `bootstrap` | `keys add` (unchanged); `setup` refuses unmanaged remotes |
| `Machine.Save`, `key = 0` stripping, load-time `backend = "ssh"`/`unmanaged` migration | append/remove-table writes in `config` only |
| "live but unregistered smol VM" fallback (`config.NewSmol`) | discovery is the only path (unmanaged by default) |
| `[hosts]`-style second family, `adopt` flag (never shipped; were in v1) | `backend` + `managed` |

### 7. Compatibility surfaces

- **`status --plain`** keeps its first four columns
  (`name\tbackend\tstate\tforwards`) and appends a fifth: `managed`,
  `unmanaged`, `shadowed` (a config entry hiding a local smol VM of the same
  name, §2) or `-` (hubs). A row from an older hub has no fifth field; the
  laptop renders `-`, never `unmanaged`, which would be a false claim
  (`hublist.go:104` and `Status.swift:88` already tolerate ≥3 fields). The
  new column is **not** a `hubMinVersion` bump; only the new proxied
  `create`/`setup --template …` flags are. The backend column reads `smol`,
  `remote` or `hub`. The state token `dormant` disappears, and `broken conf` narrows to
  "devvm.toml failed to parse". The Swift app (`Status.swift:15,54,73`,
  `MenuBar.swift:376`) drops its dormant branches and learns the two new
  values: one small, coordinated Swift change. The forwards tokens (`up:N`,
  `reconnecting:N`, `down`, `-`) are unchanged, because saved forwards stay
  host-readable.
- **`status --plain --watch`** swaps `machines/` for `devvm.toml` and the
  state dir in its fsnotify set. Smol VMs created or deleted with raw
  `smolvm` aren't seen until the next re-emit. devvm's own `create`/`delete`
  still call `TouchChanged`, so this matches today.
- **Hub surface:** `create HUB/NAME --template T` and `setup HUB/NAME
  --template T` resolve the template on the laptop and send explicit flags
  to the proxied command (as `keys add` inlines key lines today), so the
  hub's file never matters for it. This changes the proxied argv, so it needs
  a `hubMinVersion` bump. A hub lists its own `[machines.*]` entries too (it
  does today: `h/scottdev3`); the laptop's merged listing hides a hub's rows
  whose backend is `remote` and whose name is also in the laptop's own file,
  since the direct path is the right one, and shows the rest. Hubs never chain. **A
  hub is not a jump host**: reaching a box through another host is ssh's
  `ProxyJump` on the `host` alias, not `HUB/NAME`.
- **The ports-list line format is unchanged.**

### 8. Migration, by hand

No command. The release that ships the new loader deletes the old one. When
`machines/` exists without `devvm.toml`, every command prints one line
pointing here. The mapping, applied once per host with the author's help:

- a remote conf becomes `[machines.NAME]` with `backend = "remote"`,
  `managed = true` for `remote-managed`, `host`/`user` split from `ssh_host`,
  and `transport`/`mosh_server`/`identity` carried over;
- a hub conf becomes `[machines.NAME]` with `backend = "hub"`, `host`, `user`;
- `config.toml` becomes `[templates.default]` (`bootstrap_hook` → `hook`);
- smol confs are dropped (discovered); each devvm-made smol VM gets a
  managed marker (`devvm setup NAME` writes it and is idempotent);
- per-box `ports` and hub `[machines.X].ports` go to
  `state/devvm/ports/NAME` or `HUB@NAME`;
- `~/.config/devvm/{run,cache,known_hosts}` move to `~/.local/state/devvm`.

## CLI surface after

- **Lifecycle:** `create NAME [--template T] [--memory] [--disk]`, `setup
  NAME [--template T]`, `start`, `stop`, `delete`.
- **Connect:** `attach`, `shell`, `exec`, `auth`.
- **Connect, kept unchanged:** `cp-in`, `cp-out`, `lockdown` (adopt hosts,
  with its confirm).
- **Configure:** `repos {add,list}`, `ports {add [--ephemeral],rm,list,up,down}`,
  and `keys {…}` (unchanged; already writes no config).
- **Config:** `config {path,show,edit,check,add,rm}`.
- **Maintain:** `update`, `menubar`.

A fresh box for a new task is `delete old`, `create new`, `attach new`.

## Implementation order

Each step ships on its own and leaves the tree green; the old loader stays
until step 5.

1. **Paths split** (§1): `config.Paths{Config, State}`; `run/`, `cache/`,
   `known_hosts`, `cm-%C` move to `$XDG_STATE_HOME/devvm`. Behaviour-neutral.
2. **Saved forwards as host state** (§5): `state/ports/NAME`; the CLI
   readers swap; hub `[machines.X].ports`, `UpdateHubMachine` and the flock go.
3. **smol identity and the marker** (§2): one `ls --json` snapshot in
   resolve with `created_at` and sizes; `Managed` settled on the record;
   `bridged()` gated; the daemon transport refuses to install when unmanaged
   and re-reads the marker on reconnect; `delete` refuses unmanaged smol and
   entries; `provision`/`deprovision`/dormant removed; `status -v` sizes from
   the snapshot. The fifth `--plain` column and the Swift change land here.
4. **`setup` and templates** (§3): `bootstrap` → `setup` with the fixed
   order and the handover prompt; `[templates.*]` from the new file read
   alongside the old confs; `EnsureUser(name)`; root adoption in `setup`;
   explicit flags on the proxied form (`hubMinVersion` bump).
5. **New loader and the `config` noun** (§1, §4): `devvm.toml` replaces
   `machines/*.toml` and `config.toml`; `config {path,show,edit,check,add,rm}`;
   `defaults`, remote `create`, `repos rm/clone`, `--no-clone` removed;
   `status --watch` fsnotify set swapped; the one-line pointer when
   `machines/` exists without `devvm.toml`.
6. **Hand migration** of the three hosts (§8), then the release.

## Open questions

None open. Answered, recorded so they aren't reopened:
- **Key setter:** no; `config edit` only (user, 2026-10-17).
- **`delete` on a config entry:** refuse, point at `config rm` (user,
  2026-10-17). Same for an unmanaged smol VM → `smolvm machine rm`.
- **Ports:** saved forwards are host state (§5).
- **Sizes:** read from `smolvm machine ls --json` (§3).
- **Template inheritance:** one-level fallback to `default` only (§3).
- **Hub templates:** inlined by the laptop (§7).
- **Hubs:** `backend = "hub"` in `[machines]`.
- **A hub entry naming the host itself:** cosmetic hint only (§1).
- **`memory`/`disk` on remote boxes:** ignored, and `config check` warns.

## Review log

**v2 → v3** (design discussion with the user, 2026-10-17):

- **Two axes:** `backend ∈ {smol, remote, hub}` (mandatory) + `managed`
  bool (default false) replace the hyphenated backend values. The user found
  the implicit default backend confusing in the v2 sample.
- **Managed/unmanaged smol VMs** for symmetry with remote boxes: discovered
  VMs are unmanaged and hands-off; `create` or `setup` makes one managed, via
  a host marker keyed on smolvm's `created_at`.
- **`bootstrap` → `setup`**; devvm's own prereqs become automatic on managed
  boxes only; template key `bootstrap_hook` → `hook`.
- **`host` + `user`** replace `user@host`; `setup` does root adoption with no
  config write; `config add` no longer dials anything.
- **Template fallback** to `default` for unset fields (one level).
- **`config migrate` cut**; migration by hand (§8).
- **Hub self-match** demoted from behaviour to a cosmetic hint; hubs
  documented as not jump hosts (use `ProxyJump`); laptop hides a hub's
  remote entries it also has directly.

**v3, second independent review** (2026-10-17), accepted:

- **Unmanaged smol VMs narrowed** (§2): every smol exec assumes the `dev`
  user and sudoers that only devvm creates, and the daemon installed the
  agent with no gate. Unmanaged now means `status`/`start`/`stop` and
  `shell`/`exec` as smolvm's default user; everything else needs `setup`,
  and the daemon refuses to install on an unmanaged box.
- **Lazy prereqs dropped** (§3): managed ⇔ `setup` ran, so prereqs
  (including tmux, gh, mosh-server) belong in `setup`.
- **`user` unset means no `user@`** (§1), so ssh aliases keep working.
- `cm-%C` moves to the state dir; `config.Paths` is step 1.
- `setup` order fixed (keys before harden); `EnsureUser(name)`.
- Marker and ports files truncated on recreate; `delete` removes both.
- `config rm` reaps local daemons and ports files.
- `shadowed` token; fifth column from an old hub renders `-`, no
  `hubMinVersion` bump for it.
- `repos list` needs a running box; `lockdown`/`cp-in`/`cp-out` listed as
  kept; `ports add` keeps saving by default (`--ephemeral` opts out).
- `setup` on a discovered VM prompts (user request): TTY confirm, `--yes`
  otherwise.

**v1 → v2** (one independent review, 2026-09-27). Changes accepted:

- **§4, root adoption:** `bootstrap`'s root adoption wrote `ssh_host` back,
  which broke "VM commands never write". It moves to `config add`.
- **§5, ports:** every v1 ports option broke the `down` forwards token or
  added a mechanism. v2 keeps saved forwards as host state, read by the CLI
  as today. The v1 claim that "the daemon reads it" was also wrong about
  today's code.
- **§2, name resolution:** v1 ran `smolvm machine ls` on every name lookup.
  v2 lets the entry win, flags collisions, and has `create` refuse. The v1
  "until one is renamed" was wrong: smolvm can't rename.
- **§1, one table family:** the `hosts` family and the `adopt` flag are
  replaced by the existing `backend` values.
- **§8:** the transition release is dropped.
- **§3:** the "template edits never change an existing VM" claim is
  qualified.

Taken partly: the review suggested cutting `config rm`/`set`/`unset` and
the comment-preserving writer. `set`/`unset` and the line editor are cut (it
becomes open question 1). `rm` stays, because deleting a whole table is as
structurally safe as appending one, and you asked for devvm to help manage
the file.
