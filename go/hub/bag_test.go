package hub

// The message file (docs/wake-live-event-id.md, Message file): one file per message, its trailer, bags kept as
// received, the content-read check and damaged files.

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type member struct {
	name, body, link string
	typ              byte
}

func craftTar(ms ...member) string {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, m := range ms {
		hd := &tar.Header{Name: m.name, Typeflag: m.typ, Mode: 0o644, Size: int64(len(m.body)), Linkname: m.link, ModTime: time.Unix(0, 0)}
		if m.typ != tar.TypeReg {
			hd.Size = 0
		}
		if m.typ == tar.TypeXGlobalHeader {
			hd.PAXRecords = map[string]string{"comment": "abc"}
		}
		tw.WriteHeader(hd)
		if m.typ == tar.TypeReg {
			tw.Write([]byte(m.body))
		}
	}
	tw.Close()
	return b.String()
}

// trailerOf: a stored message's trailer, parsed; and its header block.
func trailerOf(t *testing.T, p string) (layout, string) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil || len(b) < trailerSize {
		t.Fatalf("%s: %v (%d bytes)", p, err, len(b))
	}
	l, ok := parseTrailer(b[len(b)-trailerSize:])
	if !ok {
		t.Fatalf("%s: trailer does not parse: %q", p, b[len(b)-trailerSize:])
	}
	return l, string(b[:l.hdr])
}

func TestTrailer(t *testing.T) {
	l := layout{hdr: 40, msgOff: 40, msgSize: 3, total: 40 + 3 + trailerSize}
	tb := l.trailer()
	eq(t, "212 bytes", len(tb), trailerSize)
	lines := strings.SplitAfter(string(tb), "\n")
	eq(t, "line widths", fmt.Sprint(len(lines[0]), len(lines[1]), len(lines[2]), len(lines[3]), len(lines[4])), "34 37 35 33 73")
	eq(t, "zero-padded", lines[0], "Header-Size: 00000000000000000040\n")
	eq(t, "Sha256 64 zeros", lines[4], "Sha256: "+strings.Repeat("0", 64)+"\n")
	got, ok := parseTrailer(tb)
	eq(t, "parses back", fmt.Sprint(got, ok), fmt.Sprint(l, true))
	for what, bad := range map[string]string{
		"short":            string(tb[:211]),
		"a space":          strings.Replace(string(tb), "Header-Size: 0", "Header-Size:  ", 1),
		"a letter":         strings.Replace(string(tb), "00000000000000000040", "0000000000000000004x", 1),
		"another name":     strings.Replace(string(tb), "Total-Size", "Total-Sizz", 1),
		"offset past body": string(layout{hdr: 40, msgOff: 41, msgSize: 3, total: 40 + 3 + trailerSize}.trailer()),
		"header past file": string(layout{hdr: 400, msgOff: 400, msgSize: 0, total: 300}.trailer()),
	} {
		if _, ok := parseTrailer([]byte(bad)); ok {
			t.Errorf("%s: parsed", what)
		}
	}
}

