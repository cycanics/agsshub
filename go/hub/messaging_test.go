package hub

// Messages, forms, receiving, files, dedup, hardening, flood budgets, aliases, GC, chat and the
// doorbell, chunked framing.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func twoRuns(t *testing.T) *th {
	x := newTH(t)
	x.reg("ws-a", "register run.t+alice@acme.ssh --ttl 600", x.key("alice").line)
	x.reg("ws-b", "register run.t+bob@acme.ssh --ttl 600", x.key("bob").line)
	x.reg("ws-c", "register run.t+cara@corp.ssh --ttl 600", x.key("cara").line)
	return x
}

func TestMessagesAndForms(t *testing.T) {
	x := twoRuns(t)
	x.dis(alice, "send "+bob+" -- hello-bob")
	lst := x.dis(bob, "recv --peek").out
	has(t, "recv shows From", lst, "From: "+alice)
	has(t, "recv shows the body", lst, "hello-bob")
	x.dis(bob, "recv")
	eq(t, "text consumed by plain recv", x.dis(bob, "recv --peek").out, empty)
	code(t, "unknown peer -> 3", x.dis(alice, "send run.t+ghost/ws-a@acme.ssh -- x"), 3)
	code(t, "empty message -> 2", x.dis(alice, "send "+bob), 2)
	code(t, "-m of only newlines is nothing to send", x.dis(alice, "send "+bob+" -m", "\n\n"), 2)

	// a file round-trips through the same-name folder
	blob := make([]byte, 200000)
	rand.Read(blob)
	x.dis(alice, "send "+bob+" --file p.bin", string(blob))
	mid := ids(x.dis(bob, "recv --peek").out)
	has(t, "the listing's header names the payload", x.dis(bob, "recv --peek").out, "Payload: file p.bin\n")
	got := x.dis(bob, "recv --id "+mid+" --peek:file").out
	eq(t, "file sha round-trip", sha256.Sum256([]byte(got)), sha256.Sum256(blob))
	eq(t, "a full listing carries the payload", sha256.Sum256([]byte(partOf(x.dis(bob, "recv").out, mid, "payload"))), sha256.Sum256(blob))
	eq(t, "message consumed by the full read", x.dis(bob, "recv --peek").out, empty)

	x.dis(alice, "send "+bob+" --id ID1 -- one")
	r := x.dis(alice, "send "+bob+" --id ID1 -- two")
	eq(t, "a resent id is dropped, saying so", r.out, "dup ID1 (dropped)\n")
	code(t, "and exits 0", r, 0)
	eq(t, "one copy", ids(x.dis(bob, "recv --peek").out), "ID1")
	x.dis(bob, "recv")

	code(t, "traversal filename rejected", x.dis(alice, "send "+bob+" --file ../evil"), 2)
	code(t, "a leading - in a file name refused", x.dis(alice, "send "+bob+" --file -evil.txt"), 2)
	code(t, "a leading _ in a file name taken", x.dis(alice, "send "+bob+" --id U1 --file _ok.txt", "x"), 0)
	x.dis(alice, "send "+bob+" --id H1 --file real.txt", "")
	code(t, "a bad msgid", x.dis(bob, "recv --id ../x --peek:file"), 2)
	code(t, "no such message", x.dis(bob, "recv --id H2 --peek:file"), 3)
	eq(t, "an empty file payload", x.dis(bob, "recv --id H1 --peek:file").code, 0)
	eq(t, "recv_ack", x.dis(bob, "recv_ack H1").out, "acked H1\n")

	// message forms
	x.dis(alice, "send "+bob+" --id F1 -- a    b")
	has(t, "inline keeps runs of spaces", x.dis(bob, "recv --peek").out, "a    b")
	x.dis(alice, "send "+bob+" --id F2 -m", "line one\nline two\n")
	has(t, "-m takes a multi-line body", x.dis(bob, "recv --peek").out, "line one\nline two\r\n")
	x.dis(alice, "send "+bob+" --id F3 -m:rfc5322", "Reply-To: foreman@beta.ssh\nRe: q-7f3a\n\nthe body\n")
	f3, _ := os.ReadFile(filepath.Join(x.state, "inbox", fsName(bob), "F3.msg"))
	has(t, "rfc5322 body arrives", x.dis(bob, "recv --peek").out, "the body")
	eq(t, "the stored message", string(f3[:len(f3)-trailerSize]), fmt.Sprintf("From: %s\nDate: %s\nSerial: 1\nReply-To: foreman@beta.ssh\nRe: q-7f3a\n\nthe body",
		alice, hdrGet(headerBlock(filepath.Join(x.state, "inbox", fsName(bob), "F3.msg")), "Date")))
	code(t, "unknown field refused", x.dis(alice, "send "+bob+" --id F4 -m:rfc5322", "X-Evil: 1\n\nbody\n"), 2)
	code(t, "hub-stamped field refused", x.dis(alice, "send "+bob+" --id F5 -m:rfc5322", "From: someone-else\n\nbody\n"), 2)
	code(t, "missing blank line refused", x.dis(alice, "send "+bob+" --id F6 -m:rfc5322", "Reply-To: x\nno blank line\n"), 2)
	code(t, "a CRLF header block is refused, as before", x.dis(alice, "send "+bob+" --id F6b -m:rfc5322", "Subject: x\r\n\r\nbody\r\n"), 2)
	x.dis(alice, "send "+bob+" --id F7 -m:rfc5322", "Reply-To: one\n  folded-continuation\n\nbody\n")
	f7, _ := os.ReadFile(filepath.Join(x.state, "inbox", fsName(bob), "F7.msg"))
	has(t, "folded continuation kept", string(f7), "Reply-To: one\n  folded-continuation\n")
	code(t, "a folded first line refused", x.dis(alice, "send "+bob+" --id F8 -m:rfc5322", " x\n\nbody\n"), 2)
	code(t, "two stdin modes refused", x.dis(alice, "send "+bob+" --id F9 -m -- also-inline"), 2)
	code(t, "a header block over 4096 bytes", x.dis(alice, "send "+bob+" --id F10 -m:rfc5322", "Subject: "+strings.Repeat("x", 4100)+"\n\nb\n"), 2)
	code(t, "a value-taking option with no value is a usage error", x.dis(alice, "send "+bob+" --id"), 2)
	code(t, "an unexpected arg", x.dis(alice, "send "+bob+" --frob"), 2)
	code(t, "a missing target", x.dis(alice, "send"), 2)
	x.dis(bob, "recv")

	// receiving
	x.dis(alice, "send "+bob+" --id R1 -m:rfc5322", "Reply-To: foreman@beta.ssh\nRe: q-7f3a\nSubject: ingest\n\nthe body\n")
	peek := x.dis(bob, "recv --peek").out
	has(t, "listing shows Reply-To", peek, "Reply-To: foreman@beta.ssh")
	has(t, "listing shows Re", peek, "Re: q-7f3a")
	has(t, "and Subject", peek, "Subject: ingest")
	raw := x.dis(bob, "recv --id R1 --peek:rfc5322").out
	has(t, "raw keeps the hub's From", raw, "From: "+alice)
	has(t, "raw keeps the client's field", raw, "Reply-To: foreman@beta.ssh")
	eq(t, "raw: header block, blank line, body; unframed", strings.HasSuffix(raw, "Subject: ingest\n\nthe body"), true)
	eq(t, "--peek:rfc5322 never consumes", ids(x.dis(bob, "recv --peek").out), "R1")
	code(t, "raw of a missing message -> 3", x.dis(bob, "recv --id NOPE --peek:rfc5322"), 3)
	r = x.dis(bob, "recv_ack NOPE")
	eq(t, "ack of a missing message: gone, exit 0", fmt.Sprint(r.code, " ", r.out), "0 gone NOPE\n")
	x.dis(alice, "send "+bob+" --id R2 --file f.txt", "data")
	eq(t, "plain recv consumes files too", ids(x.dis(bob, "recv").out)+"|"+ids(x.dis(bob, "recv --peek").out), "R1 R2|")
	eq(t, "an empty command is recv", x.dis(bob, "").out, empty)
	code(t, "an unknown verb", x.dis(bob, "frob"), 2)
}

