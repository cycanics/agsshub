#!/usr/bin/env bash
# dev-hub.sh — the hub through real ssh: builds the daemon and the dev hub image, starts it under a
# throwaway compose project on a free loopback port, runs dev-hub/setup.sh, and drives it with ssh and
# `docker exec` (the operator). Also re-runs install.sh in the container. Removes its container, network
# and image on exit.
# Needs docker with the compose plugin. Exits nonzero if any check fails. Run: test/dev-hub.sh
set -uo pipefail
SRC="$(cd "$(dirname "$0")/.." && pwd)"
T="$(mktemp -d "${TMPDIR:-/tmp}/agsshub-devhub.XXXXXX")"
export COMPOSE_PROJECT_NAME="agsshub-devhub-test-$$" DEV_HUB_IMAGE="agsshub-dev-hub:test-$$" \
       DEV_HUB_NETWORK="agsshub-devhub-test-$$" DEV_HUB_PORT= DEV_HUB_HUBD_ARGS="-reap-interval 1s"
DC=(docker compose -f "$SRC/dev-hub/compose.yaml")
cleanup(){
  "${DC[@]}" down -v --remove-orphans >/dev/null 2>&1
  docker image rm "$DEV_HUB_IMAGE" >/dev/null 2>&1
  rm -rf "$T"
}
trap cleanup EXIT

PASS=0; FAIL=0
expect_eq(){ if [ "$2" = "$3" ]; then echo "  ok   $1"; PASS=$((PASS+1)); else echo "  FAIL $1: got [$2] want [$3]"; FAIL=$((FAIL+1)); fi; }
expect_has(){ case "$2" in *"$3"*) echo "  ok   $1"; PASS=$((PASS+1));; *) echo "  FAIL $1: [$2] lacks [$3]"; FAIL=$((FAIL+1));; esac; }
expect_code(){ if [ "$2" = "$3" ]; then echo "  ok   $1"; PASS=$((PASS+1)); else echo "  FAIL $1: exit $2 want $3"; FAIL=$((FAIL+1)); fi; }
# the read stream (docs/wake-live-event-id.md, Reading): the msgids of its chunks of one part (default msg)
cids(){ grep -ao "^[0-9a-f]*;id=[^;]*;part=${1:-msg}"$'\r' | sed 's/^[0-9a-f]*;id=\([^;]*\);.*/\1/'; }
EMPTY=$'0\r\n\r'                                  # an empty listing, as $(...) leaves it

echo "# up"
for g in go "$HOME/sdk/go/bin/go"; do command -v "$g" >/dev/null 2>&1 && { "$SRC/go/build.sh" >&2 || exit 1; break; }; done   # no toolchain: the binary built before
"${DC[@]}" up -d --build --wait > "$T/up.log" 2>&1; rc=$?
expect_code "compose builds and starts a healthy hub" "$rc" 0
[ "$rc" = 0 ] || { tail -20 "$T/up.log"; exit 1; }
C="$("${DC[@]}" ps -q hub)"
port="$("${DC[@]}" port hub 2422)"
expect_has "published on loopback only" "$port" "127.0.0.1:"
expect_eq "tini is PID 1" "$(docker exec "$C" ps -o comm= -p 1)" "tini"

echo "# setup"
CL="$T/client"
"$SRC/dev-hub/setup.sh" --out "$CL" commhub@home.ssh group.demo@expa.ssh run.demo+t1@expa.ssh > "$T/setup.out" 2> "$T/setup.err"; rc=$?
expect_code "setup enrolls, mints and logs every identity in" "$rc" 0
[ "$rc" = 0 ] || { cat "$T/setup.err"; exit 1; }
expect_eq "three identities, the run's /<ws> appended through the registrar" "$(cut -f1 "$CL/identities" | tr '\n' ' ')" \
  "commhub@home.ssh group.demo@expa.ssh run.demo+t1/ws-dev@expa.ssh "
expect_has "setup prints the client command" "$(cat "$T/setup.out")" "ssh -F $CL/ssh_config commhub.home id"
H(){ ssh -F "$CL/ssh_config" "$@"; }
. "$CL/env"
HK(){ local k="$1"; shift                       # one key, no config: the command line setup prints
  ssh -F /dev/null -p "$DEV_HUB_PORT" -i "$k" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 \
      -o HostKeyAlias="$DEV_HUB_HOST_KEY_ALIAS" -o UserKnownHostsFile="$DEV_HUB_KNOWN_HOSTS" \
      -o StrictHostKeyChecking=yes "hub@$DEV_HUB_HOST" "$@"; }
HUBID=commhub@home.ssh; GRP=group.demo@expa.ssh; RUN=run.demo+t1/ws-dev@expa.ssh
expect_eq "commhub id" "$(H commhub.home id)" "$HUBID serial=1"
expect_eq "group id" "$(H group.demo.expa id)" "$GRP serial=1"
expect_eq "run id" "$(H run.demo+t1.ws-dev.expa id)" "$RUN serial=1"
H commhub.home who >/dev/null 2>&1; expect_code "who is gone: a usage error" "$?" 2

echo "# identities through ssh"
ssh-keygen -q -t ed25519 -N '' -f "$T/stranger"
expect_eq "the bare ssh command line works" "$(HK "$CL/keys/$HUBID" id)" "$HUBID serial=1"
HK "$T/stranger" id >/dev/null 2>&1; expect_code "an unclaimed key is refused" "$?" 255
H dev-hub-reg register foreman@expa.ssh < "$T/stranger.pub" > "$T/o" 2>&1; rc=$?
expect_code "the registrar refuses an operator role" "$rc" 3
expect_has "naming hub-mint" "$(cat "$T/o")" "hub-mint"
H dev-hub-reg register bob@expa.ssh < "$T/stranger.pub" >/dev/null 2>&1; expect_code "and an unknown role" "$?" 2
H dev-hub-reg register run.x+y/ws-other@expa.ssh < "$T/stranger.pub" >/dev/null 2>&1; expect_code "and another workspace's pin" "$?" 3
H dev-hub-reg register run.x+y@beta.ssh < "$T/stranger.pub" >/dev/null 2>&1; expect_code "and an org it is not enrolled for" "$?" 3
ssh-keygen -q -t ed25519 -N '' -f "$T/grp2"
out="$(docker exec -i "$C" hub-mint add "$GRP" --replace < "$T/grp2.pub")"
expect_has "the operator rebinds the group to another key" "$out" "serial=2"
HK "$CL/keys/$GRP" id >/dev/null 2>&1; expect_code "the old key is out" "$?" 255
expect_eq "the new key is in" "$(HK "$T/grp2" id)" "$GRP serial=2"
docker exec -i "$C" hub-mint add "$GRP" --replace < "$CL/keys/$GRP.pub" >/dev/null
docker exec -u hub "$C" hub-mint list >/dev/null 2>&1; expect_code "hub-mint is root's" "$?" 1
docker exec -i -u hub "$C" /usr/local/lib/hub/mint add foreman@expa.ssh < "$T/stranger.pub" >/dev/null 2>&1
expect_code "the daemon's user cannot mint an operator role" "$?" 1
docker exec "$C" test -e /var/lib/hub/claims/operator/foreman@expa.ssh; expect_code "and left no record" "$?" 1

