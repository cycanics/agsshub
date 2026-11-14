package hub

// RFC 9112 chunked framing (docs/live-send.md, Framing), ported from dechunk / live_relay /
// a live reply and a wake's: the same line limits, the same outcomes, the same messages.

import (
	"bufio"
	"bytes"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var hexSizeRE = regexp.MustCompile(`^[0-9A-Fa-f]{1,8}$`)

// readLineN is bash's `IFS= read -r -n n line`: up to n bytes, or up to a newline (consumed, not
// returned). ok is false at EOF before either; line then holds what was read.
func readLineN(r *bufio.Reader, n int) (string, bool) {
	var b []byte
	for len(b) < n {
		c, err := r.ReadByte()
		if err != nil {
			return string(b), false
		}
		if c == '\n' {
			return string(b), true
		}
		b = append(b, c)
	}
	return string(b), true
}

// readLineCapped is `IFS= read -r line` with bounded memory: the line up to its newline (ok=false at
// EOF), at most max bytes of it kept; n is its full length.
func readLineCapped(r *bufio.Reader, max int) (line string, n int, ok bool) {
	var b []byte
	for {
		c, err := r.ReadByte()
		if err != nil {
			return string(b), n, false
		}
		if c == '\n' {
			return string(b), n, true
		}
		n++
		if len(b) < max {
			b = append(b, c)
		}
	}
}

// copyChunk copies size bytes from r to w in pieces, also into dec (up to decKeep bytes) when given.
// short: r ended first (what it had was copied); werr: w failed.
func copyChunk(w io.Writer, r io.Reader, size int64, dec *bytes.Buffer, keep int) (short, werr bool) {
	buf := make([]byte, 32*1024)
	for size > 0 {
		n := int64(len(buf))
		if n > size {
			n = size
		}
		m, err := io.ReadFull(r, buf[:n])
		if m > 0 {
			if _, e := w.Write(buf[:m]); e != nil {
				return false, true
			}
			if dec != nil && dec.Len() < keep {
				k := m
				if dec.Len()+k > keep {
					k = keep - dec.Len()
				}
				dec.Write(buf[:k])
			}
		}
		size -= int64(m)
		if err != nil {
			return true, false
		}
	}
	return false, false
}

// decKeep: the decoded prefix a header-block check needs (FieldMaxBytes and a blank line).
const decKeep = 2*FieldMaxBytes + 2

func chunkSize(line string) (int64, bool) {
	s := before(strings.TrimSuffix(line, "\r"), ";")
	if !hexSizeRE.MatchString(s) {
		return 0, false
	}
	v, _ := strconv.ParseInt(s, 16, 64)
	return v, true
}

func isAbort(line string) (string, bool) {
	name, rest, found := strings.Cut(line, ":")
	if !strings.EqualFold(name, "abort") || len(name) != 5 {
		return "", false
	}
	if !found {
		rest = line
	}
	return rest, true
}

// dechunk decodes a framed body from r into w, at most cap bytes. The body is complete only when the
// last (zero-size) chunk and the end of its trailer arrive; an Abort trailer field is not a message.
// 0 complete · 4 incomplete/aborted · 2 malformed or over cap · 1 w failed; msg is the stderr line.
func dechunk(r *bufio.Reader, w io.Writer, cap int64) (int, string) {
	var total int64
	for {
		line, ok := readLineN(r, 256)
		if !ok {
			return 4, "dispatch: chunked body ended before its last chunk"
		}
		size, ok := chunkSize(line)
		if !ok {
			return 2, "dispatch: bad chunk size line"
		}
		if size == 0 {
			break
		}
		total += size
		if total > cap {
			return 2, "dispatch: body exceeds cap"
		}
		if n, err := io.CopyN(w, r, size); n < size {
			if err != io.EOF {
				return 1, errStore
			}
			return 4, "dispatch: chunked body ended inside a chunk"
		}
		crlf, ok := readLineN(r, 2)
		if !ok {
			return 4, "dispatch: chunked body ended after a chunk"
		}
		if strings.TrimSuffix(crlf, "\r") != "" {
			return 2, "dispatch: chunk data not followed by CRLF"
		}
	}
	for n := 0; ; {
		line, ok := readLineN(r, 1024)
		if !ok {
			return 4, "dispatch: chunked body ended inside its trailer"
		}
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			return 0, ""
		}
		if n++; n > 32 {
			return 2, "dispatch: trailer too long"
		}
		if rest, ab := isAbort(line); ab {
			return 4, "dispatch: sender aborted:" + rest
		}
	}
}