func TestListingOrderAndLocale(t *testing.T) {
	x := twoRuns(t)
	for _, id := range []string{"m-2", "m-1", "ab", "a-z"} {
		x.dis(alice, "send "+bob+" --id "+id+" -- x")
	}
	eq(t, "a listing is in arrival order, not by msgid", ids(x.dis(bob, "recv --peek").out), "m-2 m-1 ab a-z")
	same := time.Now()
	for _, id := range []string{"m-2", "m-1", "ab", "a-z"} {
		os.Chtimes(filepath.Join(x.h.mbox(bob), id+"."+msgExt), same, same)
	}
	eq(t, "the same arrival stamp: msgid byte order", ids(x.dis(bob, "recv --peek").out), "a-z ab m-1 m-2")
	has(t, "recv <own identity> is your own mailbox", ids(x.dis(bob, "recv "+bob+" --peek").out), "m-1")
	code(t, "another identity is not your mailbox", x.dis(bob, "recv "+alice+" --peek"), 3)
}

func TestFloodBudgets(t *testing.T) {
	x := twoRuns(t)
	x.mustMint("commhub@home.ssh", "hubk")
	x.mustMint("group.demo@expa.ssh", "grpk")
	grp, hub := "group.demo@expa.ssh", "commhub@home.ssh"
	frozen := time.Now() // the buckets refill per second of the hub's clock: held still, a slow machine refills nothing
	x.h.now = func() time.Time { return frozen }
	budgets := filepath.Join(x.state, "budgets")
	reset := func() {
		os.RemoveAll(filepath.Join(x.state, "flood"))
		os.MkdirAll(filepath.Join(x.state, "flood"), 0o2770)
	}
	sends := func(from, to string, n int) (acc int) {
		for i := 0; i < n; i++ {
			if x.dis(from, fmt.Sprintf("send %s -- b%d", to, i)).code == 0 {
				acc++
			}
		}
		return
	}
	reset()
	if n := sends(grp, hub, 20); n < 12 || n > 13 {
		t.Errorf("no budget: the default burst of 12, got %d", n)
	}
	os.WriteFile(filepath.Join(budgets, "group"), []byte("subject=group\nrate=20\nburst=50\n"), 0o640)
	reset()
	eq(t, "a role budget lifts every identity of the role", sends(grp, hub, 40), 40)
	os.WriteFile(filepath.Join(budgets, grp), []byte("subject="+grp+"\nburst=3\n"), 0o640)
	reset()
	eq(t, "an identity's own budget beats its role's", sends(grp, hub, 6), 3)
	r := x.dis(grp, "send "+hub+" -- over")
	code(t, "over budget -> 5", r, 5)
	eq(t, "the refusal names the budget, a field left out being the default", r.err,
		"dispatch: rate limited (group.demo@expa.ssh -> commhub@home.ssh: 0.5/s, burst 3); slow down\n")
	reset()
	if n := sends(hub, grp, 20); n < 12 || n > 13 {
		t.Errorf("the budget is the sender's: the other way keeps the default, got %d", n)
	}
	os.Remove(filepath.Join(budgets, grp))
	os.WriteFile(filepath.Join(budgets, "group"), []byte("subject=group\nrate=3\nburst=10\n"), 0o640)
	reset()
	os.WriteFile(filepath.Join(x.state, "flood", grp+"__"+hub), []byte(fmt.Sprintf("tokens=0\nlast=%d\n", frozen.Unix()-1)), 0o660)
	eq(t, "the rate refills: 3/s for one second is 3 messages", sends(grp, hub, 3), 3)
	os.WriteFile(filepath.Join(budgets, "group"), []byte("subject=group\nrate=fast\nburst=-1\n"), 0o640)
	reset()
	if n := sends(grp, hub, 20); n < 12 || n > 13 {
		t.Errorf("a malformed budget is the default, got %d", n)
	}
	os.WriteFile(filepath.Join(budgets, "group"), []byte("subject=group\nrate=20\nburst=50\n"), 0o640)
	reset()
	eq(t, "rates are decimal messages/second", fmt.Sprintf("%d %d %d %s", must(rateMT("0.5")), must(rateMT("2")), must(rateMT("1.25")), mtRate(1250)), "500 2000 1250 1.25")
	if _, ok := rateMT("0"); ok {
		t.Error("a zero rate is a rate")
	}
	eq(t, "the role's budget", fmt.Sprint(x.h.budgetFor(grp)), "20000 50000")
}

