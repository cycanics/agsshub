package hub

// Live sends: offered on the recipient's follow stream, taken by
// accept, the reply on the sender's stdout, nothing spooled, every exit code.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type liveRig struct {
	x        *th
	t        *testing.T
	hub, grp string
	lvf      *bg // the recipient's follow
	mu       sync.Mutex
	mode     string
	reqs     map[string]*syncBuf // sid -> what its accept session printed
	errs     map[string]*syncBuf
	codes    map[string]int
	stop     chan struct{}
	wg       sync.WaitGroup
}

var liveRE = regexp.MustCompile(`(?m)^LIVE ([0-9a-f]{16}) (\S+) (\S+) (raw|chunked)( chat)?$`)

func newLiveRig(t *testing.T) *liveRig {
	x := newTH(t)
	x.reg("ws-a", "register run.t+alice@acme.ssh --ttl 600", x.key("alice").line)
	x.mustMint("commhub@home.ssh", "hubk")
	x.mustMint("group.demo@expa.ssh", "grpk")
	os.WriteFile(filepath.Join(x.state, "budgets", "group"), []byte("subject=group\nrate=50\nburst=500\n"), 0o640)
	return &liveRig{x: x, t: t, hub: "commhub@home.ssh", grp: "group.demo@expa.ssh", mode: "ok",
		reqs: map[string]*syncBuf{}, errs: map[string]*syncBuf{}, codes: map[string]int{}}
}

func (r *liveRig) setMode(m string) { r.mu.Lock(); r.mode = m; r.mu.Unlock() }

// reply: the recipient's reply in the current mode; raw for a raw kind.
func (r *liveRig) reply(mid string, raw ...bool) (string, bool) {
	r.mu.Lock()
	m := r.mode
	r.mu.Unlock()
	if len(raw) > 0 && raw[0] {
		switch m {
		case "ok":
			return "Status: 200\r\ncontent-type: text/plain\r\n\r\ngot " + mid, true
		case "404":
			return "Status: 404\n\n", true
		case "none":
			return "", true
		}
	}
	switch m {
	case "ok":
		return "Status: 200\r\ncontent-type: text/plain\r\n\r\n" + string(frame([]byte("got "+mid), 3, "")), true
	case "404":
		return "Status: 404\n\n0\r\n\r\n", true
	case "none":
		return "", true
	case "bad":
		return "Status: 200\nX-Evil: 1\n\n0\r\n\r\n", true
	case "cut":
		return "Status: 200\n\n5\r\nhello\r\n", true
	case "abort":
		return "Status: 500\n\n2\r\nhe\r\n0\r\nAbort: gone\r\n\r\n", true
	case "hold":
		time.Sleep(3 * time.Second)
		return "", true
	}
	return "", false // ignore
}

// follow starts the recipient's follow and a responder that accepts every LIVE line once.
func (r *liveRig) follow() {
	r.lvf = r.x.start(r.x.adm(r.hub), "recv --follow --peek", "")
	waitFor(r.t, 2*time.Second, "the recipient's follow", r.x.followed(r.hub))
	r.stop = make(chan struct{})
	seen := map[string]bool{}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			select {
			case <-r.stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			for _, m := range liveRE.FindAllStringSubmatch(r.lvf.out.String(), -1) {
				sid, mid, raw := m[1], m[3], m[4] == "raw"
				if seen[sid] {
					continue
				}
				seen[sid] = true
				r.wg.Add(1)
				r.mu.Lock()
				ignore := r.mode == "ignore"
				r.mu.Unlock()
				if ignore {
					r.wg.Done()
					continue
				}
				go func() { // lv_reply "$sid" "$mid" | accept "$sid": the accept starts at once
					defer r.wg.Done()
					pr, pw := io.Pipe()
					go func() {
						reply, _ := r.reply(mid, raw)
						io.WriteString(pw, reply)
						pw.Close()
					}()
					o, e := &syncBuf{}, &syncBuf{}
					r.mu.Lock()
					r.reqs[sid], r.errs[sid] = o, e
					r.mu.Unlock()
					c := r.x.h.Dispatch(context.Background(), r.x.adm(r.hub), "accept "+sid, IO{Stdin: pr, Stdout: o, Stderr: e})
					pr.Close()
					r.mu.Lock()
					r.codes[sid] = c
					r.mu.Unlock()
				}()
			}
		}
	}()
}

