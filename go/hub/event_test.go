package hub

// Event ids: the publish table, the record and its boot id, a hub restart, done and the position notice,
// repeated msgids, listing order across sessions, a failed record write, and the races of a fan-out
// (docs/wake-live-event-id.md, Fan-out, Event ids, Position notice).

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const evTopic = "topic+ev@acme.ssh"

// evRig: alice publishes to evTopic; bob and alice each hold a subscription.
func evRig(t *testing.T) (x *th, sb, sa string) {
	x = twoRuns(t)
	x.mkTopic(evTopic, alice)
	sb = strings.TrimSpace(x.dis(bob, "sub "+evTopic).out)
	sa = strings.TrimSpace(x.dis(alice, "sub "+evTopic).out)
	return x, sb, sa
}

func (x *th) record(topic string) string { return readString(x.h.recordPath(topic)) }

// recSeq: the record file's session and seq, "<id> <seq>\n", its boot id checked to be the hub's.
func (x *th) recSeq(topic string) string {
	f := strings.Fields(x.record(topic))
	if len(f) != 3 || f[2] != x.h.bootID {
		return "malformed: " + x.record(topic)
	}
	return f[0] + " " + f[1] + "\n"
}

// evs: a listing's event msgids as "session/seq", space-joined.
func evs(l string) string {
	var out []string
	for _, m := range strings.Fields(ids(l)) {
		if ev, ok := parseEventMsgid(m); ok {
			out = append(out, fmt.Sprintf("%s/%d", ev.id, ev.seq))
		}
	}
	return strings.Join(out, " ")
}

// seqs: a listing's event seqs, space-joined.
func seqs(l string) string {
	var out []string
	for _, m := range strings.Fields(ids(l)) {
		if ev, ok := parseEventMsgid(m); ok {
			out = append(out, fmt.Sprint(ev.seq))
		}
	}
	return strings.Join(out, " ")
}

// heads: a listing's position notices' msgids.
func heads(l string) string {
	var out []string
	for _, c := range chunks(l) {
		if c.part == "msg" && strings.HasPrefix(c.id, HeadPrefix) {
			out = append(out, c.id)
		}
	}
	return strings.Join(out, " ")
}

const idA, idB = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"

func ev(id string, seq int64) string { return evID{id, seq}.msgid() }

func TestPublishTable(t *testing.T) {
	x, sb, sa := evRig(t)
	pub := func(id string) string {
		f := ""
		if id != "" {
			f = " --id " + id
		}
		r := x.dis(alice, "send "+evTopic+f+" -- x")
		return fmt.Sprint(r.code, "|", strings.TrimSpace(r.out))
	}
	ok := func(id string) string { return "0|published -> " + evTopic + " msgid=" + id + " subs=2" }
	eq(t, "no record yet: head-none, alone", allIDs(x.dis(bob, "recv "+sb+" --peek").out), "head-none/msg")
	eq(t, "(with no header)", x.dis(bob, "recv "+sb+" --peek").out, "34;id=head-none;part=msg\r\nFrom: "+evTopic+"\nDate: "+isoTime(x.h.epoch())+"\n\n\r\n0\r\n\r\n")
	r := pub("")
	minted := x.h.topicDone(evTopic).id
	eq(t, "no --id, record none: a minted session, seq 1", r, ok(ev(minted, 1)))
	eq(t, "16 hex", regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(minted), true)
	eq(t, "the record: session, seq, the boot id", x.record(evTopic), minted+" 1 "+x.h.bootID+"\n")
	eq(t, "no --id: (s, q+1)", pub(""), ok(ev(minted, 2)))
	eq(t, "(i, p), i ≠ s: taken", pub(ev(idA, 5)), ok(ev(idA, 5)))
	eq(t, "(s, q+1)", pub(ev(idA, 6)), ok(ev(idA, 6)))
	eq(t, "(s, p), p > q+1", pub(ev(idA, 9)), ok(ev(idA, 9)))
	eq(t, "(s, q): dup, nothing delivered", pub(ev(idA, 9)), "0|dup")
	eq(t, "(s, p), p < q: stale", pub(ev(idA, 3)), "0|stale: last "+ev(idA, 9))
	eq(t, "no --id continues the publisher's session", pub(""), ok(ev(idA, 10)))
	l := x.dis(bob, "recv "+sb+" --peek").out
	want := fmt.Sprintf("%s/1 %s/2 %s/5 %s/6 %s/9 %s/10", minted, minted, idA, idA, idA, idA)
	eq(t, "every delivery's msgid is its publication's", evs(l), want)
	eq(t, "the same msgid in every subscription", evs(x.dis(alice, "recv "+sa+" --peek").out), want)
	eq(t, "one inode per event: hard links", fileIno(filepath.Join(x.h.mbox(sb), ev(idA, 10)+".msg")) == fileIno(filepath.Join(x.h.mbox(sa), ev(idA, 10)+".msg")), true)
	eq(t, "the record as written last", x.recSeq(evTopic), idA+" 10\n")
	eq(t, "done: the last assigned", x.h.topicDone(evTopic).String(), ev(idA, 10))
	eq(t, "no position notice when the mailbox holds done", heads(l), "")
	x.dis(bob, "recv_ack "+sb+" "+ev(idA, 10))
	l = x.dis(bob, "recv "+sb+" --peek").out
	eq(t, "the position notice once the mailbox lacks done", heads(l), "head-"+idA+"-10")
	if !strings.HasSuffix(l, ";id=head-"+idA+"-10;part=msg\r\nFrom: "+evTopic+"\nDate: "+isoTime(x.h.epoch())+"\n\n\r\n0\r\n\r\n") {
		t.Errorf("the position notice last, with no header: %q", l)
	}
	// a record of none, with an --id: taken as given
	tn := "topic+ev2@acme.ssh"
	x.mkTopic(tn, alice)
	x.dis(bob, "sub "+tn)
	eq(t, "(i, p) on none", fmt.Sprint(x.dis(alice, "send "+tn+" --id "+ev(idB, 7)+" -- x").code, x.recSeq(tn)), "0"+idB+" 7\n")
	// a gap marker takes a seq like any other
	code(t, "a gap marker", x.dis(alice, "send "+evTopic+" -m:rfc5322", "Gap: lost some\n\n"), 0)
	eq(t, "takes the next seq", x.h.topicDone(evTopic).String(), ev(idA, 11))
}