// hdrComplete is hdr_complete over the decoded bytes so far: 0 they start with a complete header
// block, 1 not yet, 2 already over FieldMaxBytes without one.
func hdrComplete(b []byte) int {
	n := 0
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			n += len(b)
			if n > FieldMaxBytes {
				return 2
			}
			return 1
		}
		if i == 0 {
			return 0
		}
		n += i + 1
		if n > FieldMaxBytes {
			return 2
		}
		b = b[i+1:]
	}
}

// relay: copies the framed stream r to w untouched, chunk by chunk, appending the
// decoded bytes to dec when given. With stop it stops (10) at a chunk boundary once dec holds a
// complete header block. 0 the last chunk and its trailer passed · 4 the stream ended before them,
// or the trailer says Abort · 2 malformed or over cap · 7 w broke. total counts decoded bytes.
type relay struct {
	total int64
	cap   int64
	errf  func(string) // the sender's stderr
	wrote int64        // bytes written to w
	// keepAll: dec gets every decoded byte (up to cap), not only a header block's worth: what follows the
	// block in its chunk is forwarded too (D21)
	keepAll bool
	lastAt  int64 // wrote when the last chunk's line began
}

// put writes s to w, counting.
func (lr *relay) put(w io.Writer, s string) error {
	n, err := io.WriteString(w, s)
	lr.wrote += int64(n)
	return err
}

func (lr *relay) run(r *bufio.Reader, w io.Writer, dec *bytes.Buffer, stop bool) int {
	for {
		line, ok := readLineN(r, 256)
		if !ok {
			return 4
		}
		if len(line) >= 256 {
			lr.errf("dispatch: chunk size line too long")
			return 2
		}
		size, ok := chunkSize(line)
		if !ok {
			lr.errf("dispatch: bad chunk size line")
			return 2
		}
		if size == 0 {
			lr.lastAt = lr.wrote
		}
		if err := lr.put(w, line+"\n"); err != nil {
			return 7
		}
		if size == 0 {
			break
		}
		lr.total += size
		if lr.total > lr.cap {
			lr.errf("dispatch: body exceeds cap")
			return 2
		}
		cw := &countWriter{w: w}
		keep := decKeep
		if lr.keepAll {
			keep = int(lr.cap)
		}
		short, werr := copyChunk(cw, r, size, dec, keep)
		lr.wrote += cw.n
		if werr {
			return 7
		}
		if short {
			lr.errf("dispatch: chunked body ended inside a chunk")
			return 4
		}
		crlf, ok := readLineN(r, 2)
		if !ok {
			return 4
		}
		if strings.TrimSuffix(crlf, "\r") != "" {
			lr.errf("dispatch: chunk data not followed by CRLF")
			return 2
		}
		if err := lr.put(w, crlf+"\n"); err != nil {
			return 7
		}
		if !stop {
			continue
		}
		switch hdrComplete(dec.Bytes()) {
		case 0:
			return 10
		case 2:
			lr.errf("dispatch: header block too large")
			return 2
		}
	}
	abort := false
	for n := 0; ; {
		line, ok := readLineN(r, 1024)
		if !ok {
			return 4
		}
		if len(line) >= 1024 {
			lr.errf("dispatch: trailer line too long")
			return 2
		}
		if err := lr.put(w, line+"\n"); err != nil {
			return 7
		}
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			break
		}
		if n++; n > 32 {
			lr.errf("dispatch: trailer too long")
			return 2
		}
		if _, ab := isAbort(line); ab {
			abort = true
		}
	}
	if abort {
		lr.errf("dispatch: the sender aborted")
		return 4
	}
	return 0
}

// replyHeader is the reply's header block: Status (2xx, 4xx, 5xx), optional Content-Type.
// 0 complete · 2 malformed · 7 ended before its blank line. why says what was wrong.
func replyHeader(r *bufio.Reader) (status, ctype string, code int, why string) {
	n := 0
	for {
		line, ln, ok := readLineCapped(r, FieldMaxBytes+1)
		if !ok {
			return "", "", 7, "the recipient ended the exchange without a complete reply"
		}
		if ln == len(line) { // the whole line is here (a longer one is too large whatever it holds)
			line = strings.TrimSuffix(line, "\r")
			ln = len(line)
		}
		if ln == 0 {
			break
		}
		if n += ln + 1; n > FieldMaxBytes {
			return "", "", 2, "reply header block too large"
		}
		k, v, found := strings.Cut(line, ":")
		if !found {
			return "", "", 2, "reply header line without a field name"
		}
		v = strings.TrimPrefix(v, " ")
		switch {
		case strings.EqualFold(k, "status") && len(k) == 6:
			if status != "" || !statusRE.MatchString(v) {
				return "", "", 2, "bad reply Status '" + firstN(v, 32) + "'"
			}
			status = v
		case strings.EqualFold(k, "content-type") && len(k) == 12:
			if ctype != "" || v == "" || hasCntrl(v) {
				return "", "", 2, "bad reply Content-Type"
			}
			ctype = v
		default:
			return "", "", 2, "reply field not allowed: " + firstN(k, 64) + " (allowed: Status, Content-Type)"
		}
	}
	if status == "" {
		return "", "", 2, "reply without Status"
	}
	return status, ctype, 0, ""
}

