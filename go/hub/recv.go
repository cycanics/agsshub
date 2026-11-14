package hub

// Reading (docs/wake-live-event-id.md, Reading): recv, recv_bag and recv_ack. stdout of recv is an HTTP/1.1
// chunked stream, every chunk `;id=<msgid>;part=<part>`, except the raw outputs (--peek:rfc5322,
// --peek:file, --peek:bag) and recv_bag. Only a full read consumes; --peek never does. Every read that
// outputs body or payload bytes checks the file's size against its trailer's Total-Size first; a damaged
// file is set aside in <mailbox>/.damaged/.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"agsshub/expiry"
)

// headerMax bounds a stored header block: the client's (FieldMaxBytes) and the hub's, a --msg path included.
const headerMax = FieldMaxBytes + msgPathMax + 1024

// msgPathMax: the longest --msg path.
const msgPathMax = 1024

// payloadChunk: the size of each part=payload chunk.
const payloadChunk = 64 * 1024

// ackMax: the most ids one `recv_ack -m:ids` takes.
const ackMax = 1024

// headerBlock: the lines of a stored message up to its first blank line (a header-only read: no check).
func headerBlock(path string) []string {
	b := headOnly(path)
	if b == nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n\n"), "\n")
}

// headOnly: a stored message's header block, its blank line included, without the content-read check; nil
// when it cannot be read.
func headOnly(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := make([]byte, headerMax)
	n, _ := io.ReadFull(f, b)
	i := bytes.Index(b[:n], []byte("\n\n"))
	if i < 0 {
		return nil
	}
	return b[:i+2]
}

// hdrGet: the value of the first line whose name (up to ": ") is name.
func hdrGet(lines []string, name string) string {
	for _, l := range lines {
		k, v, found := strings.Cut(l, ": ")
		if !found {
			k, v = l, l
		}
		if k == name {
			return v
		}
	}
	return ""
}

// msgFiles: the .msg files of a mailbox in msgid (byte) order.
func msgFiles(box string) []string {
	var out []string
	for _, n := range globNames(box) {
		if strings.HasSuffix(n, "."+msgExt) {
			out = append(out, filepath.Join(box, n))
		}
	}
	return out
}

// openContent opens mid's message file in box for a content read: nil when it is not there; a damaged one
// is set aside (and nil).
func (s *Session) openContent(box, mid string) (*msgFile, bool) {
	m, err := openMsg(filepath.Join(box, mid+"."+msgExt))
	if d, ok := err.(*errDamaged); ok {
		s.h.setAsideDamaged(box, mid+"."+msgExt, d)
		return nil, true
	}
	return m, false
}

// mustContent is openContent for a raw read that names mid: exit 3 none, exit 1 damaged.
func (s *Session) mustContent(box, mid string) *msgFile {
	m, damaged := s.openContent(box, mid)
	if damaged {
		s.die(1, "dispatch: %s: damaged", mid)
	}
	if m == nil {
		s.die(3, "dispatch: no such message: %s", mid)
	}
	return m
}

// --- what a read asks for --------------------------------------------------------------------------------

// The content of a read (Content).
const (
	cFull    = iota // header + body + payload: a full read without --peek, or --peek:full
	cBody           // --peek: header + body
	cHeader         // --peek:header
	cRFC5322        // --peek:rfc5322: one message unframed
	cFile           // --peek:file: the file payload, raw
	cBag            // --peek:bag [<path>]: the tar, or one member, raw
)

type readReq struct {
	name, box string // the mailbox read: the session's own, or a subscription the session owns
	sub       string // the subscription, or ""
	peek      bool   // never consumes
	content   int
	bagPath   string // --peek:bag <path>
	limit     int64  // --limit, 0 for none
	ids       []string
	follow    bool
	hdrErr    bool // --header:stderr
}

func (r *readReq) raw() bool { return r.content >= cRFC5322 }

// mailboxArg takes a leading argument containing `@`: the mailbox to read (your own, or a subscription you
// own); none: your own.
func (s *Session) mailboxArg(p *args) (name, box, sub string) {
	name = s.me
	if p.more() && strings.Contains(p.peek(), "@") {
		n := p.shift()
		if n != s.me {
			if !s.subOwned(n) {
				s.die(3, "dispatch: not your mailbox: %s", n)
			}
			name, sub = n, n
		}
	}
	return name, s.h.mbox(name), sub
}

