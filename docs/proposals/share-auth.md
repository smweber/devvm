# Proposal: share the host's logins with a new box (`share_auth`)

Status: proposed, 2026-11-20, from a design discussion with the user. Not
scheduled, nothing built. **Built after `simplify.md`** and written against
its model: `auth`, `repos add` and "managed" are gone, `setup` applies only
to boxes devvm created (smol VMs, provider servers), and a failed clone no
longer ends setup. First and only tool in this cut: GitHub. Once agreed,
the order of work goes in `ROADMAP.md`.

## Problem

`create NAME` with `repos` in the template cannot clone them. `setup` clones
`owner/repo` specs with `gh repo clone` in the guest, and nothing has logged
the guest's `gh` in at that point. After `simplify.md` the damage is
contained (keys and hardening run first, and logged-out repos are skipped
with a hint), but the repos still do not arrive: the user attaches, runs
`gh auth login` through the browser bridge, and clones by hand. That is an
interactive step per box, and one GitHub token per box.

## Goals

- A box made from an opted-in template has working `git` and `gh` when
  `create` returns: private clones, pushes, PRs. No browser step, so it
  works under `--yes` and for agents running in the box while nobody is
  attached.
- One line of config, no per-VM step, no step on github.com.
- Nothing is copied unless the user's file says so.
- A config shape other tools (claude, codex) can join without a rename.

## Non-goals

- **Not a credential manager.** No rotation, no expiry tracking, no
  per-VM tokens, no revocation. devvm copies a login the host already has.
- **No narrower credential than the host's.** The guest gets the host
  token's scopes, as a browser `gh auth login` in the box gives it today. A
  scoped PAT is the user's to arrange (§2, `GH_TOKEN`).
- **No claude or codex in this cut** (§7).
- **Nothing for entries.** `setup` never runs on a `[machines.*]` box, so
  nothing is copied to one.

## Design

### 1. What goes in the file

```toml
[templates.default]
share_auth = ["github"]     # tools whose host login setup copies into the box
```

- A list of tool names; `"github"` is the only one. An unknown name is a
  load error (the loader is strict), reported by `config check`.
- Built-in: empty. Like every template field it falls back to
  `[templates.default]` one level, and `share_auth = []` in a named
  template turns it off there: that is the template for boxes that run
  untrusted code, or for cloud servers the user would rather not hold a
  token (§6).
- `config show` prints it with its source, like the other fields.
- No visible flag. A laptop-resolved template carries it to a hub the way
  `simplify.md` step 17 carries the other fields (§5).

### 2. The setup step

`setup` gains one step, before the hook:

ensure user → prereqs → agent + shim → keys → harden → **share auth** →
hook → repos

Before the hook, so a hook that clones private dotfiles works. That means
the hook runs with the credential present (§6). After harden, so a problem
here can never cost a box its hardening.

For `"github"`:

1. **Guest check** (one exec): `gh api user` succeeds → the guest already
   has a working login; leave it alone, say so in one line, done. Whose
   login it is is not compared: an existing login always wins.
2. **Read the host's token**: `gh auth token --hostname github.com`
   (gh ≥ 2.17). No `gh`, no login, or a keychain read that fails → one
   warning line, the step is over, setup continues.
3. **Refuse an expiring login.** `gh auth status --json hosts` reporting
   `refreshable` for github.com means a copy dies at the host's next
   refresh. One line naming `gh auth login --expiring=false`; setup
   continues. A `gh` too old for `--json` cannot hold an expiring login.
4. **Install** (one exec, as the login user, token on stdin):
   `unset GH_TOKEN GITHUB_TOKEN`, then `gh auth login --hostname github.com
   --with-token` (`--insecure-storage` when the guest's `gh` knows the
   flag), `gh auth setup-git`, `gh config set git_protocol https --host
   github.com`.
5. **One line** on success, naming the host the login came from.

The token goes over the exec's stdin only: never argv, env, a file on the
host, or a log. A failed install is a warning, never a failed setup.

**smol needs a backend change for that.** A one-shot smol exec has no
stdin today: `execArgv` (`backend/smol.go`) passes `-i` ("keep stdin
open") only for a spawn or a TTY, which is why `agentbin` and
`bootstrap/browser.go` write files with `printf` arguments instead. ssh is
fine (`keys` already pipes `authorized_keys` that way). So `ExecOpts`
gains a field that makes a one-shot smol exec pass `-i`, and a live test
on a real smolvm must show the guest command sees EOF when the host
closes the pipe (else: `Spawn`, write, close). Without it the share works
on a provider server and fails on every smol VM, local or on a hub.

"The host" is whichever host runs `setup`. `gh auth token` honours
`GH_TOKEN`/`GITHUB_TOKEN`, so a user who exports a PAT shares that instead;
devvm does not second-guess it.

### 3. Repos

The repos step is `simplify.md` phase 1's, unchanged: a failed clone never
ends setup, and `owner/repo` specs are skipped with one line when the
guest's `gh` has no working login. This proposal changes only that line's
hint, which now names `share_auth = ["github"]` first and `gh auth login`
in the box second. With the share in place the guest is logged in before
the repos step, so the skip no longer happens on an opted-in template.

No logged-out fallback for public `owner/repo` specs.

### 4. Repair

There is no `auth` command. When a box's copy stops working (the host's
token was revoked or replaced):

