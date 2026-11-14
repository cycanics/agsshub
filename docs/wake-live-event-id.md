# Wake, live and event ids

Status: built in the hub (`go/hub`, `go/descriptor`, `install.sh`), *Reading* included. `live-send.md`, `identities.md`, `topics.md`, `complete-bodies.md`, `expiry-and-subject.md`, `help.md`, `dev-hub.md`, `hub.md`, `README.md` and `SKILL.md` describe this design; the table *Replaces* names their sections that carry it.

## Terms

| Term | Meaning |
|---|---|
| `addr` | an address that takes instances (`hub-mint add … --instances`) |
| `addr+inst` | one instance of `addr`; its mailbox is `inbox/addr+inst/` |
| follow | a `recv --follow` session on one mailbox |
| S | the sender's ssh session |
| H | the hub |
| A | the process holding `addr`'s follow |
| I | the instance process; it follows `addr+inst` |
| exchange | a LIVE rendezvous in H's memory, keyed by `<sid>`; it exists while S is connected |
| wake set | the instances of `addr` that H lists in the wake notice |
| transient entry | an in-memory member of the wake set, made by a `--live` to an `addr+inst` nobody follows; nothing stored |
| ask entry | an in-memory member of the wake set, made by a `send`, `send_bag`, `send --live` or `chat` to an `addr+inst` with no mailbox, or by a `--wait` send to an `addr+inst` with a mailbox and no follow, while S is held; gone with A's answer, the wake timeout, a `register` of `addr+inst`, a follow of `addr+inst` starting, or its last held S |
| record | per topic, `topics/<t>/record`: `<session> <seq> <boot_id>`, the last publication's, or `none`; used for seq assignment |
| boot id | the kernel's `/proc/sys/kernel/random/boot_id`, new at every OS boot |
| kind | a live exchange's type: `send --live`, `send --live --chunked`, `chat`, `chat --chunked` (*LIVE*) |
| session | the topic's sequence session: 16 lower-case hex digits. A new one starts only when the sequence is lost or undefined: a publisher's own new session; the hub with no record for the topic; a hub start whose boot id differs from the record's; seq passing int64 max. It is not a subscription's id |
| seq | the publication's number within its session: decimal, from 1, no leading zeros, at most int64 max; meaningless without its session |
| event msgid | a topic publication's msgid, its identity: `event-<session>-<seq>`, e.g. `event-3f9a2c1e8b7d6a54-7` |
| a delivery | identified by its event msgid alone; within a session, seq counts |
| a subscription | sees every session the topic goes through, so it can hold deliveries of several sessions |
| every subscription | the same event msgid for one publication: one file, hard-linked into each |
| done | per topic, in H's memory: the `(session, seq)` of the last completed fan-out, or `none` |
| wake notice | H-rendered, no `Subject`, header `Wake:`, in `addr`'s mailbox |
| position notice | H-rendered, no `Subject`, no header, in a subscription's mailbox: its msgid `head-<session>-<seq>` or `head-none` carries `done` |
| mailbox lock | `<mbox>/.lock`: flock, one delivery or removal of that mailbox at a time |
| admission lock | an in-memory mutex per `addr+inst` in H (a refcounted map, no file): one admission decision for `addr+inst` at a time, with or without its mailbox |
| topic lock | an in-memory mutex per topic in H (one hub process), held across steps 1–4 of *Fan-out* |
| message file | `<msgid>.msg`: one file per stored message: header block, body, payload, 212-byte trailer (*Message file*) |
| payload | a `--file`'s bytes or a `send_bag`'s tar, exactly as received, inside the message file |
| stored | exit 0 of a send: the message survives a process crash; after an OS crash or power loss the last few seconds of writes can be lost |
| durable | exit 0 of a send with `--durable`: the message also survives an OS crash or power loss (*Durability*) |

| Rule | |
|---|---|
| content | not H's: a body and a payload are stored as received (a chunked body decoded); H checks completeness and its own disk, not what they hold |
| pending state | only in the receiving mailbox, or in H's memory tied to a connected session. H keeps no queue, no line list, no retry |
| the one exception | the flow C transient entry: in H's memory after S exits 6, nothing stored, ended by its `stay-up-till` or I's follow (owner-approved) |

## Replaces

| Existing section | Replaced by |
|---|---|
| `live-send.md` *Wake*; *Recipient* (the `WAKE` line paragraph); *On the hub* rows one wake per instance, "a wake's too"; row wake timeout (now for ask entries only) | *Wake notice*, flows A2, C2, C |
| `live-send.md` *Invocation* target and sender rows (exit 2 / 3 for non-live ends); *Outcomes* rows 2, 3, 6 | *Exits* |
| `identities.md` *Waking an instance*; *Registering an instance* row `mailbox` ("the next send wakes the instance"); *Instances* row any other address | flows A, A2, C; *Wake notice*; *Admitting and rejecting an instance* |
| `README.md` *Exit codes* `6` ("(`--live`) recipient not live"), `8`; no `9` | *Exits*: 6 also for `send` / `send_bag` to an instance; 8 now `--wait`'s not picked; 9 added |
| `README.md:70` (per-subscription `Sub-Id`), `README.md:84` (the `WAKE` line), `README.md:91` (`Sub-Id` / `Sub-Seq` added per subscription) | *Event ids*, *Wake notice*, *Fan-out* |
| `hub.md:34` (follow: a queue of LIVE/WAKE lines); `hub.md:39`, `hub.md:46` (`inbox/<sub>/.seq/.lock` and its numbering) | *LIVE*; *Fan-out* (mailbox lock) |
| `topics.md` *Publishing* steps 3–5; row "a mailbox holds a message with that id already … uses no number" (`topics.md:88`); "A repeated id is a new publication … one that holds it refuses it" (`topics.md:93-96`); the table "If the hub or the publishing session dies part way"; "A subscription made while a publication fans out" | *Fan-out* (a repeated msgid is `dup`) |
| `topics.md` *Continuity* (ids and numbers per subscription, numbering rows, subscriptions from before ids, "Neither field is in the stored message", listing order); *On disk* `inbox/<sub>/.seq/*`; *Implementation* row ids and numbers | *Event ids*, *Upgrade* |
| `help.md` descriptor fields | `live: false` (*Exits*) |
| `expiry-and-subject.md` *Bytes* rows listing and raw (`Sub-Id` in a subscription); *Order* row a subscription; *The notice probe* (`Sub-Id`) | *Event ids*: notices carry nothing about the session |
| `live-send.md` *Invocation* row `--chunked` ("implied"); *Framing* rows live request, live reply | *LIVE* (kinds, framing) |
| `identities.md` *Channels* (held-open `chat`, the doorbell); *Implementation* row `ringFor`; `topics.md` *Modes* `--wake`; `who`; chat with no peer, `@identity`, spooled or published chat lines | *LIVE* (`chat`); *Removed from H* |
| `topics.md` *Event ids* rows record, clean shutdown, `done`, a restart; `hub.md` *How it is built* row event ids | *Event ids* (record) |
| `README.md` *Bags* (name judging, type filtering, caps), *Message spool* (the attachment folder), *Receiving* (`-m:rfc5322` is what is on disk); `complete-bodies.md` exit 2 on `send_bag`; `topics.md` *Publishing* step 3 (a bag's tree), *On disk*, the `Bag` field; `expiry-and-subject.md` the attachment folder | *Message file* |
| `topics.md` *Publishing* step 0 ("fsynced"); `expiry-and-subject.md` *a crash* (what is synced) | *Durability* |
| `identities.md` *The ask* row `chat`; *In and out* row publisher (`send --live` exit 3) | flows A2, C2 (`chat`); *Exits* 9 |
| `topics.md` *On disk* `mode=` in `topics/<t>/subs/<sub>` | *Removed from H* |
| `README.md` the `recv` lines of the opening example, *Receiving*, *Following a mailbox* `--keep`, *Bags* (fetching), *Expiry notices* (clearing, trust); `SKILL.md` *Receiving*; `topics.md` *Verbs* (the recv options), *Modes* (`--follow --keep`), *Read*, *Position notice*; `live-send.md` *Recipient* (`--keep`), *Wake notice* rows `recv`, trust; `expiry-and-subject.md` *Bytes*, *Trust*, *Order*, *Reading and clearing*, the probe's reads; `hub.md` a consuming read; `help.md` the `recv` usage | *Reading* |

## Flows

| # | Case | Steps |
|---|---|---|
| A | `send` / `send_bag` to `addr+inst` without `--wait`, mailbox present, followed or not | H stores m under the mailbox lock at once → S exit 0. Not followed: `addr+inst` joins the wake set → H prints the wake notice on `addr`'s follow → A starts I → I follows `addr+inst` → I receives m |
| A1 | `send` / `send_bag` to `addr+inst` with `--wait`, mailbox present and followed | H stores m under the mailbox lock → S held for the pickup (*Wait for pickup*): exit 0 picked, 8 not picked within `<s>`, 3 if `reject` lands first |
| A2 | `send` / `send_bag` to `addr+inst`, no mailbox; or with `--wait`, mailbox present and not followed | H takes S's flood token → holds S → adds an ask entry `{addr+inst, From: S}` → prints the wake notice (the line's `ask=` field) on `addr`'s follow → A answers: `admit <inst>` (H makes `inbox/addr+inst/` if absent, stores m; S exit 0, or with `--wait` 0 picked / 8 not picked within `<s>`) or `reject <inst>` (S exit 3, nothing stored). No answer within `wake_secs`: if the mailbox exists at that moment, m is stored, S exit 8; if not, nothing stored, S exit 6, `retry-after=<live_retry_secs>` on stderr |
| B | `send --live` or `chat` to a followed target (`addr`, or a followed `addr+inst`) | H opens exchange `<sid>` → prints `LIVE <sid> <from> <msgid> (chunked\|raw) [chat]` on the target's follow → the target opens a new session `ssh hub accept <sid>` → its accept stdin starts with a `Status` header block: `2xx` (the stream follows) or any other (the end; S exits 0 with it) → no accept within `live_accept_secs`: S exit 6 |
| C2 | `send --live` or `chat` to `addr+inst`, no mailbox | as A2, nothing stored: `admit` → H makes `inbox/addr+inst/`, then flow C; `reject` → S exit 3; no answer within `wake_secs` → S exit 6, `retry-after` |
| C | `send --live` or `chat` to `addr+inst` with a mailbox, not followed | H adds a transient entry `{addr+inst, From: S, stay-up-till}` → S exit 6, `retry-after=<live_retry_secs>` on stderr; nothing stored → H prints the wake notice on `addr`'s follow → A starts I → I follows `addr+inst` → S reconnects: flow B |
| D | publish to a topic | *Fan-out*; a failed link: m absent in that mailbox, the publication's seq used → that reader sees a gap |