func (s *Session) doRecv(a []string) {
	p := &args{s: s, a: a}
	r := &readReq{}
	r.name, r.box, r.sub = s.mailboxArg(p)
	peekSeen := false
	for p.more() {
		o := p.peek()
		switch {
		case o == "--peek" || strings.HasPrefix(o, "--peek:"):
			if peekSeen {
				s.die(2, "dispatch: one --peek")
			}
			peekSeen, r.peek = true, true
			p.shift()
			switch o {
			case "--peek":
				r.content = cBody
			case "--peek:header":
				r.content = cHeader
			case "--peek:full":
				r.content = cFull
			case "--peek:rfc5322":
				r.content = cRFC5322
			case "--peek:file":
				r.content = cFile
			case "--peek:bag":
				r.content = cBag
				if p.more() && !strings.HasPrefix(p.peek(), "-") {
					r.bagPath = p.shift()
				}
			default:
				s.die(2, "dispatch: unknown content %s", o)
			}
		case o == "--limit":
			v := p.value("--limit")
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 1 || strconv.FormatInt(n, 10) != v {
				s.die(2, "dispatch: bad --limit")
			}
			r.limit = n
		case o == "--id":
			id := p.value("--id")
			if !validMsgid(id) {
				s.die(2, "dispatch: bad msgid: %s", id)
			}
			r.ids = append(r.ids, id)
		case o == "--follow":
			r.follow = true
			p.shift()
		case o == "--header:stderr":
			r.hdrErr = true
			p.shift()
		default:
			p.unexpected()
		}
	}
	switch {
	case r.follow && len(r.ids) > 0:
		s.die(2, "dispatch: --id with --follow")
	case r.limit > 0 && (r.follow || len(r.ids) > 0):
		s.die(2, "dispatch: --limit is for a listing")
	case r.raw() && len(r.ids) != 1:
		s.die(2, "dispatch: --peek:%s needs exactly one --id", [...]string{"rfc5322", "file", "bag"}[r.content-cRFC5322])
	case r.content == cRFC5322 && r.hdrErr:
		s.die(2, "dispatch: --peek:rfc5322 carries its header; --header:stderr does not apply")
	}
	switch {
	case r.raw():
		s.recvRaw(r)
	case r.follow:
		s.exit(s.recvFollow(r))
	case len(r.ids) > 0:
		s.recvIDs(r)
	default:
		s.recvList(r)
	}
}

// --- the stream ------------------------------------------------------------------------------------------

// entry is one message or notice of a read: its header block (its blank line included), its body and, when
// the read wants it, its payload.
type entry struct {
	mid    string
	notice bool // rendered by the hub: expired-*, wakes-*, head-*
	head   []byte
	body   io.Reader
	bodyN  int64
	pay    io.Reader
	payN   int64
	m      *msgFile
}

func (e *entry) close() {
	if e.m != nil {
		e.m.Close()
	}
}

// noticeEntry: a rendered notice (header block, blank line, body).
func noticeEntry(mid, rendered string) *entry {
	head, body, _ := strings.Cut(rendered, "\n\n")
	return &entry{mid: mid, notice: true, head: []byte(head + "\n\n"), body: strings.NewReader(body), bodyN: int64(len(body))}
}

// msgEntry opens mid in box for read r: nil when it is gone or damaged (set aside).
func (s *Session) msgEntry(r *readReq, mid string) *entry {
	if r.content == cHeader { // a header-only read: no content check
		head := headOnly(filepath.Join(r.box, mid+"."+msgExt))
		if head == nil {
			return nil
		}
		return &entry{mid: mid, head: head}
	}
	m, _ := s.openContent(r.box, mid)
	if m == nil {
		return nil
	}
	e := &entry{mid: mid, head: m.head, body: m.body(), bodyN: m.l.msgSize, m: m}
	if r.content == cFull {
		if _, _, pr, n := m.payload(); pr != nil {
			e.pay, e.payN = pr, n
		}
	}
	return e
}

// chunker writes the chunked stream on the session's stdout and counts its bytes.
type chunker struct {
	s *Session
	n int64
}

func (c *chunker) head(mid, part string, size int64) {
	k, _ := fmt.Fprintf(c.s.out, "%x;id=%s;part=%s\r\n", size, mid, part)
	c.n += int64(k) + size + 2
}