- `devvm setup NAME` again, from any laptop: step 1's guest check fails on
  a dead login, and steps 2–4 copy the host's current one. Setup is safe to
  re-run.
- Or `gh auth login` inside `attach`, through the browser bridge: a login
  of the box's own, which the guest check then leaves alone.

### 5. Hubs

`setup HUB/NAME` already runs on the hub, so the hub is "the host": a hub
VM gets **the hub's** `gh` login. The laptop's token never crosses the hub
connection and nothing is stashed there. Cost: one `gh auth login` on each
hub, once; a hub that shares auth needs `gh` beside `devvm` and `smolvm`.

- No `--template`: the hub resolves its own `[templates.default]`, as it
  does for every field.
- `--template T` (resolved on the laptop): `share_auth` travels with the
  rest of the laptop-resolved template as one more hidden flag, beside
  `--repo` (`simplify.md` step 17). A hub
  older than it refuses; the minimum version is pinned by a test, like
  `hubMinVersion`, and the refusal maps to the `devvm update` line.

Hub VMs are therefore independent of any laptop's login.

### 6. Trust

- The guest holds the host token's full scopes (`repo`, `read:org`, `gist`
  for a default `gh` login), in `~/.config/gh/hosts.yml`, as after a
  browser `gh auth login` in the box. Anything running as the user or root
  in the box can use it.
- The hook runs with it. A `url:` hook is remote code.
- **Cloud servers hold it too.** A template that inherits `share_auth` from
  `[templates.default]` puts the token at rest on a provider server, with
  no deliberate step per box. `share_auth = []` in that template opts it
  out.
- One token, many boxes: revoking it logs out the host and every box. That
  is no worse than a login per box, since gh tokens cannot be revoked one
  at a time (only the whole GitHub CLI authorization). Recovery is `gh auth
  login` on the host, then `devvm setup NAME` per box (§4).
- A token that lands in a public repo is revoked by GitHub's secret
  scanning, with the same effect.
- Off by default, only on boxes devvm created, and never written to
  `devvm.toml` or any host state.

### 7. Later tools

`share_auth` is a list so `"claude"` and `"codex"` can join as recipes
behind the same step. Neither is designed. Both need research first:
whether their credentials rotate on refresh (a copy would then break, as an
expiring gh login does) and what each vendor supports for headless use.

## Facts this rests on

Read from the cli/cli source and GitHub's docs on 2026-11-20, by research
agents; not re-verified by hand.

- `gh auth login --with-token` checks only that the token works and has
  `repo` + `read:org`; nothing checks its type. It never configures git,
  hence `setup-git`. It refuses while `GH_TOKEN` is set in its environment.
- Guest `gh` as old as Ubuntu 22.04's 2.4.0 has `--with-token`,
  `setup-git` and `gh api`. `--insecure-storage` is 2.26+.
- GitHub keeps ten tokens per user/app/scope set and revokes unused, then
  least recently used, ones: a login per VM runs into it, one shared token
  does not.
- Expiring gh logins are opt-in (gh 2.103+), planned as the default for new
  sign-ins with no date; existing logins are unaffected.
- `gh auth logout` does not revoke. cli/cli PR #15100 would change that: a
  host logout would then end every box's copy.
- Nothing in GitHub's terms addresses using one's own token on one's own
  machines. Not confirmed: that expiring tokens keep the `gho_` prefix (so
  §2 uses `--json`, not the prefix).

## Alternatives considered

- **Browser login during setup**: interactive, and one token per VM hits
  the ten-token cap.
- **Fine-grained PAT stored by devvm**: narrower, but manual steps on
  github.com and one token per owner. Still available through `GH_TOKEN`.
- **Laptop token to hub VMs** (stashed on the hub): more code and a token
  at rest on the hub; replaced by §5.
- **Per-VM ssh key uploaded to GitHub**: individually revocable, but needs
  `admin:public_key`, leaves stray keys, and gives no `gh`.
- **ssh agent forwarding**: nothing at rest, but only while attached, and
  it exposes every key in the agent.
- **Clone on the host, ship a bundle**: no credential in the guest, so no
  push either.

## Implementation order

After `simplify.md` has landed (its phase 1 already made clones non-fatal
and added the logged-out skip; its step 17 left the hidden flags a
template reaches a hub by).

0. Stdin on a one-shot smol exec (§2), with a live test on real smolvm.
   Everything else waits on this.
1. `share_auth` in the loader, `config show`/`check`, the hub transport.
2. The setup step for `"github"`, with the host's `gh` behind a var and the
   guest faked. Tests: existing login left alone; expiring refused; no
   host login warns; the token is in stdin and nowhere else; harden has
   already run when the step fails.
3. The repos skip's hint names `share_auth`.
4. Hubs: the minimum-version test.
5. Docs: `config.md` §3, `CLAUDE.md` (setup order), the create hints.

No guest agent rebuild; no `--plain` change, so no Swift change.

## Open questions

- Should `create` on a TTY offer the share when the template has repos and
  the key is unset? Left out; the skip hint may be enough.
- If cli/cli #15100 lands, should `status` notice a box whose copy died?
