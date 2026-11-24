# DevVM menu bar app

A macOS menu bar companion for `devvm`. It is a thin shell: every action is a
`devvm` invocation, and the machine list comes from `devvm status --plain
--watch`, so it shows what the CLI would and nothing else. It never opens a
terminal; `attach` and `shell` stay in your own terminal.

Needs macOS 14 or later.

What it does:

- **Machine list**, live, in sections: *Local* (smol VMs), *Remote* (your
  `[machines.*]` boxes), one per provider (*Hetzner*), then one per hub. A
  row is a glyph and the name (● running or reachable, ○ stopped, ◌
  anything unsettled), with a badge only when there is something to say:
  `stopped`, `unreachable`, `not checked`, `reconnecting`, `2 ports`. The
  app keeps one `status --plain --watch` child and redraws when devvm
  reports a change. No polling. Opening the menu also runs a one-off
  `status --plain`, which is how a VM stopped behind devvm's back gets
  noticed. A devvm.toml that does not parse is one row that opens the file.
- **Hubs**: machines on another host running devvm (`HUB/NAME` in the
  CLI) are listed under a header for the hub itself. When a hub does not
  answer, its header says so and its machines show as *unreachable* (the
  last listing devvm saw) until it is back; nothing on them can be chosen
  meanwhile. Every action on them is the same `devvm` call with the full
  `HUB/NAME`.
- **Per machine**: Start or Stop (a provider's server too; stopping one
  first says it is still billed), one ports item (*Ports up* when the saved
  forwards are down, *Ports down* when they are up, *Reconnect now* while
  they reconnect), each forwarded port opened in the browser, Copy in…
  (file picker), Copy out…, and the inbox folder for drops (default `~`).
- **Drop shelf**: *Show Drop Shelf* in the menu opens a small floating
  panel with one tile per running machine (hub machines included), in the
  menu's order. A tile is that machine's copy surface:
  - **drop** files on it to copy them into the machine's inbox, which the
    tile names (`devvm cp-in -t <inbox> NAME -- files…`, `-r` added when a
    folder is among them). One drop is one invocation and one
    notification; the tile shows a spinner while it copies, then ✓ or ✗
    for a few seconds. If a file already exists the notification offers
    **Replace** (re-runs with `-f`);
  - **click** it for Copy out…;
  - **right-click** it for the machine's menu (the same items as its
    submenu in the menu bar).

  The shelf floats above other windows on every Space (and beside
  full-screen apps) without taking focus from the app you drag out of, can
  be dragged anywhere (by a tile too), remembers its position and whether
  it was open across launches, and stays open until you close it or choose
  *Hide Drop Shelf*. It opens only from the menu: nothing watches your
  drags. Tiles never move while the pointer is over the shelf: a machine
  that went away meanwhile keeps its tile, dimmed and refusing drops,
  until the pointer leaves, so a drop cannot land on a neighbour that slid
  into place. Pointing at the shelf (or showing it) also refreshes the
  listing, at most every 30 seconds, as opening the menu does. (The status
  item itself is not a drop target: dragging to the menu bar sets off
  Mission Control.)
- **Icon**: a filled box when any machine is running; a badge dot when any
  machine's forwards are reconnecting (laptop just woke, host unreachable);
  an outline when nothing is running.
- **Copy out…** and **Inbox folder…**: a guest path field with shell-style
  Tab completion backed by `devvm __complete`. Tab completes a unique match
  (a directory keeps its `/`), otherwise extends to the matches' common
  prefix; with nothing left to extend, repeated Tabs cycle through the
  matches (Shift-Tab backwards), listed under the field as `n of m`. Typing
  ends the cycle. While cycling, Tab moves to the next candidate; to
  descend into a directory, type a character (zsh menu-complete behaviour).
  For Copy out, confirming the path hides the field and opens a save panel
  (a file) or folder picker (a path ending in `/`) in front; cancelling
  that brings the path field back to fix the path, and closing the path
  window ends it. The field keeps the last path while the app runs.
- **Updates** and **Launch at login** live under *DevVM* at the foot of the
  menu. *Check for updates…* runs `devvm update --check`; updating runs
  `devvm update` and relaunches the app. When a newer release exists, or
  the app's version and the CLI's differ, an item for it (*Update devvm
  to…*, *Reinstall menu bar app*, which runs `devvm menubar`) appears at
  the top of the menu.

## Build

Requires Apple's Command Line Tools only (`xcode-select --install`), no
Xcode:

```sh
cd contrib/macos
./build.sh            # version from git describe
./build.sh v0.2.0     # or pin the version (release.yml passes the tag)
open dist/DevVM.app
```

