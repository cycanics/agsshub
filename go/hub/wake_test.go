package hub

// Admitting and rejecting instances, the ask hold, the wake notice, flow C's transient entry, and the
// exits of a send to an instance (docs/wake-live-event-id.md, flows A-C, Wake notice, Exits).

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const wHub = "commhub@home.ssh"

func (x *th) settings(kv string) {
	x.t.Helper()
	if err := os.WriteFile(filepath.Join(x.state, "settings"), []byte(kv), 0o640); err != nil {
		x.t.Fatal(err)
	}
}

func inst(id string) string { return "group.sess+" + id + "@expa.ssh" }

var wakeBlockRE = regexp.MustCompile(`(?s)^From: (\S+)\nDate: \S+\nWake: (\d+)\n\n(.*)$`)

// wakes: every wake notice in a listing or follow output: msgid, count, lines.
type wakeSeen struct {
	msgid string
	n     string
	body  string
}

func wakesIn(out string) []wakeSeen {
	var ws []wakeSeen
	for _, c := range chunks(out) {
		if c.part != "msg" || !strings.HasPrefix(c.id, WakePrefix) {
			continue
		}
		if m := wakeBlockRE.FindStringSubmatch(c.data); m != nil {
			ws = append(ws, wakeSeen{c.id, m[2], m[3]})
		}
	}
	return ws
}

func lastWake(out string) wakeSeen {
	ws := wakesIn(out)
	if len(ws) == 0 {
		return wakeSeen{}
	}
	return ws[len(ws)-1]
}

var dateInRE = regexp.MustCompile(`@20[0-9-]{8}T[0-9:]{8}Z`)

// wakeLines: a notice's lines with each sender's time as @T.
func wakeLines(w wakeSeen) string { return dateInRE.ReplaceAllString(w.body, "@T") }

// heldSend starts a send held for an ask; it returns once the ask is listed.
func (x *th) heldSend(t *testing.T, from, cmd, stdin, id string) *bg {
	t.Helper()
	b := x.start(x.adm(from), cmd, stdin)
	waitFor(t, 3*time.Second, "the ask for "+id, func() bool {
		x.h.asks.mu.Lock()
		defer x.h.asks.mu.Unlock()
		a := x.h.asks.m[inst(id)]
		return a != nil && a.from[from] > 0
	})
	return b
}

