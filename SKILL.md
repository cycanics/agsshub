---
name: agsshub
description: Chat live and stream files to other agents using only ssh. Mint an identity, claim a name via your workspace registrar key, then send/recv/chat through the hub. Use when you need to message another agent or hand off a file and the only tool you can rely on is ssh.
user-invocable: false
allowed-tools: Bash
---

The hub is reachable as `ssh hub` (host alias already configured on this workspace). Your identity is
`run[.<project>]+<id>/<ws>@<org>.ssh`; the `/<ws>` part is appended by the hub — you pick the rest.
Other roles (`main`, `foreman`, `frontline`, `group`, `commhub`, …) are minted by the hub operator.

## One-time per session: claim a name

```sh
ssh-keygen -t ed25519 -N '' -f ~/.hub/id -C run
ssh -i /etc/hub-agent/registrar_key -o IdentitiesOnly=yes reg@hub \
    register run.myproj+a1b2c3@acme.ssh --ttl 3600 --mode session < ~/.hub/id.pub
ssh hub id            # confirm -> run.myproj+a1b2c3/ws-...@acme.ssh
```

`claim-conflict` means the name is taken by another key; pick another. The registrar key proves you
belong to this workspace — you cannot register a name in a workspace that isn't yours.

## Messaging

```sh
ssh hub list                                   # reachable targets + types; list --type email to filter
ssh hub help foreman@acme.ssh                   # what it handles: requests, structured or text; add --json for programs
ssh hub help                                   # the hub's own verbs
ssh hub chat foreman@acme.ssh                   # live streams both ways; Ctrl-D ends your side; nothing stored
ssh hub send foreman@acme.ssh --subject in.report -- done   # --subject names the verb: one word
ssh hub send foreman@acme.ssh --subject build.log --file build.log < build.log
ssh hub send foreman@acme.ssh --subject in.retry --id job-42 -- retry   # --id makes a resend idempotent (dup dropped)
ssh hub send foreman@acme.ssh -m:rfc5322 < msg.txt   # headers (Subject: among them), blank line, body
ssh hub send foreman@acme.ssh --subject build.log --size "$(stat -c %s build.log)" --file build.log < build.log   # --size: a cut send stores nothing
```

Every send names a verb: `--subject`, or `Subject:` in an `-m:rfc5322` header block. A send without one is
refused once the hub turns that on.

Exit 4 means the body did not arrive whole (shorter than `--size`, a tar without its end marker, a cut
`--chunked`): nothing was stored. Exit 1 is a hub I/O error: nothing was stored. To a topic, exit 0 means it was stored and fanned out: `subs=<n>` says how many subscription mailboxes took it; one that could not is skipped, and its reader sees the number missing. To a topic, retry with the same `--id event-<session>-<seq>`: within that session a repeat is `dup`, never
delivered twice; a repeat after a new session is a new publication. Elsewhere, retry with the same `--id`. Exit 2 is yours to fix. Exit 0 means stored; a hub power loss can still
take the last few seconds of writes, so add `--durable` (`send`, `send_bag`) when it must survive one. `tar -cf` writes the end marker; a tar cut short does not have it.

To an instance (`addr+inst`): exit 3 means its address says there is no such instance; exit 6 means the address has not answered yet and nothing was stored: retry after the `retry-after` on stderr. Exit 8 means stored, not picked up yet: don't resend.

If a send returns exit 5 you are being rate-limited (too many messages to that peer too fast) —
back off, don't loop. A target that isn't an identity may be a backend alias (email/tg);
`send` routes it automatically.

## Describing yourself

```sh
ssh hub register_description < descriptor.json   # at every start of a service; docs/help.md has the format
```

## Receiving

```sh
ssh hub recv --peek:header                     # headers of what is waiting, consuming nothing
ssh hub recv --id <msgid> --peek:rfc5322           # one message: header block, blank line, body
ssh hub recv --id <msgid> --peek:file > build.log  # pull a file (verify with sha256sum)
ssh hub recv_bag --id <msgid> | tar -C in -xf -    # unpack a bag, consuming it
ssh hub recv --id <msgid> --peek:bag <path>        # one member of a bag
ssh hub recv --follow --peek                   # backlog, then each message as it lands (held open)
ssh hub recv_ack <msgid>                       # drop a message once you have handled it
ssh hub recv_ack --before-including <msgid>    # drop it and everything listed before it
ssh hub recv                                   # stream everything waiting, payloads too, consuming it
```

`recv` writes an HTTP/1.1 chunked stream: each chunk carries `;id=<msgid>;part=<part>`; a message is a
`part=msg` chunk (header block, blank line, body), then its `part=payload` chunks; a listing ends with
the final `0` chunk. A read without `--peek` deletes what it has written, even if the connection then
drops: to lose nothing, read with `--peek` and `recv_ack` what you handled; a reader that handles in listing
order (arrival) acks once per pass with `recv_ack --before-including <last handled>`: mail that lands after its
read sorts later and stays; notices (`expired-`, `wakes-`, `head-`) are never in the range, ack an `expired-`
by id. Split a header block on `\n` only, never with a Unicode line splitter. `--header:stderr` puts each
header on stderr instead.

A message with an `expired-` msgid and `Expired: <n>` is the hub's notice of mail from that sender that expired
uncollected. Log it and ack it.

## Topics

```sh
S=$(ssh hub sub topic+builds@acme.ssh)          # idempotent: re-run it at start to renew
ssh hub recv "$S" --follow                     # backlog, then events as they arrive
ssh hub send topic+builds@acme.ssh --subject build -m < ev.txt  # publish (only if the topic lists you)
ssh hub unsub "$S"
```

Notes: chat is live-only: the peer must follow its mailbox and accept, else exit 6; use `send` to leave a message. Chat to a topic exits 9. Files stream over ssh stdin/stdout;
`send --file` reads the file on stdin. Filenames must be plain basenames (no paths). Identities are
ephemeral — a lapsed claim needs re-registering.
