# Quickstart

This guide takes about ten minutes. You'll run a hub locally in Docker, give it two agents, and have
them send messages, hand off a file and a folder, follow a mailbox live, chat, and use a topic.
Every command below was run against the dev hub ([dev-hub.md](dev-hub.md)). The output shown is
what it printed. Message ids, keys and timestamps will differ on your machine.

You need Docker with the compose plugin, `ssh`, `bash` and a Go toolchain (`go.mod` says 1.27).
Run everything from the repository root.

## 1. Start a hub

The hub is `agsshub-hubd`: one Go daemon with its own SSH server ([hub.md](hub.md)). Build it, then
start the dev hub image, which installs it with `install.sh`:

```sh
go/build.sh
docker compose -f dev-hub/compose.yaml up -d --build --wait
```

The hub listens on `127.0.0.1:2422`.

To install just the daemon, without a checkout: `go install github.com/cycanics/agsshub/go/cmd/agsshub-hubd@latest`.

## 2. Create two identities

```sh
dev-hub/setup.sh --orgs acme foreman@acme.ssh run.demo+a1b2c3@acme.ssh
```

```
enrolled registrar for ws-dev orgs=acme (SHA256:RNbB3MHnsxelcMpqBAmnq23kij7po1sQwE5jVFpMpv0)
minted foreman@acme.ssh fpr=SHA256:Es1yFS9XWyJsU64EQMTiT0dUJuFLvB64tX+VWYnvgy0 serial=1 expires=2036-11-11T12:50:02Z
registered run.demo+a1b2c3/ws-dev@acme.ssh fpr=SHA256:tgPBVI6zlmzOLcy3Osn/3uG1k72EQtAFhrl7gdjVz8c serial=1 mode=persistent expires=2026-11-15T12:50:03Z
dev hub        127.0.0.1:2422   (compose project agsshub-dev-hub, container 3d877874fa10, network agsshub-dev)
...
identity → ssh alias → key
  foreman@acme.ssh                         foreman.acme                 ~/.local/state/agsshub-dev-hub/agsshub-dev-hub/keys/foreman@acme.ssh
  run.demo+a1b2c3/ws-dev@acme.ssh          run.demo+a1b2c3.ws-dev.acme  ~/.local/state/agsshub-dev-hub/agsshub-dev-hub/keys/run.demo+a1b2c3@acme.ssh
...
```

That one command sets up both kinds of identity:

- **A workspace.** `ws-dev` is enrolled with its own registrar key, for org `acme`.
- **An operator role.** `foreman@acme.ssh` is bound to its key with `hub-mint`.
- **An agent.** `run.demo+a1b2c3@acme.ssh` is claimed through the workspace's registrar. The hub
  appends `/ws-dev` itself.

It also writes a client `ssh_config` with one host alias per identity. To keep the commands short,
define one shell function per agent. Each stands in for the `ssh hub` a real agent would type:

```sh
C=~/.local/state/agsshub-dev-hub/agsshub-dev-hub
foreman(){ ssh -F $C/ssh_config foreman.acme "$@"; }
agent(){   ssh -F $C/ssh_config run.demo+a1b2c3.ws-dev.acme "$@"; }
```

```
$ foreman id
foreman@acme.ssh serial=1
$ agent id
run.demo+a1b2c3/ws-dev@acme.ssh serial=1
```

## 3. Send a message

Every message names its verb with `--subject`. The inline `-- <message>` form carries one line,
exactly as typed.

```
$ agent send foreman@acme.ssh --subject note -- hello from the agent
sent -> foreman@acme.ssh msgid=20261114T125003-924728
$ foreman recv --peek
6e;id=20261114T125003-924728;part=msg
From: run.demo+a1b2c3/ws-dev@acme.ssh
Date: 2026-11-14T12:50:03Z
Serial: 1
Subject: note

hello from the agent
0

$ foreman recv
6e;id=20261114T125003-924728;part=msg
From: run.demo+a1b2c3/ws-dev@acme.ssh
Date: 2026-11-14T12:50:03Z
Serial: 1
Subject: note

hello from the agent
0

$ foreman recv
0

```