func TestAdmission(t *testing.T) {
	x := instRig(t)
	x.settings("wake_secs=30\n")
	box := func(id string) string { return filepath.Join(x.state, "inbox", inst(id)) }

	// no mailbox: held, then admitted
	s1 := x.heldSend(t, wHub, "send "+inst("a1")+" --id m-1 -- hello", "", "a1")
	l := x.dis(par, "recv --peek").out
	eq(t, "the ask on the address's wake notice", wakeLines(lastWake(l)), inst("a1")+"\task="+wHub+"\n")
	eq(t, "Wake: 1", lastWake(l).n, "1")
	eq(t, "nothing stored while held", exists(box("a1")), false)
	r := x.dis(par, "admit a1")
	eq(t, "admit", r.out, "admitted "+inst("a1")+"\n")
	eq(t, "the held send is stored: exit 0", s1.wait(t, 3*time.Second), 0)
	has(t, "saying sent", s1.out.String(), "sent -> "+inst("a1")+" msgid=m-1")
	eq(t, "its message in the new mailbox", exists(filepath.Join(box("a1"), "m-1.msg")), true)
	eq(t, "the ask gone, the mail listed", wakeLines(lastWake(x.dis(par, "recv --peek").out)), inst("a1")+"\tmail=1\tfrom="+wHub+"@T\n")

	// mailbox present, no --wait: stored at once, followed or not
	r = x.dis(wHub, "send "+inst("a1")+" --id m-2 -- again")
	code(t, "no --wait, mailbox: stored", r, 0)
	eq(t, "mail=2", wakeLines(lastWake(x.dis(par, "recv").out)), inst("a1")+"\tmail=2\tfrom="+wHub+"@T\n")
	eq(t, "recv does not consume the wake notice", lastWake(x.dis(par, "recv").out).n, "1")

	// reject of an ask: exit 3, nothing stored, and nothing kept: the next send asks again
	s2 := x.heldSend(t, wHub, "send "+inst("b1")+" --id m-3 -- x", "", "b1")
	eq(t, "reject", x.dis(par, "reject b1").out, "rejected "+inst("b1")+"\n")
	eq(t, "the held sender exits 3", s2.wait(t, 3*time.Second), 3)
	eq(t, "nothing stored, no mailbox", exists(box("b1")), false)
	eq(t, "and the wake set lists it no more", strings.Contains(x.dis(par, "recv --peek").out, inst("b1")), false)
	s2 = x.heldSend(t, wHub, "send "+inst("b1")+" --id m-3b -- x", "", "b1")
	x.dis(par, "reject b1")
	eq(t, "the hub remembers nothing: asked again, then 3", s2.wait(t, 3*time.Second), 3)
	s2 = x.heldSend(t, wHub, "send_bag "+inst("b1"), string(makeTar(t, map[string]string{"a": "b"})), "b1")
	x.dis(par, "reject b1")
	eq(t, "send_bag: held, 3", s2.wait(t, 3*time.Second), 3)
	s2 = x.heldSend(t, wHub, "send "+inst("b1")+" --live -m", string(frame([]byte("x"), 4, "")), "b1")
	x.dis(par, "reject b1")
	eq(t, "send --live: held, 3", s2.wait(t, 3*time.Second), 3)
	// admit without an ask makes the mailbox
	eq(t, "admit, no ask", x.dis(par, "admit b1").out, "admitted "+inst("b1")+"\n")
	eq(t, "its mailbox made", isDir(box("b1")), true)
	code(t, "and a send stores at once", x.dis(wHub, "send "+inst("b1")+" -- y"), 0)

	// reject of an instance with a mailbox: its mail discarded, journal line with the count
	eq(t, "reject with mail", x.dis(par, "reject a1").out, "rejected "+inst("a1")+"\n")
	eq(t, "its mailbox gone", exists(box("a1")), false)
	has(t, "the journal counts what went", readString(filepath.Join(x.state, "claims", ".journal")), "reject "+inst("a1")+" by "+par+" discarded=2")
	for _, n := range globNamesAll(filepath.Join(x.state, "inbox")) {
		if strings.HasPrefix(n, ".stage.") {
			t.Errorf("a stage left: %s", n)
		}
	}

	// nothing to do: exit 0, no journal line for a mailbox that was not there
	r = x.dis(par, "reject zz1")
	code(t, "reject with nothing to do: 0", r, 0)
	eq(t, "says rejected", r.out, "rejected "+inst("zz1")+"\n")
	lacks(t, "no discard line", readString(filepath.Join(x.state, "claims", ".journal")), "reject "+inst("zz1"))

	// a claim with no mailbox asks like any other: the claim implies nothing
	if _, rr := x.register("c1", "c1k"); rr.code != 0 {
		t.Fatalf("register: %+v", rr)
	}
	s3 := x.heldSend(t, wHub, "send "+inst("c1")+" --id m-4 -- z", "", "c1")
	eq(t, "held, no mailbox made", exists(box("c1")), false)
	x.dis(par, "admit c1")
	eq(t, "admitted: stored", s3.wait(t, 3*time.Second), 0)
	eq(t, "the mailbox made for it", exists(filepath.Join(box("c1"), "m-4.msg")), true)

	// several senders, one ask; register answers it as admit
	var held []*bg
	for i, from := range []string{wHub, wHub, "group.demo@expa.ssh"} {
		held = append(held, x.heldSend(t, from, fmt.Sprintf("send %s --id e-%d -- x", inst("e1"), i), "", "e1"))
	}
	w := lastWake(x.dis(par, "recv --peek").out)
	eq(t, "one ask, its senders distinct", regexp.MustCompile(`(?m)^.*\task=.*\n`).FindAllString(w.body, -1), []string{inst("e1") + "\task=" + wHub + ",group.demo@expa.ssh\n"})
	x.register("e1", "e1k")
	for i, b := range held {
		eq(t, fmt.Sprintf("held sender %d: stored when register answers", i), b.wait(t, 3*time.Second), 0)
	}
	eq(t, "all three stored", len(msgFiles(box("e1"))), 3)

	// the address answers for its own instances only
	code(t, "admit of another address's instance: 3", x.dis(par, "admit group.demo+x1@expa.ssh"), 3)
	code(t, "admit as an address without instances: 3", x.dis("group.demo@expa.ssh", "admit x1"), 3)
	code(t, "admit without an instance: 2", x.dis(par, "admit"), 2)
	eq(t, "several per call", x.dis(par, "reject g1 g2").out, "rejected "+inst("g1")+"\nrejected "+inst("g2")+"\n")
}

// An ask made again after its answer renders the same line: its msgid is new, so the address sees it.
func TestReAskIsANewNotice(t *testing.T) {
	x := instRig(t)
	x.settings("wake_secs=30\n")
	s := x.heldSend(t, wHub, "send "+inst("a1")+" -- x", "", "a1")
	w1 := x.h.renderWake(par)
	x.dis(par, "reject a1")
	s.wait(t, 3*time.Second)
	s = x.heldSend(t, wHub, "send "+inst("a1")+" -- x", "", "a1")
	w2 := x.h.renderWake(par)
	eq(t, "the same line", w2.body, w1.body)
	if w1.msgid == w2.msgid {
		t.Error("a second ask kept the first one's msgid: its notice is not printed")
	}
	eq(t, "the same state, the same msgid", x.h.renderWake(par).msgid, w2.msgid)
	x.dis(par, "reject a1")
	s.wait(t, 3*time.Second)
}

