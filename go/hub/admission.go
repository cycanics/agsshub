package hub

// Admitting an instance and the wake notice (docs/identities.md, Instances; docs/live-send.md, Wake
// notice). An instance is addr+inst; addr's manager alone decides whether one exists. A send to it is
// stored at once when its mailbox is there and, with --wait, it is followed; otherwise it holds the sender
// and asks addr, whose follow lists it on the wake notice: `admit` or `reject` answers. The hub remembers
// nothing about a refused id. Every decision for one addr+inst runs under its admission lock, an in-memory
// mutex.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// isInstance: t is the instance form of an address.
func isInstance(t string) bool {
	if !strings.Contains(t, "+") {
		return false
	}
	_, _, ok := instSplit(t)
	return ok
}

// nudgeAddr wakes the follow of inst's address: its wake notice may have changed.
func (h *Hub) nudgeAddr(inst string) {
	if parent, _, ok := instSplit(inst); ok {
		h.follows.nudge(parent)
	}
}

// instBox is the one maker of an instance's mailbox, called under its admission lock: false when the
// mailbox cannot be made.
func (h *Hub) instBox(inst string) bool {
	box := h.mbox(inst)
	if isDir(box) {
		return true
	}
	os.MkdirAll(h.inbox, 0o2770)
	if err := os.Mkdir(box, 0o2770); err != nil && !errors.Is(err, fs.ErrExist) {
		return false
	}
	return isDir(box)
}

// --- ask entries: senders held for the address's answer -------------------------------------------

type ask struct {
	from   map[string]int // held senders, each counted per held session
	held   int
	done   chan struct{} // closed with the answer
	answer int           // 0 answered (admit, register, a follow) · 3 rejected · 6 no answer
	timer  *time.Timer
	seq    uint64 // this ask among all asks: an ask made again for the same id gets a new wake msgid
}

type askTable struct {
	mu  sync.Mutex
	m   map[string]*ask
	seq uint64
}

// askCap: ask entries per address; a send that would make one more is rate limited (exit 5).
const askCap = 256

// join adds a held sender to inst's ask, made if there is none; under inst's admission lock. nil: the
// address has askCap asks already.
func (h *Hub) askJoin(inst, from string) *ask {
	t := h.asks
	t.mu.Lock()
	a := t.m[inst]
	if a == nil {
		parent, _, _ := instSplit(inst)
		n := 0
		for fq := range t.m {
			if p, _, _ := instSplit(fq); p == parent {
				n++
			}
		}
		if n >= askCap {
			t.mu.Unlock()
			return nil
		}
		t.seq++
		a = &ask{from: map[string]int{}, done: make(chan struct{}), seq: t.seq}
		t.m[inst] = a
		_, d := h.wakeSecs()
		a.timer = time.AfterFunc(d, func() {
			unlock := h.admMu.lock(inst)
			h.askEnd(inst, a, 6)
			unlock()
		})
	}
	a.held++
	a.from[from]++
	t.mu.Unlock()
	h.nudgeAddr(inst)
	return a
}

// askEnd gives a its answer, if it is still inst's ask; under inst's admission lock.
func (h *Hub) askEnd(inst string, a *ask, answer int) {
	t := h.asks
	t.mu.Lock()
	if a == nil {
		a = t.m[inst]
	}
	if a == nil || t.m[inst] != a {
		t.mu.Unlock()
		return
	}
	delete(t.m, inst)
	a.answer = answer
	a.timer.Stop()
	close(a.done)
	t.mu.Unlock()
	h.nudgeAddr(inst)
}

// askLeave: a held sender went away; the ask ends with its last one. Under inst's admission lock.
func (h *Hub) askLeave(inst string, a *ask, from string) {
	t := h.asks
	t.mu.Lock()
	if t.m[inst] != a {
		t.mu.Unlock()
		return
	}
	a.held--
	if a.from[from]--; a.from[from] <= 0 {
		delete(a.from, from)
	}
	last := a.held <= 0
	t.mu.Unlock()
	if last {
		h.askEnd(inst, a, 6)
	} else {
		h.nudgeAddr(inst)
	}
}

// --- transient entries: a --live to an instance with a mailbox nobody follows (flow C) ------------

type transient struct {
	date  int64     // the live attempt, H's clock
	till  time.Time // stay-up-till
	timer *time.Timer
}

