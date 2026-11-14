#!/bin/sh
# dev-hub-entrypoint — what systemd does for the hub on a VM, done here: host keys on first start, then
# agsshub-hubd as hub (it reaps by itself). HUBD_ARGS adds flags. This script stays the daemon's parent and
# passes it TERM/INT.
set -eu
ssh-keygen -A >/dev/null                       # /etc/ssh/ssh_host_*: setup.sh pins the ed25519 one
[ -f /etc/hub/ssh_host_ed25519_key ] ||
  install -o root -g hub -m 0640 /etc/ssh/ssh_host_ed25519_key /etc/hub/ssh_host_ed25519_key
# shellcheck disable=SC2086
setpriv --reuid=hub --regid=hub --init-groups \
  /usr/local/sbin/agsshub-hubd -listen :2422 -hostkey /etc/hub/ssh_host_ed25519_key -v ${HUBD_ARGS:-} &
pid=$!
trap 'kill -TERM "$pid" 2>/dev/null || true' TERM INT
rc=0; wait "$pid" || rc=$?
if kill -0 "$pid" 2>/dev/null; then rc=0; wait "$pid" || rc=$?; fi   # a signal ended the first wait
exit "$rc"