// chunk writes one chunk of data from the readers, size bytes in all (size > 0).
// A short copy (a read error after the open's check) or a failed write cuts the stream: the session exits
// 1 with no final chunk, so a size line never promises bytes that do not follow, and a gone client stops
// the read.
func (c *chunker) chunk(mid, part string, size int64, rs ...io.Reader) {
	c.s.outMu.Lock()
	defer c.s.outMu.Unlock()
	c.head(mid, part, size)
	var n int64
	for _, r := range rs {
		k, err := io.Copy(c.s.out, r)
		n += k
		if err != nil {
			break
		}
	}
	if _, err := c.s.out.WriteString("\r\n"); err != nil || n != size {
		if err == nil {
			c.s.h.logf("recv %s: %s: %d of %d bytes read; stream cut", c.s.me, mid, n, size)
		}
		c.s.die(1, "dispatch: %s: read cut short; stream cut", mid)
	}
}

// split writes r's n bytes as chunks of at most payloadChunk; nothing when n is 0.
func (c *chunker) split(mid, part string, r io.Reader, n int64) {
	for n > 0 {
		k := min(n, payloadChunk)
		c.chunk(mid, part, k, io.LimitReader(r, k))
		n -= k
	}
}

// final writes the last chunk, with ext (";limit", ";end=…") or none.
func (c *chunker) final(ext string) {
	c.s.outMu.Lock()
	c.s.out.WriteString("0" + ext + "\r\n\r\n")
	c.s.outMu.Unlock()
}

// line: a one-line chunk (part=missing, part=live).
func (c *chunker) line(mid, part, l string) {
	c.chunk(mid, part, int64(len(l)+1), strings.NewReader(l+"\n"))
}

// emit streams e as read r asks: false when stdout failed (the client is gone). It answers --wait when the
// body or the payload went out.
func (s *Session) emit(c *chunker, r *readReq, e *entry) bool {
	withBody := r.content != cHeader
	if !r.hdrErr {
		size := int64(len(e.head))
		var rs []io.Reader
		rs = append(rs, bytes.NewReader(e.head))
		if withBody {
			size += e.bodyN
			rs = append(rs, e.body)
		}
		c.chunk(e.mid, "msg", size, rs...)
	} else {
		id := "Header: " + e.mid + "\n"
		c.line(e.mid, "id", strings.TrimSuffix(id, "\n"))
		if s.flush() != nil { // the id chunk first, then its header block on stderr
			return false
		}
		s.errw.Write(append([]byte(id), e.head...))
		if withBody {
			c.split(e.mid, "body", e.body, e.bodyN)
		}
	}
	c.split(e.mid, "payload", e.pay, e.payN)
	if !e.notice && (withBody || e.payN > 0) {
		s.h.picked(r.box, e.mid)
	}
	return true
}

// consumeBatch: the stdout bytes after which a full read flushes and removes what it streamed whole.
const consumeBatch = 1 << 20

// consumer removes what a full read streamed whole, once it is written to the connection: the entries wait
// in pending until a flush succeeds (at the read's end, or every consumeBatch bytes); a failed flush removes
// nothing. wakes-* and head-* are never consumed; expired-* only if unchanged.
type consumer struct {
	s       *Session
	r       *readReq
	c       *chunker
	pending []string
	msgs    bool  // a message among pending
	at      int64 // c.n at the last commit
}

// add queues e for removal; false when a commit it triggered failed.
func (k *consumer) add(e *entry) bool {
	if k.r.peek || e.notice && !strings.HasPrefix(e.mid, expiry.Prefix) {
		return true
	}
	k.pending = append(k.pending, e.mid)
	k.msgs = k.msgs || !strings.HasPrefix(e.mid, expiry.Prefix)
	if k.c.n-k.at >= consumeBatch {
		return k.commit()
	}
	return true
}

// commit flushes stdout, then removes the pending entries; false when the flush failed (nothing removed).
func (k *consumer) commit() bool {
	if k.s.flush() != nil {
		return false
	}
	k.at = k.c.n
	s, r := k.s, k.r
	for _, mid := range k.pending {
		if strings.HasPrefix(mid, expiry.Prefix) {
			expiry.Clear(r.box, mid, s.h.expiryOptions())
			continue
		}
		dropMsg(r.box, mid)
	}
	if k.msgs && isInstance(r.name) { // once per batch: its address's wake notice changed
		s.h.mailChanged(r.name)
	}
	k.pending, k.msgs = k.pending[:0], false
	return true
}

// --- a listing -------------------------------------------------------------------------------------------