func TestEventIDGrammar(t *testing.T) {
	x, sb, _ := evRig(t)
	for _, bad := range []string{"x", "m-1", "event-" + idA, "event-" + idA + "-0", "event-" + idA + "--1", "event-AAAAAAAAAAAAAAAA-1",
		"event-" + idA[:15] + "-1", "event-" + idA + "-9223372036854775808", "event-" + idA + "-01", "event-" + idA + "-1x"} {
		code(t, "a publish with --id "+bad+": 2", x.dis(alice, "send "+evTopic+" --id "+bad+" -- x"), 2)
	}
	eq(t, "nothing assigned", x.h.topicDone(evTopic).String(), "none")
	code(t, "int64 max parses", x.dis(alice, "send "+evTopic+" --id "+ev(idA, math.MaxInt64)+" -- x"), 0)
	code(t, "no --id past int64 max", x.dis(alice, "send "+evTopic+" -- x"), 0)
	d := x.h.topicDone(evTopic)
	eq(t, "a new session, seq 1", fmt.Sprint(d.id != idA, d.seq), "true 1")
	for _, p := range []string{"expired-x", "wakes-x", "head-x", "head-none"} {
		code(t, "reserved "+p+" to a topic: 2", x.dis(alice, "send "+evTopic+" --id "+p+" -- x"), 2)
		code(t, "reserved "+p+" to an identity: 2", x.dis(alice, "send "+bob+" --id "+p+" -- x"), 2)
	}
	code(t, "event-* to an identity: 2", x.dis(alice, "send "+bob+" --id "+ev(idA, 1)+" -- x"), 2)
	code(t, "event-* on send_bag to an identity: 2", x.dis(alice, "send_bag "+bob+" --id "+ev(idA, 1), string(makeTar(t, map[string]string{"a": "b"}))), 2)
	code(t, "send_bag to a topic takes it", x.dis(alice, "send_bag "+evTopic+" --id "+ev(idB, 4), string(makeTar(t, map[string]string{"a": "b"}))), 0)
	has(t, "its msgid", ids(x.dis(bob, "recv "+sb+" --peek").out), ev(idB, 4))
	eq(t, "the record", x.recSeq(evTopic), idB+" 4\n")
	for i, form := range []struct{ cmd, in string }{
		{"-m", "x"}, {"-m:rfc5322", "Subject: s\n\nb\n"}, {"--file f", "data"}, {"--chunked -m", string(frame([]byte("x"), 4, ""))}, {"--size 1 -m", "x"},
	} {
		code(t, "form "+form.cmd, x.dis(alice, fmt.Sprintf("send %s --id %s %s", evTopic, ev(idB, int64(5+i)), form.cmd), form.in), 0)
	}
	eq(t, "five more", x.recSeq(evTopic), idB+" 9\n")
	lacks(t, "the body untouched: the msgid is no field", x.stored(sb, ev(idB, 5)), "event-")
}

