# DevVM menu bar app

A macOS menu bar companion for `devvm`. It is a thin shell: every action is a
`devvm` invocation, and the machine list comes from `devvm status --plain
--watch`, so it shows what the CLI would and nothing else. It never opens a
terminal; `attach`, `shell`, and `auth` stay in your own tmux.

What it does:

- **Drop files on the icon** to copy them into the selected machine
  (`devvm cp-in -t <inbox> NAME files…`, `-r` added when a folder is among
  them). One drop is one invocation and one notification. If a file already
  exists the notification offers **Replace** (re-runs with `-f`).
- **Machine list** with state and forwards, live: the app keeps one
  `status --plain --watch` child and redraws when devvm reports a change.
  No polling. Opening the menu also runs a one-off `status --plain`, which
  is how a VM stopped behind devvm's back gets noticed.
- **Hubs**: machines on another host running devvm (`HUB/NAME` in the
  CLI) are listed in a section per hub, under a header row for the hub
  itself. When a hub does not answer its machines show as *unreachable*
  (the last listing devvm saw) until it is back; nothing on them can be
  chosen meanwhile. Every action on them is the same `devvm` call with the
  full `HUB/NAME`.
- **Icon**: a filled box when the selected drop target is running; a badge
  dot when any machine's forwards are reconnecting (laptop just woke, host
  unreachable); an outline when there is no usable drop target.
- **Per machine**: Start / Stop, Ports up / Ports down, open each forwarded
  port in the browser, Copy in… (file picker), Copy out… (a guest path field
  with Tab completion backed by `devvm __complete`, then a save panel or
  folder picker), and the inbox folder for drops (default `~`).
- **Updates**: *Check for updates…* runs `devvm update --check`; updating
  runs `devvm update` and relaunches the app. When the app's version and
  the CLI's differ, a *Reinstall menu bar app* item runs `devvm menubar`.
- **Launch at login** via the system login-items API.

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

Tick *Launch at login* in the menu. It uses `SMAppService`, which works with
ad-hoc signed apps on macOS 13+; if it reports an error, add
`~/Applications/DevVM.app` under System Settings › General › Login Items
instead.

## Notes

- Notifications need the app to be a proper bundle; if the notification
  center is unavailable or denied, results show in a small floating panel
  that closes itself.
- Remote machines always report `reachable` (devvm does not probe them), so
  a drop onto an unreachable host fails after ssh's 10-second connect
  timeout, with the error in a notification.
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
| `drop`   | each drag that reaches the icon and why it was accepted or refused, what was dropped, and the copy result |
| `status` | the `status --plain --watch` child (start, exit, restart delay), every snapshot applied, drop-target selection changes, `ports list` results |
| `menu`   | every menu action (start, stop, ports, copy in/out, inbox, launch at login, quit) and its outcome; completion lookups at debug level |
| `notify` | notification authorization, every notification posted, fallbacks to the toast panel, Replace actions |
| `update` | update checks and their result, Update now / Reinstall, the relaunch path taken |

Live, while you reproduce (drop a file, open the menu):

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
