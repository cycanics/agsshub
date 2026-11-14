package hub

// Reading (docs/wake-live-event-id.md, Reading): the chunked stream, its parts and final chunks, --id,
// --limit, --header:stderr, consumption, recv_ack, and the removed read forms.

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A listing is a valid chunked stream: every chunk ;id=<msgid>;part=<part>, a msg chunk per message
// (header block, blank line, body), payload chunks of at most 64 KiB, then the final chunk.
func TestReadStream(t *testing.T) {
	x := twoRuns(t)
	eq(t, "an empty listing: the final chunk alone", x.dis(bob, "recv").out, "0\r\n\r\n")
	x.dis(alice, "send "+bob+" --id a1 --subject note -- hello")
	big := strings.Repeat("p", 2*payloadChunk+17)
	x.dis(alice, "send "+bob+" --id a2 --file r.bin", big)
	l := x.dis(bob, "recv --peek:full").out
	eq(t, "valid, final 0", final(l), "")
	eq(t, "chunks in order", allIDs(l), "a1/msg a2/msg a2/payload a2/payload a2/payload")
	date := hdrGet(headerBlock(filepath.Join(x.h.mbox(bob), "a1.msg")), "Date")
	want := "From: " + alice + "\nDate: " + date + "\nSerial: 1\nSubject: note\n\nhello"
	eq(t, "a msg chunk: size, extensions, header block, blank line, body", strings.HasPrefix(l, fmt.Sprintf("%x;id=a1;part=msg\r\n%s\r\n", len(want), want)), true)
	var sizes []int
	for _, c := range chunks(l) {
		if c.part == "payload" {
			sizes = append(sizes, len(c.data))
		}
	}
	eq(t, "payload chunks of 64 KiB", fmt.Sprint(sizes), fmt.Sprint([]int{payloadChunk, payloadChunk, 17}))
	eq(t, "the payload as stored", partOf(l, "a2", "payload"), big)
	eq(t, "--peek: no payload chunks", allIDs(x.dis(bob, "recv --peek").out), "a1/msg a2/msg")
	eq(t, "--peek:header: the header block only", msgOf(x.dis(bob, "recv --peek:header").out, "a1"), "From: "+alice+"\nDate: "+date+"\nSerial: 1\nSubject: note\n\n")
	eq(t, "a full read: header, body and payload", allIDs(x.dis(bob, "recv").out), "a1/msg a2/msg a2/payload a2/payload a2/payload")
	eq(t, "and consumed", x.dis(bob, "recv").out, empty)
}

// The listing order: the wake notice, expiry notices, messages, a subscription's position notice. Every
// notice is a part=msg chunk; wakes-* and head-* are never consumed, expired-* by a full read.
func TestReadOrderAndNotices(t *testing.T) {
	x := instRig(t)
	x.dis(par, "admit a1")
	x.dis(wHub, "send "+inst("a1")+" --id i-1 -- x")
	x.dis(wHub, "send "+par+" --id m-1 --subject v -- old")
	x.age(par, "m-1")
	x.h.Reap()
	x.dis(wHub, "send "+par+" --id m-2 -- new")
	l := x.dis(par, "recv").out
	cs := chunks(l)
	if len(cs) != 3 {
		t.Fatalf("three entries: %q", l)
	}
	eq(t, "wake notice first", strings.HasPrefix(cs[0].id, WakePrefix) && cs[0].part == "msg", true)
	eq(t, "then the expiry notice", strings.HasPrefix(cs[1].id, "expired-") && cs[1].part == "msg", true)
	eq(t, "then the message", cs[2].id+"/"+cs[2].part, "m-2/msg")
	eq(t, "the full read consumed the expiry notice and the message, not the wake notice", allIDs(x.dis(par, "recv --peek").out), cs[0].id+"/msg")

	y, sb, _ := evRig(t)
	y.dis(alice, "send "+evTopic+" -- e1")
	y.dis(bob, "recv_ack "+sb+" "+ev(y.h.topicDone(evTopic).id, 1))
	l = y.dis(bob, "recv "+sb).out
	eq(t, "a subscription lacking done: the position notice last", allIDs(l), headMsgid(y.h.topicDone(evTopic))+"/msg")
	eq(t, "never consumed", allIDs(y.dis(bob, "recv "+sb).out), headMsgid(y.h.topicDone(evTopic))+"/msg")
}