func must(v int64, ok bool) int64 { return v }

func TestAliasesAndList(t *testing.T) {
	x := twoRuns(t)
	os.WriteFile(filepath.Join(x.state, "aliases", "carol"), []byte("type=email\naddr=carol@example.com\n"), 0o640)
	r := x.dis(alice, "send carol -- hi")
	code(t, "backend placeholder -> 40", r, 40)
	has(t, "saying so", r.err, "backend 'email' not implemented (target=carol addr=carol@example.com)")
	all := x.dis(alice, "list").out
	has(t, "list shows a native identity", all, alice+"\ttype=native")
	has(t, "list shows an alias with its type", all, "carol\ttype=email")
	eq(t, "list --type email filters", x.dis(alice, "list --type email").out, "carol\ttype=email\taddr=carol@example.com\n")
	lacks(t, "list --type native has no alias", x.dis(alice, "list --type native").out, "carol")
	// a backend plugin, when one is wired, gets the send
	plug := filepath.Join(x.dir, "lib", "backends", "email")
	os.MkdirAll(plug, 0o755)
	os.WriteFile(filepath.Join(plug, "send"), []byte("#!/bin/sh\necho \"to=$HUB_ADDR from=$HUB_FROM body=$HUB_BODY\"\nexit 7\n"), 0o755)
	x.h.cfg.Lib = filepath.Join(x.dir, "lib")
	r = x.dis(alice, "send carol -- via-plugin")
	eq(t, "the plugin's output and exit", fmt.Sprint(r.code, " ", r.out), "7 to=carol@example.com from="+alice+" body=via-plugin\n")
}