`build.sh` compiles `Sources/*.swift` with `swiftc` for both arm64 and
x86_64 and `lipo`s them into a universal `dist/DevVM.app` (the release runner
is Apple Silicon, and an arm64-only app cannot launch on an Intel Mac), stamps
the version into `Info.plist`, ad-hoc signs it, and zips it to
`dist/devvm-menubar.zip` with a `.sha256` beside it. `DEVVM_APP_ARCH=host
./build.sh` builds only this machine's slice for faster local iteration.

To install by hand: `ditto dist/DevVM.app ~/Applications/DevVM.app`. The
supported route once a release exists is `devvm menubar`, which downloads
`devvm-menubar.zip` for your CLI's version from the GitHub release, verifies
it against its `.sha256` sidecar, installs it into `~/Applications` (quitting
a running copy first), and opens it. Run it again to open the app; it only
re-downloads when the installed version differs (`--force` reinstalls,
`--version vX.Y.Z` pins a release for dev builds). After that, `devvm update`
keeps the app at the CLI's version, relaunching it if it was running. The
release workflow builds the zip on a macOS runner and attaches it to every
tagged release.

## How it finds devvm

A GUI app launched from Finder or at login gets a bare `PATH`
(`/usr/bin:/bin:/usr/sbin:/sbin`), which would hide both `devvm` and the
tools it shells out to (`smolvm`, `ssh`, `scp`, `gh`). At launch the app
asks your login shell for its `PATH` (`$SHELL -l -c /usr/bin/env`, five-second
limit) and also appends `~/.local/bin`, `/opt/homebrew/bin`, and
`/usr/local/bin`. Every child gets that `PATH`. If your shell only sets
`PATH` in an interactive-only rc file (`.zshrc` rather than `.zprofile`),
move the export or rely on the appended defaults.

If `devvm` is still not found, the menu says so and offers only Quit.

## Gatekeeper

The app is ad-hoc signed (no Apple developer account). That is fine for:

- an app you built locally (no quarantine flag), and
- the zip fetched by `devvm menubar` (downloads made by devvm carry no
  quarantine flag either).

A zip downloaded in a browser *is* quarantined and macOS will refuse to open
it. Clear the flag once:

```sh
xattr -d com.apple.quarantine ~/Applications/DevVM.app
```

## Launch at login

Tick *DevVM › Launch at login* in the menu. It uses `SMAppService`, which works with
ad-hoc signed apps; if it reports an error, add
`~/Applications/DevVM.app` under System Settings › General › Login Items
instead.

## Notes

- Notifications need the app to be a proper bundle; if the notification
  center is unavailable or denied, results show in a small floating panel
  that closes itself.
- A remote box's state is devvm's last probe of it (`reachable`,
  `unreachable`, or `?` when never probed or behind a `ProxyJump`), refreshed
  by the one-off `status` each menu open runs. A box at `?` still gets a
  tile, so a drop onto a host that is in fact down fails after ssh's connect
  timeout (10 seconds unless the entry sets `connect_timeout`), with the
  error in a notification.
- After `devvm update` replaces the CLI, the watch child keeps running the
  old binary until the app restarts it; the app does that when it notices
  the CLI version changed (on the next menu open), and the update flow
  relaunches the app anyway.

## Troubleshooting

The app has no window, so it narrates to the unified log (Console.app, or
`log` in a terminal). Everything is under the subsystem
`com.smweber.devvm.menubar`, split into categories:

| category | what it records |
|---|---|
| `app`    | launch (version, bundle path), the resolved PATH and the `devvm` binary found, SIGTERM, termination |
| `devvm`  | every CLI invocation with its pid, exit status, and last stderr line |
| `drop`   | the shelf shown or hidden, each drag that reaches a tile and why it was accepted or refused, what was dropped, and the copy result |
| `status` | the `status --plain --watch` child (start, exit, restart delay), every snapshot applied, `ports list` results |
| `menu`   | every menu action (drop shelf, start, stop, ports, copy in/out, inbox, launch at login, quit) and its outcome; completion lookups at debug level |
| `notify` | notification authorization, every notification posted, fallbacks to the toast panel, Replace actions |
| `update` | update checks and their result, Update now / Reinstall, the relaunch path taken |

Live, while you reproduce (drop a file on the shelf, open the menu):

```sh
log stream --predicate 'subsystem == "com.smweber.devvm.menubar"' --level info
```

Add `--level debug` to also see the per-keystroke completion lookups of the
Copy out panel. After the fact, the last ten minutes:

```sh
log show --last 10m --predicate 'subsystem == "com.smweber.devvm.menubar"' --info
```

One category only, for example drops:

```sh
log stream --predicate 'subsystem == "com.smweber.devvm.menubar" AND category == "drop"'
```

Values are logged in the clear on purpose (paths, machine names, the last
stderr line); a command's full output is never logged.

To run a local build instead of the installed one:

```sh
cd contrib/macos && ./build.sh && open dist/DevVM.app
```
