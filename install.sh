#!/usr/bin/env bash
#
# Build the devvm HOST binary for this machine and install it (default:
# ~/.local/bin/devvm), so local changes are usable without cutting a GitHub
# release. Pass a directory to install somewhere else.
#
# Like release.sh, it refreshes the embedded guest agents first so the
# installed binary never carries a stale agent.
set -euo pipefail
cd "$(dirname "$0")"

dest="${1:-$HOME/.local/bin}"

./build.sh

# Same --version stamp as release.sh; local builds show tag+dirty state.
version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)

mkdir -p "$dest"
echo "building $dest/devvm ($version)"
CGO_ENABLED=0 go build -trimpath \
    -ldflags="-X github.com/smweber/devvm/internal/cli.Version=$version" \
    -o "$dest/devvm" ./cmd/devvm

echo "done: $dest/devvm"
