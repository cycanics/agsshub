# Identities

## Grammar

```
<role>[.<project>][+<instance>][/<ws>]@<org>.ssh
```

Everything concrete lives in the local part; the domain is the Gitea **org** plus the fixed
`.ssh` suffix. So an identity is also a legal e-mail address, and the same string is the
account's address in Gitea.

| Part | Rule |
|---|---|
| local | up to the first `.` is the **role** · between the first `.` and `+`/`/` is the **project** · after `+` is the **instance** · after `/` is the **workspace** |
| domain | everything before the trailing `.ssh` is the **org** |

Project and org may both contain dots; the parse stays unambiguous because the role never does,
the instance is delimited by `+`, the workspace by `/`, and the org is simply the domain minus one
fixed `.ssh` suffix.

Why not put the workspace in the domain: a Gitea org may itself contain dots (`ws.acme.home` is a
valid org), so `<ws>.<org>.ssh` could not be split back without reserving a name prefix. Why the
`.ssh` suffix at all: Gitea rejects a single-label domain, so a bare `@<org>` is not a usable
e-mail address. `+` cannot be used for the workspace because it would then be indistinguishable
from an instance.

**On disk**, an identity is written with `/` replaced by `~` (`fs_name` in `lib/common.sh`) —
a `/` in a claim or mailbox name would otherwise be a path separator. `~` occurs in no identity
part, so the mapping is unambiguous.

## Slots

| Slot | Values | Grammar |
|---|---|---|
| `role` | one of the roles below; any other is not an identity | `[a-z][a-z0-9-]{0,31}` |
| `project` | a repository name in that org; for `group`, the group's name | `[a-z0-9_-][a-z0-9._-]{0,63}`, no `..` |
| `instance` | short id: a run's, or an instance's of an address that takes them (e.g. the first 8 hex digits of a session UUID); for `publisher`, the app | `[a-z0-9][a-z0-9-]{0,31}` |
| `org` | `home` · `acme` · `beta` · `gamma` · … — a Gitea org; always present | `[a-z0-9][a-z0-9._-]{0,63}`, no `..` |
| `ws` | `ws-a` · `ws-b` · … — the workspace VM; only on a pinned role | `ws-[a-z0-9][a-z0-9-]{0,30}` |

Whether `project` and `instance` are required, optional or absent, and whether `/<ws>` is
present, is decided by the role.

## Roles

| Role | Is | Shape | Project | Instance | Lifetime | Minted by | In: can receive | Out: can send |
|---|---|---|---|---|---|---|---|---|
| `main` | a workspace's agent for one org: receives what is addressed to it, dispatches runs | pinned | — | — | permanent | operator, at enrolment | yes | yes |
| `run` | one execution of a charter | pinned | optional | required | released at exit; TTL as backstop | its workspace, at launch | yes | yes |
| `foreman` | org supervisor | ws-less | — | — | permanent | operator | yes | yes |
| `frontline` | org's external face | ws-less | — | — | permanent | operator | yes | yes |
| `vmmanager`, `inference` | fleet services | pinned | — | — | permanent | operator | yes | yes |
| `group` | a group the operator names: an agent group, a set of sessions | ws-less | required: the group's name | — | permanent | operator | yes | yes |
| `commhub` | the comms hub | ws-less | — | — | permanent | operator | yes | yes |
| `publisher` | an app's publishing identity in one org, held by that app's bridge: one per org per app | ws-less | — | required: the app | permanent | operator | no | yes: to topics only |
| `svc` | a service (git-mirror, …): what a `group` may do, named apart from agent groups | ws-less | required: the service's name | — | permanent | operator | yes | yes |
| `topic` | the hub's own address: what is sent to it reaches its subscriptions — see [topics.md](topics.md) | ws-less | optional | required | until the operator removes it | the hub; **never claimable** | from the publishers its policy allows | no |
| `sub` | the hub's own address: one subscription to a topic | ws-less | required | required | until unsubscribed or expired | the hub; **never claimable** | only from its topic | no |

`run` is the only role minted automatically. Everything else is an operator act, which is what keeps
the identity space from growing on its own. `group` follows that rule: like `foreman` and
`frontline` it is permanent and ws-less.

An operator-minted address minted `--instances` also takes `+<instance>`: see *Instances*. A
publisher's `+<instance>` is its name (`publisher+gitea@home.ssh`), not an instance.

