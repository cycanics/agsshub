package hub

// Expiry notices and --subject (docs/expiry-and-subject.md). The notices themselves are go/expiry's; here
// is what the hub does with them: the reaper's three steps, and showing and clearing in recv.

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/cycanics/agsshub/go/descriptor"
	"github.com/cycanics/agsshub/go/expiry"
)

// subjectField is a Subject as a stored client field.
func subjectField(verb string) []byte { return []byte("Subject: " + verb + "\n") }

// checkSubject exits 2 for a --subject outside its grammar.
func (s *Session) checkSubject(v string) {
	if !subjectRE.MatchString(v) {
		s.die(2, "dispatch: bad --subject: one word, an alphanumeric, then [A-Za-z0-9._/-], 64 at most")
	}
}

// checkSendID judges a send's --id: exit 2 for a bad one, one of the hub's (expired-*, wakes-*, head-*), and
// event-* anywhere but on a publication. On a publication (topic) it must be event-<session>-<seq>: the flag
// it returns, nil without an --id.
func (s *Session) checkSendID(id string, topic bool) *evID {
	if id == "" {
		return nil
	}
	if !validMsgid(id) {
		s.die(2, "dispatch: bad --id")
	}
	for _, p := range []string{expiry.Prefix, WakePrefix, HeadPrefix} {
		if strings.HasPrefix(id, p) {
			s.die(2, "dispatch: bad --id: %s* is the hub's", p)
		}
	}
	if !topic {
		if strings.HasPrefix(id, EventPrefix) {
			s.die(2, "dispatch: bad --id: %s* is a topic publication's msgid", EventPrefix)
		}
		return nil
	}
	ev, ok := parseEventMsgid(id)
	if !ok {
		s.die(2, "dispatch: bad --id: a publication's is event-<16 lower-case hex>-<seq from 1, no leading zeros>")
	}
	return &ev
}

func (h *Hub) expiryOptions() expiry.Options {
	return expiry.Options{Now: h.now, Logf: func(format string, a ...any) {
		h.logf(format, a...)
		h.journalf(format, a...)
	}}
}

// noticeProbe: the operator's notice_probe setting names this identity. A pattern outside --see's alphabet
// turns the whole setting off.
func (h *Hub) noticeProbe(me string) bool {
	ps := strings.FieldsFunc(recGet(h.settings, "notice_probe"), func(r rune) bool { return r == ' ' })
	if len(ps) == 0 || descriptor.CheckPatterns(ps) != nil {
		return false
	}
	for _, p := range ps {
		if bashMatch(p, me) {
			return true
		}
	}
	return false
}

// boxReadable: someone can read this mailbox: a claim, live or lapsed; a subscription; an instance whose
// address takes instances. Nothing else gets a notice: not a topic's staged copies, not a removed identity.
func (h *Hub) boxReadable(name string) bool {
	if len(h.claimFind(name)) > 0 || h.isSub(name) {
		return true
	}
	if parent, _, isInst := instSplit(name); isInst {
		return h.instCapable(parent)
	}
	return false
}

// expireMail takes one mailbox's expired messages (msgids) through the three steps; leftover claims of a
// sweep that crashed join them.
func (h *Hub) expireMail(name, box string, old []string) {
	xd := filepath.Join(box, expiry.Dir)
	if len(old) == 0 && !isDir(xd) {
		return
	}
	if !h.boxReadable(name) { // mail nobody can read goes as it always did, its notices too
		// under the registry lock: judged against a claim registry no operator tool is changing
		unlock, err := h.lockRegistry()
		if err != nil {
			return
		}
		defer unlock()
		if h.boxReadable(name) {
			return
		}
		os.RemoveAll(xd)
		for _, mid := range old {
			os.Remove(filepath.Join(box, mid+"."+msgExt))
		}
		return
	}
	o := h.expiryOptions()
	if len(old) > 0 {
		failed := 0
		err := expiry.EnsureDir(box)
		for _, mid := range old {
			claim := filepath.Join(xd, expiry.ClaimPrefix+mid)
			if err != nil {
				failed++
			} else if exists(claim) { // a leftover of that name goes first
				continue
			} else if os.Rename(filepath.Join(box, mid+"."+msgExt), claim) != nil { // a reader took it, or no write here
				failed++
			}
		}
		if failed > 0 {
			o.Logf("reap: cannot claim %d of %d expired messages in %s", failed, len(old), name)
		}
	}
	testHook("claimed")
	done, err := expiry.Count(box, o)
	if err != nil {
		o.Logf("reap: counting %s: %v; what could not be counted stays claimed", name, err)
	}
	if len(done) > 0 {
		h.follows.nudge(name)
	}
	if len(old) > 0 {
		h.mailChanged(name) // an instance's mail went: its address's wake notice
	}
	testHook("counted")
	for _, c := range done {
		os.Remove(filepath.Join(xd, c))
	}
	testHook("deleted")
}
