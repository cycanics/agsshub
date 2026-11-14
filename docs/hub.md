# The hub (agsshub-hubd)

`agsshub-hubd` is the hub: one daemon with its own SSH server, over a data dir of plain files. sshd stays
on the machine for admin logins only.

```sh
go/build.sh    # -> go/bin/agsshub-hubd and hub-descriptor (static)
./install.sh   # the daemon on :2422 (HUBD_ARGS in /etc/default/agsshub-hubd)
```

## What clients see

Every verb of `ssh hub <verb> …` and `ssh reg@hub …`: [README](../README.md), [identities.md](identities.md),
[live-send.md](live-send.md), [topics.md](topics.md).

| | |
|---|---|
| port | `-listen`, default 2422; clients' `Host hub` gets `Port` |
| host key | `/etc/hub/ssh_host_ed25519_key`: install.sh copies the machine's sshd key when there is one, so a client that pins the key keeps its pin |
| requests | exec only: `pty-req`, `shell`, `subsystem`, `env`, X11, agent and every forwarding are refused; `ssh hub` without a command is a shell request, refused; `ssh hub recv` streams the mailbox |
| keepalive | a probe after 15 s of silence, dropped after 4 unanswered (`-alive-interval`, `-alive-count`) |
| sessions per connection | 64 (`-max-sessions`) |
| a value-taking option given last (`send x --id`) | exit 2, `… needs a value` |
| a `-m` / `-m:rfc5322` body | over 64 MiB (the file cap) refused, exit 2 |
| a consuming read (plain `recv`, `--follow` and `--id` without `--peek`, `recv_bag` without `--peek`) | a message is consumed once its chunks are written whole to the connection, not once the client has received them: a dropped connection loses it, and a message cut mid-write stays. A reader that must not lose mail uses `--peek` and `recv_ack` |

## How it is built

| Part | |
|---|---|
| admission | in-process: `.byfpr`/`.ibyfpr` link, record key id, expiry, key type, an instance's address |
| registrar keys | `/etc/hub/registrars/authorized_keys`, read per login: the workspace is the last word of each line's `command="/usr/local/lib/hub/registrar <ws>"`, which nothing runs |
| a command | a goroutine per exec; its context ends when the channel or the connection closes |
| follow | a registry entry: a wake channel and the follow id (`<conn>.<session>`); the newest follower replaces the older (under the registry's mutex, the follow lock: registered and the old one marked, map operations only; the old one signalled after it, and writes its own final chunk); it re-checks its claim every second as well, so a claim change it was not woken for ends it within 1 s. Each pass writes a `part=live` chunk per open exchange not announced yet, each once, and an address's wake notice when it changed; a replaced follow ends with `0;end=replaced;by=<conn>.<session>`, a gone subscription with `0;end=gone` |
| live exchange | a table entry per `send --live` or `chat`: the accept session hands its stdin/stdout to the sender's goroutine, which relays both ways and posts the verdict |
| admission ([identities.md](identities.md#admitting-and-rejecting-an-instance)) | an in-memory mutex per instance; ask entries and flow C's transient entries in memory; nothing is kept about a refused id |
| wake notice | rendered at each read from an in-memory summary per address (each instance's unread senders and dates, re-read per instance on each change of its mailbox) and those entries |
| stored messages | one file each, `<msgid>.msg`: header block, body, payload (a file, or a bag's tar as received, never extracted; a bag's body is its `Msg:` member), a 212-byte trailer (`Header-Size`, `Message-Offset`, `Message-Size`, `Total-Size`, reserved `Sha256`), written in one pass; a content read whose stat differs from `Total-Size` moves the file to `<mailbox>/.damaged/` ([wake-live-event-id.md](wake-live-event-id.md#message-file)) |
| writes | no fsync on the send and publish path; `--durable` fsyncs the message file and its mailbox directory before exit 0; expiry notices are fsynced before the expired mail is deleted ([wake-live-event-id.md](wake-live-event-id.md#durability)) |
| flood buckets | `flood/<from>__<to>`, a mutex per bucket; the reaper removes full (idle) ones |
| event ids ([topics.md](topics.md#event-ids)) | per topic: an in-memory lock held across a publication's stamp and links, `done` in memory, the record `<session> <seq> <boot_id>` in `topics/<t>/record` (a start on another boot id: a new session), written at each publication, no fsync |
| reaper | a goroutine every minute (`-reap-interval`) |
| expiry notices ([expiry-and-subject.md](expiry-and-subject.md)) | `go/expiry` in-process |
| the session ended | noticed at once |

Three kinds of lock file, each an `flock`: `claims/.lock` keeps registry writers in different processes apart
(the daemon, `hub-mint`); each mailbox's `inbox/<mailbox>/.expired/.lock` keeps its notices' writers and
clearers one at a time; and the mailbox lock, `inbox/<mailbox>/.lock`, keeps a delivery into a subscription
or an instance, and the removal of either mailbox, one at a time. A lock taken on a mailbox renamed away
meanwhile is let go: the taker finds the mailbox gone.

## The operator tools

`hub-mint`, `hub-budget`, `hub-topic`, `hub-enroll`, `hub-alias` are shell tools on `lib/common.sh`: root runs
them, and they write files. **The daemon observes those files** rather than taking commands:

| Written by the operator | The daemon |
|---|---|
| claims (`hub-mint add/rm`, `--instances`) | reads them at every login and every command; watches `claims/` and `subs/` with inotify, so a follow whose claim changed ends at once |
| budgets, topics, aliases, settings, orgs, registrar keys | reads them where they are used, every time |

Why not subcommands talking to the daemon: the daemon would need write access to what only the operator may
write (`claims/operator`, budgets, topic policy, aliases), and the tools would need a running daemon.

`hub-mint` runs `lib/mint`, and `hub-topic register_description` runs `hub-descriptor`, which checks a
description for it ([help.md](help.md)).

## Install

`install.sh` installs the daemon and the operator tools, makes the parts of `claims/` the daemon writes
group-writable, and, under systemd, starts `agsshub-hubd.service`. In an image it leaves starting the daemon
to the entrypoint (`dev-hub/entrypoint.sh`). Re-running it keeps the state. On a machine that ran sshd's forced commands for the hub, it removes their drop-in, reaper timer and
helpers, which would otherwise serve and reap beside the daemon, and what an earlier hub kept per topic: its
publish ledger and pending records under `topics/<topic>/`, and its staged copies in `inbox/<topic>/`.

## Tests

| | |
|---|---|
| `cd go && go test ./...` | the hub in-process, the operator's state from the real `lib/mint`; `lib/mint`'s refusals; the SSH layer (exit status, refusals, MaxSessions, keepalive, a follow ending with its connection, a publisher's key) |
| `test/dev-hub.sh` | the hub through real ssh in a container, and the operator tools as root ([dev-hub.md](dev-hub.md)) |
| `test/full-disk.sh` | every send form against a really full disk, in a container |
