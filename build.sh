#!/usr/bin/env bash
#
# Cross-compile the devvm-agent guest binaries and drop them into
# internal/agentbin/bin/ so go:embed bakes them into the host binary. Re-run
# whenever cmd/devvm-agent (or anything it imports) changes; commit the results.
#
# The build must be reproducible so the committed bytes match a fresh CI build
# (the release workflow's drift-check). -trimpath strips paths; -buildvcs=false
# strips the git revision/dirty stamp Go embeds by default, which otherwise
# differs between a dirty local tree and CI's clean tagged checkout.
#
# Guests are always linux; we build both arches devvm supports.
set -euo pipefail

cd "$(dirname "$0")"
out="internal/agentbin/bin"
mkdir -p "$out"

for arch in amd64 arm64; do
    echo "building devvm-agent linux/$arch"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
        go build -trimpath -buildvcs=false -ldflags='-s -w' \
        -o "$out/devvm-agent-linux-$arch" ./cmd/devvm-agent
done

echo "done: $out"
