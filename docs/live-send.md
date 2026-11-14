# Live send

`send --live` streams a message to a recipient holding a live connection and streams the
recipient's reply back on the same session. `chat` opens streams both ways on the same exchange.
Nothing is stored.

## Invocation

```sh
ssh hub send <target> --live [--chunked] [--id <msgid>] [--subject <verb>] -m          < body
ssh hub send <target> --live [--chunked] [--id <msgid>] -m:rfc5322                     < body
ssh hub send <target> --live [--chunked] [--id <msgid>] [--subject <verb>] --file <name> < body
ssh hub chat <peer> [--chunked]
```

| Command | LIVE line | Streams | End of a direction |
|---|---|---|---|
| `send <peer> --live` | `… raw` | request: the sender's stdin raw until EOF, at most `FileMaxBytes` (64 MiB); reply: the recipient's accept stdin raw until EOF, no size cap | EOF; a dropped connection gives exit 7 |
| `send <peer> --live --chunked` | `… chunked` | both framed (*Framing*, done by the caller, not by the hub), request at most `FileMaxBytes` (64 MiB), reply with no size cap, the hub checks each direction | final chunk; EOF without a final chunk, or `Abort`, gives exit 4; a drop gives exit 7 |
| `chat <peer>` | `… raw chat` | open-ended streams both ways, raw; the back stream starts with the recipient's `Status` header block | EOF from each side; the session ends when both have closed; a drop gives exit 7 |
| `chat <peer> --chunked` | `… chunked chat` | open-ended streams both ways: the sender frames its stdin, the recipient frames its accept stdin after its `Status` header block; the hub checks each direction and forwards it framed, so each reader gets the final chunk and any `Abort`; no size cap | final chunk each way; a missing one or `Abort` gives exit 4; a drop gives exit 7 |

`chat` is a verb of its own, not an alias of `send`, and has no `--mode` flag. It needs a peer; there
is no `@identity` addressing and no spooling to the mailbox. It takes no `--id` and no `--subject`
(exit 2): the hub mints the msgid, and the accept header block carries `Subject: chat`.

| Rule | |
|---|---|
| target | native identities only; a topic, subscription or alias, a publisher, the instance form of an address that takes no instances, and an identity whose description says `live: false` ([help.md](help.md)) are refused: live not supported, exit 9, for every kind. An instance nobody follows is not offered the message: *Instances asleep* |
| sender | not a publisher: it receives nothing, so it sends no `--live` and no `chat` (exit 9) |
| message forms | `send --live`: `-m`, `-m:rfc5322` or `--file <name>`, one of them; an inline `-- <message>` is refused (exit 2) |
| `--size` | with `--live` or `chat`: exit 2 |
| `--id` | same grammar as `send`, `expired-*`, `wakes-*`, `head-*` and `event-*` refused (exit 2); delivered to the recipient as `Msgid`; the hub mints one without it |
| `--durable` | a spooled send's only: exit 2 ([wake-live-event-id.md](wake-live-event-id.md#durability)) |
| `--wait` | a spooled send's only, to an identity or an instance: exit 2 ([wake-live-event-id.md](wake-live-event-id.md#wait-for-pickup)) |
| `--subject` | same grammar as `send`; delivered in the hub header block. With `-m:rfc5322` it is refused (exit 2): that header block carries `Subject` |
| `--chunked` | the caller frames both directions (*Framing*); without it both are raw and the hub copies bytes. The hub never frames on a caller's behalf |
| `-m:rfc5322` | the client header block is checked against the allowed fields before the message is offered, and goes into the hub header block (`Reply-To`, `Re`, `Subject`, `Deadline`): the request after the blank line is the body only; a disallowed field (`Gap` too: it belongs to a send to a topic), a header block without its blank line, or one over `FIELD_MAX_BYTES` is refused (exit 2) |
| flood control | the per-`(sender,recipient)` bucket applies (exit 5); a recipient that is not live costs no token, except an instance nobody follows: each attempt spends one (*Instances asleep*). At most 256 ask entries per address; a send that would make one more: exit 5 |
| spool / mailbox | none — a live send or chat that is not taken live is not delivered |

## Recipient

A recipient is live while it holds the follow of its own mailbox (`recv --follow`, or
`recv <own identity> --follow`, with or without `--peek`). A subscription follow does not make
its owner live.

