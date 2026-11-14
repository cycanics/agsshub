package hub

// Topics and subscriptions (docs/topics.md). Functions return
// codes rather than exiting, so chat can publish a line and carry on.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (h *Hub) topicDir(t string) string  { return filepath.Join(h.topics, fsName(t)) }
func (h *Hub) subLink(sub string) string { return filepath.Join(h.subs, fsName(sub)) }

func (h *Hub) isTopic(t string) bool {
	return idRole(t) == "topic" && isFile(filepath.Join(h.topicDir(t), "policy"))
}

func (h *Hub) isSub(t string) bool { return idRole(t) == "sub" && isFile(h.subLink(t)) }

func (s *Session) subOwned(sub string) bool {
	return s.h.isSub(sub) && recGet(s.h.subLink(sub), "owner") == s.me
}

// policyAllows: id matches one of the shell patterns under key in the policy (else def's).
func policyAllows(pol, key, id, def string) bool {
	pats := recGet(pol, key)
	if pats == "" {
		pats = def
	}
	for _, p := range words(pats) {
		if bashMatch(p, id) {
			return true
		}
	}
	return false
}

// subMint: sub.<project>.<name>+<rand> for topic.<project>+<name>; sub.<name>+<rand> for topic+<name>.
func subMint(topic string) string {
	n := idName(topic)
	head := before(n, "+")
	inst, _ := after(n, "+")
	proj := ""
	if p, ok := after(head, "."); ok {
		proj = p + "."
	}
	return fmt.Sprintf("sub.%s%s+%s@%s%s", proj, inst, randHex(4), idOrg(topic), idSuffix)
}

// mayPublish: the policy's word, said on stderr when it is no: before a body is read.
func (s *Session) mayPublish(topic string) bool {
	if policyAllows(filepath.Join(s.h.topicDir(topic), "policy"), "publish", s.me, "") {
		return true
	}
	s.errf("dispatch: %s may not publish to %s", s.me, topic)
	return false
}

// topicPublish: 0 published: stored once and linked into each subscription mailbox that takes it, subs=
// counting those; or dup / stale with nothing delivered · 1 the message cannot be stored, or its record
// written (a hub I/O error) · 3 not allowed · 5 rate limited · else the store's refusal. flag is the --id
// event-<session>-<seq>, nil without one.
func (s *Session) topicPublish(topic string, spec *msgSpec, flag *evID) int {
	h := s.h
	say := func(format string, a ...any) { s.printf(format, a...) }
	td := h.topicDir(topic)
	pol := filepath.Join(td, "policy")
	if !s.mayPublish(topic) {
		return 3
	}
	pr, _ := readRec(pol)
	rate, burst := int64(TopicRate), int64(TopicBurst)
	if topicNumR.MatchString(pr["rate"]) {
		rate, _ = strconv.ParseInt(pr["rate"], 10, 64)
	}
	if topicNumR.MatchString(pr["burst"]) {
		burst, _ = strconv.ParseInt(pr["burst"], 10, 64)
	}
	if flag != nil { // 0: a dup or stale answers before the body is staged (and again under the lock)
		pre := h.topicSt(topic)
		pre.mu.Lock()
		h.loadTopic(topic, pre)
		_, why := nextEvent(pre.record, flag, false)
		pre.mu.Unlock()
		if why != "" {
			say("%s\n", why)
			return 0
		}
	}
	// 0: the message stored once, outside the topic lock, in a stage that is no mailbox; removed on each way
	// out below, not by a defer: a crash leaves it to the reaper
	stage := h.newStage()
	if rc := s.writeMessage(stage, spec); rc != 0 {
		return rc
	}
	if !h.floodTake(s.me, topic, rate*1000, burst*1000) { // once the body is whole: a cut or a dup costs none
		os.Remove(stage)
		s.errf("dispatch: rate limited (publisher->topic); slow down")
		return 5
	}
	testHook("stored")
	st := h.topicSt(topic)
	st.pub.Lock() // steps 1-4
	locked := time.Now()
	testHook("locked")
	defer func() { lockHeld(topic, time.Since(locked)); st.pub.Unlock() }()
	// 1: the publication's (session, seq)
	if !exists(stage) { // reaped while it waited for the lock (an hour): nothing assigned, retryable
		s.errf(errStore)
		return 1
	}
	st.mu.Lock()
	h.loadTopic(topic, st)
	ev, why := nextEvent(st.record, flag, st.boot != h.bootID)
	st.mu.Unlock()
	if why != "" {
		os.Remove(stage)
		say("%s\n", why)
		return 0
	}
	// 2: the record, before any link: a crash leaves a seq unused, never one given twice
	if err := h.writeRecord(topic, ev); err != nil {
		h.logf("topic %s: record %s not written: %v", topic, ev, err)
		h.journalf("publish %s %s from %s: record not written", topic, ev, s.me)
		os.Remove(stage)
		s.errf(errStore)
		return 1
	}
	st.mu.Lock()
	st.record, st.boot = ev, h.bootID
	st.mu.Unlock()
	testHook("recorded")
	// 3: one link per subscription
	n, failed := h.fanout(td, stage, ev.msgid())
	testHook("fanned")
	// 4
	os.Remove(stage)
	st.mu.Lock()
	st.done = ev
	st.mu.Unlock()
	fails := ""
	if failed > 0 {
		fails = fmt.Sprintf(" failed=%d", failed)
	}
	h.journalf("publish %s %s from %s subs=%d%s", topic, ev, s.me, n, fails)
	say("published -> %s msgid=%s subs=%d\n", topic, ev, n)
	return 0
}