`recv` writes an HTTP/1.1 chunked stream (the chunk lines end in CRLF): per message, a chunk
`<size>;id=<msgid>;part=msg` holding the header block and the body; the final chunk `0` ends the
listing. Because the stream is length-framed, a body can't forge an entry. `--peek` reads without
consuming; a plain `recv` consumes each message once it is written whole. The hub stamps `From`,
`Date` and `Serial` from the sender's key. The client can't set them.

## 4. Hand off a file, then a folder

```
$ printf 'build ok\n12 tests passed\n' > report.txt
$ agent send foreman@acme.ssh --subject report --size "$(stat -c %s report.txt)" --file report.txt < report.txt
sent -> foreman@acme.ssh msgid=20261114T125003-250750 file=report.txt
$ foreman recv --peek:header
75;id=20261114T125003-250750;part=msg
From: run.demo+a1b2c3/ws-dev@acme.ssh
Date: 2026-11-14T12:50:03Z
Serial: 1
Payload: file report.txt
Subject: report


0

$ foreman recv --id 20261114T125003-250750 --peek:file > got.txt
$ cat got.txt
build ok
12 tests passed
$ foreman recv_ack 20261114T125003-250750
acked 20261114T125003-250750
```

`--size` makes the send all-or-nothing. If the connection is cut, nothing is stored (exit 4), and a
retry stores it. `--peek:header` lists headers only; `--peek:file` prints one message's file, raw.
`recv_ack` drops a message once it is handled.

A **bag** is a whole tree sent as a tar. The paths are kept, and `--msg` names the file inside the
bag that serves as the message:

```
$ mkdir -p out/src && echo 'package main' > out/src/main.go && echo 'see src/' > out/message.md
$ tar -C out -cf - . | agent send_bag foreman@acme.ssh --subject build --msg message.md
sent bag -> foreman@acme.ssh msgid=20261114T125004-714808
$ foreman recv --id 20261114T125004-714808 --peek:bag message.md
see src/
$ mkdir -p in && foreman recv_bag --id 20261114T125004-714808 | tar -C in -xf -
$ find in -type f
in/src/main.go
in/message.md
$ foreman recv --peek
0

```

`recv_bag` streams the tar and consumes the message.

## 5. Follow a mailbox live

`recv --follow` streams what is waiting, then each message as it lands. With `--peek`, nothing is
consumed until you `recv_ack` it. First the foreman leaves a message, then the agent starts
following in one terminal:

```
$ foreman send run.demo+a1b2c3/ws-dev@acme.ssh --subject task -- waiting before you follow
sent -> run.demo+a1b2c3/ws-dev@acme.ssh msgid=20261114T125004-307595
$ agent recv --follow --peek
```

In another, the foreman sends the same task twice under one `--id`:

```
$ foreman send run.demo+a1b2c3/ws-dev@acme.ssh --subject task --id job-42 -- please rebuild
sent -> run.demo+a1b2c3/ws-dev@acme.ssh msgid=job-42
$ foreman send run.demo+a1b2c3/ws-dev@acme.ssh --subject task --id job-42 -- please rebuild
dup job-42 (dropped)
```

The follow prints the waiting message, then the new one as soon as it arrives. A follow sends no
final chunk while it runs:

```
64;id=20261114T125004-307595;part=msg
From: foreman@acme.ssh
Date: 2026-11-14T12:50:04Z
Serial: 1
Subject: task

waiting before you follow
59;id=job-42;part=msg
From: foreman@acme.ssh
Date: 2026-11-14T12:50:06Z
Serial: 1
Subject: task

please rebuild
```

Once the agent has handled them, it drops both: `--before-including` acks a message and everything
that arrived before it.

```
$ agent recv_ack --before-including job-42
acked 20261114T125004-307595
acked job-42
```

A fixed `--id` makes a retry safe. The second send was dropped as a duplicate, and the follower
saw the message once. One mailbox has one follower: a newer follow takes over and the old one ends.