// A --wait send after a notice was stored (a client's bag): the mailbox has mail and nobody follows it, so
// the sender asks; the line carries both the mail and the ask, and admit stores it.
func TestWaitAskCarriesMail(t *testing.T) {
	x := instRig(t)
	x.settings("wake_secs=30\n")
	x.dis(par, "admit a1")
	code(t, "the notice is stored at once", x.dis(wHub, "send "+inst("a1")+" --id n-1 -- notice"), 0)
	box := filepath.Join(x.state, "inbox", inst("a1"))
	s := x.heldSend(t, wHub, "send "+inst("a1")+" --id b-1 --wait 0.2 -- bag", "", "a1")
	line := wakeLines(lastWake(x.dis(par, "recv --peek").out))
	eq(t, "the line carries the mail and the ask", line, inst("a1")+"\tmail=1\tfrom="+wHub+"@T\task="+wHub+"\n")
	eq(t, "the message not stored yet", exists(filepath.Join(box, "b-1.msg")), false)
	x.dis(par, "admit a1")
	eq(t, "admitted, stored, then not picked within the wait: 8", s.wait(t, 3*time.Second), 8)
	eq(t, "stored", exists(filepath.Join(box, "b-1.msg")), true)
}

// A follow starting ends the ask: the held sender stores and waits for the pickup.
func TestFollowEndsAsk(t *testing.T) {
	x := instRig(t)
	x.settings("wake_secs=30\n")
	x.dis(par, "admit a1")
	ser, _ := x.register("a1", "a1k")
	s := x.heldSend(t, wHub, "send "+inst("a1")+" --id b-1 --wait 30 -- bag", "", "a1")
	ia := Admission{FQ: inst("a1"), Serial: ser, KeyID: x.keyid("a1k")}
	f := x.start(ia, "recv --follow")
	eq(t, "the ask ended by the follow, stored, then picked: 0", s.wait(t, 3*time.Second), 0)
	has(t, "picked", s.out.String(), "picked")
	eq(t, "no ask left", len(x.h.asks.m), 0)
	f.cancel()
	f.wait(t, 2*time.Second)

	// followed: a --wait send does not ask at all
	f2 := x.start(ia, "recv --follow --peek:header")
	waitFor(t, 2*time.Second, "the follow", x.followed(inst("a1")))
	s2 := x.start(x.adm(wHub), "send "+inst("a1")+" --id b-2 --wait 30 -- bag", "")
	waitFor(t, 2*time.Second, "stored without an ask", func() bool { return exists(filepath.Join(x.state, "inbox", inst("a1"), "b-2.msg")) })
	eq(t, "no ask made", len(x.h.asks.m), 0)
	// a sender already in --wait when reject lands: 3, not 8
	code(t, "reject", x.dis(par, "reject a1"), 0)
	eq(t, "the waiting sender: 3", s2.wait(t, 3*time.Second), 3)
	f2.cancel()
	f2.wait(t, 2*time.Second)
}

// No answer in time: the mailbox at that moment decides: there, stored, exit 8; not there, nothing stored, 6.
func TestNoAnswerBothStates(t *testing.T) {
	x := instRig(t)
	x.settings("wake_secs=0.3\nlive_retry_secs=7\n")
	box := filepath.Join(x.state, "inbox", inst("a1"))
	r := x.dis(wHub, "send "+inst("a1")+" --id m-1 --wait 5 -- x")
	code(t, "no mailbox, no answer: 6", r, 6)
	has(t, "retry-after", r.err, "retry-after=7")
	eq(t, "nothing stored", exists(box), false)
	x.dis(par, "admit a1")
	r = x.dis(wHub, "send "+inst("a1")+" --id m-2 --wait 5 -- x")
	code(t, "--wait, mailbox, not followed, no answer: stored, 8", r, 8)
	has(t, "says so", r.out, "stored, not picked")
	eq(t, "stored", exists(filepath.Join(box, "m-2.msg")), true)
	// a mailbox appearing during the ask counts (without --wait too): the instance's connect makes it
	ser, _ := x.register("b1", "b1k")
	s := x.heldSend(t, wHub, "send "+inst("b1")+" --id m-3 -- x", "", "b1")
	x.idis(inst("b1"), ser, "b1k", "id")
	eq(t, "no answer, the mailbox made meanwhile: stored, 8", s.wait(t, 3*time.Second), 8)
	eq(t, "stored", exists(filepath.Join(x.state, "inbox", inst("b1"), "m-3.msg")), true)
	// a live send with no answer stays flow C: exit 6 with retry-after, nothing delivered
	r = x.dis(wHub, "send "+inst("a2")+" --live -m", string(frame([]byte("x"), 4, "")))
	code(t, "live, no answer: 6", r, 6)
	has(t, "retry-after", r.err, "retry-after=7")
}