// The record across a hub restart on the same boot: a topic keeps its session and continues at seq+1;
// done = the record read. On another boot the hub's next stamp is a new session; a publisher's own --id is
// honoured as at any time.
func TestRecordAcrossRestart(t *testing.T) {
	x, sb, _ := evRig(t)
	x.dis(alice, "send "+evTopic+" --id "+ev(idA, 41)+" -- x")
	x.restart()
	eq(t, "done after a restart: the record read", x.h.topicDone(evTopic).String(), ev(idA, 41))
	x.dis(bob, "recv "+sb)
	eq(t, "the position notice gives it", heads(x.dis(bob, "recv "+sb).out), "head-"+idA+"-41")
	x.restart()
	x.dis(alice, "send "+evTopic+" -- x")
	eq(t, "continues at seq+1", evs(x.dis(bob, "recv "+sb+" --peek").out), idA+"/42")
	eq(t, "a retry of the publisher's last after a restart: dup", strings.TrimSpace(x.dis(alice, "send "+evTopic+" --id "+ev(idA, 42)+" -- x").out), "dup")
	// another boot: the hub mints a new session at its next own stamp
	x.restart()
	x.h.bootID = "another-boot"
	eq(t, "done is still the record's", x.h.topicDone(evTopic).String(), ev(idA, 42))
	eq(t, "a publisher's own --id on another boot: honoured", strings.TrimSpace(x.dis(alice, "send "+evTopic+" --id "+ev(idA, 43)+" -- x").out), "published -> "+evTopic+" msgid="+ev(idA, 43)+" subs=2")
	eq(t, "the record now carries this boot", strings.Fields(x.record(evTopic))[2], "another-boot")
	x.restart()
	x.h.bootID = "a-third-boot"
	x.dis(alice, "send "+evTopic+" -- x")
	d := x.h.topicDone(evTopic)
	eq(t, "the hub's own stamp on another boot: a new session at 1", fmt.Sprint(d.id != idA, d.seq), "true 1")
	eq(t, "and its next continues it", strings.TrimSpace(x.dis(alice, "send "+evTopic+" -- y").out), "published -> "+evTopic+" msgid="+ev(d.id, 2)+" subs=2")
	// a malformed record: none, logged, overwritten at the next publication
	for _, bad := range []string{"garbage\n", idA + " 42\n", idA + " 42 b 7\n", idA + " 042 b\n", idA + " 42 b"} {
		os.WriteFile(x.h.recordPath(evTopic), []byte(bad), 0o640)
		x.restart()
		eq(t, "malformed "+bad+": none", x.h.topicDone(evTopic).String(), "none")
	}
	x.dis(alice, "send "+evTopic+" -- x")
	eq(t, "overwritten", regexp.MustCompile(`^[0-9a-f]{16} 1 `+regexp.QuoteMeta(x.h.bootID)+`\n$`).MatchString(x.record(evTopic)), true)
	// removed with the topic; a topic added again starts afresh, though the hub kept the old one in memory
	x.bash(`rm -rf "$HUB_ROOT/topics/$1"`, evTopic)
	x.mkTopic(evTopic, alice)
	eq(t, "a topic added again: none", x.h.topicDone(evTopic).String(), "none")
}

// The record is a temp file renamed over, never fsynced; the kernel's boot id is in it.
func TestRecordFile(t *testing.T) {
	x, _, _ := evRig(t)
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	eq(t, "the hub's boot id is the kernel's", x.h.bootID, strings.TrimSpace(string(b)))
	x.dis(alice, "send "+evTopic+" -- x")
	eq(t, "no temp left", strings.Join(globNamesAll(x.h.topicDir(evTopic)), " "), "policy record subs")
}

