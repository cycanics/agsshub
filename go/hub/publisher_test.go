package hub

// What each role may receive and send, the publisher, gap markers.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	pubID = "publisher+gitea@home.ssh"
	flID  = "frontline@home.ssh"
	tpID  = "topic.gitea.tracker-inbox+tasks@home.ssh"
)

// mkTopic writes a topic's policy, as hub-topic does.
func (x *th) mkTopic(topic, publish string) {
	td := filepath.Join(x.state, "topics", fsName(topic))
	os.MkdirAll(filepath.Join(td, "subs"), 0o2770)
	os.WriteFile(filepath.Join(td, "policy"), []byte("topic="+topic+"\npublish="+publish+"\n"), 0o640)
}

// disWithin runs a command that must end by itself within d: exit -1 when it did not (a refusal that
// does not happen would otherwise hold a follow open).
func (x *th) disWithin(a Admission, cmd string, d time.Duration) result {
	b := x.start(a, cmd, "")
	select {
	case c := <-b.done:
		return result{b.out.String(), b.err.String(), c}
	case <-time.After(d):
		b.cancel()
		<-b.done
		return result{b.out.String(), b.err.String(), -1}
	}
}

var dateRE = regexp.MustCompile(`(?m)^Date: .*$`)

func dateless(s string) string { return dateRE.ReplaceAllString(s, "Date: D") }

var arrivedRE = regexp.MustCompile(`(?m)^Arrived: [0-9]+$`)

func arrivedless(s string) string { return arrivedRE.ReplaceAllString(s, "Arrived: A") }

func TestRoleDirections(t *testing.T) {
	want := map[string]string{"main": "yes/yes", "run": "yes/yes", "foreman": "yes/yes", "frontline": "yes/yes",
		"vmmanager": "yes/yes", "inference": "yes/yes", "group": "yes/yes", "commhub": "yes/yes",
		"topic": "policy/no", "sub": "topic/no", "publisher": "no/yes", "svc": "yes/yes"}
	eq(t, "the role table: every role", len(Roles), len(want))
	for r, w := range want {
		eq(t, "what "+r+" may receive (in) and send (out)", Roles[r].In+"/"+Roles[r].Out, w)
	}
	// a role that sends nothing sends nothing: a hand-made claim of a hub role
	x := twoRuns(t)
	fake := "topic+fake@acme.ssh"
	os.WriteFile(filepath.Join(x.state, "claims/operator", fake), []byte("fq="+fake+"\nserial=1\nexpires="+itoa(x.h.epoch()+600)+"\n"), 0o640)
	a := Admission{FQ: fake, Serial: "1"}
	r := x.disA(a, "send "+bob+" -- x")
	code(t, "a role that sends nothing (out: no): send -> 3", r, 3)
	has(t, "saying so", r.err, fake+" is a topic: it sends nothing")
	code(t, "send_bag", x.disA(a, "send_bag "+bob, string(makeTar(t, map[string]string{"m.md": "x\n"}))), 3)
	code(t, "chat", x.disA(a, "chat "+bob, "x\n"), 3)
}