echo "# messages through ssh"
printf 'Subject: in.message\nRe: none\n\n{"text":"hi"}\n' | H commhub.home send "$GRP" -m:rfc5322 --id m-1 >/dev/null
raw="$(H group.demo.expa recv --id m-1 --peek:rfc5322)"
expect_has "the hub stamps From" "$raw" "From: $HUBID"
expect_has "the client's Subject travels" "$raw" "Subject: in.message"
expect_has "and the body" "$raw" '{"text":"hi"}'
out="$(printf 'Subject: again\n\nx\n' | H commhub.home send "$GRP" -m:rfc5322 --id m-1)"
expect_has "a resent id is dropped" "$out" "dup m-1"
expect_eq "acked" "$(H group.demo.expa recv_ack m-1)" "acked m-1"
expect_eq "acked again: gone" "$(H group.demo.expa recv_ack m-1)" "gone m-1"
H commhub.home send "$GRP" --id m-2 -- "a    b" >/dev/null
expect_has "an inline message keeps its spaces" "$(H group.demo.expa recv)" "a    b"
H commhub.home send nobody@expa.ssh -- x >/dev/null 2>&1; expect_code "an unknown target -> 3" "$?" 3
head -c 300000 /dev/urandom > "$T/blob"
H "run.demo+t1.ws-dev.expa" send "$GRP" --id f-1 --file blob.bin < "$T/blob" >/dev/null
H group.demo.expa recv --id f-1 --peek:file > "$T/blob.out"
expect_eq "a file round-trips" "$(sha256sum < "$T/blob.out")" "$(sha256sum < "$T/blob")"
H group.demo.expa recv --id f-1 --peek:full > "$T/blob.stream"
expect_eq "a --peek:full stream: msg, then payload chunks" "$(cids payload < "$T/blob.stream" | uniq -c | tr -s ' ')" " 5 f-1"
H group.demo.expa recv --id f-1 --peek:header --header:stderr > "$T/hdr.out" 2> "$T/hdr.err"
expect_eq "--header:stderr: the id chunk on stdout" "$(cat "$T/hdr.out")" $'c;id=f-1;part=id\r\nHeader: f-1\n\r\n0\r\n\r'
expect_has "the header block on stderr after the same line" "$(cat "$T/hdr.err")" "Header: f-1
From: $RUN"
printf '5\r\nhello\r\n0\r\n\r\n' | H commhub.home send "$GRP" --id c-1 -m --chunked >/dev/null
expect_has "a chunked body is stored" "$(H group.demo.expa recv)" "hello"
printf '5\r\nhello\r\n' | H commhub.home send "$GRP" --id c-2 -m --chunked >/dev/null 2>&1
expect_code "one cut before its last chunk -> 4" "$?" 4
printf hello | H commhub.home send "$GRP" --id z-1 --size 5 -m >/dev/null; expect_code "--size exact" "$?" 0
expect_has "and stored" "$(H group.demo.expa recv)" "hello"
printf hello | H commhub.home send "$GRP" --id z-2 --size 6 -m >/dev/null 2> "$T/o"; expect_code "--size short -> 4" "$?" 4
expect_has "says so" "$(cat "$T/o")" "body ended after 5 of 6 bytes"
printf hello | H commhub.home send "$GRP" --id z-2 --size 4 -m >/dev/null 2>&1; expect_code "--size long -> 2" "$?" 2
head -c 1000 "$T/blob" | H commhub.home send "$GRP" --id z-3 --size 1001 --file f.bin >/dev/null 2>&1; expect_code "--file --size short -> 4" "$?" 4
expect_eq "nothing stored" "$(H group.demo.expa recv --peek)" "$EMPTY"
head -c 1000 "$T/blob" | H commhub.home send "$GRP" --id z-3 --size 1000 --file f.bin >/dev/null; expect_code "its retry stores" "$?" 0
H group.demo.expa recv_ack z-3 >/dev/null
mkdir -p "$T/bag/d"; printf 'a\n' > "$T/bag/a.txt"; head -c 40000 "$T/blob" > "$T/bag/d/b.bin"; tar -C "$T/bag" -cf "$T/bag.tar" .
sz="$(stat -c %s "$T/bag.tar")"
head -c $((sz - 10240)) "$T/bag.tar" | H commhub.home send_bag "$GRP" --id bg-1 >/dev/null 2> "$T/o"; expect_code "a tar cut at a member boundary -> 4" "$?" 4
expect_has "says so" "$(cat "$T/o")" "tar ended before its end marker"
head -c 5000 "$T/bag.tar" | H commhub.home send_bag "$GRP" --id bg-1 >/dev/null 2>&1; expect_code "and cut inside a member -> 4" "$?" 4
expect_eq "nothing stored" "$(H group.demo.expa recv --peek)" "$EMPTY"
# the ssh client killed mid-stream: the connection is gone, the hub sees the end of input
{ head -c 5000 "$T/bag.tar"; sleep 30; } | H commhub.home send_bag "$GRP" --id bg-1 >/dev/null 2>&1 & kpid=$!
sleep 2; pkill -f "ssh .*commhub.home send_bag $GRP --id bg-1" 2>/dev/null; wait "$kpid" 2>/dev/null
sleep 1
expect_eq "a killed ssh: nothing stored" "$(H group.demo.expa recv --peek)" "$EMPTY"
expect_eq "no temp left open as a stage" "$(docker exec "$C" sh -c 'ls -A /var/lib/hub/inbox/group.demo@expa.ssh | grep -c "^.stage"')" 0
H commhub.home send_bag "$GRP" --id bg-1 < "$T/bag.tar" >/dev/null; expect_code "the retry stores" "$?" 0
H group.demo.expa recv_bag --id bg-1 > "$T/bag.out"; tar -xOf "$T/bag.out" ./d/b.bin | sha256sum > "$T/bag.sum"
expect_eq "whole" "$(cat "$T/bag.sum")" "$(sha256sum < "$T/bag/d/b.bin")"
expect_eq "recv_bag consumed it" "$(H group.demo.expa recv --peek | cids)" ""

echo "# budgets through ssh"
docker exec "$C" sh -c 'rm -f /var/lib/hub/flood/*'
acc=0; rc5=""; t0=$SECONDS
for i in $(seq 1 16); do
  if H commhub.home send "$GRP" -- "b$i" >/dev/null 2> "$T/o"; then acc=$((acc+1)); else rc5=$?; cp "$T/o" "$T/refused"; fi
done
# 12 at once, then the default 0.5/s refill for however long the 16 connections took
[ "$acc" -ge 12 ] && [ "$acc" -le $(( 13 + (SECONDS - t0) / 2 )) ]
expect_code "default budget: a burst of 12, then 0.5/s (got $acc in $((SECONDS - t0)) s)" "$?" 0
expect_eq "over budget -> 5" "$rc5" 5
expect_has "the refusal names the budget" "$(cat "$T/refused")" "0.5/s, burst 12"
expect_eq "the operator sets a role budget" "$(docker exec "$C" hub-budget set commhub --rate 50 --burst 100)" \
  "budget commhub rate=50/s burst=100"
expect_has "and lists it" "$(docker exec "$C" hub-budget list)" "commhub	rate=50/s	burst=100"
docker exec "$C" sh -c 'rm -f /var/lib/hub/flood/*'
acc=0; for i in $(seq 1 30); do H commhub.home send "$GRP" -- "c$i" >/dev/null 2>&1 && acc=$((acc+1)); done
expect_eq "commhub now sends 30 in a row" "$acc" 30
docker exec "$C" hub-budget set "$HUBID" --burst 2 >/dev/null
docker exec "$C" sh -c 'rm -f /var/lib/hub/flood/*'
acc=0; for i in $(seq 1 4); do H commhub.home send "$GRP" -- "d$i" >/dev/null 2>&1 && acc=$((acc+1)); done
expect_eq "its own identity budget beats the role's" "$acc" 2
docker exec "$C" hub-budget set group --rate 0 >/dev/null 2>&1; expect_code "a zero rate is refused" "$?" 2
docker exec "$C" hub-budget set topic --burst 5 >/dev/null 2>&1; expect_code "a hub role has no budget" "$?" 2
docker exec -u hub "$C" hub-budget list >/dev/null 2>&1; expect_code "hub-budget is root's" "$?" 1
docker exec "$C" hub-budget rm "$HUBID" >/dev/null; docker exec "$C" hub-budget rm commhub >/dev/null
expect_has "removed" "$(docker exec "$C" hub-budget list)" "no budgets"
H group.demo.expa recv >/dev/null

