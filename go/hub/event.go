package hub

// Event ids (docs/wake-live-event-id.md, Event ids): each publication to a topic gets one (session, seq),
// assigned under the topic's lock from topics/<t>/record; its msgid is event-<session>-<seq>, the same in
// every subscription. done is the last completed fan-out, which the position notice gives a reader.

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// evID is one publication's (session, seq); id "" is none.
type evID struct {
	id  string
	seq int64
}

// EventPrefix starts every topic publication's msgid; HeadPrefix every position notice's.
const (
	EventPrefix = "event-"
	HeadPrefix  = "head-"
)

// msgid: event-<session>-<seq>.
func (e evID) msgid() string { return fmt.Sprintf("%s%s-%d", EventPrefix, e.id, e.seq) }

func (e evID) String() string {
	if e.id == "" {
		return "none"
	}
	return e.msgid()
}

var (
	evIDRE  = regexp.MustCompile(`^[0-9a-f]{16}$`)
	evSeqRE = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	bootRE  = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// parseSessSeq reads `<16hex>-<seq>`: seq decimal from 1, no leading zeros, at most int64 max.
func parseSessSeq(v string) (evID, bool) {
	id, sq, ok := strings.Cut(v, "-")
	if !ok || !evIDRE.MatchString(id) || !evSeqRE.MatchString(sq) {
		return evID{}, false
	}
	n, err := strconv.ParseInt(sq, 10, 64)
	if err != nil {
		return evID{}, false
	}
	return evID{id, n}, true
}

// parseEventMsgid reads an event msgid, event-<session>-<seq>.
func parseEventMsgid(m string) (evID, bool) {
	v, ok := strings.CutPrefix(m, EventPrefix)
	if !ok {
		return evID{}, false
	}
	return parseSessSeq(v)
}

// nextEvent is step 1 of a fan-out: the publication's (session, seq) from the record and the --id (nil
// without one); fresh: the hub's own stamping starts a new session (a hub start on another boot than the
// record's). why is "dup" or "stale: last …" when nothing is to be delivered.
func nextEvent(rec evID, flag *evID, fresh bool) (ev evID, why string) {
	if flag == nil {
		if rec.id == "" || rec.seq == math.MaxInt64 || fresh {
			return evID{randHex(8), 1}, ""
		}
		return evID{rec.id, rec.seq + 1}, ""
	}
	f := *flag
	switch {
	case rec.id == "" || f.id != rec.id || f.seq > rec.seq:
		return f, ""
	case f.seq == rec.seq:
		return evID{}, "dup"
	}
	return evID{}, "stale: last " + rec.String()
}

// topicState is a topic's in-memory part: its lock (held across steps 1-4 of a fan-out and by sub), its
// record and done, read from topics/<t>/record once per topic directory.
type topicState struct {
	pub sync.Mutex // the topic lock

	mu     sync.Mutex // the fields below
	loaded bool
	ino    uint64 // the topic directory's: one removed and added again is read again
	record evID   // the last (session, seq) assigned
	boot   string // the record's boot id: another than the hub's makes the hub's next stamp a new session
	done   evID
}

func (h *Hub) topicSt(t string) *topicState {
	h.tsMu.Lock()
	defer h.tsMu.Unlock()
	st := h.ts[t]
	if st == nil {
		st = &topicState{}
		h.ts[t] = st
	}
	return st
}

func dirIno(p string) uint64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

func (h *Hub) recordPath(t string) string { return filepath.Join(h.topicDir(t), "record") }

// loadTopic reads the record, under st.mu, unless this topic directory's was read already.
func (h *Hub) loadTopic(t string, st *topicState) {
	ino := dirIno(h.topicDir(t))
	if st.loaded && st.ino == ino && (st.record.id == "" || exists(h.recordPath(t))) {
		return // read already; a record gone from under it means the topic was removed (and maybe added again)
	}
	st.loaded, st.ino = true, ino
	st.record, st.boot = evID{}, ""
	b, err := os.ReadFile(h.recordPath(t))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		h.logf("topic %s: record unreadable (%v): none", t, err)
	default:
		ev, boot, ok := parseRecord(b)
		if !ok {
			h.logf("topic %s: record malformed: none until the next publication", t)
			h.journalf("topic %s: record malformed: none", t)
		} else {
			st.record, st.boot = ev, boot
		}
	}
	st.done = st.record
}

// topicDone: the topic's done, the record read first if it was not yet.
func (h *Hub) topicDone(t string) evID {
	st := h.topicSt(t)
	st.mu.Lock()
	defer st.mu.Unlock()
	h.loadTopic(t, st)
	return st.done
}

// testErr: a failure injected at a point of a record write ("record-tmp", "record-rename"); tests set it.
// nil otherwise.
var testErr = func(point string) error { return nil }

// parseRecord reads `<16hex> <seq> <boot_id>\n`.
func parseRecord(b []byte) (evID, string, bool) {
	line, rest, nl := bytes.Cut(b, []byte("\n"))
	f := strings.Fields(string(line))
	if !nl || len(rest) > 0 || len(f) != 3 || !bootRE.MatchString(f[2]) {
		return evID{}, "", false
	}
	ev, ok := parseSessSeq(f[0] + "-" + f[1])
	if !ok {
		return evID{}, "", false
	}
	return ev, f[2], true
}

// writeRecord is step 2: a temp file, then a rename; no fsync (an OS crash can roll it back: the boot id
// makes the next start a new session).
func (h *Hub) writeRecord(t string, ev evID) error {
	td := h.topicDir(t)
	tmp := filepath.Join(td, fmt.Sprintf(".record.%d.%d", os.Getpid(), stageSeq.Add(1)))
	err := testErr("record-tmp")
	if err == nil {
		err = writeFile(tmp, []byte(fmt.Sprintf("%s %d %s\n", ev.id, ev.seq, h.bootID)))
	}
	if err == nil {
		if err = testErr("record-rename"); err == nil {
			err = os.Rename(tmp, filepath.Join(td, "record"))
		}
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// readBootID: the kernel's boot id; a random one, logged, when it cannot be read (every start is then a
// new boot: a new session per topic, never a seq used twice).
func readBootID(logf func(string, ...any)) string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if v := strings.TrimSpace(string(b)); err == nil && bootRE.MatchString(v) {
		return v
	}
	v := randHex(16)
	logf("boot id unreadable (%v): using %s; each hub start is a new session per topic", err, v)
	return v
}

// --- a subscription's mailbox ------------------------------------------------------------------------

// subTopic: the topic a subscription is fed by.
func (h *Hub) subTopic(sub string) string { return recGet(h.subLink(sub), "topic") }

// --- the position notice -------------------------------------------------------------------------------

func headMsgid(d evID) string {
	if d.id == "" {
		return HeadPrefix + "none"
	}
	return fmt.Sprintf("%s%s-%d", HeadPrefix, d.id, d.seq)
}

// headDue: the position notice goes after a subscription's backlog: its mailbox does not hold done.
func (h *Hub) headDue(box string, d evID) bool {
	return d.id == "" || !exists(filepath.Join(box, d.msgid()+"."+msgExt))
}

func (h *Hub) headRaw(topic string) string {
	return fmt.Sprintf("From: %s\nDate: %s\n\n", topic, isoTime(h.epoch()))
}