| Step | |
|---|---|
| announce | the hub writes a `part=live` chunk, data `LIVE <sid> <from> <msgid> (chunked\|raw) [chat]` and a newline, on the recipient's follow stream ([wake-live-event-id.md](wake-live-event-id.md#the-stream)), from the open exchanges addressed to it not announced yet: one line per exchange, printed once, on whichever follow takes it, only between messages, never inside one. `<sid>` is 16 lower-case hex digits, `<from>` the hub-verified sender; the framing token is always present, and `chat` marks a chat. The hub keeps the exchange table only, no line list: a closed exchange is never printed, and a follow that reconnects does not get an announced one again (unaccepted, its sender exits 6) |
| a line the recipient does not support | it ignores the line; the sender exits 6 after `live_accept_secs` |
| order | none: LIVE lines and notices may arrive in any order |
| accept | `ssh hub accept <sid>`: only the announced recipient may (another identity: exit 3), once (a second accept: exit 3) |
| deadline | not accepted within T of the announcement → the sender exits 6 and an accept that comes later exits 3 (the recipient drops the line). T = `live_accept_secs` in the hub settings (default 5 s) |
| concurrency | every exchange is its own `accept` session; a recipient runs them side by side, e.g. over its one SSH control connection (the hub allows 64 sessions per connection) |

The follow of an address that takes instances also carries its wake notice (*Wake notice*).

### The accept session

**stdout** — the hub header block, a blank line, then the sender's stream as it arrives: in a
chunked kind its framed body, untouched (chunk extensions and trailer included); in a raw kind its
bytes, copied:

```
From: <sender identity>
Date: <UTC, ISO 8601, seconds>
Msgid: <msgid>
Payload: file <name>             only with --file
Subject: <verb>                  only with --subject; chat on a chat
<client fields>                  only with -m:rfc5322: its header block (Reply-To, Re, Subject, Deadline)
Transfer-Encoding: chunked       only in a chunked kind

<body>
```

| Request | the (decoded) body is |
|---|---|
| `-m` | the message |
| `-m:rfc5322` | the body; the client's fields are in the hub header block. In a chunked kind the hub frames the decoded bytes that followed the header block as one chunk of their own; the rest of the stream passes untouched |
| `--file <name>` (`Payload: file <name>`) | the file's bytes |
| `chat` (the LIVE line says `chat`) | the sender's stream |

`Payload` is the stored message's field, as in a message file.

In a chunked kind the recipient decodes the framing and takes the message only on a complete last
chunk whose trailer has no `Abort`; anything else is discarded. In a raw kind the request ends at
the sender's EOF.

**stdin** — the reply: a header block, a blank line, then the reply body. In a chunked kind the
recipient frames the body like the request; the hub decodes the reply of `send --live --chunked` and
forwards it to the sender as it arrives, and checks the back stream of `chat --chunked` and forwards it
framed; in a raw kind the body runs to the recipient's EOF and the hub copies bytes. Line
ends in the header block are LF; a CR before the LF is dropped.

```
Status: <3-digit code>
Content-Type: <media type>        optional

<reply body>
```

| Rule | |
|---|---|
| fields | `Status` (required, once), `Content-Type` (optional, once); names are case-insensitive; anything else makes the reply malformed |
| `Status` | `2xx`, `4xx` or `5xx` |
| size | header block ≤ `FIELD_MAX_BYTES`; the request of `send --live` at most `FileMaxBytes` (64 MiB), past it exit 2; the reply has no size cap |
| a `chat` | the back stream starts with the same header block: `2xx`, then the stream; any other `Status`, then the end, and the sender exits 0 with the `Status` on stdout. No size cap |
| body, chunked | ends at its last chunk; EOF before it, or an `Abort` trailer: the sender exits 4 |
| body, raw | ends at EOF |
| timing | the hub reads the reply once the request has ended (its last chunk, or EOF); a reply written earlier waits in the pipe |
| EOF | raw kinds: the sender's stdin EOF is an EOF on the accept session's stdout, and in a `chat` the recipient's stdin EOF is one on the sender's stdout while the sender's stdin may stay open (D24). Chunked kinds send no early EOF: the last chunk ends a direction. An OpenSSH client that has then sent its own EOF has EOF both ways and closes its channel before the hub's exit status: it exits 255 although the exchange completed. An OpenSSH ControlMaster client (a session over a mux) never passes the hub's EOF on to its stdout (OpenSSH 9.6p1, measured): an accept over a mux never sees a raw request end, so a receiver that accepts over a mux takes chunked kinds only |
| a drop | an EOF on a raw direction is a drop (exit 7) only when that side's SSH connection is gone; a raw reply cut by a drop can read as complete, which a chunked kind rules out |

**exit status** — how the exchange ended for the sender:

| Exit | |
|---|---|
| 0 | the complete reply reached the sender |
| 2 | bad `accept` arguments, or the hub refused the reply as malformed (the reason on stderr; the sender exits 7) |
| 3 | no such exchange for this identity: unknown, addressed to another identity, already accepted, or expired |
| 4 | the sender's request was incomplete or aborted; discard it (the reply was not forwarded) |
| 7 | the exchange broke: the sender went away, or the reply ended early |
| 255 | ssh failure |

