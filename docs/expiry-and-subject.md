# Mandatory Subject and expiry notices

Status: `--subject` and expiry notices are built; enforcing `Subject` is not (*Enforcement*).

Every message a client sends to an identity names its verb in `Subject`. A message without one is the
hub's own, or a client's message to the hub. When mail expires uncollected, the mailbox's owner learns
what was lost, before anything else it reads.

## Enforcement

| Now | Once enforcement is on |
|---|---|
| `--subject` and its grammar; `--id expired-*` refused; notices. A send without `Subject` is still stored | a send without `Subject` refused; exactly one non-blank `Subject:` in a header block; `help` descriptors lose `subject: null` |

## Subject

| Message | `Subject` |
|---|---|
| to an identity, an alias, or a topic | required: without one, exit 2, nothing stored. No flood token, except `--live -m:rfc5322`, where the header block is read after the token and any wake, as for `Gap` today |
| a gap marker: to a topic, with `Gap` | none; `Gap` stays its only client field |
| a `chat` | `chat` in the accept header block, set by the hub; `chat` takes no `--subject` |
| an expiry, wake or position notice | none: rendered by the hub |

| Form | How it is set |
|---|---|
| `-m:rfc5322` | `Subject:` in the header block |
| `-- <message>`, `-m`, `--file <name>`, `send_bag` | `--subject <verb>` |
| `--live`, `--chunked` | as the form they are added to |

| Rule | |
|---|---|
| `--subject` | one word: an alphanumeric, then `[A-Za-z0-9._/-]`, 64 at most; otherwise, or with no value, exit 2. With `-m:rfc5322`: exit 2, since the header block carries it |
| `Subject:` in a header block | exactly one, non-blank on its own line, otherwise exit 2. Free text stays allowed: the verb is its first word |
| where it is stored | a client field of the message, after `Payload` and `Msg` on a bag, so `recv` shows it the same way for every form |
| a live request | `--subject` joins the hub header block on the recipient's `accept`, after `Payload`, before `Transfer-Encoding`; with `-m:rfc5322` it stays in the decoded body. A reader of `-m` or `--file` live requests reads it there |
| an alias's backend | `HUB_SUBJECT` in its environment |

`help` descriptors lose `subject: null`: `subject` becomes a required string, and `files` no longer
forces its absence. Text requests name a verb like any other. The one registered description has none.

## Expiry notices

When the reaper expires a message uncollected for `MSG_TTL`, it counts it in one notice per sender and
mailbox. A reader sees the notice as a message from that sender, before any other message.

```
From: commhub@home.ssh
Date: 2026-11-08T05:00:00Z
Expired: 17

verb	count	oldest	newest
in.action	2	2026-11-01T06:02:00Z	2026-11-01T08:15:00Z
out.*	10	2026-11-01T05:10:00Z	2026-11-01T09:40:00Z

special	count	oldest	newest
[gap]	1	2026-11-01T05:01:00Z	2026-11-01T05:01:00Z
[others]	3	2026-11-01T05:30:00Z	2026-11-01T07:10:00Z
[no-subject]	1	2026-11-01T07:00:00Z	2026-11-01T07:00:00Z
```

| Field | |
|---|---|
| `From` | the sender of the expired messages, as the hub stamped it when they were sent |
| `Date` | when the notice last changed |
| `Expired` | the total. Not a client field, so no client can set it |
| `Subject` | none |

A table with no rows is left out. `oldest` and `newest` are the `Date` of the row's messages, or the
file's modification time for one without.

| Bytes | |
|---|---|
| tables | fields separated by a TAB; every line ends in a newline; one blank line between two tables, none at the end |
| a `part=msg` chunk, `recv --id <msgid> --peek:rfc5322` | `From`, `Date`, `Expired`, a blank line, the tables |

| Trust | |
|---|---|
| a notice | no `Subject`, and an `expired-` msgid. A client's `--id expired-*` is refused, exit 2 |
| the stream | length-framed: a message's body cannot imitate a notice ([wake-live-event-id.md](wake-live-event-id.md#the-stream)) |
| a reader's only action | log it, ack it, optionally rescan |

### Groups

The JSON holds raw counts per verb. Grouping happens at rendering, as a pure function of those rows, so
it does not depend on the order of sweeps.

| Step, when counted | The message goes to |
|---|---|
| 1. a `Gap` field | `gap` |
| 2. no `Subject`, or a blank one | `no_subject` |
| 3. the first word of `Subject`, split on ASCII space and tab, matches `[A-Za-z0-9._/-]{1,64}` | that word's row |
| 4. otherwise | `others` |
| more than 1,024 verb rows | the message goes to `others` |

| Step, when rendered | |
|---|---|
| first, over 32 characters | the verb becomes its longest prefix ending in one of `. / - _` within 32 characters, written `<prefix>*`, or goes to `[others]` if it has none. Rows of one name sum: counts add, lowest `oldest`, highest `newest` |
| then, over 16 verb rows | merge **all** rows sharing the longest prefix ending in a separator into `<prefix>*`, and repeat. Ties: the prefix first in byte order. No shared prefix: the row with the lowest count, then last in byte order, goes to `[others]`. Each step leaves fewer rows, so it ends |
| order | verb rows in byte order. `[gap]`, `[others]`, `[no-subject]` in that order, in their own table |

`*` and `[` lie outside the verb alphabet: a real verb cannot read as a prefix row or a special row.

### Order

