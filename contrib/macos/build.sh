#!/usr/bin/env bash
# Build DevVM.app with nothing but Apple's Command Line Tools (no Xcode):
#   ./build.sh [VERSION]
# VERSION defaults to `git describe`; release.yml passes the tag so the app's
# CFBundleShortVersionString matches the devvm binary it ships beside (the
# app compares the two to offer a reinstall).
#
# The bundle is universal (arm64 + x86_64) by default: the release runner is
# Apple Silicon, and an arm64-only app cannot launch on an Intel Mac (Rosetta
# only goes the other way). DEVVM_APP_ARCH=host builds just this machine's
# slice for faster local iteration.
set -euo pipefail
cd "$(dirname "$0")"

if [[ "$(uname -s)" != Darwin ]]; then
  echo "build.sh: DevVM.app can only be built on macOS (needs AppKit)" >&2
  exit 1
fi
command -v swiftc >/dev/null || { echo "build.sh: swiftc not found; run: xcode-select --install" >&2; exit 1; }

version=${1:-$(git describe --tags --always 2>/dev/null || echo 0.0.0)}
version=${version#v}
# CFBundleVersion must be period-separated integers; the short version keeps
# any git-describe suffix (e.g. 0.2.0-3-gabc123) for humans.
bundle_version=$(printf '%s' "$version" | sed 's/[^0-9.].*//')
bundle_version=${bundle_version:-0}

host_arch=$(uname -m)
case "${DEVVM_APP_ARCH:-universal}" in
  universal) arches="arm64 x86_64" ;;
  host)      arches=$host_arch ;;
  *) echo "build.sh: DEVVM_APP_ARCH must be 'universal' or 'host'" >&2; exit 1 ;;
esac
sdk=$(xcrun --show-sdk-path)

app=dist/DevVM.app
rm -rf "$app" dist/devvm-menubar.zip dist/devvm-menubar.zip.sha256 dist/slices
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources" dist/slices

slices=()
for arch in $arches; do
  echo "compiling ($arch, version $version)…"
  swiftc -O -target "$arch-apple-macos13.0" -sdk "$sdk" \
    -framework AppKit -framework UserNotifications -framework ServiceManagement \
    -o "dist/slices/DevVM-$arch" \
    Sources/*.swift
  slices+=("dist/slices/DevVM-$arch")
done
if [[ ${#slices[@]} -eq 1 ]]; then
  cp "${slices[0]}" "$app/Contents/MacOS/DevVM"
else
  lipo -create "${slices[@]}" -output "$app/Contents/MacOS/DevVM"
fi
rm -rf dist/slices

# Stamp the versions into the bundle's Info.plist (the source keeps 0.0.0).
cp Info.plist "$app/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $version" "$app/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleVersion $bundle_version" "$app/Contents/Info.plist"
printf 'APPL????' >"$app/Contents/PkgInfo"

# Ad-hoc signature (after lipo — signing covers every slice): enough to run
# locally and, when the zip is fetched by `devvm menubar` (no quarantine
# flag), enough to open without Gatekeeper. No --deep: it is deprecated for
# signing and there is no nested code to reach.
codesign --force -s - "$app"

# ditto preserves the bundle's structure and resource metadata; a plain zip
# does not always.
ditto -c -k --keepParent "$app" dist/devvm-menubar.zip
(cd dist && shasum -a 256 devvm-menubar.zip >devvm-menubar.zip.sha256)

echo
echo "built $app ($arches)"
echo "  open:    open $app"
echo "  install: ditto $app ~/Applications/DevVM.app && open ~/Applications/DevVM.app"
echo "  zip:     dist/devvm-menubar.zip ($(cut -c1-12 dist/devvm-menubar.zip.sha256)…)"