A sender whose session ends while it waits for the reply is noticed within 5 s; its accept session
then exits 7.

## Streams

| Direction | Carries |
|---|---|
| sender stdin → recipient | the request, after the hub header block: framed and untouched in a chunked kind, raw bytes copied in a raw kind |
| recipient → sender stdout | the reply: header block (`Status`, `Content-Type`), blank line, body — forwarded as it arrives |
| sender stderr | hub diagnostics only; never part of the reply |

A chunked request ends at its last chunk: the sender may keep its side open (the hub ends the
session after the reply and exit status) or close it; a sender that closes before the last chunk
has aborted. A raw request ends at the sender's EOF.

A `chat` carries open-ended streams both ways on one exchange: the sender's stdin to the
recipient's accept stdout, the recipient's accept stdin to the sender's stdout, which starts with the
recipient's `Status` header block (`2xx` then the stream; any other, the end). Each direction ends
at its EOF (raw) or its final chunk (chunked); the session ends when both have closed. There is no
size cap.

## Reply

What the sender reads on stdout:

```
Status: <3-digit code>
Content-Type: <media type>        when the recipient set one

<body>
```

| `Status` | Recipient's verdict |
|---|---|
| `2xx` | accepted |
| `4xx` | rejected — do not resend |
| `5xx` | failed — may resend |

## Outcomes

The session's exit code says who decided; `Status` says what the recipient decided.

| Exit | Outcome | stdout |
|---|---|---|
| 0 | recipient replied; verdict in `Status` | the complete reply |
| 2 | bad request: arguments, `--id`, `--wait`, `--size`, form conflict, inline message, a refused client header block, malformed framing | empty |
| 3 | no such target — including an instance its address `reject`ed — or the session's key no longer holds the sender's identity | empty |
| 4 | a chunked kind: a direction ended without its final chunk, or with `Abort`; a recipient that had accepted an incomplete request saw it and discards it | empty, or the partial reply |
| 5 | rate limited | empty |
| 6 | recipient not live: nothing follows its mailbox, or it did not accept within T (a recipient that does not support the LIVE line ignores it); or an instance nobody follows (flow C), or no answer to its ask (flow C2), which carry `retry-after=<s>` on stderr. Nothing delivered | empty |
| 7 | the stream broke: a dropped connection (every kind), or a malformed reply | empty if before the reply's header block; otherwise the partial reply |
| 9 | live not supported, for every kind: a topic, a subscription, an alias; a publisher as target or as sender; an instance form of an address that takes no instances; an identity whose description says `live: false` | empty |
| 255 | ssh failure (connection, auth) — set by the ssh client | empty |

## Instances asleep