// Each case of the layout table, and what a reader gets of each.
func TestMessageFileLayout(t *testing.T) {
	x := twoRuns(t)
	os.WriteFile(filepath.Join(x.state, "budgets", "run"), []byte("subject=run\nrate=100\nburst=100\n"), 0o640)
	box := x.h.mbox(bob)
	file := func(id string) string { return filepath.Join(box, id+".msg") }
	// 1: an empty message (a gap marker's shape needs a topic: an empty -m:rfc5322 body with Subject)
	code(t, "case 1", x.dis(alice, "send "+bob+" --id c1 -m:rfc5322", "Subject: v\n\n"), 2)
	// 2: a body
	x.dis(alice, "send "+bob+" --id c2 --subject v -m", "hello\n\n")
	l, hdr := trailerOf(t, file("c2"))
	H := int64(len(hdr))
	eq(t, "case 2: offsets", fmt.Sprint(l.hdr, l.msgOff, l.msgSize, l.total), fmt.Sprint(H, H, 5, H+5+trailerSize))
	eq(t, "the header block", dateless(hdr), "From: "+alice+"\nDate: D\nSerial: 1\nSubject: v\n\n")
	eq(t, "the body: trailing newlines gone", readString(file("c2"))[H:H+5], "hello")
	eq(t, "-m:rfc5322 gives header block and body", dateless(x.dis(bob, "recv --id c2 --peek:rfc5322").out), "From: "+alice+"\nDate: D\nSerial: 1\nSubject: v\n\nhello")
	// 4: --file
	data := "a\x00b\nfile bytes\n"
	x.dis(alice, "send "+bob+" --id c4 --subject v --file f.bin", data)
	l, hdr = trailerOf(t, file("c4"))
	H = int64(len(hdr))
	eq(t, "case 4: offsets", fmt.Sprint(l.hdr, l.msgOff, l.msgSize, l.total), fmt.Sprint(H, H, 0, H+int64(len(data))+trailerSize))
	has(t, "Payload: file <name>, before the client's fields", hdr, "Serial: 1\nPayload: file f.bin\nSubject: v\n\n")
	eq(t, "the payload as received", readString(file("c4"))[H:H+int64(len(data))], data)
	eq(t, "no attachment folder", exists(filepath.Join(box, "c4")), false)
	eq(t, "--peek:file: the payload, raw", x.dis(bob, "recv --id c4 --peek:file").out, data)
	code(t, "--peek:bag on a file payload: 2", x.dis(bob, "recv --id c4 --peek:bag"), 2)
	code(t, "--peek:file without --id: 2", x.dis(bob, "recv --peek:file"), 2)
	code(t, "--peek:file with two: 2", x.dis(bob, "recv --id c4 --id c2 --peek:file"), 2)
	code(t, "--peek:file of a message without payload: 2", x.dis(bob, "recv --id c2 --peek:file"), 2)
	eq(t, "-m:rfc5322 leaves the payload out", dateless(x.dis(bob, "recv --id c4 --peek:rfc5322").out), "From: "+alice+"\nDate: D\nSerial: 1\nPayload: file f.bin\nSubject: v\n\n")
	l4 := x.dis(bob, "recv --peek").out
	eq(t, "--peek: header block, no payload", dateless(msgOf(l4, "c4")), "From: "+alice+"\nDate: D\nSerial: 1\nPayload: file f.bin\nSubject: v\n\n")
	eq(t, "(no payload chunk)", partOf(l4, "c4", "payload"), "")
	eq(t, "--peek:full: the payload chunks too", partOf(x.dis(bob, "recv --id c4 --peek:full").out, "c4", "payload"), data)
	code(t, "recv_bag of a file payload: 2", x.dis(bob, "recv_bag --id c4"), 2)
	// 5, 6, 7: bags
	bag := craftTar(member{name: "a.txt", body: "AAAA", typ: tar.TypeReg}, member{name: "./m.md", body: "the body\n", typ: tar.TypeReg},
		member{name: "e", typ: tar.TypeReg}, member{name: "d/", typ: tar.TypeDir})
	x.dis(alice, "send_bag "+bob+" --id c5 --subject v --msg m.md", bag)
	l, hdr = trailerOf(t, file("c5"))
	H = int64(len(hdr))
	B := int64(len(bag))
	k := int64(strings.Index(bag, "the body\n"))
	eq(t, "case 5: the body is the member inside the tar", fmt.Sprint(l.hdr, l.msgOff, l.msgSize, l.total), fmt.Sprint(H, H+k, 9, H+B+trailerSize))
	has(t, "Payload: bag, Msg", hdr, "Serial: 1\nPayload: bag\nMsg: m.md\nSubject: v\n\n")
	eq(t, "the tar as received", readString(file("c5"))[H:H+B], bag)
	eq(t, "recv_bag: the tar", x.dis(bob, "recv_bag --id c5 --peek").out, bag)
	eq(t, "--peek:bag: the tar", x.dis(bob, "recv --id c5 --peek:bag").out, bag)
	eq(t, "a bag's body: its --msg member", bodyOf(x.dis(bob, "recv --id c5 --peek").out, "c5"), "the body\n")
	eq(t, "-m:rfc5322: header, then the body range", dateless(x.dis(bob, "recv --id c5 --peek:rfc5322").out), "From: "+alice+"\nDate: D\nSerial: 1\nPayload: bag\nMsg: m.md\nSubject: v\n\nthe body\n")
	eq(t, "--peek:bag <path>: a member", x.dis(bob, "recv --id c5 --peek:bag a.txt").out, "AAAA")
	eq(t, "a leading ./ is ignored", x.dis(bob, "recv --peek:bag ./m.md --id c5").out, "the body\n")
	code(t, "--peek:file on a bag: 2", x.dis(bob, "recv --id c5 --peek:file"), 2)
	code(t, "a directory member: 2", x.dis(bob, "recv --id c5 --peek:bag d/"), 2)
	code(t, "no such member: 3", x.dis(bob, "recv --id c5 --peek:bag zz"), 3)
	eq(t, "the listing: the header, then the body", strings.HasSuffix(msgOf(x.dis(bob, "recv --peek").out, "c5"), "Subject: v\n\nthe body\n"), true)
	x.dis(alice, "send_bag "+bob+" --id c6 --msg e", bag)
	l, hdr = trailerOf(t, file("c6"))
	H = int64(len(hdr))
	eq(t, "case 6: an empty --msg member", fmt.Sprint(l.msgOff > H, l.msgSize, l.total), fmt.Sprint(true, 0, H+B+trailerSize))
	x.dis(alice, "send_bag "+bob+" --id c7", bag)
	l, hdr = trailerOf(t, file("c7"))
	H = int64(len(hdr))
	eq(t, "case 7: no --msg", fmt.Sprint(l.hdr, l.msgOff, l.msgSize, l.total), fmt.Sprint(H, H, 0, H+B+trailerSize))
	lacks(t, "no Msg", hdr, "Msg:")
	eq(t, "a bag without --msg: an empty body", bodyOf(x.dis(bob, "recv --id c7 --peek").out, "c7"), "")
	// consuming: only a full read; --peek never
	for _, c := range []string{"recv --peek", "recv --peek:full", "recv --peek:header", "recv --id c5 --peek:bag", "recv --id c4 --peek:file", "recv_bag --id c6 --peek", "recv --id c2 --peek:rfc5322"} {
		x.dis(bob, c)
		eq(t, c+" consumes nothing", ids(x.dis(bob, "recv --peek").out), "c2 c4 c5 c6 c7")
	}
	eq(t, "recv_bag: the first message in listing order must be a bag", x.dis(bob, "recv_bag").code, 2)
	eq(t, "(nothing consumed)", exists(file("c2")), true)
	full := x.dis(bob, "recv --id c4").out
	eq(t, "a full --id read: header, body, payload", partOf(full, "c4", "payload"), data)
	eq(t, "and consumes", exists(file("c4")), false)
	eq(t, "recv_bag without --peek consumes", x.dis(bob, "recv_bag --id c5").out+fmt.Sprint(exists(file("c5"))), bag+"false")
	x.dis(bob, "recv")
	eq(t, "a full listing consumes payload messages too", x.dis(bob, "recv --peek").out, empty)
	code(t, "recv_bag of an empty mailbox: 3", x.dis(bob, "recv_bag"), 3)
}

