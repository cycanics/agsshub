# Backend plugins (egress transports)

A **backend** delivers an outbound message to a transport other than the native ssh spool —
`email`, `tg` (Telegram). Backends are addressed through the
**alias registry**: an operator maps a local alias to a type + address with `hub-alias`, e.g.

```
hub-alias add carol --type email --addr carol@example.com
hub-alias add opsroom --type tg --addr -1001234567890
hub-alias add ops-mail --type email --addr ops@example.com
```

The hub resolves a send target: a live native claim → native spool; otherwise an alias →
this backend seam. `list --type email` filters the registry by type.

## Contract

Drop an **executable** `backends/<type>/send` (this dir, installed to `/usr/local/lib/hub/backends/`).
The hub (`Config.Lib`, `-lib`) runs it when a message routes to that type. Until one exists, the seam reports
`backend '<type>' not implemented` (exit 40) — that is the placeholder state.

`send` receives:

| Channel | Value |
|---|---|
| env `HUB_FROM` | authenticated sender identity (hub-stamped, trustworthy) |
| env `HUB_TARGET` | the alias the sender addressed |
| env `HUB_ADDR` | the alias's `addr` (email / tg chat id) |
| env `HUB_BODY` | message body (may be empty) |
| env `HUB_FILE` | attachment basename (empty if none) |
| env `HUB_SUBJECT` | the message's `--subject`, or its `Subject:` line (empty if none) |
| stdin | attachment bytes when `HUB_FILE` is set |

Exit `0` on delivery; nonzero to signal failure to the sender. Never print secrets. Inbound
(transport → hub) is a separate `backends/<type>/poll` daemon that sends as the mapped identity — not specified here yet; add alongside the first real backend.

## Status

`email/`, `tg/` are placeholders (`send.example` templates only). No backend is wired.