// A failed record write: exit 1, nothing linked, the stage removed, the record unchanged; the retry is no dup.
func TestFailedRecordWrite(t *testing.T) {
	x, sb, _ := evRig(t)
	x.dis(alice, "send "+evTopic+" --id "+ev(idA, 1)+" -- x")
	for _, point := range []string{"record-tmp", "record-rename"} {
		testErr = func(p string) error {
			if p == point {
				return errors.New("injected")
			}
			return nil
		}
		r := x.dis(alice, "send "+evTopic+" --id "+ev(idA, 2)+" -- x")
		testErr = func(string) error { return nil }
		eq(t, point+": exit 1, retryable", fmt.Sprint(r.code, "|", r.out, "|", r.err), "1||"+errStore+"\n")
		eq(t, point+": nothing linked", evs(x.dis(bob, "recv "+sb+" --peek").out), idA+"/1")
		eq(t, point+": the record unchanged", x.recSeq(evTopic), idA+" 1\n")
		eq(t, point+": no stage, no temp left", fmt.Sprint(len(x.stages()), x.leftOf(evTopic)), "0policy subs|")
		eq(t, point+": in memory too", x.h.topicDone(evTopic).String(), ev(idA, 1))
	}
	code(t, "the retry: delivered, not dup", x.dis(alice, "send "+evTopic+" --id "+ev(idA, 2)+" -- x"), 0)
	eq(t, "(delivered)", evs(x.dis(bob, "recv "+sb+" --peek").out), idA+"/1 "+idA+"/2")
}

// Listing order: arrival (not by name): within a session that is seq order; publishers interleaving
// sessions are listed as they published.
func TestListingOrder(t *testing.T) {
	x, sb, _ := evRig(t)
	for _, q := range []int64{9, 10, 100} {
		x.dis(alice, "send "+evTopic+" --id "+ev(idB, q)+" -- x")
	}
	x.dis(alice, "send "+evTopic+" --id "+ev(idA, 2)+" -- x")
	x.dis(alice, "send "+evTopic+" --id "+ev(idB, 101)+" -- x")
	x.dis(alice, "send "+evTopic+" --id "+ev(idA, 11)+" -- x")
	l := x.dis(bob, "recv "+sb+" --peek").out
	eq(t, "arrival: by seq in a session, sessions as published", evs(l),
		idB+"/9 "+idB+"/10 "+idB+"/100 "+idA+"/2 "+idB+"/101 "+idA+"/11")
	f := x.start(x.adm(bob), "recv "+sb+" --follow --peek")
	waitFor(t, 2*time.Second, "the backlog", func() bool { return strings.Count(f.out.String(), ";id=event-") == 6 })
	eq(t, "the follow too", evs(f.out.String()), evs(l))
	f.cancel()
}

// A name that exists already counts as a failed link: the publication's seq is used there, a gap.
func TestExistingNameIsAFailedLink(t *testing.T) {
	x, sb, sa := evRig(t)
	os.WriteFile(x.h.journal, nil, 0o660)
	x.dis(alice, "send "+evTopic+" --id "+ev(idA, 5)+" -- first")
	x.dis(alice, "send "+evTopic+" --id "+ev(idB, 1)+" -- y")
	x.dis(alice, "recv_ack "+sa+" "+ev(idA, 5))
	r := x.dis(alice, "send "+evTopic+" --id "+ev(idA, 5)+" -- again") // another session since: a new publication
	eq(t, "published to the one that had acked it", strings.TrimSpace(r.out), "published -> "+evTopic+" msgid="+ev(idA, 5)+" subs=1")
	has(t, "a failed link in the journal", readString(x.h.journal), " publish "+evTopic+" "+ev(idA, 5)+" from "+alice+" subs=1 failed=1\n")
	has(t, "the holder kept its first", x.dis(bob, "recv "+sb+" --id "+ev(idA, 5)+" --peek:rfc5322").out, "\n\nfirst")
}