echo "# topics"
out="$(docker exec "$C" hub-topic add topic+events@expa.ssh --publish "$HUBID" --subscribe 'group.*@expa.ssh')"
expect_has "the operator adds a topic" "$out" "topic topic+events@expa.ssh"
docker exec "$C" hub-topic add topic+events/ws-dev@expa.ssh --publish x >/dev/null 2>&1; expect_code "a topic is ws-less" "$?" 2
SUB="$(H group.demo.expa sub topic+events@expa.ssh)"
expect_has "the group subscribes" "$SUB" "sub.events+"
H group.demo.expa sub topic+events@expa.ssh --live >/dev/null 2>&1; expect_code "--live: 2" "$?" 2
H group.demo.expa sub topic+events@expa.ssh --wake >/dev/null 2>&1; expect_code "--wake: 2" "$?" 2
H commhub.home send topic+events@expa.ssh --id e-1 -- event-one >/dev/null 2>&1; expect_code "--id on a publish other than event-<session>-<seq>: 2" "$?" 2
H commhub.home send topic+events@expa.ssh -- event-one >/dev/null
expect_has "a publish reaches the subscription" "$(H group.demo.expa recv "$SUB")" "event-one"

echo "# publishers, gap markers and event ids through ssh"
ssh-keygen -q -t ed25519 -N '' -f "$T/pubk"; PUB=publisher+gitea@expa.ssh; TT=topic.gitea.inbox+tasks@expa.ssh
expect_has "the operator mints a publisher" "$(docker exec -i "$C" hub-mint add "$PUB" < "$T/pubk.pub")" "minted $PUB "
docker exec "$C" hub-topic add "$TT" --publish "$PUB" --subscribe 'group.*@expa.ssh' >/dev/null
GS="$(H group.demo.expa sub "$TT")"
lst="$(H group.demo.expa recv "$GS" --peek)"
expect_eq "no publication yet: head-none after the backlog" "$(cids <<< "$lst")" "head-none"
out="$(printf 'Gap: started\n\n' | HK "$T/pubk" send "$TT" -m:rfc5322 2>&1)"; rc=$?
ses="$(sed -n 's/.*msgid=event-\([0-9a-f]*\)-1 .*/\1/p' <<< "$out")"
expect_eq "its key in a plain ssh client: a gap marker to its topic, the hub's msgid" "$rc|$out" "0|published -> $TT msgid=event-$ses-1 subs=1"
expect_eq "a 16-hex session" "$([[ $ses =~ ^[0-9a-f]{16}$ ]] && echo hex)" "hex"
printf 'Subject: issues\n\n{"n":2}\n' | HK "$T/pubk" send "$TT" -m:rfc5322 --id "event-$ses-10" >/dev/null
out="$(HK "$T/pubk" recv 2>&1)"; rc=$?
expect_eq "and nothing else: a read -> 3" "$rc|$out" "3|dispatch: $PUB is a publisher: it receives nothing, so recv is not for it (send, send_bag, id and help are)"
HK "$T/pubk" send "$GRP" -- hi >/dev/null 2>&1; expect_code "a send to an identity -> 3" "$?" 3
H commhub.home send "$PUB" -- hi >/dev/null 2>&1; expect_code "a send to it -> 3" "$?" 3
printf 'Gap: x\nSubject: y\n\n' | HK "$T/pubk" send "$TT" -m:rfc5322 >/dev/null 2>&1; expect_code "a gap marker with another field -> 2" "$?" 2
printf 'Subject: a\rb\n\nx\n' | HK "$T/pubk" send "$TT" -m:rfc5322 >/dev/null 2>&1; expect_code "a CR in a field value -> 2" "$?" 2
printf 'Re: caf\xe9\n\nx\n' | HK "$T/pubk" send "$TT" -m:rfc5322 >/dev/null 2>&1; expect_code "a field value not valid UTF-8 -> 2 (D34)" "$?" 2
lst="$(H group.demo.expa recv "$GS" --peek)"
expect_eq "a subscription through ssh: by seq, each msgid its event's, no head (the mailbox holds done)" \
  "$(cids <<< "$lst" | tr '\n' ' ')" "event-$ses-1 event-$ses-10 "
expect_eq "a raw read: the hub's fields, the client's, the body" "$(H group.demo.expa recv "$GS" --id "event-$ses-10" --peek:rfc5322 | sed -n '1p;4,6p' | tr '\n' '|')" "From: $PUB|Subject: issues||{\"n\":2}"
expect_eq "the record: session, seq, boot id" "$(docker exec "$C" cut -d' ' -f1,2 "/var/lib/hub/topics/$TT/record")" "$ses 10"
expect_eq "its boot id the kernel's" "$(docker exec "$C" cut -d' ' -f3 "/var/lib/hub/topics/$TT/record")" "$(docker exec "$C" cat /proc/sys/kernel/random/boot_id)"
printf 'Subject: issues\n\n{"n":0}\n' | HK "$T/pubk" send "$TT" -m:rfc5322 --id "event-$ses-10" > "$T/o"
expect_eq "the record's own msgid again: dup" "$(cat "$T/o")" "dup"
printf 'x\n' | HK "$T/pubk" send "$TT" -m --id "event-$ses-3" > "$T/o"
expect_eq "an older seq: stale" "$(cat "$T/o")" "stale: last event-$ses-10"
H group.demo.expa recv_ack "$GS" "event-$ses-10" >/dev/null
expect_eq "the mailbox lacks done: the position notice, last" "$(H group.demo.expa recv "$GS" --peek | cids | tail -1)" "head-$ses-10"
expect_eq "by --id, raw" "$(H group.demo.expa recv "$GS" --id "head-$ses-10" --peek:rfc5322 | sed -n '1p;3p' | tr '\n' '|')" "From: $TT||"
expect_eq "an older head- by --id: part=missing" "$(H group.demo.expa recv "$GS" --id "head-$ses-9" | cids missing)" "head-$ses-9"
timeout 60 ssh -F "$CL/ssh_config" group.demo.expa recv "$GS" --follow --peek > "$T/gf.out" 2>/dev/null < /dev/null &
GFP=$!
for i in $(seq 1 100); do grep -aq ';id=head-' "$T/gf.out" && break; sleep 0.1; done
printf 'Subject: issues\n\n{"n":3}\n' | HK "$T/pubk" send "$TT" -m:rfc5322 >/dev/null
for i in $(seq 1 100); do grep -aq ";id=event-$ses-11;" "$T/gf.out" && break; sleep 0.1; done
pkill -P "$GFP" ssh; wait "$GFP" 2>/dev/null
expect_eq "a follow through ssh: the backlog, the head, then the next delivery" "$(cids < "$T/gf.out" | tr '\n' ' ')" "event-$ses-1 head-$ses-10 event-$ses-11 "
expect_eq "its stdout a chunk stream from the first byte" "$(head -c 40 "$T/gf.out" | head -1 | grep -c '^[0-9a-f]*;id=event-')" 1
printf '%s\n' "event-$ses-1" "event-$ses-11" | H group.demo.expa recv_ack "$GS" -m:ids > "$T/o"
expect_eq "recv_ack -m:ids" "$(cat "$T/o")" "acked event-$ses-1
acked event-$ses-11"
for n in 12 13; do printf 'Subject: issues\n\n{"n":%s}\n' "$n" | HK "$T/pubk" send "$TT" -m:rfc5322 >/dev/null; done
expect_eq "recv_ack --before" "$(H group.demo.expa recv_ack "$GS" --before "event-$ses-13")" "acked event-$ses-12"
expect_eq "recv_ack --before-including" "$(H group.demo.expa recv_ack "$GS" --before-including "event-$ses-13")" "acked event-$ses-13"
out="$(H group.demo.expa recv_ack "$GS" --before-including "event-$ses-13" 2>&1)"; expect_code "--before-including an absent msgid -> 3" "$?" 3

