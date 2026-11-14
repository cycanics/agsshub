package hub

// --subject, the hub's own msgids, Subject: chat, and expiry notices (docs/expiry-and-subject.md), on the
// daemon.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"agsshub/expiry"
)

// age makes a mailbox's message look uncollected for longer than MSG_TTL.
func (x *th) age(box, mid string) {
	x.t.Helper()
	old := time.Now().Add(-(MsgTTLSecs + 60) * time.Second)
	if err := os.Chtimes(filepath.Join(x.h.mbox(box), mid+"."+msgExt), old, old); err != nil {
		x.t.Fatal(err)
	}
}

// aged sends a message and ages it. Bob's mailbox unless it says otherwise.
func (x *th) aged(from, to, mid, opts string) {
	x.t.Helper()
	code(x.t, "send "+mid, x.dis(from, "send "+to+" --id "+mid+" "+opts+" -- text-of-"+mid), 0)
	x.age(to, mid)
}

func (x *th) stored(box, mid string) string {
	return readString(filepath.Join(x.h.mbox(box), mid+"."+msgExt))
}

func (x *th) expiredDir(box string) string { return filepath.Join(x.h.mbox(box), expiry.Dir) }

// noticeID: a listing's first expired-* msgid.
var noticeIDRE = regexp.MustCompile(`;id=(expired-[0-9a-f]+-[0-9]+);part=`)

func noticeID(listing string) string {
	if m := noticeIDRE.FindStringSubmatch(listing); m != nil {
		return m[1]
	}
	return ""
}

func TestSubjectOnEveryForm(t *testing.T) {
	x := twoRuns(t)
	code(t, "inline", x.dis(alice, "send "+bob+" --id s1 --subject in.a -- hi"), 0)
	has(t, "stored as a client field", x.stored(bob, "s1"), "Serial: 1\nSubject: in.a\n\nhi")
	code(t, "-m", x.dis(alice, "send "+bob+" --id s2 --subject in.b -m", "body\n"), 0)
	has(t, "stored", x.stored(bob, "s2"), "Subject: in.b\n\nbody")
	code(t, "--file", x.dis(alice, "send "+bob+" --id s3 --subject in.c --file f.txt", "bytes"), 0)
	has(t, "stored", x.stored(bob, "s3"), "Subject: in.c\n\n")
	bag := string(makeTar(t, map[string]string{"m.md": "the message\n", "d/x": "y"}))
	code(t, "send_bag", x.dis(alice, "send_bag "+bob+" --id s4 --msg m.md --subject in.d", bag), 0)
	has(t, "after Bag and Msg", x.stored(bob, "s4"), "Payload: bag\nMsg: m.md\nSubject: in.d\n\n")
	code(t, "-m --chunked", x.dis(alice, "send "+bob+" --id s5 --subject in.e -m --chunked", string(frame([]byte("cb\n"), 2, ""))), 0)
	has(t, "stored", x.stored(bob, "s5"), "Subject: in.e\n\ncb")
	code(t, "--file --chunked", x.dis(alice, "send "+bob+" --id s6 --subject in.f --file g.txt --chunked", string(frame([]byte("cb"), 1, ""))), 0)
	has(t, "stored", x.stored(bob, "s6"), "Subject: in.f\n")
	code(t, "send_bag --chunked", x.dis(alice, "send_bag "+bob+" --id s7 --subject in.g --chunked", string(frame([]byte(bag), 4000, ""))), 0)
	has(t, "stored", x.stored(bob, "s7"), "Subject: in.g\n")
	l := x.dis(bob, "recv --peek").out
	for _, v := range []string{"in.a", "in.b", "in.c", "in.d", "in.e", "in.f", "in.g"} {
		has(t, "recv shows it the same way", l, "Subject: "+v+"\n")
	}
	has(t, "a free word after the option", x.dis(alice, "send "+bob+" --subject a/b-c_d.e -- hello there  now").err, "")
	has(t, "the inline message stays verbatim", x.dis(bob, "recv --peek").out, "Subject: a/b-c_d.e\n\nhello there  now\r\n")

	tp := "topic+s@acme.ssh"
	x.mkTopic(tp, alice)
	sub := strings.TrimSpace(x.dis(bob, "sub "+tp).out)
	code(t, "published with one", x.dis(alice, "send "+tp+" --id "+ev(idA, 1)+" --subject ev.a -- x"), 0)
	code(t, "a bag published with one", x.dis(alice, "send_bag "+tp+" --id "+ev(idA, 2)+" --msg m.md --subject ev.b", bag), 0)
	sl := x.dis(bob, "recv "+sub+" --peek").out
	has(t, "the topic path carries it", msgOf(sl, ev(idA, 1)), "From: "+alice)
	has(t, "after the hub fields", msgOf(sl, ev(idA, 1)), "\nSubject: ev.a\n\nx")
	has(t, "a bag's too", msgOf(sl, ev(idA, 2)), "\nSubject: ev.b\n\n")
	has(t, "raw: after the hub's fields, the client's", x.dis(bob, "recv "+sub+" --id "+ev(idA, 2)+" --peek:rfc5322").out, "Payload: bag\nMsg: m.md\nSubject: ev.b\n\nthe message\n")
}

func TestSubjectGrammar(t *testing.T) {
	x := twoRuns(t)
	for _, form := range []string{"send " + bob + " -m", "send " + bob + " --file f", "send_bag " + bob} {
		for _, bad := range []string{".a", "-a", "a*", "a,b", "[gap]", "é", strings.Repeat("a", 65)} {
			r := x.dis(alice, form+" --subject "+bad, "x")
			code(t, form+" --subject "+bad, r, 2)
			has(t, "saying why", r.err, "bad --subject")
		}
		r := x.dis(alice, form+" --subject")
		code(t, form+": as the last argument", r, 2)
		eq(t, "saying so", r.err, "dispatch: --subject needs a value\n")
		code(t, "64 is the most", x.dis(alice, "send "+bob+" --subject "+strings.Repeat("a", 64)+" -- x"), 0)
	}
	r := x.dis(alice, "send "+bob+" --subject -- oops")
	code(t, "the separator is no value", r, 2)
	r = x.dis(alice, "send "+bob+" --subject in.a -m:rfc5322", "Subject: x\n\nb\n")
	code(t, "with -m:rfc5322: the header block carries it", r, 2)
	has(t, "saying so", r.err, "header block")
	r = x.dis(alice, "send "+bob+" --subject in.a --live -m:rfc5322")
	code(t, "--live --id too --peek:rfc5322", r, 2)
	x.dis(bob, "recv")
	eq(t, "nothing stored", x.dis(bob, "recv --peek").out, empty)
	// no token taken: the default burst is 12; refusals past it are refusals, and a send still goes
	code(t, "a refusal takes no flood token", x.dis(alice, "send "+bob+" -- ok"), 0)
}