// listing: the read's entries in order, opened lazily: the wake notice, expiry notices in byte order of
// from, messages in the mailbox's order, then a subscription's position notice (when the mailbox does not
// hold done). Each func returns nil for an entry gone meanwhile.
func (s *Session) listing(r *readReq) []func() *entry {
	h := s.h
	var out []func() *entry
	var done evID
	topic := ""
	if r.sub != "" {
		topic = h.subTopic(r.sub)
		done = h.topicDone(topic) // before the scan: never a head the mailbox lacks
	} else if h.wakeAddr(s.me) {
		out = append(out, func() *entry { w := h.renderWake(s.me); return noticeEntry(w.msgid, w.raw()) })
	}
	for _, n := range expiry.List(r.box, h.expiryOptions()) {
		out = append(out, func() *entry { return noticeEntry(n.Msgid, expiry.RenderRaw(n)) })
	}
	for _, mid := range s.boxMids(r) {
		out = append(out, func() *entry { return s.msgEntry(r, mid) })
	}
	if r.sub != "" && h.headDue(r.box, done) {
		out = append(out, func() *entry { return noticeEntry(headMsgid(done), h.headRaw(topic)) })
	}
	return out
}

// boxMids: the messages of the read's mailbox in its order, every mailbox alike: arrival, the mtime the
// hub stamps as a message lands (topic.go deliverToSub, send.go link), ties by msgid.
func (s *Session) boxMids(r *readReq) []string { return s.h.arrivalOrder(r.box) }

// arrivalOrder reads box under its arrival lock, so no store lands while the directory is read: a listing
// that holds a message holds every one that arrived before it (a directory read in several calls could
// otherwise miss an earlier name and see a later one).
func (h *Hub) arrivalOrder(box string) []string {
	unlock := h.arrMu.lock(box)
	type ent struct {
		mid   string
		mtime int64
	}
	var es []ent
	for _, n := range globNames(box) {
		mid, ok := strings.CutSuffix(n, "."+msgExt)
		if !ok {
			continue
		}
		e := ent{mid: mid}
		if fi, err := os.Stat(filepath.Join(box, n)); err == nil {
			e.mtime = fi.ModTime().UnixNano()
		}
		es = append(es, e)
	}
	unlock()
	sort.Slice(es, func(i, j int) bool {
		if es[i].mtime != es[j].mtime {
			return es[i].mtime < es[j].mtime
		}
		return es[i].mid < es[j].mid
	})
	mids := make([]string, len(es))
	for i, e := range es {
		mids[i] = e.mid
	}
	return mids
}

func (s *Session) recvList(r *readReq) {
	c := &chunker{s: s}
	k := &consumer{s: s, r: r, c: c}
	counted := false // an entry other than the wake and position notices went out: --limit applies from then
	for _, open := range s.listing(r) {
		if counted && r.limit > 0 && c.n >= r.limit { // a message boundary at or past --limit, more waiting
			c.final(";limit")
			k.commit()
			return
		}
		e := open()
		if e == nil {
			continue
		}
		counted = counted || !strings.HasPrefix(e.mid, WakePrefix) && !strings.HasPrefix(e.mid, HeadPrefix)
		ok := s.emit(c, r, e) && k.add(e)
		e.close()
		if !ok {
			return
		}
	}
	c.final("")
	k.commit()
}

// --- by msgid --------------------------------------------------------------------------------------------

// byID: the entry msgid names in the read's mailbox, notices included; nil when it holds none.
func (s *Session) byID(r *readReq, mid string) *entry {
	h := s.h
	switch {
	case strings.HasPrefix(mid, expiry.Prefix):
		if n := expiry.Current(r.box, mid, h.expiryOptions()); n != nil {
			return noticeEntry(n.Msgid, expiry.RenderRaw(*n))
		}
		return nil // an older msgid, or a notice_probe's (never stored)
	case strings.HasPrefix(mid, WakePrefix):
		if r.sub == "" && h.wakeAddr(s.me) {
			w := h.renderWake(s.me)
			return noticeEntry(w.msgid, w.raw())
		}
		return nil
	case strings.HasPrefix(mid, HeadPrefix):
		if r.sub != "" {
			t := h.subTopic(r.sub)
			if mid == headMsgid(h.topicDone(t)) {
				return noticeEntry(mid, h.headRaw(t))
			}
		}
		return nil
	}
	return s.msgEntry(r, mid)
}

func (s *Session) recvIDs(r *readReq) {
	c := &chunker{s: s}
	k := &consumer{s: s, r: r, c: c}
	found := 0
	for _, mid := range r.ids {
		e := s.byID(r, mid)
		if e == nil {
			c.line(mid, "missing", "Missing: "+mid)
			continue
		}
		found++
		ok := s.emit(c, r, e) && k.add(e)
		e.close()
		if !ok {
			return
		}
	}
	c.final("")
	k.commit()
	if found == 0 {
		s.exit(3)
	}
}