// reject with a claim: the claim ends (the instance is refused from its next command), its mailbox and mail go,
// the follow ends, and a send asks again.
func TestRejectWithClaim(t *testing.T) {
	x := instRig(t)
	x.settings("wake_secs=30\n")
	ser, _ := x.register("a1", "a1k")
	ia := Admission{FQ: inst("a1"), Serial: ser, KeyID: x.keyid("a1k")}
	f := x.start(ia, "recv --follow --peek")
	waitFor(t, 2*time.Second, "the follow", x.followed(inst("a1")))
	x.dis(par, "admit a1")
	x.dis(wHub, "send "+inst("a1")+" --id m-1 -- x")
	r := x.dis(par, "reject a1")
	code(t, "reject with a claim: 0", r, 0)
	eq(t, "the claim gone", isFile(x.h.instRecord(inst("a1"))), false)
	eq(t, "the mailbox gone", exists(filepath.Join(x.state, "inbox", inst("a1"))), false)
	eq(t, "the follow ended", f.wait(t, 3*time.Second), 0)
	eq(t, "a key no longer holding the identity: refused", x.idis(inst("a1"), ser, "a1k", "recv").code, 3)
	journal := readString(filepath.Join(x.state, "claims", ".journal"))
	has(t, "the journal says the claim went", journal, "release "+inst("a1")+" by "+par+" (reject)")
	has(t, "and counts the mail", journal, "reject "+inst("a1")+" by "+par+" discarded=1")
	s := x.heldSend(t, wHub, "send "+inst("a1")+" -- again", "", "a1")
	x.dis(par, "reject a1")
	eq(t, "the next send asks and gets 3", s.wait(t, 3*time.Second), 3)
}

// An instance connecting during a reject (past its claim check, then the reject) does not remake the mailbox.
func TestConnectDuringReject(t *testing.T) {
	x := instRig(t)
	ser, _ := x.register("a1", "a1k")
	fired := false
	testHook = func(p string) {
		if p == "inst-connect" && !fired {
			fired = true
			code(t, "reject lands in the connect", x.dis(par, "reject a1"), 0)
		}
	}
	defer noHook()
	r := x.idis(inst("a1"), ser, "a1k", "id")
	code(t, "the connect: refused", r, 3)
	eq(t, "no mailbox made", exists(filepath.Join(x.state, "inbox", inst("a1"))), false)
}

func TestAskTimeoutAndLeave(t *testing.T) {
	x := instRig(t)
	x.settings("wake_secs=0.3\nlive_retry_secs=7\n")
	r := x.dis(wHub, "send "+inst("a1")+" --id m-1 -- x")
	code(t, "no answer within wake_secs: exit 6", r, 6)
	has(t, "retry-after on stderr", r.err, "\nretry-after=7\n")
	eq(t, "nothing stored, no mailbox", exists(filepath.Join(x.state, "inbox", inst("a1"))), false)
	r = x.dis(wHub, "send "+inst("a1")+" --live -m", string(frame([]byte("x"), 4, "")))
	code(t, "flow C2, no answer: exit 6", r, 6)
	has(t, "retry-after", r.err, "retry-after=7")
	eq(t, "no ask left", len(x.h.asks.m), 0)

	// the last held sender going away ends the ask
	x.settings("wake_secs=30\n")
	b := x.heldSend(t, wHub, "send "+inst("a2")+" -- x", "", "a2")
	f := x.start(x.adm(par), "recv --follow --peek")
	waitFor(t, 2*time.Second, "the ask on the follow", func() bool { return strings.Contains(f.out.String(), "\task=") })
	b.cancel()
	b.wait(t, 3*time.Second)
	waitFor(t, 2*time.Second, "the ask's end on the follow", func() bool { return lastWake(f.out.String()).n == "0" })
	eq(t, "the ask gone", len(x.h.asks.m), 0)

	// an address that connects within wake_secs sees the ask
	f.cancel()
	f.wait(t, 2*time.Second)
	b = x.heldSend(t, wHub, "send "+inst("a3")+" -- x", "", "a3")
	f = x.start(x.adm(par), "recv --follow --peek")
	waitFor(t, 2*time.Second, "the ask on the new follow", func() bool { return strings.Contains(f.out.String(), inst("a3")+"\task=") })
	x.dis(par, "admit a3")
	eq(t, "then the admit stores it", b.wait(t, 3*time.Second), 0)
}