echo "# the hub's own housekeeping"
expect_has "the daemon runs as hub" "$(docker exec "$C" ps -o user=,args= -A)" "hub      /usr/local/sbin/agsshub-hubd"
j1="$(docker exec "$C" sh -c 'wc -l < /var/lib/hub/claims/.journal')"
expect_eq "install marked the message-file format" "$(docker exec "$C" cat /var/lib/hub/.format)" 2
docker exec "$C" sh -c ': > /usr/local/lib/hub/hub-expiry'   # an earlier release's helper, as an upgraded host holds it
docker exec "$C" /src/agsshub/install.sh > "$T/reinstall0.log" 2>&1; expect_code "install.sh runs again" "$?" 0
docker exec "$C" test -e /usr/local/lib/hub/hub-expiry; expect_code "and removes an earlier release's hub-expiry" "$?" 1
expect_eq "a re-run of this format wipes nothing" "$(docker exec "$C" sh -c 'for b; do test -d "/var/lib/hub/inbox/$b" || echo "lost $b"; done' _ "$SUB" "$GS" "$GRP" "$HUBID")" ""
# the previous hub's state: a subscription's .seq/, a delivery in the old layout, a record, no .format
LS=sub.legacy+0000beef@expa.ssh
docker exec "$C" sh -c 'b="/var/lib/hub/inbox/$1"; mkdir -p "$b/.seq" "$b/x-1" && printf "From: a@b.ssh\n\nold\n" > "$b/x-1.msg" && printf "f\n" > "$b/x-1/f" && chown -R hub:hub "$b" && rm /var/lib/hub/.format' _ "$LS"
docker exec "$C" /src/agsshub/install.sh > "$T/reinstall.log" 2>&1; expect_code "install.sh on the previous format" "$?" 0
expect_has "says what it wiped" "$(cat "$T/reinstall.log")" "wiped: mailboxes, subscriptions, topic records (format 2)"
expect_eq "every mailbox gone" "$(docker exec "$C" sh -c 'ls -A /var/lib/hub/inbox | wc -l')" 0
expect_eq "the subscription index gone" "$(docker exec "$C" sh -c 'find /var/lib/hub/subs /var/lib/hub/topics/*/subs -mindepth 1 | wc -l')" 0
expect_eq "every record gone" "$(docker exec "$C" sh -c 'ls /var/lib/hub/topics/*/record 2>/dev/null | wc -l')" 0
expect_eq "the format marked" "$(docker exec "$C" cat /var/lib/hub/.format)" 2
expect_has "and says what it left to the caller" "$(cat "$T/reinstall.log")" "no systemd"
expect_eq "the journal survives it" "$(docker exec "$C" sh -c 'wc -l < /var/lib/hub/claims/.journal')" "$j1"
expect_eq "and so do the claims" "$(H commhub.home id)" "$HUBID serial=1"
"$SRC/dev-hub/setup.sh" --out "$CL" commhub@home.ssh group.demo@expa.ssh run.demo+t1@expa.ssh >/dev/null 2>&1
expect_code "setup runs again on the same keys" "$?" 0
expect_eq "renewing, not rebinding" "$(H group.demo.expa id)" "$GRP serial=3"

echo "# following your own mailbox through ssh"
H group.demo.expa recv --peek | cids | H group.demo.expa recv_ack -m:ids >/dev/null 2>&1
docker exec "$C" sh -c 'rm -f /var/lib/hub/flood/*'
until_ok(){ local i; for i in $(seq 1 60); do "$@" && return 0; sleep 0.1; done; return 1; }
followed(){ H group.demo.expa help "$1" --json | grep -q '"live":true'; }
timeout 60 ssh -F "$CL/ssh_config" group.demo.expa recv "$GRP" --follow --peek > "$T/f1.out" 2>&1 < /dev/null &
FP=$!
until_ok followed "$GRP"
t0=$(date +%s%N); H commhub.home send "$GRP" --id f-1 -- one >/dev/null
until_ok grep -aq ';id=f-1;' "$T/f1.out"; ms=$(( ($(date +%s%N) - t0) / 1000000 ))
expect_has "recv <own address> --follow --peek sees new mail" "$(cat "$T/f1.out")" ";id=f-1;part=msg"
[ "$ms" -lt 2000 ]; expect_code "within 2 s of the send: woken, not polled ($ms ms)" "$?" 0
pkill -9 -P "$FP" ssh; wait "$FP" 2>/dev/null                     # the client vanishes
timeout 30 ssh -F "$CL/ssh_config" group.demo.expa recv --follow --peek > "$T/f2.out" 2>&1 < /dev/null &
FP2=$!
until_ok grep -aq ';id=f-1;' "$T/f2.out"
expect_has "the client reconnects and follows again at once" "$(cat "$T/f2.out")" ";id=f-1;part=msg"
timeout 30 ssh -F "$CL/ssh_config" group.demo.expa recv --follow --peek > "$T/f3.out" 2>&1 < /dev/null &
FP3=$!
wait "$FP2"; rc=$?
expect_code "a second follower replaces the first, which exits 0" "$rc" 0
tklog(){ docker logs "$C" 2>&1 | grep -ao "follow $GRP conn=[0-9]*\.[0-9]* replaced conn=[0-9]*\.[0-9]*" | tail -1; }
tkhas(){ [ -n "$(tklog)" ]; }; until_ok tkhas   # the log driver copies stderr asynchronously
TK="$(tklog)"
NEWC="${TK#*conn=}"; NEWC="${NEWC%% *}"
expect_has "the hub logs the takeover" "$TK" "follow $GRP conn="
expect_has "the replaced stream ends with 0;end=replaced;by=<the new connection>" "$(cat "$T/f2.out")" $'\r\n0;end=replaced;by='"$NEWC"$'\r\n\r\n'
until_ok grep -aq ';id=f-1;' "$T/f3.out"
t0=$SECONDS
docker exec -i "$C" hub-mint add "$GRP" --replace < "$T/grp2.pub" >/dev/null
wait "$FP3"; rc=$?
expect_code "rebinding the identity ends its follow" "$rc" 0
[ $((SECONDS - t0)) -le 3 ]; expect_code "at once ($((SECONDS - t0)) s)" "$?" 0
expect_has "saying so" "$(cat "$T/f3.out")" "is gone"
expect_has "its stream ends with 0;end=gone" "$(cat "$T/f3.out")" $'\r\n0;end=gone\r\n\r\n'
docker exec -i "$C" hub-mint add "$GRP" --replace < "$CL/keys/$GRP.pub" >/dev/null

echo "# live sends through ssh"
docker exec "$C" hub-budget set group --rate 50 --burst 500 >/dev/null
docker exec "$C" sh -c 'rm -f /var/lib/hub/flood/*'
frame(){ local f="$1" cs="$2" sz off=0 n; sz=$(stat -c %s "$f")
  while [ "$off" -lt "$sz" ]; do n=$(( sz-off < cs ? sz-off : cs ))
    printf '%x\r\n' "$n"; tail -c +$((off+1)) "$f" | head -c "$n"; printf '\r\n'; off=$((off+n)); done
  printf '0\r\n\r\n'; }