func TestReaperGC(t *testing.T) {
	x := twoRuns(t)
	x.dis(alice, "send "+bob+" -m", "stale")
	x.dis(alice, "send "+bob+" --file s.bin", "xx")
	old := time.Now().Add(-30 * 24 * time.Hour)
	box := filepath.Join(x.state, "inbox", fsName(bob))
	entries, _ := os.ReadDir(box)
	for _, e := range entries {
		os.Chtimes(filepath.Join(box, e.Name()), old, old)
	}
	os.MkdirAll(filepath.Join(box, ".stage.1.1"), 0o700)
	os.Chtimes(filepath.Join(box, ".stage.1.1"), old, old)
	x.h.Reap()
	l := x.dis(bob, "recv --peek").out
	eq(t, "stale mail GC'd: a notice says so", ids(l), noticeID(l))
	has(t, "both counted", l, "\nExpired: 2\n")
	left, _ := os.ReadDir(box)
	eq(t, "no inbox residue but the notice", len(left), 1)
	eq(t, "(it)", left[0].Name(), ".expired")
}

func TestChunked(t *testing.T) {
	x := twoRuns(t)
	peek := func() string { return x.dis(bob, "recv --peek").out }
	ctext := []byte("line1\r\nline2\n")
	code(t, "chunked -m stored", x.dis(alice, "send "+bob+" --id C1 -m --chunked", string(frame(ctext, 5, ";x=1"))), 0)
	has(t, "chunked -m body decoded", peek(), "line2")
	x.dis(bob, "recv")
	blob := make([]byte, 200000)
	rand.Read(blob)
	x.dis(alice, "send "+bob+" --id C2 --file c.bin --chunked", string(frame(blob, 65000, ";x=1")))
	eq(t, "chunked binary file round-trip", bytes.Equal([]byte(x.dis(bob, "recv --id C2 --peek:file").out), blob), true)
	x.dis(alice, "send "+bob+" --id C3 -m:rfc5322 --chunked", string(frame([]byte("Subject: framed\n\nbody-r\n"), 7, "")))
	has(t, "chunked rfc5322 keeps fields", peek(), "Subject: framed")
	x.dis(bob, "recv")
	full := frame(ctext, 5, ";x=1")
	r := x.dis(alice, "send "+bob+" --id C4 -m --chunked", string(full[:len(full)-5]))
	code(t, "no last chunk -> 4", r, 4)
	eq(t, "saying so", r.err, "dispatch: chunked body ended before its last chunk\n")
	fb := frame(blob, 65000, ";x=1")
	code(t, "ends inside a chunk -> 4", x.dis(alice, "send "+bob+" --id C5 --file c.bin --chunked", string(fb[:100000])), 4)
	r = x.dis(alice, "send "+bob+" --id C6 -m --chunked", "5\r\nhello\r\n0\r\nAbort: auth\r\n\r\n")
	code(t, "Abort trailer -> 4", r, 4)
	eq(t, "saying so", r.err, "dispatch: sender aborted: auth\n")
	code(t, "bad size line -> 2", x.dis(alice, "send "+bob+" --id C7 -m --chunked", "zz\r\nhello\r\n0\r\n\r\n"), 2)
	code(t, "data not followed by CRLF -> 2", x.dis(alice, "send "+bob+" --id C7b -m --chunked", "5\r\nhelloXY0\r\n\r\n"), 2)
	code(t, "LF-only framing is accepted", x.dis(alice, "send "+bob+" --id C7c -m --chunked", "5\nhello\n0\n\n"), 0)
	x.dis(bob, "recv_ack C2 C7c")
	eq(t, "nothing incomplete was stored", peek(), empty)
	ents, _ := os.ReadDir(filepath.Join(x.state, "inbox"))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".chunked") {
			t.Error("a decode temp file was left")
		}
	}
	code(t, "chunked + inline refused", x.dis(alice, "send "+bob+" --chunked -- inline"), 2)
	tarb := makeTar(t, map[string]string{"m.md": "bagmsg\n", "d/f": "x\n"})
	x.dis(alice, "send_bag "+bob+" --msg m.md --id C8 --chunked", string(frame(tarb, 3000, "")))
	eq(t, "chunked bag: its body is the --msg member", bodyOf(x.dis(bob, "recv --id C8 --peek").out, "C8"), "bagmsg\n")
	code(t, "truncated chunked bag -> 4", x.dis(alice, "send_bag "+bob+" --msg m.md --id C9 --chunked", string(frame(tarb, 3000, "")[:4000])), 4)
}