func TestSubjectStepOneEnforcesNothing(t *testing.T) {
	x := twoRuns(t)
	code(t, "no Subject on a send is still fine", x.dis(alice, "send "+bob+" -- plain"), 0)
	code(t, "two Subject fields too", x.dis(alice, "send "+bob+" -m:rfc5322", "Subject: a\nSubject: b\n\nx\n"), 0)
	code(t, "a blank one too", x.dis(alice, "send "+bob+" -m:rfc5322", "Subject: \n\nx\n"), 0)
}

func TestSubjectOnALiveRequest(t *testing.T) {
	r := newLiveRig(t)
	x, hub, grp := r.x, r.hub, r.grp
	r.follow()
	defer r.close()
	framed := string(frame([]byte("hello\n"), 4, ""))
	code(t, "-m", x.dis(grp, "send "+hub+" --live --chunked --id LS1 --subject out.do -m", framed), 0)
	sid := r.sid("LS1")
	r.acceptCode(sid)
	h, _, _ := strings.Cut(r.req(sid), "\n\n")
	eq(t, "the hub header block has it, before Transfer-Encoding", regexp.MustCompile(`Date: \S+\n`).ReplaceAllString(h, ""),
		"From: "+grp+"\nMsgid: LS1\nSubject: out.do\nTransfer-Encoding: chunked")
	eq(t, "the framed body untouched", body(r.req(sid)), framed)
	code(t, "--file", x.dis(grp, "send "+hub+" --live --chunked --id LS2 --file a.txt --subject out.f", framed), 0)
	sid = r.sid("LS2")
	r.acceptCode(sid)
	h, _, _ = strings.Cut(r.req(sid), "\n\n")
	has(t, "after Payload", h, "Payload: file a.txt\nSubject: out.f\nTransfer-Encoding: chunked")
	code(t, "without one", x.dis(grp, "send "+hub+" --live --chunked --id LS3 -m", framed), 0)
	sid = r.sid("LS3")
	r.acceptCode(sid)
	lacks(t, "no field", r.req(sid), "Subject:")
}

func TestHubSubjectForABackend(t *testing.T) {
	x := twoRuns(t)
	lib := filepath.Join(x.dir, "lib")
	if out, err := exec.Command("cp", "-a", x.lib, lib).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	plug := filepath.Join(lib, "backends", "email")
	os.WriteFile(filepath.Join(plug, "send"), []byte("#!/bin/sh\necho \"subject=[$HUB_SUBJECT]\"\n"), 0o755)
	x.h.cfg.Lib = lib
	os.WriteFile(filepath.Join(x.state, "aliases", "carol"), []byte("type=email\naddr=c@x\n"), 0o640)
	eq(t, "--subject", x.dis(alice, "send carol --subject in.a -- hi").out, "subject=[in.a]\n")
	eq(t, "a header block's", x.dis(alice, "send carol -m:rfc5322", "Subject: free text\n\nb\n").out, "subject=[free text]\n")
	eq(t, "none", x.dis(alice, "send carol -- hi").out, "subject=[]\n")
}

func TestHubsMsgidsAreRefused(t *testing.T) {
	r := newLiveRig(t)
	x, hub, grp := r.x, r.hub, r.grp
	r.follow()
	defer r.close()
	bag := string(makeTar(t, map[string]string{"m.md": "x"}))
	tp := "topic+x@home.ssh"
	x.mkTopic(tp, grp)
	for what, c := range map[string][2]string{
		"inline":      {"send " + hub + " --id expired-x -- hi", ""},
		"-m":          {"send " + hub + " --id expired-x -m", "b"},
		"--file":      {"send " + hub + " --id expired-x --file f", "b"},
		"rfc":         {"send " + hub + " --id expired-x -m:rfc5322", "Subject: a\n\nb"},
		"bag":         {"send_bag " + hub + " --id expired-x", bag},
		"live":        {"send " + hub + " --live --id expired-x -m", string(frame([]byte("b"), 1, ""))},
		"a topic":     {"send " + tp + " --id expired-x -- hi", ""},
		"a topic bag": {"send_bag " + tp + " --id expired-x", bag},
	} {
		res := x.dis(grp, c[0], c[1])
		code(t, what, res, 2)
		eq(t, what+": saying why", res.err, "dispatch: bad --id: expired-* is the hub's\n")
	}
	eq(t, "nothing stored", x.dis(hub, "recv --peek").out, empty)
	code(t, "another id is fine", x.dis(grp, "send "+hub+" --id expiredx -- hi"), 0)
	code(t, "and an id with it inside", x.dis(grp, "send "+hub+" --id a-expired-x -- hi"), 0)
}

func TestReapCountsAndTheThreeSteps(t *testing.T) {
	x := twoRuns(t)
	x.aged(alice, bob, "m1", "--subject in.x")
	code(t, "a message with a file", x.dis(alice, "send "+bob+" --id m2 --subject in.x --file f.txt", "bytes"), 0)
	x.age(bob, "m2")
	x.aged(alice, bob, "m3", "")
	code(t, "fresh mail", x.dis(alice, "send "+bob+" --id m4 -- fresh"), 0)
	x.h.Reap()
	box := x.h.mbox(bob)
	eq(t, "no claim left", len(filepath.Join(box, expiry.Dir)) > 0 && func() bool {
		ms, _ := filepath.Glob(filepath.Join(box, expiry.Dir, ".claim.*"))
		return len(ms) == 0
	}(), true)
	l := x.dis(bob, "recv --peek").out
	eq(t, "the notice comes first, then the mail", ids(l), noticeID(l)+" m4")
	today := time.Now().UTC().Format("2006-01-02")
	has(t, "from the sender", l, "From: "+alice+"\nDate: "+today)
	has(t, "its total", l, "\nExpired: 3\n\nverb\tcount\toldest\tnewest\nin.x\t2\t")
	has(t, "its special table", l, "\n\nspecial\tcount\toldest\tnewest\n[no-subject]\t1\t")
	lacks(t, "no Subject of its own", msgOf(l, noticeID(l)), "Subject:")
	lacks(t, "nor a body it never had", msgOf(l, noticeID(l)), "text-of-")
}

