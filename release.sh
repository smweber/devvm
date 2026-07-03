#!/usr/bin/env bash
#
# Cross-compile the devvm HOST binary for every supported host platform into
# dist/, for upload as GitHub release assets. bootstrap.sh downloads these so a
# host needs no Go toolchain.
#
# Each host binary embeds the linux guest agents (see build.sh), so it is
# self-contained. The guest is always linux; the host may be macOS or linux.
set -euo pipefail
cd "$(dirname "$0")"

# Refresh the embedded guest agents so releases never ship a stale one.
./build.sh

rm -rf dist && mkdir -p dist
for p in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
    os=${p%/*} arch=${p#*/}
    out="dist/devvm-$os-$arch"
    echo "building $out"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
        go build -trimpath -ldflags='-s -w' -o "$out" ./cmd/devvm
done

(cd dist && sha256sum devvm-* >SHA256SUMS)
echo "done: dist/"
