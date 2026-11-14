package hub

// A session is one exec request: a command of an identity (user hub) or of a registrar (user reg), run as a
// goroutine. stdin/stdout/stderr are the SSH channel's; the exit code goes back as exit-status.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

// Session is what the hub knows of a command (identity, serial, key id: set at login,
// unreachable by the client) and its environment (SSH_ORIGINAL_COMMAND, stdio).
type Session struct {
	h      *Hub
	ctx    context.Context // done when the channel or its connection closes
	me     string
	serial string
	keyid  string
	conn   string // the follow id `<conn>.<session>` (Admission.Conn)
	ws     string // the registrar's workspace (user reg)
	raw    string // SSH_ORIGINAL_COMMAND, verbatim

	in     *bufio.Reader
	out    *bufio.Writer // stdout, flushed at every boundary that matters
	rawOut io.Writer     // stdout, unbuffered
	outMu  sync.Mutex    // stdout's writers
	errw   io.Writer
	cio    IO

	temps []*os.File // unlinked temp files holding stdin: their space goes when the session ends
}

// exitPanic unwinds a session to its exit status, as an exit anywhere in it. It never
// crosses a goroutine: only the session's own goroutine calls exit.
type exitPanic struct{ code int }

func (s *Session) exit(code int) { panic(exitPanic{code}) }

// die: a message on stderr, then exit.
func (s *Session) die(code int, format string, args ...any) {
	s.errf(format, args...)
	s.exit(code)
}

func (s *Session) errf(format string, args ...any) {
	fmt.Fprintf(s.errw, format+"\n", args...)
}

func (s *Session) printf(format string, args ...any) {
	s.outMu.Lock()
	fmt.Fprintf(s.out, format, args...)
	s.outMu.Unlock()
}

func (s *Session) println(line string) { s.printf("%s\n", line) }

func (s *Session) flush() error {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	return s.out.Flush()
}

// IO is a session's channel. CloseOut, when set, sends EOF on stdout (a raw live request's end); ConnGone,
// when set, says the client's connection is gone (a drop, where an EOF on the channel alone is an end).
type IO struct {
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	CloseOut func() error
	ConnGone func() bool
}

// closeOut: EOF on the session's stdout, when the channel can say it.
func (s *Session) closeOut() {
	s.flush()
	if s.cio.CloseOut != nil {
		s.cio.CloseOut()
	}
}

// gone: the session's client dropped: its connection ended (in-process, its context: there is no connection).
// A session that closed its channel in order, its connection still up, is not gone.
func (s *Session) gone() bool {
	if s.cio.ConnGone != nil {
		runtime.Gosched() // a dropped connection's EOF and its context's end come together
		return s.cio.ConnGone()
	}
	return s.ctx.Err() != nil
}

func (h *Hub) newSession(ctx context.Context, raw string, cio IO) *Session {
	return &Session{
		h:      h,
		ctx:    ctx,
		raw:    raw,
		in:     bufio.NewReaderSize(cio.Stdin, 64*1024),
		out:    bufio.NewWriterSize(cio.Stdout, 32*1024),
		rawOut: cio.Stdout,
		errw:   cio.Stderr,
		cio:    cio,
	}
}

// run calls f, turning exit panics into the exit code; anything else is a bug: logged, exit 1.
func (s *Session) run(f func()) (code int) {
	defer func() {
		for _, f := range s.temps {
			f.Close()
		}
		if r := recover(); r != nil {
			if e, ok := r.(exitPanic); ok {
				code = e.code
			} else {
				s.h.logf("session %s %q: panic: %v\n%s", s.me, firstN(s.raw, 80), r, debug.Stack())
				fmt.Fprintf(s.errw, "dispatch: internal error\n")
				code = 1
			}
		}
		if err := s.flush(); err != nil && code == 0 {
			code = 1
		}
	}()
	f()
	return 0
}

// Dispatch runs one command of identity me: the client's verb in raw.
func (h *Hub) Dispatch(ctx context.Context, a Admission, raw string, cio IO) int {
	s := h.newSession(ctx, raw, cio)
	s.me, s.serial, s.keyid, s.conn = a.FQ, a.Serial, a.KeyID, a.Conn
	if s.conn == "" { // a call outside the ssh server: a connection of its own
		s.conn = strconv.FormatUint(connSeq.Add(1), 10) + "." + strconv.FormatUint(chanSeq.Add(1), 10)
	}
	return s.run(s.dispatch)
}

// Registrar runs one command of workspace ws's registrar key.
func (h *Hub) Registrar(ctx context.Context, ws, raw string, cio IO) int {
	s := h.newSession(ctx, raw, cio)
	s.ws = ws
	return s.run(s.registrar)
}

// words is `read -r -a A <<< "$RAW"`: the first line, split on runs of spaces and tabs (IFS).
func words(raw string) []string {
	return strings.FieldsFunc(before(raw, "\n"), func(r rune) bool { return r == ' ' || r == '\t' })
}

