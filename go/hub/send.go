package hub

// Sending (README, "Message forms"; docs/wake-live-event-id.md, Message file, Admission, Fan-out): the body
// streams in one pass into a message file staged in inbox/ (inbox/.stage.*), which is no mailbox; only once
// it is whole is a flood token spent or a sender held; then it is hard-linked into the mailbox, the link
// being the commit and the dedup gate.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var sizeRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,11})$`)

// checkSize judges --size before anything is read: its value, when v is well formed and goes with the form.
func (s *Session) checkSize(v, mode, fname, inline string, chunked, live bool) int64 {
	if !sizeRE.MatchString(v) {
		s.die(2, "dispatch: bad --size")
	}
	if chunked || live || inline != "" {
		s.die(2, "dispatch: --size: not with --chunked, --live or an inline message")
	}
	if mode == "" && fname == "" {
		s.die(2, "dispatch: --size needs -m, -m:rfc5322 or --file")
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	if n == 0 && mode != "" {
		s.die(2, "dispatch: --size 0: -m and -m:rfc5322 have nothing to send")
	}
	if n > FileMaxBytes {
		s.die(2, "dispatch: body exceeds cap")
	}
	return n
}

// errStore: a hub I/O error on any form (docs/complete-bodies.md): retryable, nothing stored.
const errStore = "dispatch: cannot store the message: a hub I/O error; retry"

// stdinStream: stdin as the send reads it, its framing or --size checked as it goes (cap: decoded bytes).
func (s *Session) stdinStream(chunked bool, sized int64, cap int64) io.Reader {
	switch {
	case chunked:
		return newChunkReader(s.in, cap)
	case sized >= 0:
		return &sizedReader{r: s.in, n: sized}
	}
	return s.in
}

// streamFail ends a send whose stream failed while the hub parsed it: the stream's own check first (a cut
// inside a header block is 4, not 2), else code and msg.
func (s *Session) streamFail(src io.Reader, code int, msg string) {
	if ie := stickyErr(src); ie != nil {
		s.die(ie.code, "%s", ie.msg)
	}
	s.die(code, "%s", msg)
}

func (s *Session) doSend(a []string) {
	h := s.h
	p := &args{s: s, a: a}
	target := p.shift()
	var mode, fname, msgid, subject, size string
	hasSubject, hasSize, chunked, live := false, false, false, false
	o := &sendOpts{}
loop:
	for p.more() {
		switch p.peek() {
		case "--chunked":
			chunked = true
			p.shift()
		case "--live":
			live = true
			p.shift()
		case "-m":
			mode = "body"
			p.shift()
		case "-m:rfc5322":
			mode = "rfc"
			p.shift()
		case "--file":
			fname = p.value("--file")
		case "--id":
			msgid = p.value("--id")
		case "--subject":
			subject, hasSubject = p.value("--subject"), true
		case "--size":
			size, hasSize = p.value("--size"), true
		case "--wait", "--durable":
			o.parse(p)
		case "--":
			p.shift()
			break loop // the rest is the inline message
		default:
			p.unexpected()
		}
	}
	if target == "" {
		s.die(2, "dispatch: missing target")
	}
	// An inline message is taken from the raw command string, not the split words: the command
	// arrives verbatim (runs of spaces and all), and only word-splitting would lose that.
	inline := ""
	if _, rest, ok := strings.Cut(s.raw, " -- "); ok {
		inline = rest
	}
	chose := 0
	for _, v := range []string{mode, fname, inline} {
		if v != "" {
			chose++
		}
	}
	if chose > 1 {
		s.die(2, "dispatch: use one of -m / -m:rfc5322 / --file / -- <message>")
	}
	if hasSubject {
		if mode == "rfc" {
			s.die(2, "dispatch: --subject: with -m:rfc5322 the header block carries Subject")
		}
		s.checkSubject(subject)
	}
	sizeN := int64(-1)
	if hasSize {
		sizeN = s.checkSize(size, mode, fname, inline, chunked, live)
	}
	if live && (o.durable || o.hasWait) {
		s.die(2, "dispatch: --durable and --wait are a spooled send's; --live stores nothing")
	}
	if live {
		if receivesNothing(s.me) {
			s.die(9, "dispatch: %s is a %s: it receives nothing, so it sends no --live (that waits for a reply)", s.me, idRole(s.me))
		}
		s.sendLive(target, mode, fname, msgid, inline, subject, chunked) // exits
	}
	if chunked && inline != "" {
		s.die(2, "dispatch: --chunked frames stdin; an inline message has none")
	}
	if fname != "" && !validFile(fname) {
		s.die(2, "dispatch: unsafe filename")
	}
	if chose == 0 {
		s.die(2, "dispatch: nothing to send (need -m or --file)")
	}
	typ, addr, ok := s.resolveTarget(target)
	if !ok {
		s.die(3, "dispatch: no such target %s", target)
	}
	if typ == "sub" {
		s.die(3, "dispatch: %s is a subscription; publish to its topic", target)
	}
	flag := s.checkSendID(msgid, typ == "topic")
	o.judge(target, typ)
	if typ != "topic" && s.sendRefused(target) {
		s.exit(3)
	}
	if typ == "topic" && !s.mayPublish(target) {
		s.exit(3)
	}
	src := s.stdinStream(chunked, sizeN, FileMaxBytes)
	var fields []byte
	if mode == "rfc" {
		br := bufio.NewReaderSize(src, 8*1024)
		hdr, code, msg := parseRFC5322(br)
		if code != 0 {
			s.streamFail(src, code, msg)
		}
		fields = hdr
		src = br
	}
	if subject != "" {
		fields = subjectField(subject)
	}
	if why := gapProblem(fields, typ); why != "" {
		s.die(2, "dispatch: %s", why)
	}
	spec := &msgSpec{kind: "text", from: s.me, fields: fields, gap: hasGap(fields)}
	switch {
	case fname != "":
		spec.kind, spec.fname, spec.src = "file", fname, src
	case mode != "":
		spec.src = src
	default:
		spec.inline = []byte(inline)
	}
	if typ == "topic" {
		s.exit(s.topicPublish(target, spec, flag))
	}
	if typ != "native" {
		s.exit(s.sendAlias(typ, addr, target, spec))
	}
	if msgid == "" {
		msgid = h.mintMsgid()
	}
	file := ""
	if fname != "" {
		file = " file=" + fname
	}
	spec.durable = o.durable
	if s.spool(target, msgid, spec, o) {
		s.printf("sent -> %s msgid=%s%s\n", target, msgid, file)
	}
}

func (s *Session) doSendBag(a []string) {
	h := s.h
	p := &args{s: s, a: a}
	target := p.shift()
	var msgpath, msgid, subject string
	hasSubject, chunked := false, false
	o := &sendOpts{}
	for p.more() {
		switch p.peek() {
		case "--wait", "--durable":
			o.parse(p)
		case "--msg":
			msgpath = p.value("--msg")
		case "--id":
			msgid = p.value("--id")
		case "--subject":
			subject, hasSubject = p.value("--subject"), true
		case "--chunked":
			chunked = true
			p.shift()
		default:
			p.unexpected()
		}
	}
	if target == "" {
		s.die(2, "dispatch: missing target")
	}
	if msgpath != "" && (bagName(msgpath) == "" || len(msgpath) > msgPathMax || valueFault(msgpath) != "") {
		s.die(2, "dispatch: bad --msg path")
	}
	if hasSubject {
		s.checkSubject(subject)
	}
	typ, _, ok := s.resolveTarget(target)
	if !ok {
		s.die(3, "dispatch: no such target %s", target)
	}
	if typ == "sub" {
		s.die(3, "dispatch: %s is a subscription; publish to its topic", target)
	}
	if typ != "native" && typ != "topic" {
		s.die(2, "dispatch: send_bag is native-only (target is %s)", typ)
	}
	if typ != "topic" && s.sendRefused(target) {
		s.exit(3)
	}
	if typ == "topic" && !s.mayPublish(target) {
		s.exit(3)
	}
	flag := s.checkSendID(msgid, typ == "topic")
	o.judge(target, typ)
	var fields []byte
	if subject != "" {
		fields = subjectField(subject)
	}
	spec := &msgSpec{kind: "bag", from: s.me, fields: fields, msgpath: msgpath, src: s.stdinStream(chunked, -1, BagMaxBytes)}
	if typ == "topic" {
		s.exit(s.topicPublish(target, spec, flag))
	}
	if msgid == "" {
		msgid = h.mintMsgid()
	}
	spec.durable = o.durable
	if s.spool(target, msgid, spec, o) {
		s.printf("sent bag -> %s msgid=%s\n", target, msgid)
	}
}

// sendOpts: --wait and --durable.
type sendOpts struct {
	s       *Session
	hasWait bool
	waitStr string
	wait    time.Duration
	durable bool
}

func (o *sendOpts) parse(p *args) {
	o.s = p.s
	switch p.peek() {
	case "--durable":
		o.durable = true
		p.shift()
	case "--wait":
		o.waitStr, o.hasWait = p.value("--wait"), true
		d, ok := parseWait(o.waitStr)
		if !ok {
			p.s.die(2, "dispatch: bad --wait: seconds, 0.001 to 600, up to 3 decimals")
		}
		o.wait = d
	}
}

// judge: --wait goes to an identity or an instance only (exit 2); --durable on a topic or an alias is ignored,
// and says so (support needs a hub inbox, not built yet).
func (o *sendOpts) judge(target, typ string) {
	if o.s == nil {
		return
	}
	if o.hasWait && (typ != "native" || receivesNothing(target)) {
		o.s.die(2, "dispatch: --wait waits for an identity's or an instance's pickup; %s is none", target)
	}
	if o.durable && typ != "native" {
		o.s.errf("durable: ignored")
		o.durable = false
	}
}

// spool stores spec for target, an identity or an instance: true stored, the caller says so; false answered
// already (a dup, or --wait's outcome). Every other outcome exits: 3 a rejected instance, 5 rate limited,
// 6 no answer and no mailbox, 8 stored, not picked (--wait, or no answer with its mailbox there), 1 a hub I/O
// error, 2 or 4 the body.
func (s *Session) spool(target, msgid string, spec *msgSpec, o *sendOpts) bool {
	h := s.h
	inst := isInstance(target)
	box := h.mbox(target)
	if exists(filepath.Join(box, msgid+"."+msgExt)) {
		s.printf("dup %s (dropped)\n", msgid)
		return false
	}
	stage := h.newStage()
	if rc := s.writeMessage(stage, spec); rc != 0 {
		s.exit(rc)
	}
	defer os.Remove(stage) // the mailbox's link stays; a crash leaves the stage to the reaper
	if !h.floodTake(s.me, target) {
		s.die(5, "dispatch: rate limited (%s -> %s: %s); slow down", s.me, target, h.budgetShow(s.me))
	}
	made := !isDir(box) // the send makes its mailbox (an admit, a first message): --durable syncs inbox/
	noAnswer := false   // the ask ended unanswered, its mailbox there: stored, exit 8, no wait
	if inst {
		noAnswer = s.admitSendOrExit(target, o.hasWait) // flows A, A1, A2
	} else if err := os.MkdirAll(box, 0o2770); err != nil {
		s.die(1, "%s", errStore)
	}
	if inst {
		testHook("inst-staged")
	}
	var w *waiter
	defer func() {
		if w != nil {
			h.waitDrop(box, msgid, w)
		}
	}()
	// before the link, under the mailbox lock: a pickup cannot slip between it and the wait, and a reject
	// that renames the mailbox away finds the waiter with the mail it discards
	before := func() {
		if o.hasWait && !noAnswer {
			w = h.waitAdd(box, msgid)
		}
	}
	switch rc := s.link(box, msgid, stage, target, inst, before); rc {
	case 0:
	case 10:
		return false // a dup: no wait
	default:
		s.exit(rc)
	}
	if spec.durable { // the file was fsynced as it was written; now the names
		err := syncPath(box)
		if err == nil && made {
			err = syncPath(h.inbox)
		}
		if err != nil {
			if inst && !isDir(box) { // a reject took the mailbox, and this message with it
				s.die(3, errInstGone, target)
			}
			s.die(1, "dispatch: stored, but not made durable: a hub I/O error")
		}
	}
	h.delivered(target)
	if noAnswer {
		w, _ := h.wakeSecs()
		s.println("stored, not picked: no answer within " + w + " s")
		s.exit(8)
	}
	if !o.hasWait {
		return true
	}
	t := time.NewTimer(o.wait)
	defer t.Stop()
	select {
	case <-w.picked:
		s.println("picked")
	case <-w.gone: // a reject discarded the mailbox and this message with it
		s.die(3, errInstGone, target)
	case <-t.C:
		s.println("stored, not picked within " + o.waitStr)
		s.exit(8)
	case <-s.ctx.Done():
		s.exit(8) // nobody hears it
	}
	return false
}

// newStage: a fresh name for a message file staged in inbox/.
func (h *Hub) newStage() string {
	os.MkdirAll(h.inbox, 0o2770)
	return h.stageName(h.inbox)
}

// link is the commit: the staged message, stamped with its arrival, hard-linked into box as <msgid>.msg. An
// instance's mailbox (inst) under its mailbox lock, after the mailbox is checked again; before runs just
// before the link, under that lock. 0 stored · 10 dup (said on stdout) · 3 the mailbox gone · 1 a hub I/O error.
func (s *Session) link(box, msgid, stage, target string, inst bool, before func()) int {
	h := s.h
	msgfile := filepath.Join(box, msgid+"."+msgExt)
	if inst {
		unlock := lockBox(box)
		if unlock == nil {
			if isDir(box) { // there, its lock not to be had: the hub's I/O, not a gone target
				s.errf(errStore)
				return 1
			}
			s.errf(errInstGone, target)
			return 3
		}
		defer unlock()
	}
	before()
	defer h.arrMu.lock(box)() // the stamp and the link together: one that lands later sorts later
	if err := h.stamp(stage); err != nil {
		s.errf(errStore)
		return 1
	}
	if err := os.Link(stage, msgfile); err != nil {
		if errors.Is(err, fs.ErrExist) {
			s.printf("dup %s (dropped)\n", msgid)
			return 10
		}
		s.errf(errStore)
		return 1
	}
	return 0
}

// delivered: a message landed in target's mailbox: wake its follower, and an instance's address, whose wake
// notice changed.
func (h *Hub) delivered(target string) {
	h.follows.nudge(target)
	if isInstance(target) {
		h.mailChanged(target)
	}
}

// --- aliases -------------------------------------------------------------------------------------------

// sendAlias hands a send to an alias's backend: the body read whole into HUB_BODY, a file onto its stdin
// through an unlinked temp file, so its end is known before the token is spent.
func (s *Session) sendAlias(typ, addr, target string, spec *msgSpec) int {
	h := s.h
	body := spec.inline
	var in io.Reader = s.in
	switch {
	case spec.kind == "file":
		f := s.stdinTemp()
		if _, err := io.Copy(f, &capReader{r: spec.src, left: FileMaxBytes, what: "file"}); err != nil {
			s.copyFail(err)
		}
		f.Seek(0, io.SeekStart)
		in = f
	case spec.src != nil:
		b, err := io.ReadAll(&capReader{r: spec.src, left: FileMaxBytes, what: "body"})
		if err != nil {
			s.copyFail(err)
		}
		body = cmdSubst(b)
		if len(body) == 0 {
			s.die(2, "dispatch: nothing to send (need -m or --file)")
		}
	}
	if !h.floodTake(s.me, target) {
		s.die(5, "dispatch: rate limited (%s -> %s: %s); slow down", s.me, target, h.budgetShow(s.me))
	}
	return s.routeBackend(typ, addr, target, body, spec.fname, hdrGet(strings.Split(string(spec.fields), "\n"), "Subject"), in)
}

// copyFail ends a send whose stdin copy failed: the stream's check, or the hub's I/O.
func (s *Session) copyFail(err error) {
	var ie *inErr
	if errors.As(err, &ie) {
		s.die(ie.code, "%s", ie.msg)
	}
	s.die(1, "%s", errStore)
}

// routeBackend hands a send to an alias's backend plugin (backends/<type>/send), exiting with its
// status; without one, the seam answers 40.
func (s *Session) routeBackend(typ, addr, target string, body []byte, fname, subject string, in io.Reader) int {
	plugin := filepath.Join(s.h.cfg.Lib, "backends", typ, "send")
	if fi, err := os.Stat(plugin); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
		s.flush()
		cmd := exec.CommandContext(s.ctx, plugin)
		cmd.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND="+s.raw, "HUB_FROM="+s.me, "HUB_TARGET="+target,
			"HUB_ADDR="+addr, "HUB_BODY="+string(body), "HUB_FILE="+fname, "HUB_SUBJECT="+subject)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = in, s.rawOut, s.errw
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		if err != nil {
			return 1
		}
		return 0
	}
	s.errf("dispatch: backend '%s' not implemented (target=%s addr=%s)", typ, target, addr)
	return 40
}

// stdinTemp: an unlinked temp file; its space goes when the session ends.
func (s *Session) stdinTemp() *os.File {
	h := s.h
	os.MkdirAll(h.inbox, 0o2770)
	tmp := filepath.Join(h.inbox, fmt.Sprintf(".chunked.%d.%d", os.Getpid(), stageSeq.Add(1)))
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		s.die(1, errStore)
	}
	os.Remove(tmp)
	s.temps = append(s.temps, f)
	return f
}
