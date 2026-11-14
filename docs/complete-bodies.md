# Complete bodies

Status: built; `--file` without `--size` or `--chunked` still stores what came until `Subject` is enforced ([expiry-and-subject.md](expiry-and-subject.md#enforcement)).

A send's body counts only if it arrived whole and was stored. End of input alone does not say so: a cut connection and a crashed sender look like a clean end. And a store that failed must not be reported as stored. Without this, a retry of the same `--id` answers `dup`, and what was stored stays short or absent.

| Form | What shows it is whole | Missing or short |
|---|---|---|
| `send_bag` | tar's end marker: two zero blocks after the last member | exit 4, nothing stored |
| `--file <name>` | `--size <n>`, or `--chunked` | a mismatch: exit 4, nothing stored |
| `-m`, `-m:rfc5322` | `--size <n>`, or `--chunked` | a mismatch: exit 4, nothing stored. Without either, the hub stores what came |
| `-- <message>` | it is the command line: whole or absent | — |
| `--live` | always chunked | as `docs/live-send.md` |

| Takes effect | |
|---|---|
| now | the tar end marker; `--size` where given |
| once `Subject` is enforced ([expiry-and-subject.md](expiry-and-subject.md#enforcement)) | `--file` without `--size` or `--chunked`: exit 2 |

| Rule | |
|---|---|
| `--size` | bytes of all of stdin, `0` or `[1-9][0-9]{0,11}`. With `-m:rfc5322` that includes the header block. Exit 2: with `--chunked`, `--live`, `-- <message>` or `send_bag`; malformed or no value; over the form's cap (checked before reading); `0` with `-m` or `-m:rfc5322`, which have nothing to send |
| fewer bytes than `--size` | exit 4, nothing stored: a retry can fix it |
| more bytes than `--size` | exit 2: ssh adds no bytes, so a retry cannot fix it |
| where it is checked | once, at the top of every send, by copying up to n+1 bytes to an unlinked temp file (`sizedStdin`, beside `chunkedStdin`), before anything strips or parses. So it covers `-m`, `-m:rfc5322`, `--file`, a topic and an alias, and a cut inside a header block is 4, not 2 |
| flood token, wake | spent only after the body's completeness is known, on every form. So a cut send wakes nothing |
| the end marker | two zero blocks read after the last member. A reader that records whether the file's end was reached; after `tar.Reader.Next` returns `io.EOF`, reached means no marker. Last-bytes-zero and `Seek` are both wrong: a member may end in zeros |
| a tar without it, or cut inside a member or header | exit 4. Garbage that is not a tar: exit 2 |
| a retry after exit 4 | stores normally: nothing took the id |
| `dup` | only when the id exists (`fs.ErrExist`). Any other failure to link is not `dup` |
| a hub I/O error on any form: disk full, a failed copy, read, link or rename | exit 1, retryable, nothing reported as stored |
| a topic | stored means stored once for the fan-out: exit 1 when it cannot be. A subscription mailbox that cannot take its delivery after that is skipped, not the publisher's failure: exit 0, `subs=` counting the others ([topics.md](topics.md#publishing)) |
| exit 2 on `send_bag` | stays for what a retry cannot fix: garbage that is not a tar, over the size cap (stored bytes), `--msg` naming no regular member, an empty bag, a bad `--subject`. Member names, types, count and depth are not checked ([wake-live-event-id.md](wake-live-event-id.md#bags)) |

RFC 5322 has no end marker: a blank line ends the headers and the body runs to the end of input. That is why `-m:rfc5322` takes `--size` like `-m`. It is a flag rather than a `Content-Length` field, so every form works alike and the closed list of client fields stays closed.

## Tests

| Suite | Checks |
|---|---|
| `go/hub`, `test/full-disk.sh` | required: a full disk and no free inodes on `-m`, `--file`, `send_bag` and a topic, with no fsync on the path: exit 1, or exit 0 with what it says stored there (a topic: `subs=0` for a skipped mailbox, which a retry with the id reaches), never `dup`; a corpus of tars from GNU tar, Python, busybox, Go, a hand ustar, `git archive`, sparse and long names; a tar cut at a member boundary and mid-member, then a full retry that stores; a last member ending in zeros, cut after it; a tar with only one zero block; `--size` exact, short, long, on `--file`, `-m`, `-m:rfc5322`, on a topic; `--size` with `--chunked`, `--live`, `--` and on `send_bag`, malformed, leading zeros, over the cap, `0`, last argument; a NUL as the extra byte; a wake not fired by a cut send; nothing stored and no id taken on exit 4 |
| `test/dev-hub.sh` | a cut ssh stream through the real hub |