type transientTable struct {
	mu sync.Mutex
	m  map[string]map[string]*transient // inst -> sender -> entry
}

// transientTouch makes or refreshes from's entry for inst: stay-up-till is this attempt + 3 × live_retry_secs.
func (h *Hub) transientTouch(inst, from string) time.Time {
	_, d := h.liveRetrySecs()
	now := h.now()
	till := now.Add(3 * d)
	t := h.trans
	t.mu.Lock()
	per := t.m[inst]
	if per == nil {
		per = map[string]*transient{}
		t.m[inst] = per
	}
	if old := per[from]; old != nil {
		old.timer.Stop() // a timer that fired already finds a newer entry, and leaves it
	}
	e := &transient{date: now.Unix(), till: till}
	per[from] = e
	e.timer = time.AfterFunc(3*d, func() { h.transientEnd(inst, from, e) })
	t.mu.Unlock()
	h.nudgeAddr(inst)
	return till
}

// transientEnd: from's entry for inst, if it is still e (nil: every entry of inst) ends.
func (h *Hub) transientEnd(inst, from string, e *transient) {
	t := h.trans
	t.mu.Lock()
	per := t.m[inst]
	n := 0
	for f, x := range per {
		if e == nil || (f == from && x == e) {
			x.timer.Stop()
			delete(per, f)
			n++
		}
	}
	if len(per) == 0 {
		delete(t.m, inst)
	}
	t.mu.Unlock()
	if n > 0 {
		h.nudgeAddr(inst)
	}
}

func (h *Hub) liveRetrySecs() (string, time.Duration) {
	return h.settingSecs("live_retry_secs", liveRetryDef, 600000)
}

// retryAfter ends a send with exit 6 and the retry-after hint (flows A2, C2, C).
func (s *Session) retryAfter(format string, args ...any) {
	v, _ := s.h.liveRetrySecs()
	s.errf(format, args...)
	s.errf("retry-after=%s", v)
	s.exit(6)
}

// --- the decision -----------------------------------------------------------------------------------

// admStored is admission's answer when no answer came within wake_secs and the mailbox is there at that
// moment: the message is stored, the sender exits 8.
const admStored = -1

// admission is the decision for a send to inst (wait: a --wait send): 0 stored at once, or the ask answered
// and the mailbox there · 3 rejected, or admitted and its mailbox gone again · 6 no answer within wake_secs,
// no mailbox · admStored the same with a mailbox · 5 the ask cap. Asked, the sender is held, outside the
// lock, until the ask is answered.
func (s *Session) admission(inst string, wait bool) int {
	h := s.h
	unlock := h.admMu.lock(inst)
	if isDir(h.mbox(inst)) && (!wait || h.follows.following(inst)) {
		unlock()
		return 0
	}
	a := h.askJoin(inst, s.me)
	unlock()
	if a == nil {
		return 5
	}
	testHook("held")
	select {
	case <-a.done:
	case <-s.ctx.Done():
		unlock := h.admMu.lock(inst)
		h.askLeave(inst, a, s.me)
		unlock()
		s.exit(6) // nobody hears it
	}
	switch {
	case a.answer == 3:
		return 3
	case a.answer == 6: // the mailbox at this moment, not when the sender joined
		if isDir(h.mbox(inst)) {
			return admStored
		}
		return 6
	case !isDir(h.mbox(inst)): // admitted, then a reject in between
		return 3
	}
	return 0
}

// admitSendOrExit runs admission for a send or a live send to inst and ends the session on 3 or 6. True: the
// ask had no answer and the mailbox is there: a send stores and exits 8; a live send goes on to flow C.
func (s *Session) admitSendOrExit(inst string, wait bool) bool {
	switch s.admission(inst, wait) { // a held send: its --durable fsync comes once it is stored, after admit
	case admStored:
		return true
	case 5:
		parent, _, _ := instSplit(inst)
		s.die(5, "dispatch: rate limited: %s has %d asks waiting; slow down", parent, askCap)
	case 3:
		s.die(3, "dispatch: no such target %s: refused by its address", inst)
	case 6:
		parent, _, _ := instSplit(inst)
		w, _ := s.h.wakeSecs()
		s.retryAfter("dispatch: %s did not answer for %s within %s s; nothing stored", parent, inst, w)
	}
	return false
}

// --- the store into an instance's mailbox -----------------------------------------------------------

const errInstGone = "dispatch: no such target %s: refused by its address"