func TestBagMembers(t *testing.T) {
	x := twoRuns(t)
	os.WriteFile(filepath.Join(x.state, "budgets", "run"), []byte("subject=run\nrate=100\nburst=100\n"), 0o640)
	send := func(id, tarball string, msg ...string) result {
		cmd := "send_bag " + bob + " --id " + id
		if len(msg) > 0 {
			cmd += " --msg " + msg[0]
		}
		return x.dis(alice, cmd, tarball)
	}
	// last occurrence wins, for --msg and for recv --file
	twice := craftTar(member{name: "m", body: "first\n", typ: tarTypeReg}, member{name: "./m", body: "second\n", typ: tarTypeReg})
	code(t, "two members of one name", send("L1", twice, "./m"), 0)
	eq(t, "--msg: the last", bodyOf(x.dis(bob, "recv --id L1 --peek").out, "L1"), "second\n")
	eq(t, "--peek:bag <path>: the last", x.dis(bob, "recv --id L1 --peek:bag m").out, "second\n")
	code(t, "the last a directory: --msg refused", send("L2", craftTar(member{name: "m", body: "x", typ: tarTypeReg}, member{name: "m", typ: tar.TypeDir}), "m"), 2)
	// names, types, count and depth are not judged
	for i, c := range []string{
		craftTar(member{name: "/etc/x", body: "x", typ: tarTypeReg}),
		craftTar(member{name: "a/../../x", body: "x", typ: tarTypeReg}),
		craftTar(member{name: "a/.hidden", body: "x", typ: tarTypeReg}),
		craftTar(member{name: "a/b/c/d/e/f/g/h/i", body: "x", typ: tarTypeReg}),
		craftTar(member{name: "l", link: "/etc/passwd", typ: tar.TypeSymlink}, member{name: "f", body: "f", typ: tarTypeReg}),
		craftTar(member{name: "p", typ: tar.TypeFifo}, member{name: "f", body: "f", typ: tarTypeReg}),
		craftTar(member{name: "h", link: "/etc/passwd", typ: tar.TypeLink}, member{name: "f", body: "f", typ: tarTypeReg}),
	} {
		code(t, fmt.Sprint("not judged ", i), send(fmt.Sprintf("N%d", i), c), 0)
	}
	code(t, "a symlink member is no regular one: recv --file 2", x.dis(bob, "recv --id N4 --peek:bag l"), 2)
	code(t, "--msg naming a symlink: 2", send("S1", craftTar(member{name: "l", link: "x", typ: tar.TypeSymlink}), "l"), 2)
	sp := tarFile(t, "sparse_gnu")
	rd := tar.NewReader(strings.NewReader(sp))
	hd, _ := rd.Next()
	code(t, "--msg naming a sparse member: 2", send("S2", sp, hd.Name), 2)
	code(t, "the sparse bag itself is stored", send("S3", sp), 0)
	code(t, "--peek:bag of a sparse member: 2", x.dis(bob, "recv --id S3 --peek:bag "+hd.Name), 2)
	for _, c := range []struct {
		what string
		tar  string
		msg  string
	}{
		{"an empty bag", craftTar(member{name: "d/", typ: tar.TypeDir}), "empty bag"},
		{"not a tar", "garbage that is no tar archive at all, but long enough to fill a header block " + strings.Repeat("x", 600), "bad tar"},
	} {
		r := send("BX", c.tar)
		code(t, c.what, r, 2)
		has(t, c.what+" (message)", r.err, c.msg)
	}
	r := send("B2", craftTar(member{name: "m.md", body: "m\n", typ: tarTypeReg}), "none.md")
	code(t, "--msg naming no member", r, 2)
	has(t, "(message)", r.err, "--msg names no member of the bag: none.md")
	r = send("B4", craftTar(member{typ: tar.TypeXGlobalHeader, name: "pax_global_header"}, member{name: "f", body: "f\n", typ: tarTypeReg}))
	code(t, "a global header (git archive) is no member", r, 0)
	code(t, "recv_bag of a message that is no bag", func() result { x.dis(alice, "send "+bob+" --id T1 -- t"); return x.dis(bob, "recv_bag --id T1") }(), 2)
	eq(t, "a dup bag", send("B4", craftTar(member{name: "f", body: "f\n", typ: tarTypeReg})).out, "dup B4 (dropped)\n")
	eq(t, "no stage left", len(x.stages()), 0)
	// the size cap: stored bytes
	big := craftTar(member{name: "big", body: strings.Repeat("z", BagMaxBytes-2048), typ: tarTypeReg})
	code(t, "at the cap: stored", send("Z1", big), 0)
	r = send("Z2", big+strings.Repeat("\x00", 4096))
	code(t, "past it: 2", r, 2)
	has(t, "(message)", r.err, "bag exceeds cap")
}

