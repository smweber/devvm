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

`build.sh` compiles `Sources/*.swift` with `swiftc`, assembles
`dist/DevVM.app`, stamps the version into `Info.plist`, ad-hoc signs it, and
zips it to `dist/devvm-menubar.zip` with a `.sha256` beside it.

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