// A follow's stdout is a valid chunk stream from its first byte, the notices sent on connect included:
// the notice_probe notice, the wake notice, expiry notices, LIVE announcements, the position notice. No
// plain line reaches stdout.
func TestFollowStreamFromFirstByte(t *testing.T) {
	x := instRig(t)
	x.setProbe(par)
	x.dis(par, "admit a1")
	x.dis(wHub, "send "+inst("a1")+" --id i-1 -- x")
	x.dis(wHub, "send "+par+" --id m-1 --subject v -- old")
	x.age(par, "m-1")
	x.h.Reap()
	x.dis(wHub, "send "+par+" --id m-2 -- new")
	f := x.start(x.adm(par), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the backlog", func() bool { return strings.Contains(f.out.String(), ";id=m-2;") })
	o := f.out.String()
	cs, fin, err := stream(o)
	if err != nil {
		t.Fatalf("not a chunk stream from its first byte: %v\n%q", err, o)
	}
	eq(t, "no final chunk while it runs", fin, "-")
	var got []string
	for _, c := range cs {
		got = append(got, strings.SplitN(c.id, "-", 2)[0]+"/"+c.part)
	}
	eq(t, "probe, wake notice, expiry notice, message: each a msg chunk", strings.Join(got, " "), "expired/msg wakes/msg expired/msg m/msg")
	f.cancel()
	f.wait(t, 2*time.Second)

	y, sb, _ := evRig(t)
	y.setProbe(bob)
	y.dis(alice, "send "+evTopic+" -- e1")
	y.dis(bob, "recv_ack "+sb+" "+ev(y.h.topicDone(evTopic).id, 1))
	g := y.start(y.adm(bob), "recv "+sb+" --follow", "")
	waitFor(t, 2*time.Second, "the position notice", func() bool { return strings.Contains(g.out.String(), ";id=head-") })
	cs, fin, err = stream(g.out.String())
	eq(t, "a subscription's follow: probe, then the position notice, valid", fmt.Sprint(err, " ", fin, " ", len(cs), " ", cs[len(cs)-1].id), "<nil> - 2 "+headMsgid(y.h.topicDone(evTopic)))
	y.dis(bob, "unsub "+sb)
	eq(t, "the subscription gone: exit 0", g.wait(t, 3*time.Second), 0)
	eq(t, "final chunk 0;end=gone", final(g.out.String()), ";end=gone")
}

// --id: the named messages in the order given; an id not held is a part=missing chunk in its place; exit 3
// only if none was found. A full --id read consumes what it streamed.
func TestReadByID(t *testing.T) {
	x := twoRuns(t)
	for _, id := range []string{"b1", "b2", "b3"} {
		x.dis(alice, "send "+bob+" --id "+id+" -- "+id)
	}
	r := x.dis(bob, "recv --id b3 --id nope --id b1 --peek")
	eq(t, "in the order given, missing in its place", fmt.Sprint(r.code, " ", allIDs(r.out), " ", final(r.out)), "0 b3/msg nope/missing b1/msg ")
	eq(t, "the missing chunk's data", partOf(r.out, "nope", "missing"), "Missing: nope\n")
	r = x.dis(bob, "recv --id nope --id gone")
	eq(t, "none found: exit 3, the stream still complete", fmt.Sprint(r.code, " ", allIDs(r.out), " ", final(r.out)), "3 nope/missing gone/missing ")
	x.dis(bob, "recv --id b2")
	eq(t, "a full --id read consumes", ids(x.dis(bob, "recv --peek").out), "b1 b3")
	code(t, "--id with --follow: 2", x.dis(bob, "recv --id b1 --follow"), 2)
	code(t, "a bad --id: 2", x.dis(bob, "recv --id a/b"), 2)
	code(t, "--id without a value: 2", x.dis(bob, "recv --id"), 2)
	code(t, "--peek:rfc5322 without --id: 2", x.dis(bob, "recv --peek:rfc5322"), 2)
	code(t, "--peek:rfc5322 with two: 2", x.dis(bob, "recv --peek:rfc5322 --id b1 --id b3"), 2)
	code(t, "--peek:rfc5322 with --follow: 2", x.dis(bob, "recv --peek:rfc5322 --follow"), 2)
	code(t, "--peek:bag without --id: 2", x.dis(bob, "recv --peek:bag"), 2)
	code(t, "an unknown content: 2", x.dis(bob, "recv --peek:body"), 2)
	code(t, "two --peek: 2", x.dis(bob, "recv --peek --peek:full"), 2)
	code(t, "a mailbox is told by its @: not yours, 3", x.dis(bob, "recv "+alice+" --id b1"), 3)
	eq(t, "nothing consumed by the refusals", ids(x.dis(bob, "recv --peek").out), "b1 b3")
}

// --limit <bytes>: stops at the first message boundary at or past it, final 0;limit; a larger message is
// still sent whole; only messages streamed whole are consumed.
func TestReadLimit(t *testing.T) {
	x := twoRuns(t)
	for _, id := range []string{"c1", "c2", "c3"} {
		x.dis(alice, "send "+bob+" --id "+id+" -- "+strings.Repeat("z", 100))
	}
	one := len(x.dis(bob, "recv --id c1 --peek").out) - len(empty)
	r := x.dis(bob, "recv --peek --limit 1")
	eq(t, "a message larger than the limit, whole; then 0;limit", fmt.Sprint(allIDs(r.out), " ", final(r.out)), "c1/msg ;limit")
	r = x.dis(bob, "recv --peek --limit "+fmt.Sprint(one+1))
	eq(t, "the boundary at or past the limit", fmt.Sprint(allIDs(r.out), " ", final(r.out)), "c1/msg c2/msg ;limit")
	r = x.dis(bob, "recv --peek --limit "+fmt.Sprint(one))
	eq(t, "exactly at the limit: stops there", fmt.Sprint(allIDs(r.out), " ", final(r.out)), "c1/msg ;limit")
	r = x.dis(bob, "recv --peek --limit 1000000")
	eq(t, "not reached: 0", final(r.out), "")
	r = x.dis(bob, "recv --limit "+fmt.Sprint(2*one))
	eq(t, "a full read with a limit", fmt.Sprint(allIDs(r.out), " ", final(r.out)), "c1/msg c2/msg ;limit")
	eq(t, "consumes only what it streamed", ids(x.dis(bob, "recv --peek").out), "c3")
	r = x.dis(bob, "recv --limit 1")
	eq(t, "the last one: no more waiting, final 0", fmt.Sprint(allIDs(r.out), " ", final(r.out)), "c3/msg ")
	for _, bad := range []string{"0", "-1", "x", "01", "1.5"} {
		code(t, "--limit "+bad+": 2", x.dis(bob, "recv --limit "+bad), 2)
	}
	code(t, "--limit with --id: 2", x.dis(bob, "recv --limit 9 --id c1"), 2)
	code(t, "--limit with --follow: 2", x.dis(bob, "recv --limit 9 --follow"), 2)
}

// seqLog records the order of writes to stdout and stderr.
type seqLog struct {
	mu  sync.Mutex
	ops []string
}

type seqW struct {
	l    *seqLog
	name string
	b    strings.Builder
}

func (w *seqW) Write(p []byte) (int, error) {
	w.l.mu.Lock()
	defer w.l.mu.Unlock()
	w.l.ops = append(w.l.ops, w.name+":"+string(p))
	w.b.Write(p)
	return len(p), nil
}

func (x *th) disSeq(me, cmd string) (out, errs string, ops []string, code int) {
	l := &seqLog{}
	o, e := &seqW{l: l, name: "out"}, &seqW{l: l, name: "err"}
	code = x.h.Dispatch(context.Background(), x.adm(me), cmd, IO{Stdin: strings.NewReader(""), Stdout: o, Stderr: e})
	return o.b.String(), e.b.String(), l.ops, code
}

// before2: the first stdout write containing a comes before the first stderr write starting with b.
func before2(ops []string, a, b string) bool {
	ia, ib := -1, -1
	for i, op := range ops {
		if ia < 0 && strings.HasPrefix(op, "out:") && strings.Contains(op, a) {
			ia = i
		}
		if ib < 0 && strings.HasPrefix(op, "err:"+b) {
			ib = i
		}
	}
	return ia >= 0 && ib >= 0 && ia < ib
}

// --header:stderr: chunked modes write a part=id chunk `Header: <msgid>\n` first, then the header block on
// stderr after the same line, then part=body and part=payload chunks; raw outputs write the stderr block
// first, then the raw bytes; --peek:rfc5322 refuses it.
func TestHeaderStderr(t *testing.T) {
	x := twoRuns(t)
	x.dis(alice, "send "+bob+" --id h1 --subject s -- body-one")
	x.dis(alice, "send "+bob+" --id h2 --file f.bin", "FILEDATA")
	bag := string(makeTar(t, map[string]string{"m.md": "bagmsg\n", "x": "X"}))
	x.dis(alice, "send_bag "+bob+" --id h3 --msg m.md", bag)
	hb := func(id string) string { return string(headOnly(filepath.Join(x.h.mbox(bob), id+".msg"))) }

	out, errs, ops, c := x.disSeq(bob, "recv --peek:full --header:stderr")
	eq(t, "exit 0, valid", fmt.Sprint(c, " ", final(out)), "0 ")
	eq(t, "chunks: id, body, payload per message", allIDs(out), "h1/id h1/body h2/id h2/payload h3/id h3/body h3/payload")
	eq(t, "the id chunk's data", partOf(out, "h1", "id"), "Header: h1\n")
	eq(t, "the body chunk", partOf(out, "h1", "body"), "body-one")
	eq(t, "the payload chunk", partOf(out, "h2", "payload"), "FILEDATA")
	eq(t, "stderr: per message the Header line, the header block, a blank line", errs, "Header: h1\n"+hb("h1")+"Header: h2\n"+hb("h2")+"Header: h3\n"+hb("h3"))
	eq(t, "no header on stdout", strings.Contains(out, "From: "), false)
	for _, id := range []string{"h1", "h2", "h3"} {
		eq(t, id+": the stdout id chunk before its stderr block", before2(ops, fmt.Sprintf("%x", len("Header: "+id+"\n"))+";id="+id+";part=id\r\nHeader: "+id+"\n\r\n", "Header: "+id+"\n"), true)
	}
	out, errs, _, _ = x.disSeq(bob, "recv --peek:header --header:stderr")
	eq(t, "--peek:header: the id chunk only", allIDs(out), "h1/id h2/id h3/id")
	eq(t, "(the headers on stderr)", strings.Count(errs, "Header: "), 3)
	out, _, _, _ = x.disSeq(bob, "recv --peek --header:stderr --id h3")
	eq(t, "--peek: id and body, no payload", allIDs(out), "h3/id h3/body")

	for _, cmd := range []string{"recv --id h2 --peek:file --header:stderr", "recv --id h3 --peek:bag x --header:stderr", "recv_bag --id h3 --peek --header:stderr"} {
		out, errs, ops, c = x.disSeq(bob, cmd)
		id := strings.Fields(cmd)[2]
		if strings.HasPrefix(cmd, "recv_bag") {
			id = "h3"
		}
		eq(t, cmd+": exit 0, the stderr block", fmt.Sprint(c, " ", errs), "0 Header: "+id+"\n"+hb(id))
		eq(t, cmd+": raw stdout", len(out) > 0 && !strings.Contains(out, ";part="), true)
		eq(t, cmd+": stderr first, then the raw bytes", len(ops) > 0 && strings.HasPrefix(ops[0], "err:Header: "+id), true)
	}
	code(t, "--peek:rfc5322 with --header:stderr: 2", x.dis(bob, "recv --id h1 --peek:rfc5322 --header:stderr"), 2)

	// a consuming read with --header:stderr loses nothing: recv_bag's header block on stderr
	h3 := hb("h3")
	r := x.dis(bob, "recv_bag --id h3 --header:stderr")
	eq(t, "recv_bag consumes; the header kept on stderr", fmt.Sprint(r.out == bag, " ", r.err == "Header: h3\n"+h3, " ", exists(filepath.Join(x.h.mbox(bob), "h3.msg"))), "true true false")
	// a follow
	h2 := hb("h2")
	f := x.start(x.adm(bob), "recv --follow --header:stderr", "")
	waitFor(t, 2*time.Second, "the follow", func() bool { return strings.Contains(f.out.String(), ";id=h2;part=payload") })
	eq(t, "a follow with --header:stderr", allIDs(f.out.String()), "h1/id h1/body h2/id h2/payload")
	has(t, "(its headers)", f.err.String(), "Header: h2\n"+h2)
	f.cancel()
	f.wait(t, 2*time.Second)
}

// Consumption: only a full read consumes, once the chunks are written; a write that fails consumes
// nothing.
func TestConsumption(t *testing.T) {
	x := twoRuns(t)
	x.dis(alice, "send "+bob+" --id k1 -- one")
	x.dis(alice, "send "+bob+" --id k2 --file f", "data")
	code(t, "a dropped client", result{code: x.h.Dispatch(context.Background(), x.adm(bob), "recv", IO{Stdin: strings.NewReader(""), Stdout: failingWriter{}, Stderr: io.Discard})}, 1)
	eq(t, "consumes nothing", ids(x.dis(bob, "recv --peek").out), "k1 k2")
	code(t, "--id, dropped", result{code: x.h.Dispatch(context.Background(), x.adm(bob), "recv --id k2", IO{Stdin: strings.NewReader(""), Stdout: failingWriter{}, Stderr: io.Discard})}, 1)
	code(t, "recv_bag, dropped", result{code: x.h.Dispatch(context.Background(), x.adm(bob), "recv_bag --id k2", IO{Stdin: strings.NewReader(""), Stdout: failingWriter{}, Stderr: io.Discard})}, 2)
	eq(t, "(still there)", ids(x.dis(bob, "recv --peek").out), "k1 k2")
	x.dis(bob, "recv")
	eq(t, "the full read consumed both", x.dis(bob, "recv --peek").out, empty)
	// a long listing commits every consumeBatch bytes: a cut after one keeps only what was not yet flushed
	for _, id := range []string{"L1", "L2", "L3"} {
		x.dis(alice, "send "+bob+" --id "+id+" --file f", strings.Repeat("y", 600<<10))
	}
	cut := &cutWriter{left: 1300 << 10}
	x.h.Dispatch(context.Background(), x.adm(bob), "recv", IO{Stdin: strings.NewReader(""), Stdout: cut, Stderr: io.Discard})
	eq(t, "cut past the first batch: its messages consumed, the rest kept", ids(x.dis(bob, "recv --peek").out), "L3")
	x.dis(bob, "recv")
	x.dis(alice, "send "+bob+" --id k3 -- three")
	f := x.start(x.adm(bob), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "k3", func() bool { return strings.Contains(f.out.String(), ";id=k3;") })
	x.dis(alice, "send "+bob+" --id k4 -- four")
	waitFor(t, 2*time.Second, "k4", func() bool { return strings.Contains(f.out.String(), ";id=k4;") })
	eq(t, "a --peek follow streams each once, consumes none", fmt.Sprint(ids(f.out.String()), " ", ids(x.dis(bob, "recv --peek").out)), "k3 k4 k3 k4")
	f.cancel()
	f.wait(t, 2*time.Second)
}

// recv_ack: ids as arguments or on stdin (-m:ids); acked, gone, refused; usage errors exit 2 before any id
// is acted on.
func TestRecvAck(t *testing.T) {
	x, sb, _ := evRig(t)
	for _, id := range []string{"q1", "q2", "q3"} {
		x.dis(alice, "send "+bob+" --id "+id+" -- x")
	}
	r := x.dis(bob, "recv_ack q1 q9 wakes-0000000000000000 head-none")
	eq(t, "one line per id", fmt.Sprint(r.code, " ", r.out), "0 acked q1\ngone q9\nrefused wakes-0000000000000000\nrefused head-none\n")
	r = x.dis(bob, "recv_ack -m:ids", "q2\n\nq3\r\nq2\n")
	eq(t, "-m:ids: one per line, blank lines skipped", fmt.Sprint(r.code, " ", r.out), "0 acked q2\nacked q3\ngone q2\n")
	x.dis(alice, "send "+bob+" --id q4 -- x")
	for _, c := range []struct{ what, cmd, in string }{
		{"a flag", "recv_ack q4 --peek", ""},
		{"a second mailbox", "recv_ack " + bob + " " + alice + " q4", ""},
		{"a malformed id, nothing acked", "recv_ack q4 ../x", ""},
		{"no id", "recv_ack", ""},
		{"-m:ids and arguments", "recv_ack -m:ids q4", ""},
		{"-m:ids over 1024", "recv_ack -m:ids", strings.Repeat("q4\n", 1025)},
		{"-m:ids with a malformed one", "recv_ack -m:ids", "q4\na b\n"},
	} {
		code(t, c.what+": 2", x.dis(bob, c.cmd, c.in), 2)
	}
	eq(t, "q4 survived every refusal", ids(x.dis(bob, "recv --peek").out), "q4")
	eq(t, "-m:ids at 1024: taken", x.dis(bob, "recv_ack -m:ids", strings.Repeat("q4\n", 1024)).code, 0)
	x.dis(alice, "send "+evTopic+" -- e")
	e := ev(x.h.topicDone(evTopic).id, 1)
	eq(t, "a subscription named first", x.dis(bob, "recv_ack "+sb+" -m:ids", e+"\n").out, "acked "+e+"\n")
	code(t, "another's subscription: 3", x.dis(alice, "recv_ack "+sb+" "+e), 3)
}

// recv_ack --before / --before-including (D33): what precedes the msgid in arrival order, the msgid too
// with -including. A subscription: by seq within a session, interleaved sessions as published.
func TestRecvAckBefore(t *testing.T) {
	x, sb, _ := evRig(t)
	for _, e := range []string{ev(idB, 9), ev(idB, 10), ev(idA, 2), ev(idB, 100), ev(idA, 11), ev(idA, 12)} {
		x.dis(alice, "send "+evTopic+" --id "+e+" -- x")
	}
	r := x.dis(bob, "recv_ack "+sb+" --before "+ev(idA, 11))
	eq(t, "--before: what arrived before, not the msgid", fmt.Sprint(r.code, "\n", r.out),
		"0\nacked "+ev(idB, 9)+"\nacked "+ev(idB, 10)+"\nacked "+ev(idA, 2)+"\nacked "+ev(idB, 100)+"\n")
	eq(t, "left", evs(x.dis(bob, "recv "+sb+" --peek").out), idA+"/11 "+idA+"/12")
	r = x.dis(bob, "recv_ack "+sb+" --before-including "+ev(idA, 11))
	eq(t, "--before-including: the msgid too", fmt.Sprint(r.code, "\n", r.out), "0\nacked "+ev(idA, 11)+"\n")
	eq(t, "left", evs(x.dis(bob, "recv "+sb+" --peek").out), idA+"/12")
	r = x.dis(bob, "recv_ack "+sb+" --before "+ev(idA, 12))
	eq(t, "nothing before: no lines, 0", fmt.Sprint(r.code, "|", r.out), "0|")

	// an own mailbox: arrival order too
	for _, id := range []string{"o3", "o1", "o4", "o2"} {
		x.dis(alice, "send "+bob+" --id "+id+" -- x")
	}
	eq(t, "own: --before", x.dis(bob, "recv_ack --before o4").out, "acked o3\nacked o1\n")
	eq(t, "own: --before-including", x.dis(bob, "recv_ack --before-including o4").out, "acked o4\n")
	eq(t, "left", ids(x.dis(bob, "recv --peek").out), "o2")

	// absent: exit 3, nothing acked
	r = x.dis(bob, "recv_ack --before-including o9")
	eq(t, "absent msgid: 3, nothing acked", fmt.Sprint(r.code, "|", r.out, "|", ids(x.dis(bob, "recv --peek").out)), "3||o2")
	code(t, "absent on a subscription: 3", x.dis(bob, "recv_ack "+sb+" --before "+ev(idA, 99)), 3)
	eq(t, "the subscription untouched", evs(x.dis(bob, "recv "+sb+" --peek").out), idA+"/12")

	// usage: 2, nothing acked
	for _, c := range []struct{ what, cmd, in string }{
		{"with ids", "recv_ack o2 --before o2", ""},
		{"ids after", "recv_ack --before o2 o2", ""},
		{"with -m:ids", "recv_ack -m:ids --before-including o2", "o2\n"},
		{"both forms", "recv_ack --before o2 --before-including o2", ""},
		{"no value", "recv_ack --before", ""},
		{"a malformed msgid", "recv_ack --before ../x", ""},
		{"a wake notice", "recv_ack --before-including wakes-0000000000000000", ""},
		{"a position notice", "recv_ack " + sb + " --before head-none", ""},
		{"an expiry notice", "recv_ack --before-including expired-0000000000000000-1", ""},
	} {
		code(t, c.what+": 2", x.dis(bob, c.cmd, c.in), 2)
	}
	eq(t, "o2 survived every refusal", ids(x.dis(bob, "recv --peek").out), "o2")
	code(t, "another's subscription: 3", x.dis(alice, "recv_ack "+sb+" --before-including "+ev(idA, 12)), 3)
}

// Notices are never in the range: wakes-*, head-* and expired-* stay (an expiry notice is acked by id
// only, and as the msgid exits 2); an instance's range ack nudges its address's wake notice.
func TestRecvAckBeforeNotices(t *testing.T) {
	x := instRig(t)
	x.dis(wHub, "send "+par+" --id m-1 --subject v -- old")
	x.age(par, "m-1")
	x.h.Reap()
	x.dis(wHub, "send "+par+" --id m-2 -- new")
	x.dis(wHub, "send "+par+" --id m-3 -- new")
	cs := chunks(x.dis(par, "recv --peek").out)
	if len(cs) != 4 || !strings.HasPrefix(cs[0].id, WakePrefix) || !strings.HasPrefix(cs[1].id, "expired-") {
		t.Fatalf("wake, expiry, m-2, m-3: %v", cs)
	}
	exp := cs[1].id
	r := x.dis(par, "recv_ack --before-including m-2")
	eq(t, "the message only: the wake and expiry notices stay", fmt.Sprint(r.code, "\n", r.out), "0\nacked m-2\n")
	left := chunks(x.dis(par, "recv --peek").out)
	eq(t, "left: the wake notice, the expiry notice, m-3", fmt.Sprint(len(left), " ", strings.HasPrefix(left[0].id, WakePrefix), " ", left[1].id, " ", left[2].id),
		"3 true "+exp+" m-3")
	code(t, "an expiry notice as the msgid: 2", x.dis(par, "recv_ack --before "+exp), 2)
	eq(t, "acked by id", x.dis(par, "recv_ack "+exp).out, "acked "+exp+"\n")

	// an instance's ack nudges its address's wake notice
	x.settings("wake_secs=30\nlive_retry_secs=0.25\n")
	f := x.start(x.adm(par), "recv --follow --peek")
	x.dis(par, "admit a1")
	ser, _ := x.register("a1", "a1k")
	x.dis(wHub, "send "+inst("a1")+" --id i-2 -- x")
	x.dis(wHub, "send "+inst("a1")+" --id i-1 -- x")
	waitFor(t, 2*time.Second, "mail for a1", func() bool { return lastWake(f.out.String()).n == "1" })
	eq(t, "the instance acks both, in arrival order", x.idis(inst("a1"), ser, "a1k", "recv_ack --before-including i-1").out, "acked i-2\nacked i-1\n")
	waitFor(t, 2*time.Second, "the ack nudges the wake notice", func() bool { return lastWake(f.out.String()).n == "0" })
	f.cancel()
	f.wait(t, 2*time.Second)

	y, sb, _ := evRig(t)
	y.dis(alice, "send "+evTopic+" -- e1")
	y.dis(alice, "send "+evTopic+" -- e2")
	y.dis(bob, "recv_ack "+sb+" "+ev(y.h.topicDone(evTopic).id, 2))
	l := y.dis(bob, "recv "+sb+" --peek").out
	eq(t, "e1, then the position notice", allIDs(l), ev(y.h.topicDone(evTopic).id, 1)+"/msg "+headMsgid(y.h.topicDone(evTopic))+"/msg")
	r = y.dis(bob, "recv_ack "+sb+" --before-including "+ev(y.h.topicDone(evTopic).id, 1))
	eq(t, "the position notice is never in the range", r.out, "acked "+ev(y.h.topicDone(evTopic).id, 1)+"\n")
}

// The range is the mailbox as it is at the call: a message that lands during it is not acked.
func TestRecvAckBeforeConcurrent(t *testing.T) {
	x := twoRuns(t)
	x.dis(alice, "send "+bob+" --id c1 -- x")
	x.dis(alice, "send "+bob+" --id c3 -- x")
	fired := false
	testHook = func(p string) {
		if p == "ack-ordered" && !fired {
			fired = true
			code(t, "c2 lands during the ack", x.dis(alice, "send "+bob+" --id c2 -- x"), 0)
		}
	}
	t.Cleanup(noHook)
	r := x.dis(bob, "recv_ack --before-including c3")
	eq(t, "acked what was there", fmt.Sprint(fired, " ", r.code, "\n", r.out), "true 0\nacked c1\nacked c3\n")
	eq(t, "c2 stays", ids(x.dis(bob, "recv --peek").out), "c2")
}

// A message that lands after the reader's read sorts after what it read, whatever its msgid: the reader's
// --before-including <last read> leaves it. An own mailbox (a lower msgid), an instance's, and a
// subscription (another publisher session).
func TestRecvAckBeforeArrivedAfterRead(t *testing.T) {
	x := instRig(t)
	x.dis(wHub, "send "+par+" --id z-5 -- x")
	x.dis(wHub, "send "+par+" --id z-9 -- x")
	read := ids(x.dis(par, "recv --peek").out)
	x.dis(wHub, "send "+par+" --id a-1 -- late")
	r := x.dis(par, "recv_ack --before-including z-9")
	eq(t, "own: what was read, not the later a-1", fmt.Sprint(read, "|", r.code, "\n", r.out), "z-5 z-9|0\nacked z-5\nacked z-9\n")
	eq(t, "a-1 stays", ids(x.dis(par, "recv --peek").out), "a-1")

	x.dis(par, "admit a1")
	ser, _ := x.register("a1", "a1k")
	x.dis(wHub, "send "+inst("a1")+" --id z-1 -- x")
	x.idis(inst("a1"), ser, "a1k", "recv --peek")
	x.dis(wHub, "send "+inst("a1")+" --id a-0 -- late")
	eq(t, "an instance: z-1 only", x.idis(inst("a1"), ser, "a1k", "recv_ack --before-including z-1").out, "acked z-1\n")
	eq(t, "a-0 stays", ids(x.idis(inst("a1"), ser, "a1k", "recv --peek").out), "a-0")

	y, sb, _ := evRig(t)
	y.dis(alice, "send "+evTopic+" --id "+ev(idB, 5)+" -- x")
	evs(y.dis(bob, "recv "+sb+" --peek").out)
	y.dis(alice, "send "+evTopic+" --id "+ev(idA, 1)+" -- late")
	eq(t, "a subscription: the session read, not the later one", y.dis(bob, "recv_ack "+sb+" --before-including "+ev(idB, 5)).out, "acked "+ev(idB, 5)+"\n")
	eq(t, "the later session stays", evs(y.dis(bob, "recv "+sb+" --peek").out), idA+"/1")
}

// The removed read forms exit 2.
func TestRemovedReadForms(t *testing.T) {
	x := twoRuns(t)
	x.mkTopic(evTopic, alice)
	x.dis(alice, "send "+bob+" --id r1 -- x")
	for _, cmd := range []string{"recv --file r1/x", "recv -m:rfc5322 r1", "recv --keep", "recv --follow --keep", "recv --ack r1",
		"recv_bag r1", "recv_bag --id r1 --msg", "recv_bag --keep", "sub " + evTopic + " --live", "sub " + evTopic + " --wake"} {
		code(t, cmd, x.dis(bob, cmd), 2)
	}
	eq(t, "nothing consumed", ids(x.dis(bob, "recv --peek").out), "r1")
	eq(t, "no subscription made", x.dis(bob, "list --type sub").out, "")
}

// cutWriter takes left bytes, then fails: a connection that drops part way.
type cutWriter struct{ left int }

func (w *cutWriter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		n := w.left
		w.left = 0
		return n, io.ErrClosedPipe
	}
	w.left -= len(p)
	return len(p), nil
}