// The wake notice: printed first on every follow (Wake: 0 too), again only when its msgid changes;
// rendered, never stored; recv_ack refused; --id gives the current rendering.
func TestWakeNotice(t *testing.T) {
	x := instRig(t)
	x.settings("wake_secs=30\nlive_retry_secs=0.25\n")
	f := x.start(x.adm(par), "recv --follow --peek")
	waitFor(t, 2*time.Second, "the first notice", func() bool { return len(wakesIn(f.out.String())) == 1 })
	eq(t, "first: Wake: 0", lastWake(f.out.String()).n, "0")
	x.dis(par, "admit a1")
	x.dis(wHub, "send "+inst("a1")+" --id m-1 -- x")
	waitFor(t, 2*time.Second, "a delivery nudges it", func() bool { return lastWake(f.out.String()).n == "1" })
	n := len(wakesIn(f.out.String()))
	x.dis(wHub, "send "+inst("a1")+" --id m-1 -- x") // a dup: nothing changes
	x.dis(par, "id")
	x.h.follows.nudge(par)
	time.Sleep(100 * time.Millisecond)
	eq(t, "unchanged content prints nothing", len(wakesIn(f.out.String())), n)
	// an expiry notice does not count as mail
	os.MkdirAll(filepath.Join(x.state, "inbox", inst("a1"), ".expired"), 0o2770)
	eq(t, "mail= counts .msg files only", strings.Contains(lastWake(f.out.String()).body, "mail=1"), true)

	// the instance's follow takes it off; its consume nudges
	ser, _ := x.register("a1", "a1k")
	ia := Admission{FQ: inst("a1"), Serial: ser, KeyID: x.keyid("a1k")}
	fi := x.start(ia, "recv --follow")
	waitFor(t, 2*time.Second, "the instance follows", x.followed(inst("a1")))
	waitFor(t, 2*time.Second, "a followed instance leaves the set", func() bool { return lastWake(f.out.String()).n == "0" })
	fi.cancel()
	fi.wait(t, 2*time.Second)
	waitFor(t, 2*time.Second, "its follow's end: back, with no mail (consumed)", func() bool { return lastWake(f.out.String()).n == "0" })
	x.dis(wHub, "send "+inst("a1")+" --id m-2 -- y")
	waitFor(t, 2*time.Second, "mail again", func() bool { return lastWake(f.out.String()).n == "1" })
	x.idis(inst("a1"), ser, "a1k", "recv")
	waitFor(t, 2*time.Second, "the instance's own recv nudges", func() bool { return lastWake(f.out.String()).n == "0" })

	w := lastWake(f.out.String())
	eq(t, "recv_ack wakes-*: refused", x.dis(par, "recv_ack "+w.msgid).out, "refused "+w.msgid+"\n")
	rw := x.dis(par, "recv --id wakes-0000000000000000")
	eq(t, "--id wakes-<any>: the current rendering, with the current msgid", allIDs(rw.out), w.msgid+"/msg")
	eq(t, "a full read never consumes it", allIDs(x.dis(par, "recv").out), w.msgid+"/msg")
	raw := x.dis(par, "recv --id wakes-0000000000000000 --peek:rfc5322")
	code(t, "raw of any wakes- msgid", raw, 0)
	has(t, "the current rendering", raw.out, "From: "+par+"\nDate: ")
	has(t, "Wake header", raw.out, "\nWake: 0\n\n")
	code(t, "raw of wakes- on an address without instances: 3", x.dis("group.demo@expa.ssh", "recv --id wakes-0000000000000000 --peek:rfc5322"), 3)
	lacks(t, "an address without instances gets none", x.dis("group.demo@expa.ssh", "recv --peek").out, "wakes-")
	for _, id := range []string{"wakes-x", "head-x", "expired-x", "event-x"} {
		code(t, "--id "+id+": 2", x.dis(wHub, "send group.demo@expa.ssh --id "+id+" -- x"), 2)
	}
	code(t, "a body imitating it is a message", x.dis(wHub, "send "+par+" --id fake -- Wake: 3"), 0)
	eq(t, "the same state, the same msgid", renderMsgid(x), renderMsgid(x))

	// flow C: --live to an instance with a mailbox nobody follows
	live := func() result { return x.dis(wHub, "send "+inst("a1")+" --live -m", string(frame([]byte("x"), 4, ""))) }
	r := live()
	code(t, "flow C: exit 6", r, 6)
	has(t, "retry-after", r.err, "\nretry-after=0.25\n")
	eq(t, "nothing stored", len(msgFiles(filepath.Join(x.state, "inbox", inst("a1")))), 0)
	waitFor(t, 2*time.Second, "the transient entry listed", func() bool {
		return strings.Contains(lastWake(f.out.String()).body, "stay-up-till=")
	})
	eq(t, "its line", regexp.MustCompile(`stay-up-till=\S+`).ReplaceAllString(wakeLines(lastWake(f.out.String())), "stay-up-till=S"),
		inst("a1")+"\tmail=0\tfrom="+wHub+"@T\tstay-up-till=S\n")
	live()
	eq(t, "a retry refreshes, not adds", strings.Count(lastWake(x.dis(par, "recv --peek").out).body, "\n"), 1)
	eq(t, "one entry", len(x.h.trans.m[inst("a1")]), 1)
	waitFor(t, 3*time.Second, "it ends at stay-up-till", func() bool { return lastWake(f.out.String()).n == "0" })
	live()
	fi = x.start(ia, "recv --follow --peek")
	waitFor(t, 2*time.Second, "I follows", x.followed(inst("a1")))
	waitFor(t, 2*time.Second, "its follow ends the transient entry", func() bool { return len(x.h.trans.m) == 0 })
	fi.cancel()
	fi.wait(t, 2*time.Second)

	// flow C2: an ask, admit, then flow C
	b := x.heldSend(t, wHub, "send "+inst("c2")+" --live -m", string(frame([]byte("x"), 4, "")), "c2")
	x.dis(par, "admit c2")
	eq(t, "admitted --live: flow C, exit 6", b.wait(t, 3*time.Second), 6)
	has(t, "with retry-after", b.err.String(), "retry-after=")
	eq(t, "its mailbox made", isDir(filepath.Join(x.state, "inbox", inst("c2"))), true)
}