CMH="$T/cm-commhub"; CMG="$T/cm-grp"                        # one control connection per identity
ssh -F "$CL/ssh_config" -o ControlMaster=yes -o ControlPath="$CMH" -o ControlPersist=300 -fN commhub.home
ssh -F "$CL/ssh_config" -o ControlMaster=yes -o ControlPath="$CMG" -o ControlPersist=300 -fN group.demo.expa
HC(){ ssh -F "$CL/ssh_config" -o ControlMaster=no -o ControlPath="$CMH" commhub.home "$@"; }
GC(){ ssh -F "$CL/ssh_config" -o ControlMaster=no -o ControlPath="$CMG" group.demo.expa "$@"; }
printf '{"kind":"out.deliver"}' > "$T/lv.json"; { printf 'Subject: out.deliver\n\n' > "$T/lv.eml"; cat "$T/lv.json" >> "$T/lv.eml"; }
frame "$T/lv.eml" 64 > "$T/lv.framed"
GC send "$HUBID" --live --chunked --id d-0 -m:rfc5322 < "$T/lv.framed" >/dev/null 2>&1
expect_code "a recipient that follows nothing is not live -> 6" "$?" 6
LVF="$T/lv.follow"; : > "$LVF"; echo ok > "$T/lv.mode"
timeout 600 ssh -F "$CL/ssh_config" commhub.home recv --follow --peek > "$LVF" 2>/dev/null < /dev/null &
LFP=$!
until_ok sh -c 'ssh -F "$1" commhub.home help "$2" --json | grep -q "\"live\":true"' _ "$CL/ssh_config" "$HUBID"
lv_reply(){ case "$(cat "$T/lv.mode")" in
    ok)   printf 'got %s' "$2" > "$T/lv.$1.rb"; printf 'Status: 200\nContent-Type: text/plain\n\n'; frame "$T/lv.$1.rb" 4 ;;
    raw)  printf 'Status: 200\n\ngot %s raw' "$2" ;;
    chat) printf 'Status: 200\n\npong\n' ;;
    none) : ;;
  esac; }
lv_responder(){ local n=0 line w sid from mid
  while [ ! -e "$T/lv.stop" ]; do
    while IFS= read -r line; do
      n=$((n+1)); read -r w sid from mid kind <<< "$line"
      [ "$w" = LIVE ] && [ "$(cat "$T/lv.mode")" != ignore ] || continue
      lv_reply "$sid" "$mid" | { HC accept "$sid" > "$T/lv.$sid.req" 2> "$T/lv.$sid.err"; echo $? > "$T/lv.$sid.rc"; } &
    done < <(tail -n +$((n+1)) "$LVF")
    sleep 0.02
  done
  wait; }
lv_sid(){ sed -n "s/^LIVE \([0-9a-f]*\) [^ ]* $1 chunked\$/\1/p" "$LVF" | tail -1; }
lv_responder & LVR=$!
t0=$(date +%s%N)
GC send "$HUBID" --live --chunked --id d-1 -m:rfc5322 < "$T/lv.framed" > "$T/lv.out" 2> "$T/lv.err"; rc=$?
ms=$(( ($(date +%s%N) - t0) / 1000000 ))
expect_code "a live send through ssh, accepted over the recipient's control connection -> 0" "$rc" 0
expect_eq "the reply comes back on the sender's session" "$(cat "$T/lv.out")" "Status: 200
Content-Type: text/plain

got d-1"
echo "       (live round trip $ms ms)"
sid="$(lv_sid d-1)"; until_ok test -s "$T/lv.$sid.rc"
expect_eq "its accept session exits 0" "$(cat "$T/lv.$sid.rc")" "0"
expect_has "the recipient reads who sent it" "$(cat "$T/lv.$sid.req")" "From: $GRP"
expect_has "and the client's fields in the hub header block (D21)" "$(sed -n '/^$/q;p' "$T/lv.$sid.req")" "Subject: out.deliver"
expect_has "the LIVE line ends in its framing token" "$(cat "$LVF")" " $GRP d-1 chunked"
expect_has "in a part=live chunk, its id the msgid" "$(cat "$LVF")" ";id=d-1;part=live"$'\r\n'"LIVE $sid $GRP d-1 chunked"
expect_eq "nothing spooled" "$(H commhub.home recv --peek)" "$EMPTY"
for i in $(seq 1 12); do
  ( GC send "$HUBID" --live --chunked --id p-$i -m:rfc5322 < "$T/lv.framed" > "$T/lvp.$i.out" 2> "$T/lvp.$i.err"; echo $? > "$T/lvp.$i.rc" ) &
done
for i in $(seq 1 12); do until_ok test -s "$T/lvp.$i.rc"; done
ok=0; for i in $(seq 1 12); do [ "$(cat "$T/lvp.$i.rc" 2>/dev/null)" = 0 ] && [ "$(tail -1 "$T/lvp.$i.out")" = "got p-$i" ] && ok=$((ok+1)); done
expect_eq "12 concurrent live sends over one connection to one recipient, 12 accepts over another: each reply to its sender" "$ok" 12
[ "$ok" = 12 ] || for i in $(seq 1 12); do echo "       p-$i rc=$(cat "$T/lvp.$i.rc" 2>/dev/null) $(head -c 200 "$T/lvp.$i.err")"; done
echo raw > "$T/lv.mode"
printf 'Subject: out.raw\n\nraw body\n' | GC send "$HUBID" --live --id r-1 -m:rfc5322 > "$T/lv.out" 2>/dev/null; rc=$?
expect_eq "a raw send --live through ssh: the reply to the recipient's EOF" "$rc|$(cat "$T/lv.out")" "0|Status: 200

got r-1 raw"
rsid="$(sed -n "s/^LIVE \([0-9a-f]*\) [^ ]* r-1 raw\$/\1/p" "$LVF")"; until_ok test -s "$T/lv.$rsid.rc"
expect_eq "the recipient got the request to the sender's EOF, its fields in the header block" "$(sed '/^Date: /d' "$T/lv.$rsid.req")" "From: $GRP
Msgid: r-1
Subject: out.raw

raw body"
echo chat > "$T/lv.mode"
printf 'ping\n' | GC chat "$HUBID" > "$T/lv.out" 2>/dev/null; rc=$?
expect_eq "a chat through ssh: both ways, to each side's EOF" "$rc|$(cat "$T/lv.out")" "0|Status: 200

pong"
csid="$(sed -n "s/^LIVE \([0-9a-f]*\) $GRP [^ ]* raw chat\$/\1/p" "$LVF")"; until_ok test -s "$T/lv.$csid.rc"
expect_eq "its accept: Subject: chat, the ping" "$(sed '/^Date: /d;/^Msgid: /d' "$T/lv.$csid.req")" "From: $GRP
Subject: chat

ping"
echo "       (the chat's accept session: exit $(cat "$T/lv.$csid.rc"); the raw send's: exit $(cat "$T/lv.$rsid.rc"))"
echo ok > "$T/lv.mode"
frame "$T/lv.eml" 16 | head -c -5 | GC send "$HUBID" --live --chunked --id d-2 -m:rfc5322 > "$T/lv.out" 2>/dev/null
expect_code "a request cut before its last chunk -> 4" "$?" 4
docker exec "$C" hub-budget set "$GRP" --burst 1 >/dev/null; docker exec "$C" sh -c 'rm -f /var/lib/hub/flood/*'
GC send "$HUBID" --live --chunked --id d-3 -m:rfc5322 < "$T/lv.framed" >/dev/null 2>&1
GC send "$HUBID" --live --chunked --id d-4 -m:rfc5322 < "$T/lv.framed" >/dev/null 2>&1
expect_code "over the flood budget -> 5" "$?" 5
docker exec "$C" hub-budget rm "$GRP" >/dev/null; docker exec "$C" sh -c 'rm -f /var/lib/hub/flood/*'
echo ignore > "$T/lv.mode"
docker exec "$C" sh -c 'printf "live_accept_secs=1\n" > /var/lib/hub/settings && chgrp hub /var/lib/hub/settings && chmod 0640 /var/lib/hub/settings'
t0=$(date +%s%N); GC send "$HUBID" --live --chunked --id d-5 -m:rfc5322 < "$T/lv.framed" >/dev/null 2>&1; rc=$?
ms=$(( ($(date +%s%N) - t0) / 1000000 ))
expect_code "not accepted within T -> 6" "$rc" 6
[ "$ms" -ge 900 ] && [ "$ms" -lt 4000 ]; expect_code "after the operator's live_accept_secs=1 ($ms ms)" "$?" 0
docker exec "$C" rm -f /var/lib/hub/settings
echo none > "$T/lv.mode"
GC send "$HUBID" --live --chunked --id d-6 -m:rfc5322 < "$T/lv.framed" > "$T/lv.out" 2>/dev/null
expect_code "the recipient closes its accept session without a reply -> 7" "$?" 7
expect_eq "with nothing on stdout" "$(cat "$T/lv.out")" ""
HK "$T/stranger" send "$HUBID" --live --chunked --id d-7 -m:rfc5322 < "$T/lv.framed" >/dev/null 2>&1
expect_code "an unclaimed key -> 255 (ssh)" "$?" 255
touch "$T/lv.stop"; wait "$LVR" 2>/dev/null; kill "$LFP" 2>/dev/null; wait "$LFP" 2>/dev/null
ssh -F "$CL/ssh_config" -o ControlPath="$CMH" -O exit commhub.home 2>/dev/null; ssh -F "$CL/ssh_config" -o ControlPath="$CMG" -O exit group.demo.expa 2>/dev/null