// claimHeld: the session's key still holds the identity (a rebind to another key, or a release,
// since it logged in refuses it). The same key rebound back keeps its sessions; a session without a
// key id is held to its serial.
func (s *Session) claimHeld() bool {
	want, have := "serial", s.serial
	if s.keyid != "" {
		want, have = "keyid", s.keyid
	}
	for _, f := range s.h.claimFind(s.me) {
		for _, kv := range recPairs(f) {
			if kv[0] == want && kv[1] == have {
				return true
			}
		}
	}
	return false
}

func (s *Session) dispatch() {
	h := s.h
	if !validIdent(s.me) {
		s.die(1, "dispatch: bad identity")
	}
	if !s.claimHeld() {
		s.die(3, "dispatch: %s no longer belongs to this session's key (rebound or released since it logged in); reconnect", s.me)
	}
	// An instance's session also needs its address behind it: a rebind, release or --no-instances
	// of the address ends every instance its holder registered.
	if strings.Contains(s.me, "+") {
		if parent, _, ok := instSplit(s.me); ok && !h.instanceParentOK(h.instRecord(s.me)) {
			s.die(3, "dispatch: %s is no longer an instance of %s (rebound, released, or it takes no instances now); reconnect", s.me, parent)
		}
	}
	// What the session's role may do in each direction (Roles: In, Out).
	role := idRole(s.me)
	row, _ := roleRow(role)
	if row.In != "no" { // a role that receives nothing has no mailbox
		if isInstance(s.me) { // the one mailbox maker; the claim read again under the lock a reject drops it under
			testHook("inst-connect")
			unlock := h.admMu.lock(s.me)
			held := s.claimHeld()
			if held {
				h.instBox(s.me)
			}
			unlock()
			if !held {
				s.die(3, "dispatch: %s no longer belongs to this session's key (rebound or released since it logged in); reconnect", s.me)
			}
		} else {
			os.MkdirAll(h.mbox(s.me), 0o2770)
		}
	}

	a := words(s.raw)
	verb := "recv"
	if len(a) > 0 {
		verb = a[0]
		a = a[1:]
	}
	// A role that receives nothing (a publisher) sends, and says who it is: nothing that needs a
	// mailbox or a reply. A role that sends nothing (the hub's own, never logged in) sends nothing.
	switch verb {
	case "recv", "recv_bag", "recv_ack", "accept", "sub", "unsub", "register", "release", "admit", "reject", "instances", "list", "register_description":
		if row.In == "no" {
			s.die(3, "dispatch: %s is a %s: it receives nothing, so %s is not for it (send, send_bag, id and help are)", s.me, role, verb)
		}
	}
	switch verb {
	case "chat", "send", "send_bag":
		if row.Out == "no" {
			s.die(3, "dispatch: %s is a %s: it sends nothing", s.me, role)
		}
	}
	switch verb {
	case "id":
		s.printf("%s serial=%s\n", s.me, s.serial)
	case "help":
		s.doHelp(a)
	case "register_description":
		s.doRegisterDescription(a)
	case "list":
		s.doList(a)
	case "chat":
		s.doChat(a)
	case "send":
		s.doSend(a)
	case "accept":
		s.doAccept(a)
	case "recv":
		s.doRecv(a)
	case "send_bag":
		s.doSendBag(a)
	case "recv_bag":
		s.doRecvBag(a)
	case "recv_ack":
		s.doRecvAck(a)
	case "sub":
		s.doSub(a)
	case "unsub":
		s.doUnsub(a)
	case "register":
		s.doRegister(a)
	case "release":
		s.doRelease(a)
	case "admit":
		s.doAdmit(a)
	case "reject":
		s.doReject(a)
	case "instances":
		s.doInstances(a)
	default:
		s.die(2, "usage: chat|send|send_bag|recv|recv_bag|recv_ack|accept|sub|unsub|register|release|admit|reject|instances|list|id|help|register_description (see docs)")
	}
}

// sendRefused: this session may not send to target (an identity or an alias; a topic
// and a subscription are judged by their own rules) — a publisher sends to topics only, and an
// identity whose role receives nothing is sent nothing. It says why on stderr.
func (s *Session) sendRefused(target string) bool {
	if idRole(s.me) == "publisher" {
		s.errf("dispatch: %s is a publisher: it sends to topics only, and %s is not one", s.me, target)
		return true
	}
	if receivesNothing(target) {
		s.errf("dispatch: %s is a %s: it receives nothing", target, idRole(target))
		return true
	}
	return false
}

// args is a verb's positional-parameter loop.
type args struct {
	s *Session
	a []string
}

func (p *args) more() bool   { return len(p.a) > 0 }
func (p *args) peek() string { return p.a[0] }
func (p *args) shift() string {
	if len(p.a) == 0 {
		return ""
	}
	v := p.a[0]
	p.a = p.a[1:]
	return v
}

// value takes an option's value; on a last option
// a missing value is a usage error.
func (p *args) value(opt string) string {
	if len(p.a) < 2 {
		p.s.die(2, "dispatch: %s needs a value", opt)
	}
	v := p.a[1]
	p.a = p.a[2:]
	return v
}

func (p *args) unexpected() { p.s.die(2, "dispatch: unexpected arg '%s'", p.a[0]) }