// --- raw outputs -----------------------------------------------------------------------------------------

// hdrStderr: with --header:stderr, the message's header block on stderr, before its raw bytes.
func (s *Session) hdrStderr(on bool, mid string, head []byte) {
	if on {
		s.errw.Write(append([]byte("Header: "+mid+"\n"), head...))
	}
}

// recvRaw: --peek:rfc5322, --peek:file, --peek:bag of the one --id, and recv_bag's tar. Consumes nothing.
func (s *Session) recvRaw(r *readReq) {
	mid := r.ids[0]
	if isNoticeID(mid) {
		if r.content != cRFC5322 {
			s.die(2, "dispatch: %s is a notice: it has no payload", mid)
		}
		e := s.byID(r, mid)
		if e == nil {
			s.die(3, "dispatch: no such message: %s", mid)
		}
		s.outMu.Lock()
		s.out.Write(e.head)
		io.Copy(s.out, e.body)
		s.outMu.Unlock()
		return
	}
	m := s.mustContent(r.box, mid) // opened once, first: an ack or a consuming read from then on cannot cut it short
	defer m.Close()
	var out io.Reader
	switch r.content {
	case cRFC5322:
		out = io.MultiReader(bytes.NewReader(m.head), m.body())
	case cFile:
		kind, _, pr, _ := m.payload()
		if kind != "file" {
			s.die(2, "dispatch: %s carries no file payload", mid)
		}
		out = pr
	case cBag:
		kind, _, pr, pn := m.payload()
		if kind != "bag" {
			s.die(2, "dispatch: %s carries no bag", mid)
		}
		out = pr
		if r.bagPath != "" {
			var code int
			if out, code = bagMember(pr, pn, r.bagPath); code == 2 {
				s.die(2, "dispatch: %s in %s is no regular member", r.bagPath, mid)
			} else if code == 3 {
				s.die(3, "dispatch: no such member: %s", r.bagPath)
			}
		}
	}
	s.hdrStderr(r.hdrErr, mid, m.head)
	s.outMu.Lock()
	_, err := io.Copy(s.out, out)
	s.outMu.Unlock()
	if err != nil { // a read error after the open's check, or a gone client: nothing consumed
		s.die(1, "dispatch: %s: read cut short", mid)
	}
	s.h.picked(r.box, mid)
}

// isInstanceSession: the session is an instance's.
func (s *Session) isInstanceSession() bool { return isInstance(s.me) }

// doRecvBag: `recv_bag [<mailbox>] [--id <id>] [--peek] [--header:stderr]`: the stored tar, raw; consumed
// without --peek once written.
func (s *Session) doRecvBag(a []string) {
	p := &args{s: s, a: a}
	r := &readReq{content: cFull}
	r.name, r.box, r.sub = s.mailboxArg(p)
	mid := ""
	for p.more() {
		switch p.peek() {
		case "--id":
			if mid != "" {
				s.die(2, "dispatch: recv_bag takes one --id")
			}
			mid = p.value("--id")
			if !validMsgid(mid) {
				s.die(2, "dispatch: bad msgid: %s", mid)
			}
		case "--peek":
			r.peek = true
			p.shift()
		case "--header:stderr":
			r.hdrErr = true
			p.shift()
		default:
			p.unexpected()
		}
	}
	if mid == "" {
		mids := s.boxMids(r)
		if len(mids) == 0 {
			s.die(3, "dispatch: no messages")
		}
		mid = mids[0]
	}
	r.content, r.ids = cBag, []string{mid}
	s.recvRaw(r)
	if !r.peek && s.flush() == nil {
		dropMsg(r.box, mid)
		if isInstance(r.name) {
			s.h.mailChanged(r.name)
		}
	}
}

// --- recv_ack --------------------------------------------------------------------------------------------