// --- admit, reject -----------------------------------------------------------------------------------

// instArgs: the instances a verb of the address names, each its identity; exits 2 or 3 on a bad one.
func (s *Session) instArgs(verb string, a []string) []string {
	s.instAddress()
	if len(a) == 0 {
		s.die(2, "dispatch: %s <instance> [<instance> …]", verb)
	}
	var out []string
	for _, arg := range a {
		fq, rc := s.instMine(arg)
		switch rc {
		case 2:
			s.die(2, "dispatch: %s <instance>: an id matching %s", verb, instRE.String())
		case 3:
			s.die(3, "dispatch: %s is not an instance of %s", arg, s.me)
		}
		out = append(out, fq)
	}
	return out
}

// doAdmit: `admit <inst> …` makes each instance's mailbox and answers its ask.
func (s *Session) doAdmit(a []string) {
	h := s.h
	rc := 0
	for _, fq := range s.instArgs("admit", a) {
		unlock := h.admMu.lock(fq)
		ok := h.instBox(fq)
		if ok {
			h.askEnd(fq, nil, 0)
		}
		unlock()
		if !ok {
			s.errf("dispatch: cannot admit %s: a hub I/O error; retry", fq)
			rc = 1
			continue
		}
		h.nudgeAddr(fq)
		h.journalf("admit %s by %s", fq, s.me)
		s.printf("admitted %s\n", fq)
	}
	s.exit(rc)
}

// doReject: `reject <inst> …` says each is no instance: its claim ends, its ask is answered with exit 3,
// its mailbox and mail go. With nothing to do, exit 0.
func (s *Session) doReject(a []string) {
	h := s.h
	rc := 0
	for _, fq := range s.instArgs("reject", a) {
		c, why := h.rejectOne(fq, s.me)
		if c != 0 {
			s.errf("dispatch: %s %s", fq, why)
			if c > rc {
				rc = c
			}
			continue
		}
		s.printf("rejected %s\n", fq)
	}
	s.exit(rc)
}

// rejectOne, under the registry lock, then the admission lock: the claim ends (a connect that passed its
// claim check sees that under the same lock and makes no mailbox), the ask is answered, the mailbox goes as
// a subscription's does: under its lock, renamed away, then removed. 0 · 1 an I/O error, the mailbox left
// and its mail not counted as discarded; a repeated reject removes it.
func (h *Hub) rejectOne(fq, by string) (int, string) {
	unreg, err := h.lockRegistry()
	if err != nil {
		return 1, "cannot be rejected now: registry busy; retry"
	}
	unlock := h.admMu.lock(fq)
	if rp := h.instRecord(fq); isFile(rp) {
		h.instDrop(rp)
		h.journalf("release %s by %s (reject)", fq, by)
	}
	h.askEnd(fq, nil, 3)
	h.transientEnd(fq, "", nil)
	n, gone, err := h.rejectBox(fq)
	unlock()
	unreg()
	if err != nil {
		h.logf("reject %s by %s: mailbox not removed: %v", fq, by, err)
		return 1, "cannot remove its mailbox (a hub I/O error); retry"
	}
	if gone != "" {
		h.rejectDone(fq, by, n, gone)
	}
	return 0, ""
}

// rejectBox renames a rejected instance's mailbox away under its lock, under the admission lock: the
// count of its messages and the name it went to ("" with no mailbox). A sender in --wait on it ends with 3.
func (h *Hub) rejectBox(fq string) (int, string, error) {
	box := h.mbox(fq)
	if !isDir(box) {
		return 0, "", nil
	}
	ul := lockBox(box)
	if ul == nil {
		if isDir(box) {
			return 0, "", errors.New("its mailbox lock not to be had")
		}
		return 0, "", nil
	}
	n := len(msgFiles(box))
	g := h.stageName(h.inbox)
	err := os.Rename(box, g)
	if err != nil {
		ul()
		return 0, "", err
	}
	h.waitsGone(box)
	ul()
	return n, g, nil
}

// rejectDone, outside the locks: the renamed mailbox removed, the summary read again, the journal line.
func (h *Hub) rejectDone(fq, by string, n int, gone string) {
	h.follows.nudge(fq)
	if gone != "" {
		os.RemoveAll(gone)
	}
	h.mailChanged(fq)
	h.journalf("reject %s by %s discarded=%d", fq, by, n)
}

// --- the wake notice ---------------------------------------------------------------------------------