func TestPublisher(t *testing.T) {
	x := twoRuns(t)
	k := x.key("pubk").line
	for _, c := range []struct {
		id    string
		flags []string
		code  int
		what  string
	}{
		{"publisher@home.ssh", nil, 2, "a publisher names its app: +<app> is required"},
		{"publisher.x+gitea@home.ssh", nil, 2, "and takes no project"},
		{"publisher+gitea/ws-a@home.ssh", nil, 2, "it is ws-less"},
		{pubID, []string{"--instances"}, 3, "it takes no instances: +<app> is its name"},
	} {
		out, rc := x.mint(k, append([]string{"add", c.id}, c.flags...)...)
		eq(t, c.what+" ("+strings.TrimSpace(out)+")", rc, c.code)
	}
	r := x.reg("ws-a", "register "+pubID+" --ttl 600", k)
	code(t, "no workspace mints one", r, 3)
	has(t, "the operator does", r.err, "hub-mint")
	out, rc := x.mint(k, "add", pubID)
	eq(t, "the operator mints it", rc, 0)
	lacks(t, "an address, not an instance of one", out, "instances")
	a, ok := x.h.admitHub(x.key("pubk").pub)
	eq(t, "the daemon admits its key as the publisher", ok && a.FQ == pubID, true)
	eq(t, "its session", x.dis(pubID, "id").out, pubID+" serial=1\n")
	noBox := func(what string) {
		if exists(filepath.Join(x.state, "inbox", pubID)) {
			t.Errorf("%s: the publisher has a mailbox", what)
		}
	}
	noBox("it has no mailbox")

	x.mustMint(flID, "flk")
	x.mkTopic(tpID, pubID)
	x.mkTopic("topic+other@home.ssh", "commhub@home.ssh")
	fs := strings.TrimSpace(x.dis(flID, "sub "+tpID).out)
	r = x.dis(pubID, "send "+tpID+" -m:rfc5322", "Subject: issues\n\n{\"action\":\"opened\"}\n")
	code(t, "a publisher publishes to a topic whose policy names it", r, 0)
	eq(t, "(fanned out)", r.out, "published -> "+tpID+" msgid="+x.h.topicDone(tpID).msgid()+" subs=1\n")
	codes := []int{
		x.dis(pubID, "send "+tpID+" -- inline").code,
		x.dis(pubID, "send "+tpID+" -m", "body\n").code,
		x.dis(pubID, "send "+tpID+" --file f.bin", "bytes").code,
		x.dis(pubID, "send "+tpID+" -m --chunked", string(frame([]byte("framed body"), 4, ""))).code,
		x.dis(pubID, "send_bag "+tpID+" --msg m.md", string(makeTar(t, map[string]string{"m.md": "bagbody\n"}))).code,
	}
	eq(t, "every send form: inline, -m, --file, --chunked, send_bag", fmt.Sprint(codes), "[0 0 0 0 0]")
	eq(t, "each reaches the subscription, from the publisher", strings.Count(x.dis(flID, "recv "+fs+" --peek").out, "\nFrom: "+pubID+"\n"), 6)
	code(t, "a topic whose policy does not name it refuses it", x.dis(pubID, "send topic+other@home.ssh -- nope"), 3)
	r = x.dis(pubID, "send "+flID+" -- direct")
	code(t, "it sends to topics only: an identity -> 3", r, 3)
	has(t, "saying so", r.err, pubID+" is a publisher: it sends to topics only")
	os.WriteFile(filepath.Join(x.state, "aliases", "carol"), []byte("type=email\naddr=carol@example.com\n"), 0o640)
	codes = []int{
		x.dis(pubID, "send_bag "+flID+" --msg m.md", string(makeTar(t, map[string]string{"m.md": "x\n"}))).code,
		x.dis(pubID, "send carol -- x").code,
		x.dis(pubID, "send "+fs+" -- x").code,
	}
	eq(t, "nor a bag to an identity, nor an alias, nor a subscription", fmt.Sprint(codes), "[3 3 3]")
	r = x.dis(pubID, "send "+flID+" --live -m", string(frame([]byte("x"), 4, "")))
	code(t, "nor --live (that waits for a reply): live is not supported", r, 9)
	has(t, "saying why", r.err, "it receives nothing, so it sends no --live")
	eq(t, "nothing reached the identity", x.dis(flID, "recv --peek").out, empty)
	bad := ""
	for _, v := range []string{"", "recv", "recv --peek", "recv --follow", "recv --follow --peek", "recv_ack pub-1",
		"recv --id pub-1 --peek:rfc5322", "recv --id pub-4 --peek:file", "recv " + fs + " --peek", "recv " + fs + " --follow", "recv_bag --id pub-6",
		"sub " + tpID, "unsub " + fs, "accept 0123456789abcdef", "register x1", "release",
		"instances", "list", "list --type topic"} {
		if r := x.disWithin(x.adm(pubID), v, 5*time.Second); r.code != 3 {
			bad += fmt.Sprintf("[%s]=%d ", v, r.code)
		}
	}
	eq(t, "it receives nothing: every verb that needs a mailbox or a reply -> 3", bad, "")
	code(t, "chat: live is not supported for a publisher -> 9", x.dis(pubID, "chat "+flID), 9)
	has(t, "saying so", x.dis(pubID, "recv").err, pubID+" is a publisher: it receives nothing, so recv is not for it")
	code(t, "an unknown verb is still a usage error", x.dis(pubID, "frob"), 2)
	noBox("after its refusals")

	hub := "commhub@home.ssh"
	x.mustMint(hub, "hubk")
	r = x.dis(hub, "send "+pubID+" -- hello")
	code(t, "a send to a publisher is refused (exit 3)", r, 3)
	has(t, "it receives nothing", r.err, pubID+" is a publisher: it receives nothing")
	codes = []int{
		x.dis(hub, "send "+pubID+" -m:rfc5322", "Subject: x\n\nb\n").code,
		x.dis(hub, "send_bag "+pubID+" --msg m.md", string(makeTar(t, map[string]string{"m.md": "x\n"}))).code,
		x.dis(hub, "send "+pubID+" --live -m", string(frame([]byte("x"), 4, ""))).code,
	}
	eq(t, "nor -m:rfc5322, send_bag; --live is not supported", fmt.Sprint(codes), "[3 3 9]")
	code(t, "nor a chat: 9", x.dis(hub, "chat "+pubID), 9)
	noBox("none of it made it a mailbox")
}

