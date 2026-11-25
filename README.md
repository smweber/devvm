# devvm

Persistent dev boxes behind one command. `devvm create` makes a Linux box (a
local [smolvm](https://smolmachines.com) microVM or a Hetzner Cloud server),
`devvm attach` drops you into a session there that survives disconnects, and
while you are attached the box's dev servers and login pages open in the
browser on your own machine.

## Quick start

```sh
# 1. smolvm runs the local VMs (1.35.5 or newer; devvm never installs it)
curl -sSL https://smolmachines.com/install.sh | bash

# 2. devvm (macOS or Linux), into ~/.local/bin
curl -fsSL https://raw.githubusercontent.com/smweber/devvm/master/install.sh | sh

# 3. a box
devvm create mybox      # asks a few questions; take the starter hook
devvm attach mybox      # a tmux session; Ctrl-b d detaches, it keeps running
```

`mybox` is Ubuntu 24.04 with a `dev` user (passwordless sudo), git, curl,
build tools, tmux and `gh`. The starter hook adds an editor, search tools
and a tmux that shows where you are and takes the mouse. Inside the box:

- `gh auth login` opens the login page in your browser, not the box's.
- Start a dev server and open its `localhost` URL from the box (anything
  that honours `$BROWSER`): devvm forwards the port and opens it for you.

The rest of the loop:

```sh
devvm status                      # every box, its state and forwards
devvm shell mybox                 # a plain login shell, no tmux
devvm exec mybox -- make test     # one command
devvm ports add mybox 3000        # keep a forward across sessions
devvm cp-in mybox ./notes.txt     # and cp-out
devvm stop mybox                  # start, delete
```

Every command explains itself with `--help`. `devvm completion bash|zsh|fish`
prints shell completion, which also completes box names and guest paths.
`devvm update` moves to the latest release.

## Making the box yours

Everything you configure is one file, `~/.config/devvm/devvm.toml`
(`devvm config edit`), safe to keep in your dotfiles. A template says how
`create` shapes a new box:

```toml
[templates.default]
memory = 4096
hook = "file:hook.sh"            # your setup script, beside this file
repos = ["owner/repo"]           # cloned into ~/src
share_auth = ["github"]          # copy this machine's gh login in
```

- **`hook`** is where your tools and dotfiles come from: a script devvm runs
  in the box as `dev`. `file:hook.sh` is a script on this machine, beside
  `devvm.toml` (so both travel in your dotfiles). Picking the starter in
  `create` writes `~/.config/devvm/hook.sh` for you: packages, a tmux
  config, a few shell lines. It is yours to edit, and `devvm setup NAME`
  runs it again. A hook can also be fetched (`url:URL [args]`), already in
  the box (`cmd:PATH [args]`), or `"starter"`: devvm's built-in starter as
  it ships, with no file to keep.
- **`share_auth = ["github"]`** copies the token from `gh auth token` into
  the box, so `owner/repo` clones work unattended. It carries your login's
  full scopes; leave it out to log in inside the box instead.

`devvm setup NAME` re-runs the shaping on an existing box. `--template T`
picks another `[templates.T]`; unset fields fall back to `default`.
`devvm config show` prints what each template resolves to.

## A cloud server

```toml
[providers.hetzner]
location = "nbg1"
image = "ubuntu-24.04"
ssh_key_file = "~/.ssh/id_ed25519.pub"
```

```sh
devvm providers token hetzner         # stores your API token on this machine
devvm create big --backend hetzner    # shows types and prices, then asks
```

The server gets the same setup as a local box, plus ssh hardening and a
firewall. It bills until `devvm delete`, stopped or not. `devvm providers
types hetzner` lists types, prices and availability.

## Boxes you already have

devvm connects to any Linux host you reach over ssh, and never changes it:

```sh
devvm config add work --backend remote --host work.example.com --user me
devvm attach work
```

`attach`, `shell`, `exec`, `ports`, `cp-in`/`cp-out` and `keys` all work on
it. Browser opens need devvm's small agent there: add `agent = true` to the
entry and devvm installs it in `~/.local/bin`.

A **hub** is another machine running devvm and smolvm, for example a
desktop. Add it with `--backend hub` and its VMs appear here as `HUB/NAME`:
`devvm create desk/mybox`, `devvm attach desk/mybox`.

## Good to know

- devvm only sees VMs and servers it created (they carry the label
  `devvm=true`). Others are invisible until you add that label by hand.
- Upgrading smolvm breaks running VMs: stop them all first.
- Browser opens work through `$BROWSER`, which `attach` sets for you. If a
  program in the box prints its URL instead of opening it, set `BROWSER`
  yourself: `/usr/local/bin/devvm-open-url` in a box devvm made,
  `~/.local/bin/devvm-open-url` on one of your own.
- `attach` and `shell` take `--transport mosh` on boxes reached over ssh.

## Menu bar app (macOS)

`devvm menubar` installs a small menu bar app: your boxes and their
forwards, start and stop, and a shelf to drag files in and out. It needs
macOS 14. Remove it by dragging `DevVM.app` out of `~/Applications`.

## Contributing

Building, the source layout and how releases are cut are in
[CONTRIBUTING.md](CONTRIBUTING.md); design docs are in
[docs/proposals](docs/proposals).