// WakePrefix starts every wake notice's msgid (HeadPrefix, event.go, every position notice's). A client's --id
// may not.
const WakePrefix = "wakes-"

type wakeNotice struct {
	from, date, msgid string
	n                 int
	body              string
}

func (w wakeNotice) raw() string {
	return fmt.Sprintf("From: %s\nDate: %s\nWake: %d\n\n%s", w.from, w.date, w.n, w.body)
}

// wakeLine is one instance of the wake set as rendered.
type wakeLine struct {
	inst string
	mail int
	from map[string]string // sender -> newest Date
	till time.Time         // a transient entry's stay-up-till, zero without one
	ask  []string          // an ask's held senders
	seq  uint64            // its ask's number (the wake msgid changes with it)
}

// instancesOf: addr's instances that have a mailbox.
func (h *Hub) boxInstances(addr string) []string {
	id := idSplit(addr)
	head, tail := fsName(id.Name)+"+", fsName(strings.TrimPrefix(addr, id.Name))
	var out []string
	names, _ := filepath.Glob(filepath.Join(h.inbox, globQuote(head)+"*"+globQuote(tail)))
	for _, p := range names {
		n := filepath.Base(p)
		if len(n) < len(head)+len(tail) {
			continue
		}
		fq := strings.ReplaceAll(n, "~", "/")
		if parent, _, ok := instSplit(fq); ok && parent == addr {
			out = append(out, fq)
		}
	}
	return out
}

// renderWake is addr's wake notice from the state now: each instance nobody follows, with mail, a transient
// entry or an ask entry; the ask is a field of its own, beside any mail= on the line.
func (h *Hub) renderWake(addr string) wakeNotice {
	lines := map[string]*wakeLine{}
	get := func(fq string) *wakeLine {
		l := lines[fq]
		if l == nil {
			l = &wakeLine{inst: fq, from: map[string]string{}}
			lines[fq] = l
		}
		return l
	}
	h.wsum.mu.Lock()
	for fq, mail := range h.sumAddr(addr) {
		l := get(fq)
		l.mail = len(mail)
		for _, mi := range mail {
			if mi.from != "" && mi.date > l.from[mi.from] {
				l.from[mi.from] = mi.date
			}
		}
	}
	h.wsum.mu.Unlock()
	h.trans.mu.Lock()
	for fq, per := range h.trans.m {
		if p, _, _ := instSplit(fq); p != addr {
			continue
		}
		l := get(fq)
		for from, e := range per {
			if d := isoTime(e.date); d > l.from[from] {
				l.from[from] = d
			}
			if e.till.After(l.till) {
				l.till = e.till
			}
		}
	}
	h.trans.mu.Unlock()
	h.asks.mu.Lock()
	for fq, a := range h.asks.m {
		if p, _, _ := instSplit(fq); p != addr {
			continue
		}
		l := get(fq)
		for from := range a.from {
			l.ask = append(l.ask, from)
		}
		sort.Strings(l.ask)
		l.seq = a.seq
	}
	h.asks.mu.Unlock()
	var mail, asks []*wakeLine
	for fq, l := range lines {
		if h.follows.following(fq) {
			continue
		}
		if l.mail > 0 || !l.till.IsZero() {
			mail = append(mail, l)
		} else if l.ask != nil {
			asks = append(asks, l)
		}
	}
	sort.Slice(mail, func(i, j int) bool { return mail[i].inst < mail[j].inst })
	sort.Slice(asks, func(i, j int) bool { return asks[i].inst < asks[j].inst })
	var b strings.Builder
	for _, l := range mail {
		senders := make([]string, 0, len(l.from))
		for f := range l.from {
			senders = append(senders, f)
		}
		sort.Strings(senders)
		for i, f := range senders {
			senders[i] = f + "@" + l.from[f]
		}
		fmt.Fprintf(&b, "%s\tmail=%d\tfrom=%s", l.inst, l.mail, strings.Join(senders, ","))
		if !l.till.IsZero() {
			fmt.Fprintf(&b, "\tstay-up-till=%s", isoTime(l.till.Unix()))
		}
		if l.ask != nil {
			fmt.Fprintf(&b, "\task=%s", strings.Join(l.ask, ","))
		}
		b.WriteByte('\n')
	}
	for _, l := range asks {
		fmt.Fprintf(&b, "%s\task=%s\n", l.inst, strings.Join(l.ask, ","))
	}
	body := b.String()
	// the asks' numbers too: an ask made again after its answer renders the same line, and must be printed again
	hs := sha256.New()
	hs.Write([]byte(body))
	for _, l := range append(append([]*wakeLine{}, mail...), asks...) {
		fmt.Fprintf(hs, "\n%d", l.seq)
	}
	return wakeNotice{from: addr, date: isoTime(h.epoch()), msgid: WakePrefix + hex.EncodeToString(hs.Sum(nil)[:8]),
		n: len(mail) + len(asks), body: body}
}