An instance (`addr+inst`, [identities.md](identities.md#instances)) that nobody follows is not offered a live
send or a chat. Its address (`addr`), whose follow carries the wake notice, starts it.

| # | `send --live` or `chat` to `addr+inst` | |
|---|---|---|
| C | a mailbox, nobody following | the hub adds a transient entry `{addr+inst, From: sender, stay-up-till}` and exits 6 with `retry-after=<live_retry_secs>` on stderr; nothing stored. `addr`'s wake notice lists it; `addr` starts the instance; the sender's retry, once the instance follows, is an ordinary live send |
| C2 | no mailbox | the sender is held for `addr`'s answer, as a spooled send is ([identities.md](identities.md#admitting-and-rejecting-an-instance)): `admit` → the mailbox is made, then flow C; `reject` → exit 3; no answer within `wake_secs` → exit 6, `retry-after` |

| Flow C | |
|---|---|
| `stay-up-till` | the live attempt's time + 3 × `live_retry_secs` |
| the transient entry | in the hub's memory only, nothing stored; ends at its `stay-up-till`, or when the instance follows its mailbox |
| a retry before the instance follows | exit 6 again; the entry is refreshed (`stay-up-till` from this attempt), not added twice |
| a hub restart | entries lost; the next retry makes one again |
| retry | `retry-after` is a hint; the sender decides when or whether to retry. Each attempt spends its flood token |

`retry-after` is carried by exactly these exit 6 cases: flows A2 ([identities.md](identities.md#admitting-and-rejecting-an-instance)), C2 and C.

## Wake notice

The follow of an address that takes instances lists the instances that need starting, rendered by the hub:

```
178;id=wakes-5d41402abc4b2a76;part=msg
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
| mailbox | the address's own; an address that takes no instances has none |
| `From`, `Date` | the address itself; the render time |
| msgid | `wakes-<first 16 hex of sha256 of the lines and the held asks' numbers>`: a new msgid whenever the content changes, and for an ask made again after its answer. A client's `--id wakes-*` exits 2 |
| identification | no `Subject`; `Wake: <n>`, the number of instance lines |
| the wake set | each instance not followed with `.msg` mail in its mailbox, a transient entry, or an ask entry. Expiry notices do not count |
| line | TAB-separated: the instance, then the fields it has: `mail=<n>` (`.msg` files only) and `from=<id>@<UTC>,…` for mail or a transient entry, `stay-up-till=<UTC>` on a transient entry only, `ask=<id,id,…>` on an ask entry. Lines with mail or a transient entry first, then lines with only `ask=`, each in name order. No cap |
| `from=` | per distinct sender, in name order: its hub-stamped `From`, `@`, the newest hub-stamped `Date` of its unread mail there, or of its transient entry |
| `ask=` | the held senders, distinct. The address answers with `admit` or `reject`. Independent of `mail=`: a `--wait` send held on a mailbox with mail carries both on one line |
| rendered | at read, from the hub's in-memory wake summary (per instance, each unread `.msg`'s sender and `Date`; built from disk at the address's first render, one instance's new files read after each change of its mailbox, dropped when the operator's tools change the address's claim, when inotify lost events, and at each reaper sweep while inotify is not running), the follow registry and the entries; never stored, never a `.msg` |
| printed | on every follow of the address, first in its first pass (after a `notice_probe`), `Wake: 0` included; again whenever its msgid changes |
| `recv` | streams it as a `part=msg` chunk before the notices and messages, never consumes it, `--peek` or not; `recv_ack wakes-*`: `refused` |
| `recv --id wakes-<any>` | the current rendering: `From`, `Date`, `Wake`, a blank line, the lines |

| The address's follow is woken by | |
|---|---|
| its instances | a delivery; a consume, ack or expiry there; a follow start or end |
| admission | `admit`, `reject`, a claim made or ended |
| entries | an ask or transient entry made, refreshed or ended |

## Framing

A chunked kind (`--chunked`) is framed as HTTP/1.1 chunked transfer coding (RFC 9112 §7.1), both
directions, by the caller: the sender frames its stdin, the recipient its accept stdin. The hub
never frames on a caller's behalf, and decodes and checks only a chunked stream; a raw stream it
copies.

```
<hex size>[;ext]\r\n<size bytes>\r\n     repeated
0\r\n
[trailer fields]\r\n
```

| Element | Meaning |
|---|---|
| last chunk (`0`) + empty line | the body is complete |
| EOF before it | aborted |
| trailer `Abort: <reason>` | aborted, with a reason |
| chunk extensions `;...` | reserved for control; ignored by the spool, forwarded to a live recipient |

| Path | Hub does |
|---|---|
| spool (`send --chunked` without `--live`, `send_bag --chunked`) | decodes; commits only after the last chunk and a trailer without `Abort`; otherwise stores nothing, exit 4 |
| `send --live --chunked` request | checks the framing and forwards the framed stream untouched, as it arrives; the recipient decodes it |
| `send --live --chunked` reply | checks the framing, decodes it and forwards the body to the sender as it arrives |
| `chat --chunked` | checks each direction and forwards it framed: each reader gets the final chunk and any `Abort` |
| raw, either direction | copies bytes |

## Abort

| Sender does | Recipient sees | Sender's exit |
|---|---|---|
| stops before the last chunk | an incomplete framed body — discards the message; its accept exits 4 | 4 |
| sends `Abort` in the trailer | the trailer, forwarded — discards the message; its accept exits 4 | 4 |

A sender that must not deliver a partial body uses `--chunked` and relies on the framing. An SSH `signal` request
cannot stand in for it: the hub takes exec requests only.

## On the hub

| Item | |
|---|---|
| rendezvous | a table entry per exchange, in memory: the accept session hands its stdin and stdout to the sender's session, which relays both ways and posts the verdict; gone with the sender's session. A follow prints `LIVE` lines from it |
| accept deadline | `/var/lib/hub/settings`, `live_accept_secs=<seconds>` (up to 3 decimals, 0.001–60), root:hub 0640; absent or invalid: 5 |
| ask timeout | the same file, `wake_secs=<seconds>` (up to 3 decimals, 0.001–600): how long an ask entry holds its senders; absent or invalid: 60 |
| retry hint | the same file, `live_retry_secs=<seconds>` (up to 3 decimals, 0.001–600): `retry-after` and `stay-up-till`; absent or invalid: 5 |
| pending state | only in the receiving mailbox, or in memory tied to a connected session; the one exception is flow C's transient entry, ended by its `stay-up-till` or the instance's follow |
| sessions | 64 per connection (`-max-sessions`) |