func renderMsgid(x *th) string { return x.h.renderWake(par).msgid }

// A reject between a store's stage and its link: the store finds no mailbox, exits 3, and makes none.
func TestRejectVersusStore(t *testing.T) {
	x := instRig(t)
	x.dis(par, "admit a1")
	fired := false
	testHook = func(p string) {
		if p == "inst-staged" && !fired {
			fired = true
			code(t, "the reject lands mid-store", x.dis(par, "reject a1"), 0)
		}
	}
	defer noHook()
	r := x.dis(wHub, "send "+inst("a1")+" --id m-1 -- x")
	code(t, "the store finds the mailbox gone: exit 3", r, 3)
	eq(t, "no mailbox made again", exists(filepath.Join(x.state, "inbox", inst("a1"))), false)
	eq(t, "the stage removed", len(x.stages()), 0)
	noHook()
	// the same with the mailbox lock: a store waiting on it while reject renames the mailbox away
	x.dis(par, "admit a2")
	box := filepath.Join(x.state, "inbox", inst("a2"))
	unlock := lockBox(box)
	done := make(chan result, 1)
	go func() { done <- x.dis(wHub, "send "+inst("a2")+" --id m-2 -- x") }()
	time.Sleep(50 * time.Millisecond)
	rj := make(chan result, 1)
	go func() { rj <- x.dis(par, "reject a2") }()
	time.Sleep(50 * time.Millisecond)
	unlock()
	code(t, "reject", <-rj, 0)
	r = <-done
	if r.code != 3 && r.code != 0 {
		t.Errorf("the racing store: exit %d", r.code)
	}
	eq(t, "no mailbox, whoever won", exists(box), false)
}

// admit and reject racing on one instance: the end state is always one of the two, and no ask stays.
func TestAdmitVersusReject(t *testing.T) {
	x := instRig(t)
	x.settings("wake_secs=0.2\n")
	for i := 0; i < 40; i++ {
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); x.dis(par, "admit a1") }()
		go func() { defer wg.Done(); x.dis(par, "reject a1") }()
		go func() { defer wg.Done(); x.dis(wHub, "send "+inst("a1")+" -- x") }()
		wg.Wait()
		x.dis(par, "reject a1")
		eq(t, "a reject last: no mailbox", exists(filepath.Join(x.state, "inbox", inst("a1"))), false)
	}
	waitFor(t, 2*time.Second, "no ask left", func() bool {
		x.h.asks.mu.Lock()
		defer x.h.asks.mu.Unlock()
		return len(x.h.asks.m) == 0
	})
}

// An instance's own connect makes its mailbox through the one function.
func TestInstanceConnectMakesMailbox(t *testing.T) {
	x := instRig(t)
	ser, _ := x.register("a1", "a1k")
	x.idis(inst("a1"), ser, "a1k", "id")
	eq(t, "its connect made the mailbox", isDir(filepath.Join(x.state, "inbox", inst("a1"))), true)
	eq(t, "no keyed lock left behind", len(x.h.admMu.m), 0)
}

// --live: exit 9 where live is not supported.
func TestLiveNotSupported(t *testing.T) {
	x := instRig(t)
	x.mustMint("publisher+app@expa.ssh", "pubk")
	tp := "topic+ev@expa.ssh"
	x.bash(`mkdir -p "$HUB_ROOT/topics/$1" && printf 'topic=%s\npublish=*\n' "$1" > "$HUB_ROOT/topics/$1/policy"`, tp)
	sub := strings.TrimSpace(x.dis(par, "sub "+tp).out)
	os.WriteFile(filepath.Join(x.state, "aliases", "mail"), []byte("type=email\naddr=a@b\n"), 0o640)
	fr := string(frame([]byte("x"), 4, ""))
	for _, c := range []struct{ what, me, target string }{
		{"a topic", wHub, tp},
		{"a subscription", wHub, sub},
		{"an alias", wHub, "mail"},
		{"a publisher as target", wHub, "publisher+app@expa.ssh"},
		{"a publisher as sender", "publisher+app@expa.ssh", par},
		{"an instance of an address that takes none", wHub, "group.demo+a1@expa.ssh"},
	} {
		r := x.dis(c.me, "send "+c.target+" --live -m", fr)
		code(t, c.what+": exit 9", r, 9)
	}
	desc := `{"descriptor":1,"summary":"s","live":false,"requests":[]}`
	r := x.dis("group.demo@expa.ssh", "register_description", desc)
	code(t, "a description with live: false", r, 0)
	code(t, "--live to it: 9", x.dis(wHub, "send group.demo@expa.ssh --live -m", fr), 9)
	code(t, "a plain send still stores", x.dis(wHub, "send group.demo@expa.ssh -- x"), 0)
	x.dis(par, "register_description", desc)
	code(t, "and to its instances: 9", x.dis(wHub, "send "+inst("a1")+" --live -m", fr), 9)
	code(t, "live: true is live", x.dis("group.demo@expa.ssh", "register_description", strings.Replace(desc, "false", "true", 1)), 0)
	code(t, "then --live is 6 (not followed)", x.dis(wHub, "send group.demo@expa.ssh --live -m", fr), 6)
	code(t, "live: a string is refused", x.dis("group.demo@expa.ssh", "register_description", strings.Replace(desc, "false", `"no"`, 1)), 2)
	code(t, "--id event-* on --live: 2", x.dis(wHub, "send group.demo@expa.ssh --live --id event-0123456789abcdef-1 -m", fr), 2)
}