// A chunk whose source reads short cuts the stream: exit 1, no final chunk, no size line left unfilled.
func TestShortReadCutsTheStream(t *testing.T) {
	x := twoRuns(t)
	var o, e strings.Builder
	s := x.h.newSession(context.Background(), "", IO{Stdin: strings.NewReader(""), Stdout: &o, Stderr: &e})
	s.me = bob
	code := s.run(func() {
		c := &chunker{s: s}
		c.chunk("m1", "msg", 3, strings.NewReader("ok\n"))
		c.chunk("m2", "msg", 10, strings.NewReader("abc"))
		c.final("")
	})
	eq(t, "exit 1", code, 1)
	cs, fin, err := stream(o.String())
	eq(t, "the whole chunk before it stands; the cut one is short; no final chunk", fmt.Sprint(len(cs), " ", fin, " ", err != nil), "1 - true")
	has(t, "saying so", e.String(), "read cut short")
}

// --limit counts from the first entry that is not the wake or position notice: a large wake notice does
// not stop a listing before its first message.
func TestLimitPastTheWakeNotice(t *testing.T) {
	x := instRig(t)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("w%02d", i)
		x.dis(par, "admit "+id)
		x.dis(wHub, "send "+inst(id)+" --id m -- x")
	}
	x.dis(wHub, "send "+par+" --id p1 -- one")
	x.dis(wHub, "send "+par+" --id p2 -- two")
	r := x.dis(par, "recv --limit 10")
	eq(t, "the wake notice, then one message, then 0;limit", fmt.Sprint(ids(r.out), " ", final(r.out)), "p1 ;limit")
	r = x.dis(par, "recv --limit 10")
	eq(t, "the next page", fmt.Sprint(ids(r.out), " ", final(r.out)), "p2 ")
}

// The test parser's part=info rule: `;part=info` alone, no id=; any other chunk needs both.
func TestInfoChunkForm(t *testing.T) {
	_, f, err := stream("5;part=info\r\nhi x\n\r\n0;limit\r\n\r\n")
	eq(t, "a part=info chunk carries no id", fmt.Sprint(err, " ", f), "<nil> ;limit")
	_, _, err = stream("5;id=a;part=info\r\nhi x\n\r\n0\r\n\r\n")
	eq(t, "with an id: not the form", err != nil, true)
	_, _, err = stream("0;end=gone\r\n")
	eq(t, "a final chunk needs its empty trailer and CRLF", err != nil, true)
}