| Flow C | |
|---|---|
| `stay-up-till` | the live attempt's time + 3 × `live_retry_secs` |
| transient entry | expires at its `stay-up-till`, or when I follows `addr+inst` |
| S retries before I follows | exit 6 again; the entry is refreshed (`stay-up-till` from this attempt), not added twice |
| H restart | entries lost; S's next retry makes one again; nothing stored, nothing lost |
| I never starts | each retry by S makes a wake again; each retry spends S's flood token |
| retry | `retry-after` is a hint; S decides when or whether to retry |
| future option, not this flow | H keeps S connected and hands it to I once I follows. Transparent to S: the same exits |

| Flow A2 | |
|---|---|
| order | admission always precedes wake: an ask is answered before any wake entry exists for that `addr+inst` (flows A2, C2) |
| flood token | spent when S is held, before the ask |
| ask cap | at most 256 ask entries per `addr`; a send that would make one more: exit 5, rate limited |
| several S on one `addr+inst` | one ask entry, `send`, `--live` and `chat` alike; an S that arrives while it is pending joins it; every held S gets the one answer (or the one timeout) |
| `chat` | held and asked as `send --live`: `admit` → flow C, `reject` → exit 3, no answer → exit 6 with `retry-after` |
| A's answer | keyed by `<inst>`, no `<sid>`: `admit` / `reject`, one or many instances per call. Hub verbs A runs; the hub keeps nothing about a refused id |
| `register`, a follow | `register` of `addr+inst` while its ask is pending answers it as `admit`. A follow of `addr+inst` starting ends the ask the same way: its mailbox exists, each held S then stores and, with `--wait`, waits for the pickup |
| `addr` not followed | the ask holds S all the same, up to `wake_secs`: an A that connects or reconnects within it sees the ask in its wake notice. Its outcome (exit 6 or 8) comes only at the timeout, and looks at the mailbox then, not when S was held |
| ask entry | in memory only; gone with the answer, at `wake_secs` (`/var/lib/hub/settings`, default 60; used for ask entries only), with `register` or a follow starting, or when its last held S disconnects |
| H restart | ask entries lost; their S sessions end with the hub; nothing stored |

`accept <sid>` remains only for LIVE: several exchanges to one target may be open at once, and `<sid>` resolves each without a queue in H. A wake has no `<sid>` and no `accept`.

## Wake notice

```
From: group.support@expa.ssh
Date: 2026-11-08T10:00:02Z
Wake: 4

group.support+a1b2c3d4@expa.ssh	mail=3	from=commhub@home.ssh@2026-11-08T09:58:40Z,frontline@home.ssh@2026-11-08T09:12:03Z
group.support+b7c8d9e0@expa.ssh	mail=1	from=commhub@home.ssh@2026-11-08T09:59:10Z	ask=commhub@home.ssh
group.support+e5f6a7b8@expa.ssh	mail=0	from=main/ws-a@acme.ssh@2026-11-08T10:00:00Z	stay-up-till=2026-11-08T10:00:15Z
group.support+c9d0e1f2@expa.ssh	ask=commhub@home.ssh
```

| Item | |
|---|---|
| mailbox | `addr`; an address that takes no instances gets none |
| `From` | `addr` itself |
| `Date` | the render time |
| msgid | `wakes-<first 16 hex of sha256(body and the held asks' numbers)>`: a new msgid whenever the content changes, and for an ask made again after its answer, whose line reads the same |
| identification | no `Subject`; header `Wake: <n>`, the number of instance lines (the header line stays bounded) |
| wake set | each `addr+inst` with no follow, with `.msg` mail in `inbox/addr+inst/`, a transient entry, or an ask entry. Expiry notices do not count |
| line | per instance, TAB-separated: `<addr+inst>`, then the fields it has: `mail=<n>` (`.msg` files only, not expiry notices) and `from=<id>@<UTC>,…` for mail or a transient entry, `stay-up-till=<UTC>` on a transient entry only, `ask=<id,id,…>` on an ask entry. Lines with mail or a transient entry first, then lines with only `ask=`, each in name order. No cap on lines |
| `from=` | per distinct sender, its H-stamped `From`, `@`, the newest H-stamped `Date` of that sender's unread mail in `inbox/addr+inst/`, or of its transient entry |
| `ask=` | the held senders, distinct, in name order: A answers with `admit` or `reject`. Independent of `mail=`: a `--wait` send held on a mailbox with mail carries both on one line (a client that stores a notice first, then sends a bag with `--wait`); the dates A needs are on `from=` |
| rendered | at read, from H's in-memory wake summary (per address, per instance: each unread `.msg`'s sender and `Date`), the follow registry and the ask and transient entries; never stored, never a `.msg` |
| wake summary | built from disk at the address's first render; one instance's mailbox listed again after each change of it (delivery, consume, ack, expiry, `reject`), headers read only for its new files; dropped for an address whose operator claim changes (`hub-mint`), for all when inotify lost events, and at each reaper sweep while inotify is not running; rebuilt at the next render |
| printed | on connect and on change only: first on every follow of `addr`, `Wake: 0` included; again while connected whenever its msgid changes. An unchanged notice is not printed again; A retries a start that failed itself |
| `recv` | streams it as a `part=msg` chunk, never consumes it; `recv_ack wakes-*`: `refused` |
| `recv --id wakes-<any>` | the current rendering, with the current msgid |

| Nudges `addr`'s follower | |
|---|---|
| events | a delivery to `addr+inst`; a consume, ack or expiry there; a follow start or end on `addr+inst`; `admit`, `reject`, a claim change of `addr+inst`; an ask or transient entry made, refreshed or ended |
| cost | a render reads no mailbox once the summary is built; an event re-reads that one instance's headers |

## Admitting and rejecting an instance

```sh
ssh hub admit <inst> [<inst> …]      # as addr; an ask, or any instance at any time
ssh hub reject <inst> [<inst> …]     # as addr; an ask, or any instance at any time
```