func (r *liveRig) close() { close(r.stop); r.wg.Wait(); r.lvf.cancel() }

func (r *liveRig) sid(mid string) string {
	for _, m := range liveRE.FindAllStringSubmatch(r.lvf.out.String(), -1) {
		if m[3] == mid {
			return m[1]
		}
	}
	return ""
}

func (r *liveRig) acceptCode(sid string) int {
	waitFor(r.t, 3*time.Second, "accept "+sid, func() bool { r.mu.Lock(); defer r.mu.Unlock(); _, ok := r.codes[sid]; return ok })
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.codes[sid]
}

func (r *liveRig) req(sid string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reqs[sid].String()
}

// body: what follows the hub header block.
func body(s string) string { _, b, _ := strings.Cut(s, "\n\n"); return b }

func TestLiveSends(t *testing.T) {
	r := newLiveRig(t)
	x, hub, grp := r.x, r.hub, r.grp
	lvTxt := []byte("hello live\r\nline 2\n")
	framed := string(frame(lvTxt, 4, ""))
	res := x.dis(grp, "send "+hub+" --live --chunked --id L0 -m", framed)
	code(t, "nobody follows the recipient -> 6", res, 6)
	has(t, "and says it is not live", res.err, "not live")
	r.follow()
	defer r.close()
	res = x.dis(grp, "send "+hub+" --live --chunked --id L1 -m", framed)
	code(t, "a live send to a follower that accepts -> 0", res, 0)
	eq(t, "the sender gets the reply, header block normalised, body decoded", res.out, "Status: 200\nContent-Type: text/plain\n\ngot L1")
	sid := r.sid("L1")
	has(t, "the follow stream announced it, its framing token last", r.lvf.out.String(), "LIVE "+sid+" "+grp+" L1 chunked\n")
	eq(t, "the accept session exits 0", r.acceptCode(sid), 0)
	lines := strings.Split(r.req(sid), "\n")
	eq(t, "the recipient reads the hub header block", strings.Join([]string{lines[0], lines[2], lines[3], lines[4]}, "|"),
		"From: "+grp+"|Msgid: L1|Transfer-Encoding: chunked|")
	has(t, "with a Date", lines[1], "Date: 20")
	eq(t, "then the framed body, untouched", body(r.req(sid)), framed)
	eq(t, "nothing is spooled", x.dis(hub, "recv --peek").out, empty)

	eml := string(frame([]byte("Subject: out.deliver\nRe: x-1\n\n{\"a\":1}\n"), 9, ""))
	res = x.dis(grp, "send "+hub+" --live --chunked --id L2 -m:rfc5322", eml)
	code(t, "--id live --peek:rfc5322", res, 0)
	sid = r.sid("L2")
	r.acceptCode(sid)
	eq(t, "D21: the client fields are in the hub header block", dateless(strings.SplitN(r.req(sid), "\n\n", 2)[0]), "From: "+grp+"\nDate: D\nMsgid: L2\nSubject: out.deliver\nRe: x-1\nTransfer-Encoding: chunked")
	dec := &bytes.Buffer{}
	code(t, "the body framed", result{code: func() int {
		c, _ := dechunk(bufio.NewReader(strings.NewReader(body(r.req(sid)))), dec, 1<<20)
		return c
	}()}, 0)
	eq(t, "and only the body after the blank line", dec.String(), "{\"a\":1}\n")
	code(t, "a field outside the allow-list -> 2", x.dis(grp, "send "+hub+" --live --chunked --id L3 -m:rfc5322", string(frame([]byte("X-Evil: 1\n\n{}\n"), 64, ""))), 2)
	eq(t, "and is never offered", r.sid("L3"), "")
	code(t, "a header block without its blank line -> 2", x.dis(grp, "send "+hub+" --live --chunked --id L3b -m:rfc5322", string(frame([]byte("Subject: x\nno blank line\n"), 64, ""))), 2)
	code(t, "a header block over 4096 bytes -> 2", x.dis(grp, "send "+hub+" --live --chunked --id L3c -m:rfc5322", string(frame([]byte("Subject: "+strings.Repeat("y", 5000)+"\n\nb"), 100, ""))), 2)
	bin := make([]byte, 70000)
	rand.Read(bin)
	bframed := string(frame(bin, 30000, ""))
	res = x.dis(grp, "send "+hub+" --live --chunked --id L4 --file blob.bin", bframed)
	code(t, "--file live", res, 0)
	sid = r.sid("L4")
	r.acceptCode(sid)
	eq(t, "Payload: file names it", strings.Join(strings.Split(r.req(sid), "\n")[3:5], "|"), "Payload: file blob.bin|Transfer-Encoding: chunked")
	eq(t, "binary chunks arrive untouched", sha256.Sum256([]byte(body(r.req(sid)))), sha256.Sum256([]byte(bframed)))

	code(t, "--live with an inline message -> 2", x.dis(grp, "send "+hub+" --live -- inline"), 2)
	code(t, "--live without a form -> 2", x.dis(grp, "send "+hub+" --live --chunked --id L5", framed), 2)
	os.MkdirAll(filepath.Join(x.state, "topics", "topic+builds@acme.ssh"), 0o2770)
	os.WriteFile(filepath.Join(x.state, "topics", "topic+builds@acme.ssh", "policy"), []byte("topic=topic+builds@acme.ssh\npublish=*\n"), 0o640)
	code(t, "--live to a topic -> 9", x.dis(alice, "send topic+builds@acme.ssh --live -m", framed), 9)
	os.WriteFile(filepath.Join(x.state, "aliases", "carol"), []byte("type=email\naddr=c@x\n"), 0o640)
	code(t, "--live to an alias -> 9", x.dis(grp, "send carol --live -m", framed), 9)
	code(t, "--live to an unknown identity -> 3", x.dis(grp, "send nobody@expa.ssh --live -m", framed), 3)
	code(t, "--live with a bad --id -> 2", x.dis(grp, "send "+hub+" --live --chunked --id ../x -m", framed), 2)

	r.setMode("404")
	res = x.dis(grp, "send "+hub+" --live --chunked --id L6 -m", framed)
	code(t, "a rejection is still a reply -> 0", res, 0)
	eq(t, "with its Status", res.out, "Status: 404\n\n")
	r.setMode("none")
	res = x.dis(grp, "send "+hub+" --live --chunked --id L7 -m", framed)
	code(t, "the recipient closes without a reply -> 7", res, 7)
	eq(t, "and the sender's stdout stays empty", res.out, "")
	eq(t, "its accept session exits 7", r.acceptCode(r.sid("L7")), 7)
	r.setMode("bad")
	res = x.dis(grp, "send "+hub+" --live --chunked --id L8 -m", framed)
	code(t, "a malformed reply -> 7", res, 7)
	eq(t, "nothing of it reaches the sender", res.out, "")
	sid = r.sid("L8")
	eq(t, "the recipient learns its reply was refused (accept exits 2)", r.acceptCode(sid), 2)
	has(t, "and why", r.errs[sid].String(), "X-Evil")
	r.setMode("cut")
	res = x.dis(grp, "send "+hub+" --live --chunked --id L9 -m", framed)
	code(t, "a reply cut before its last chunk -> 4", res, 4)
	eq(t, "with the partial reply on stdout", res.out, "Status: 200\n\nhello")
	r.setMode("abort")
	code(t, "a reply aborted in its trailer -> 4", x.dis(grp, "send "+hub+" --live --chunked --id L10 -m", framed), 4)
	eq(t, "its accept learns it: 4", r.acceptCode(r.sid("L10")), 4)

	r.setMode("ok")
	cut := framed[:len(framed)-5]
	res = x.dis(grp, "send "+hub+" --live --chunked --id L11 -m", cut)
	code(t, "a request cut before its last chunk -> 4", res, 4)
	eq(t, "no reply reaches that sender", res.out, "")
	sid = r.sid("L11")
	eq(t, "its accept session exits 4 (discard it)", r.acceptCode(sid), 4)
	eq(t, "having seen the partial framing as it was sent", body(r.req(sid)), cut)
	res = x.dis(grp, "send "+hub+" --live --chunked --id L12 -m", "5\r\nhello\r\n0\r\nAbort: changed my mind\r\n\r\n")
	code(t, "an Abort trailer -> 4", res, 4)
	sid = r.sid("L12")
	r.acceptCode(sid)
	has(t, "which the recipient sees in the framing (the request's tail before the verdict)", r.req(sid), "Abort: changed my mind")

	r.setMode("ignore")
	os.WriteFile(filepath.Join(x.state, "settings"), []byte("live_accept_secs=0.5\n"), 0o640)
	t0 := time.Now()
	res = x.dis(grp, "send "+hub+" --live --chunked --id L13 -m", framed)
	ms := time.Since(t0).Milliseconds()
	code(t, "a follower that does not accept within T -> 6", res, 6)
	if ms < 400 || ms >= 3000 {
		t.Errorf("after T = live_accept_secs 0.5: %d ms", ms)
	}
	eq(t, "saying so", res.err, "dispatch: "+hub+" did not accept within 0.5 s; nothing delivered\n")
	code(t, "accepting it late -> 3", x.dis(hub, "accept "+r.sid("L13")), 3)
	os.Remove(filepath.Join(x.state, "settings"))

	// only the addressed identity may accept, once
	var l14 result
	l14done := make(chan struct{})
	go func() { l14 = x.dis(grp, "send "+hub+" --live --chunked --id L14 -m", framed); close(l14done) }()
	waitFor(t, 3*time.Second, "L14 offered", func() bool { return r.sid("L14") != "" })
	sid = r.sid("L14")
	code(t, "only the addressed identity may accept -> 3", x.dis(alice, "accept "+sid), 3)
	reply, _ := func() (string, bool) { r.setMode("ok"); return r.reply("L14") }()
	code(t, "the addressed identity accepts", x.dis(hub, "accept "+sid, reply), 0)
	code(t, "a second accept of one exchange -> 3", x.dis(hub, "accept "+sid), 3)
	<-l14done
	code(t, "and the sender got its reply", l14, 0)
	code(t, "accept of a malformed id -> 2", x.dis(hub, "accept nope"), 2)
	code(t, "accept of an unknown exchange -> 3", x.dis(hub, "accept 0123456789abcdef"), 3)
	code(t, "accept with two args -> 2", x.dis(hub, "accept 0123456789abcdef x"), 2)

	// 12 at once to one recipient
	var wg sync.WaitGroup
	results := make([]result, 12)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = x.dis(grp, fmt.Sprintf("send %s --live --chunked --id P%d -m", hub, i+1), framed)
		}()
	}
	wg.Wait()
	ok := 0
	for i, rr := range results {
		if rr.code == 0 && strings.HasSuffix(rr.out, fmt.Sprintf("got P%d", i+1)) {
			ok++
		}
	}
	eq(t, "12 concurrent live sends to one recipient: each gets its own reply", ok, 12)
	eq(t, "over 12 accept sessions", len(regexp.MustCompile(`(?m)^LIVE .* P[0-9]+ chunked$`).FindAllString(r.lvf.out.String(), -1)), 12)
	x.dis(alice, "send "+hub+" --id spooled-1 -- beside the live ones")
	waitFor(t, 3*time.Second, "the spooled one", func() bool { return strings.Contains(r.lvf.out.String(), "beside the live ones") })
	lives := 0
	for _, c := range chunks(r.lvf.out.String()) {
		switch {
		case c.part == "BAD":
			t.Errorf("the follow stream: %s", c.data)
		case c.part == "live":
			lives++
			if m := liveRE.FindStringSubmatch(c.data); m == nil || c.data != m[0]+"\n" || m[3] != c.id {
				t.Errorf("a part=live chunk is one LIVE line, id its msgid: %q %q", c.id, c.data)
			}
		case strings.Contains(c.data, "\nLIVE "):
			t.Error("a LIVE line inside a message chunk")
		}
	}
	eq(t, "every LIVE line a part=live chunk", lives, len(liveRE.FindAllString(r.lvf.out.String(), -1)))

	os.WriteFile(filepath.Join(x.state, "budgets", grp), []byte("subject="+grp+"\nburst=1\n"), 0o640)
	os.RemoveAll(filepath.Join(x.state, "flood"))
	x.dis(grp, "send "+hub+" --live --chunked --id L15 -m", framed)
	code(t, "the flood budget applies to live sends -> 5", x.dis(grp, "send "+hub+" --live --chunked --id L16 -m", framed), 5)
	os.Remove(filepath.Join(x.state, "budgets", grp))
	os.RemoveAll(filepath.Join(x.state, "flood"))

	// a sender whose session ends while it waits: its accept session exits 7 at once
	r.setMode("hold")
	snd := x.start(x.adm(grp), "send "+hub+" --live --chunked --id L17 -m", framed)
	waitFor(t, 3*time.Second, "L17 taken", func() bool {
		s := r.sid("L17")
		if s == "" {
			return false
		}
		r.x.h.live.mu.Lock()
		defer r.x.h.live.mu.Unlock()
		e := r.x.h.live.m[s]
		return e != nil && e.taken
	})
	sid = r.sid("L17")
	time.Sleep(100 * time.Millisecond)
	t0 = time.Now()
	snd.cancel() // the sender's connection is gone
	eq(t, "a sender whose session ends while it waits: its accept session exits 7", r.acceptCode(sid), 7)
	if d := time.Since(t0); d > time.Second {
		t.Errorf("noticed after %v (at once)", d)
	}
	r.setMode("ok")
	waitFor(t, 3*time.Second, "every exchange closed", func() bool { return x.h.live.count() == 0 })
	x.dis(hub, "recv_ack spooled-1")
	_ = bytes.Equal
}