func TestGapMarkers(t *testing.T) {
	x := twoRuns(t)
	x.mustMint(pubID, "pubk")
	x.mustMint(flID, "flk")
	x.mkTopic(tpID, pubID)
	fs := strings.TrimSpace(x.dis(flID, "sub "+tpID).out)
	r := x.dis(pubID, "send "+tpID+" -m:rfc5322", "Gap: start\n\n")
	code(t, "a gap marker: Gap alone, no body, to a topic", r, 0)
	g1 := x.h.topicDone(tpID).msgid()
	eq(t, "(published)", r.out, "published -> "+tpID+" msgid="+g1+" subs=1\n")
	has(t, "stored: the hub's fields, Gap, an empty body", dateless(x.stored(fs, g1)), "From: "+pubID+"\nDate: D\nSerial: 1\nGap: start\n\n")
	has(t, "the listing shows its Gap", x.dis(flID, "recv "+fs+" --peek").out, "Gap: start")
	code(t, "with a body too, the field name in any case", x.dis(pubID, "send "+tpID+" -m:rfc5322", "gap: answered 502\n\nthe app got a 502\n"), 0)
	has(t, "(stored as Gap)", x.stored(fs, x.h.topicDone(tpID).msgid()), "Gap: answered 502")
	code(t, "chunked too", x.dis(pubID, "send "+tpID+" -m:rfc5322 --chunked", string(frame([]byte("Gap: start\n\n"), 5, ""))), 0)
	bad := ""
	for _, f := range []string{"Subject", "Reply-To", "Re", "Deadline"} {
		r := x.dis(pubID, "send "+tpID+" -m:rfc5322", "Gap: start\n"+f+": x\n\nb\n")
		if r.code != 2 || !strings.Contains(r.err, "a gap marker carries Gap alone, not "+f) {
			bad += fmt.Sprintf("%s=%d ", f, r.code)
		}
	}
	eq(t, "Gap with any other client field -> 2, naming it", bad, "")
	code(t, "in either order", x.dis(pubID, "send "+tpID+" -m:rfc5322", "Subject: x\nGap: start\n\nb\n"), 2)
	code(t, "two Gap fields -> 2", x.dis(pubID, "send "+tpID+" -m:rfc5322", "Gap: a\nGap: b\n\n"), 2)
	code(t, "a Gap without its reason -> 2", x.dis(pubID, "send "+tpID+" -m:rfc5322", "Gap:\n\n"), 2)
	code(t, "the reason on Gap's own line (a listing shows only that line)", x.dis(pubID, "send "+tpID+" -m:rfc5322", "Gap:   \n  folded reason\n\n"), 2)
	r = x.dis(alice, "send "+bob+" -m:rfc5322 --id gx-id", "Gap: start\n\nb\n")
	code(t, "Gap in a send to an identity -> 2", r, 2)
	has(t, "saying so", r.err, "Gap is allowed only in a send to a topic")
	if exists(filepath.Join(x.state, "inbox", fsName(bob), "gx-id.msg")) {
		t.Error("a refused Gap was stored")
	}
	grp, hub := "group.demo@expa.ssh", "commhub@home.ssh"
	x.mustMint(grp, "grpk")
	x.mustMint(hub, "hubk")
	f := x.start(x.adm(grp), "recv --follow --peek")
	waitFor(t, 2*time.Second, "the recipient's follow", x.followed(grp))
	r = x.dis(hub, "send "+grp+" --live --chunked --id gx-live -m:rfc5322", string(frame([]byte("Gap: start\n\n"), 64, "")))
	code(t, "Gap in a --live send -> 2, before it is offered", r, 2)
	has(t, "saying so", r.err, "Gap is allowed only in a send to a topic")
	lacks(t, "(nothing offered)", f.out.String(), "LIVE ")
	f.cancel()
	f.wait(t, 2*time.Second)
	eq(t, "of all that, the subscription holds the three markers", seqs(x.dis(flID, "recv "+fs+" --peek").out), "1 2 3")
}