const tarTypeReg = tar.TypeReg

// A content read compares Total-Size with the file's size: a torn or truncated file goes to .damaged/, with a
// journal line; a listing skips it, a msgid verb exits 1; the reaper deletes it after MSG_TTL.
func TestDamaged(t *testing.T) {
	x := twoRuns(t)
	os.WriteFile(x.h.journal, nil, 0o660)
	box := x.h.mbox(bob)
	for _, id := range []string{"d1", "d2", "d3", "d4", "ok"} {
		x.dis(alice, "send "+bob+" --id "+id+" --file f", strings.Repeat("y", 5000))
	}
	os.Truncate(filepath.Join(box, "d1.msg"), 4000)    // torn: the trailer gone
	b2, _ := os.ReadFile(filepath.Join(box, "d2.msg")) // Total-Size not the file's
	l2, _ := parseTrailer(b2[len(b2)-trailerSize:])
	l2.total += 7
	copy(b2[len(b2)-trailerSize:], l2.trailer())
	os.WriteFile(filepath.Join(box, "d2.msg"), b2, 0o640)
	b, _ := os.ReadFile(filepath.Join(box, "d3.msg")) // the trailer mangled
	copy(b[len(b)-trailerSize:], "Header-Size: zz")
	os.WriteFile(filepath.Join(box, "d3.msg"), b, 0o640)
	l := x.dis(bob, "recv --peek").out
	eq(t, "a listing skips them", ids(l), "d4 ok")
	j := readString(x.h.journal)
	has(t, "journal: torn", j, " damaged "+bob+" d1 stat=4000 total=unparsable\n")
	has(t, "journal: Total-Size not the size", j, fmt.Sprintf(" damaged %s d2 stat=%d total=%d\n", bob, len(b2), len(b2)+7))
	has(t, "journal: mangled", j, " damaged "+bob+" d3 stat=")
	eq(t, "moved", strings.Join(globNames(filepath.Join(box, damagedDir)), " "), "d1.msg d2.msg d3.msg")
	eq(t, "logged once: the next listing does not see them", strings.Count(readString(x.h.journal), " damaged "), 3)
	x.dis(bob, "recv --peek")
	eq(t, "(still three)", strings.Count(readString(x.h.journal), " damaged "), 3)
	os.Truncate(filepath.Join(box, "d4.msg"), 100)
	for _, cmd := range []string{"recv --id d4 --peek:rfc5322", "recv --id d4 --peek:file", "recv --id d4 --peek:bag", "recv_bag --id d4"} {
		os.Truncate(filepath.Join(box, "d4.msg"), 100)
		r := x.dis(bob, cmd)
		eq(t, cmd+": exit 1, damaged", fmt.Sprint(r.code, strings.Contains(r.err, "damaged")), "1 true")
		os.Rename(filepath.Join(box, damagedDir, "d4.msg"), filepath.Join(box, "d4.msg"))
	}
	eq(t, "a header-only read (the wake summary, mail counts) does not check", len(msgFiles(box)), 2)
	old := time.Now().Add(-(MsgTTLSecs + 60) * time.Second)
	os.Chtimes(filepath.Join(box, damagedDir, "d1.msg"), old, old)
	x.h.Reap()
	eq(t, "the reaper deletes a damaged file after MSG_TTL", strings.Join(globNames(filepath.Join(box, damagedDir)), " "), "d2.msg d3.msg")
	// a subscription: the move takes that mailbox's link only
	x.mkTopic(evTopic, alice)
	sb := strings.TrimSpace(x.dis(bob, "sub "+evTopic).out)
	sa := strings.TrimSpace(x.dis(alice, "sub "+evTopic).out)
	e := pubMsgid(x.dis(alice, "send "+evTopic+" -- shared").out)
	g, _ := os.OpenFile(filepath.Join(x.h.mbox(sb), e+".msg"), os.O_WRONLY|os.O_APPEND, 0o640)
	g.WriteString("x") // both links grow: the same inode
	g.Close()
	eq(t, "bob's listing skips it", ids(x.dis(bob, "recv "+sb+" --peek").out), "")
	eq(t, "alice's link is untouched until her own read", exists(filepath.Join(x.h.mbox(sa), e+".msg")), true)
	eq(t, "her read finds the damage too", ids(x.dis(alice, "recv "+sa+" --peek").out), "")
}

// msgBytes: a message file as the hub writes it, for tests that place one by hand.
func msgBytes(hdr, body string) []byte {
	h := int64(len(hdr))
	l := layout{hdr: h, msgOff: h, msgSize: int64(len(body)), total: h + int64(len(body)) + trailerSize}
	return append([]byte(hdr+body), l.trailer()...)
}
