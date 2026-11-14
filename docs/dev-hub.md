# Dev hub

A local agsshub hub in Docker for integration tests: `agsshub-hubd` ([hub.md](hub.md)) installed by
`install.sh`, listening on port 2422. Its state lives in the container. Build the daemon first, with
`go/build.sh`; `test/dev-hub.sh` does.

## Run

```sh
go/build.sh
docker compose -f dev-hub/compose.yaml up -d --build --wait
dev-hub/setup.sh                                  # prints the client commands
ssh -F ~/.local/state/agsshub-dev-hub/agsshub-dev-hub/ssh_config commhub.home id
docker compose -f dev-hub/compose.yaml down       # the hub and everything in it is gone
```

| Variable | Default | |
|---|---|---|
| `DEV_HUB_PORT` | `2422` | host port; set but empty = any free port |
| `DEV_HUB_BIND` | `127.0.0.1` | host address the port is published on |
| `DEV_HUB_NETWORK` | `agsshub-dev` | Docker network; on it the hub is `agsshub-dev-hub:2422` |
| `DEV_HUB_IMAGE` | `agsshub-dev-hub:latest` | |
| `COMPOSE_PROJECT_NAME` | `agsshub-dev-hub` | a second name runs a second hub beside the first |
| `DEV_HUB_HUBD_ARGS` | | more flags for `agsshub-hubd`; `test/dev-hub.sh` sets `-reap-interval 1s` |

The entrypoint makes the host keys on first start and runs the daemon as `hub` (`-v`: one log line per
connection and per command, never message content).

## setup.sh

```
dev-hub/setup.sh [--out DIR] [--ws WS] [--orgs ORG,...] [--host H] [--port P] [IDENTITY ...]
```

| | Default |
|---|---|
| `--out` | `${XDG_STATE_HOME:-~/.local/state}/agsshub-dev-hub/<compose project>` |
| `--ws`, `--orgs` | `ws-dev`, enrolled for `home,expa` |
| `--host`, `--port` | the published port |
| identities | `commhub@home.ssh group.demo@expa.ssh` |

It enrolls the test workspace with its own registrar key, mints each identity — an operator role
with `hub-mint`, a `run` through the workspace's registrar over ssh — pins the hub's host key,
writes the client files, and logs every identity in once. Keys already in `--out` are reused, so a
re-run renews. A container recreated by `up` has an empty registry and a new host key: run it again.

| File in `--out` | |
|---|---|
| `ssh_config` | `Host` per identity (alias = identity with `@`→`.`, `/`→`.`, no `.ssh`: `commhub.home`, `group.demo.expa`) and `dev-hub-reg` for the registrar; published host and port |
| `ssh_config.net` | the same, for a container on the hub's network: `agsshub-dev-hub:2422` |
| `known_hosts` | the hub's host key under `HostKeyAlias agsshub-dev-hub` |
| `keys/<identity>`, `keys/registrar` | ed25519 keys, mode 0600 |
| `identities` | `identity<TAB>alias<TAB>key` per line |
| `env` | `DEV_HUB_HOST`, `DEV_HUB_PORT`, `DEV_HUB_NETWORK`, `DEV_HUB_CONTAINER`, `DEV_HUB_KNOWN_HOSTS`, … — shell syntax |

## Client commands

```sh
C=~/.local/state/agsshub-dev-hub/agsshub-dev-hub
ssh -F $C/ssh_config commhub.home send group.demo@expa.ssh -m:rfc5322 --id <msgid> < msg.eml
ssh -F $C/ssh_config group.demo.expa recv
ssh -F $C/ssh_config dev-hub-reg register run.<project>+<id>@expa.ssh --ttl 3600 < key.pub

# without a config file
ssh -p 2422 -i $C/keys/commhub@home.ssh -o IdentitiesOnly=yes -o BatchMode=yes \
    -o HostKeyAlias=agsshub-dev-hub -o UserKnownHostsFile=$C/known_hosts -o StrictHostKeyChecking=yes \
    hub@127.0.0.1 id

# from a container on the hub's network, with $C mounted at the same path
ssh -F $C/ssh_config.net commhub.home id
```

Keys are 0600 and owned by whoever ran `setup.sh`; a container that runs ssh as another user needs
its own copy.

Operator tools run in the container as root:

```sh
docker compose -f dev-hub/compose.yaml exec -T hub hub-mint add <identity> < key.pub
docker compose -f dev-hub/compose.yaml exec -T hub hub-mint add group.demo@expa.ssh --instances < key.pub   # same key: takes instances
docker compose -f dev-hub/compose.yaml exec -T hub hub-budget set group --rate 20 --burst 200
docker compose -f dev-hub/compose.yaml exec -T hub hub-topic add topic+events@expa.ssh --publish 'commhub@home.ssh'
docker compose -f dev-hub/compose.yaml logs hub            # one line per connection and per command
```

## Against a hub VM

| | Hub VM | Dev hub |
|---|---|---|
| install | `install.sh` | `install.sh`, at image build |
| init | systemd, `agsshub-hubd.service` | entrypoint: host keys, the daemon as `hub` under tini |
| port | `-listen`, default 2422 | 2422 |
| host key | `/etc/hub/ssh_host_ed25519_key`, the machine's sshd key when there is one | generated when the container first starts |
| state | `/var/lib/hub` | inside the container |
| distribution | Ubuntu 26.04 (template 911) | `ubuntu:26.04` |

No bind mounts, no added capabilities, no host network, no port below 1024 — nothing that differs
between rootful and rootless Docker. Tested on rootful Docker 29.7.2 with Compose v5.5.0.

## Test

`test/dev-hub.sh` builds the daemon and the image, starts it under a throwaway compose project on a free
loopback port, runs `setup.sh`, and drives the hub through ssh: identities and the registrar, operator
minting and rebinding, messages, files, chunked sends, flood budgets, chat, topics,
following one's own mailbox (wake latency, a client that vanishes and reconnects, a rebind), live sends,
instances (registered by their group, a new key evicting the old, release, the reaper) and admitting them
over ssh (the wake notice on the group's follow, a held send answered by `admit`, `reject`, the ask timeout,
10 concurrent senders on one ask, flow C's `retry-after` and `stay-up-till`, `--live` to a followed instance),
event ids and the position notice, the reaper, a second `install.sh` run with the event-id upgrade of a
subscription mailbox. It removes its container,
network and image on exit.
