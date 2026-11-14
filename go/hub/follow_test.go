package hub

// Topics, subscriptions, and following a mailbox.

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTopics(t *testing.T) {
	x := twoRuns(t)
	t1 := "topic+builds@acme.ssh"
	td := filepath.Join(x.state, "topics", t1)
	os.MkdirAll(filepath.Join(td, "subs"), 0o2770)
	os.WriteFile(filepath.Join(td, "policy"), []byte("topic="+t1+"\npublish="+alice+" main/*@acme.ssh\nsubscribe=*@acme.ssh\n"), 0o640)

	s1 := strings.TrimSpace(x.dis(bob, "sub "+t1).out)
	has(t, "sub prints a subscription name", s1, "sub.builds+")
	has(t, "in the topic's org", s1, "@acme.ssh")
	eq(t, "sub is idempotent", strings.TrimSpace(x.dis(bob, "sub "+t1).out), s1)
	s2 := strings.TrimSpace(x.dis(alice, "sub "+t1).out)
	if s2 == s1 || s2 == "" {
		t.Errorf("a second subscriber gets its own name: %q %q", s1, s2)
	}
	code(t, "another org may not subscribe by default", x.dis(cara, "sub "+t1), 3)
	eq(t, "--live is accepted and changes nothing", strings.TrimSpace(x.dis(bob, "sub "+t1).out), s1)
	code(t, "--wake exits 2", x.dis(bob, "sub "+t1+" --wake"), 2)
	code(t, "sub to an unknown topic -> 3", x.dis(bob, "sub topic+nope@acme.ssh"), 3)

	out := x.dis(alice, "send "+t1+" -- build-42-green").out
	sess := x.h.topicDone(t1).id
	e := func(q int64) string { return ev(sess, q) }
	eq(t, "publish reports its fan-out", out, "published -> "+t1+" msgid="+e(1)+" subs=2\n")
	has(t, "bob's subscription has the event", x.dis(bob, "recv "+s1+" --peek").out, "build-42-green")
	has(t, "from the publisher", x.dis(bob, "recv "+s1+" --peek").out, "From: "+alice)
	has(t, "alice's subscription has it too", x.dis(alice, "recv "+s2+" --peek").out, "build-42-green")
	eq(t, "the topic keeps no copy (its record aside)", x.leftOf(t1), "policy subs|")
	eq(t, "the record", exists(filepath.Join(td, "record")), true)
	var st1, st2 syscall.Stat_t
	syscall.Stat(filepath.Join(x.state, "inbox", s1, e(1)+".msg"), &st1)
	syscall.Stat(filepath.Join(x.state, "inbox", s2, e(1)+".msg"), &st2)
	eq(t, "one inode, linked into each mailbox as its event msgid", st1.Ino, st2.Ino)
	eq(t, "and it is there", st1.Ino != 0, true)
	x.dis(bob, "recv "+s1)
	r := x.dis(alice, "send "+t1+" --id "+e(1)+" -- build-42-green")
	eq(t, "a republished msgid in the record's session: dup", fmt.Sprint(r.code, "|", r.out), "0|dup\n")
	eq(t, "nothing delivered", x.view(s1, bob), "")
	x.dis(alice, "recv_ack "+s2+" "+e(1))
	eq(t, "an ack takes the one delivery", x.view(s2, alice), "")
	code(t, "an unlisted publisher is refused", x.dis(bob, "send "+t1+" -- nope"), 3)
	code(t, "a subscription cannot be sent to directly", x.dis(alice, "send "+s1+" -- direct"), 3)
	code(t, "only the owner reads a subscription", x.dis(alice, "recv "+s1), 3)
	code(t, "nor follows it", x.dis(alice, "recv "+s1+" --follow"), 3)
	x.dis(alice, "send "+t1+" -m:rfc5322", "Subject: nightly\n\nall green\n")
	has(t, "fields travel through a topic", x.dis(bob, "recv "+s1+" --peek").out, "Subject: nightly")
	x.dis(bob, "recv "+s1)
	art := make([]byte, 5000)
	rand.Read(art)
	x.dis(alice, "send "+t1+" --file art.bin", string(art))
	has(t, "a file event names its payload on the subscription", x.dis(bob, "recv "+s1+" --peek").out, "Payload: file art.bin\n")
	eq(t, "file through a topic round-trips", partOf(x.dis(bob, "recv "+s1+" --id "+e(3)).out, e(3), "payload") == string(art), true)
	eq(t, "(consumed by the full read)", exists(filepath.Join(x.h.mbox(s1), e(3)+".msg")), false)
	eq(t, "the other subscriber still has its copy", x.dis(alice, "recv "+s2+" --id "+e(3)+" --peek:file").out, string(art))
	x.dis(alice, "send_bag "+t1+" --msg README.md", string(makeTar(t, map[string]string{"README.md": "readme\n", "sub/x.txt": "inner\n"})))
	eq(t, "a bag through a topic", bodyOf(x.dis(bob, "recv "+s1+" --id "+e(4)+" --peek").out, e(4)), "readme\n")
	eq(t, "and its member", x.dis(bob, "recv "+s1+" --id "+e(4)+" --peek:bag sub/x.txt").out, "inner\n")

	// follow a subscription: the backlog, then live; consumed as printed; a newer follower wins
	x.dis(bob, "recv "+s1)
	x.dis(bob, "recv_ack "+s1+" "+e(4))
	x.dis(alice, "send "+t1+" -- backlog-one")
	f := x.start(x.adm(bob), "recv "+s1+" --follow", "")
	waitFor(t, 2*time.Second, "the backlog", func() bool { return strings.Contains(f.out.String(), "backlog-one") })
	x.dis(alice, "send "+t1+" -- live-two")
	waitFor(t, 2*time.Second, "the new event", func() bool { return strings.Contains(f.out.String(), "live-two") })
	files, _ := filepath.Glob(filepath.Join(x.state, "inbox", s1, "*.msg"))
	eq(t, "follow consumed what it printed", len(files), 0)
	a2 := x.adm(bob)
	a2.Conn = "c-77"
	f2 := x.start(a2, "recv "+s1+" --follow", "")
	eq(t, "a second follower replaces the first, which exits 0 after its final chunk", f.wait(t, 3*time.Second), 0)
	has(t, "saying so", f.err.String(), "-- "+s1+" is followed elsewhere now (conn c-77) --")
	eq(t, "its stream ends with 0;end=replaced;by=<the new connection>", final(f.out.String()), ";end=replaced;by=c-77")
	waitFor(t, 2*time.Second, "the new follower", x.followed(s1))
	x.dis(alice, "send "+t1+" -- to-the-new-one")
	waitFor(t, 2*time.Second, "the new follower's event", func() bool { return strings.Contains(f2.out.String(), "to-the-new-one") })
	f2.cancel()

	cout := x.dis(bob, "list --type sub").out
	has(t, "list --type sub shows mine", cout, s1+"\ttype=sub\ttopic="+t1+"\texpires=")
	lacks(t, "and not someone else's", cout, s2)
	has(t, "list --type topic shows what I may do", x.dis(bob, "list --type topic").out, t1+"\ttype=topic\tpublish=no\tsubscribe=yes")
	code(t, "chat to a topic: 9", x.dis(alice, "chat "+t1), 9)
	eq(t, "unsub", x.dis(bob, "unsub "+s1).out, "unsubscribed "+s1+"\n")
	if exists(filepath.Join(x.state, "inbox", s1)) {
		t.Error("unsub left the mailbox")
	}
	code(t, "only the owner unsubscribes", x.dis(bob, "unsub "+s2), 3)
	rewriteKey(filepath.Join(td, "subs", s2), "expires", "1")
	x.h.Reap()
	if exists(filepath.Join(x.state, "subs", s2)) || exists(filepath.Join(x.state, "inbox", s2)) {
		t.Error("the reaper left an expired subscription or its mailbox")
	}
	// policy patterns are bash patterns: '*' crosses '/', brackets, negation
	for _, c := range []struct {
		pat, id string
		ok      bool
	}{
		{"*@acme.ssh", "main/ws-a@acme.ssh", true},
		{"main/*@acme.ssh", "main/ws-a@acme.ssh", true},
		{"run.ci+*@acme.ssh", "run.ci+x1/ws-a@acme.ssh", true},
		{"run.[a-c]i+*", "run.ci+x1/ws-a@acme.ssh", true},
		{"run.[!c]i+*", "run.ci+x1/ws-a@acme.ssh", false},
		{"?ain/*", "main/ws-a@acme.ssh", true},
		{"group.*@expa.ssh", "group.demo@expa.ssh", true},
		{"group.*@expa.ssh", "commhub@home.ssh", false},
	} {
		eq(t, "pattern "+c.pat+" ~ "+c.id, bashMatch(c.pat, c.id), c.ok)
		got := strings.TrimSpace(x.bash(`[[ $1 == $2 ]] && echo true || echo false`, c.id, c.pat))
		eq(t, "(bash agrees)", got, fmt.Sprint(c.ok))
	}
}