// The position notice: after the backlog, done read before the scan, only when the mailbox lacks it;
// confirmed only by its exact value.
func TestPositionNotice(t *testing.T) {
	x, sb, _ := evRig(t)
	r := x.dis(bob, "recv "+sb+" --id head-none --peek:rfc5322")
	code(t, "head-none confirmed while done is none", r, 0)
	eq(t, "raw: From, Date, a blank line", dateless(r.out), "From: "+evTopic+"\nDate: D\n\n")
	x.dis(alice, "send "+evTopic+" --id "+ev(idA, 3)+" -- x")
	x.dis(alice, "send "+evTopic+" --id "+ev(idA, 4)+" -- y")
	r = x.dis(bob, "recv "+sb+" --id head-"+idA+"-4 --peek:rfc5322")
	code(t, "the current value", r, 0)
	eq(t, "raw: no Subject, no header", dateless(r.out), "From: "+evTopic+"\nDate: D\n\n")
	r = x.dis(bob, "recv "+sb+" --id head-"+idA+"-4")
	eq(t, "--id: the notice chunk", fmt.Sprint(r.code, " ", allIDs(r.out)), "0 head-"+idA+"-4/msg")
	for _, h := range []string{"head-" + idA + "-3", "head-" + idA + "-5", "head-" + idB + "-1", "head-" + idA + "-04", "head-none"} {
		r = x.dis(bob, "recv "+sb+" --id "+h+" --id head-"+idA+"-4")
		eq(t, "not the current head: part=missing, in its place", fmt.Sprint(r.code, " ", allIDs(r.out), " ", partOf(r.out, h, "missing")), "0 "+h+"/missing head-"+idA+"-4/msg Missing: "+h+"\n")
		code(t, h+" alone: 3", x.dis(bob, "recv "+sb+" --id "+h), 3)
		code(t, h+" raw: 3", x.dis(bob, "recv "+sb+" --id "+h+" --peek:rfc5322"), 3)
	}
	eq(t, "recv_ack head-*: refused", x.dis(bob, "recv_ack "+sb+" head-"+idA+"-4").out, "refused head-"+idA+"-4\n")
	code(t, "on an identity's own mailbox: 3", x.dis(bob, "recv --id head-none --peek:rfc5322"), 3)
	eq(t, "the mailbox holds done: no notice", heads(x.dis(bob, "recv "+sb).out), "")
	eq(t, "consumed: now it shows", heads(x.dis(bob, "recv "+sb).out), "head-"+idA+"-4")
	eq(t, "not consumed: recv shows it again", heads(x.dis(bob, "recv "+sb).out), "head-"+idA+"-4")
	f := x.start(x.adm(bob), "recv "+sb+" --follow")
	waitFor(t, 2*time.Second, "the notice on the follow", func() bool { return strings.Contains(f.out.String(), ";id=head-") })
	x.dis(alice, "send "+evTopic+" -- z")
	waitFor(t, 2*time.Second, "the next delivery", func() bool { return strings.Contains(f.out.String(), ";id="+ev(idA, 5)+";") })
	eq(t, "printed once, after the backlog", strings.Count(f.out.String(), ";id=head-"), 1)
	f.cancel()
	// expiry notices in a subscription carry nothing about the session
	x.dis(alice, "send "+evTopic+" --subject v -- x")
	x.age(sb, ev(idA, 6))
	x.h.Reap()
	l := x.dis(bob, "recv "+sb+" --peek").out
	has(t, "an expiry notice", l, "\nExpired: 1\n")
	lacks(t, "with no session", l, "Sub-")
}

// A reader connecting mid-fan-out never sees a head its mailbox lacks; a subscription made during one
// waits for it, gets publications from the next one, and its position notice gives done.
func TestMidFanOut(t *testing.T) {
	x, sb, sa := evRig(t)
	x.dis(alice, "send "+evTopic+" -- one")
	first, _ := x.firstSub(evTopic, sa)
	other, oowner := sb, bob
	if first == sb {
		other, oowner = sa, alice
	}
	var mid string
	fired := false
	testHook = func(p string) {
		if p == "linked" && !fired {
			fired = true
			x.dis(oowner, "recv "+other) // consumes the first: the head now shows
			mid = heads(x.dis(oowner, "recv "+other+" --peek").out)
		}
	}
	x.dis(alice, "send "+evTopic+" -- two")
	noHook()
	d := x.h.topicDone(evTopic)
	eq(t, "a reader mid-fan-out: done is the previous publication", mid, "head-"+d.id+"-1")

	x.mkTopic("topic+mid@acme.ssh", alice)
	ma := strings.TrimSpace(x.dis(alice, "sub topic+mid@acme.ssh").out)
	x.dis(alice, "send topic+mid@acme.ssh -- one")
	fired = false
	var mb string
	waited := false
	subDone := make(chan struct{})
	testHook = func(p string) {
		if p == "linked" && !fired {
			fired = true
			go func() { mb = strings.TrimSpace(x.dis(bob, "sub topic+mid@acme.ssh").out); close(subDone) }()
			time.Sleep(100 * time.Millisecond)
			select {
			case <-subDone:
			default:
				waited = true
			}
		}
	}
	x.dis(alice, "send topic+mid@acme.ssh -- two")
	noHook()
	<-subDone
	eq(t, "sub waits for the fan-out under way", waited, true)
	l := x.dis(bob, "recv "+mb+" --peek").out
	eq(t, "made during a fan-out: it has none of that publication", ids(l), "")
	dm := x.h.topicDone("topic+mid@acme.ssh")
	eq(t, "its position notice: done", heads(l), "head-"+dm.id+"-2")
	x.dis(alice, "send topic+mid@acme.ssh -- three")
	eq(t, "it gets the next", seqs(x.dis(bob, "recv "+mb+" --peek").out), "3")
	eq(t, "the other had all", seqs(x.dis(alice, "recv "+ma+" --peek").out), "1 2 3")
}

