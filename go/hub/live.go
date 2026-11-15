package hub

// Live exchanges (docs/live-send.md): `send --live` and `chat`. The request goes from the sender's stdin to
// the accept session's stdout, the reply (or chat's back stream) from the accept session's stdin to the
// sender's stdout. The sender's goroutine drives both directions over the streams the accept session hands
// it; the accept session waits for the verdict. An exchange is an entry in a table. Without --chunked both
// directions are raw and the hub copies bytes; with it the caller frames both and the hub checks them.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cycanics/agsshub/go/descriptor"
)

type exchangeTable struct {
	mu sync.Mutex
	m  map[string]*exchange
}

func newExchangeTable() *exchangeTable { return &exchangeTable{m: map[string]*exchange{}} }

type exchange struct {
	sid, to, from, msgid string
	chunked, chat        bool
	opened               time.Time

	taken     bool           // under the table's mutex: accepted, or expired
	announced bool           // under the table's mutex: a follow printed its LIVE line
	by        *follower      // under the table's mutex: the follow that announced it
	accepted  chan *acceptor // the accept session's streams (cap 1)
	done      chan struct{}  // closed with the verdict
	once      sync.Once
	verdict   int
	why       string
}

// line: the exchange's LIVE line.
func (x *exchange) line() string {
	kind := "raw"
	if x.chunked {
		kind = "chunked"
	}
	if x.chat {
		kind += " chat"
	}
	return fmt.Sprintf("LIVE %s %s %s %s", x.sid, x.from, x.msgid, kind)
}

// acceptor: what an accept session hands the sender's goroutine.
type acceptor struct {
	in       *bufio.Reader // the reply, or chat's back stream
	out      io.Writer     // the request
	closeOut func()        // the request's end: EOF on the accept session's stdout
	gone     func() bool   // the accept session is gone (an EOF from it was a drop, not its end)
}

func (t *exchangeTable) open(to, from, msgid string, chunked, chat bool) *exchange {
	x := &exchange{to: to, from: from, msgid: msgid, chunked: chunked, chat: chat, opened: time.Now(),
		accepted: make(chan *acceptor, 1), done: make(chan struct{})}
	t.mu.Lock()
	for {
		x.sid = randHex(8)
		if t.m[x.sid] == nil {
			break
		}
	}
	t.m[x.sid] = x
	t.mu.Unlock()
	return x
}

// close ends an exchange whoever decided: its verdict, if none yet, is that it broke.
func (t *exchangeTable) close(x *exchange) {
	x.finish(7, "the sender went away")
	t.mu.Lock()
	delete(t.m, x.sid)
	x.taken = true
	t.mu.Unlock()
}

func (t *exchangeTable) get(sid string) *exchange {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[sid]
}

// take claims an exchange once: by its accept, or by its sender at the deadline.
func (t *exchangeTable) take(x *exchange) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if x.taken {
		return false
	}
	x.taken = true
	return true
}

// announce: the exchanges to name that are open, not taken and not announced yet, oldest first, each now
// marked announced: its LIVE line is printed once, on whichever follow takes it.
func (t *exchangeTable) announce(name string, by *follower) []*exchange {
	t.mu.Lock()
	var out []*exchange
	for _, x := range t.m {
		if x.to == name && !x.taken && !x.announced {
			x.announced, x.by = true, by
			out = append(out, x)
		}
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].opened.Before(out[j].opened) })
	return out
}

// unannounce: the open exchanges follow f announced and nobody took are no longer announced, so the next
// follow of their mailbox names them again (f's reader is gone or stops without taking them); how many.
func (t *exchangeTable) unannounce(f *follower) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, x := range t.m {
		if x.by == f && x.announced && !x.taken {
			x.announced, x.by = false, nil
			n++
		}
	}
	return n
}

// count: exchanges open now (for tests and the operator).
func (t *exchangeTable) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

func (x *exchange) finish(code int, why string) {
	x.once.Do(func() {
		x.verdict, x.why = code, why
		close(x.done)
	})
}

