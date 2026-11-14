# Topics

A **topic** is an address like any other, but the hub answers for it: what is sent to it is fanned
out to its subscriptions instead of waiting in a mailbox. A **subscription** is a mailbox the hub
creates for one subscriber and hands back as a name. Everything else — the spool, each mailbox's dedup
gate — is what already exists.

```
topic+builds@acme.ssh          a topic: publish to it, subscribe to it
sub.builds+k3f9a2c1@acme.ssh   a subscription: a mailbox with one owner, fed by its topic
```

Both are legal identities under the existing grammar (`docs/identities.md`), with no workspace
part: the hub owns them, and the subscriber is recorded as the owner rather than written into the
name. `topic` and `sub` are **hub roles** — the registrar refuses to let anyone claim them.

## Verbs

```sh
ssh hub sub   topic+builds@acme.ssh                      # prints the subscription name
ssh hub unsub sub.builds+k3f9a2c1@acme.ssh

ssh hub send  topic+builds@acme.ssh -m:rfc5322 < event.eml   # publish: any send form, or send_bag
printf 'Gap: started\n\n' | ssh hub send topic+builds@acme.ssh -m:rfc5322   # a gap marker
ssh hub recv  sub.builds+k3f9a2c1@acme.ssh                  # drain what is waiting
ssh hub recv  sub.builds+k3f9a2c1@acme.ssh --follow         # live: the backlog, then each event
ssh hub recv_bag sub.builds+k3f9a2c1@acme.ssh --id <msgid>  # a bag that came through a topic

ssh hub list --type topic                                   # topics, and what you may do on each
ssh hub list --type sub                                     # your subscriptions
```

`sub` answers on stdout, so it composes: `S=$(ssh hub sub topic+builds@acme.ssh)`. It is
**idempotent** — the same owner asking on the same topic gets the same name back,
with its lifetime renewed. Re-running it at every start is the intended way to keep one alive.

