#!/usr/bin/env bash
# bench.sh — latency and throughput of the real agsshub-hubd on this machine (go/cmd/hub-bench): builds
# this tree's daemon and, with OLD=<commit>, that commit's too (from `git archive`, nothing checked out), and
# runs each over loopback in a throwaway state dir. Prints a markdown table per hub and the load average.
#   test/bench.sh                      # this tree
#   OLD=48c91e6 test/bench.sh          # and the previous hub, for the forms both take
#   QUICK=1 test/bench.sh              # fewer iterations
set -euo pipefail
SRC="$(cd "$(dirname "$0")/.." && pwd)"
GO=go; command -v go >/dev/null 2>&1 || GO="$HOME/sdk/go/bin/go"
T="$(mktemp -d "${TMPDIR:-/tmp}/agsshub-bench.XXXXXX")"
trap 'rm -rf "$T"' EXIT
( cd "$SRC/go" && CGO_ENABLED=0 "$GO" build -o "$T/new-hubd" ./cmd/agsshub-hubd && "$GO" build -o "$T/hub-bench" ./cmd/hub-bench )
q=(); [ -z "${QUICK:-}" ] || q=(-quick)
timeout 3600 "$T/hub-bench" -hubd "$T/new-hubd" -lib "$SRC/lib" "${q[@]}" </dev/null
if [ -n "${OLD:-}" ]; then
  mkdir "$T/old"
  git -C "$SRC" archive "$OLD" | tar -x -C "$T/old"
  ( cd "$T/old/go" && CGO_ENABLED=0 "$GO" build -o "$T/old-hubd" ./cmd/agsshub-hubd )
  timeout 3600 "$T/hub-bench" -hubd "$T/old-hubd" -lib "$T/old/lib" -old "${q[@]}" </dev/null
fi