| Item | |
|---|---|
| who | the holder of `addr`, over its own connection, for its own instances |
| `admit` | H makes `inbox/addr+inst/` if absent and answers a pending ask: held `send` / `send_bag`: their messages stored, S exit 0, or with `--wait` 0 picked / 8 not picked within `<s>`; held `--live` and `chat`: flow C |
| `reject` | "no such instance", at any time, for one instance or many: under the registry lock and then the admission lock H ends the claim if there is one and answers a pending ask (held senders: exit 3, nothing stored); then it renames `inbox/addr+inst/` away under its mailbox lock and removes it, if it exists: its mail is discarded, no expiry notice, and the journal says `reject <inst> by <addr> discarded=<n>`. Every S in `--wait` on that mailbox gets exit 3. With nothing to do, exit 0. Earlier exit 0s stand; no ordering promise between a send and a `reject` |
| `reject` whose mailbox cannot be removed | exit 1; the claim is ended, the ask answered, the mailbox and its mail stay, nothing reported as discarded. `reject` retried does it |
| reaper | new step: removes flood buckets that are full (`flood/<from>__<to>` back at its burst: idle state). `reject` needs no step of its own: nothing is recorded about a refused id |

| Admission, a send to `addr+inst` | |
|---|---|
| lock | the admission lock covers only the decision (mailbox? followed?) and the mkdir, for a send, the ask entry, `admit`, `reject`, `register` and the start of an instance's follow. Order: registry lock (claims lock), then the admission lock |
| a held S | waits outside the admission lock; when answered, it checks the mailbox: `admit` or `register`, then a `reject` landing meanwhile: no mailbox, exit 3 |
| the store | stages outside the mailbox (`inbox/.stage.*`, as a topic publication does), takes the mailbox lock, re-checks under it that the mailbox exists, then links. The mailbox gone: the stage is removed, S exit 3. Never under the admission lock, never while the registry lock is held |
| a mailbox | exists exactly when the directory `inbox/addr+inst/` exists: H's only test, no separate record; an empty directory counts |
| who makes the mailbox | one function, under the admission lock: used by `admit`, by `register` answering an ask, and by an instance's connect, which first re-checks its claim under the same lock, so a connect during a `reject` cannot recreate the mailbox. Nothing else makes an instance mailbox: no `MkdirAll` on one anywhere |
| mailbox present, no `--wait`; `--wait` and followed | stored at once (flows A, A1) |
| no mailbox; or `--wait` and not followed | held for A's answer (flows A2, C2), `chat` included |

## LIVE

| Item | |
|---|---|
| announcement | `LIVE <sid> <from> <msgid> (chunked\|raw) [chat]` on the target's follow, as a `part=live` chunk (*Reading*), written from the open exchanges addressed to it that it has not printed yet. The framing token is always present; `chat` marks a chat |
| H state | the exchange table only; no per-follow line list. A closed exchange is never printed |
| order | none: LIVE lines and notices may arrive in any order |
| accept, reject | the target's: `accept <sid>` then `2xx` or `4xx`; or no accept: S exit 6 |
| a line for an exchange already closed | `accept` exit 3; the target drops it |
| a line the receiver does not support | the receiver ignores it; S exit 6 after `live_accept_secs` |

| Command | LIVE line | Streams | End of a direction |
|---|---|---|---|
| `send <peer> --live` | `… raw` | request: S's stdin raw until EOF, at most `FileMaxBytes` (64 MiB); reply: A's accept stdin raw until EOF, no size cap | EOF; a dropped connection gives exit 7 |
| `send <peer> --live --chunked` | `… chunked` | both framed (HTTP/1.1 chunked, done by the caller, not by H), request at most `FileMaxBytes` (64 MiB), reply with no size cap, H checks each direction | final chunk; EOF without a final chunk, or `Abort`, gives exit 4; a drop gives exit 7 |
| `chat <peer>` | `… raw chat` | open-ended streams both ways, raw; the back stream starts with A's `Status` header block | EOF from each side; the session ends when both have closed; a drop gives exit 7 |
| `chat <peer> --chunked` | `… chunked chat` | open-ended streams both ways: S frames its stdin, A frames its accept stdin after its `Status` header block; H checks each direction and forwards it framed, so each reader gets the final chunk and any `Abort`; no size cap | final chunk each way; a missing one or `Abort` gives exit 4; a drop gives exit 7 |

| Rule | |
|---|---|
| framing | the caller frames. H never frames on a caller's behalf, and decodes and checks only when the stream is chunked. In raw mode H copies bytes |
| accept header | A's accept stdout header block carries `Transfer-Encoding: chunked` only for chunked kinds |
| request of `send --live` | at most `FileMaxBytes` (64 MiB), the per-file spool cap; past it: `body exceeds cap`, exit 2 (`go/hub/live.go:188`) |
| reply of `send --live` | raw and chunked alike: a `Status` header block required; the body has no size cap, as in `go/hub/live.go` today |
| back stream of `chat` | A's accept stdin starts with a `Status` header block: `2xx`, then the stream; any other `Status`, then the end, and S exits 0 with the `Status` on stdout. S's stdout gets the block, then the stream |
| `--size` | with `--live` or `chat`: exit 2 |
| target | a topic, a publisher or a `live: false` identity: exit 9, for every kind |
| sender | a publisher: exit 9, for every kind |
| storage | nothing is ever stored |
| `chat <peer>` | a verb of its own, not an alias of `send`; no `--mode` flag. A peer is required; no `@identity` addressing, no offline spooling to the mailbox |
| `chat` msgid, `Subject` | `chat` takes no `--id` and no `--subject`: H mints the msgid, and the accept header block carries `Subject: chat` |

## Exits

| Exit | `send --live`, `chat` |
|---|---|
| 2 | `--size` or `--durable` with `--live` or `chat`; `--id` or `--subject` with `chat` |
| 4 | a chunked kind: a direction ended without its final chunk, or with `Abort` |
| 6 | not active: the target is not followed, or did not accept within `live_accept_secs` (a receiver that does not support the LIVE line ignores it); or an `addr+inst` not followed (flow C), or no answer to its ask (flow C2), which carry `retry-after=<s>` on stderr, `s` = `live_retry_secs` in `/var/lib/hub/settings`, default 5. Nothing delivered |
| 9 | live not supported: a topic, a subscription, an alias; a publisher as target or as sender; an instance form of an address that takes no instances; an identity whose descriptor says `live: false` |
| 8 | `send` / `send_bag`: stored, not picked: `--wait <s>` ended, or no answer to its ask within `wake_secs` with the mailbox present (flows A, A1, A2; *Wait for pickup*) |
| 7 | a dropped connection, every kind |
| 0, 2, 3, 5 | unchanged, less the cases moved to 9 |

| Exit | `send` / `send_bag` to `addr+inst` |
|---|---|
| 0 | stored: mailbox present, or `admit` / `register` / a follow answering the ask; with `--wait`, picked |
| 3 | no such instance: `reject` of its ask, or a `reject` landing while S waits on its pickup; nothing stored for the sender (a stored message goes with the mailbox) |
| 8 | stored, not picked: `--wait <s>` ended, or no answer within `wake_secs` with the mailbox present |
| 6 | no answer within `wake_secs` and no mailbox at that moment; `retry-after=<s>` on stderr, `s` = `live_retry_secs`. Nothing stored |

`retry-after` is carried by exactly these exit 6 cases: flows A2, C2 and C. Other exit 6 cases carry none.

| Reserved msgid | |
|---|---|
| `expired-*`, `wakes-*`, `head-*` | H's own; a client's `--id` with one of these prefixes: exit 2 |
| `event-*` | a topic publication's msgid (*Event ids*); a client's `--id event-*` on any send other than to a topic: exit 2 |

| Descriptor | |
|---|---|
| `live` | optional boolean in the `register_description` descriptor; `false`: every `--live` and `chat` to the identity exits 9. Validated by `hub-descriptor`; absent = live supported |

## Wait for pickup

```sh
ssh hub send <identity or instance> --wait <s> …      # also send_bag
```

| Item | |
|---|---|
| `--wait <s>` | on `send` and `send_bag` to an identity or an instance: after storing, H holds S up to `<s>` seconds (decimal, 0.001–600) until the message is picked, else answers. To a topic, an alias, a subscription or a publisher: exit 2. With `--live`: exit 2. Out of range or malformed: exit 2 |
| picked | the first time a read by the mailbox owner delivers the message's body or payload: a `part=msg` or `part=body` chunk, `--peek` (header + body), `--peek:full`, `--peek:rfc5322`, `--peek:file`, `--peek:bag`, `recv_bag`. Not `--peek:header`, not the ack |
| state | in H's memory, tied to the connected S; gone when S disconnects. The message stays stored either way, unless a `reject` removes its mailbox: S exits 3 |
| waiters | one per message: one sender per send. A wait ends on pickup by the mailbox owner, or with exit 3 on a `reject` |
| order | the waiter is registered before the message is linked into the mailbox, so a pickup cannot be missed: a print between the link and S's wait counts as picked |
| a `dup` | `--wait` on a send whose msgid the mailbox holds already: `dup`, exit 0, no wait |
| without `--wait` | unchanged: exit 0 once stored |