// wakeAddr: me is an address that takes instances, so its own mailbox shows the wake notice.
func (h *Hub) wakeAddr(me string) bool {
	return !isInstance(me) && h.instCapable(me)
}

// globQuote escapes filepath.Match's metacharacters.
func globQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)
	return r.Replace(s)
}

// --- the wake summary: per address, per instance, the unread mail's senders and dates ----------------

type mailInfo struct {
	from, date string
	ino        uint64 // the file's: a name used again by another delivery is read again
}

// wakeSum is what the wake notice renders mail lines from, in memory: built from disk at an address's first
// render, then one instance re-read at each change of its mailbox (mailChanged); dropped for an address
// whose claim the operator's tools changed (the watcher).
type wakeSum struct {
	mu sync.Mutex
	m  map[string]map[string]map[string]mailInfo // addr -> inst -> file -> sender, Date
}

// instMail reads one instance's unread mail from disk.
func (h *Hub) instMail(inst string) map[string]mailInfo {
	files := msgFiles(h.mbox(inst))
	if len(files) == 0 {
		return nil
	}
	out := make(map[string]mailInfo, len(files))
	for _, f := range files {
		hdr := headerBlock(f)
		out[filepath.Base(f)] = mailInfo{hdrGet(hdr, "From"), hdrGet(hdr, "Date"), fileIno(f)}
	}
	return out
}

// sumAddr: addr's summary, built from disk if it is not yet; under h.wsum.mu.
func (h *Hub) sumAddr(addr string) map[string]map[string]mailInfo {
	if a, ok := h.wsum.m[addr]; ok {
		return a
	}
	a := map[string]map[string]mailInfo{}
	for _, fq := range h.boxInstances(addr) {
		if m := h.instMail(fq); m != nil {
			a[fq] = m
		}
	}
	h.wsum.m[addr] = a
	return a
}

// mailChanged: inst's mailbox changed (a delivery, a consume or ack, an expiry, its removal); its summary is
// read again from disk, after the change, and its address's follow nudged.
func (h *Hub) mailChanged(inst string) {
	parent, _, ok := instSplit(inst)
	if !ok {
		return
	}
	h.wsum.mu.Lock()
	if a, built := h.wsum.m[parent]; built {
		if m := h.instMailSince(inst, a[inst]); len(m) > 0 {
			a[inst] = m
		} else {
			delete(a, inst)
		}
	}
	h.wsum.mu.Unlock()
	h.follows.nudge(parent)
}

// instMailSince is instMail with the headers of the files in old kept: only new files are read.
func (h *Hub) instMailSince(inst string, old map[string]mailInfo) map[string]mailInfo {
	box := h.mbox(inst)
	out := map[string]mailInfo{}
	for _, n := range globNames(box) {
		if !strings.HasSuffix(n, "."+msgExt) {
			continue
		}
		p := filepath.Join(box, n)
		ino := fileIno(p)
		if mi, ok := old[n]; ok && mi.ino == ino {
			out[n] = mi
			continue
		}
		hdr := headerBlock(p)
		out[n] = mailInfo{hdrGet(hdr, "From"), hdrGet(hdr, "Date"), ino}
	}
	return out
}

func fileIno(p string) uint64 {
	var st unix.Stat_t
	if unix.Lstat(p, &st) != nil {
		return 0
	}
	return st.Ino
}

// sumDropAll forgets every summary.
func (h *Hub) sumDropAll() {
	h.wsum.mu.Lock()
	h.wsum.m = map[string]map[string]map[string]mailInfo{}
	h.wsum.mu.Unlock()
}

// sumDrop forgets addr's summary: its next render builds it from disk again.
func (h *Hub) sumDrop(addr string) {
	h.wsum.mu.Lock()
	delete(h.wsum.m, addr)
	h.wsum.mu.Unlock()
}