// waitAccept: the accept session's streams, or nil once the deadline passed (the exchange is then
// taken, so a late accept is refused) or ctx ended.
func (h *Hub) waitAccept(ctx context.Context, x *exchange, deadline time.Time) *acceptor {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case a := <-x.accepted:
		return a
	case <-t.C:
	case <-ctx.Done():
	}
	if h.live.take(x) {
		return nil
	}
	return <-x.accepted // taken by an accept a moment ago: it hands over at once
}

// offer is what every live kind does before its streams flow: the target judged (9, 3), an instance's flows
// (C2, C), a follower and a flood token (6, 5), the exchange opened and accepted (6). pre runs once the
// target is known to follow, before the offer (the request's client header block is checked there).
func (s *Session) offer(target, msgid string, chunked, chat bool, pre func()) (*exchange, *acceptor) {
	h := s.h
	h.liveTarget(s, target) // exits 9 or 3 for one live is not for
	took := false
	if isInstance(target) && !h.follows.following(target) { // flows C2, C
		if !h.floodTake(s.me, target) {
			s.die(5, "dispatch: rate limited (%s -> %s: %s); slow down", s.me, target, h.budgetShow(s.me))
		}
		took = true
		s.admitSendOrExit(target, false) // no answer, its mailbox there: flow C all the same
		if !h.follows.following(target) {
			till := h.transientTouch(target, s.me)
			parent, _, _ := instSplit(target)
			s.retryAfter("dispatch: %s is not live: its address %s is asked to start it (stay-up-till=%s); nothing delivered",
				target, parent, isoTime(till.Unix()))
		}
	}
	if !h.follows.following(target) {
		s.die(6, "dispatch: %s is not live (nothing follows its mailbox)", target)
	}
	if !took && !h.floodTake(s.me, target) {
		s.die(5, "dispatch: rate limited (%s -> %s: %s); slow down", s.me, target, h.budgetShow(s.me))
	}
	if pre != nil {
		pre()
	}
	x := h.live.open(target, s.me, msgid, chunked, chat)
	h.follows.nudge(target) // its follow prints the LIVE line from the table
	if !h.follows.following(target) {
		h.live.close(x)
		s.die(6, "dispatch: %s is not live", target)
	}
	tStr, tDur := h.liveAcceptSecs()
	acc := h.waitAccept(s.ctx, x, time.Now().Add(tDur))
	if acc == nil {
		h.live.close(x)
		s.die(6, "dispatch: %s did not accept within %s s; nothing delivered", target, tStr)
	}
	return x, acc
}

// liveHeader: the hub header block the accept session's stdout starts with.
func (h *Hub) liveHeader(from, msgid, fname, subject string, fields []byte, chunked bool) string {
	var hb strings.Builder
	fmt.Fprintf(&hb, "From: %s\nDate: %s\nMsgid: %s\n", from, isoTime(h.epoch()), msgid)
	if fname != "" {
		fmt.Fprintf(&hb, "Payload: file %s\n", fname)
	}
	if subject != "" {
		fmt.Fprintf(&hb, "Subject: %s\n", subject)
	}
	hb.Write(fields)
	if chunked {
		hb.WriteString("Transfer-Encoding: chunked\n")
	}
	hb.WriteString("\n")
	return hb.String()
}

