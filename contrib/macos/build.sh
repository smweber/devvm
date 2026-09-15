#!/usr/bin/env bash
# Build DevVM.app with nothing but Apple's Command Line Tools (no Xcode):
#   ./build.sh [VERSION]
# VERSION defaults to `git describe`; release.yml passes the tag so the app's
# CFBundleShortVersionString matches the devvm binary it ships beside (the
# app compares the two to offer a reinstall).
set -euo pipefail
cd "$(dirname "$0")"

if [[ "$(uname -s)" != Darwin ]]; then
  echo "build.sh: DevVM.app can only be built on macOS (needs AppKit)" >&2
  exit 1
fi
command -v swiftc >/dev/null || { echo "build.sh: swiftc not found; run: xcode-select --install" >&2; exit 1; }

version=${1:-$(git describe --tags --always 2>/dev/null || echo 0.0.0)}
version=${version#v}

case "$(uname -m)" in
  arm64)  target=arm64-apple-macos13.0 ;;
  x86_64) target=x86_64-apple-macos13.0 ;;
  *) echo "build.sh: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

app=dist/DevVM.app
rm -rf "$app" dist/devvm-menubar.zip dist/devvm-menubar.zip.sha256
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"

echo "compiling ($target, version $version)…"
swiftc -O -target "$target" \
  -framework AppKit -framework UserNotifications -framework ServiceManagement \
  -o "$app/Contents/MacOS/DevVM" \
  Sources/*.swift

# Stamp the version into the bundle's Info.plist (the source keeps 0.0.0).
sed -e "s|<string>0.0.0</string>|<string>$version</string>|g" Info.plist >"$app/Contents/Info.plist"
printf 'APPL????' >"$app/Contents/PkgInfo"

# Ad-hoc signature: enough to run locally and, when the zip is fetched by
# `devvm menubar` (no quarantine flag), enough to open without Gatekeeper.
codesign --force --deep -s - "$app"

# ditto preserves the bundle's structure and resource metadata; a plain zip
# does not always.
ditto -c -k --keepParent "$app" dist/devvm-menubar.zip
(cd dist && shasum -a 256 devvm-menubar.zip >devvm-menubar.zip.sha256)

echo
echo "built $app"
echo "  open:    open $app"
echo "  install: ditto $app ~/Applications/DevVM.app && open ~/Applications/DevVM.app"
echo "  zip:     dist/devvm-menubar.zip ($(cut -c1-12 dist/devvm-menubar.zip.sha256)…)"