| Where | |
|---|---|
| `recv`, `recv --peek` | notices first, in byte order of `from`, then messages in the mailbox's order. Neither: the final chunk alone |
| `recv --follow` | notices before the backlog; one that appears later streams before the next message, and the reaper's change nudges the follower |
| a subscription | the same, then the position notice ([topics.md](topics.md#position-notice)); a notice carries nothing about the topic's session, and the skipped seqs still show the gap |

### Reading and clearing

| | |
|---|---|
| msgid | `expired-<first 16 hex of sha256(sender)>-<unix nanoseconds of the last change>` |
| a full read (`recv`, `--follow`, `--id`, without `--peek`) | write it, take the lock, read it again; removed only if the msgid is unchanged |
| `--peek` | streams it, keeps it; a `--peek` follow streams each msgid once; `recv_ack <msgid>` clears |
| `recv_ack` of an older msgid | `gone <msgid>`, exit 0, changes nothing: the newer one is due, with cumulative counts |
| `recv_ack` of `expired-*` with no notice from that sender | `gone <msgid>`, exit 0 |
| `recv --id <msgid>` | the rendered message, read as the current msgid only (`part=missing` for an older one); consumes without `--peek`, as for mail. No `Serial` |
| `--peek:file`, `--peek:bag`, `recv_bag` | exit 2: a notice has no payload |
| the flood bucket | not involved |

### On the hub

The stored form is JSON, rendered when read. It is never a `.msg`, so nothing expires it.

`inbox/<mailbox>/.expired/<sender>.json`, `/` → `~`, written by rename:

```json
{"from":"commhub@home.ssh","msgid":"expired-3f2a…-1793999999000000000","date":"2026-11-08T05:00:00Z","expired":17,
 "verbs":{"in.action":{"count":2,"oldest":"…","newest":"…"}},
 "gap":{"count":1,"oldest":"…","newest":"…"},
 "others":{"count":3,"oldest":"…","newest":"…"},
 "no_subject":{"count":1,"oldest":"…","newest":"…"}}
```

| Step, per expired message | |
|---|---|
| 1. claim | rename it to `.expired/.claim.<msgid>`. A failed rename (a reader took it, or the reaper lacks write): skip it, no count, logged once per mailbox |
| 2. count | once per sender per sweep: read the claimed messages' header blocks, merge into the JSON under `.expired/.lock`, one nudge |
| 3. delete | the claim: one file, payload included |
| a crash | the next sweep finishes leftover claims. Between counting and deleting, that sweep's claims for that sender may count twice; none is lost. Once per sender per sweep, the JSON, `.expired/` and the mailbox are fsynced before the claims are deleted |
| a race | a reader listing during a sweep may see neither a claimed message nor its notice until the sweep ends, and may print a message that is then counted, as it could with today's delete |
| a claim with no `From` | logged and deleted |
| a bad notice file | moved to `.bad.<name>`, logged, the count starts again |
| a panic | recovered per mailbox: one bad mailbox cannot stop a sweep, or the hub |

| Item | |
|---|---|
| modes | `.expired` 2770, files 0660, whatever the daemon's umask |
| hard links | a topic's fan-out links one file into many mailboxes: each mailbox counts its own |
| a mailbox gone | `.expired` is made with `Mkdir`, not `MkdirAll`, so it cannot bring a removed mailbox back |
| a mailbox nobody can read | `.expired` removed once no claim or address can receive there: a removed identity, a released instance whose address is gone. Topic mailboxes (`inbox/<topic>/` staged copies) never get notices; subscriptions do |
| an expiring subscription | deletes its unread mail and notices with its mailbox, unreported, as today: renew with `sub` to keep them |
| waking | a notice wakes nothing; `instances` counts mail only (`mail=<n>`) |
| one implementation | package `go/expiry`, in-process in the hub |
| rollback | an old hub ignores `.expired` |

## The notice probe

`notice_probe` in `/var/lib/hub/settings` shows a consumer a notice with nothing in it, so what it does with one can be watched before any mail expires.

| Setting | |
|---|---|
| `notice_probe=<patterns>` | shell patterns over identities, separated by spaces, in the alphabet and limits of `--see`. Read when a follow starts |
| absent, empty, or any pattern outside the alphabet | off, for everyone |

| A probe | |
|---|---|
| when | a matching identity opens `recv --follow`, with or without `--peek`, on its own mailbox or a subscription it owns: one, before anything else on that follow. Other reads are unchanged |
| in the stream | a `part=msg` chunk: `From`, `Date`, `Expired: 0`, a blank line |
| `From` | the session's own identity |
| msgid | `expired-<first 16 hex of sha256(identity)>-<whole unix seconds>000000000`. A stored notice's nanoseconds are never a whole second: a change landing on one takes the next nanosecond |
| stored | nothing: no `.expired`, no JSON |
| the journal, the hub's log | one `notice_probe:` line per probe shown and per probe acked |

| Reading a probe | |
|---|---|
| `recv_ack` | `acked <msgid>`, exit 0, changes nothing, whatever the setting says now. Only with the session's own sender hash and a whole second greater than 0; every other `expired-*` with no such notice is `gone` |
| `recv --id` | `part=missing`, as for a missing message; `recv_bag --id`: exit 3 |

## Tests

| Suite | Checks |
|---|---|
| `go/expiry` | each row of the group tables; ties; the same rows in any order render the same bytes; 17 rows; 1,025 verbs; a verb that looks special |
| `go/hub` | each row of the Subject tables that takes effect now, on every form, `--live` and bags included; `--subject` as the last argument; a notice first in `recv` and `--follow`, also one appearing during a follow; clearing racing a change; `recv_ack` and `recv --id` of an older msgid; `--id expired-*` refused; a body that looks like a notice, streamed as a body; a crash at each step (`testHook` at `claimed`, `counted`, `deleted`); a bad notice file; modes under the daemon's umask; one fixture through every read; a probe on a mailbox and on a subscription |