// sendLive is `send … --live`: offer stdin on target's follow stream, relay it to the accept session, relay
// the reply back. Exits.
func (s *Session) sendLive(target, mode, fname, msgid, inline, subject string, chunked bool) {
	h := s.h
	if inline != "" {
		s.die(2, "dispatch: --live streams stdin; an inline message has none")
	}
	if fname != "" {
		if !validFile(fname) {
			s.die(2, "dispatch: unsafe filename")
		}
	} else if mode == "" {
		s.die(2, "dispatch: --live needs -m, -m:rfc5322 or --file")
	}
	s.checkSendID(msgid, false)
	if msgid == "" {
		msgid = h.mintMsgid()
	}
	lr := &relay{cap: FileMaxBytes, keepAll: true, errf: func(m string) { s.errf("%s", m) }}
	// -m:rfc5322: the client header block is checked before the offer and goes into the hub header block
	// (D21); the request then carries the body only. pre: the request's bytes that follow it, as read
	var fields []byte
	var pre bytes.Buffer
	preDone := false
	x, acc := s.offer(target, msgid, chunked, false, func() {
		if mode != "rfc" {
			return
		}
		if chunked {
			var framed, dec bytes.Buffer
			rc := lr.run(s.in, &framed, &dec, true)
			switch rc {
			case 0:
				preDone = true
			case 10:
			default:
				s.exit(rc)
			}
			br := bufio.NewReader(bytes.NewReader(dec.Bytes()))
			hdr, code, msg := parseRFC5322(br)
			if code != 0 {
				s.die(code, "%s", msg)
			}
			if why := gapProblem(hdr, "native"); why != "" {
				s.die(2, "dispatch: %s", why)
			}
			fields = hdr
			// the decoded bytes past the header block: one chunk of their own; then, if the stream ended
			// meanwhile, its last chunk and trailer as received
			rest, _ := io.ReadAll(br)
			if len(rest) > 0 {
				fmt.Fprintf(&pre, "%x\r\n", len(rest))
				pre.Write(rest)
				pre.WriteString("\r\n")
			}
			if preDone {
				pre.Write(framed.Bytes()[lr.lastAt:])
			}
			return
		}
		hdr, code, msg := parseRFC5322(s.in) // raw: the block, then the body as it comes
		if code != 0 {
			s.die(code, "%s", msg)
		}
		if why := gapProblem(hdr, "native"); why != "" {
			s.die(2, "dispatch: %s", why)
		}
		fields = hdr
	})
	defer h.live.close(x)
	// the sender's session ending ends the exchange, wherever this goroutine is waiting
	stop := context.AfterFunc(s.ctx, func() { x.finish(7, "the sender went away") })
	defer stop()

	if _, err := io.WriteString(acc.out, h.liveHeader(s.me, msgid, fname, subject, fields, chunked)+pre.String()); err != nil {
		x.finish(7, "the recipient went away")
		s.exit(7)
	}
	rc := 0
	switch {
	case chunked && !preDone:
		rc = lr.run(s.in, acc.out, nil, false)
	case !chunked:
		_, err := io.Copy(acc.out, &capReader{r: s.in, left: FileMaxBytes - int64(len(fields)), what: "body"})
		switch err.(type) {
		case nil:
		case *inErr:
			s.errf("%s", err)
			rc = 2
		default:
			rc = 7
		}
	}
	switch rc {
	case 0:
	case 4:
		x.finish(4, "the request was incomplete or aborted")
		s.exit(4)
	case 7:
		s.errf("dispatch: %s went away", target)
		x.finish(7, "the recipient went away")
		s.exit(7)
	default:
		x.finish(4, "the request was malformed")
		s.exit(rc)
	}
	if !chunked { // a raw request ends at EOF; a chunked one at its last chunk, the accept's stdout kept open
		acc.closeOut()
	}

	head, code, why := replyHead(acc.in)
	if code != 0 {
		s.errf("dispatch: %s", why)
		x.finish(code, "the reply broke: "+why)
		s.exit(7)
	}
	s.flush()
	if _, err := io.WriteString(s.rawOut, head); err != nil {
		x.finish(7, "the sender went away")
		s.exit(7)
	}
	if chunked {
		switch code, why := replyBody(acc.in, s.rawOut); {
		case code == 0:
		case code == 8:
			x.finish(7, "the sender went away")
			s.exit(7)
		case code == 4 && !acc.gone(): // a chunked direction without its last chunk, or with Abort
			s.errf("dispatch: %s", why)
			x.finish(4, why)
			s.exit(4)
		default:
			s.errf("dispatch: %s", why)
			x.finish(7, "the reply broke: "+why)
			s.exit(7)
		}
	} else if code, why := rawCopy(s.rawOut, acc); code != 0 {
		s.errf("dispatch: %s", why)
		x.finish(7, why)
		s.exit(7)
	}
	x.finish(0, "ok")
	s.exit(0)
}

