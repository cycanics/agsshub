#!/bin/sh
# build.sh — build agsshub-hubd and hub-descriptor into go/bin/ (static; what install.sh and the dev hubs
# install). GO=<path to go> to choose the toolchain.
set -eu
cd "$(dirname "$0")"
GO="${GO:-go}"
command -v "$GO" >/dev/null 2>&1 || GO="$HOME/sdk/go/bin/go"
CGO_ENABLED=0 "$GO" build -trimpath -ldflags "-s -w" -o bin/ ./cmd/agsshub-hubd ./cmd/hub-descriptor
echo "built $(pwd)/bin/agsshub-hubd, hub-descriptor"
