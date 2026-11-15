package hub

// Following a mailbox: one follower per mailbox, the newest wins. A delivery, a claim change, an open
// exchange or a change of the wake notice wakes it; the 30 s poll is a backstop.

import (
	"sync"
	"time"

	"github.com/cycanics/agsshub/go/expiry"
)

// followRegistry: the follower of each mailbox. Its mutex is the follow lock: a takeover registers the new
// follower and marks the previous one under it, map operations only — nothing is written or closed while
// it is held.
type followRegistry struct {
	mu sync.Mutex
	m  map[string]*follower
}

type follower struct {
	name     string
	conn     string        // the follow id `<conn>.<session>` of the session following
	wake     chan struct{} // cap 1: a pending nudge
	replaced chan struct{} // closed after the follow lock, once by is set
	by       string        // the follow id that took over: set under the follow lock
}

func newFollowRegistry() *followRegistry {
	return &followRegistry{m: map[string]*follower{}}
}

func (f *follower) poke() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// isReplaced: a newer follower took over; f.by names its connection.
func (f *follower) isReplaced() bool {
	select {
	case <-f.replaced:
		return true
	default:
		return false
	}
}

// register makes connection conn's follow the follower of name. Under the follow lock it registers and
// marks the previous follower replaced by conn; after it, the previous one is signalled and ends itself:
// its final chunk is written by its own session, never here. old is nil when nothing followed name.
func (r *followRegistry) register(name, conn string) (f, old *follower) {
	f = &follower{name: name, conn: conn, wake: make(chan struct{}, 1), replaced: make(chan struct{})}
	r.mu.Lock()
	if old = r.m[name]; old != nil {
		old.by = conn
	}
	r.m[name] = f
	r.mu.Unlock()
	if old != nil {
		close(old.replaced)
	}
	return f, old
}

func (r *followRegistry) unregister(f *follower) {
	r.mu.Lock()
	if r.m[f.name] == f {
		delete(r.m, f.name)
	}
	r.mu.Unlock()
}

func (r *followRegistry) get(name string) *follower {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[name]
}

// following: something follows name's mailbox — the test of a live recipient.
func (r *followRegistry) following(name string) bool { return r.get(name) != nil }

func (r *followRegistry) nudge(name string) {
	if f := r.get(name); f != nil {
		f.poke()
	}
}

// nudgeMatching wakes every follower whose mailbox name satisfies pred.
func (r *followRegistry) nudgeMatching(pred func(string) bool) {
	r.mu.Lock()
	var fs []*follower
	for n, f := range r.m {
		if pred(n) {
			fs = append(fs, f)
		}
	}
	r.mu.Unlock()
	for _, f := range fs {
		f.poke()
	}
}

// followAlive: the followed mailbox still exists for its follower — the identity still holds the
// claim (serial) this session was admitted with, or the subscription is still there.
func (s *Session) followAlive(name string) bool {
	if name == s.me {
		for _, f := range s.h.claimFind(s.me) {
			if recGet(f, "serial") == s.serial {
				return true
			}
		}
		return false
	}
	return s.h.isSub(name)
}