// LIVE lines come from the open-exchange table: each exchange announced once, an exchange closed before
// the follow never.
func TestLiveFromTable(t *testing.T) {
	x := instRig(t)
	x.settings("live_accept_secs=2\n")
	a := x.h.live.open(par, wHub, "m-0", true, false)
	x.h.live.close(a) // closed before anything follows
	f := x.start(x.adm(par), "recv --follow --peek")
	waitFor(t, 2*time.Second, "the follow", x.followed(par))
	fr := string(frame([]byte("x"), 4, ""))
	s1 := x.start(x.adm(wHub), "send "+par+" --live --chunked --id m-1 -m", fr)
	s2 := x.start(x.adm(wHub), "send "+par+" --live --chunked --id m-2 -m", fr)
	liveRE := regexp.MustCompile(`(?m)^LIVE [0-9a-f]{16} ` + regexp.QuoteMeta(wHub) + ` (m-\d) chunked$`)
	waitFor(t, 2*time.Second, "both LIVE lines", func() bool { return len(liveRE.FindAllString(f.out.String(), -1)) == 2 })
	x.h.follows.nudge(par)
	time.Sleep(50 * time.Millisecond)
	eq(t, "each once", len(liveRE.FindAllString(f.out.String(), -1)), 2)
	lacks(t, "a closed exchange is never printed", f.out.String(), " m-0\n")
	a2 := x.adm(par)
	a2.Conn = "c-9"
	f2 := x.start(a2, "recv --follow --peek")
	eq(t, "the older one ends, exit 0", f.wait(t, 2*time.Second), 0)
	eq(t, "with 0;end=replaced;by=c-9", final(f.out.String()), ";end=replaced;by=c-9")
	waitFor(t, 2*time.Second, "the newer follow", x.followed(par))
	waitFor(t, 2*time.Second, "the newer follow", func() bool { return len(liveRE.FindAllString(f2.out.String(), -1)) == 2 })
	time.Sleep(100 * time.Millisecond)
	eq(t, "D37: the replaced reader stops, so the newer follow announces the offers nobody took, each once", len(liveRE.FindAllString(f2.out.String(), -1)), 2)
	eq(t, "unaccepted: exit 6", s1.wait(t, 4*time.Second), 6)
	s2.wait(t, 4*time.Second)
	lacks(t, "no retry-after for a plain not-accepted", s1.err.String(), "retry-after")
}

// A refreshed transient entry outlives the timer of the one it replaced, even one that fired already.
func TestTransientRefresh(t *testing.T) {
	x := instRig(t)
	x.settings("live_retry_secs=30\n")
	x.h.transientTouch(inst("a1"), wHub)
	old := x.h.trans.m[inst("a1")][wHub]
	x.h.transientTouch(inst("a1"), wHub)
	x.h.transientEnd(inst("a1"), wHub, old) // the old timer, had it fired between Stop and the lock
	eq(t, "the refreshed entry stays", len(x.h.trans.m[inst("a1")]), 1)
}

// An instance's mailbox there but its lock not to be had is the hub's I/O: exit 1, never 3 (read as gone).
func TestInstanceStoreIOErrorIsOne(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode")
	}
	x := instRig(t)
	x.dis(par, "admit a1")
	box := filepath.Join(x.state, "inbox", inst("a1"))
	os.Chmod(box, 0o500)
	defer os.Chmod(box, 0o2770)
	r := x.dis(wHub, "send "+inst("a1")+" -- x")
	code(t, "the lock file cannot be made: exit 1", r, 1)
	has(t, "retry", r.err, "retry")
}

// instances lists an empty mailbox with no claim: the upgrade sweep rejects those.
func TestInstancesListsEmptyMailboxes(t *testing.T) {
	x := instRig(t)
	x.dis(par, "admit a1")
	eq(t, "listed", x.dis(par, "instances").out, inst("a1")+"\tmail=0\tholder=none\tclaim=none\n")
}

