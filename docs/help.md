# Help

`help` tells a caller what the hub supports and what requests an address handles, including whether
each request is structured or text. The hub answers it. A client registers its description with the
hub through `register_description`; the hub validates it there, because that request is addressed to
the hub. Messages between clients stay unparsed, and `help` serves what was validated without parsing
it again.

## Invocation

```sh
ssh hub help [--json]                 # the hub's own verbs
ssh hub help <target> [--json]        # what this address handles
ssh hub register_description [--see <pattern>]... < descriptor.json
ssh hub register_description --clear
```

| Rule | |
|---|---|
| who may call `help` | every identity; a publisher only with no target or a topic, otherwise exit 3, since `list` is barred to it too |
| who may call `register_description` | an identity whose role receives; a publisher or an instance: exit 3 |
| cost of `help` | one file read: no flood token, no wake, nothing reaches the service |
| a hub without `help` | exit 2 with the usage line, as for any unknown verb. Exit 2 from `register_description` also means a refused description, so a service calls `help` first |

## Output format

| Answering for | `help` | `help --json` |
|---|---|---|
| a programmatic service: at least one request, all `structured` | JSON | JSON |
| a service with a `text` request, or with no requests | text | JSON |
| an address with no description | text | JSON, `descriptor: null` |
| the hub itself | text | JSON |

A program always passes `--json`: on exit 0, stdout is then exactly one compact JSON object and a
newline. On any other exit, stdout is empty and the reason is on stderr, in both formats. Which default
applies is decided once per view (*Scopes*), at registration, and stored with the description.

### JSON

```json
{"target":"commhub@home.ssh","type":"native","role":"commhub","receives":"yes","claim":"live","live":true,"instances":false,"described":"2026-11-05T21:30:00Z","descriptor":{"descriptor":1,"summary":"...","requests":[]}}
```

| Key | Value | Present for |
|---|---|---|
| `target` | the argument as given | all |
| `type` | `native`, `topic`, or an alias's type, as in `list` | all |
| `role` | the role | `native`, `topic` |
| `receives` | role table, `In`: `yes`, `no`, `policy` | `native`, `topic` |
| `claim` | `live`, `lapsed`, `none`: the wording of `instances` | `native` |
| `live` | its own mailbox is followed | `native` |
| `instances` | the address was minted `--instances` | `native`, not an instance |
| `address` | the address whose description this is | an instance |
| `publish`, `subscribe` | booleans: what the caller may do, as in `list` | `topic` |
| `described` | when the description was registered, or `null` | all |
| `descriptor` | the description's view for this target, or `null` | all |

`live: false` on an instance means asleep: `send --live` wakes it if its address is live. The hub
writes the keys in this order. Every value but `descriptor` comes from a fixed set or a checked grammar,
so it needs no escaping; `descriptor` is bytes stored at registration, already compacted.

### Text

```
Target: frontline@acme.ssh
Type: native
Role: frontline
Receives: yes
Claim: live
Live: yes
Instances: no
Described: 2026-11-05T21:30:00Z

Takes requests for this org.

(no subject)	text	spool	both	-	Anything to route or answer.

(no subject):
  Say what you need and by when.
```

The header block carries the same facts, with `Described: no` for `null`. Below it: the summary, a
`Version:` line when set, then one tab-separated line per request (subject, form, delivery joined by `,`
as registered, scope, files or `-`, summary), then each guide, indented two spaces per line. Schemas appear only in JSON.

Text output always says something after the header block, and these cases use fixed wording:

| Case | Below the header block |
|---|---|
| no description | `No description registered: this address has not said what requests it handles.` |
| no requests | the summary, then `Takes no requests.` |
| an alias | `An alias for delivery through <type>: it has no description.` |

### Targets

| Target | Answer |
|---|---|
| an identity | its facts and description |
| an instance | its own facts and the instance view of its address's description; never wakes it. As with `send`, any instance id under an address that takes instances resolves, with `claim: none` when unregistered |
| a topic | its facts and the description the operator set |
| an alias | its facts; `described` and `descriptor` are `null` |
| a subscription | refused, exit 2: help its topic |

Access is judged on the target, before anything about it is read. A caller without access gets exit 3
and an empty stdout, whether the target exists or not and whether it is described or not, so `help`
tells it nothing.

| Target | Who has access |
|---|---|
| an identity or instance | its own org, plus any `--see` pattern in its description |
| a topic | its own org, any `--see` pattern, and whoever its policy lets publish or subscribe |
| an alias | every identity but a publisher: an alias has no org, and `list` shows it |

| Exit | |
|---|---|
| 0 | answered |
| 1 | a hub fault: `help.json` missing, say |
| 2 | unexpected arguments, a target that is neither an identity nor an alias by grammar, or one whose role is `sub` |
| 3 | no access, no such target, or two live claims for it (one message for all three), or a publisher naming anything but a topic |

The order is arguments, then grammar and role (exit 2, nothing read), then a publisher's limit, then
access. Any failure while judging access counts as no access. `help` resolves its target without
`resolve_target`'s own stderr lines, which would tell apart what this table groups.