// The hub enforces the role, so it holds for the publisher's key in any ssh client: it publishes and
// is refused the rest, over the daemon's own SSH server.
func TestServerPublisher(t *testing.T) {
	x := twoRuns(t)
	x.mustMint(pubID, "pubk")
	x.mustMint(flID, "flk")
	x.mkTopic(tpID, pubID)
	fs := strings.TrimSpace(x.dis(flID, "sub "+tpID).out)
	ts := startServer(t, x, ServerConfig{})
	c := ts.dial(t, "hub", "pubk")
	o, _, rc, err := runSSH(c, "send "+tpID+" -m:rfc5322", "Subject: issues\n\n{}\n")
	s1 := x.h.topicDone(tpID).msgid()
	eq(t, "over ssh, the publisher's key publishes", fmt.Sprint(rc, " ", err, " ", o), "0 <nil> published -> "+tpID+" msgid="+s1+" subs=1\n")
	_, e, rc, _ := runSSH(c, "recv --peek", "")
	eq(t, "and is refused a read", fmt.Sprint(rc, " ", e), "3 dispatch: "+pubID+" is a publisher: it receives nothing, so recv is not for it (send, send_bag, id and help are)\n")
	_, e, rc, _ = runSSH(c, "send "+flID+" -- x", "")
	eq(t, "and a send to an identity", fmt.Sprint(rc, " ", e), "3 dispatch: "+pubID+" is a publisher: it sends to topics only, and "+flID+" is not one\n")
	has(t, "the event reached the subscription, from the publisher", msgOf(x.dis(flID, "recv "+fs+" --peek").out, s1), "From: "+pubID+"\n")
}

// svc is a service's role: everything a group may do, named apart from agent groups.
func TestServiceRole(t *testing.T) {
	eq(t, "the same row as group", Roles["svc"], Roles["group"])
	const svcID = "svc.git-mirror@home.ssh"
	r := newLiveRig(t)
	x := r.x
	for _, c := range []struct {
		id, what string
		code     int
	}{
		{"svc@home.ssh", "it names its service: .<project> is required", 2},
		{"svc.git-mirror/ws-a@home.ssh", "it is ws-less", 2},
	} {
		out, rc := x.mint(x.key("svck").line, "add", c.id)
		eq(t, c.what+" ("+strings.TrimSpace(out)+")", rc, c.code)
	}
	code(t, "no workspace mints one", x.reg("ws-a", "register "+svcID+" --ttl 600", x.key("svck").line), 3)
	x.mustMint(svcID, "svck")
	os.WriteFile(filepath.Join(x.state, "budgets", "svc"), []byte("subject=svc\nrate=50\nburst=500\n"), 0o640)
	eq(t, "its session", x.dis(svcID, "id").out, svcID+" serial=1\n")
	if !exists(filepath.Join(x.state, "inbox", svcID)) {
		t.Fatal("no mailbox")
	}
	code(t, "it is sent to", x.dis(r.grp, "send "+svcID+" --id to-svc -- hello"), 0)
	has(t, "and reads its mailbox", msgOf(x.dis(svcID, "recv --peek").out, "to-svc"), "From: "+r.grp+"\n")
	code(t, "it sends", x.dis(svcID, "send "+r.grp+" --id from-svc -- hi"), 0)
	has(t, "(received)", msgOf(x.dis(r.grp, "recv --peek").out, "from-svc"), "From: "+svcID+"\n")

	r.follow()
	defer r.close()
	res := x.dis(svcID, "send "+r.hub+" --live --chunked --id SV1 -m", string(frame([]byte("q"), 4, "")))
	code(t, "a live request from it", res, 0)
	eq(t, "answered", res.out, "Status: 200\nContent-Type: text/plain\n\ngot SV1")

	x.mkTopic(tpID, svcID)
	ss := strings.TrimSpace(x.dis(svcID, "sub "+tpID).out)
	if !strings.HasPrefix(ss, "sub.") {
		t.Fatalf("sub --live: %q", ss)
	}
	ws := ss
	res = x.dis(svcID, "send "+tpID+" -m:rfc5322", "Subject: e\n\n{}\n")
	code(t, "it publishes to a topic whose policy names it", res, 0)
	eq(t, "its subscription receives it", seqs(x.dis(svcID, "recv "+ss+" --peek").out), "1")
	code(t, "and acks", x.dis(svcID, "recv_ack "+ss+" "+pubMsgid(res.out)), 0)
	code(t, "unsub", x.dis(svcID, "unsub "+ws), 0)
	_, rc := x.mint(x.key("svck").line, "add", svcID, "--instances")
	eq(t, "it takes instances, as a group does", rc, 0)
}