// recvFollow: the backlog, then each message as it lands, as a chunked stream with no final chunk while it
// runs. Every pass writes first a part=live chunk per open exchange to the mailbox not announced yet (own
// mailbox; each once, on whichever follow writes it), and an address's wake notice when its msgid changed;
// then the expiry notices, then the messages in the mailbox's order (a subscription: by seq within a session,
// and after the first pass's backlog the position notice). A full follow consumes each entry once its
// pass is flushed; a --peek follow streams each once. Exit 0 with `0;end=replaced;by=<conn>.<session>` when a newer follower
// takes over (checked before each entry, so a takeover mid-pass ends it there);
// 0 with `0;end=gone` when the mailbox is gone for us (released, rebound, unsubscribed); 0 when the session
// ends.
func (s *Session) recvFollow(r *readReq) int {
	h := s.h
	name, box := r.name, r.box
	var f, old *follower
	if isInstance(name) { // under its admission lock: an ask made before this ends here, one after sees the follow
		unlock := h.admMu.lock(name)
		f, old = h.follows.register(name, s.conn)
		h.askEnd(name, nil, 0)
		unlock()
	} else {
		f, old = h.follows.register(name, s.conn)
	}
	if old != nil {
		h.logf("follow %s conn=%s replaced conn=%s", name, s.conn, old.conn)
	}
	if isInstance(name) { // its address's wake set loses it, and a transient entry for it ends
		h.transientEnd(name, "", nil)
		h.nudgeAddr(name)
		defer h.nudgeAddr(name)
	}
	defer h.follows.unregister(f)
	own, isSub := name == s.me, r.sub != ""
	wakes := own && h.wakeAddr(s.me)
	topic := ""
	if isSub {
		topic = h.subTopic(name)
	}
	c := &chunker{s: s}
	shown := map[string]bool{}
	lastWake := ""
	poll := time.NewTimer(followPoll)
	defer poll.Stop()
	// The watcher wakes a follower whose claim changed; a cheap re-check every second bounds what a
	// missed event costs (one claims/ readdir and one small read).
	check := time.NewTicker(time.Second)
	defer check.Stop()
	how := "woken"
	first := true
	k := &consumer{s: s, r: r, c: c}
	// show streams e once; a full follow consumes it at the pass's flush (an expiry notice that cannot be
	// cleared stays shown: a change is a new msgid). false when the session is gone.
	show := func(e *entry) bool {
		defer e.close()
		shown[e.mid] = true
		return s.emit(c, r, e) && k.add(e)
	}
	// however it ends, the offers it announced and nobody took go back: the next follow names them (a
	// replaced reader stops without taking them; a dropped one never read them)
	defer func() {
		if own && h.live.unannounce(f) > 0 {
			h.follows.nudge(name)
		}
	}()
	// replaced: what this pass flushed is consumed, then the final chunk naming the new connection
	replaced := func() int {
		if !k.commit() {
			return 0
		}
		c.final(";end=replaced;by=" + f.by)
		s.flush()
		s.errf("-- %s is followed elsewhere now (conn %s) --", name, f.by)
		return 0 // ended with its final chunk
	}
	for {
		if f.isReplaced() { // before the gone check: a replaced follow always ends with end=replaced
			return replaced()
		}
		if !s.followAlive(name) {
			break
		}
		var done evID
		if isSub {
			done = h.topicDone(topic) // before the scan: never a head the mailbox lacks
		}
		if first && h.noticeProbe(s.me) { // once, on connection, before anything else; nothing stored, nothing to clear
			n := expiry.Probe(s.me, h.expiryOptions())
			e := noticeEntry(n.Msgid, expiry.RenderRaw(n))
			if !s.emit(c, r, e) {
				return 0
			}
			h.logf("notice_probe: %s shown %s on %s", s.me, n.Msgid, name)
			h.journalf("notice_probe: %s shown %s on %s", s.me, n.Msgid, name)
		}
		if own {
			for _, x := range h.live.announce(name, f) {
				c.line(x.msgid, "live", x.line())
			}
		}
		if f.isReplaced() {
			return replaced()
		}
		if wakes {
			if w := h.renderWake(s.me); w.msgid != lastWake {
				lastWake = w.msgid
				if !s.emit(c, r, noticeEntry(w.msgid, w.raw())) {
					return 0
				}
			}
		}
		seen := map[string]bool{}
		// notices first: one that appeared since the last pass streams before the next message
		for _, n := range expiry.List(box, h.expiryOptions()) {
			if f.isReplaced() {
				return replaced()
			}
			seen[n.Msgid] = true
			if !shown[n.Msgid] && !show(noticeEntry(n.Msgid, expiry.RenderRaw(n))) {
				return 0 // the session is gone; the notice stays
			}
		}
		for _, mid := range s.boxMids(r) {
			seen[mid] = true
			if shown[mid] {
				continue
			}
			if f.isReplaced() {
				return replaced()
			}
			e := s.msgEntry(r, mid) // nil: gone, or damaged and set aside
			if e != nil && !show(e) {
				return 0 // the session is gone; the message stays
			}
		}
		if f.isReplaced() {
			return replaced()
		}
		if first && isSub && h.headDue(box, done) {
			if !s.emit(c, r, noticeEntry(headMsgid(done), h.headRaw(topic))) {
				return 0
			}
		}
		first = false
		for k := range shown {
			if !seen[k] { // acked elsewhere
				delete(shown, k)
			}
		}
		if !k.commit() {
			return 0 // the session is gone; what was not flushed stays
		}
	wait:
		for {
			select {
			case <-f.wake:
				how = "woken"
				break wait
			case <-poll.C:
				poll.Reset(followPoll)
				how = "poll"
				break wait
			case <-check.C:
				if !s.followAlive(name) {
					how = "claim check"
					break wait
				}
			case <-f.replaced:
				return replaced()
			case <-s.ctx.Done():
				return 0
			}
		}
	}
	c.final(";end=gone")
	s.flush()
	s.errf("-- %s is gone --", name)
	if h.Verbose {
		h.logf("follow %s: gone (%s; serial %s)", name, how, s.serial)
	}
	return 0
}