echo "# instances through ssh"
HKX(){ local k="$1" cp="$2"; shift 2                 # HK over a control connection of its own
  ssh -F /dev/null -p "$DEV_HUB_PORT" -i "$k" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 \
      -o HostKeyAlias="$DEV_HUB_HOST_KEY_ALIAS" -o UserKnownHostsFile="$DEV_HUB_KNOWN_HOSTS" -o StrictHostKeyChecking=yes \
      -o ControlMaster=auto -o ControlPath="$cp" -o ControlPersist=120 "hub@$DEV_HUB_HOST" "$@"; }
for k in ik1 ik2; do ssh-keygen -q -t ed25519 -N '' -f "$T/$k"; done
IA=group.demo+a1b2c3d4@expa.ssh
H commhub.home register a1b2c3d4 < "$T/ik1.pub" > /dev/null 2>&1; expect_code "an address that takes no instances registers none" "$?" 3
H commhub.home send commhub+a1b2c3d4@home.ssh -- x > /dev/null 2>&1; expect_code "nor is its instance form a target" "$?" 3
expect_has "the operator lets the group take instances (same key: a renewal)" \
  "$(docker exec -i "$C" hub-mint add "$GRP" --instances < "$CL/keys/$GRP.pub")" " instances"
out="$(H group.demo.expa register a1b2c3d4 < "$T/ik1.pub")"
expect_has "the group registers an instance's key over its own connection" "$out" "registered $IA fpr="
expect_has "the instance logs in with that key" "$(HK "$T/ik1" id)" "$IA serial="
expect_eq "its claim and key link: written by the daemon as hub" \
  "$(docker exec "$C" stat -c '%U:%G %a' "/var/lib/hub/claims/instance/$IA" | tr -d '\n'; docker exec "$C" sh -c 'stat -c " %U:%G" /var/lib/hub/claims/.ibyfpr/*')" "hub:hub 640 hub:hub"
HKX "$T/ik1" "$T/cm-ik1" -fN; timeout 60 ssh -F /dev/null -o ControlPath="$T/cm-ik1" -o ControlMaster=no x recv --follow --peek > "$T/ia.out" 2>&1 < /dev/null & IAP=$!
until_ok sh -c 'ssh -F "$1" group.demo.expa help "$2" --json | grep -q "\"live\":true"' _ "$CL/ssh_config" "$IA"
expect_has "a new run registers a new key" "$(H group.demo.expa register a1b2c3d4 < "$T/ik2.pub")" "registered $IA "
wait "$IAP"; expect_code "the previous run's follow ends at once" "$?" 0
expect_has "saying so" "$(cat "$T/ia.out")" "is gone"
ssh -F /dev/null -o ControlPath="$T/cm-ik1" -o ControlMaster=no x id > "$T/o" 2>&1; expect_code "its open connection is refused from its next command" "$?" 3
ssh -F /dev/null -o ControlPath="$T/cm-ik1" -O exit x 2>/dev/null
HK "$T/ik1" id > /dev/null 2>&1; expect_code "its key no longer logs in" "$?" 255
expect_has "the new one does" "$(HK "$T/ik2" id)" "$IA serial="
expect_eq "the group releases it" "$(H group.demo.expa release a1b2c3d4)" "released $IA"
HK "$T/ik2" id > /dev/null 2>&1; expect_code "and its key is out" "$?" 255

echo "# admitting and waking an instance through ssh"
CMG2="$T/cm-grp2"; CMH2="$T/cm-hub2"
ssh -F "$CL/ssh_config" -o ControlMaster=yes -o ControlPath="$CMG2" -o ControlPersist=300 -fN group.demo.expa
ssh -F "$CL/ssh_config" -o ControlMaster=yes -o ControlPath="$CMH2" -o ControlPersist=300 -fN commhub.home
G2(){ ssh -F "$CL/ssh_config" -o ControlMaster=no -o ControlPath="$CMG2" group.demo.expa "$@"; }
H2(){ ssh -F "$CL/ssh_config" -o ControlMaster=no -o ControlPath="$CMH2" commhub.home "$@"; }
docker exec "$C" hub-budget set commhub --rate 50 --burst 500 >/dev/null; docker exec "$C" sh -c 'rm -f /var/lib/hub/flood/*'
until_long(){ local i; for i in $(seq 1 150); do "$@" 2>/dev/null && return 0; sleep 0.1; done; return 1; }   # 15 s
lastwake(){ awk '/^[0-9a-f]+;id=wakes-[^;]*;part=msg\r$/{w=1;h=1;b="";next} w&&h{if($0=="")h=0;next} w&&/^\r$/{w=0;l=b;next} w{b=b $0 "\n"} END{printf "%s", l}' "$PF"; }
nowake(){ [ -z "$(lastwake)" ]; }
PF="$T/par.follow"; : > "$PF"
I2=group.demo+c0ffee01@expa.ssh
timeout 600 ssh -F "$CL/ssh_config" -o ControlMaster=no -o ControlPath="$CMG2" group.demo.expa recv --follow --peek > "$PF" 2>/dev/null < /dev/null & PFP=$!
until_long grep -q '^Wake: 0$' "$PF"; expect_code "the group's follow: the wake notice first, Wake: 0" "$?" 0
( H2 send "$I2" --id n-1 -- nobody-home > "$T/n1.out" 2> "$T/n1.err"; echo $? > "$T/n1.rc" ) &
until_long grep -q "^$I2	ask=$HUBID\$" "$PF"; expect_code "a send to an unknown instance: held, the ask on the group's follow" "$?" 0
expect_eq "nothing stored while held, no mailbox" "$(G2 instances | grep -c "^$I2	")" "0"
expect_eq "the group admits it" "$(G2 admit c0ffee01)" "admitted $I2"
until_long test -s "$T/n1.rc"; expect_eq "the held send: stored, exit 0" "$(cat "$T/n1.rc"):$(cat "$T/n1.out")" "0:sent -> $I2 msgid=n-1"
until_long sh -c 'grep -q "^$1	mail=1	from=$2@" "$3"' _ "$I2" "$HUBID" "$PF"; expect_code "its mail on the wake notice" "$?" 0
expect_eq "its mail, nobody following" "$(G2 instances | grep "^$I2	")" "$I2	mail=1	holder=none	claim=none"
k="$T/wk.c0ffee01.key"; ssh-keygen -q -t ed25519 -N '' -f "$k"
G2 register c0ffee01 < "$k.pub" > /dev/null
t0=$(date +%s%N)
timeout 300 ssh -F /dev/null -p "$DEV_HUB_PORT" -i "$k" -o IdentitiesOnly=yes -o BatchMode=yes -o HostKeyAlias="$DEV_HUB_HOST_KEY_ALIAS" \
  -o UserKnownHostsFile="$DEV_HUB_KNOWN_HOSTS" -o StrictHostKeyChecking=yes "hub@$DEV_HUB_HOST" recv --follow --peek > "$T/wk.c0ffee01.out" 2>&1 < /dev/null & IFP=$!