// replyHead reads a reply's (or chat's back stream's) header block and renders it for the sender: 0, or 2
// malformed / 7 cut (why).
func replyHead(r *bufio.Reader) (string, int, string) {
	status, ctype, code, why := replyHeader(r)
	if code != 0 {
		return "", code, why
	}
	head := "Status: " + status + "\n"
	if ctype != "" {
		head += "Content-Type: " + ctype + "\n"
	}
	return head + "\n", 0, ""
}

// rawCopy copies a raw back stream to w until the accept session's EOF: 0, or 7 when the EOF was the session
// going away, or w broke.
func rawCopy(w io.Writer, acc *acceptor) (int, string) {
	ew := &errWriter{w: w}
	_, err := io.Copy(ew, acc.in)
	if ew.err != nil {
		return 7, "the sender went away"
	}
	if err != nil || acc.gone() {
		return 7, "the recipient went away"
	}
	return 0, ""
}

// errWriter remembers the first write error.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
}

// doChat is `chat <peer> [--chunked]`: open-ended streams both ways on one exchange. Exits.
func (s *Session) doChat(a []string) {
	h := s.h
	p := &args{s: s, a: a}
	peer, chunked := "", false
	if p.more() && !strings.HasPrefix(p.peek(), "-") {
		peer = p.shift()
	}
	for p.more() {
		switch p.peek() {
		case "--chunked":
			chunked = true
			p.shift()
		case "--id", "--subject":
			s.die(2, "dispatch: chat takes no %s: the hub mints the msgid, and the Subject is chat", p.peek())
		case "--size", "--durable", "--wait":
			s.die(2, "dispatch: %s is not for chat: nothing is stored", p.peek())
		default:
			p.unexpected()
		}
	}
	if peer == "" {
		s.die(2, "dispatch: chat <peer> [--chunked]: a peer is required")
	}
	if !validIdent(peer) {
		s.die(2, "dispatch: bad peer: %s", identProblem(peer))
	}
	if receivesNothing(s.me) {
		s.die(9, "dispatch: %s is a %s: it receives nothing, so it chats with nobody", s.me, idRole(s.me))
	}
	msgid := h.mintMsgid()
	x, acc := s.offer(peer, msgid, chunked, true, nil)
	defer h.live.close(x)
	stop := context.AfterFunc(s.ctx, func() { x.finish(7, "the sender went away") })
	defer stop()
	if _, err := io.WriteString(acc.out, h.liveHeader(s.me, msgid, "", "chat", nil, chunked)); err != nil {
		x.finish(7, "the recipient went away")
		s.exit(7)
	}
	// forward: the sender's stdin to the accept session's stdout, until its EOF or last chunk
	fwd := make(chan int, 1)
	go func() {
		rc := 0
		if chunked {
			lr := &relay{cap: math.MaxInt64, errf: func(m string) { s.errf("%s", m) }}
			rc = lr.run(s.in, acc.out, nil, false)
		} else if _, err := io.Copy(acc.out, s.in); err != nil {
			rc = 7
		}
		if rc != 7 && !chunked {
			acc.closeOut()
		}
		fwd <- rc
	}()
	// back: the accept session's stdin to the sender's stdout, its Status block first
	head, code, why := replyHead(acc.in)
	if code != 0 {
		s.errf("dispatch: %s", why)
		x.finish(code, "the back stream broke: "+why)
		s.exit(7)
	}
	s.flush()
	if _, err := io.WriteString(s.rawOut, head); err != nil {
		x.finish(7, "the sender went away")
		s.exit(7)
	}
	if !strings.HasPrefix(head, "Status: 2") { // refused: the end
		x.finish(0, "refused")
		s.exit(0)
	}
	back, bwhy := 0, ""
	if chunked {
		lr := &relay{cap: math.MaxInt64, errf: func(m string) { s.errf("%s", m) }}
		switch back = lr.run(acc.in, s.rawOut, nil, false); {
		case back == 4 && acc.gone():
			back, bwhy = 7, "the recipient went away"
		case back == 4:
			bwhy = "the back stream was incomplete or aborted"
		case back == 2:
			back, bwhy = 7, "the back stream was malformed"
		case back == 7:
			bwhy = "the sender went away"
		}
	} else {
		back, bwhy = rawCopy(s.rawOut, acc)
		if back == 0 { // D24: the recipient's EOF is the sender's, now, whatever its own stdin still sends
			s.closeOut()
		}
	}
	f := 0
	if back == 0 { // the session ends when both directions have; a broken back stream ends it now
		f = <-fwd
	}
	switch {
	case back == 7 || f == 7:
		if bwhy == "" {
			bwhy = "the recipient went away"
		}
		x.finish(7, bwhy)
		s.exit(7)
	case f == 4 || back == 4:
		if bwhy == "" {
			bwhy = "the request was incomplete or aborted"
		}
		x.finish(4, bwhy)
		s.exit(4)
	case f != 0:
		x.finish(4, "the request was malformed")
		s.exit(f)
	}
	x.finish(0, "ok")
	s.exit(0)
}