// lockHeld: how long a publication held its topic's lock; tests measure it. A no-op otherwise.
var lockHeld = func(topic string, d time.Duration) {}

// testHook: called at each point of a publication ("stage", "stored", "locked", "stamped", "recorded",
// "deliver", "linked", "fanned"), of
// an instance store ("inst-staged") and of an ask ("held"); tests stop there, as a crash would. A no-op
// otherwise. Also "tree", after each file of a bag's tree is linked.
var testHook = func(point string) {}

// fanout: the staged message file src into each subscription's mailbox as <name>.msg: the deliveries made,
// and the links that failed. Each delivered one is nudged.
func (h *Hub) fanout(td, src, name string) (n, failed int) {
	subsDir := filepath.Join(td, "subs")
	for _, e := range globNames(subsDir) {
		rp := filepath.Join(subsDir, e)
		if !isFile(rp) {
			continue
		}
		sub := recGet(rp, "sub")
		if sub == "" { // a record half-written
			continue
		}
		dst := h.mbox(sub)
		delivered, bad := h.deliverToSub(src, dst, name)
		if bad {
			failed++
		}
		if !delivered {
			continue
		}
		n++
		h.follows.nudge(sub)
	}
	return n, failed
}

// deliverToSub links the staged message file src into the subscription's mailbox dst as <name>.msg, its
// payload inside it, under the mailbox lock. failed: the link failed, or the name is taken already; the
// publication's seq is used, so that reader sees a gap. A mailbox that is gone (unsubscribed or reaped
// while the publication ran) takes nothing and is no failure: it is never made again here.
func (h *Hub) deliverToSub(src, dst, name string) (delivered, failed bool) {
	testHook("deliver")
	unlock := lockBox(dst)
	if unlock == nil {
		return false, isDir(dst) // gone: no failure; there, without its lock to be had: failed
	}
	defer unlock()
	// stamped with its arrival with the link, under the mailbox's arrival lock; the topic's lock orders the
	// publications, so each stamp is later than the last's. One file in every mailbox: its last stamp
	// counts, and it keeps the reaper off the stage while the fan-out goes on. Unstamped, it would carry
	// its write time, from before the topic's lock: not linked, a gap instead
	defer h.arrMu.lock(dst)()
	if h.stamp(src) != nil {
		return false, true
	}
	if err := os.Link(src, filepath.Join(dst, name+"."+msgExt)); err != nil {
		return false, isDir(dst)
	}
	testHook("linked")
	return true, false
}

// dropMsg removes a message: its one file.
func dropMsg(box, mid string) { os.Remove(filepath.Join(box, mid+"."+msgExt)) }

// topicSub prints the subscription name: the same owner and topic get the same name back, renewed; else a
// new one.
func (s *Session) topicSub(topic string, ttl int64) int {
	h := s.h
	td := h.topicDir(topic)
	if !policyAllows(filepath.Join(td, "policy"), "subscribe", s.me, "*@"+idOrg(topic)+idSuffix) {
		s.errf("dispatch: %s may not subscribe to %s", s.me, topic)
		return 3
	}
	st := h.topicSt(topic)
	st.pub.Lock() // after the fan-out under way: a new subscription gets publications from the next one
	defer st.pub.Unlock()
	now := h.epoch()
	exp := now + ttl
	subsDir := filepath.Join(td, "subs")
	os.MkdirAll(subsDir, 0o2770)
	os.MkdirAll(h.subs, 0o2770)
	for _, e := range globNames(subsDir) {
		rp := filepath.Join(subsDir, e)
		r, ok := readRec(rp)
		if !ok || r["owner"] != s.me {
			continue
		}
		rewriteKey(rp, "expires", strconv.FormatInt(exp, 10))
		if r["sub"] != "" && isFile(rp) { // what a race with the reaper left of it is made whole: its link and mailbox
			h.subPlace(topic, r["sub"])
		}
		s.println(r["sub"])
		return 0
	}
	var sub string
	for tries := 0; ; tries++ {
		if tries >= 5 {
			s.errf("dispatch: could not mint a subscription name")
			return 1
		}
		sub = subMint(topic)
		rp := filepath.Join(subsDir, fsName(sub))
		tmp := filepath.Join(subsDir, fmt.Sprintf(".new.%d.%d", os.Getpid(), stageSeq.Add(1)))
		os.WriteFile(tmp, []byte(fmt.Sprintf("sub=%s\ntopic=%s\nowner=%s\ncreated=%d\nexpires=%d\n",
			sub, topic, s.me, now, exp)), 0o640)
		err := os.Link(tmp, rp)
		os.Remove(tmp)
		if err == nil {
			break
		}
	}
	h.subPlace(topic, sub)
	h.journalf("sub %s topic=%s owner=%s", sub, topic, s.me)
	s.println(sub)
	return 0
}