// A raw send --live: the request is stdin's bytes to EOF, the reply the recipient's to its EOF; nothing framed.
func TestLiveRaw(t *testing.T) {
	r := newLiveRig(t)
	x, hub, grp := r.x, r.hub, r.grp
	r.follow()
	defer r.close()
	res := x.dis(grp, "send "+hub+" --live --id R1 -m", "plain bytes\r\n0\r\n\r\n")
	code(t, "raw -m", res, 0)
	eq(t, "the reply: its header block, then the bytes", res.out, "Status: 200\nContent-Type: text/plain\n\ngot R1")
	sid := r.sid("R1")
	has(t, "LIVE … raw", r.lvf.out.String(), "LIVE "+sid+" "+grp+" R1 raw\n")
	r.acceptCode(sid)
	eq(t, "no Transfer-Encoding in a raw kind", dateless(r.req(sid)), "From: "+grp+"\nDate: D\nMsgid: R1\n\nplain bytes\r\n0\r\n\r\n")
	res = x.dis(grp, "send "+hub+" --live --id R2 -m:rfc5322", "Subject: out.x\nReply-To: a@b.ssh\n\nthe body\n")
	code(t, "raw -m:rfc5322", res, 0)
	sid = r.sid("R2")
	r.acceptCode(sid)
	eq(t, "D21: the client fields in the hub header block, the body alone after it", dateless(r.req(sid)), "From: "+grp+"\nDate: D\nMsgid: R2\nSubject: out.x\nReply-To: a@b.ssh\n\nthe body\n")
	code(t, "a refused field before any offer -> 2", x.dis(grp, "send "+hub+" --live --id R3 -m:rfc5322", "X-Evil: 1\n\nb\n"), 2)
	eq(t, "(never offered)", r.sid("R3"), "")
	res = x.dis(grp, "send "+hub+" --live --id R4 --subject v --file f.bin", "\x00\x01bytes")
	code(t, "raw --file", res, 0)
	sid = r.sid("R4")
	r.acceptCode(sid)
	eq(t, "Payload: file, Subject", dateless(r.req(sid)), "From: "+grp+"\nDate: D\nMsgid: R4\nPayload: file f.bin\nSubject: v\n\n\x00\x01bytes")
	r.setMode("404")
	eq(t, "a raw rejection", x.dis(grp, "send "+hub+" --live -m", "x").out, "Status: 404\n\n")
	r.setMode("none")
	code(t, "no reply at all -> 7", x.dis(grp, "send "+hub+" --live -m", "x"), 7)
	r.setMode("ok")
	big := strings.Repeat("z", FileMaxBytes+1)
	res = x.dis(grp, "send "+hub+" --live --id R5 -m", big)
	code(t, "a request past FileMaxBytes -> 2", res, 2)
	has(t, "(says so)", res.err, "body exceeds cap")
	eq(t, "its accept discards it: 4", r.acceptCode(r.sid("R5")), 4)
	for _, c := range []struct{ what, cmd string }{
		{"--size", "send " + hub + " --live --size 1 -m"},
		{"an inline message", "send " + hub + " --live -- x"},
		{"--id event-*", "send " + hub + " --live --id event-aaaaaaaaaaaaaaaa-1 -m"},
		{"--id head-*", "send " + hub + " --live --id head-x -m"},
	} {
		code(t, c.what+" with --live -> 2", x.dis(grp, c.cmd, "x"), 2)
	}
}