## The descriptor

JSON. `Subject` selects a request, `-m` or `-m:rfc5322` carries it, spool or `--live` delivers it.
Files travel with no `Subject`, because `--file` excludes `-m:rfc5322` and `send_bag` takes no fields.

```json
{
  "descriptor": 1,
  "version": "0.5",
  "summary": "Bridges chat platforms to agent addresses.",
  "requests": [
    {
      "subject": "out.deliver",
      "form": "structured",
      "delivery": ["live"],
      "summary": "Post a message to a conversation routed to the sender.",
      "guide": "Retry under the same --id: a repeat gets the original result.",
      "schema": {
        "type": "object",
        "required": ["conv", "message"]
      },
      "reply": {
        "content_type": "application/json",
        "statuses": [200, 400, 403, 404, 422, 500, 503]
      }
    }
  ]
}
```

A text request has no `schema`:

```json
{
  "subject": null,
  "form": "text",
  "delivery": ["spool"],
  "summary": "Anything to route or answer.",
  "guide": "Say what you need and by when."
}
```

| Key | Required | Meaning |
|---|---|---|
| `descriptor` | yes | format version, the literal `1` |
| `version` | no | the service's own version |
| `summary` | yes | what this service is |
| `live` | no | `false`: the hub refuses every `send --live` to this identity, and to its instances, with exit 9 ([live-send.md](live-send.md#outcomes)); absent or `true`: live supported |
| `requests` | yes | what it handles; `[]` = it takes no requests |
| `requests[].subject` | yes | the `Subject` that selects it; `null` = a message without one |
| `requests[].form` | yes | `structured` or `text` |
| `requests[].delivery` | yes | `spool`, `live`, or both |
| `requests[].summary` | yes | what it does |
| `requests[].schema` | `structured` only | JSON Schema of the body |
| `requests[].guide` | `text`; optional for `structured` | what to write, or how to use the request |
| `requests[].reply` | no | `live` only: `content_type`, `statuses`, optional `schema` |
| `requests[].files` | no | `file` or `bag`: the request carries files, sent with `--file` or `send_bag` |
| `requests[].scope` | no | `address`, `instance` or `both` (default): who handles it, on an address that takes instances |

| Form | The body is | Sent as |
|---|---|---|
| `structured` | JSON matching `schema`; a body that does not match is the service's to refuse (`400`/`422` when live) | `-m:rfc5322`, `Subject` set |
| `text` | prose for a person, an agent or a model to interpret | `-m`, or `-m:rfc5322` when `subject` is set |

A description says what a service handles, not what the caller is allowed to do: grants stay with the
service. It cannot express what a service emits, ordering across several sends, or a different body per
reply status beyond `reply.schema`; `guide` carries those in prose.

### Scopes

| Answering for | View | Requests shown |
|---|---|---|
| the address | whole | all of them, with `scope` as registered |
| an instance | instance | `instance` and `both` |

An address that takes no instances handles every request whatever its scope. An instance view with no
requests answers as any service with none. Each view has its own default format, by the rule in *Output
format*.

## Registering a description

A service runs `register_description` at every start, so the description follows the service's
version. It is a verb of its own, not JSON appended to the key on `register`'s stdin: services are
operator-minted, so their key goes through `hub-mint add`, which the operator runs once and the service
never does. Only `run` identities and instances pass through `register`, and an instance registers no
description.

| Check, at registration | Refused with exit 2 and the reason on stderr |
|---|---|
| size | empty, or over 64 KiB |
| encoding | not UTF-8, a BOM, or a lone surrogate escape |
| syntax | not exactly one JSON object; a number that overflows an IEEE 754 double; a repeated key; more than 32 nested containers |
| types | `descriptor` not the literal `1`; a key's value not of the type its row implies: `requests` and `delivery` arrays, `schema` and `reply` objects, `statuses` an array of integers, `live` a boolean, the rest strings (`subject` may be `null`) |
| keys | a missing key, or one the table above does not have, at the top, in a request, or in `reply` |
| one-line values | `summary`, `version`, `subject`, a request's `summary`, `reply.content_type`: empty, over 200 characters, outer spaces, or a control character (below 32, 127, 0x80–0x9F) |
| `guide` | empty, or a control character other than newline and tab |
| `form`, `delivery`, `files`, `scope` | outside their values; `delivery` empty or with a repeat |
| form | `structured` without `schema`, or with `subject: null`; `text` with `schema` or without `guide` |
| subjects | the same `subject` and `files` pair twice, anywhere in the description, so one `null` for prose and one for each kind of file are allowed |
| `reply` | on a request that is not `live`; a status outside 2xx, 4xx and 5xx, the only ones a reply can carry |
| `files` | with a `subject`, or on a `structured` request; `bag` on a `live` request |

The hub checks that `schema` is an object, not that it is a correct JSON Schema; that stays in the
service's own tests. Refused, the earlier description stays in place.

| Item | |
|---|---|
| access | `--see`, repeatable, at most 16: a pattern over the caller's identity, up to 128 characters of `[a-z0-9._+/@*?-]`; anything else, or `--see` with no value, is exit 2. The alphabet rules out extended globs, which can hang a reader and which the two hubs treat differently. It adds to the identity's current org, worked out when read, and cannot narrow it. Crossing an org line is an explicit grant, and `--clear` ends it |
| stdout | `described <identity> <date>`, or `cleared <identity>` |
| exit 1 | the registry is busy, or a hub fault; retry |
| `--clear` | removes the description; exit 0 when there was none |
| at service start | exit 3 can also mean the key was rebound since the connection opened; reconnect, as for any verb |
| a topic | `hub-topic register_description <topic> [--see <pattern>]... < descriptor.json`, same checks, and a `live` delivery, `live: true` and `scope` are refused because a topic takes no live send and no instances. `--clear` as above. Operators only, since a topic has no holder |

| Event | The description |
|---|---|
| claim lapsed | kept; `claim: lapsed` shows it |
| rebound by the operator (`--replace`) | kept: the address is the service, as with the mailbox |
| released, removed, reaped | removed |
| reclaimed by a different key after expiry (registrar) | removed: the new holder has not said this |
| `--no-instances` | kept; its instance forms exit 3 |
| an instance released, reaped or evicted | unaffected: its answer shows `claim: none` |
| topic removed; topic added again | removed with the topic's directory; `add` over an existing topic keeps it |

## The hub's own answer

`help` prints `Caller`, `Role`, `Receives` and `Sends` for the session's identity, then `help.txt`.
`help --json` prints the same four facts as keys, with `help.json` under `"hub"`:

```json
{"caller":"main/ws-a@acme.ssh","role":"main","receives":"yes","sends":"yes","hub":{"verbs":[{"verb":"recv","usage":"recv [<mailbox>] [--peek[:header|full|rfc5322|file|bag [PATH]]] [--limit BYTES] [--id ID... | --follow] [--header:stderr]","needs":["receive"],"summary":"List, read or follow a mailbox as a chunked stream; only a read without --peek consumes."}]}}
```

`needs` lists `receive` and `send` as a verb requires them, read against the caller's `receives` and
`sends`. Both files are static; `help.json` is compact, and a test holds it to that.

## On the hub

| Item | |
|---|---|
| description | `/var/lib/hub/described/<identity>` (`/` → `~`): `Date`, `See` (empty for the default), then in fixed order `Whole-Default`, `Whole-Json-Bytes`, `Whole-Text-Bytes`, `Instance-Default`, `Instance-Json-Bytes`, `Instance-Text-Bytes`, a blank line, then each view's compacted JSON and text rendering (no text when its default is `json`). hub:hub 2770 |
| writing | to a temp file, then rename, under the registry lock (`lockRegistry`), with the claim checked again inside it, so a write cannot outlive a `hub-mint rm` |
| reading | headers and body from one open file, so a reader sees one version |
| sweep | under the registry lock, the reaper removes descriptions with no claim, and old temp files |
| topic's description | `/var/lib/hub/topics/<topic>/descriptor`, the same format, written with `hub-descriptor --topic`: the whole view, and an empty instance view |
| hub's verbs | `lib/help.json`, `lib/help.txt`, installed to `/usr/local/lib/hub/`; read through `Config.Lib` |
| validator and renderer | one implementation, package `go/descriptor`: in-process in the hub, and `/usr/local/lib/hub/hub-descriptor` for `bin/hub-topic`. It takes `--see` and `--topic`, reads a descriptor on stdin and writes `See` and the views, or exits 2 |

A validator in jq does not hold up: jq accepts `01`, `.5`, `1.`, `+1`, `nan`, a BOM and invalid UTF-8.
`encoding/json` alone is not enough either, since it accepts invalid UTF-8, lone surrogates and repeated
keys, so the package adds `utf8.Valid` and a walk over the raw tokens.

## Tests

| Suite | Checks |
|---|---|
| `go/descriptor` | one fixture per row of the check table, under `test/descriptors/{good,bad}/`; the rendering of each text form; each view's requests and default format |
| `go/hub` | `--json` output parses strictly on every exit-0 path, and `live: true` |
| `go/hub` | default format per row of the format table; empty stdout on every refusal; each fixed wording, read from the rendered output; an instance served the instance view without a wake; `--see` grant and repeat; a caller without access gets identical stderr, stdout and exit for a described target, an undescribed one and a missing one; `--clear` ends a grant; a publisher's targets; split brain with the same message; a refused registration keeps the earlier description; each row of the event table; the write that loses to a `hub-mint rm`; the sweep |
| `go/hub` against `testdata/help-matrix.golden` | the bytes of every asker, target and form, with no follower, text and JSON; the refusals of `register_description` |
| `test/dev-hub.sh` | `help` and `register_description` through real ssh; `hub-topic register_description`, its `--clear` and refusals, and removing and re-adding a topic, which need root |
| completeness | every verb the hub takes is in `help.json` and `help.txt`, and no others |