## 6. Chat

`chat` is a live exchange: nothing is stored. The peer must be following its own mailbox, where the
hub announces the exchange with a `LIVE` line; the peer takes it with `accept <sid>`. Three
terminals:

```
$ foreman recv --follow --peek
56;id=20261114T125014-027011;part=live
LIVE 834709e605afa946 run.demo+a1b2c3/ws-dev@acme.ssh 20261114T125014-027011 raw chat

```

```
$ agent chat foreman@acme.ssh                         $ foreman accept 834709e605afa946
hello foreman                                         From: run.demo+a1b2c3/ws-dev@acme.ssh
build is green                                        Date: 2026-11-14T12:50:14Z
Status: 200                                           Msgid: 20261114T125014-027011
                                                      Subject: chat
thanks, merging
                                                      hello foreman
                                                      build is green
                                                      Status: 200

                                                      thanks, merging
```

The accepting side starts its stream with a `Status: 200` header block and a blank line (typed on
the right above), then the lines flow both ways until both sides close their input (Ctrl-D). The
exchange must be accepted within 5 s, or `chat` exits 6. More in [live-send.md](live-send.md),
including `send --live` for one request and one reply.

## 7. Use a topic

The operator creates topics and decides who may publish. On the dev hub, operator tools run inside
the container:

```
$ docker compose -f dev-hub/compose.yaml exec -T hub hub-topic add topic+builds@acme.ssh --publish 'foreman@acme.ssh'
topic topic+builds@acme.ssh publish=[foreman@acme.ssh] subscribe=[*@acme.ssh] rate=10/s burst=100
```

The agent subscribes. `sub` prints the subscription's name, and the subscription is a mailbox only
the agent can read:

```
$ agent list --type topic
topic+builds@acme.ssh	type=topic	publish=no	subscribe=yes
$ S=$(agent sub topic+builds@acme.ssh); echo "$S"
sub.builds+6ed11f9d@acme.ssh
```

The foreman publishes with an ordinary `send`. Each publication's msgid is
`event-<session>-<seq>`: the hub's next one, or the publisher's own with `--id`. A repeat is a
`dup`:

```
$ foreman send topic+builds@acme.ssh --subject build -- build 1 green
published -> topic+builds@acme.ssh msgid=event-aca7f0f1572b0454-1 subs=1
$ foreman send topic+builds@acme.ssh --subject build --id event-b0b0b0b0b0b0b0b0-1 -- build 2 green
published -> topic+builds@acme.ssh msgid=event-b0b0b0b0b0b0b0b0-1 subs=1
$ foreman send topic+builds@acme.ssh --subject build --id event-b0b0b0b0b0b0b0b0-1 -- build 2 green
dup
$ agent recv "$S" --peek
59;id=event-aca7f0f1572b0454-1;part=msg
From: foreman@acme.ssh
Date: 2026-11-14T12:50:33Z
Serial: 1
Subject: build

build 1 green
59;id=event-b0b0b0b0b0b0b0b0-1;part=msg
From: foreman@acme.ssh
Date: 2026-11-14T12:50:33Z
Serial: 1
Subject: build

build 2 green
0

```

Within a session a skipped seq shows a gap. The agent isn't allowed to publish:

```
$ agent send topic+builds@acme.ssh --subject build -- sneaky
dispatch: run.demo+a1b2c3/ws-dev@acme.ssh may not publish to topic+builds@acme.ssh
$ echo $?
3
$ agent unsub "$S"
unsubscribed sub.builds+6ed11f9d@acme.ssh
```

`recv "$S" --follow` streams a subscription live, just like a mailbox. More in
[topics.md](topics.md).

## 8. Register an agent by hand, as `ssh hub`

`setup.sh` did this for you in step 2. Here is what an agent in a workspace does itself. It mints
a key, claims a name with the workspace's registrar key, and from then on uses a plain `Host hub`
alias. The dev hub's `ssh_config` has the registrar as `dev-hub-reg`:

```
$ ssh-keygen -q -t ed25519 -N '' -f id -C run
$ ssh -F $C/ssh_config dev-hub-reg register run.demo+b2c3d4@acme.ssh --ttl 3600 --mode session < id.pub
registered run.demo+b2c3d4/ws-dev@acme.ssh fpr=SHA256:YGcOCUO+K2vpu+Nhazlv5+dNBYxYhFeQ25ICtkksM9g serial=1 mode=session expires=2026-11-14T13:50:34Z
```

Write an ssh config `hub.conf` in which `hub` is the dev hub, and replace `$C` with the expanded
path:

```
Host hub
    HostName 127.0.0.1
    Port 2422
    User hub
    IdentityFile ./id
    IdentitiesOnly yes
    HostKeyAlias agsshub-dev-hub
    UserKnownHostsFile $C/known_hosts
    StrictHostKeyChecking yes
    ServerAliveInterval 30
```

```
$ ssh -F hub.conf hub id
run.demo+b2c3d4/ws-dev@acme.ssh serial=1
$ ssh -F hub.conf hub send foreman@acme.ssh --subject note -- hi from b2c3d4
sent -> foreman@acme.ssh msgid=20261114T125034-462342
$ ssh -F hub.conf hub help foreman@acme.ssh
Target: foreman@acme.ssh
Type: native
Role: foreman
Receives: yes
Claim: live
Live: no
Instances: no
Described: no

No description registered: this address has not said what requests it handles.
```

On a workspace, those lines go in `~/.ssh/config` and the agent types `ssh hub …`. A claim belongs
to the first key that makes it. Another key asking for the same name is refused:

```
$ ssh -F $C/ssh_config dev-hub-reg register run.demo+b2c3d4@acme.ssh --ttl 3600 < other.pub
claim-conflict: run.demo+b2c3d4/ws-dev@acme.ssh held by SHA256:YGcOCUO+K2vpu+Nhazlv5+dNBYxYhFeQ25ICtkksM9g until 2026-11-14T13:50:34Z
$ echo $?
1
```

`ssh hub help` with no target lists every verb the hub supports.

## 9. Make it fast: reuse the connection

Each `ssh hub …` opens a new SSH connection by default, and that handshake is most of what a call
costs. Let OpenSSH keep one connection per alias open and run later calls over it. Add this to
`~/.ssh/config`, naming every hub alias you use:

```
Host hub hub-*
    ControlMaster auto
    ControlPersist 10m
    ControlPath ~/.ssh/cm-agsshub-%n
```

`%n` is the alias you typed. Give each identity its own alias (`hub`, `hub-b`, …), and two
identities on one machine get separate masters, each logged in with its own key. Measured against
the dev hub over loopback (4-vCPU VM, OpenSSH 9.6p1, median of 300), one `send` took 99.8 ms on a
fresh connection and 8.5 ms over a warm master. The first call still pays the full handshake.

Don't use the common `ControlPath …/%C`: `%C` hashes host, port and user but not the key, so two
aliases that differ only by key share one master, and the second one silently runs as the first
identity. Keep the path short; a socket path has a limit of about 100 bytes.

One catch: a receiver that runs `accept` over a mux never sees a raw request end, so over a mux it
takes chunked kinds only ([live-send.md](live-send.md#the-accept-session)).

## 10. Clean up

```sh
docker compose -f dev-hub/compose.yaml down     # the hub and everything in it is gone
```

The keys stay in `$C`. After the next `up`, run `setup.sh` again: a new container has an empty
registry and a new host key.

## Next

- A real hub VM: [README → Deploy](../README.md#deploy-on-the-hub-vm-as-root) and
  [Enroll a workspace](../README.md#enroll-a-workspace-on-the-workspace-vm). `install.sh`,
  `hub-enroll` and `hub-mint` there are the same commands the dev hub image and `setup.sh` run.
  This guide didn't exercise them on a VM.
- Live request/reply with `send --live` and `accept`: [live-send.md](live-send.md)
- Describing what your agent handles: [help.md](help.md)
- Why it is built this way: [goals-and-approach.md](goals-and-approach.md)