| Exit, stdout | `--wait <s>` |
|---|---|
| 0, `picked` | the owner was shown the message within `<s>` |
| 8, `stored, not picked within <s>` | the message stays stored |
| 3 | a `reject` took the mailbox, and the message with it |
| any other | as without `--wait`: nothing is waited for |
| stdout | `picked`, `stored, not picked within <s>`, `stored, not picked: no answer within <wake_secs> s` (the ask had no answer, its mailbox there), or `dup` |

## Durability

```sh
ssh hub send <target> --durable …      # also send_bag
```

| Item | |
|---|---|
| exit 0 | stored: the message survives a process crash. After an OS crash or power loss the last few seconds of writes can be lost |
| `--durable` | on `send` and `send_bag`: before H answers exit 0, it fsyncs the message file and its mailbox directory, and `inbox/` too when the send created the mailbox directory. The message then survives an OS crash or power loss |
| `--durable`, a held send | the fsync is done when the message is stored, after `admit` |
| `--durable` with `--live` or `chat` | exit 2 |
| `--durable` to a topic or an alias | accepted and ignored; stderr says `durable: ignored`. Support needs a hub inbox (an input queue), and for an alias `HUB_DURABLE=1` passed to its backend (not built yet) |
| no fsync | without `--durable`, nothing on the send or publish path: the message file, the stage directory, new instance mailbox directories, the topic record |
| expiry notices | the one ordering kept: per sender per sweep, the notice JSON, `.expired/` and the mailbox are fsynced before the expired messages are deleted |
| the record | temp file, rename: an atomic replace for a process crash, no fsync. An OS crash can roll it back; the boot id in it makes the next start a new session (*Event ids*) |

## Fan-out

| Before the topic lock | |
|---|---|
| 0 | with `--id event-…`, a `dup` or `stale` answers before the body is staged, nothing stored. The body goes in one pass straight into the stage file (`inbox/.stage.*`), no fsync (*Durability*). A failure: exit 1, nothing assigned |

| Step, under the topic lock | |
|---|---|
| 1 | compute the next `(session, seq)` from the record and the `--id` (*Event ids*); `dup` / `stale` answer here, the stage removed, nothing written |
| 2 | write the record `<session> <seq> <boot_id>`: temp file, rename, no fsync. A failure: H removes the stage, leaves the record file and the in-memory value unchanged, exit 1; nothing is linked |
| 3 | per subscription mailbox, under its mailbox lock: hard-link the stage's message file as `event-<session>-<seq>.msg` (payload included). One link per subscription, one inode per event. A name that exists already in that mailbox counts as a failed link: the seq is used, that reader sees a gap |
| 4 | remove the stage; set `done` to this `(session, seq)`; release the topic's lock |