var statusRE = regexp.MustCompile(`^[245][0-9][0-9]$`)

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func hasCntrl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// replyBody: the chunk-framed body, written decoded to w as it
// arrives. 0 complete · 7 cut short, bad framing, or aborted (why) · 8 w broke (the sender is gone).
func replyBody(r *bufio.Reader, w io.Writer) (int, string) {
	for {
		line, ok := readLineN(r, 256)
		if !ok {
			return 4, "the reply ended before its last chunk"
		}
		size, ok := chunkSize(line)
		if !ok {
			return 7, "bad chunk size line in the reply"
		}
		if size == 0 {
			break
		}
		short, werr := copyChunk(w, r, size, nil, 0)
		if werr {
			return 8, ""
		}
		if short {
			return 4, "the reply ended inside a chunk"
		}
		crlf, ok := readLineN(r, 2)
		if !ok || strings.TrimSuffix(crlf, "\r") != "" {
			return 7, "reply chunk not followed by CRLF"
		}
	}
	for n := 0; ; {
		line, ok := readLineN(r, 1024)
		if !ok {
			return 4, "the reply ended inside its trailer"
		}
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			return 0, ""
		}
		if n++; n > 32 {
			return 7, "reply trailer too long"
		}
		if rest, ab := isAbort(line); ab {
			return 4, "the recipient aborted its reply:" + rest
		}
	}
}

// --- RFC 5322 header blocks (parse_rfc5322) ----------------------------------------------------

// parseRFC5322 reads a header block from r and returns it validated and canonicalised, as stored:
// "<Field>: <value>\n" per field, folded lines kept. The body is what is left in r. A value holding a
// control character other than TAB (a CR, say) is refused: a reader that splits lines at it would see a
// field nobody sent. So is a value that is not valid UTF-8 (D34). NUL bytes are dropped.
// 0 ok · 2 refused (msg on stderr).
func parseRFC5322(r *bufio.Reader) ([]byte, int, string) {
	var hout bytes.Buffer
	n := 0
	canon := ""
	for {
		line, ln, ok := readLineCapped(r, FieldMaxBytes+1)
		if !ok {
			return nil, 2, "dispatch: no blank line — headers must be followed by one"
		}
		if strings.IndexByte(line, 0) >= 0 {
			ln -= strings.Count(line, "\x00")
			line = strings.ReplaceAll(line, "\x00", "")
		}
		if n += ln + 1; n > FieldMaxBytes {
			return nil, 2, "dispatch: header block too large"
		}
		if line == "" {
			return hout.Bytes(), 0, ""
		}
		if line[0] == ' ' || line[0] == '\t' { // folded continuation
			if hout.Len() == 0 {
				return nil, 2, "dispatch: message starts with a folded line"
			}
			if msg := valueFault(line); msg != "" {
				return nil, 2, "dispatch: field " + canon + ": " + msg
			}
			hout.WriteString(line + "\n")
			continue
		}
		name, val, found := strings.Cut(line, ":")
		if !found {
			return nil, 2, "dispatch: not a header line: " + line
		}
		val = strings.TrimPrefix(val, " ")
		var ok2 bool
		if canon, ok2 = fieldCanon(name); !ok2 {
			return nil, 2, "dispatch: field not allowed: " + name + " (allowed: " + strings.Join(clientFields, " ") + ")"
		}
		if msg := valueFault(val); msg != "" {
			return nil, 2, "dispatch: field " + canon + ": " + msg
		}
		hout.WriteString(canon + ": " + val + "\n")
	}
}

// valueFault: why a client header value is refused, "" when it is not: a control character other than
// TAB, or bytes that are not valid UTF-8 (D34: a stored header block is always valid UTF-8).
func valueFault(v string) string {
	switch {
	case !ctlFree(v):
		return "a control character in its value"
	case !utf8.ValidString(v):
		return "its value is not valid UTF-8"
	}
	return ""
}

// ctlFree: no control character but TAB (bytes 0x00-0x1f and 0x7f, as [[:cntrl:]] in the C
// locale).
func ctlFree(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}

// cmdSubst is what "$(cat)" makes of bytes: NULs dropped, trailing newlines removed.
func cmdSubst(b []byte) []byte {
	if bytes.IndexByte(b, 0) >= 0 {
		b = bytes.ReplaceAll(b, []byte{0}, nil)
	}
	return bytes.TrimRight(b, "\n")
}
