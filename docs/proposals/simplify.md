# Proposal: simplify (fewer commands, no "managed", one ownership rule)

Status: proposed, 2026-11-20, from a design discussion with the user
(v1.2: a code review's findings folded in and its questions settled). Nothing
built. The goal is that devvm only gets simpler and more robust:
every phase removes more than it adds. `share-auth.md` is built **after**
this plan and is written against the model below. Once agreed, the order of
work goes in `ROADMAP.md`.

## Problem

devvm is conceptually complicated, a result of organic growth:

- Every machine has two properties (backend, and whether devvm "manages"
  it), and each backend settles the second a different way: a marker file
  (smol), a key in the entry (remote), a label (hetzner), never (hub).
  Several cells of that grid exist for little use.
- `auth` and `repos` predate the browser bridge and discovered machines;
  what they do is now one command typed in the box.
- Forwards can be saved from four places and created three ways.
- One failed clone during `setup` skips keys and hardening on a cloud box.
- Upgrade code for layouts no host still has is read on every invocation.

## The model after this plan

**devvm shapes what it creates and only connects to everything else.**

| Kind | Backends | Where it lives | What devvm does |
|------|----------|----------------|-----------------|
| Boxes devvm made | smol, hetzner | discovered by the `devvm=true` label, no entry | `create`, `setup`, `delete`, and everything else |
| Boxes you told it about | remote, hub | `[machines.*]` in `devvm.toml` | connects only; never shapes |

- "Managed" stops being a user-facing concept. `setup` applies to smol VMs
  and provider servers, and to nothing else.
- An entry is a box devvm reaches over ssh: `attach`, `shell`, `exec`,
  `ports`, `cp-in`/`cp-out`, `keys`. The one thing devvm may write there
  beyond `~/.ssh` is the agent, and only when the entry says `agent = true`.
- Forwards: a loopback URL a guest program opens is forwarded automatically
  while a session is attached (the browser bridge, unchanged). Anything else
  is `ports add`, remembered on this host and restored on `attach`, `shell`
  and `start`.

## Decisions (2026-11-20)

1. Remove `auth`. The bridge covers it: `gh auth login` and `codex login`
   run inside `attach` open on the laptop (codex confirmed by the user on
   scottdev3).
2. Remove `repos` (`add`, `list`). The template's `repos` field and setup's
   clone step stay.
3. Remove `lockdown`: it is setup's harden step under another name.
4. The agent on an entry is consent in the file: `agent = true`.
5. Ownership of a discovered machine is a label on the machine, for smol as
   for hetzner. No marker files.
6. No unmanaged smol VMs and no unlabelled provider servers: devvm does not
   see what it did not create. No `setup --adopt`.
7. No managed remote entries. A box from another cloud is shaped by hand and
   reached as an entry, or gets a provider.
8. Ports leave templates, `create` and `setup`. `--ephemeral` goes. Saved
   forwards stay, and come up on `attach`/`shell` too.
9. Left alone: `keys dedupe`, `providers token`, the hub/remote entry split
   (one entry per role, as now), the "no saved forwards at all" option, the
   OAuth callback's 10-minute ttl, forward ownership and the bump rule, and
   the daemon, relay and bridge machinery.

## Phases

Each phase ships on its own. The bug fix is first; the phase that needs a
newer smolvm is late.

### Phase 1: `setup` never skips keys and hardening

`shape` (`internal/cli/setup.go`) runs hook → repos → keys → harden and
returns on the first error, so a failed clone or hook leaves a billing cloud
server unhardened.

1. New order: ensure user → prereqs → agent + shim → **keys → harden** →
   hook → repos (→ saved forwards, until phase 3 removes that step). The
   security steps no longer sit behind user code or the network. Harden
   reloads sshd and pins `AllowUsers` to the login user; the steps after it
   run as that user on the same connection, which a live test must confirm.
2. A failed clone no longer ends setup: the remaining repos are tried, and
   setup exits non-zero at the end with "setup finished, but: failed to
   clone …". `create` reports that as created-with-a-problem. This needs a
   typed error: today `create --backend hetzner` wraps any `shape` error
   as "setup failed … it bills" (`cli/providers_lifecycle.go`), and the
   hub proxy skips its post-run steps on any non-zero exit (`proxy.go`).
3. Logged out is a skip, not a failure: if the guest's `gh` has no working
   login when the repos step starts, `owner/repo` specs are skipped with
   one line naming them and the fix (`gh auth login` in the box, then clone
   by hand), and setup exits zero. URL specs are still tried with plain
   `git`.

Harden's apt and unattended-upgrades now run before the hook, so a hook's
own apt may meet the dpkg lock: devvm's apt calls pass
`-o DPkg::Lock::Timeout`.

Tests: harden runs after a failed clone and after a failed hook.

### Phase 2: remove commands

4. **`agent = true`** on a `[machines.NAME]` entry (remote only; an error
   under a hub). The daemon reads its entry once at start, as now; with the
   key it runs the user-scoped `~/.local/bin` install (and so replaces a
   stale agent after `devvm update` by itself); without it, it only probes,
   as today. This replaces `auth --install-agent`, the consent prompt, and
   the install-then-kick-then-wait handoff. Until phase 4, a `managed =
   true` entry keeps its root install. The daemon captures its record once
   (`sshAgentHooks` in `session/transport_ssh_agent.go`), so a key added
   to the file takes effect when the daemon next starts (`ports down`,
   end the attach). Two devvm hosts with different builds reaching one
   box would reinstall over each other on every respawn (they already
   share `~/.devvm/agent.sock`, ROADMAP Known issues): one devvm host per
   entry for the bridge.
5. **Remove `auth`**: `internal/auth`, `internal/cli/auth.go`, the command,
   the hub auth proxy (`authHubMachine`), `openAuthBridge`.
   `DEVVM_NO_SUBSCRIBE` stays (proxied attach/shell use it), as does the
   daemon's `kick`.
6. **Remove `repos`**: the noun and both leaves, `runReposAdd`,
   `runReposList`, `promptRepo`, and the laptop-side `repos add` handling in
   `proxy.go`. `cloneRepos` stays for setup.
7. **Remove `lockdown`**. `bootstrap.Harden` stays for setup.
8. **Dotfiles residue**: the "run bootstrap.sh on the host" strings
   (`cli/create.go`, `backend/smol.go`, `update`'s help) name smolvm's own
   installer instead; the jj colocation after a clone goes (a preference
   that belongs in the user's hook); the hidden `--bootstrap-hook` flag
   goes. The built-in hook is already `none`, and prereqs is already the
   minimal bootstrap: nothing is added.

Strings that name `devvm auth` change with step 5: the daemon's "no
browser events until 'devvm auth NAME' installs it" line
(`transport_ssh_agent.go`) names `agent = true`; the bridge notices in
`cli/bridge.go`; create/setup's closing hints (`cli/create.go`), which also
stop naming `repos add`. Nothing else depends on `auth`: codex's 1455
callback is bound by the daemon's own URL classifier
(`session/bridge.go`). Lost with it are two conveniences: picking
`codex login --device-auth` when available, and chaining `gh auth
setup-git` after `gh auth login`.

### Phase 3: ports

9. Ports leave templates, `create` and `setup`: the `ports` template field
   (a load error naming `ports add` if still present), `--port`,
   `--no-ports`, the form's ports question, `seedPorts` with its
   fresh-versus-merge rule, the proxy's strip-and-reseed for hubs, and
   setup's last step. `ports add` is the only way to save a forward.
   One rule of `seedPorts` survives: **`create` clears a leftover ports
   file for the name** (`config.RemovePorts`; for `HUB@NAME` after a
   proxied create), since a box removed outside this host (raw `smolvm
   rm`, the provider console) leaves its file, and step 11 would raise a
   predecessor's forwards on the first attach. `--port` and `--no-ports`
   stay as hidden, ignored flags until phase 5, so a laptop and a hub one
   phase apart still proxy `create`/`setup`.
10. Remove `ports add --ephemeral`.
11. `attach` and `shell` push the saved forwards when they connect, as
    `start` and `ports up` do. The daemon still never reads the ports file:
    the client pushes adds. Not by calling `tunnelUpWait`, which blocks on
    the daemon's come-up (undoing the 5s bound for a dead remote box) and
    prints: the push is best-effort and quiet, in the background, on the
    session's own client, and never fails the session. Laptop side only: a
    proxied attach on a hub runs under `DEVVM_NO_SUBSCRIBE` and does not
    touch the daemon there. `bootForSession`'s push after booting a
    stopped VM (`cli/machine_cmds.go`) folds into this. `ports down` then
    holds only until the next `attach`/`shell`/`start`/`ports up` (Open
    questions).

`ports list`'s line format and the owner kinds do not change, so no Swift
change. `ports up`/`down` stay.

### Phase 4: remove "managed"

Largest phase: resolve, setup, discovery and the daemon. One commit per
step, a live test after each.

12. **No managed remote entries.** The `managed` key is a load error naming
    the fix (delete the line; `agent = true` for the bridge). Gone with it:
    `config add --managed`, `setup --host`, `checkTemplateUser`, the
    "managed entry with no `user` logs in as the template's user" rule,
    pinned `known_hosts` for entries, and the root-install posture for
    entries. `setup` on an entry refuses in one line. The root-then-create-
    user flow stays for fresh provider servers. Dead with it:
    `pinSharedBy` over entries and `config rm`'s pin clearing,
    `checkPrereqs` (`bootstrap/prereqs.go`), `hubMachineIsSmol`. A
    provider `setup --template T` whose user differs from the login keeps
    a warning (the template's user is ignored there).
13. **Hetzner: labelled servers only.** List with the label selector;
    resolve ignores an unlabelled server. Gone: unmanaged provider rows,
    the confirm on `start`/`stop`, `providerNoPromoteErr`, `delete`'s
    refusal, and the `--yes` on `start`/`stop` if the unmanaged confirm
    was its only use. Adding the label by hand still makes a server
    appear, which is the only promotion.
14. **smol: the label is ownership.** `create` passes `--label devvm=true`
    (smolvm 1.35.5 has `machine create --label` and `machine update
    --label`; verified here 2026-11-20 with a round trip through `machine
    ls --json`). Discovery keeps rows carrying it. Gone:
    `<State>/managed/`, `config/managed.go`, the `created_at` compare and
    its normalisation, stale-marker handling. The daemon's re-check before
    a dial becomes "still listed with the label". devvm requires a smolvm
    with `--label` and says so in one line when the flag is missing.
15. **No unmanaged smol VMs.** Gone: `resolveUnmanaged`, the plain-exec
    limited mode, `setup --adopt`, `confirmAdopt`, `adoptPlan`,
    `AdoptHint`, `ErrUnmanaged`. A command naming an unlabelled VM says "a
    smol VM named X exists, but devvm did not create it"; `create X`
    refuses on the name. An entry still shadows a discovered machine of the
    same name.

`Managed()` stays as a predicate (true for discovered machines, false for
entries) so call sites change little; the `--plain` fifth column stays and
reads `managed` on smol and hetzner rows, `-` on entries and hubs. Swift,
in the same commit as step 15: `needsAdopt`, `isUnlabelled` and `isUsable`
(`contrib/macos/Sources/Status.swift`), and the adopt item and unlabelled
branch in `MenuBar.swift`. `setup --yes` goes if `--adopt`'s refusal was
its only use.

### Phase 5: clean up

16. **Delete the upgrade paths**: `config.MigrateState`,
    `config.MigratePorts`, `backend.SeedLegacyMarkers`, old-layout daemon
    handling in session clients/`update`/`delete`/`config rm`, the
    `remote-managed`/`remote-unmanaged`/`dormant` mapping in
    `parseHubRows`, the pointer line for an old `machines/` dir. Raise
    `hubMinVersion` (and the forwards/bridge minimums) to the release that
    carries this plan. Residue to sweep with it: `cli/create.go`,
    `config/file.go` (old backend values), `cli/status.go` (`dormant`),
    `cli/list.go`, `session/reap.go`, `cli/update.go`, `session/client.go`,
    and their tests (`session/legacy_test.go`, `cli/unmanaged_test.go`
    among about twenty files).
17. **Shrink what a laptop sends its hub.** `HUB/NAME` is only ever a
    hub's smol VM, and after step 12 no entry is shaped, so `user`, keys,
    `harden` and `fail2ban` never cross: delete `--user`, `--key`,
    `--harden`, `--fail2ban`, the hidden `--port`/`--no-ports`, and the
    proxy's `templateKeyLines` use. What is left is hook, memory, disk and
    repos, carried as now: `--inline-template`, the visible
    `--hook`/`--memory`/`--disk` and a hidden `--repo`. No new encoding.
18. **Docs**: `CLAUDE.md` (backend axes, setup order, vocabulary, the
    removed commands), `README.md`, "as shipped" notes in `config.md`,
    `providers.md`, `browser-bridge.md`, `hub.md`; `ROADMAP.md` gets the
    milestone.

### After this plan: `share-auth.md`

Copies the host's `gh` login into a box devvm created, as a setup step
before the hook. It needs nothing from the agent or the daemon.

## What the user does by hand

- **Before phase 4**: upgrade smolvm on the ThinkPad (and any other hub) to
  a version with `--label` and run a live pass there, since devvm's smolvm
  assumptions (exec refusing a stopped VM, a booting VM listing as
  `stopped`, the 11 MiB stdout cap) are pinned to 1.16.1 and untested on
  1.35.5. Label each existing VM once: `smolvm machine update --name NAME
  --label devvm=true` (labels can change while the machine runs).
- **With phase 2**: add `agent = true` to `scottdev3`. `auth` was the only
  installer for it, and an agent from another build is never spawned, so
  without the key the first `devvm update` after phase 2 ends that box's
  bridge.
- **With phase 3**: delete any `ports` line from a template.
- **With phase 4**, for `anytime-staging-1`: delete `managed = true`
  (devvm no longer runs `setup` or harden on it) and add `agent = true`.
  Keep its `user = "dev"` line: an entry without one stops defaulting to
  the template's user. Its host key was pinned in devvm's own
  `known_hosts`, so ssh to it once by hand to record it in
  `~/.ssh/known_hosts` (the detached daemon cannot answer the prompt). On
  the box, delete `/etc/profile.d/devvm.sh` and the root-owned agent and
  shim in `/usr/local/bin`: they are never updated again, and a bare
  `shell` would keep finding the stale shim.
- **With phases 3, 4 and 5**: update devvm on the laptop and every hub
  together (proxied flags, labels, then the minimum version).
- **`BROWSER` in a bare `shell` on an entry** is the user's to export
  (`~/.local/bin/devvm-open-url`): devvm writes no profile file on a box it
  does not shape. `attach` sets it in the tmux session, as now.

## Costs accepted

- A box devvm did not create cannot be shaped by devvm. Another cloud needs
  a provider, or shaping by hand.
- A smol VM made with raw smolvm, and an unlabelled server in the provider
  project, are invisible to devvm (including to `status`' forgotten-box
  guard).
- Repos in a template do not clone at `create` until `share-auth.md` lands:
  they are skipped with a hint, and the login is `gh auth login` inside
  `attach`.
- A forward the bridge made for a dev server is gone on detach and does not
  return on reattach; `ports add` once is the answer for a port worth
  keeping.

## Settled after the review

From the code review of 2026-11-20; settled with the user the same day.

- **`ports down` after step 11** holds until the next
  `attach`/`shell`/`start`/`ports up`: one rule, "connecting brings saved
  forwards up". `update` still does not respawn a daemon that `ports down`
  emptied.
- **No `--template-json` (step 17).** After the deletions only
  `--inline-template` and `--repo` are hidden, which is less than a new
  encoding.
- **A line for unlabelled provider servers** (with step 13): one `status`
  line ("N servers in the project without the devvm label"), from one
  unfiltered list call in the live round, so a billing server that lost its
  label is still noticed.
- **The template `user` field goes** (phase 4, after step 12): the login
  on a provider server is always `dev`. smol ignores the field, and
  provider `create` already refuses any template whose user differs from
  the default's. Cost: no other username on a devvm-made server. An entry's
  own `user` key is unaffected.
- **mosh leaves prereqs** (phase 2, with the dotfiles residue). Harden's
  firewall is `ufw default deny incoming` plus the ssh port over tcp, so
  mosh's UDP ports are closed on every hardened box: the package is
  installed and cannot be reached. `transport = "mosh"` stays for entries,
  where the user installs mosh and owns the firewall.

## Unknowns

- Which smolvm release added `--label` (the minimum devvm must require).
  1.16.1's `ls --json` already carries a `labels` field; its `create` flags
  were not checked.
- Whether harden's sshd reload mid-setup is safe on every image devvm
  supports (phase 1's live test).
- Whether smolvm 1.35.5 changed the exec, boot or stdout-cap behaviour
  devvm's comments pin to 1.16.1.