// Concurrent publications on one topic: seqs 1..n, each once, every mailbox in seq order.
func TestConcurrentEventIDs(t *testing.T) {
	x, sb, sa := evRig(t)
	var wg sync.WaitGroup
	for i := 1; i <= 24; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); x.dis(alice, "send "+evTopic+" -- x") }()
	}
	wg.Wait()
	want := ""
	for i := 1; i <= 24; i++ {
		want += fmt.Sprint(" ", i)
	}
	eq(t, "24 at once: 1..24 in seq order", " "+seqs(x.dis(bob, "recv "+sb+" --peek").out), want)
	eq(t, "the other the same", " "+seqs(x.dis(alice, "recv "+sa+" --peek").out), want)
	eq(t, "done", x.h.topicDone(evTopic).seq, 24)
	eq(t, "the record: the last", strings.Fields(x.record(evTopic))[1], "24")
}

// The body is stored outside the topic lock: a publication still storing does not hold up another.
func TestStoreOutsideLock(t *testing.T) {
	x, sb, _ := evRig(t)
	release := make(chan struct{})
	held := make(chan struct{})
	first := true
	var mu sync.Mutex
	testHook = func(p string) {
		mu.Lock()
		f := p == "stored" && first
		if f {
			first = false
		}
		mu.Unlock()
		if f {
			close(held)
			<-release
		}
	}
	defer noHook()
	slow := make(chan result, 1)
	go func() { slow <- x.dis(alice, "send "+evTopic+" -- slow") }()
	<-held
	r := x.dis(alice, "send "+evTopic+" -- quick")
	code(t, "a publication while another is still storing", r, 0)
	close(release)
	code(t, "the slow one", <-slow, 0)
	l := x.dis(bob, "recv "+sb+" --peek").out
	d := x.h.topicDone(evTopic).id
	has(t, "seqs in lock order: quick first", msgOf(l, ev(d, 1)), "quick")
	has(t, "then slow", msgOf(l, ev(d, 2)), "slow")
}

// dup and stale answer before the body is staged: nothing stored, nothing taken.
func TestDupBeforeStage(t *testing.T) {
	x, _, _ := evRig(t)
	x.dis(alice, "send "+evTopic+" --id "+ev(idA, 3)+" -- x")
	staged := 0
	testHook = func(p string) {
		if p == "stage" {
			staged++
		}
	}
	defer noHook()
	bag := string(makeTar(t, map[string]string{"f": "b1\n"}))
	r := x.dis(alice, "send_bag "+evTopic+" --id "+ev(idA, 3), bag)
	eq(t, "a dup bag: dup, nothing staged", fmt.Sprint(r.code, "|", r.out, "|", staged, "", len(x.stages())), "0|dup\n|00")
	r = x.dis(alice, "send "+evTopic+" --id "+ev(idA, 2)+" -m", "x")
	eq(t, "a stale one", fmt.Sprint(r.code, "|", r.out, "|", staged), "0|stale: last "+ev(idA, 3)+"\n|0")
}

// A publication's flood token is spent once its body is whole: a dup, a stale or a cut one costs none.
func TestPublishTokenAfterBody(t *testing.T) {
	x, _, _ := evRig(t)
	os.WriteFile(filepath.Join(x.h.topicDir(evTopic), "policy"), []byte("topic="+evTopic+"\npublish="+alice+"\nrate=0\nburst=2\n"), 0o640)
	code(t, "the first", x.dis(alice, "send "+evTopic+" --id "+ev(idA, 1)+" -- x"), 0)
	for i := 0; i < 5; i++ {
		eq(t, "a dup, again and again: no token", x.dis(alice, "send "+evTopic+" --id "+ev(idA, 1)+" -- x").out, "dup\n")
	}
	code(t, "a cut one", x.dis(alice, "send "+evTopic+" --chunked -m", "3\r\nabc\r\n"), 4)
	code(t, "the second token is still there", x.dis(alice, "send "+evTopic+" -- y"), 0)
	code(t, "then none", x.dis(alice, "send "+evTopic+" -- z"), 5)
}