// The reply has no size cap; a recipient that goes away mid-reply is a drop: 7.
func TestLiveRawReplyUncapped(t *testing.T) {
	x := newLiveRig(t).x
	hub, grp := "commhub@home.ssh", "group.demo@expa.ssh"
	f := x.start(x.adm(hub), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the follow", x.followed(hub))
	big := strings.Repeat("r", FileMaxBytes+4096)
	snd := x.start(x.adm(grp), "send "+hub+" --live --id U1 -m", "q")
	waitFor(t, 2*time.Second, "the offer", func() bool { return liveRE.MatchString(f.out.String()) })
	sid := liveRE.FindStringSubmatch(f.out.String())[1]
	a := x.start(x.adm(hub), "accept "+sid, "Status: 200\n\n"+big)
	eq(t, "the sender: exit 0", snd.wait(t, 20*time.Second), 0)
	eq(t, "the whole reply past FileMaxBytes", len(snd.out.String()), len("Status: 200\n\n")+len(big))
	eq(t, "accept 0", a.wait(t, 3*time.Second), 0)
	// a drop: the accept session ends mid-reply
	snd = x.start(x.adm(grp), "send "+hub+" --live --id U2 -m", "q")
	waitFor(t, 2*time.Second, "the second offer", func() bool { return len(liveRE.FindAllStringSubmatch(f.out.String(), -1)) == 2 })
	sid = liveRE.FindAllStringSubmatch(f.out.String(), -1)[1][1]
	a = x.start(x.adm(hub), "accept "+sid)
	waitFor(t, 2*time.Second, "the request", func() bool { return strings.Contains(a.out.String(), "\n\nq") })
	io.WriteString(a.in, "Status: 200\n\npartial")
	time.Sleep(50 * time.Millisecond)
	a.cancel()
	eq(t, "the recipient dropped: the sender exits 7", snd.wait(t, 3*time.Second), 7)
	f.cancel()
}

// chat: open-ended streams both ways; the back stream starts with a Status block.
func TestChat(t *testing.T) {
	r := newLiveRig(t)
	x, hub, grp := r.x, r.hub, r.grp
	code(t, "nobody follows: 6", x.dis(grp, "chat "+hub), 6)
	f := x.start(x.adm(hub), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the follow", x.followed(hub))
	offer := func(n int) (string, string) {
		waitFor(t, 2*time.Second, "an offer", func() bool { return len(liveRE.FindAllStringSubmatch(f.out.String(), -1)) >= n })
		m := liveRE.FindAllStringSubmatch(f.out.String(), -1)[n-1]
		return m[1], m[0]
	}
	// raw: both ways, each to its EOF
	c := x.start(x.adm(grp), "chat "+hub)
	sid, line := offer(1)
	eq(t, "LIVE … raw chat", strings.HasSuffix(line, " raw chat") && strings.Contains(line, " "+grp+" "), true)
	a := x.start(x.adm(hub), "accept "+sid)
	io.WriteString(c.in, "hello A\n")
	waitFor(t, 2*time.Second, "S's bytes at A", func() bool { return strings.HasSuffix(a.out.String(), "\n\nhello A\n") })
	has(t, "the accept header: Subject: chat, no framing", dateless(a.out.String()), "From: "+grp+"\nDate: D\nMsgid: ")
	has(t, "(Subject)", a.out.String(), "\nSubject: chat\n\n")
	lacks(t, "(no Transfer-Encoding)", a.out.String(), "Transfer-Encoding")
	io.WriteString(a.in, "Status: 200\n\nhello S\n")
	waitFor(t, 2*time.Second, "A's bytes at S", func() bool { return c.out.String() == "Status: 200\n\nhello S\n" })
	io.WriteString(c.in, "more\n")
	waitFor(t, 2*time.Second, "S again", func() bool { return strings.HasSuffix(a.out.String(), "more\n") })
	c.in.Close()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-c.done:
		t.Error("the session ended with one side still open")
	default:
	}
	io.WriteString(a.in, "bye\n")
	a.in.Close()
	eq(t, "both closed: S exits 0", c.wait(t, 3*time.Second), 0)
	eq(t, "S got all of it", c.out.String(), "Status: 200\n\nhello S\nbye\n")
	eq(t, "A exits 0", a.wait(t, 3*time.Second), 0)
	// a refusal: any other Status ends it at once, S exit 0 with the Status
	c = x.start(x.adm(grp), "chat "+hub)
	sid, _ = offer(2)
	a = x.start(x.adm(hub), "accept "+sid, "Status: 403\n\n")
	eq(t, "refused: S exits 0", c.wait(t, 3*time.Second), 0)
	eq(t, "with the Status on stdout", c.out.String(), "Status: 403\n\n")
	// chunked: framed both ways, forwarded framed; a missing final chunk is 4
	c = x.start(x.adm(grp), "chat "+hub+" --chunked")
	sid, line = offer(3)
	eq(t, "LIVE … chunked chat", strings.HasSuffix(line, " chunked chat"), true)
	a = x.start(x.adm(hub), "accept "+sid)
	io.WriteString(c.in, string(frame([]byte("framed hi"), 4, "")))
	waitFor(t, 2*time.Second, "framed at A", func() bool { return strings.HasSuffix(a.out.String(), "0\r\n\r\n") })
	has(t, "Transfer-Encoding: chunked", a.out.String(), "Transfer-Encoding: chunked\n\n")
	io.WriteString(a.in, "Status: 200\n\n"+string(frame([]byte("framed back"), 5, "")))
	c.in.Close()
	a.in.Close()
	eq(t, "chunked chat: 0", c.wait(t, 3*time.Second), 0)
	eq(t, "S gets the back stream framed", c.out.String(), "Status: 200\n\n"+string(frame([]byte("framed back"), 5, "")))
	c = x.start(x.adm(grp), "chat "+hub+" --chunked")
	sid, _ = offer(4)
	a = x.start(x.adm(hub), "accept "+sid)
	io.WriteString(c.in, "3\r\nabc\r\n") // no final chunk
	c.in.Close()
	io.WriteString(a.in, "Status: 200\n\n"+string(frame([]byte("ok"), 5, "")))
	a.in.Close()
	eq(t, "a direction without its final chunk: 4", c.wait(t, 3*time.Second), 4)
	eq(t, "A learns it: 4", a.wait(t, 3*time.Second), 4)
	// a drop
	c = x.start(x.adm(grp), "chat "+hub)
	sid, _ = offer(5)
	a = x.start(x.adm(hub), "accept "+sid)
	io.WriteString(a.in, "Status: 200\n\nx")
	waitFor(t, 2*time.Second, "the back stream", func() bool { return strings.HasSuffix(c.out.String(), "x") })
	a.cancel()
	eq(t, "A dropped: S exits 7", c.wait(t, 3*time.Second), 7)
	f.cancel()
	// refusals
	x.mkTopic("topic+c@acme.ssh", alice)
	for _, cc := range []struct {
		what, cmd string
		want      int
	}{
		{"no peer", "chat", 2},
		{"--id", "chat " + hub + " --id x", 2},
		{"--subject", "chat " + hub + " --subject v", 2},
		{"--size", "chat " + hub + " --size 1", 2},
		{"--durable", "chat " + hub + " --durable", 2},
		{"--wait", "chat " + hub + " --wait 1", 2},
		{"a bad peer", "chat Bad@x", 2},
		{"a topic", "chat topic+c@acme.ssh", 9},
		{"an unknown peer", "chat group.nobody@expa.ssh", 3},
	} {
		code(t, "chat: "+cc.what, x.dis(grp, cc.cmd), cc.want)
	}
	x.mustMint("publisher+p@acme.ssh", "pubk")
	code(t, "a publisher as sender: 9", x.dis("publisher+p@acme.ssh", "chat "+hub), 9)
	code(t, "who is gone", x.dis(grp, "who"), 2)
}

// eofIO: a session's channel whose stdout EOF is recorded (what CloseWrite does on an SSH channel).
type eofIO struct {
	in  *io.PipeWriter
	out *syncBuf
	eof chan struct{}
	io  IO
}

func newEOFIO() *eofIO {
	pr, pw := io.Pipe()
	e := &eofIO{in: pw, out: &syncBuf{}, eof: make(chan struct{})}
	var once sync.Once
	e.io = IO{Stdin: pr, Stdout: e.out, Stderr: io.Discard, CloseOut: func() error { once.Do(func() { close(e.eof) }); return nil }}
	return e
}

func (e *eofIO) closed() bool {
	select {
	case <-e.eof:
		return true
	default:
		return false
	}
}

// D24: in a raw kind, the sender's stdin EOF closes the accept session's stdout, and the recipient's stdin EOF
// closes the sender's stdout, while the other direction may still be open.
func TestRawEOFPropagates(t *testing.T) {
	x := newLiveRig(t).x
	hub, grp := "commhub@home.ssh", "group.demo@expa.ssh"
	f := x.start(x.adm(hub), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the follow", x.followed(hub))
	sid := func(n int) string {
		waitFor(t, 2*time.Second, "an offer", func() bool { return len(liveRE.FindAllStringSubmatch(f.out.String(), -1)) >= n })
		return liveRE.FindAllStringSubmatch(f.out.String(), -1)[n-1][1]
	}
	for i, kind := range []string{"send " + hub + " --live -m", "chat " + hub} {
		S, A := newEOFIO(), newEOFIO()
		sDone, aDone := make(chan int, 1), make(chan int, 1)
		go func() { sDone <- x.h.Dispatch(context.Background(), x.adm(grp), kind, S.io) }()
		id := sid(i + 1)
		go func() { aDone <- x.h.Dispatch(context.Background(), x.adm(hub), "accept "+id, A.io) }()
		io.WriteString(S.in, "ping\n")
		waitFor(t, 2*time.Second, kind+": the request at A", func() bool { return strings.HasSuffix(A.out.String(), "ping\n") })
		eq(t, kind+": A's stdout open while S's stdin is", A.closed(), false)
		S.in.Close()
		waitFor(t, 2*time.Second, kind+": S's EOF closes A's stdout", A.closed)
		io.WriteString(A.in, "Status: 200\n\npong\n")
		time.Sleep(50 * time.Millisecond)
		eq(t, kind+": S's stdout open while A's stdin is", S.closed(), false)
		A.in.Close()
		eq(t, kind+": S exits 0", <-sDone, 0)
		eq(t, kind+": with the reply", S.out.String(), "Status: 200\n\npong\n")
		eq(t, kind+": A exits 0", <-aDone, 0)
	}
	// chat: A's EOF closes S's stdout while S's stdin is still open
	S, A := newEOFIO(), newEOFIO()
	sDone := make(chan int, 1)
	go func() { sDone <- x.h.Dispatch(context.Background(), x.adm(grp), "chat "+hub, S.io) }()
	id := sid(3)
	go x.h.Dispatch(context.Background(), x.adm(hub), "accept "+id, A.io)
	io.WriteString(A.in, "Status: 200\n\nbye\n")
	A.in.Close()
	waitFor(t, 2*time.Second, "chat: A's EOF closes S's stdout, S's stdin still open", S.closed)
	eq(t, "S got the back stream", S.out.String(), "Status: 200\n\nbye\n")
	eq(t, "A's stdout still open: S has not closed", A.closed(), false)
	S.in.Close()
	eq(t, "then S's EOF ends it: 0", <-sDone, 0)
	f.cancel()
}

// D21 with a chunk that holds the header block and a long body: the body after the block reaches the
// recipient whole, framed.
func TestLiveRFCOneBigChunk(t *testing.T) {
	r := newLiveRig(t)
	x, hub, grp := r.x, r.hub, r.grp
	r.follow()
	defer r.close()
	body := strings.Repeat("b", 64*1024)
	res := x.dis(grp, "send "+hub+" --live --chunked --id B1 -m:rfc5322", string(frame([]byte("Subject: s\n\n"+body), 1<<20, "")))
	code(t, "one 64 KiB chunk", res, 0)
	sid := r.sid("B1")
	r.acceptCode(sid)
	dec := &bytes.Buffer{}
	c, _ := dechunk(bufio.NewReader(strings.NewReader(body2(r.req(sid)))), dec, 1<<21)
	eq(t, "framed whole", c, 0)
	eq(t, "the body whole", dec.Len(), len(body))
}

// body2: what follows the hub header block (the first blank line).
func body2(s string) string { _, b, _ := strings.Cut(s, "\n\n"); return b }