func TestOrderAndConsuming(t *testing.T) {
	x := twoRuns(t)
	x.mustMint("svc.a@acme.ssh", "svca")
	x.aged(bob, alice, "b1", "--subject v") // the sender run.t+bob/…
	x.aged("svc.a@acme.ssh", alice, "a1", "--subject v")
	code(t, "fresh", x.dis(bob, "send "+alice+" --id z1 -- hi"), 0)
	eq(t, "before the sweep: arrival order", ids(x.dis(alice, "recv --peek").out), "b1 a1 z1")
	x.h.Reap()
	l := x.dis(alice, "recv --peek").out
	i, j := strings.Index(l, "From: "+bob), strings.Index(l, "From: svc.a@acme.ssh")
	if i < 0 || j < 0 || !(i < j) {
		t.Errorf("notices are in byte order of from: %s", l)
	}
	if k := strings.Index(l, ";id=z1;"); k < j {
		t.Errorf("notices before mail: %s", l)
	}
	eq(t, "--peek keeps them", x.dis(alice, "recv --peek").out, l)
	eq(t, "plain recv lists the same", x.dis(alice, "recv").out, l)
	eq(t, "and consumes them", x.dis(alice, "recv").out, empty)
}

func TestOnlyANoticeIsNotNoMessages(t *testing.T) {
	x := twoRuns(t)
	x.aged(alice, bob, "m1", "--subject v")
	x.h.Reap()
	lacks(t, "a mailbox with only a notice", x.dis(bob, "recv --peek").out, "(no messages)")
}

func TestFollowAndNotices(t *testing.T) {
	x := twoRuns(t)
	x.aged(alice, bob, "m0", "--subject before")
	x.h.Reap()
	code(t, "backlog mail", x.dis(alice, "send "+bob+" --id m1 -- backlog"), 0)
	f := x.start(x.adm(bob), "recv --follow", "")
	waitFor(t, 2*time.Second, "the follower", x.followed(bob))
	waitFor(t, 2*time.Second, "the backlog", func() bool { return strings.Contains(f.out.String(), ";id=m1;") })
	o := f.out.String()
	if !strings.Contains(o, "Expired: 1") || strings.Index(o, "Expired: 1") > strings.Index(o, ";id=m1;") {
		t.Errorf("a notice comes before the backlog: %s", o)
	}
	waitFor(t, 2*time.Second, "it is cleared", func() bool { return len(expiry.List(x.h.mbox(bob), expiry.Options{})) == 0 })

	// one appearing during the follow: the reaper's change nudges it, well inside the 30 s poll
	code(t, "a message that will expire", x.dis(alice, "send "+bob+" --id m2 --subject during -- x"), 0)
	waitFor(t, 2*time.Second, "it is read", func() bool { return strings.Contains(f.out.String(), ";id=m2;") })
	mkAged := func(mid string) {
		os.MkdirAll(x.h.mbox(bob), 0o2770)
		os.WriteFile(filepath.Join(x.h.mbox(bob), mid+"."+msgExt), []byte("From: "+alice+"\nDate: 2026-11-01T00:00:00Z\nSerial: 1\nSubject: during\n\nx\n"), 0o640)
		x.age(bob, mid)
	}
	mkAged("m3")
	x.h.Reap()
	waitFor(t, 2*time.Second, "the new notice", func() bool { return strings.Contains(f.out.String(), "during\t1\t") })
	f.cancel()
	f.wait(t, 2*time.Second)
}