until_long grep -q nobody-home "$T/wk.c0ffee01.out"; expect_code "the group starts it: the instance has what waited" "$?" 0
until_long nowake; expect_eq "a followed instance leaves the wake set" "$(lastwake)" ""
echo "       (instance login and follow through ssh until the notice changed: $(( ($(date +%s%N) - t0) / 1000000 )) ms)"
H2 send "$I2" --id w-2 -- again > /dev/null; expect_code "the next send: stored" "$?" 0
until_long grep -q again "$T/wk.c0ffee01.out"; expect_code "and the instance has it" "$?" 0
kill "$IFP" 2>/dev/null; wait "$IFP" 2>/dev/null
until_long sh -c 'grep -q "^$1	mail=2	" "$2"' _ "$I2" "$PF"; expect_code "its follow gone: back on the notice, mail=2 (kept)" "$?" 0
H2 send "$I2" --live --chunked --id lv-c -m:rfc5322 < "$T/lv.framed" > /dev/null 2> "$T/o"; rc=$?
expect_eq "--live to it asleep (flow C): exit 6, retry-after" "$rc|$(tail -1 "$T/o")" "6|retry-after=5"
until_long grep -q "^$I2	mail=2	from=$HUBID@.*	stay-up-till=" "$PF"; expect_code "its line carries stay-up-till" "$?" 0
expect_eq "rejected" "$(G2 reject dead0000)" "rejected group.demo+dead0000@expa.ssh"
( H2 send group.demo+dead0000@expa.ssh --id w-3 -- x > /dev/null 2> "$T/o"; echo $? > "$T/w3.rc" ) &
until_long grep -q "^group.demo+dead0000@expa.ssh	ask=$HUBID\$" "$PF"
G2 reject dead0000 > /dev/null
until_long test -s "$T/w3.rc"; expect_eq "a send asks again and reject answers it: 3" "$(cat "$T/w3.rc")" 3
k5="$T/wk.c0ffee05.key"; ssh-keygen -q -t ed25519 -N '' -f "$k5"; G2 register c0ffee05 < "$k5.pub" > /dev/null
G2 reject c0ffee05 > /dev/null 2>&1; expect_code "reject of an instance with a claim: 0" "$?" 0
expect_eq "its claim gone" "$(G2 instances | grep -c c0ffee05)" 0
docker exec "$C" sh -c 'printf "wake_secs=1\n" > /var/lib/hub/settings && chgrp hub /var/lib/hub/settings && chmod 0640 /var/lib/hub/settings'
t0=$(date +%s%N); H2 send group.demo+c0ffee02@expa.ssh --id w-4 -- x > /dev/null 2> "$T/o"; rc=$?; ms=$(( ($(date +%s%N) - t0) / 1000000 ))
expect_eq "no answer within the operator's wake_secs=1: exit 6, retry-after, nothing stored ($ms ms)" "$rc|$(tail -1 "$T/o")|$(G2 instances | grep -c c0ffee02)" "6|retry-after=5|0"
docker exec "$C" rm -f /var/lib/hub/settings
I3=group.demo+c0ffee03@expa.ssh
for i in $(seq 1 10); do ( H2 send "$I3" --id c-$i -- concurrent-$i > /dev/null 2>&1; echo $? > "$T/wc.$i.rc" ) & done
until_long grep -q "^$I3	ask=$HUBID\$" "$PF"
sleep 1; expect_eq "10 concurrent sends to one unknown instance: one ask line" "$(lastwake | grep -c "^$I3	ask=")" "1"
G2 admit c0ffee03 > /dev/null
for i in $(seq 1 10); do until_long test -s "$T/wc.$i.rc"; done
ok=0; for i in $(seq 1 10); do [ "$(cat "$T/wc.$i.rc" 2>/dev/null)" = 0 ] && ok=$((ok+1)); done
expect_eq "one admit: all 10 stored" "$ok" 10
expect_eq "the group's view" "$(G2 instances | grep "^$I3")" "$I3	mail=10	holder=none	claim=none"
I4=group.demo+c0ffee04@expa.ssh; k4="$T/wk.c0ffee04.key"; ssh-keygen -q -t ed25519 -N '' -f "$k4"
G2 register c0ffee04 < "$k4.pub" > /dev/null
timeout 300 ssh -F /dev/null -p "$DEV_HUB_PORT" -i "$k4" -o IdentitiesOnly=yes -o BatchMode=yes -o HostKeyAlias="$DEV_HUB_HOST_KEY_ALIAS" \
  -o UserKnownHostsFile="$DEV_HUB_KNOWN_HOSTS" -o StrictHostKeyChecking=yes "hub@$DEV_HUB_HOST" recv --follow --peek > "$T/wk.c0ffee04.out" 2>&1 < /dev/null & I4P=$!
i4live(){ G2 instances | grep -q "^$I4	.*holder=live"; }
until_long i4live
( H2 send "$I4" --live --chunked --id lw-1 -m:rfc5322 < "$T/lv.framed" > "$T/lw.out" 2>/dev/null; echo $? > "$T/lw.rc" ) &
until_long grep -q '^LIVE ' "$T/wk.c0ffee04.out"
lsid="$(sed -n 's/^LIVE \([0-9a-f]*\) .*/\1/p' "$T/wk.c0ffee04.out" | head -1)"
printf 'Status: 200\n\n0\r\n\r\n' | HK "$k4" accept "$lsid" > /dev/null 2>&1
until_long test -s "$T/lw.rc"
expect_eq "a live send to a followed instance: the LIVE line from the exchange table, its reply back" "$(cat "$T/lw.rc"):$(cat "$T/lw.out")" "0:Status: 200"
kill "$I4P" 2>/dev/null; wait "$I4P" 2>/dev/null
docker exec "$C" sed -i 's/^expires=.*/expires=1/' "/var/lib/hub/claims/instance/$I2"
until_ok sh -c '! docker exec "$1" test -e "/var/lib/hub/claims/instance/$2"' _ "$C" "$I2"; expect_code "the reaper sweeps an expired instance" "$?" 0
HK "$k" id > /dev/null 2>&1; expect_code "its key no longer logs in" "$?" 255
expect_has "its mail stays, nobody following" "$(G2 instances)" "$I2	mail=2	holder=none	claim=none"
kill "$PFP" 2>/dev/null; wait "$PFP" 2>/dev/null
ssh -F "$CL/ssh_config" -o ControlPath="$CMG2" -O exit group.demo.expa 2>/dev/null; ssh -F "$CL/ssh_config" -o ControlPath="$CMH2" -O exit commhub.home 2>/dev/null
docker exec "$C" hub-budget rm commhub > /dev/null