// doRecvAck: `recv_ack [<mailbox>] <id>…`, `recv_ack [<mailbox>] -m:ids` (ids on stdin, one per line, at
// most ackMax), or `recv_ack [<mailbox>] --before[-including] <id>` (what precedes <id> in listing order).
// Every id is checked before any is acted on.
func (s *Session) doRecvAck(a []string) {
	p := &args{s: s, a: a}
	name, box, sub := s.mailboxArg(p)
	var ids []string
	fromStdin := false
	before, incl := "", false
	for p.more() {
		switch v := p.peek(); {
		case v == "-m:ids":
			fromStdin = true
			p.shift()
		case v == "--before" || v == "--before-including":
			if before != "" {
				s.die(2, "dispatch: one --before or --before-including")
			}
			before, incl = p.value(v), v == "--before-including"
		case strings.HasPrefix(v, "-"):
			s.die(2, "dispatch: recv_ack takes no %s", v)
		case strings.Contains(v, "@"):
			s.die(2, "dispatch: one mailbox: %s", v)
		default:
			ids = append(ids, p.shift())
		}
	}
	if before != "" {
		if fromStdin || len(ids) > 0 {
			s.die(2, "dispatch: --before or --before-including, not with ids or -m:ids")
		}
		s.ackBefore(name, box, sub, before, incl)
		return
	}
	if fromStdin {
		if len(ids) > 0 {
			s.die(2, "dispatch: ids as arguments or -m:ids, not both")
		}
		sc := bufio.NewScanner(io.LimitReader(s.in, int64(ackMax)*70))
		for sc.Scan() {
			if l := strings.TrimSuffix(sc.Text(), "\r"); l != "" {
				ids = append(ids, l)
			}
		}
		if len(ids) > ackMax {
			s.die(2, "dispatch: at most %d ids", ackMax)
		}
		if extra, _ := s.in.Peek(1); len(extra) > 0 {
			s.die(2, "dispatch: at most %d ids", ackMax)
		}
	}
	if len(ids) == 0 {
		s.die(2, "dispatch: recv_ack needs an id")
	}
	for _, id := range ids {
		if !validMsgid(id) {
			s.die(2, "dispatch: bad msgid: %s", id)
		}
	}
	s.ackIDs(name, box, sub, ids)
}

// ackIDs acks ids in turn, one line each, then nudges the address's wake notice once if a message went.
func (s *Session) ackIDs(name, box, sub string, ids []string) {
	removed := false
	for _, id := range ids {
		v := s.ackOne(box, sub, id)
		removed = removed || v == "acked" && !isNoticeID(id)
		s.println(v + " " + id)
	}
	if removed && isInstance(name) { // once: its address's wake notice changed
		s.h.mailChanged(name)
	}
}

// ackBefore acks the messages that precede target in arrival order, target too with incl. The order is read
// once, at the call: what lands meanwhile is not in it, and sorts after target anyway. Notices are never in
// it: one as target exits 2; a target the mailbox does not hold: exit 3, nothing acked.
func (s *Session) ackBefore(name, box, sub, target string, incl bool) {
	if !validMsgid(target) {
		s.die(2, "dispatch: bad msgid: %s", target)
	}
	if isNoticeID(target) {
		s.die(2, "dispatch: %s is a notice: it is never in a --before range; ack it by id", target)
	}
	order := s.boxMids(&readReq{name: name, box: box, sub: sub})
	testHook("ack-ordered")
	at := -1
	for i, id := range order {
		if id == target {
			at = i
			break
		}
	}
	if at < 0 {
		s.die(3, "dispatch: no such message: %s", target)
	}
	if incl {
		at++
	}
	s.ackIDs(name, box, sub, order[:at])
}

// ackOne acks id: "acked", "gone" (already removed, which counts as done), or "refused" (a notice the hub
// renders).
func (s *Session) ackOne(box, sub, id string) string {
	switch {
	case strings.HasPrefix(id, WakePrefix), strings.HasPrefix(id, HeadPrefix):
		return "refused"
	case strings.HasPrefix(id, expiry.Prefix):
		switch expiry.Clear(box, id, s.h.expiryOptions()) {
		case expiry.Cleared:
			return "acked"
		case expiry.Failed:
			s.die(1, "dispatch: cannot clear %s", id)
		}
		if expiry.IsProbe(id, s.me) { // not stored: nothing to do
			s.h.logf("notice_probe: %s acked %s", s.me, id)
			s.h.journalf("notice_probe: %s acked %s", s.me, id)
			return "acked"
		}
		return "gone"
	}
	if err := os.Remove(filepath.Join(box, id+"."+msgExt)); err != nil {
		if !os.IsNotExist(err) {
			s.die(1, "dispatch: cannot ack %s", id)
		}
		return "gone"
	}
	return "acked"
}

// isNoticeID: a msgid of one of the hub's own notices (expired-*, wakes-*, head-*).
func isNoticeID(m string) bool {
	return strings.HasPrefix(m, expiry.Prefix) || strings.HasPrefix(m, WakePrefix) || strings.HasPrefix(m, HeadPrefix)
}