func TestFollowKeepPrintsOnceAndAckClears(t *testing.T) {
	x := twoRuns(t)
	x.aged(alice, bob, "m0", "--subject v")
	x.h.Reap()
	f := x.start(x.adm(bob), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the follower", x.followed(bob))
	waitFor(t, 2*time.Second, "the notice", func() bool { return strings.Contains(f.out.String(), "Expired: 1") })
	code(t, "a message wakes it", x.dis(alice, "send "+bob+" --id m1 -- x"), 0)
	waitFor(t, 2*time.Second, "the message", func() bool { return strings.Contains(f.out.String(), ";id=m1;") })
	eq(t, "the notice printed once", strings.Count(f.out.String(), "Expired: 1"), 1)
	id := noticeID(f.out.String())
	eq(t, "it stays", noticeID(x.dis(bob, "recv --peek").out), id)
	r := x.dis(bob, "recv_ack "+id)
	code(t, "--ack clears it", r, 0)
	eq(t, "saying so", r.out, "acked "+id+"\n")
	eq(t, "gone", noticeID(x.dis(bob, "recv --peek").out), "")
	f.cancel()
	f.wait(t, 2*time.Second)
}

func TestFollowKeepPrintsAChangedNotice(t *testing.T) {
	x := twoRuns(t)
	x.aged(alice, bob, "m0", "--subject v")
	x.h.Reap()
	f := x.start(x.adm(bob), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the notice", func() bool { return strings.Contains(f.out.String(), "Expired: 1") })
	x.dis(alice, "send "+bob+" --id m1 --subject v -- x")
	waitFor(t, 2*time.Second, "m1", func() bool { return strings.Contains(f.out.String(), ";id=m1;") })
	x.age(bob, "m1")
	x.h.Reap()
	waitFor(t, 2*time.Second, "its next msgid", func() bool { return strings.Contains(f.out.String(), "Expired: 2") })
	f.cancel()
	f.wait(t, 2*time.Second)
}

func TestAckAndRawOfAnOlderMsgid(t *testing.T) {
	x := twoRuns(t)
	x.aged(alice, bob, "m1", "--subject v")
	x.h.Reap()
	older := noticeID(x.dis(bob, "recv --peek").out)
	x.aged(alice, bob, "m2", "--subject v")
	x.h.Reap()
	cur := noticeID(x.dis(bob, "recv --peek").out)
	if older == cur {
		t.Fatal("the msgid did not change")
	}
	r := x.dis(bob, "recv_ack "+older)
	code(t, "recv_ack of an older msgid: exit 0", r, 0)
	eq(t, "gone", r.out, "gone "+older+"\n")
	eq(t, "changes nothing", noticeID(x.dis(bob, "recv --peek").out), cur)
	r = x.dis(bob, "recv --id "+older+" --peek:rfc5322")
	code(t, "--peek:rfc5322 of an older msgid: exit 3", r, 3)
	eq(t, "as a missing message", r.err, "dispatch: no such message: "+older+"\n")
	has(t, "cumulative counts", x.dis(bob, "recv --id "+cur+" --peek:rfc5322").out, "\nExpired: 2\n\nverb\tcount\toldest\tnewest\nv\t2\t")
	r = x.dis(bob, "recv --id "+older+" --id "+cur)
	eq(t, "--id of an older msgid: part=missing; the current streams", allIDs(r.out), older+"/missing "+cur+"/msg")
	eq(t, "and the full --id read consumed the current one", noticeID(x.dis(bob, "recv --peek").out), "")
	x.aged(alice, bob, "m3", "--subject v")
	x.h.Reap()
	cur = noticeID(x.dis(bob, "recv --peek").out)
	r = x.dis(bob, "recv --id "+cur+" --peek:rfc5322")
	code(t, "the current one", r, 0)
	has(t, "its fields", r.out, "From: "+alice+"\nDate: ")
	lacks(t, "no Serial", r.out, "Serial")
	has(t, "a new notice after the clear", r.out, "\nExpired: 1\n\nverb\tcount\toldest\tnewest\nv\t1\t")
	eq(t, "--peek:rfc5322 keeps it", noticeID(x.dis(bob, "recv --peek").out), cur)
	eq(t, "recv_ack clears it", x.dis(bob, "recv_ack "+cur).out, "acked "+cur+"\n")
	r = x.dis(bob, "recv_ack "+cur)
	eq(t, "recv_ack with no notice from that sender: gone, exit 0", fmt.Sprint(r.code, " ", r.out), "0 gone "+cur+"\n")
}

func TestFilesAndBagsOfANotice(t *testing.T) {
	x := twoRuns(t)
	x.aged(alice, bob, "m1", "--subject v")
	x.h.Reap()
	id := noticeID(x.dis(bob, "recv --peek").out)
	code(t, "--peek:file", x.dis(bob, "recv --id "+id+" --peek:file"), 2)
	code(t, "--peek:bag", x.dis(bob, "recv --id "+id+" --peek:bag"), 2)
	code(t, "recv_bag", x.dis(bob, "recv_bag --id "+id), 2)
	code(t, "the notice directory is no msgid", x.dis(bob, "recv --id .expired --peek:file"), 2)
	eq(t, "it stays", noticeID(x.dis(bob, "recv --peek").out), id)
}

func TestSubscriptionsGetNotices(t *testing.T) {
	x := twoRuns(t)
	tp := "topic+n@acme.ssh"
	x.mkTopic(tp, alice)
	s1 := strings.TrimSpace(x.dis(bob, "sub "+tp).out)
	s2 := strings.TrimSpace(x.dis(alice, "sub "+tp).out)
	code(t, "publish", x.dis(alice, "send "+tp+" --id "+ev(idA, 1)+" --subject ev -- x"), 0)
	code(t, "publish", x.dis(alice, "send "+tp+" -- x"), 0)
	x.age(s1, ev(idA, 1)) // one file linked into both: the same mtime
	code(t, "bob reads the second", x.dis(bob, "recv_ack "+s1+" "+ev(idA, 2)), 0)
	x.h.Reap()
	l := x.dis(bob, "recv "+s1+" --peek").out
	has(t, "nothing about the session", dateless(l), "From: "+alice+"\nDate: D\nExpired: 1\n\nverb\tcount")
	lacks(t, "no Sub- field", l, "Sub-")
	has(t, "raw the same", dateless(x.dis(bob, "recv "+s1+" --id "+noticeID(l)+" --peek:rfc5322").out), "\nDate: D\nExpired: 1\n")
	has(t, "a hard link: each mailbox counts its own", x.dis(alice, "recv "+s2+" --peek").out, "\nExpired: 1\n\nverb\tcount\toldest\tnewest\nev\t1\t")
	eq(t, "the topic's staged copies get none", exists(x.expiredDir(tp)), false)
	code(t, "the next delivery", x.dis(alice, "send "+tp+" -- x"), 0)
	l = x.dis(bob, "recv "+s1+" --peek").out
	eq(t, "the skipped numbers still show the gap", seqs(l), "3")
	e3 := ev(idA, 3)
	f := x.start(x.adm(bob), "recv "+s1+" --follow", "")
	waitFor(t, 2*time.Second, "the notice, then the message", func() bool { return strings.Contains(f.out.String(), ";id="+e3+";") })
	o := f.out.String()
	if strings.Index(o, "Expired: 1") > strings.Index(o, ";id="+e3+";") {
		t.Errorf("order in a followed subscription: %s", o)
	}
	lacks(t, "in the follow, the same fields", o, "Sub-")
	f.cancel()
	f.wait(t, 2*time.Second)
	code(t, "a stored notice", x.dis(alice, "recv "+s2+" --peek"), 0)
	code(t, "unsub", x.dis(alice, "unsub "+s2), 0)
	eq(t, "unsub takes its notices with its mailbox", exists(x.h.mbox(s2)), false)
}

func TestTopicMailboxNeverGetsANotice(t *testing.T) {
	x := twoRuns(t)
	tp := "topic+p@acme.ssh"
	x.mkTopic(tp, alice)
	box := x.h.mbox(tp)
	os.MkdirAll(box, 0o2770)
	f := filepath.Join(box, "stale."+msgExt)
	os.WriteFile(f, []byte("From: "+alice+"\nDate: 2026-11-01T00:00:00Z\n\nx\n"), 0o640)
	x.age(tp, "stale")
	x.h.Reap()
	eq(t, "the staged copy went", exists(f), false)
	eq(t, "and no notice", exists(filepath.Join(box, expiry.Dir)), false)
}

func TestAMailboxNobodyCanRead(t *testing.T) {
	x := twoRuns(t)
	x.mustMint("svc.gone@acme.ssh", "gone")
	x.aged(alice, "svc.gone@acme.ssh", "m1", "--subject v")
	x.h.Reap()
	eq(t, "while it is claimed, a notice", isDir(x.expiredDir("svc.gone@acme.ssh")), true)
	out, rc := x.mint("", "rm", "svc.gone@acme.ssh")
	eq(t, "removed: "+out, rc, 0)
	x.h.Reap()
	eq(t, "removed identity: .expired goes", isDir(x.expiredDir("svc.gone@acme.ssh")), false)

	// an unreadable mailbox's mail goes uncounted
	nobody := "svc.nobody@acme.ssh"
	os.MkdirAll(x.h.mbox(nobody), 0o2770)
	f := filepath.Join(x.h.mbox(nobody), "m."+msgExt)
	os.WriteFile(f, []byte("From: "+alice+"\n\nx\n"), 0o640)
	x.age(nobody, "m")
	x.h.Reap()
	eq(t, "its mail goes as it always did", exists(f), false)
	eq(t, "with no notice", exists(x.expiredDir(nobody)), false)

	// an instance: the address takes instances, so its mailbox can still be read
	x.mustMint("group.g@acme.ssh", "ggg", "--instances")
	inst := "group.g+i1@acme.ssh"
	x.dis("group.g@acme.ssh", "register i1", x.key("i1").line)
	code(t, "admit", x.dis("group.g@acme.ssh", "admit i1"), 0)
	code(t, "stored, nobody following", x.dis(alice, "send "+inst+" --id m2 --subject v -- x"), 0)
	x.age(inst, "m2")
	x.h.Reap()
	eq(t, "an instance gets one", isDir(x.expiredDir(inst)), true)
	code(t, "released", x.dis("group.g@acme.ssh", "release i1"), 0)
	x.h.Reap()
	eq(t, "released, its address still there: it stays", isDir(x.expiredDir(inst)), true)
	r := x.dis("group.g@acme.ssh", "instances")
	has(t, "a notice is not mail to instances", r.out, inst+"\tmail=0\t")
	out, rc = x.mint("", "rm", "group.g@acme.ssh")
	eq(t, "the address removed: "+out, rc, 0)
	x.h.Reap()
	eq(t, "and its address gone: .expired goes", isDir(x.expiredDir(inst)), false)
}

func TestALapsedClaimStillGetsNotices(t *testing.T) {
	x := twoRuns(t)
	x.mustMint("svc.lapsed@acme.ssh", "lap")
	x.aged(alice, "svc.lapsed@acme.ssh", "m1", "--subject v")
	rec := x.h.claimFind("svc.lapsed@acme.ssh")[0]
	os.WriteFile(rec, []byte(regexp.MustCompile(`expires=\d+`).ReplaceAllString(readString(rec), "expires=1")), 0o640)
	x.h.Reap()
	eq(t, "its mailbox is the address", isDir(x.expiredDir("svc.lapsed@acme.ssh")), true)
}

func TestAClaimWithNoFromIsLoggedAndDeleted(t *testing.T) {
	x := twoRuns(t)
	box := x.h.mbox(bob)
	os.MkdirAll(box, 0o2770)
	os.WriteFile(filepath.Join(box, "x."+msgExt), []byte("Date: 2026-11-01T00:00:00Z\n\nbody\n"), 0o640)
	x.age(bob, "x")
	os.WriteFile(x.h.journal, nil, 0o660)
	x.h.Reap()
	eq(t, "no message left", exists(filepath.Join(box, "x."+msgExt)), false)
	eq(t, "no claim", exists(filepath.Join(box, expiry.Dir, ".claim.x")), false)
	eq(t, "no notice", x.dis(bob, "recv --peek").out, empty)
	has(t, "logged", readString(x.h.journal), "has no From")
}

func TestABadNoticeFile(t *testing.T) {
	x := twoRuns(t)
	x.aged(alice, bob, "m1", "--subject v")
	x.h.Reap()
	name := expiry.FileName(alice)
	p := filepath.Join(x.expiredDir(bob), name)
	os.WriteFile(p, []byte("{not json"), 0o660)
	os.WriteFile(x.h.journal, nil, 0o660)
	eq(t, "a read skips it", x.dis(bob, "recv --peek").out, empty)
	eq(t, "moved aside", exists(filepath.Join(x.expiredDir(bob), ".bad."+name)), true)
	has(t, "logged", readString(x.h.journal), "bad notice")
	os.WriteFile(p, []byte("{not json"), 0o660)
	x.aged(alice, bob, "m2", "--subject v")
	x.h.Reap()
	has(t, "the count starts again", x.dis(bob, "recv --peek").out, "\nExpired: 1\n")
}

func TestACrashAtEachStep(t *testing.T) {
	for point, want := range map[string]int{"claimed": 2, "counted": 4, "deleted": 2} {
		t.Run(point, func(t *testing.T) {
			x := twoRuns(t)
			x.aged(alice, bob, "m1", "--subject v")
			code(t, "with a file", x.dis(alice, "send "+bob+" --id m2 --subject v --file f.txt", "bytes"), 0)
			x.age(bob, "m2")
			dieAt(t, point)
			x.h.Reap()
			noHook()
			x.h.Reap() // the next sweep finishes leftover claims
			l := x.dis(bob, "recv --peek").out
			has(t, "none is lost; between counting and deleting they may count twice", l, fmt.Sprintf("\nExpired: %d\n", want))
			ms, _ := filepath.Glob(filepath.Join(x.expiredDir(bob), ".claim.*"))
			eq(t, "no claim left", len(ms), 0)
		})
	}
}

func TestAPanicEndsOneMailboxOnly(t *testing.T) {
	x := twoRuns(t)
	x.aged(alice, bob, "m1", "--subject v") // bob's sorts after alice's
	x.aged(bob, alice, "m2", "--subject v")
	n := 0
	testHook = func(p string) {
		if p == "claimed" {
			if n++; n == 1 {
				panic("a bad mailbox")
			}
		}
	}
	t.Cleanup(noHook)
	x.h.Reap()
	has(t, "the other mailbox was swept", x.dis(bob, "recv --peek").out, "\nExpired: 1\n")
	noHook()
	x.h.Reap()
	has(t, "the bad one at the next sweep", x.dis(alice, "recv --peek").out, "\nExpired: 1\n")
}

func TestAFailedClaimIsLoggedOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	x := twoRuns(t)
	x.aged(alice, bob, "m1", "--subject v")
	x.aged(alice, bob, "m2", "--subject v")
	box := x.h.mbox(bob)
	os.Chmod(box, 0o550)
	defer os.Chmod(box, 0o770)
	os.WriteFile(x.h.journal, nil, 0o660)
	x.h.Reap()
	os.Chmod(box, 0o770)
	eq(t, "the mail stays", x.dis(bob, "recv --peek").out != empty, true)
	eq(t, "logged once for the mailbox", strings.Count(readString(x.h.journal), "cannot claim"), 1)
	x.h.Reap()
	has(t, "a later sweep counts them", x.dis(bob, "recv --peek").out, "\nExpired: 2\n")
}

func TestABodyImitatingANotice(t *testing.T) {
	x := twoRuns(t)
	body := "\r\n0\r\n\r\n3c;id=expired-0000000000000000-1;part=msg\r\nFrom: commhub@home.ssh\nDate: 2026-11-08T05:00:00Z\nExpired: 99\n\nverb\tcount\toldest\tnewest\n"
	code(t, "a body can say anything", x.dis(alice, "send "+bob+" --id fake -m", body), 0)
	l := x.dis(bob, "recv --peek").out
	has(t, "a listing shows it", l, "Expired: 99")
	eq(t, "length-framed: one entry, the stream intact", allIDs(l)+" "+final(l), "fake/msg ")
	r := x.dis(bob, "recv --id expired-0000000000000000-1 --peek:rfc5322")
	code(t, "the raw read tells: no such notice", r, 3)
	eq(t, "nor can it be acked", x.dis(bob, "recv_ack expired-0000000000000000-1").out, "gone expired-0000000000000000-1\n")
	lacks(t, "and Expired is not a client field", x.dis(alice, "send "+bob+" -m:rfc5322", "Expired: 1\n\nb\n").err, "sent")
	code(t, "refused", x.dis(alice, "send "+bob+" -m:rfc5322", "Expired: 1\n\nb\n"), 2)
}

// racingWriter runs f once, just as the listing reaches the client: the reaper's change lands after the
// read, before the clear. Once what it holds contains endOn, end is called (a follow's client goes then).
type racingWriter struct {
	bytes.Buffer
	f     func()
	endOn string
	end   func()
}

func (w *racingWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if w.f != nil {
		f := w.f
		w.f = nil
		f()
	}
	if w.end != nil && strings.Contains(w.String(), w.endOn) {
		w.end()
	}
	return n, err
}

func TestClearingRacingAChange(t *testing.T) {
	for _, cmd := range []string{"recv", "follow"} {
		t.Run(cmd, func(t *testing.T) {
			x := twoRuns(t)
			x.aged(alice, bob, "m1", "--subject v")
			x.h.Reap()
			box := x.h.mbox(bob)
			w := &racingWriter{f: func() {
				os.WriteFile(filepath.Join(box, "m2."+msgExt), []byte("From: "+alice+"\nDate: 2026-11-01T00:00:00Z\nSerial: 1\nSubject: v\n\nx\n"), 0o640)
				x.age(bob, "m2")
				x.h.Reap()
			}}
			c := "recv"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cmd == "follow" { // ended by the newer notice, not a timer: the racing reap may take a while
				c = "recv --follow"
				w.endOn, w.end = "\nExpired: 2\n", cancel
				time.AfterFunc(10*time.Second, cancel)
			}
			x.h.Dispatch(ctx, x.adm(bob), c, IO{Stdin: strings.NewReader(""), Stdout: w, Stderr: io.Discard})
			has(t, "what was read was the first", w.String(), "\nExpired: 1\n")
			if cmd == "recv" {
				has(t, "the changed notice stays, cumulative", x.dis(bob, "recv --peek").out, "\nExpired: 2\n")
			} else {
				has(t, "the follow prints the newer one, cumulative", w.String(), "\nExpired: 2\n")
			}
		})
	}
}

func TestAFollowPrintsANoticeItCannotClearOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens anything")
	}
	x := twoRuns(t)
	x.aged(alice, bob, "m0", "--subject v")
	x.h.Reap()
	lk := filepath.Join(x.expiredDir(bob), ".lock")
	os.Chmod(lk, 0)
	defer os.Chmod(lk, 0o660)
	f := x.start(x.adm(bob), "recv --follow", "")
	waitFor(t, 2*time.Second, "the notice", func() bool { return strings.Contains(f.out.String(), "Expired: 1") })
	code(t, "a message wakes it", x.dis(alice, "send "+bob+" --id m1 -- x"), 0)
	code(t, "another", x.dis(alice, "send "+bob+" --id m2 -- x"), 0)
	waitFor(t, 2*time.Second, "the messages", func() bool { return strings.Contains(f.out.String(), ";id=m2;") })
	eq(t, "printed once", strings.Count(f.out.String(), "Expired: 1"), 1)
	f.cancel()
	f.wait(t, 2*time.Second)
}