| Rule | |
|---|---|
| event msgid | the same in every mailbox: the file's name; no per-mailbox number |
| a repeated msgid | within the record's session: the record's own msgid is `dup`, an older seq `stale`, neither delivered. A repeat after a new session is a new publication: the publisher's input |
| reading by msgid | every read that names a msgid (`recv --id <msgid>`, `recv_bag --id <msgid>`, `recv_ack <msgid>`) acts on that one delivery, as in any mailbox |
| identity and instance mailboxes | keep `<msgid>.msg` |
| order | the topic's lock is held across seq assignment and fan-out, so deliveries appear in every mailbox in seq order; landing out of order would show readers false gaps |
| cost | the topic lock serialises publications and `sub` renewals on one topic, across the stamp, the record write and the links; the body's store is outside it |
| listing order | every mailbox lists in arrival order: the message file's mtime, which H stamps as the message lands, with its link, under the mailbox's arrival lock (a subscription: also under the topic's lock, on the one file the links share), rising strictly while H runs; ties by msgid. A listing reads the mailbox under the same lock, so it holds every message that arrived before the last one it holds. In a subscription that is seq order within a session, and publishers that interleave sessions on one topic list as they published (their input). A message that lands after a reader's read sorts after everything it read |
| a clock step back | the one exception: a wall clock set back across an H restart lets what lands next sort before older messages. A reader may see a session change it would not have seen (one extra rebuild), and a `--before…` range may take such a message unseen. Within one run H's stamps keep rising |
| a subscription created during a fan-out | `subPlace` runs under the topic's lock, so it gets publications from the next one; its position notice gives `done` |
| the mailbox lock | kept, as `<mbox>/.lock`: `deliverToSub` and `dropSubBox` hold it. Only the numbering goes |

| The hub or the publishing session dies part way | The subscriptions |
|---|---|
| before the links (steps 0–2) | none has it |
| during the links (step 3) | those it linked have it; the rest have nothing |
| either way | after a process restart the topic continues at the record's seq + 1: a seq written at step 2 and not linked everywhere is a gap. After an OS boot the topic starts a new session: readers see a session change |

## Message file

Every stored message is one file, `<msgid>.msg` (in a subscription, `event-<session>-<seq>.msg`). H does not touch the payload: the file a sender streams and the tar a sender bags are stored exactly as received.

| Part, in file order | |
|---|---|
| 1. header block | the hub fields, the client fields, and `Payload: file <name>` or `Payload: bag` with `Msg: <path>` (a bag with `--msg`); no `Payload` line without a payload. Ends in a blank line. Written first, once its client fields have been read: for `-m:rfc5322`, at the end of stdin's header block, which comes before any payload |
| 2. body | plain and `--file` messages: directly after the header block |
| 3. payload | streamed from stdin in one pass: the file for `--file`, the tar exactly as received for `send_bag` (a folder travels as a bag), or nothing. A bag's body is not copied: it is the `Msg:` member's data inside the tar |
| 4. trailer | fixed, 212 bytes, written last (below) |

```
Header-Size: <20>
Message-Offset: <20>
Message-Size: <20>
Total-Size: <20>
Sha256: <64>
```

| Trailer | |
|---|---|
| lines | five, in this order, each `<name>: <value>\n`: one space after the colon. `<20>` is a 20-digit zero-padded decimal |
| bytes | `Header-Size` 34, `Message-Offset` 37, `Message-Size` 35, `Total-Size` 33, `Sha256` 73: 212 |
| `Header-Size` | H: the header block, its blank line included |
| `Message-Offset`, `Message-Size` | where the body is, and its bytes |
| `Total-Size` | the whole file, trailer included |
| `Sha256` | 64 zeros, reserved: never computed, never checked |

H = `Header-Size`, T = 212.

| # | Case | Header-Size | Message-Offset | Message-Size | Total-Size | Header has |
|---|---|---|---|---|---|---|
| 1 | empty message | H | H | 0 | H+T | – |
| 2 | message, body b | H | H | b | H+b+T | – |
| 4 | `--file`, empty body, file F | H | H | 0 | H+F+T | `Payload: file <name>` |
| 5 | bag of size B, `--msg` member of size m at tar offset k | H | H+k | m | H+B+T | `Payload: bag`, `Msg: <path>` |
| 6 | bag, `--msg` member empty | H | H+k | 0 | H+B+T | `Payload: bag`, `Msg: <path>` |
| 7 | bag of size B, no `--msg` | H | H | 0 | H+B+T | `Payload: bag` |

| Reader | |
|---|---|
| body | `[Message-Offset, Message-Offset + Message-Size)` |
| payload | only when the header has `Payload:`. `file`: `[Header-Size + Message-Size, Total-Size − T)`; `bag`: `[Header-Size, Total-Size − T)` |
| I/O | a file under about 4 KiB: open, fstat (for the `Total-Size` check), one read. Otherwise one `pread` of the trailer, then one `pread` of header and body (a bag: of the header, then of its body range) |
| writing | one pass, in file order: no placeholder, no rewrite in place |

| Read | |
|---|---|
| header | the stored header block: hub fields and client fields; never the trailer (*Reading*). Always valid UTF-8: the hub refuses a client value that is not (exit 2, nothing stored), and its own fields are its own |
| header lines | each ends at the byte `\n`: a reader splits the block there, never with a Unicode line splitter (Python `str.splitlines`, a split on U+2028, …), since a value may hold U+0085, U+2028, U+2029, `\v`, `\f` or `\x1c`-`\x1e` |
| body | the body range: not the bytes on disk, which also hold the payload and the trailer |
| payload | the payload range, as stored: `--peek:file`, `--peek:bag`, `recv_bag`, and the `part=payload` chunks of a full read and `--peek:full` |
| `--peek:bag <path>` | scans the bag payload's tar headers for that member and streams its bytes |
| fan-out | one hard link of the message file per subscription (*Fan-out* step 3) |
| removal, expiry | remove the one file |

| `-m:rfc5322` on a send | |
|---|---|
| the client header block | H parses it into the stored header block (or a live request's hub header block). After the blank line, every reader gets the body only. So no field says which form was sent |

| Content-read check | |
|---|---|
| which reads | every read that outputs body or payload bytes: every `recv` content but `--peek:header`, and `recv_bag`. It compares `Total-Size` with the file's size (stat); a trailer that does not parse is damaged too |
| skipped | scans that only count or name files: mail counts, `instances`, the wake render, reaper sweeps |
| a mismatch, an unparsable trailer | a torn or truncated file: H moves it to `<mailbox>/.damaged/<name>`, journal line `damaged <mailbox> <msgid> stat=<n> total=<n>`. `--peek:rfc5322`, `--peek:file`, `--peek:bag`, `recv_bag`: exit 1, `damaged`. A chunked stream skips the entry, logs it once, and goes on |
| who moves it | H, on the content read that finds the mismatch |
| a subscription | the move takes only that mailbox's link; each mailbox finds damage on its own read |
| `.damaged/` | the reaper deletes its files after `MSG_TTL` |
| reporting | visible only in the journal; an expiry-notice `[damaged]` row is not built yet |

### Bags

| Check at `send_bag` | Kept: completeness and the hub's disk |
|---|---|
| the tar end marker | missing, or the tar cut: exit 4, nothing stored |
| not a tar | exit 2 |
| an empty bag | exit 2 |
| the size cap | 256 MB of stored bytes; over it: exit 2 |
| `--msg` | names no regular member, or a sparse one: exit 2 |

| Members | |
|---|---|
| matching a name | for `--msg` and `recv --peek:bag <path>`: a leading `./` is ignored; a name that occurs twice: the last occurrence wins |
| `recv --peek:bag <path>` | a regular member: streamed. A sparse or non-regular member: exit 2 |
| not checked | member names, member types, the member count, the path depth. Nothing in the payload is rewritten; directories travel inside the tar untouched |

## Reading

```
recv [<mailbox>] [--peek[:<content>]] [--limit <bytes>] [--header:stderr]
recv [<mailbox>] --id <id> [--id <id>…] [--peek[:<content>]] [--header:stderr]
recv [<mailbox>] --follow [--peek[:<content>]] [--header:stderr]
recv_bag [<mailbox>] [--id <id>] [--peek] [--header:stderr]
recv_ack [<mailbox>] <id>…
recv_ack [<mailbox>] -m:ids          (ids on stdin, one per line, at most 1024)
recv_ack [<mailbox>] --before <msgid>
recv_ack [<mailbox>] --before-including <msgid>
```

| Item | |
|---|---|
| `<mailbox>` | an argument containing `@`; a msgid never contains one, which is how the two are told apart. Without it, the session's own mailbox |
| a listing | `recv` without `--id` and `--follow`: what is waiting, then the end |
| `--id` | the named messages, in the order given |
| `--follow` | the backlog, then each message as it lands, until the session ends, the mailbox is gone for it, or a newer follow takes over (*Takeover*) |
| `--id` with `--follow` | exit 2 |
| order | the wake notice, expiry notices in byte order of `from`, messages in arrival order (*Fan-out*, listing order), then a subscription's position notice |

### The stream

stdout is an HTTP/1.1 chunked stream (RFC 9112 §7.1). Every chunk carries the extensions `;id=<msgid>;part=<part>`, except `part=info`, which has no `id=`. Without `--header:stderr`:

```
55;id=note-1;part=msg
From: commhub@home.ssh
Date: 2026-11-08T10:00:02Z
Serial: 1
Subject: note

hello

69;id=report-7;part=msg
From: main/ws-a@acme.ssh
Date: 2026-11-08T10:00:03Z
Serial: 1
Payload: file report.pdf
Subject: report


10000;id=report-7;part=payload
<65536 bytes>
2a31;id=report-7;part=payload
<10801 bytes>
0
```

Each size line and each chunk's data end in CRLF, shown here as line breaks.

| Chunk | |
|---|---|
| `part=msg` | starts a message: the header block (hub fields, then client fields), a blank line, then the body |
| `part=payload` | after the msg chunk, in order, one or more, when the content includes the payload: the file's bytes or the tar as stored |
| the next message | starts at the next `part=msg` chunk |
| `0` | the final chunk: a listing, or `--id`, is complete. `0;limit`: `--limit` stopped the listing, more is waiting |
| final chunk bytes | as RFC 9112: `0[;ext]\r\n`, an empty trailer section, then `\r\n`: `0\r\n\r\n`, `0;limit\r\n\r\n`, `0;end=replaced;by=<conn>.<session>\r\n\r\n`, `0;end=gone\r\n\r\n` |
| exit with a final chunk | a listing or follow that ends with its final chunk exits 0, `end=replaced` and `end=gone` included (`--id` with none of its ids found: 3, *Exits*) |
| a follow | sends no final chunk while it runs; replaced by a newer follower, it ends with `0;end=replaced;by=<conn>.<session>`, the follow id of the follow that took over (*Takeover*); its mailbox gone for it, with `0;end=gone` |
| `part=live` | a follow of the reader's own mailbox: one chunk `;id=<msgid>;part=live` per LIVE announcement, data the `LIVE` line plus `\n`, between messages (*LIVE*) |
| `part=info` | a hub status line with no msgid: one chunk `;part=info`, no `id=`, data the line plus `\n`. A line that repeats what a notice or a final chunk says is dropped instead; none is written today: an empty listing is the final chunk alone, and a follow's two plain lines (`-- <mailbox> is followed elsewhere now (conn <conn>) --`, `-- <mailbox> is gone --`) are on stderr, beside its final chunk |
| a cut stream | has no final chunk |
| an empty listing | the final chunk alone |
| notices | `expired-*`, `wakes-*`, `head-*`: a `part=msg` chunk, as any message |
| `part=missing` | `--id` only: an id the mailbox does not hold gets one chunk `;id=<id>;part=missing`, data `Missing: <id>\n`, in its place in the order given. Exit 3 only if none was found |

| Rule | |
|---|---|
| every byte | everything a `recv` mode writes to stdout is a chunk, from the first byte: the notices of a connect (`notice_probe`, the wake notice, expiry notices, the position notice) are `part=msg` chunks. stderr carries hub error lines and the `--header:stderr` blocks |
| trust | the stream is length-framed, so a body cannot forge an entry. A reader acts on the stream as read, notices included; there is no confirm read |

### Takeover

| Rule | |
|---|---|
| one follower per mailbox | a mailbox has at most one follow; a new `recv <mailbox> --follow` takes over from the one there |
| who gets `end=replaced` | only the follow that held the mailbox when another took over. Nothing else ever does: not a connection that follows nothing, and none of the identity's other live connections (sends, `accept`, `chat`, follows of its other mailboxes) |
| under the mailbox's follow lock | the new follow registers itself as the follower and marks the previous one replaced, then the lock is released. Nothing is written and nothing is closed under it |
| after the lock | the previous follow writes its final chunk `0;end=replaced;by=<conn>.<session>` (the new follow's follow id), exits 0, and its session closes. It checks before each entry, so a takeover in the middle of a pass ends it there: what that pass flushed is consumed as usual (a full follow), nothing after it is streamed. A previous follow blocked writing to a client that stopped reading ends once that write returns; the new follow does not wait for it |
| live offers | a follow that ends, replaced or not, hands back the `part=live` offers it announced and nobody took: the next follow of the mailbox announces them. One taken meanwhile is not (`accept` takes an exchange once) |
| follow id | `<conn>.<session>`: the hub numbers every ssh connection and logs it (`conn <id>: accepted …`), and every session channel on it; a takeover is logged as `follow <mailbox> conn=<new> replaced conn=<old>`, both follow ids. Follows multiplexed on one connection (ControlMaster) differ in the session part |
| a reader on `end=replaced` | means a second instance of the same service is running: a deployment error. Who does what: |
| the deployer | runs exactly one instance per service |
| the hub | allows one follower: ends the old follow with `0;end=replaced;by=<id>` and logs both follow ids (rows above) |
| the consumer | stops following, sends the acks already due, logs at error level `superseded by=<id>: another instance is running`, and exits **3**; it does not follow again by itself |
| the supervisor | keeps its normal restart policy; nothing is changed for this. A policy that restarts a failed exit (systemd `Restart=always`, docker `unless-stopped`) restarts the exited instance, which takes the follow back, so the two instances alternate: the visible symptom of the duplicate deployment |

### Consumption

| Read | |
|---|---|
| consuming | a full read: plain `recv`, `--follow` and `--id`, each without `--peek`; `recv_bag` without `--peek` |
| consumed | once the message has been streamed whole: its msg chunk and all its payload chunks |
| when | a consuming read deletes once the chunks are written to the connection, not once the client has received them: a dropped connection loses them (measured: up to about 0.5 MiB in flight). So does a reader that stops reading while its ssh client stays up: OpenSSH discards what it can no longer write to a closed stdout and keeps reading, so the read runs to its end and consumes everything. A reader that must not lose mail reads with `--peek` and acks with `recv_ack` |
| `--peek` | never consumes. A `--peek` follow streams each message once |
| `wakes-*`, `head-*` | never consumed |
| `expired-*` | consumed by a full read: after it is written, H takes the lock and reads it again; removed only if the msgid is unchanged |

### Content

| Content | Gives |
|---|---|
| a full read | header + body + payload |
| `--peek` | header + body |
| `--peek:header` | header only |
| `--peek:full` | header + body + payload |
| `--peek:rfc5322` | one message, unframed: header block, blank line, body. Requires a single `--id`, else exit 2 |
| `--peek:file` | requires exactly one `--id`, else exit 2: the file payload's bytes, raw; exit 2 on a bag or no payload |
| `--peek:bag [<path>]` | requires exactly one `--id`, else exit 2: the stored tar, raw; with `<path>`, that one member's bytes, raw, found from the tar headers, nothing extracted to disk; exit 2 on a file payload; exit 3 if the member is not found |
| a bag's body | `recv --id <id> --peek`: its `--msg` member's data |

| Notices by `--id` | |
|---|---|
| `expired-*` | the current msgid only; an older one: `part=missing` |
| `wakes-<any>` | the current rendering, with the current msgid |
| `head-*` | the topic's current `head-` msgid: the notice chunk; any other `head-`: `part=missing` |
| `--peek:file`, `--peek:bag`, `recv_bag` | exit 2: a notice has no payload |

| Members, `--peek:bag <path>` | |
|---|---|
| matching | a leading `./` is ignored; a name that occurs twice: the last occurrence wins |
| a sparse or non-regular member | exit 2 |

### `--header:stderr`

Moves the header off stdout, in every mode except `--peek:rfc5322`, where it gives exit 2.

| Where | |
|---|---|
| stderr, per message | `Header: <msgid>`, the header lines, then a blank line |
| stdout, chunked modes, per message | first a `part=id` chunk whose data is exactly `Header: <msgid>\n`, the same line as on stderr. It is never empty, since a 0-size chunk is the end marker, and it tells the reader the message has arrived. Then `part=body` and `part=payload` chunks, as the content gives them. With `--peek:header`, the `part=id` chunk only |
| stdout, raw outputs (`--peek:file`, `--peek:bag`, `recv_bag`) | the raw bytes; stderr gets that message's header block |
| order, chunked modes | H writes the `part=id` chunk to stdout first, then the message's header block to stderr |
| order, raw outputs | not chunked, so the pairing rule does not apply: H writes the header block to stderr first, then the raw bytes to stdout, so a script can read the header before the content |
| hub error lines | `dispatch: …` lines may come on stderr between blocks |

| Reader rule | |
|---|---|
| pairing, chunked modes | the reader takes a `part=id` chunk's line exactly as it is and searches stderr for that same line. The header is everything after it, up to the next blank line. It never pairs by position |
| waiting | the block may not have arrived yet: the reader waits on stderr for it |
| hub error lines | skipped: they never equal a `Header:` line |

### `--limit <bytes>`

| Item | |
|---|---|
| stops | at the first message boundary at or past `<bytes>` of output; the final chunk is `0;limit` |
| a message larger than the limit | still sent whole |
| consumption | only messages streamed whole are consumed |

### `recv_bag`

| Item | |
|---|---|
| stdout | the stored tar, raw |
| consumes | without `--peek` |
| without `--id` | the first message in listing order |
| exit 2, nothing consumed | that message's payload is not a bag, or it has a body outside the bag. A bag's body is its `--msg` member inside the tar, or empty |
| `--header:stderr` | the header block on stderr: a consuming `recv_bag` loses nothing |

### `recv_ack`

| Item | |
|---|---|
| flags | `-m:ids`, `--before`, `--before-including`; any other flag, or a second argument containing `@`, gives exit 2 |
| `-m:ids` | the ids on stdin, one per line, at most 1024 |
| `--before <msgid>` | acks the messages that arrived before `<msgid>` (arrival order, *Fan-out*), not `<msgid>` itself. Every mailbox alike: own, instance, subscription |
| `--before-including <msgid>` | the same, `<msgid>` included |
| `--before…`: `<msgid>` absent | exit 3, nothing acked |
| `--before…`: notices | never in the range: `wakes-*`, `expired-*` and `head-*` keep their places in a listing and are never acked by it; as `<msgid>`, exit 2. An `expired-*` is acked by id only |
| `--before…`: scope | the order is read once, at the call. A message that lands after the reader's read, during the call included, sorts after `<msgid>` and is not acked (a clock step back aside, *Fan-out*) |
| `--before…`: retry | not idempotent: a retry after an answer lost on the way finds `<msgid>` gone, exit 3. The reader then acks the ids it handled by id |
| `--before…` with ids or `-m:ids` | exit 2 |
| `--before…` stdout | one line per entry acked, in order: `acked <id>`; `gone <id>` for one another ack removed meanwhile. Nothing before `<msgid>`: no line, exit 0 |
| locking, wake notice | as by id: the mailbox's own locking; one wake-notice nudge per call |
| stdout | one line per id: `acked <id>`; `gone <id>`, already removed, which counts as done; `refused <id>`, a notice that cannot be acked (`wakes-`, `head-`) |
| exit | 0 when every id was handled; 2 when one is malformed |
| `expired-*` | cleared only if the msgid is unchanged. An older msgid, or a sender with no notice: `gone`. A `notice_probe`'s msgid: `acked`, nothing changed |

### Exits

| Exit | Reading |
|---|---|
| 0 | the read completed: every listing and follow that ends with its final chunk (`0`, `0;limit`, `0;end=replaced;by=<conn>.<session>`, `0;end=gone`); `recv_ack`: every id handled, or everything before the `--before…` msgid |
| 1 | a damaged message file on a raw output (`--peek:rfc5322`, `--peek:file`, `--peek:bag`, `recv_bag`): `damaged`; a chunked stream skips it (*Content-read check*) |
| 2 | `--id` with `--follow`; `--peek:rfc5322` without a single `--id`, or with `--header:stderr`; `--peek:file` or `--peek:bag` without exactly one `--id`; `--peek:file` on a bag or no payload; a payload read of a notice; `--peek:bag` on a file payload or a sparse or non-regular member; `recv_bag` on a payload that is not a bag or a body outside the bag; `recv_ack` with a flag, a second mailbox or a malformed id, `--before…` with ids or `-m:ids`, a notice's msgid (`wakes-*`, `expired-*`, `head-*`) to `--before…` |
| 3 | `--id`: none of the ids found; `--peek:bag <path>`: no such member; `recv_bag`: no such message, or without `--id` an empty mailbox; `recv_ack --before…`: no such msgid |

## Event ids

| Item | |
|---|---|
| event msgid | `event-<session>-<seq>`: `<session>` 16 lower-case hex digits, `<seq>` decimal from 1, no leading zeros, at most int64 max by parse; `0`, a leading zero or anything else: exit 2. A topic publication's msgid is its identity |
| `--id` on a publish | every publish to a topic: `send <topic>` (each body form) and `send_bag <topic>`. `--id event-<session>-<seq>`: the publisher supplies its own sequence; no `--id`: H assigns the next one. Any other `--id`: exit 2 |
| `--id event-*` elsewhere | any send other than to a topic: exit 2 (reserved, *Exits*) |
| body | untouched: the msgid is an argument, not a field |
| record | `topics/<t>/record`: one line `<16hex> <seq> <boot_id>\n`, the boot id of the hub that wrote it. No message data, never hard-linked |
| write | step 2 of *Fan-out*, at each publication: after the stamp, before the links: temp file, rename, no fsync. A process crash keeps it: the page cache survives |
| read | once per topic, at its first publication or the first connect to one of its subscriptions after H starts, whichever comes first. Missing or malformed → `none`; a malformed one is logged, then overwritten at the next publication |
| `done` | after H starts, `done` = the record's `(session, seq)`; then set at the end of each fan-out (step 4) |
| lifetime | created at the topic's first publication; removed with the topic (`hub-topic rm`) |
| H restart, same boot | the record's boot id equals the kernel's: the topic keeps its session and continues at the record's seq + 1 |
| H start after an OS boot | the boot ids differ: H mints a new session for that topic at its next hub-assigned publication (no `--id`). A publisher's own `--id event-<s>-<p>` is honoured after a reboot as at any time; a publisher resending an old seq is its own input. An OS crash may have rolled the record back; the new session keeps a seq from being used twice, and readers see one session change |
| int64 max | when H's own stamping (no `--id`) would pass int64 max, it mints a new session and starts at 1 |

| Publish | Record before | H does | Exit, stdout |
|---|---|---|---|
| no `--id` | `none` | mints a 16-hex session; seq 1 | 0, `published …` |
| no `--id` | `(s, q)` | `(s, q+1)` | 0, `published …` |
| `event-<i>-<p>` | `none` | `(i, p)` | 0, `published …` |
| `event-<i>-<p>`, `i ≠ s` | `(s, q)` | `(i, p)` | 0, `published …` |
| `event-<s>-<q+1>` | `(s, q)` | `(s, q+1)` | 0, `published …` |
| `event-<s>-<p>`, `p > q+1` | `(s, q)` | `(s, p)` | 0, `published …` |
| `event-<s>-<q>` | `(s, q)` | nothing delivered | 0, `dup` |
| `event-<s>-<p>`, `p < q` | `(s, q)` | nothing delivered | 0, `stale: last event-<s>-<q>` |
| any other `--id` | any | nothing delivered | 2 |
| a store fails (a hub I/O error) | any | record unchanged | 1, retryable |

| Delivery | |
|---|---|
| msgid | `event-<session>-<seq>`: the publication's session (*Terms*) and its seq within it |
| identity | the msgid alone; readers parse session and seq from it. No header carries them |
| file | `event-<session>-<seq>.msg`, payload included, hard-linked into every subscription |
| every subscription | the same msgid: seq belongs to the publication, not to a recipient |
| a gap marker (`Gap`) | a publication like any other: it takes a seq |
| an expiry notice or a `notice_probe` in a subscription | carries nothing about the session |
| H and gaps | H never claims a gap and sends no gap marker of its own. A topic not yet published to reports `none`, and H treats its own `none` as no claim |

| Reader, a delivery | |
|---|---|
| same session as its last, seq ≤ last | a redelivery (a reconnect, or a crash before the ack): ack and skip |
| same session, seq = last + 1 | the next one |
| same session, seq > last + 1 | a gap |
| another session | a gap, unless the reader's last is `none`; the reader tells its own `none` from H's `none` and decides |
| a `Gap` field | a gap |
| a `head-` notice beyond its last delivery | a gap |

| On a `head-<s>-<q>` beyond its last delivery | |
|---|---|
| beyond | same session as the reader's last with `q` > last; or another session: a session change, a gap unless the reader's last is `none` |
| the reader | either handles the gap (rebuild or rescan) and then sets its last to `<s>-<q>`, or ignores the head and waits for the next delivery, which then shows the jump. The reader's choice; the hub has no part in it |
| a mailbox that missed a publication (flow D) | its reader sees `n-1`, then `n+1` |

| Publishers | |
|---|---|
| one publisher with `--id event-…` | its `(session, seq)` is the record, written as given at each publication, kept across an H restart |
| several publishers on one topic | pass no `--id`: H numbers them in one sequence. Each may publish `Gap`. One passing `--id event-…` among others: their coordination, outside H |
| a publisher repeating an old session's msgid after starting a new one | a new publication in a new session: the publisher's input, not a hub matter. Readers see a session change |
| a publisher keeping one connection for its events | not supported |

### Position notice

```
37;id=head-3f9a2c1e8b7d6a54-41;part=msg
From: topic+builds@acme.ssh
Date: 2026-11-08T10:00:02Z


```

| Item | |
|---|---|
| msgid | `head-<session>-<seq>` (`head-none` for `none`): the value itself, reserved to H |
| identification | no `Subject`, no header: the msgid carries the topic's `done`, read before the backlog scan, the head of completed fan-outs |
| where | after the backlog, on `recv --follow <sub>` and on `recv <sub>`, only when the mailbox does not hold that publication |
| mid-fan-out | `done` is set only when a fan-out ends, so a reader connecting during one never sees a head its mailbox lacks |
| use | only to detect a gap after the backlog: a head beyond the last delivery read. Never a position to ack up to |
| `recv` | a `part=msg` chunk, never consumed; `recv_ack head-*`: `refused` |
| `recv --id head-…` | the notice chunk for the topic's current `head-` msgid; any other `head-`: `part=missing` |
| effect | none on the record: a `none` is never a gap claim |

Removed: the per-subscription id mint (`subEnsure`), the per-subscription counter and number entries (`inbox/<sub>/.seq/.id`, `.last`, `<msgid>`; `subNumber`, `subScan`).

### Upgrade

| Step | |
|---|---|
| 1 | stop everything: the hub and its consumers |
| 2 | delete every mailbox (`inbox/*`: identity, instance and subscription mailboxes, their `.expired`), the whole subscription index (every `topics/*/subs/*` and every `subs/*` link) and every `topics/*/record`. Consumers re-subscribe at start, as they do today |
| 3 | install the new hub |
| 4 | start everything |

## Removed from H

| Item | Today |
|---|---|
| WAKE exchange | `wakeForSend`, `wake`, `wakeCall` (`go/hub/instances.go`), the wake table (`go/hub/hub.go`) |
| `wake_secs` for wakes | stays only as the ask entry timeout (flow A2) |
| exit 8 as not woken | `notWoken` (`go/hub/instances.go`), the `(not woken)` lines (`go/hub/spool.go`) |
| per-follow line list | `follower.events`, `announce` (`go/hub/follow.go`) |
| per-subscription numbering | `go/hub/continuity.go`, less the mailbox lock |
| instance mailbox made by a store | the store's `MkdirAll` for instances (`go/hub/spool.go`); an instance store stages in `inbox/.stage.*` and links |
| doorbell | `ringFor`, `ringOne` (`go/hub/chat.go`) and their `-- new mail … --` / `-- new event … --` lines |
| `who` | `doWho` (`go/hub/chat.go`), the `who` verb (`go/hub/session.go`) |
| chat relay | the line relay in `go/hub/chat.go`: chat with no peer, `@identity` addressing, lines spooled to a mailbox or published to a topic; `chat` is a LIVE kind |
| `sub --wake`, `sub --live` | the `wake` mode and the no-op `--live` (`go/hub/topic.go:365`): `sub` has one behaviour; `--live` and `--wake` exit 2 |
| record reservation | `recordBlock`, `reservation`, the `exact` field, the record write at shutdown (`go/hub/event.go`) |
| `mode=` | the field in `topics/<t>/subs/<sub>`; the upgrade removes the old records |
| `Bag`, `File` fields | in the stored header block; `Payload` replaces them |
| `Form`, `File` in the live accept header block | `Payload: file <name>` replaces `Form: file` + `File`; the LIVE line marks a chat |
| the attachment folder | `<msgid>/` beside the `.msg`, for `--file` and bags; the payload is inside the message file |
| bag extraction | the extracted tree, `placeTree`, `linkTree`, `dropTree` (`go/hub/topic.go`), `extractFile`, `syncTree`, the member count, type and depth checks (`go/hub/bag.go`), `validBagPath` (`go/hub/ident.go`) |
| the listing format | `go/hub/recv.go:120`: the `=== msgid: … === end` blocks with their `file: … fetch:` and `bag: … fetch:` lines; the chunked stream replaces them (*Reading*) |
| read forms | `recv --file`, `recv -m:rfc5322` (now `--peek:rfc5322`), `--keep`, `recv --ack` (now `recv_ack`), `recv_bag <msgid>` (now `recv_bag --id`) and its `--msg` (now `recv --id <id> --peek`, which gives the body). Sending keeps `-m:rfc5322`, `--file` and `send_bag` |
| fsync on the send and publish path | the `Sync` and `syncPath` calls of a store, a stage, a bag, an instance mailbox (`go/hub/spool.go`, `bag.go`, `admission.go`, `instances.go`); kept for expiry notices and `--durable` |

## Tests

| Required | |
|---|---|
| `lib/help.json`, `lib/help.txt` | build item: the `recv`, `recv_bag` and `recv_ack` usages of *Reading*; `recv_ack` a verb of its own |
| `test/full-disk.sh` | still exit 1 on a full disk for every send form, with no fsync on the path |

| Suite | As built (`feat/wake-live-event-id`, read side included), measured on a 4-vCPU VM, 2026-11-08 |
|---|---|
| `go test ./...` | 196 tests (hub 165, expiry 24, descriptor 7; 3 skipped in this environment), green; among them the message-file layout cases 1-7, the trailer, damaged files, bag members, event ids and the record, `--wait` (each pickup, `--peek:header` none), `--durable` (the fsynced paths watched: none without it), raw and chunked LIVE kinds, `chat`, D24's EOFs, the ask cap, idle flood buckets; the read stream: a valid chunk stream from a follow's first byte, the listing order, `--id` with `part=missing`, `--limit`, `--header:stderr` order on both outputs, consumption per flush and a cut past the first batch, a short read cutting the stream, `recv_ack`, `recv_ack --before` and `--before-including` (a subscription across interleaved sessions, an own mailbox, an absent msgid, notices never in the range, an arrival during the call, an arrival after the read with a lower msgid in an own, an instance's and a subscription's mailbox, combined with ids), arrival order with msgid ties, a header value that is not valid UTF-8, the removed forms |
| `test/dev-hub.sh` | 225 checks through real ssh, green |
| `test/full-disk.sh` | green: 256 sends on a 2 MB tmpfs, 76 meet exit 1, never `dup`, no fsync |

`test/bench.sh` (`go/cmd/hub-bench`): the daemon on loopback ssh, one connection per identity, each operation one
session; latency from the session's open to its exit status. 4 vCPUs, other agents' load on the machine:
load average 0.54 before, 1.91 after the new hub's run; 1.94 after the old one's. The previous hub is `48c91e6`
(`feat/publish-all-who-accept`): it has no `--durable`, `chat` kind, wake notice or chunked read stream; its every
send fsyncs; its raw read is `recv -m:rfc5322 <id> --keep`, its follow `--follow --keep`.

| Operation | n | New: median / p95 ms | New: throughput | Old: median / p95 ms | Old: throughput |
|---|---|---|---|---|---|
| `send -m` 1 KiB | 300 | 0.73 / 1.20 | 1103 ops/s | 58.98 / 143.06 | 14 ops/s |
| `send -m` 1 KiB `--durable` | 300 | 52.33 / 116.90 | 16 ops/s | – | – |
| `send --file` 1 MiB | 50 | 2.92 / 4.42 | 321 MiB/s | 122.03 / 281.84 | 7 MiB/s |
| `send --file` 64 MiB | 7 | 118.33 / 126.39 | 536 MiB/s | 323.02 / 360.72 | 199 MiB/s |
| `send_bag` 64 MiB | 7 | 116.19 / 137.58 | 529 MiB/s | 432.79 / 879.50 | 129 MiB/s |
| `send_bag` 252 MiB | 5 | 479.99 / 516.42 | 518 MiB/s | 1181.61 / 1401.42 | 209 MiB/s |
| publish `-m` 1 KiB, 1 subscription | 300 | 0.96 / 1.38 | 971 ops/s | 67.64 / 145.97 | 12 ops/s |
| publish, 10 subscriptions | 300 | 1.36 / 1.78 | 687 ops/s | 101.31 / 210.41 | 9 ops/s |
| publish, 100 subscriptions | 300 | 5.60 / 6.18 | 172 ops/s | 158.38 / 302.11 | 6 ops/s |
| `recv --id <id> --peek:rfc5322` 1 KiB (old: `-m:rfc5322 <id> --keep`) | 300 | 0.43 / 0.67 | 2167 ops/s | 0.43 / 0.65 | 2171 ops/s |
| `recv --peek`, 100 × 1 KiB | 100 | 2.00 / 2.69 | 478 ops/s | 3.24 / 4.17 | 292 ops/s |
| `recv`, consuming, 100 × 1 KiB | 100 | 3.51 / 4.12 | 274 ops/s | – | – |
| `send --live --chunked` round trip | 300 | 1.23 / 1.74 | 772 ops/s | 1.22 / 1.79 | 766 ops/s |
| `chat` round trip (raw) | 300 | 1.35 / 1.84 | 695 ops/s | – | – |
| `recv_ack --before-including <last>`, 500-message backlog, one call | 20 | 7.18 / 7.58 | 137 ops/s | – | – |
| `recv_ack -m:ids`, the same 500, two calls of 250 | 20 | 7.64 / 8.32 | 130 ops/s | – | – |
| `recv_ack -m:ids`, the same 500, one call | 20 | 7.08 / 8.24 | 138 ops/s; the three `recv_ack` rows from their own run (`-only recv_ack`), load 2.00 before, 3.11 after: the ack form costs the same, the gap to two calls is one ssh session |
| wake-notice render, 1000 instances × 10 unread | 20 | 5.65 / 6.10 warm; 185.6 cold (summary from disk) | 179 ops/s | – | – |

Pre-deploy run: `test/bench.sh` with `OLD=48c91e6`, hub built from `ecd1479`, a 4-vCPU VM, 2026-11-08. Load average 2.21 before and 3.03 after the new hub's run; 3.03 before and 2.05 after the old one's. `/proc/pressure/io` `some` avg10: 4.15 before, 35.73 after, the fsyncs of the old hub and `--durable` included. Medians and p95s are in ms.

| Operation | n | New: median / p95 | New: throughput | Old: median / p95 | Old: throughput |
|---|---|---|---|---|---|
| `send -m` 1 KiB | 300 | 0.70 / 1.14 | 1253 ops/s | 58.47 / 117.21 | 16 ops/s |
| `send -m` 1 KiB `--durable` | 300 | 50.76 / 150.57 | 16 ops/s | – | – |
| `send --file` 1 MiB | 50 | 2.81 / 3.18 | 355 MiB/s | 109.24 / 284.27 | 7 MiB/s |
| `send --file` 64 MiB | 7 | 130.99 / 139.33 | 494 MiB/s | 339.86 / 385.45 | 185 MiB/s |
| `send_bag` 64 MiB | 7 | 127.02 / 140.96 | 501 MiB/s | 422.60 / 706.25 | 137 MiB/s |
| `send_bag` 252 MiB | 5 | 508.54 / 566.85 | 483 MiB/s | 1181.34 / 1333.61 | 208 MiB/s |
| publish `-m` 1 KiB, 1 subscription | 300 | 0.89 / 1.30 | 977 ops/s | 66.86 / 160.50 | 13 ops/s |
| publish, 10 subscriptions | 300 | 1.31 / 1.70 | 716 ops/s | 106.61 / 286.71 | 8 ops/s |
| publish, 100 subscriptions | 300 | 5.49 / 6.32 | 172 ops/s | 159.18 / 343.26 | 5 ops/s |
| `recv --id <id> --peek:rfc5322` 1 KiB | 300 | 0.43 / 0.65 | 2157 ops/s | 0.44 / 0.65 | 2103 ops/s |
| `recv --peek`, 100 × 1 KiB | 100 | 2.02 / 2.50 | 481 ops/s | 3.24 / 3.87 | 303 ops/s |
| `recv`, consuming, 100 × 1 KiB | 100 | 3.51 / 4.09 | 278 ops/s | – | – |
| `recv_ack --before-including`, 500-message backlog, one call | 20 | 7.83 / 8.55 | 126 ops/s | – | – |
| `recv_ack -m:ids`, the same 500, two calls | 20 | 7.55 / 7.96 | 133 ops/s | – | – |
| `recv_ack -m:ids`, the same 500, one call | 20 | 6.97 / 7.30 | 144 ops/s | – | – |
| `send --live --chunked` round trip | 300 | 1.21 / 1.86 | 743 ops/s | 1.15 / 1.59 | 817 ops/s |
| `chat` round trip | 300 | 1.32 / 1.80 | 724 ops/s | – | – |
| wake-notice render, 1000 instances × 10 unread | 20 | 2.97 / 3.85 warm; 180.8 cold | 331 ops/s | – | – |
