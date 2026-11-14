#!/usr/bin/env bash
# full-disk.sh — go/hub's TestFullDisk on a real full disk: a container with a small tmpfs, which needs
# neither root here nor a mount. Run: test/full-disk.sh
set -euo pipefail
SRC="$(cd "$(dirname "$0")/.." && pwd)"
GO=go; command -v go >/dev/null 2>&1 || GO="$HOME/sdk/go/bin/go"
T="$(mktemp -d "${TMPDIR:-/tmp}/agsshub-fulldisk.XXXXXX")"; NAME="agsshub-fulldisk-$$"
trap 'docker rm -f "$NAME" >/dev/null 2>&1; rm -rf "$T"' EXIT
"$SRC/go/build.sh" >/dev/null
( cd "$SRC/go" && CGO_ENABLED=0 "$GO" test -c -o "$T/hub.test" ./hub )
timeout 600 docker run --rm --name "$NAME" --network none -u "$(id -u):$(id -g)" \
  --tmpfs "/small:size=2m,nr_inodes=1024,mode=1777" \
  -v "$T/hub.test:/t/hub.test:ro" -v "$SRC/lib:/src/lib:ro" -v "$SRC/test/tars:/src/test/tars:ro" \
  -v "$SRC/go/bin:/src/go/bin:ro" \
  -e HUB_TEST_SMALLFS=/small \
  -e HOME=/tmp -w /src/go/hub ubuntu:26.04 \
  /t/hub.test -test.run '^TestFullDisk$' -test.timeout 540s -test.v </dev/null