func TestAVanishedClientFindsItsNoticeAgain(t *testing.T) {
	x := twoRuns(t)
	x.aged(alice, bob, "m1", "--subject v")
	x.h.Reap()
	x.h.Dispatch(context.Background(), x.adm(bob), "recv", IO{Stdin: strings.NewReader(""), Stdout: failingWriter{}, Stderr: io.Discard})
	has(t, "not consumed", x.dis(bob, "recv --peek").out, "\nExpired: 1\n")
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// A claim change under the registry lock (a claim gone for a moment, then back) is judged after the lock.
func TestRemovingNoticesWaitsForTheRegistry(t *testing.T) {
	x := twoRuns(t)
	id := "svc.r@acme.ssh"
	x.mustMint(id, "rrr")
	x.aged(alice, id, "m1", "--subject v")
	x.h.Reap()
	rec := x.h.claimFind(id)[0]
	saved := readString(rec)
	unlock, err := x.h.lockRegistry()
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(rec)
	done := make(chan struct{})
	go func() { x.h.expireMail(id, x.h.mbox(id), nil); close(done) }()
	time.Sleep(100 * time.Millisecond)
	eq(t, "it waits", isDir(x.expiredDir(id)), true)
	os.WriteFile(rec, []byte(saved), 0o640)
	unlock()
	<-done
	eq(t, "and judges again: readable after all", isDir(x.expiredDir(id)), true)
}

func TestAMailboxGoneIsNotBroughtBack(t *testing.T) {
	x := twoRuns(t)
	x.mustMint("svc.old@acme.ssh", "olk")
	x.aged(alice, "svc.old@acme.ssh", "m1", "--subject v")
	box := x.h.mbox("svc.old@acme.ssh")
	os.Rename(box, box+".away")
	x.h.expireMail("svc.old@acme.ssh", box, []string{"m1"})
	eq(t, "not recreated", exists(box), false)
}

func TestModesUnderTheDaemon(t *testing.T) {
	x := twoRuns(t)
	x.aged(alice, bob, "m1", "--subject v")
	x.h.Reap()
	fi, _ := os.Stat(x.expiredDir(bob))
	eq(t, ".expired", fi.Mode()&(os.ModeSetgid|os.ModePerm), os.ModeSetgid|0o770)
	fi, _ = os.Stat(filepath.Join(x.expiredDir(bob), expiry.FileName(alice)))
	eq(t, "the notice", fi.Mode().Perm(), os.FileMode(0o660))
}

// One fixture with a table of each kind, through every read of a notice.
func TestOneFixtureInEveryRead(t *testing.T) {
	x := twoRuns(t)
	x.mustMint("svc.s@acme.ssh", "ssk")
	tp := "topic+b@acme.ssh"
	x.mkTopic(tp, alice)
	sub := strings.TrimSpace(x.dis(bob, "sub "+tp).out)
	mk := func(box, mid, from, fields string) {
		os.MkdirAll(x.h.mbox(box), 0o2770)
		os.WriteFile(filepath.Join(x.h.mbox(box), mid+"."+msgExt), []byte("From: "+from+"\nDate: 2026-11-01T0"+fmt.Sprint(len(mid)%10)+":00:00Z\nSerial: 1\n"+fields+"\nx\n"), 0o640)
		x.age(box, mid)
	}
	for _, box := range []string{bob, sub} {
		for i := 0; i < 24; i++ {
			mk(box, fmt.Sprintf("v%d", i), alice, fmt.Sprintf("Subject: out.%c%d\n", 'a'+i%3, i))
		}
		mk(box, "long", alice, "Subject: "+strings.Repeat("l", 20)+"."+strings.Repeat("m", 20)+"\n")
		mk(box, "gap", alice, "Gap: r\n")
		mk(box, "bad", alice, "Subject: nön\n")
		mk(box, "none", "svc.s@acme.ssh", "")
	}
	code(t, "fresh", x.dis(alice, "send "+bob+" --id zz -- hi"), 0)
	x.h.Reap()
	l := x.dis(bob, "recv --peek").out
	has(t, "the fixture has a table of each", l, "verb\tcount")
	has(t, "and the specials", l, "[gap]\t1\t")
	has(t, "and [others]", l, "[others]\t")
	has(t, "and [no-subject]", l, "[no-subject]\t1\t")
	code(t, "the subscription's listing", x.dis(bob, "recv "+sub+" --peek"), 0)
	for _, id := range noticeIDRE.FindAllStringSubmatch(l, -1) {
		r := x.dis(bob, "recv --id "+id[1]+" --peek:rfc5322")
		code(t, "raw", r, 0)
		has(t, "raw: its count", r.out, "Expired: ")
		code(t, "--peek:file of a notice", x.dis(bob, "recv --id "+id[1]+" --peek:file"), 2)
		code(t, "recv_bag of a notice", x.dis(bob, "recv_bag --id "+id[1]), 2)
		eq(t, "--id: the notice as a msg chunk", allIDs(x.dis(bob, "recv --id "+id[1]+" --peek").out), id[1]+"/msg")
	}
	code(t, "raw of no notice", x.dis(bob, "recv --id expired-0000000000000000-1 --peek:rfc5322"), 3)
	eq(t, "recv_ack of no notice: gone", x.dis(bob, "recv_ack expired-0000000000000000-1").out, "gone expired-0000000000000000-1\n")
	id := noticeID(x.dis(bob, "recv "+sub+" --peek").out)
	has(t, "raw on a subscription: the same", x.dis(bob, "recv "+sub+" --id "+id+" --peek:rfc5322").out, "Expired: ")

	// a later sweep adds to the notice
	mk(bob, "more", alice, "Subject: out.a0\n")
	x.h.Reap()
	has(t, "a later sweep's count", x.dis(bob, "recv --peek").out, "\nExpired: 28\n")
	code(t, "a plain recv consumes", x.dis(bob, "recv"), 0)
	eq(t, "and then there is none", x.dis(bob, "recv --peek").out, empty)
}

func TestSubjectAndIDRefusals(t *testing.T) {
	x := twoRuns(t)
	for _, c := range [][2]string{
		{"send " + bob + " --subject", ""},
		{"send " + bob + " --subject .bad -- x", ""},
		{"send " + bob + " --subject ok -m:rfc5322", "Subject: x\n\nb\n"},
		{"send " + bob + " --id expired-1 -- x", ""},
		{"send " + bob + " --live --id expired-1 -m", ""},
		{"send_bag " + bob + " --subject", ""},
		{"send_bag " + bob + " --subject .bad", ""},
		{"send_bag " + bob + " --id expired-1", ""},
	} {
		g := x.dis(alice, c[0], c[1])
		eq(t, c[0]+": exit 2", g.code, 2)
		has(t, c[0]+": says why", g.err, "dispatch: ")
	}
}

// --- the notice probe ------------------------------------------------------------------------------------

var probeTimeRE = regexp.MustCompile(`-[0-9]+000000000;part=msg\r\nFrom: (\S+)\nDate: \S+`)

// probeForm: a stream with a probe's time taken out.
func probeForm(s string) string {
	return probeTimeRE.ReplaceAllString(s, "-T;part=msg\r\nFrom: $1\nDate: D")
}

func (x *th) setProbe(v string) {
	x.t.Helper()
	if err := os.WriteFile(filepath.Join(x.state, "settings"), []byte("notice_probe="+v+"\n"), 0o640); err != nil {
		x.t.Fatal(err)
	}
}

// followFirst: what a daemon follow prints until marker appears.
func (x *th) followFirst(me, cmd, marker string) string {
	x.t.Helper()
	f := x.start(x.adm(me), cmd, "")
	waitFor(x.t, 2*time.Second, marker, func() bool { return strings.Contains(f.out.String(), marker) })
	o := f.out.String()
	f.cancel()
	f.wait(x.t, 2*time.Second)
	return o
}

func TestProbeOnAFollow(t *testing.T) {
	x := twoRuns(t)
	h := "expired-" + fmt.Sprintf("%x", sha256.Sum256([]byte(bob)))[:16]
	tp := "topic+pr@acme.ssh"
	x.mkTopic(tp, alice)
	sub := strings.TrimSpace(x.dis(bob, "sub "+tp).out)
	x.setProbe("nobody@* " + bob)
	own := fmt.Sprintf("%x;id=", len("From: "+bob+"\nDate: 2026-11-08T00:00:00Z\nExpired: 0\n\n")) + h + "-T;part=msg\r\nFrom: " + bob + "\nDate: D\nExpired: 0\n\n\r\n"
	code(t, "backlog", x.dis(alice, "send "+bob+" --id m1 -- hi"), 0)

	o := probeForm(x.followFirst(bob, "recv --follow --peek", ";id=m1;"))
	if !strings.HasPrefix(o, own) {
		t.Errorf("the probe first, whole: %q", o)
	}
	eq(t, "one", strings.Count(o, "Expired: 0"), 1)
	eq(t, "a new connection: one again", strings.Count(x.followFirst(bob, "recv --follow --peek", ";id=m1;"), "Expired: 0"), 1)

	code(t, "published", x.dis(alice, "send "+tp+" --id "+ev(idA, 1)+" --subject e -- ev"), 0)
	o = probeForm(x.followFirst(bob, "recv "+sub+" --follow --peek", ";id="+ev(idA, 1)+";"))
	if want := own; !strings.HasPrefix(o, want) {
		t.Errorf("in a subscription, nothing about the session: %q", o)
	}

	eq(t, "nothing stored", isDir(x.expiredDir(bob)), false)
	lacks(t, "plain recv unchanged", x.dis(bob, "recv --peek").out, "Expired: 0")
	lacks(t, "nor a listing of the subscription", x.dis(bob, "recv "+sub+" --peek").out, "Expired: 0")

	// a follow that consumes: the probe, then the message; the probe left nothing behind
	o = x.followFirst(bob, "recv --follow", ";id=m1;")
	id := noticeID(o)
	if !probeTimeRE.MatchString(o) || !strings.HasPrefix(id, h+"-") {
		t.Errorf("a consuming follow prints it too: %q", o)
	}
	eq(t, "still nothing stored", isDir(x.expiredDir(bob)), false)

	r := x.dis(bob, "recv_ack "+id)
	eq(t, "recv_ack of a probe: acked", fmt.Sprint(r.code, " ", r.out+r.err), "0 acked "+id+"\n")
	r = x.dis(bob, "recv_ack "+sub+" "+id)
	eq(t, "of it in a subscription: acked", fmt.Sprint(r.code, " ", r.out+r.err), "0 acked "+id+"\n")
	x.setProbe("")
	eq(t, "the setting off: an ack that was on its way still is one", x.dis(bob, "recv_ack "+id).out, "acked "+id+"\n")

	code(t, "--peek:rfc5322 of a probe", x.dis(bob, "recv --id "+id+" --peek:rfc5322"), 3)
	eq(t, "--id of a probe: part=missing", allIDs(x.dis(bob, "recv --id "+id).out), id+"/missing")
	code(t, "recv_bag of a probe", x.dis(bob, "recv_bag --id "+id), 2)
	eq(t, "another identity's: gone", x.dis(alice, "recv_ack "+id).out, "gone "+id+"\n")
	eq(t, "not a whole second: gone", x.dis(bob, "recv_ack "+h+"-1793999999000000001").out, "gone "+h+"-1793999999000000001\n")
	eq(t, "the zero second: gone", x.dis(bob, "recv_ack "+h+"-0").out, "gone "+h+"-0\n")
}

func TestProbeSetting(t *testing.T) {
	x := twoRuns(t)
	code(t, "backlog", x.dis(alice, "send "+bob+" --id m1 -- hi"), 0)
	shown := func() bool {
		return strings.Contains(x.followFirst(bob, "recv --follow --peek", ";id=m1;"), "Expired: 0")
	}
	eq(t, "no settings file: off", shown(), false)
	for _, c := range []struct {
		v    string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"run.t+bob/*", true},
		{"*@acme.ssh", true},
		{"run.t+bo?/ws-b@acme.ssh", true},
		{"  nobody@*   *@acme.ssh  ", true},
		{"run.t+alice/*", false},
		{"*@acme.ssh BAD", false},
		{"*@acme.ssh\t", false},
		{"*@acme.ssh [a]", false},
		{"*@acme.ssh !x", false},
		{"*@acme.ssh " + strings.Repeat("a", 129), false},
		{strings.Repeat("x ", 16) + "*@acme.ssh", false},
		{strings.Repeat("x ", 15) + "*@acme.ssh", true},
	} {
		x.setProbe(c.v)
		eq(t, fmt.Sprintf("notice_probe=%q", c.v), shown(), c.want)
	}
}