The table is `ROLES` in `lib/common.sh` and `Roles` in `go/hub/ident.go`; a role is added in both (a
test compares them).

### In and out

What each role can receive and send is the hub's to enforce, so it holds for a role's key in any
ssh client:

| | |
|---|---|
| a role that receives nothing (`publisher`) | has no mailbox, and owns no subscription. A send to it is refused, exit 3 (`… is a publisher: it receives nothing`) — `send`, `send_bag`; `send --live` and `chat` to it exit 9 ([live-send.md](live-send.md#invocation)). Its own sessions run `send`, `send_bag`, `id` and `help` (the hub's own, or a topic's): every other verb of the hub exits 3 (`… it receives nothing, so <verb> is not for it`); `send --live` and `chat` from it exit 9, as to it; an unknown verb is a usage error, exit 2, as for anyone |
| a publisher | sends to topics only, in every send form, under the topic's publish policy like any publisher. To an identity, a subscription or an alias: exit 3 (`… is a publisher: it sends to topics only, and <target> is not one`) — unless the request is refused before its target is judged, exit 2, as anyone's would be: a `Gap` field outside a send to a topic, `send_bag` to an alias |
| a role that sends nothing (`topic`, `sub`) | is never logged in; were a claim for one made by hand, its `send`, `send_bag` and `chat` would exit 3 |

## Examples

```
main/ws-a@acme.ssh                      workspace agent for org acme on ws-a
main/ws-a@home.ssh                 the same VM, a different org, a separate identity
run.myproj+a1b2c3/ws-a@home.ssh    a run: project myproj, instance a1b2c3
foreman@beta.ssh                        the org's supervisor, wherever it runs
frontline@beta.ssh
vmmanager/ws-sys@home.ssh
group.support@expa.ssh                   group "support" in org expa, on whichever host holds its key
group.support+a1b2c3d4@expa.ssh          an instance of it, when it takes instances
commhub@home.ssh                     the comms hub
publisher+gitea@home.ssh             Gitea's publishing identity in org home (its bridge's key)
svc.git-mirror@home.ssh              the git-mirror service
```

## Who mints

| Minter | Roles | How |
|---|---|---|
| a workspace | `run` | `register` through that workspace's registrar key (`reg@hub`). The registrar appends its own `/<ws>`; a `/<ws>` the client writes must be that workspace. The org must be one the workspace is enrolled for. |
| the operator | `main`, `foreman`, `frontline`, `vmmanager`, `inference`, `group`, `commhub`, `publisher`, `svc` | `hub-mint` on the hub, as root. A pinned identity names its workspace (`main/ws-a@acme.ssh`), which must be enrolled for the org; a ws-less one needs no workspace at all. |
| the hub | `topic`, `sub` | never claimed; the hub answers for them |
| an address's holder | the instances of an address minted `--instances` | `ssh hub register <instance> < key.pub` — see *Instances* |

A registrar refuses an operator role (exit 3, naming `hub-mint`) and a hub role; `hub-mint`
refuses a workspace role and a hub role.

An operator-minted identity is bound to one key, not to a host. Whoever holds the key connects as
the identity, so a group moves between hosts with its key — or the operator rebinds the
identity to the new host's key (`hub-mint add … --replace`), which evicts the old key in the same
step. Eviction covers connections already open: every command re-checks the claim its session was
admitted with, so a held-open connection of the old key (a ControlMaster) is refused (exit 3) from
the next command on, and its follow ends. The mailbox does not move in either case.

## The mailbox is the address

**The identity is exactly what was registered, and its mailbox is `inbox/<identity>/`.** The
minter of a claim is provenance in the claim record, never part of the destination.

That is what makes a role portable: `foreman@beta.ssh` keeps its mailbox when the foreman moves to
another host, and mail sent while its claim has lapsed is drained by whoever renews it. Rewriting
the destination to the current holder would strand that mail in a box nobody claims.

| Shape | Example | Why |
|---|---|---|
| **ws-less** — one per org | `foreman@beta.ssh`, `group.support@expa.ssh` | the role is the address; where it runs is provenance |
| **ws-pinned** — one per org per workspace | `main/ws-a@acme.ssh`, `run.myproj+a1b2c3/ws-a@home.ssh` | there is one per machine, so the machine is part of the identity |

A ws-pinned name may only be minted by the workspace it pins, or by the operator for a workspace
enrolled for its org — a host cannot mint an identity that claims to live somewhere else.

| Claim record | Send | Connect |
|---|---|---|
| live | delivered | admitted |
| lapsed (expired, not yet reaped — `persistent` and operator-minted claims are never reaped) | delivered | refused |
| none — released, or reaped | unknown target (exit 3) | refused |

**Split brain:** two live claims for one identity is refused rather than resolved — at claim time,
when a live claim for it exists under another minter, and at send time, when a target has more than
one. Picking one silently is how two foremen end up answering the same question.

## Instances

An address minted `hub-mint add <address> --instances` takes instances: the address with
`+<instance>` added to its name. Nothing else about the address changes.

```
group.support@expa.ssh            the address
group.support+a1b2c3d4@expa.ssh   one of its instances
main+a1b2c3d4/ws-a@acme.ssh      an instance of a pinned address: +<instance> stands before /<ws>
```

| Rule | |
|---|---|
| which addresses | any operator-minted identity but a publisher (`hub-mint --instances` refuses it, exit 3: its `+<app>` is its name); the flag stays with its claim through renewals and rebinds; `--no-instances` removes it |
| instance id | `[a-z0-9][a-z0-9-]{0,31}` |
| one level | an instance form is never minted (`hub-mint`: exit 3) and takes no instances itself |
| any other address | its instance form is no target (exit 3; `send --live`: 9), and nobody registers it (exit 3) |

### Registering an instance

Only the address's holder registers its instances, over its own connection, with the instance's
public key on stdin; each start or resume registers a fresh key.

```sh
ssh hub register a1b2c3d4 --ttl 3600 < run.pub   # → registered group.support+a1b2c3d4@expa.ssh fpr=… serial=… expires=…
ssh hub release a1b2c3d4                         # the address drops it; an instance runs `release` for itself
ssh hub instances
```

| | |
|---|---|
| `<instance>` | the id, or the whole instance identity |
| lifetime | `--ttl` 60 s … 1 day, default 1 hour; the reaper removes the claim at expiry |
| the same key again | renews; serial unchanged |
| another key | takes over at once: the previous key is refused from its next command (exit 3) and at login, and its follow ends |
| serial | from one counter for every instance |
| the claim ends | at `release`, `reject` or expiry, and when the address is rebound to another key, released, loses `--instances`, or its own claim lapses |
| keys | one key, one identity, across every claim |
| mailbox | `inbox/<instance>/`, made by `admit`, by a `register` answering an ask, and by the instance's own connect; kept when the claim ends: mail waits, and the address's wake notice lists it. `reject` removes it. It exists exactly when that directory exists, an empty one included: the hub's only test |
| flood budget | the instance's own, else its address's, else its role's, else the default |
| `register` | answers its pending ask as `admit`. A follow of the instance starting ends the ask the same way |
| exit | `0` ok · `1` the key is bound to another identity, or the registry is busy · `2` malformed id, key or `--ttl` · `3` not allowed: the caller takes no instances, is an instance, or names another address's instance; `release` of an instance without a claim |

`instances` lists each instance with a claim, a follower or a mailbox (empty ones too), one per line;
`(no instances)` when none; exit 2 with an argument; exit 3 for an address that takes none.

```
<instance identity>	mail=<n>	holder=live|none	claim=live|lapsed|none
```

### Admitting and rejecting an instance

```sh
ssh hub admit <instance> [<instance> …]    # as the address; an ask, or any instance at any time
ssh hub reject <instance> [<instance> …]   # as the address; an ask, or any instance at any time
```

| # | `send`, `send_bag` to an instance | |
|---|---|---|
| A | its mailbox present, no `--wait` | stored, exit 0, at once; the address's wake notice lists it until the instance follows its mailbox |
| A1 | its mailbox present, `--wait`, followed | stored, then held for the pickup: exit 0, or exit 8 after `<s>`, the message stored ([wake-live-event-id.md](wake-live-event-id.md#wait-for-pickup)) |
| A2 | no mailbox; or `--wait`, its mailbox present, not followed | the hub takes the sender's flood token, holds the sender and adds an ask entry; the address's wake notice shows it (`ask=`, [live-send.md](live-send.md#wake-notice)). `admit` → the mailbox made if absent, the message stored, exit 0 (with `--wait`: picked, or exit 8 after `<s>`). `reject` → exit 3, nothing stored. No answer within `wake_secs` → the mailbox looked at then: present, the message stored, exit 8; absent, exit 6, `retry-after=<live_retry_secs>` on stderr, nothing stored |
| a `reject` during `--wait` | a sender waiting on its pickup when `reject` lands exits 3 |

`send --live` or `chat` to an instance nobody follows: [live-send.md](live-send.md#instances-asleep).

| The ask | |
|---|---|
| order | admission comes before the wake notice: an ask is answered before any mail line exists for that instance |
| several senders | one ask entry, `send`, `--live` and `chat` alike; a sender that arrives while it is pending joins it; every held sender gets the one answer, or the one timeout |
| cap | at most 256 ask entries per address; a send that would make one more: exit 5, rate limited |
| the address not following | the ask holds all the same, up to `wake_secs`: an address that connects within it sees the ask in its wake notice |
| the entry | in memory only; gone with the answer, at `wake_secs`, with `register` or the instance's follow starting, or when its last held sender disconnects. A hub restart loses it, and its senders' sessions end with the hub; nothing stored |
| `chat` | held and asked as `send --live`: admission comes before the wake ([live-send.md](live-send.md#instances-asleep)) |

| `admit`, `reject` | |
|---|---|
| who | the holder of the address, over its own connection, for its own instances; one or many per call |
| `admit` | makes the mailbox if absent, answers a pending ask; held `send` / `send_bag`: stored, exit 0 (with `--wait`: picked or 8); held `--live` and `chat`: flow C. `admitted <instance>` |
| `reject` | "no such instance". Under the registry lock then the admission lock it ends the claim if there is one and answers a pending ask (held senders: exit 3); then it removes the mailbox if there is one: under its mailbox lock, renamed away, then removed. Its mail is discarded, with no expiry notice; the journal says `reject <instance> by <address> discarded=<n>`. A sender waiting on its pickup exits 3. With nothing to do: exit 0, `rejected <instance>`. The hub keeps nothing about a refused id |
| `reject` whose mailbox cannot be removed | exit 1; the claim is ended and the ask answered, the mailbox and its mail stay, nothing reported as discarded. `reject` again removes it |
| ordering | earlier exit 0s stand; there is no ordering promise between a send and a `reject`. A store that loses to a `reject` exits 3 and makes no mailbox |

| On the hub | |
|---|---|
| admission lock | an in-memory mutex per instance (a refcounted map, no file): one decision for that instance at a time — mailbox? followed? — and the mkdir, for a send, an ask, `admit`, `reject`, `register` and the start of the instance's follow. The registry lock, when taken, comes first |
| a held sender | waits outside the admission lock; when answered, it checks the mailbox again (a `reject` landed meanwhile: exit 3) |
| the store | staged outside the mailbox (`inbox/.stage.*`), then linked in under the mailbox lock after the mailbox is checked again; the mailbox gone: the stage removed, exit 3. Never under the admission or registry lock |
| the one mailbox maker | `instBox` (`go/hub/admission.go`), under the admission lock: used by `admit`, by `register` answering an ask, and by the instance's connect, which re-checks its claim under the lock so a connect during a `reject` cannot recreate the mailbox |
| timeouts | `wake_secs` (default 60) and `live_retry_secs` (default 5) in `/var/lib/hub/settings` ([live-send.md](live-send.md#on-the-hub)) |

## What the hub enforces

- **the role table** — an identity's role is one of the table's, with the project, instance and
  shape the table gives it
- **in and out** — a role that receives nothing has no mailbox and is sent nothing; a publisher
  sends to topics only (*In and out*)
- **who mints** — a registrar mints only workspace roles, `hub-mint` only operator roles; hub roles
  are claimed by nobody
- **the `/<ws>` part** — appended by the registrar from its own argv, never chosen by the client; a
  host can mint only names pinned to its own workspace
- **the org** — checked at claim time against the list of orgs that workspace is enrolled for, so
  `ws-a` cannot claim `@beta.ssh` unless it is enrolled for beta
- **one live claim per identity** — split brain is refused at claim time and at send time
- **one key, one identity** — a key bound to one identity cannot claim another
- **first-come ownership** of a registrar claim, with a tombstone window against reclaim by a
  different key
- **instances** — only for an address minted `--instances`, registered only by its holder, and
  admitted only while that address is live, still takes instances, and is bound to the key whose
  holder registered them

What it does not enforce, and must not be assumed to: **org separation between identities on the
same workspace.** One registrar key per machine means whoever holds it can mint any org that
machine is enrolled for. Separation inside a workspace is the VM's property, never the hub's.

## Implementation

| | Where |
|---|---|
| the role table | `ROLES`, `role_row` / `role_get` in `lib/common.sh`; `Roles` in `go/hub/ident.go` |
| in and out | `receivesNothing`, `sendRefused` and `dispatch` in `go/hub` |
| the grammar, with per-part validation | `identProblem`, `idSplit` in `go/hub/ident.go`; `ident_problem`, `id_split` in `lib/common.sh`, for the operator tools |
| claims filed per minter as the identity, `/`→`~` on disk | `claimFind` in `go/hub/claims.go`; `claim_path`, `claim_find` in `lib/common.sh` |
| registering, with `/<ws>` appended | `go/hub/registrar.go` |
| operator minting | `bin/hub-mint` (root check) → `lib/mint` |
| split brain at claim time, one key per identity, one writer at a time | `go/hub/registrar.go`, `lockRegistry`; `claim_live_elsewhere`, `key_bound_elsewhere`, `claims_lock` in `lib/common.sh` |
| routing, split brain at send time | `resolveTarget` in `go/hub` |
| org checked at claim time against enrolment | `ws_may_claim_org`, list at `$HUB_ROOT/orgs/<ws>` written by `hub-enroll <ws> <org>[,<org>…]` |
| instances: the form, the address behind one, ending them | `instSplit`, `instCapable`, `instanceParentOK`, `instDrop` in `go/hub`; `inst_split`, `evict_instances` in `lib/common.sh`, for `hub-mint`; claims `claims/instance/<identity>`, keys `claims/.ibyfpr/<keyid>` |
| registering, releasing, listing, waking instances | `go/hub/instances.go` |

### Registering

```sh
# once per workspace, by the operator on the hub: which orgs it may claim
hub-enroll ws-a home,acme < registrar.pub

# operator roles, by the operator on the hub (each key is the identity's own public key)
hub-mint add main/ws-a@home.ssh < main.pub
hub-mint add foreman@home.ssh     < foreman.pub
hub-mint add group.support@expa.ssh   < group.pub
hub-mint add group.support@expa.ssh --instances < group.pub      # the same key: renewed, now takes instances
hub-mint add commhub@home.ssh     < commhub.pub
hub-mint add publisher+gitea@home.ssh < gitea-bridge.pub     # an app's bridge, in one org
hub-mint add group.support@expa.ssh --replace < other-host.pub   # rebind to another key
hub-mint rm  foreman@home.ssh
hub-mint list

# runs, through the workspace's registrar key
ssh reg@hub register run.myproj+a1b2c3@home.ssh --ttl 3600 < run.pub   # → run.myproj+a1b2c3/ws-a@home.ssh
ssh reg@hub release  run.myproj+a1b2c3@home.ssh
ssh reg@hub list
```

| | Registrar | `hub-mint` |
|---|---|---|
| lifetime | `--ttl` 60 s … 1 day, default 1 hour | `--ttl` ≥ 60 s, default 10 years |
| mode | `--mode connection\|session\|persistent`, default `session`; the reaper keeps a lapsed `persistent` claim for renewal | always `persistent` |
| same key again | renews; serial unchanged | renews; serial unchanged |
| different key | refused while the claim is live, and for 10 minutes after a release or reap | refused without `--replace`; with it, rebinds and bumps the serial |
| instances | — | `--instances` lets the address take them; kept through renewals and rebinds; `--no-instances` removes it |
| exit | `0` ok · `1` conflict (held, tombstoned, split brain, key bound) · `2` malformed · `3` not allowed (role, workspace, org) | the same; `3` also for an instance form |

### Not yet implemented

| | |
|---|---|
| automatic `run` minting and release | runs still speak as their workspace's identity — `fleet-ws` side |
| collecting mail for a run that has already exited | it sits until the reaper GCs it |