// A reject whose mailbox cannot be removed: exit 1, the mailbox and mail stay and nothing is reported as
// discarded; a repeated reject removes it.
func TestRejectThatCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode")
	}
	x := instRig(t)
	x.dis(par, "admit a1")
	x.dis(wHub, "send "+inst("a1")+" --id m-1 -- x")
	x.dis(wHub, "send "+inst("a1")+" --id m-2 -- y")
	box := filepath.Join(x.state, "inbox", inst("a1"))
	journal := func() string { return readString(filepath.Join(x.state, "claims", ".journal")) }
	lock := filepath.Join(box, ".lock") // made by the stores: unwritable, the mailbox lock is not to be had
	os.Chmod(lock, 0o400)
	r := x.dis(par, "reject a1")
	code(t, "exit 1", r, 1)
	has(t, "says retry", r.err, "retry")
	eq(t, "the mailbox there", isDir(box), true)
	lacks(t, "nothing reported discarded", journal(), "reject "+inst("a1"))
	x.h.Reap()
	eq(t, "the reaper leaves it", len(msgFiles(box)), 2)
	os.Chmod(lock, 0o640)
	code(t, "a repeated reject", x.dis(par, "reject a1"), 0)
	eq(t, "gone", exists(box), false)
	has(t, "the journal line", journal(), "reject "+inst("a1")+" by "+par+" discarded=2")
	eq(t, "no stage left", len(x.stages()), 0)
	eq(t, "one journal line", strings.Count(journal(), "reject "+inst("a1")+" "), 1)
}

// reject racing a store: the mailbox goes with nothing stored after it; a store waiting on the mailbox lock
// finds the mailbox gone: exit 3, and it makes none.
func TestRejectVersusStoreLocked(t *testing.T) {
	x := instRig(t)
	x.dis(par, "admit a1")
	box := filepath.Join(x.state, "inbox", inst("a1"))
	r := x.dis(par, "reject a1")
	code(t, "reject", r, 0)
	has(t, "nothing discarded", readString(filepath.Join(x.state, "claims", ".journal")), "reject "+inst("a1")+" by "+par+" discarded=0")
	eq(t, "no mailbox", exists(box), false)
	eq(t, "no stage left", len(x.stages()), 0)
}

// The wake summary, kept in memory and updated per event, equals a rebuild from disk after a random run
// of deliveries, consumes, acks, expiries, rejects, admits and follows.
func TestWakeSummaryMatchesDisk(t *testing.T) {
	x := instRig(t)
	x.settings("wake_secs=0.05\n")
	rnd := rand.New(rand.NewSource(7))
	ids := []string{"a1", "a2", "a3", "a4"}
	sers := map[string]string{}
	for _, id := range ids[:2] {
		sers[id], _ = x.register(id, id+"k")
	}
	x.dis(par, "admit a3")
	x.h.renderWake(par) // built
	for i := 0; i < 300; i++ {
		id := ids[rnd.Intn(len(ids))]
		switch rnd.Intn(8) {
		case 0, 1, 2:
			from := []string{wHub, "group.demo@expa.ssh"}[rnd.Intn(2)]
			x.dis(from, fmt.Sprintf("send %s --id m%d --subject v -- x", inst(id), i))
		case 3:
			if s := sers[id]; s != "" {
				x.idis(inst(id), s, id+"k", "recv")
			}
		case 4:
			if s := sers[id]; s != "" {
				if fs := msgFiles(x.h.mbox(inst(id))); len(fs) > 0 {
					m := strings.TrimSuffix(filepath.Base(fs[0]), ".msg")
					x.idis(inst(id), s, id+"k", "recv_ack "+m)
				}
			}
		case 5:
			if fs := msgFiles(x.h.mbox(inst(id))); len(fs) > 0 {
				x.age(inst(id), strings.TrimSuffix(filepath.Base(fs[0]), ".msg"))
				x.h.Reap()          // drops the summaries
				x.h.renderWake(par) // rebuilt, as the address's next follow pass would
			}
		case 6:
			if sers[id] == "" {
				x.dis(par, []string{"reject ", "admit "}[rnd.Intn(2)]+id)
			}
		case 7:
			if s := sers[id]; s != "" {
				f := x.start(Admission{FQ: inst(id), Serial: s, KeyID: x.keyid(id + "k")}, "recv --follow")
				waitFor(t, 2*time.Second, "follow", x.followed(inst(id)))
				time.Sleep(20 * time.Millisecond)
				f.cancel()
				f.wait(t, 2*time.Second)
			}
		}
	}
	x.h.wsum.mu.Lock()
	mem := fmt.Sprint(x.h.wsum.m[par])
	delete(x.h.wsum.m, par)
	disk := fmt.Sprint(x.h.sumAddr(par))
	x.h.wsum.mu.Unlock()
	eq(t, "the summary equals a rebuild from disk", mem, disk)
	if len(disk) < 10 {
		t.Errorf("the run left too little to compare: %s", disk)
	}
}