func TestFollowOwnMailbox(t *testing.T) {
	x := twoRuns(t)
	x.dis(alice, "send "+bob+" --id m-2 -- second")
	x.dis(alice, "send "+bob+" --id m-1 -- first")
	o := x.start(x.adm(bob), "recv "+bob+" --follow --peek", "")
	waitFor(t, 2*time.Second, "the backlog", func() bool { return strings.Contains(o.out.String(), ";id=m-2;") })
	eq(t, "follow prints the backlog in arrival order", ids(o.out.String()), "m-2 m-1")
	t0 := time.Now()
	x.dis(alice, "send "+bob+" --id m-3 -- third")
	waitFor(t, 2*time.Second, "m-3", func() bool { return strings.Contains(o.out.String(), ";id=m-3;") })
	if d := time.Since(t0); d > 5*time.Second { // the poll is 30 s; the send itself takes its fsyncs' time
		t.Errorf("new mail reached the follower after %v: polled, not woken", d)
	}
	files, _ := filepath.Glob(filepath.Join(x.state, "inbox", fsName(bob), "*.msg"))
	eq(t, "--peek consumes nothing", len(files), 3)
	code(t, "acks work beside the follow", x.dis(bob, "recv_ack m-1"), 0)
	code(t, "with the mailbox named too", x.dis(bob, "recv_ack "+bob+" m-2"), 0)
	x.dis(alice, "send "+bob+" --id m-4 -- fourth")
	waitFor(t, 2*time.Second, "m-4", func() bool { return strings.Contains(o.out.String(), ";id=m-4;") })
	eq(t, "each message is shown once", ids(o.out.String()), "m-2 m-1 m-3 m-4")
	x.dis(alice, "send_bag "+bob+" --msg m.md --id m-5", string(makeTar(t, map[string]string{"m.md": "bagbody\n"})))
	waitFor(t, 2*time.Second, "so does a bag", func() bool { return strings.Contains(o.out.String(), ";id=m-5;") })
	has(t, "a bag's entry: its body", o.out.String(), "\n\nbagbody\n\r\n")
	eq(t, "a --peek follow streams no payload", partOf(o.out.String(), "m-5", "payload"), "")
	eq(t, "recv_bag <own identity>", len(x.dis(bob, "recv_bag "+bob+" --id m-5 --peek").out), 10240)
	a2 := x.adm(bob)
	a2.Conn = "c2"
	o2 := x.start(a2, "recv --follow --peek", "")
	eq(t, "a newer follower replaces the older, which exits 0 after its final chunk", o.wait(t, 3*time.Second), 0)
	eq(t, "the replaced stream: valid from its first byte, final 0;end=replaced;by=c2", final(o.out.String()), ";end=replaced;by=c2")
	has(t, "and says so", o.err.String(), "followed elsewhere")
	waitFor(t, 2*time.Second, "the new one's backlog", func() bool { return strings.Contains(o2.out.String(), ";id=m-5;") })
	has(t, "recv --follow without a mailbox is your own", o2.out.String(), ";id=m-3;")
	o2.cancel()
	o2.wait(t, 2*time.Second)
	o3 := x.start(x.adm(bob), "recv --follow", "")
	waitFor(t, 2*time.Second, "m-5", func() bool { return strings.Contains(o3.out.String(), ";id=m-5;") })
	files, _ = filepath.Glob(filepath.Join(x.state, "inbox", fsName(bob), "*.msg"))
	eq(t, "without --peek the follow consumes what it streams, payloads included", len(files), 0)
	eq(t, "the full follow streamed the bag's payload", len(partOf(o3.out.String(), "m-5", "payload")), 10240)
	o3.cancel()
	eq(t, "the session ending ends the follow", o3.wait(t, time.Second), 0)
	eq(t, "with no final chunk", final(o3.out.String()), "-")
	eq(t, "and its registration", x.h.follows.following(bob), false)

	// a rebind ends the old key's follow at once
	x.mustMint("group.demo@expa.ssh", "grpk")
	g := x.start(Admission{FQ: "group.demo@expa.ssh", Serial: "1", KeyID: x.keyid("grpk")}, "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the group follow", x.followed("group.demo@expa.ssh"))
	x.mint(x.key("grpk2").line, "add", "group.demo@expa.ssh", "--replace") // hub-mint: the daemon observes it
	eq(t, "rebinding the identity ends the old key's follow at once (observed: well inside the 1 s re-check)", g.wait(t, 300*time.Millisecond), 0)
	has(t, "the mailbox is gone for it", g.err.String(), "-- group.demo@expa.ssh is gone --")
	eq(t, "its stream ends with 0;end=gone", final(g.out.String()), ";end=gone")

	// the reaper expiring a claim ends its follow
	fol := "run.t+fol/ws-a@acme.ssh"
	x.reg("ws-a", "register "+fol+" --ttl 600", x.key("folk").line)
	ff := x.start(x.adm(fol), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "its follow", x.followed(fol))
	rewriteKey(filepath.Join(x.state, "claims/ws-a", fsName(fol)), "expires", "1")
	x.h.Reap()
	eq(t, "the reaper expiring a claim ends its follow at once", ff.wait(t, 2*time.Second), 0)
	// a registrar release ends it too
	x.reg("ws-a", "register "+fol+" --ttl 600", x.key("folk").line)
	ff = x.start(x.adm(fol), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "its follow", x.followed(fol))
	x.reg("ws-a", "release "+fol, "")
	eq(t, "a release ends its follow at once", ff.wait(t, 2*time.Second), 0)
}

func baseNames(ps []string) []string {
	var out []string
	for _, p := range ps {
		out = append(out, filepath.Base(p))
	}
	return out
}
