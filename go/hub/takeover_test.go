package hub

// D37: one follower per mailbox; a takeover replaces only the connection following that mailbox.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateWriter: once armed, every Write blocks until the gate opens — a client that stopped reading.
type gateWriter struct {
	buf     syncBuf
	armed   atomic.Bool
	blocked chan struct{} // closed at the first blocked Write
	once    sync.Once
	gate    chan struct{}
}

func newGateWriter() *gateWriter {
	return &gateWriter{blocked: make(chan struct{}), gate: make(chan struct{})}
}

func (g *gateWriter) Write(p []byte) (int, error) {
	if g.armed.Load() {
		g.once.Do(func() { close(g.blocked) })
		<-g.gate
	}
	return g.buf.Write(p)
}

func admc(x *th, me, conn string) Admission {
	a := x.adm(me)
	a.Conn = conn
	return a
}

// A takeover ends the old follow with exactly 0;end=replaced;by=<new conn>, exit 0, logged once; the
// identity's other live connections — a follow of its subscription, a chat it accepted, a send — go on.
func TestFollowTakeoverOnlyTheFollower(t *testing.T) {
	r := newLiveRig(t)
	x, hub, grp := r.x, r.hub, r.grp
	t1 := "topic+builds@acme.ssh"
	td := filepath.Join(x.state, "topics", t1)
	os.MkdirAll(filepath.Join(td, "subs"), 0o2770)
	os.WriteFile(filepath.Join(td, "policy"), []byte("topic="+t1+"\npublish="+grp+"\nsubscribe=*@home.ssh\n"), 0o640)
	sub := strings.TrimSpace(x.dis(hub, "sub "+t1).out)
	has(t, "a subscription", sub, "sub.builds+")

	old := x.start(admc(x, hub, "c-old"), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the follow", x.followed(hub))
	sf := x.start(admc(x, hub, "c-sub"), "recv "+sub+" --follow --peek", "")
	waitFor(t, 2*time.Second, "the subscription's follow", x.followed(sub))
	// a chat the identity accepted, on another connection
	c := x.start(x.adm(grp), "chat "+hub)
	waitFor(t, 2*time.Second, "the offer", func() bool { return len(liveRE.FindAllStringSubmatch(old.out.String(), -1)) >= 1 })
	sid := liveRE.FindAllStringSubmatch(old.out.String(), -1)[0][1]
	a := x.start(admc(x, hub, "c-acc"), "accept "+sid)
	io.WriteString(c.in, "before\n")
	waitFor(t, 2*time.Second, "the chat's bytes", func() bool { return strings.HasSuffix(a.out.String(), "before\n") })

	nf := x.start(admc(x, hub, "c-new"), "recv --follow --peek", "")
	eq(t, "the old follow exits 0", old.wait(t, 3*time.Second), 0)
	eq(t, "with exactly the final chunk 0;end=replaced;by=<new conn>", final(old.out.String()), ";end=replaced;by=c-new")
	eq(t, "the stream ends with those bytes", strings.HasSuffix(old.out.String(), "\r\n0;end=replaced;by=c-new\r\n\r\n"), true)
	has(t, "stderr says so", old.err.String(), "-- "+hub+" is followed elsewhere now (conn c-new) --")
	eq(t, "the takeover is logged once", strings.Count(x.logs.String(), "follow "+hub+" conn=c-new replaced conn=c-old\n"), 1)
	waitFor(t, 2*time.Second, "the new follower", func() bool { f := x.h.follows.get(hub); return f != nil && f.conn == "c-new" })

	// everything else of the identity goes on
	code(t, "a send on another connection", x.disA(admc(x, hub, "c-send"), "send "+grp+" -- during"), 0)
	x.dis(grp, "send "+t1+" -- after-takeover")
	waitFor(t, 2*time.Second, "the subscription's follow gets the event", func() bool { return strings.Contains(sf.out.String(), "after-takeover") })
	io.WriteString(c.in, "after\n")
	waitFor(t, 2*time.Second, "the chat still relays", func() bool { return strings.HasSuffix(a.out.String(), "after\n") })
	for _, b := range []*bg{sf, c, a, nf} {
		select {
		case <-b.done:
			t.Errorf("another connection of the identity ended: %q %q", b.out.String(), b.err.String())
		default:
		}
		lacks(t, "and got no replaced", b.out.String()+b.err.String(), "replaced")
		lacks(t, "(nor its stderr line)", b.err.String(), "followed elsewhere")
	}
	eq(t, "one takeover logged in all", strings.Count(x.logs.String(), " replaced conn="), 1)
	x.dis(hub, "send "+hub+" --id m-new -- to-the-new")
	waitFor(t, 2*time.Second, "the new follow streams", func() bool { return strings.Contains(nf.out.String(), ";id=m-new;") })
	lacks(t, "the old one streams nothing after it ended", old.out.String(), "m-new")
	c.in.Close()
	a.in.Close()
}

// The takeover holds the follow lock for map operations only: with the old follow blocked in a write (a
// client that stopped reading), the new follow registers and streams, the lock is free, and the old one ends
// with its final chunk once its write returns — streaming nothing that arrived after the takeover.
func TestFollowTakeoverNoWriteUnderLock(t *testing.T) {
	x := twoRuns(t)
	x.dis(alice, "send "+bob+" --id m-1 -- one")
	gw := newGateWriter()
	old := x.startOut(admc(x, bob, "c-old"), "recv --follow --peek", gw)
	waitFor(t, 2*time.Second, "the backlog", func() bool { return strings.Contains(gw.buf.String(), ";id=m-1;") })
	gw.armed.Store(true)
	x.dis(alice, "send "+bob+" --id m-2 -- two")
	select {
	case <-gw.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("the old follow never wrote m-2")
	}
	nf := x.start(admc(x, bob, "c-new"), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the new follower registered", func() bool { f := x.h.follows.get(bob); return f != nil && f.conn == "c-new" })
	waitFor(t, 2*time.Second, "the new follow's backlog", func() bool { return strings.Contains(nf.out.String(), ";id=m-2;") })
	waitFor(t, time.Second, "the follow lock free while the old writer blocks", func() bool {
		if x.h.follows.mu.TryLock() {
			x.h.follows.mu.Unlock()
			return true
		}
		return false
	})
	x.dis(alice, "send "+bob+" --id m-3 -- three")
	waitFor(t, 2*time.Second, "a delivery reaches the new follower", func() bool { return strings.Contains(nf.out.String(), ";id=m-3;") })
	select {
	case <-old.done:
		t.Fatal("the old follow ended while its write was blocked")
	default:
	}
	close(gw.gate)
	eq(t, "the old follow exits 0 once its write returns", old.wait(t, 3*time.Second), 0)
	eq(t, "with 0;end=replaced;by=c-new", final(gw.buf.String()), ";end=replaced;by=c-new")
	has(t, "after what it was writing", gw.buf.String(), ";id=m-2;")
	lacks(t, "and nothing that arrived after the takeover", gw.buf.String(), ";id=m-3;")
	nf.cancel()
}

// A takeover in the middle of a pass ends it there: the old follow, blocked writing the first of a backlog,
// streams nothing after that entry once its write returns.
func TestFollowTakeoverMidPass(t *testing.T) {
	x := twoRuns(t)
	big := strings.Repeat("x", 40<<10) // over the session's 32 KiB stdout buffer: its write leaves mid-pass
	for _, id := range []string{"b-1", "b-2", "b-3"} {
		x.dis(alice, "send "+bob+" --id "+id+" -m", big)
	}
	gw := newGateWriter()
	gw.armed.Store(true)
	old := x.startOut(admc(x, bob, "c-old"), "recv --follow --peek", gw)
	select {
	case <-gw.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("the old follow never wrote")
	}
	nf := x.start(admc(x, bob, "c-new"), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the new follow's backlog", func() bool { return strings.Contains(nf.out.String(), ";id=b-3;") })
	close(gw.gate)
	eq(t, "the old follow exits 0", old.wait(t, 3*time.Second), 0)
	eq(t, "with 0;end=replaced;by=c-new", final(gw.buf.String()), ";end=replaced;by=c-new")
	has(t, "after the entry it was writing", gw.buf.String(), ";id=b-1;")
	lacks(t, "and none after it", gw.buf.String(), ";id=b-2;")
	lacks(t, "(nor the last)", gw.buf.String(), ";id=b-3;")
	nf.cancel()
}

// A replaced follow hands back the LIVE offers it announced and nobody took: its reader stops, so the
// new follow announces them.
func TestFollowTakeoverHandsBackOffers(t *testing.T) {
	r := newLiveRig(t)
	x, hub, grp := r.x, r.hub, r.grp
	old := x.start(admc(x, hub, "c-old"), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the follow", x.followed(hub))
	c := x.start(x.adm(grp), "chat "+hub)
	waitFor(t, 2*time.Second, "the offer on the old follow", func() bool { return liveRE.MatchString(old.out.String()) })
	nf := x.start(admc(x, hub, "c-new"), "recv --follow --peek", "")
	eq(t, "the old follow ends", old.wait(t, 3*time.Second), 0)
	waitFor(t, 2*time.Second, "the offer on the new follow", func() bool { return liveRE.MatchString(nf.out.String()) })
	sid := liveRE.FindStringSubmatch(nf.out.String())[1]
	eq(t, "the same exchange", sid, liveRE.FindStringSubmatch(old.out.String())[1])
	a := x.start(admc(x, hub, "c-acc"), "accept "+sid)
	io.WriteString(c.in, "hi\n")
	waitFor(t, 2*time.Second, "taken from the new follow", func() bool { return strings.HasSuffix(a.out.String(), "hi\n") })
	c.in.Close()
	a.in.Close()
	nf.cancel()
}

// A follow whose session ends (the client dropped) hands back its untaken offers too: the reconnecting
// follow announces them.
func TestFollowDropHandsBackOffers(t *testing.T) {
	r := newLiveRig(t)
	x, hub, grp := r.x, r.hub, r.grp
	old := x.start(x.adm(hub), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the follow", x.followed(hub))
	c := x.start(x.adm(grp), "chat "+hub)
	waitFor(t, 2*time.Second, "the offer", func() bool { return liveRE.MatchString(old.out.String()) })
	old.cancel()
	old.wait(t, 2*time.Second)
	nf := x.start(x.adm(hub), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the offer again", func() bool { return liveRE.MatchString(nf.out.String()) })
	c.in.Close()
	nf.cancel()
}