A leading argument containing `@` names the mailbox to read, for `recv`, `recv_bag` and `recv_ack`
alike — a subscription, or your own identity; a message id never contains `@`, so the two cannot be
confused. Every other read option (`--peek[:<content>]`, `--id`, `--follow`, `--limit`,
`--header:stderr`; [wake-live-event-id.md](wake-live-event-id.md#reading)) works on a subscription as on your own mailbox, except that a subscription lists by
session and seq (*Event ids*), each delivery's msgid is its event msgid `event-<session>-<seq>`, and a read can end
with the position notice (*Event ids*). Only the owner may read one.

`chat` to a topic exits 9: a topic takes no live exchange.

## Modes

| `sub` | On each event |
|---|---|
| `sub <topic>` | lands in the subscription's mailbox; a `recv --follow` on it is woken at once |
| `--live`, `--wake` | exit 2 |

The event is in the mailbox, so a reader who was away loses nothing — **the mailbox is the
address**, here as everywhere.

`--follow` consumes each event once it has written it whole to the connection. Bytes written to a
connection that then drops are gone, so a reader that must not lose one uses `--follow --peek` and
acks what it has processed (`recv_ack <sub> <msgid>`). A subscription has one follower; a newer one
takes over, and only the follow of that subscription ends, with `0;end=replaced;by=<conn>.<session>`; the
subscriber's other connections go on. The rules are those of following any mailbox (README, *Following a
mailbox*; [wake-live-event-id.md](wake-live-event-id.md#takeover)).

## Publishing

Publishing is `send` or `send_bag` addressed to the topic — every message form works. The body is read
whole first, so a cut one (exit 4) takes no token and leaves nothing, and its id stays free
([complete bodies](complete-bodies.md)). The hub checks the sender against the topic's `publish` patterns,
takes a token from the topic's own rate bucket (not the per-pair one), stores the message, then fans out under
the topic's lock:

| Step | |
|---|---|
| 0, before the lock | with `--id event-…`, a `dup` or `stale` answers before the body is staged. The message stored once, through the ordinary spool with all its checks, in a folder of the hub's own (`inbox/.stage.*`), which is no mailbox; no fsync ([wake-live-event-id.md](wake-live-event-id.md#durability)) |
| 1 | the next `(session, seq)` from the record and the `--id` (*Event ids*); `dup` and `stale` answer here, nothing written |
| 2 | the record `<session> <seq> <boot_id>` written: a temp file, rename, no fsync (*Event ids*) |
| 3 | per subscription, under its mailbox lock: the message hard-linked in as `event-<session>-<seq>.msg`, its payload inside it. One link per subscription, one inode per event. Each delivered subscription's follower is woken |
| 4 | the stage removed; `done` set to this `(session, seq)`; the lock released |

The topic lock is an in-memory mutex per topic in the hub. It serialises publications and `sub`
on one topic, across the stamp, the record write and the links (the body's store is outside it), so deliveries appear in every mailbox in seq order. A `sub`
made while a publication fans out waits for it, and gets publications from the next one.

A publication lives only in the mailboxes that took it, and in the record's `(session, seq)`. The hub keeps nothing
else of it, and retries nothing.

| What happened | The publisher gets | The subscriptions |
|---|---|---|
| the message cannot be stored (the disk is full, say) | exit 1, `dispatch: cannot store the message: a hub I/O error; retry` | none has it; the record unchanged, so a retry is no `dup` |
| the record cannot be written | the same | none has it; the stage is removed and the record left as it was |
| it was stored | exit 0, `published -> <topic> msgid=event-<session>-<seq> subs=<n>`, `n` counting the mailboxes that took it | each that could take it has it, once |
| a mailbox cannot take it: its link fails, or the name exists already in it | the same, not counted | that one is skipped: the publication's seq is used, so its reader sees a gap. The journal says `publish <topic> event-<session>-<seq> from <publisher> subs=<n> failed=<k>` |
| `--id` names the record's own `event-<session>-<seq>` | exit 0, `dup` | nothing delivered |
| `--id` names a seq below the record's, same session | exit 0, `stale: last event-<session>-<seq>` | nothing delivered |
| `--id` not of the form `event-<session>-<seq>` | exit 2 | nothing delivered |
| a topic with no subscriptions | exit 0, `subs=0` | — |

Within the record's session, a repeat of its own msgid is `dup` and an older seq `stale`: neither is delivered. A repeat after a new session is a new publication: the publisher's input.

If the hub or the publishing session dies part way, the publisher gets no answer:

| It died | The subscriptions |
|---|---|
| before the links | none has it |
| during the links | those it linked have it; the rest have nothing |
| either way | after a process restart the topic continues at the record's seq + 1: a seq written and not linked everywhere is a gap. After an OS boot it starts a new session |

What it does next is the publisher's: a retry is a new publication, or `dup` when it passes the same
`--id event-…` and the record has it; or it publishes a `Gap` marker. The stage the dead one left goes with the
reaper.

`From:` on a delivered event is the publisher, stamped by the hub as always.

Whoever the policy names may publish. The role made for it is `publisher`
(`publisher+<app>@<org>.ssh`, [identities.md](identities.md#in-and-out)): minted by the operator, its
key held by one app's bridge, it receives nothing and sends to topics only — so its key, wherever it
ends up, can do nothing else. A publisher's sends are publications: the topic's bucket applies, never
a sender budget.

The hub stays SSH-only. An app that speaks only HTTP — its webhooks — publishes through the agsshub
bridge (`home/ssh-chat-bridge`): a sidecar in the app's network namespace that listens on
127.0.0.1 only and runs `send` as the app's `publisher`.

## Gap markers

A publisher whose source may have lost events before this one says so with a gap marker: a
publication whose one client field is `Gap: <reason>`. Subscribers get it like any
event, with its own seq (*Event ids*), with `Gap: <reason>` among its fields.

| Rule | |
|---|---|
| where | a send to a topic; `Gap` in any other send, `--live` included, exits 2 |
| alone | no `Subject`, `Reply-To`, `Re` or `Deadline` beside it, and one `Gap` field: exit 2 otherwise, naming the other field |
| reason | for logs, on the `Gap` line itself (a listing shows only that line): a `Gap` without one exits 2 |
| body | optional: a marker may be its field alone |

A gap found just before an event is two publications: the marker, then the event.

## Event ids

Every publication to a topic has a session and a seq, and its msgid is `event-<session>-<seq>`: the
topic's sequence session and the publication's number in it. It belongs to the publication, not to a recipient.

| Term | |
|---|---|
| session | the topic's sequence session: 16 lower-case hex digits. A new one starts only when the sequence is lost or undefined: a publisher's own new session; the hub with no record for the topic; a hub start whose boot id differs from the record's; seq passing int64 max. It is not a subscription's id |
| seq | the publication's number within its session: decimal, from 1, at most int64 max; meaningless without its session |
| msgid | `event-<session>-<seq>`, e.g. `event-3f9a2c1e8b7d6a54-7`: the publication's identity |
| a delivery | identified by its msgid alone; within a session, seq counts |
| a subscription | sees every session the topic goes through, so it can hold deliveries of several sessions |
| every subscription | the same msgid for one publication: one file, hard-linked into each |

| Item | |
|---|---|
| `--id` on a publish | `send <topic>` (each body form) and `send_bag <topic>`: `--id event-<session>-<seq>`, the publisher's own sequence, or no `--id`, and the hub assigns the next one. Any other `--id`: exit 2. The body is untouched: the msgid is an argument, not a field |
| `--durable` on a publish | accepted and ignored, `durable: ignored` on stderr: support needs a hub inbox (not built yet) ([wake-live-event-id.md](wake-live-event-id.md#durability)) |
| `--id event-*` elsewhere | on any other send, `--live` included: exit 2, a prefix reserved like `expired-`, `wakes-` and `head-` |
| record | `topics/<t>/record`: one line `<16hex> <seq> <boot_id>`, the kernel boot id (`/proc/sys/kernel/random/boot_id`) of the hub that wrote it. Written at step 2 of each publication, before any link: a temp file, then rename, an atomic replace; no fsync. A process crash keeps it, because the page cache survives. Created at the topic's first publication, removed with the topic (`hub-topic rm`) |
| read | once per topic, at its first publication or the first read of one of its subscriptions after the hub starts. Missing: `none`. Malformed: `none`, logged, overwritten at the next publication |
| `done` | per topic, in the hub's memory: the `(session, seq)` of the last completed fan-out. After a start, the record's `(session, seq)`; then set at the end of each fan-out |
| a restart, same boot | a topic keeps its session and continues at the record's seq + 1 |
| a start after an OS boot | the record's boot id differs: the hub mints a new session for that topic at its next hub-assigned publication; a publisher's own `--id event-<s>-<p>` is honoured as at any time. An OS crash can roll the unsynced record back; the new session keeps a seq from being used twice, and readers see one session change |
| int64 max | when the hub's own stamping (no `--id`) would pass it, it mints a new session and starts at 1 |

| Publish | Record before | The hub | Exit, stdout |
|---|---|---|---|
| no `--id` | `none` | mints a 16-hex session; seq 1 | 0, `published …` |
| no `--id` | `(s, q)` | `(s, q+1)` | 0, `published …` |
| `event-<i>-<p>` | `none` | `(i, p)` | 0, `published …` |
| `event-<i>-<p>`, `i ≠ s` | `(s, q)` | `(i, p)` | 0, `published …` |
| `event-<s>-<p>`, `p > q` | `(s, q)` | `(s, p)` | 0, `published …` |
| `event-<s>-<q>` | `(s, q)` | nothing delivered | 0, `dup` |
| `event-<s>-<p>`, `p < q` | `(s, q)` | nothing delivered | 0, `stale: last event-<s>-<q>` |
| any other `--id` | any | nothing delivered | 2 |
| a store or record write fails | any | record unchanged | 1, retryable |

| Publishers | |
|---|---|
| one with `--id event-…` | its `(session, seq)` is the record, kept across a hub restart on the same boot |
| several on one topic | pass no `--id`: the hub numbers them in one sequence. Each may publish `Gap`. One passing `--id event-…` among others: their coordination, outside the hub |
| a publisher repeating an old session's msgid after starting a new one | a new publication in a new session: the publisher's input, not a hub matter. Readers see a session change |
| a gap marker | a publication like any other: it takes a seq |

| Read | Where |
|---|---|
| every read | the msgid in each chunk's `;id=` extension ([wake-live-event-id.md](wake-live-event-id.md#reading)) |
| a `part=msg` chunk, `recv <sub> --id <msgid> --peek:rfc5322` | the header block and the message body: the hub's fields (`From`, `Date`, `Serial`; `Payload`, a bag's `Msg`), then the client's, then the body range; not the payload or the trailer ([wake-live-event-id.md](wake-live-event-id.md#message-file)) |
| a listing, a follow | arrival order, as in every mailbox: the message file's mtime, stamped under the topic's lock before each link, so seq order within a session; sessions interleaved by publishers list as published. Session ids need not increase |
| a clock step back across a hub restart | can misorder two sessions: the reader sees one extra session change, i.e. one extra rebuild ([wake-live-event-id.md](wake-live-event-id.md#fan-out)) |

```
6e;id=event-3f9a2c1e8b7d6a54-3;part=msg
From: publisher+gitea@home.ssh
Date: 2026-10-29T16:24:43Z
Serial: 1
Subject: issues

{"action":"labeled"}

```

```
From: publisher+gitea@home.ssh
Date: 2026-10-29T16:24:43Z
Serial: 1
Subject: issues

{"action":"labeled"}
```

Any other mailbox lists in arrival order too. Every read that names a msgid (`recv --id <msgid>`, `recv_bag --id <msgid>`, `recv_ack <msgid>`) acts on that one delivery, as in any mailbox.

| A reader, on a delivery | |
|---|---|
| same session as its last, seq ≤ last | a redelivery (a reconnect, or a crash before the ack): ack and skip |
| same session, seq > last + 1 | a gap |
| another session | a gap, unless its last is `none`; it tells its own `none` from the hub's `none` and decides |
| a `Gap` field | a gap |
| a `head-` notice beyond its last delivery | a gap |

| On a `head-<s>-<q>` beyond its last delivery | |
|---|---|
| beyond | the same session as the reader's last with `q` > last; or another session: a session change, a gap unless its last is `none` |
| a reader | either handles the gap (rebuild or rescan) and then sets its last to `<s>-<q>`, or ignores the head and waits for the next delivery, which then shows the jump. The reader's choice; the hub has no part in it |

A reader acks only after processing.

The hub never claims a gap and sends no gap marker of its own. What a subscription misses — mail the reaper
collects after `MSG_TTL`, a delivery whose link failed — shows as a skipped seq; mail the reaper
collects is also counted in an expiry notice ([expiry-and-subject.md](expiry-and-subject.md)), which carries
nothing about the session. A topic not yet published to reports `none`, which is no gap claim.

### Position notice

After the backlog, `recv <sub>` and `recv <sub> --follow` stream the topic's `done`, only when the mailbox
does not hold that publication:

```
37;id=head-3f9a2c1e8b7d6a54-41;part=msg
From: topic+builds@acme.ssh
Date: 2026-11-08T10:00:02Z


```

| Item | |
|---|---|
| msgid | `head-<session>-<seq>`, `head-none` for `none`: the value itself. A client's `--id head-*` exits 2 |
| identification | no `Subject`, no header: the msgid is the value |
| value | `done`, read before the backlog scan: a reader connecting during a fan-out never sees a head its mailbox lacks |
| use | only to detect a gap after the backlog: a head beyond the last delivery read. Never a position to ack up to |
| `recv` | a `part=msg` chunk, at most once per read or follow, never consumed; `recv_ack head-*`: `refused` |
| `recv <sub> --id head-…` | the notice chunk for the topic's current `head-` msgid; any other `head-`: `part=missing` |
| effect | none on the record |

## Policy

An operator creates a topic, and its policy, on the hub:

```sh
hub-topic add topic+builds@acme.ssh --publish 'main/ws-a@acme.ssh' --publish 'run.ci+*@acme.ssh'
hub-topic add topic+alerts@acme.ssh --publish '*@acme.ssh' --subscribe '*@acme.ssh *@beta.ssh' --rate 20
hub-topic rm  topic+builds@acme.ssh
hub-topic list
```

| Key | Default | Meaning |
|---|---|---|
| `publish` | nobody | shell patterns an identity must match to publish |
| `subscribe` | `*@<org>.ssh` | shell patterns an identity must match to subscribe — the topic's own org |
| `rate` / `burst` | 10/s, 100 | the topic's publish bucket, in messages |

Subscribing across an org line is therefore an explicit grant, never the default.

A topic can say what its events are: `hub-topic register_description <topic> < descriptor.json`, served by
`help <topic>` to its org and to whoever its policy admits ([help.md](help.md)).

## On disk

```
topics/<topic>/policy          topic=, publish=, subscribe=, rate=, burst=   (operator-written)
topics/<topic>/subs/<sub>      sub=, topic=, owner=, created=, expires=
subs/<sub>                     -> ../topics/<topic>/subs/<sub>   (lookup by name)
topics/<topic>/record          <16hex> <seq> <boot_id>
inbox/<sub>/                   the subscription's mailbox
inbox/<sub>/event-<session>-<seq>.msg   one delivery, its payload inside it
inbox/<sub>/.lock              flock: the mailbox lock, one delivery or removal at a time
inbox/.stage.*                 a publication's message while it fans out
```

A topic has no mailbox. The reaper expires subscriptions (default 30 days, `sub --ttl` up to 90; renewed by
re-`sub`) and removes their mailboxes with them. A temporary folder
(`inbox/.stage.*`, `inbox/<mailbox>/.stage.*`) that a killed hub left goes after an hour.

## Implementation

| | Where |
|---|---|
| resolving a topic or subscription name | `resolveTarget` in `go/hub` |
| publish, subscribe, unsubscribe, list | `go/hub/topic.go` |
| event ids: the record, `done`, the topic lock, the position notice | `go/hub/event.go` |
| the mailbox lock | `lockBox`, `dropSubBox` in `go/hub/continuity.go` |
| the fan-out | `topicPublish`, `fanout`, `deliverToSub` in `go/hub/topic.go`; the store, `storePut` and `storeBag` in `go/hub/spool.go` and `go/hub/bag.go` |
| gap markers | `gapProblem` in `go/hub/ident.go` |
| the publisher's limits | [identities.md](identities.md#implementation) |
| reading a subscription, `--follow` | `go/hub/follow.go` |
| hub roles refused at claim time | `Roles` (minter `hub`) in `go/hub/ident.go`; `ROLES` in `lib/common.sh`, for `lib/mint` |
| creating topics | `bin/hub-topic` |

## Not yet implemented

| | |
|---|---|
| `sub --webhook <url>` | an outbound callback per event, through a `backends/webhook/send` |
| an owner's view of who subscribes | `hub-topic list` shows counts only |