func TestProbeOnBothKindsOfMailbox(t *testing.T) {
	x := twoRuns(t)
	tp := "topic+pb@acme.ssh"
	x.mkTopic(tp, alice)
	sub := strings.TrimSpace(x.dis(bob, "sub "+tp).out)
	x.setProbe("*@acme.ssh")
	code(t, "mail", x.dis(alice, "send "+bob+" --id m1 --subject v -- hi"), 0)
	code(t, "an event", x.dis(alice, "send "+tp+" --subject e -- ev"), 0)
	for _, c := range []struct{ cmd, marker string }{
		{"recv --follow --peek", "\n\nhi\r\n"},
		{"recv " + sub + " --follow --peek", "\n\nev\r\n"},
	} {
		has(t, c.cmd+": a probe", x.followFirst(bob, c.cmd, c.marker), "Expired: 0\n\n\r\n")
	}
	x.setProbe("")
	id := "expired-" + fmt.Sprintf("%x", sha256.Sum256([]byte(bob)))[:16]
	for _, c := range []struct {
		cmd  string
		want int
		out  string
	}{
		{"recv_ack " + id + "-1793999999000000000", 0, "acked " + id + "-1793999999000000000\n"},
		{"recv_ack " + sub + " " + id + "-1793999999000000000", 0, "acked " + id + "-1793999999000000000\n"},
		{"recv_ack " + id + "-1793999999000000001", 0, "gone " + id + "-1793999999000000001\n"},
		{"recv --id " + id + "-1793999999000000000 --peek:rfc5322", 3, ""},
	} {
		r := x.dis(bob, c.cmd)
		code(t, c.cmd, r, c.want)
		eq(t, c.cmd+": stdout", r.out, c.out)
	}
}