echo "# help and register_description (docs/help.md)"
D_PROG='{"descriptor":1,"summary":"Only for programs.","requests":[{"subject":"a.b","form":"structured","delivery":["live"],"summary":"Do it.","schema":{}}]}'
D_TEXT='{"descriptor":1,"summary":"For people.","requests":[{"subject":null,"form":"text","delivery":["spool"],"summary":"Anything.","guide":"Say it."}]}'
D_EV='{"descriptor":1,"summary":"Events.","requests":[{"subject":"e","form":"structured","delivery":["spool"],"summary":"An event.","schema":{}}]}'
GRP_D="group.demo@expa.ssh"
out="$(H group.demo.expa help "$GRP_D")"
expect_has "no description: the text says so" "$out" "No description registered: this address has not said what requests it handles."
out="$(printf '%s' "$D_TEXT" | H group.demo.expa register_description --see '*@home.ssh')"; expect_has "registered through the hub" "$out" "described $GRP_D "
expect_has "a text service answers text" "$(H group.demo.expa help "$GRP_D")" "(no subject)	text	spool	both	-	Anything."
expect_has "another org reads through --see" "$(H commhub.home help "$GRP_D" --json)" '"summary":"For people."'
printf '%s' "$D_PROG" | H group.demo.expa register_description >/dev/null
expect_has "a programmatic service answers JSON" "$(H group.demo.expa help "$GRP_D")" '{"target":"'"$GRP_D"'"'
out="$(H commhub.home help "$GRP_D" 2>&1)"; rc=$?
expect_eq "without the grant: refused" "$rc|$out" "3|dispatch: no such target, or no access: $GRP_D"
out="$(H commhub.home help svc.nobody@expa.ssh 2>&1)"; rc=$?
expect_eq "the same as a target that does not exist" "$rc|$out" "3|dispatch: no such target, or no access: svc.nobody@expa.ssh"
printf '{' | H group.demo.expa register_description >/dev/null 2>&1; expect_code "bad JSON is refused" "$?" 2
expect_has "the hub's own verbs" "$(H commhub.home help)" "Caller: commhub@home.ssh"
expect_has "and as JSON" "$(H commhub.home help --json)" '"hub":{"verbs":['
expect_eq "--clear" "$(H group.demo.expa register_description --clear </dev/null)" "cleared $GRP_D"
docker exec "$C" hub-topic add topic+helped@expa.ssh --publish commhub@home.ssh >/dev/null
out="$(printf '%s' "$D_EV" | docker exec -i "$C" hub-topic register_description topic+helped@expa.ssh)"; expect_has "the operator describes a topic" "$out" "described topic+helped@expa.ssh "
expect_has "and it serves" "$(H group.demo.expa help topic+helped@expa.ssh)" '"summary":"Events."'
expect_has "to whoever its policy lets publish" "$(H commhub.home help topic+helped@expa.ssh)" '"publish":true'
printf '%s' "$D_PROG" | docker exec -i "$C" hub-topic register_description topic+helped@expa.ssh >/dev/null 2>&1; expect_code "a live request on a topic is refused" "$?" 2
expect_has "and the description stays" "$(H group.demo.expa help topic+helped@expa.ssh)" '"summary":"Events."'
docker exec "$C" hub-topic register_description topic+nope@expa.ssh </dev/null >/dev/null 2>&1; expect_code "no such topic" "$?" 3
docker exec "$C" hub-topic add topic+helped@expa.ssh --publish x@expa.ssh >/dev/null
expect_has "re-adding a topic keeps it" "$(H group.demo.expa help topic+helped@expa.ssh)" '"summary":"Events."'
expect_eq "--clear" "$(docker exec "$C" hub-topic register_description topic+helped@expa.ssh --clear)" "cleared topic+helped@expa.ssh"
printf '%s' "$D_EV" | docker exec -i "$C" hub-topic register_description topic+helped@expa.ssh >/dev/null
docker exec "$C" hub-topic rm topic+helped@expa.ssh >/dev/null
docker exec "$C" hub-topic add topic+helped@expa.ssh --publish x@expa.ssh >/dev/null
expect_has "removing a topic removes it" "$(H group.demo.expa help topic+helped@expa.ssh)" "No description registered"
docker exec "$C" hub-topic rm topic+helped@expa.ssh >/dev/null
docker exec "$C" hub-topic rm topic+events@expa.ssh >/dev/null
expect_eq "and its subscriptions' mailboxes, leaving no stage" "$(docker exec "$C" sh -c 'ls -A /var/lib/hub/inbox | grep -c -e "^$1\$" -e "^\.stage\."' _ "$SUB")" 0

echo "# --subject and expiry notices"
HBOX=/var/lib/hub/inbox/$HUBID
out="$(H group.demo.expa send "$HUBID" --id xp-1 --subject in.demo -- through ssh)"; expect_has "--subject through the hub" "$out" "msgid=xp-1"
expect_has "recv shows it" "$(H commhub.home recv --peek)" "Subject: in.demo

through ssh"
H group.demo.expa send "$HUBID" --subject 2>/dev/null </dev/null; expect_code "with no value" "$?" 2
H group.demo.expa send "$HUBID" --id expired-1 -- x >/dev/null 2>&1; expect_code "--id expired-* refused" "$?" 2
docker exec "$C" touch -d '30 days ago' "$HBOX/xp-1.msg"
# the notice, not the message's going: the sweep takes the message a moment before it writes the notice
until_long docker exec "$C" test -s "$HBOX/.expired/$GRP.json"; expect_code "the reaper's notice lands" "$?" 0
BM="$(docker exec "$C" stat -c '%U:%G %a' "$HBOX")"
lst="$(H commhub.home recv --peek)"
expect_has "the reaper's sweep leaves a notice (mailbox $BM)" "$lst" "From: $GRP
Date: "
expect_has "with its count" "$lst" "
Expired: 1

verb	count	oldest	newest
in.demo	1	"
expect_eq "the mail went" "$(docker exec "$C" sh -c "ls $HBOX | grep -c xp-1")" 0
expect_eq ".expired under the reaper's umask" "$(docker exec "$C" stat -c %a "$HBOX/.expired")" 2770
expect_eq "the notice" "$(docker exec "$C" stat -c %a "$HBOX/.expired/$GRP.json")" 660
N="$(cids <<< "$lst" | grep '^expired-')"
expect_has "the raw read" "$(H commhub.home recv --id "$N" --peek:rfc5322)" "From: $GRP"
expect_eq "recv_ack clears what the reaper wrote" "$(H commhub.home recv_ack "$N")" "acked $N"
expect_eq "cleared" "$(H commhub.home recv --peek)" "$EMPTY"

echo "# the notice probe"
docker exec "$C" sh -c 'printf "notice_probe=%s\n" "$1" > /var/lib/hub/settings && chgrp hub /var/lib/hub/settings && chmod 0640 /var/lib/hub/settings' _ "$HUBID"
H group.demo.expa send "$HUBID" --id pb-1 --subject v -- x >/dev/null
PF="$T/pf.out"; : > "$PF"
timeout 60 ssh -F "$CL/ssh_config" commhub.home recv --follow --peek > "$PF" 2>/dev/null < /dev/null & PFP=$!
timeout 5 bash -c 'until grep -aq ";id=pb-1;" "$1"; do sleep 0.1; done' _ "$PF"; expect_code "a follow through the hub" "$?" 0
kill "$PFP" 2>/dev/null; wait "$PFP" 2>/dev/null
expect_eq "the probe first, a msg chunk from the first byte" "$(sed -n '1s/^[0-9a-f]*;id=expired-[0-9a-f]*-[0-9]*;part=msg\r$/probe/p' "$PF")" "probe"
expect_eq "whole" "$(sed -n '4,6p' "$PF")" "Expired: 0
"$'\n\r'
expect_has "from itself" "$(sed -n 2p "$PF")" "From: $HUBID"
N="$(cids < "$PF" | grep '^expired-')"
out="$(H commhub.home recv_ack "$N" 2>&1)"; expect_eq "recv_ack of the probe: acked" "$?|$out" "0|acked $N"
H commhub.home recv --id "$N" --peek:rfc5322 >/dev/null 2>&1; expect_code "--peek:rfc5322: no such message" "$?" 3
expect_eq "nothing stored" "$(docker exec "$C" sh -c "ls $HBOX/.expired/*.json 2>/dev/null | wc -l")" 0
docker exec "$C" rm -f /var/lib/hub/settings
H commhub.home recv_ack pb-1 >/dev/null

echo "==== $PASS passed, $FAIL failed ===="
[ "$FAIL" = 0 ]
