#!/usr/bin/env bash
# install.sh — set up the agsshub hub on a Debian/Ubuntu VM or in an image. Run as root on the HUB host.
#
# The hub is agsshub-hubd (go/bin/agsshub-hubd, built by go/build.sh): its own SSH server on its own
# port (HUBD_ARGS in /etc/default/agsshub-hubd, default :2422), the reaper inside it.
#
# Idempotent. Creates the service user, the registry/spool layout, installs the daemon, its data and the
# operator tools. Does NOT enroll any workspace (see hub-enroll). Without systemd as init (a container),
# starting the daemon is left to the caller — dev-hub/entrypoint.sh does it.
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "install.sh: run as root" >&2; exit 1; }
[ $# = 0 ] || { echo "usage: install.sh" >&2; exit 2; }
SRC="$(cd "$(dirname "$0")" && pwd)"
for b in agsshub-hubd hub-descriptor; do
  [ -x "$SRC/go/bin/$b" ] || { echo "install.sh: no $SRC/go/bin/$b — run go/build.sh" >&2; exit 1; }
done
SYSTEMD=0; [ -d /run/systemd/system ] && SYSTEMD=1

echo "== user & group"
getent group hub >/dev/null || groupadd --system hub
id hub >/dev/null 2>&1 || useradd --system --gid hub -m -d /var/lib/hub-home -s /bin/sh hub

echo "== helpers -> /usr/local/lib/hub"
# what the operator tools source and run, and what the daemon reads
install -d -o root -g root -m 0755 /usr/local/lib/hub
install -o root -g root -m 0644 "$SRC/lib/common.sh" "$SRC/lib/help.json" "$SRC/lib/help.txt" /usr/local/lib/hub/
install -o root -g root -m 0755 "$SRC/lib/mint" "$SRC/go/bin/hub-descriptor" /usr/local/lib/hub/
rm -f /usr/local/lib/hub/hub-expiry     # an earlier release's mailbox-merge helper

echo "== plugin seam -> /usr/local/lib/hub/backends"
cp -a "$SRC/lib/backends" /usr/local/lib/hub/
chown -R root:root /usr/local/lib/hub/backends
# .example templates stay non-executable; a wired backend is an executable `send`

echo "== operator tools -> /usr/local/sbin"
for t in hub-enroll hub-alias hub-topic hub-mint hub-budget; do
  install -o root -g root -m 0755 "$SRC/bin/$t" "/usr/local/sbin/$t"
done

echo "== registry & spool -> /var/lib/hub"
# One daemon, user hub, writes it all, so the registry is group-writable; claims/operator stays the
# operator's (root) alone.
install -d -o root -g hub -m 2750 /var/lib/hub
install -d -o hub  -g hub -m 2770 /var/lib/hub/claims
install -d -o hub  -g hub -m 2770 /var/lib/hub/claims/.byfpr /var/lib/hub/claims/.tmp \
                                  /var/lib/hub/claims/.tomb  /var/lib/hub/claims/.serial
install -d -o root -g hub -m 2750 /var/lib/hub/claims/operator # hub-mint writes; the daemon reads
install -d -o hub  -g hub -m 2770 /var/lib/hub/claims/instance /var/lib/hub/claims/.ibyfpr
[ -e /var/lib/hub/claims/.lock ] || install -o hub -g hub -m 0640 /dev/null /var/lib/hub/claims/.lock
install -d -o hub  -g hub -m 2770 /var/lib/hub/inbox           # the spool
install -d -o hub  -g hub -m 2770 /var/lib/hub/flood           # per-(sender,recipient) token buckets
install -d -o root -g hub -m 2750 /var/lib/hub/aliases         # operator writes (hub-alias); the daemon reads
install -d -o root -g hub -m 2750 /var/lib/hub/budgets         # operator writes (hub-budget); the daemon reads
install -d -o root -g hub -m 2770 /var/lib/hub/topics          # operator writes policy (hub-topic); the daemon writes subs
install -d -o hub  -g hub -m 2770 /var/lib/hub/subs            # subscription name -> record links
install -d -o hub  -g hub -m 2770 /var/lib/hub/described       # the daemon writes; hub-mint removes
[ -e /var/lib/hub/claims/.journal ] || install -o hub -g hub -m 0660 /dev/null /var/lib/hub/claims/.journal   # kept on re-run
chmod 0660 /var/lib/hub/claims/.journal
shopt -s nullglob
for d in /var/lib/hub/claims/ws-*/; do chmod 2770 "$d"; done   # each workspace's registrar claims
shopt -u nullglob

echo "== retired: the forced-command hub"
# a hub that ran sshd's forced commands: its drop-in, reaper timer and helpers would serve, and reap,
# beside the daemon
rm -f /etc/ssh/sshd_config.d/60-hub.conf /etc/tmpfiles.d/hub.conf /etc/hub/impl
rm -rf /usr/local/lib/hub/dispatch /usr/local/lib/hub/registrar /usr/local/lib/hub/authkeys \
       /usr/local/lib/hub/hub-reaper /usr/local/lib/hub/responders /usr/local/lib/hub/frontends
if [ "$SYSTEMD" = 1 ]; then
  systemctl disable --now hub-reaper.timer 2>/dev/null || true
  rm -f /etc/systemd/system/hub-reaper.service /etc/systemd/system/hub-reaper.timer
fi
if command -v sshd >/dev/null 2>&1; then
  sshd -t
  [ "$SYSTEMD" = 0 ] || systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true
fi

echo "== registrars authorized_keys -> /etc/hub"
install -d -o root -g root -m 0755 /etc/hub /etc/hub/registrars
[ -f /etc/hub/registrars/authorized_keys ] || install -o root -g root -m 0644 /dev/null /etc/hub/registrars/authorized_keys

echo "== the message-file format (docs/wake-live-event-id.md, Upgrade)"
# Once, on a hub of an earlier format: every mailbox (identity, instance and subscription, their .expired), the whole subscription
# index (topics/*/subs/*, subs/*) and every topics/*/record go; consumers
# re-subscribe at start. The daemon is stopped first. /var/lib/hub/.format marks it done, so a re-run keeps
# what the new hub stored.
FORMAT=2
if [ "$(cat /var/lib/hub/.format 2>/dev/null)" != "$FORMAT" ]; then
  [ "$SYSTEMD" = 0 ] || systemctl stop agsshub-hubd ssh-chat-hubd 2>/dev/null || true  # ssh-chat-hubd: its name before the rename
  find /var/lib/hub/inbox -mindepth 1 -maxdepth 1 -exec rm -rf {} +
  find /var/lib/hub/subs -mindepth 1 -maxdepth 1 -exec rm -rf {} +
  shopt -s nullglob
  for td in /var/lib/hub/topics/*/; do
    rm -f "$td/record" "$td"/.record.*
    [ ! -d "$td/subs" ] || find "$td/subs" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
  done
  shopt -u nullglob
  printf '%s\n' "$FORMAT" > /var/lib/hub/.format
  chown root:hub /var/lib/hub/.format; chmod 0640 /var/lib/hub/.format
  echo "   wiped: mailboxes, subscriptions, topic records (format $FORMAT)"
fi

echo "== agsshub-hubd -> /usr/local/sbin"
# an install from before the rename: stop ssh-chat-hubd (same port), keep its flags, drop its files
if [ -f /etc/systemd/system/ssh-chat-hubd.service ]; then
  [ "$SYSTEMD" = 0 ] || systemctl disable --now ssh-chat-hubd 2>/dev/null || true
  rm -f /etc/systemd/system/ssh-chat-hubd.service
fi
[ -f /etc/default/ssh-chat-hubd ] && [ ! -f /etc/default/agsshub-hubd ] && mv /etc/default/ssh-chat-hubd /etc/default/agsshub-hubd
rm -f /usr/local/sbin/ssh-chat-hubd
install -d -o root -g root -m 0755 /etc/systemd/system
install -o root -g root -m 0755 "$SRC/go/bin/agsshub-hubd" /usr/local/sbin/agsshub-hubd
install -o root -g root -m 0644 "$SRC/systemd/agsshub-hubd.service" /etc/systemd/system/agsshub-hubd.service
[ -f /etc/default/agsshub-hubd ] || printf '# agsshub-hubd flags (agsshub-hubd -h)\nHUBD_ARGS="-listen :2422"\n' > /etc/default/agsshub-hubd
# its host key: the machine's own when there is one (a client that pins it by key keeps its pin)
if [ ! -f /etc/hub/ssh_host_ed25519_key ]; then
  if [ -f /etc/ssh/ssh_host_ed25519_key ]; then
    install -o root -g hub -m 0640 /etc/ssh/ssh_host_ed25519_key /etc/hub/ssh_host_ed25519_key
    install -o root -g root -m 0644 /etc/ssh/ssh_host_ed25519_key.pub /etc/hub/ssh_host_ed25519_key.pub
  else
    ssh-keygen -q -t ed25519 -N '' -C agsshub-hubd -f /etc/hub/ssh_host_ed25519_key
    chgrp hub /etc/hub/ssh_host_ed25519_key; chmod 0640 /etc/hub/ssh_host_ed25519_key
  fi
fi
if [ "$SYSTEMD" = 1 ]; then
  systemctl daemon-reload
  systemctl enable agsshub-hubd
  systemctl restart agsshub-hubd
else
  echo "   no systemd: agsshub-hubd not started (dev-hub/entrypoint.sh does that)"
fi
# a topic's publish ledger and pending records, once the daemon that wrote them is gone: nothing reads them
rm -rf /var/lib/hub/topics/*/seen /var/lib/hub/topics/*/pending
# and its staged copies of each publication, in a mailbox named for the topic: only a name under topics/
# with a policy, of the topic role — never a subscription's or an identity's mailbox
shopt -s nullglob
for td in /var/lib/hub/topics/*/; do
  t="${td%/}"; t="${t##*/}"
  case "$t" in topic+?*|topic.?*) [ -f "$td/policy" ] && rm -rf "/var/lib/hub/inbox/$t" ;; esac
done
shopt -u nullglob

echo "== done. Next: enroll each workspace (hub-enroll) and mint the operator identities (hub-mint)."
