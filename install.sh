#!/bin/sh
#
# Install devvm: the latest release binary for this machine, checked against
# the release's SHA256SUMS, into ~/.local/bin.
#
#   curl -fsSL https://raw.githubusercontent.com/smweber/devvm/master/install.sh | sh
#
# DEVVM_INSTALL_DIR=/somewhere/bin installs elsewhere; DEVVM_VERSION=vX.Y.Z
# pins a release. Afterwards `devvm update` keeps it current. (To build from
# a clone instead, see install-dev.sh.)
#
# Everything runs inside main, called on the last line, so a download cut
# short runs nothing.
set -eu

repo="smweber/devvm"

say() { printf '%s\n' "$*"; }
die() {
    printf 'devvm install: %s\n' "$*" >&2
    exit 1
}

main() {
    command -v curl >/dev/null 2>&1 || die "curl is needed"

    case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) die "no release for $(uname -s): devvm runs on macOS and Linux" ;;
    esac
    case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) die "no release for $(uname -m): devvm is built for amd64 and arm64" ;;
    esac
    asset="devvm-$os-$arch"

    if [ -n "${DEVVM_VERSION:-}" ]; then
        base="https://github.com/$repo/releases/download/$DEVVM_VERSION"
    else
        base="https://github.com/$repo/releases/latest/download"
    fi
    dir="${DEVVM_INSTALL_DIR:-$HOME/.local/bin}"

    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT INT TERM

    say "Downloading $asset..."
    curl -fsSL -o "$tmp/$asset" "$base/$asset" || die "could not download $base/$asset"
    curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || die "could not download $base/SHA256SUMS"

    want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
    [ -n "$want" ] || die "SHA256SUMS has no line for $asset"
    if command -v sha256sum >/dev/null 2>&1; then
        got=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
    elif command -v shasum >/dev/null 2>&1; then
        got=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
    else
        die "sha256sum or shasum is needed to check the download"
    fi
    [ "$got" = "$want" ] || die "checksum mismatch for $asset (got $got, want $want); nothing was installed"

    mkdir -p "$dir"
    chmod 755 "$tmp/$asset"
    # Into the directory first, then renamed over: never a half-written devvm,
    # and a running one keeps its old file.
    mv "$tmp/$asset" "$dir/.devvm.new"
    mv -f "$dir/.devvm.new" "$dir/devvm"

    say "Installed $("$dir/devvm" --version 2>/dev/null || echo devvm) to $dir/devvm"
    say ""
    case ":$PATH:" in
    *":$dir:"*) ;;
    *)
        say "$dir is not on your PATH. Add it (and open a new shell):"
        say "  export PATH=\"$dir:\$PATH\""
        say ""
        ;;
    esac
    if ! command -v smolvm >/dev/null 2>&1; then
        say "Local VMs need smolvm (1.35.5 or newer):"
        say "  curl -sSL https://smolmachines.com/install.sh | bash"
        say ""
    fi
    say "Next:"
    say "  devvm create mybox"
}

main "$@"