// subPlace gives a subscription whose record is in place its link and its mailbox, those it lacks; under
// the topic's lock.
func (h *Hub) subPlace(topic, sub string) {
	if link := h.subLink(sub); !isFile(link) {
		os.Remove(link)
		os.Symlink("../topics/"+fsName(topic)+"/subs/"+fsName(sub), link)
	}
	os.MkdirAll(h.mbox(sub), 0o2770)
}

// rewriteKey is `sed "s/^key=.*/key=v/"` through a temp file and a rename.
func rewriteKey(path, key, v string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.SplitAfter(string(b), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, key+"=") {
			nl := ""
			if strings.HasSuffix(l, "\n") {
				nl = "\n"
			}
			lines[i] = key + "=" + v + nl
		}
	}
	tmp := fmt.Sprintf("%s.%d", path, os.Getpid())
	if os.WriteFile(tmp, []byte(strings.Join(lines, "")), 0o640) == nil {
		os.Rename(tmp, path)
	}
}

func (s *Session) topicUnsub(sub string) int {
	h := s.h
	if !s.subOwned(sub) {
		s.errf("dispatch: not your subscription: %s", sub)
		return 3
	}
	if rp, err := filepath.EvalSymlinks(h.subLink(sub)); err == nil {
		os.Remove(rp)
	}
	os.Remove(h.subLink(sub))
	h.dropSubBox(h.mbox(sub))
	h.follows.nudge(sub)
	h.journalf("unsub %s owner=%s", sub, s.me)
	s.printf("unsubscribed %s\n", sub)
	return 0
}

// topicList: the topics, with what the caller may do on each.
func (s *Session) topicList() {
	h := s.h
	for _, d := range globNames(h.topics) {
		pol := filepath.Join(h.topics, d, "policy")
		if !isFile(pol) {
			continue
		}
		t := recGet(pol, "topic")
		if t == "" {
			continue
		}
		pub, subok := "no", "no"
		if policyAllows(pol, "publish", s.me, "") {
			pub = "yes"
		}
		if policyAllows(pol, "subscribe", s.me, "*@"+idOrg(t)+idSuffix) {
			subok = "yes"
		}
		s.printf("%s\ttype=topic\tpublish=%s\tsubscribe=%s\n", t, pub, subok)
	}
}

// subList: the caller's own subscriptions.
func (s *Session) subList() {
	h := s.h
	for _, n := range globNames(h.subs) {
		l := filepath.Join(h.subs, n)
		if !isFile(l) {
			continue
		}
		r, _ := readRec(l)
		if r["owner"] != s.me {
			continue
		}
		exp := ""
		if e, ok := atoi64(r["expires"]); ok {
			exp = isoTime(e)
		}
		s.printf("%s\ttype=sub\ttopic=%s\texpires=%s\n", r["sub"], r["topic"], exp)
	}
}

func (s *Session) doSub(a []string) {
	p := &args{s: s, a: a}
	topic := p.shift()
	ttl := strconv.Itoa(SubTTLSecs)
	for p.more() {
		switch p.peek() {
		case "--live", "--wake":
			s.die(2, "dispatch: sub %s is gone: sub has one behaviour; its follow is woken by each delivery", p.peek())
		case "--ttl":
			ttl = p.value("--ttl")
		default:
			p.unexpected()
		}
	}
	if topic == "" {
		s.die(2, "dispatch: missing topic")
	}
	if !topicNumR.MatchString(ttl) {
		s.die(2, "dispatch: bad --ttl")
	}
	t, err := strconv.ParseInt(ttl, 10, 64)
	if err != nil || t > SubTTLMax {
		t = SubTTLMax
	}
	if t < 60 {
		t = 60
	}
	if !s.h.isTopic(topic) {
		s.die(3, "dispatch: no such topic %s", topic)
	}
	s.exit(s.topicSub(topic, t))
}

func (s *Session) doUnsub(a []string) {
	if len(a) == 0 || a[0] == "" {
		s.die(2, "dispatch: missing subscription")
	}
	s.exit(s.topicUnsub(a[0]))
}