// doAccept takes an exchange announced on your follow stream (`LIVE <sid> …`):
// stdout = the hub header block and the request, stdin = the reply. Exit: the verdict.
func (s *Session) doAccept(a []string) {
	h := s.h
	if len(a) > 1 {
		s.die(2, "dispatch: unexpected arg '%s'", a[1])
	}
	sid := ""
	if len(a) == 1 {
		sid = a[0]
	}
	if !sidRE.MatchString(sid) {
		s.die(2, "dispatch: accept needs the exchange id of a LIVE line")
	}
	x := h.live.get(sid)
	if x == nil || x.to != s.me {
		s.die(3, "dispatch: no live exchange %s for %s", sid, s.me)
	}
	if !h.live.take(x) {
		s.die(3, "dispatch: live exchange %s was taken or has expired", sid)
	}
	s.flush()
	x.accepted <- &acceptor{in: s.in, out: s.rawOut, closeOut: s.closeOut, gone: s.gone}
	select {
	case <-x.done:
	case <-s.ctx.Done(): // this session is gone; the sender learns it from the streams
		s.exit(7)
	}
	switch x.verdict {
	case 0:
		s.exit(0)
	case 2:
		s.die(2, "dispatch: reply refused: %s", x.why)
	case 4:
		s.die(4, "dispatch: %s; discard it", x.why)
	default:
		s.die(7, "dispatch: the exchange broke: %s", x.why)
	}
}

// liveTarget: target is one a live exchange may go to; exits 9 for one live is not for (a topic, a
// subscription, an alias, a publisher, the instance form of an address that takes no instances, an
// identity whose description says live: false), 3 for no such target, as a send would.
func (h *Hub) liveTarget(s *Session, target string) {
	if validIdent(target) && hubRole(idRole(target)) && (h.isTopic(target) || h.isSub(target)) {
		s.die(9, "dispatch: a live exchange goes to an identity; %s is a %s", target, idRole(target))
	}
	if validIdent(target) && isInstance(target) {
		if parent, _, _ := instSplit(target); !h.instCapable(parent) {
			s.die(9, "dispatch: %s takes no instances: no live exchange with %s", parent, target)
		}
	}
	typ, _, ok := s.resolveTarget(target)
	if !ok {
		s.die(3, "dispatch: no such target %s", target)
	}
	if typ != "native" {
		s.die(9, "dispatch: a live exchange goes to an identity; %s is a %s", target, typ)
	}
	if idRole(s.me) == "publisher" || receivesNothing(target) {
		s.die(9, "dispatch: a publisher neither sends nor takes a live exchange")
	}
	addr := target
	if parent, _, ok := instSplit(target); ok {
		addr = parent
	}
	if rec := loadDescription(h.describedPath(addr)); rec != nil && descriptor.LiveRefused(rec.Whole.JSON) {
		s.die(9, "dispatch: %s takes no live exchange (its description: live: false)", addr)
	}
}
