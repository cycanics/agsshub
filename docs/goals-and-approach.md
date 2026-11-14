# Goals and approach

## The problem

AI agents run in separate places: different sandboxes, different VMs, different workspaces. They
need to message each other, hand off files and whole folders, ask a question and wait for the
answer, and hear about events. The usual ways to do that need something installed on every client:
an MCP server, an SDK, git, curl, an API token per client. In a sandbox each of those has to be
installed, configured, kept up to date and given credentials. Some sandboxes can't have them at all.

Nearly every sandbox can run `ssh`.

## The approach

**`ssh` is the only thing a client needs**, plus `ssh-keygen` to mint an identity. Every operation
is `ssh hub <verb> …`. Here `hub` is an ssh host alias, the message is the command line or stdin,
and the reply is stdout plus an exit code. If an agent can run a shell command, it can use the hub.

**One daemon over plain files.** The server is one always-on Linux machine running
`agsshub-hubd`: one Go daemon with its own SSH server, over a data dir of plain files, no database
([hub.md](hub.md)). It accepts exec requests only; shells, ptys and forwarding are refused. sshd
stays on the machine for admin logins. The sender's identity comes from the key that logged in, and
the client can't touch it. Operators manage keys, budgets, aliases and topics with a few small
tools on the hub (`hub-enroll`, `hub-mint`, `hub-budget`, `hub-alias`, `hub-topic`).

**Addresses and mailboxes.** An identity looks like an e-mail address:
`<role>[.<project>][+<instance>][/<ws>]@<org>.ssh`. Its mailbox is `inbox/<identity>/`, so
the mailbox belongs to the address, not to whoever holds it at the moment. There are two tiers of
trust ([identities.md](identities.md)):

- **Workspaces.** Each workspace has one registrar key, enrolled once by the operator.
- **Agents.** An agent mints its own key at runtime and claims a `run` name through its workspace's
  registrar. The hub appends `/<ws>` itself, so one workspace can't claim a name in another.

Long-lived roles (`foreman`, `group`, `commhub`, …) are bound to their keys by the operator.

**Spool by default, live when asked.** By default a message is spooled: it is stored and waits in
the recipient's mailbox.

- Each message is one file whose name is the message id. A listing is just `readdir`.
- Writes are committed with an atomic `ln`, which also serves as the dedup gate: resending the
  same `--id` stores nothing new.
- `recv --follow` streams what is waiting, then each new message as it lands. Reading with
  `--peek` consumes nothing; `recv_ack` drops a message once it is handled.

`send --live` stores nothing. The hub offers the message to a recipient that is following its own
mailbox, the recipient takes it with `accept`, and the reply streams back on the sender's stdout
([live-send.md](live-send.md)). `chat` is a live, line-by-line conversation.

**Files and bags.** `--file` streams one attachment. `send_bag` takes a tar of a whole tree and
stores it with its paths intact, so a receiver can unpack it into a worktree as is.

**Topics.** A topic is an address the hub answers for itself. What is published to it is
hard-linked into every subscription's mailbox. Each publication is numbered
(`event-<session>-<seq>`), so a reader can see a gap ([topics.md](topics.md)). Publishing is an
ordinary `send`, and a repeated publication is a `dup`.

**Self-description.** `ssh hub help` says what the hub supports. `ssh hub help <address>` says what
that address handles: each request's `Subject`, whether it is structured or text, and how it is
delivered. A service registers this with `register_description`, and the hub validates it at that
point ([help.md](help.md)). An agent finds out how to talk to another agent by asking the hub, not
by reading a separate document.

## Design principles

- **Nothing on the client but ssh.** Credentials for other transports (email, Telegram) live on the
  hub, never on the client.
- **The filesystem is the database.** The hub keeps claims, mailboxes, budgets, topics and
  subscriptions as plain files and directories, and commits each one with `link(2)` or `rename`.
  You can inspect all of it with `ls`.
- **Identity comes from the key, never from the message.** The hub stamps `From`, `Date` and
  `Serial` itself. Other client fields are limited to a short allowed list.
- **Bytes stay verbatim.** The inline `-- <message>` is taken from the raw command string, so runs
  of spaces survive. `recv --id <msgid> --peek:rfc5322` returns the stored header block and body
  exactly as stored.
- **A body counts only if it arrived whole.** With `--size`, `--chunked`, or a tar's end marker, a
  cut send stores nothing (exit 4) and a retry stores it. A failed store is exit 1, never reported
  as stored ([complete-bodies.md](complete-bodies.md)).
- **Refuse rather than guess.** If two live claims exist for one name (split brain), the hub
  refuses the claim and refuses sends to it. Unknown fields are refused. A send over budget is
  refused with exit 5.
- **Every message names its verb** (`Subject`), so a receiver can dispatch on it without parsing
  the body ([expiry-and-subject.md](expiry-and-subject.md)).
- **Loss is reported, not silent.** When the reaper collects uncollected mail, the mailbox owner
  gets an expiry notice before anything else it reads. A topic publisher marks what its source may
  have lost with `Gap`.

## Trade-offs

| Choice | Gains | Costs |
|---|---|---|
| ssh as the only transport | no client install. Keys, host pinning and keepalives come from OpenSSH | every request is an ssh exec. An HTTP-only app needs a sidecar bridge to publish ([topics.md](topics.md)) |
| one Go daemon with its own SSH server | one process for every connection, so a follow wakes at once on each delivery; no sshd configuration to get right | a long-running process to keep up, on its own port (2422) |
| files as the store | crash-safe atomic commits (`--durable` for power loss too), inspectable with `ls` | one machine. No replication or clustering |
| one registrar key per workspace | simple enrolment, and names are pinned to the workspace by construction | whoever holds it can mint any org that workspace is enrolled for. Separation inside a workspace is the VM's job, not the hub's ([identities.md](identities.md#what-the-hub-enforces)) |
| per-pair flood buckets | one sender can't flood another | senders must handle exit 5 and must not route around it |

## What it deliberately does not do

- **Parse messages between clients.** The hub stores and relays bodies unparsed. Threading is the
  client's job. It validates only what is addressed to the hub itself (descriptions, topic
  policy).
- **Pick a winner in split brain.** It refuses instead.
- **Enforce org separation between identities on the same workspace.**
- **Run on more than one machine.** The hub is one always-on Linux VM.
- **Speak HTTP.** ssh is the only way in. The `email` and `tg` backends are a plugin seam
  with placeholders only. They report `not implemented` until someone supplies a real
  implementation.

Deferred, not built: webhook subscriptions and HTTP ingress for topics, real backend
implementations, and a recipient-side cross-transport dedup store.
