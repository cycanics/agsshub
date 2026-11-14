package hub

// The message file (docs/wake-live-event-id.md, Message file): one file per stored message, written in one
// pass in file order: the header block, the body, the payload (a --file's bytes or a bag's tar, exactly as
// received), and a fixed 212-byte trailer that says where each part is.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// trailerSize: the trailer's bytes: Header-Size 34, Message-Offset 37, Message-Size 35, Total-Size 33, Sha256 73.
const trailerSize = 212

// layout is what a trailer says.
type layout struct{ hdr, msgOff, msgSize, total int64 }

func (l layout) trailer() []byte {
	return []byte(fmt.Sprintf("Header-Size: %020d\nMessage-Offset: %020d\nMessage-Size: %020d\nTotal-Size: %020d\nSha256: %064d\n",
		l.hdr, l.msgOff, l.msgSize, l.total, 0))
}

var trailerNames = []string{"Header-Size: ", "Message-Offset: ", "Message-Size: ", "Total-Size: "}

// parseTrailer reads a trailer, every line in its place and width; false when it does not parse or its
// offsets do not fit the file it says it ends.
func parseTrailer(b []byte) (layout, bool) {
	if len(b) != trailerSize {
		return layout{}, false
	}
	var v [4]int64
	off := 0
	for i, name := range trailerNames {
		line := b[off : off+len(name)+21]
		off += len(line)
		num, ok := bytes.CutPrefix(line, []byte(name))
		if !ok || num[20] != '\n' {
			return layout{}, false
		}
		for _, c := range num[:20] {
			if c < '0' || c > '9' {
				return layout{}, false
			}
		}
		n, err := strconv.ParseInt(string(num[:20]), 10, 64)
		if err != nil {
			return layout{}, false
		}
		v[i] = n
	}
	sum, ok := bytes.CutPrefix(b[off:], []byte("Sha256: "))
	if !ok || len(sum) != 65 || sum[64] != '\n' {
		return layout{}, false
	}
	l := layout{v[0], v[1], v[2], v[3]}
	if l.hdr < 1 || l.msgOff < l.hdr || l.msgSize < 0 || l.total < l.hdr+trailerSize || l.msgOff+l.msgSize > l.total-trailerSize {
		return layout{}, false
	}
	return l, true
}

// --- writing ---------------------------------------------------------------------------------------------

// msgWriter writes a message file at a new path, counting what it wrote.
type msgWriter struct {
	f   *os.File
	w   *bufio.Writer
	n   int64
	err error // the first write error: sticky
}

func newMsgWriter(p string) (*msgWriter, error) {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return nil, err
	}
	return &msgWriter{f: f, w: bufio.NewWriterSize(f, 256*1024)}, nil
}

func (m *msgWriter) Write(p []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	n, err := m.w.Write(p)
	m.n += int64(n)
	if err != nil {
		m.err = &hubWriteErr{err}
	}
	return n, m.err
}

// hubWriteErr: a write of the hub's own failed (the disk full, say): exit 1, never the sender's fault.
type hubWriteErr struct{ err error }

func (e *hubWriteErr) Error() string { return e.err.Error() }
func (e *hubWriteErr) Unwrap() error { return e.err }

// finish writes the trailer, then flushes and closes; sync fsyncs the file first (--durable).
func (m *msgWriter) finish(l layout, sync bool) error {
	l.total = m.n + trailerSize
	m.Write(l.trailer())
	err := m.err
	if err == nil {
		err = m.w.Flush()
	}
	if err == nil && sync {
		fsynced(m.f.Name())
		err = m.f.Sync()
	}
	if cerr := m.f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (m *msgWriter) abort() { m.f.Close() }

// inErr: the sender's stream failed a check (cut: 4, malformed or over a cap: 2); msg is the stderr line.
type inErr struct {
	code int
	msg  string
}

func (e *inErr) Error() string { return e.msg }

// msgSpec is one message to store.
type msgSpec struct {
	kind    string // "text", "file" or "bag"
	from    string
	fields  []byte    // the client's header block, as stored
	inline  []byte    // the body of an inline message (src nil)
	src     io.Reader // stdin, framing and --size already applied; nil for an inline message
	fname   string    // --file's name
	msgpath string    // a bag's --msg
	gap     bool      // a gap marker: may be empty
	durable bool
}

// writeMessage writes spec's message file at p in one pass. 0 written · 2 or 4 the sender's stream refused
// (stderr said why) · 1 a hub I/O error. A failure removes p.
func (s *Session) writeMessage(p string, spec *msgSpec) int {
	h := s.h
	m, err := newMsgWriter(p)
	if err != nil {
		s.errf(errStore)
		return 1
	}
	testHook("stage")
	var hb bytes.Buffer
	fmt.Fprintf(&hb, "From: %s\nDate: %s\nSerial: %s\n", spec.from, isoTime(h.epoch()), s.serial)
	switch spec.kind {
	case "file":
		fmt.Fprintf(&hb, "Payload: file %s\n", spec.fname)
	case "bag":
		hb.WriteString("Payload: bag\n")
		if spec.msgpath != "" {
			fmt.Fprintf(&hb, "Msg: %s\n", spec.msgpath)
		}
	}
	hb.Write(spec.fields)
	hb.WriteString("\n")
	m.Write(hb.Bytes())
	l := layout{hdr: m.n, msgOff: m.n}
	fail := func(err error) int {
		m.abort()
		os.Remove(p)
		var ie *inErr
		if errors.As(err, &ie) {
			s.errf("%s", ie.msg)
			return ie.code
		}
		s.errf(errStore)
		return 1
	}
	switch spec.kind {
	case "text":
		if spec.src == nil {
			m.Write(spec.inline)
		} else if err := copyText(m, spec.src, FileMaxBytes); err != nil {
			return fail(err)
		}
		l.msgSize = m.n - l.hdr
		if l.msgSize == 0 && !spec.gap {
			return fail(&inErr{2, "dispatch: nothing to send (need -m or --file)"})
		}
	case "file":
		if _, err := io.Copy(m, &capReader{r: spec.src, left: FileMaxBytes, what: "file"}); err != nil {
			return fail(err)
		}
	case "bag":
		k, size, err := copyBag(m, spec.src, spec.msgpath)
		if err != nil {
			return fail(err)
		}
		if spec.msgpath != "" {
			l.msgOff, l.msgSize = l.hdr+k, size
		}
	}
	if m.err != nil {
		return fail(m.err)
	}
	if err := m.finish(l, spec.durable); err != nil {
		os.Remove(p)
		s.errf(errStore)
		return 1
	}
	return 0
}

// capReader reads r up to left bytes; one more is a cap error (exit 2).
type capReader struct {
	r    io.Reader
	left int64
	what string
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, &inErr{2, "dispatch: " + c.what + " exceeds cap"}
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return 0, &inErr{2, "dispatch: " + c.what + " exceeds cap"}
	}
	return n, err
}

// copyText is the body of -m or -m:rfc5322 as "$(cat)" makes of it: NULs dropped, trailing newlines removed;
// at most max bytes of input.
func copyText(w io.Writer, r io.Reader, max int64) error {
	cr := &capReader{r: r, left: max, what: "body"}
	buf := make([]byte, 64*1024)
	out := make([]byte, 0, 64*1024)
	nl := 0 // newlines held back: trailing ones go
	for {
		n, err := cr.Read(buf)
		out = out[:0]
		for _, c := range buf[:n] {
			switch c {
			case 0:
			case '\n':
				nl++
			default:
				for ; nl > 0; nl-- {
					out = append(out, '\n')
				}
				out = append(out, c)
			}
		}
		if len(out) > 0 {
			if _, werr := w.Write(out); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// eofSeen records whether the underlying read ever returned io.EOF, and counts, capping at max. It exposes
// only Read, so tar.Reader reads every block rather than seeking past members.
type eofSeen struct {
	r   io.Reader
	eof bool
	n   int64
	max int64
}

func (e *eofSeen) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.n += int64(n)
	if err == io.EOF {
		e.eof = true
	}
	if e.max > 0 && e.n > e.max {
		return n, &inErr{2, "dispatch: bag exceeds cap"}
	}
	return n, err
}

// bagName: a member name as matched: a leading ./ ignored.
func bagName(n string) string { return strings.TrimPrefix(n, "./") }

// sparseMember: a member whose data in the tar is not its bytes.
func sparseMember(hd *tar.Header) bool {
	if hd.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for k := range hd.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
}

// copyBag streams a tar from r to w in one pass, exactly as received, judging only what the hub must: its
// end marker (cut: 4), that it is a tar (2), not empty (2), at most BagMaxBytes (2), and --msg naming a
// regular, non-sparse member (2; of two with its name, the last). k is the --msg member's data offset in the
// tar, size its bytes.
func copyBag(w io.Writer, r io.Reader, msgpath string) (k, size int64, err error) {
	e := &eofSeen{r: r, max: BagMaxBytes}
	cw := &countWriter{w: w}
	tr := tar.NewReader(io.TeeReader(e, cw))
	want := bagName(msgpath)
	n, found, ok := 0, false, false
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, bagErr(err)
		}
		if hd.Typeflag == tar.TypeXGlobalHeader { // metadata (git archive writes one), not a member
			continue
		}
		name := strings.TrimSuffix(bagName(hd.Name), "/")
		if name != "" && name != "." && hd.Typeflag != tar.TypeDir {
			n++
		}
		if want != "" && bagName(hd.Name) == want {
			found, ok = true, hd.Typeflag == tar.TypeReg && !sparseMember(hd)
			k, size = cw.n, hd.Size
		}
	}
	if e.eof {
		return 0, 0, &inErr{4, errTarCut}
	}
	if _, err := io.Copy(cw, e); err != nil { // what follows the end marker (a record's padding): as received
		return 0, 0, bagErr(err)
	}
	switch {
	case n == 0:
		return 0, 0, &inErr{2, "dispatch: empty bag"}
	case want != "" && !found:
		return 0, 0, &inErr{2, "dispatch: --msg names no member of the bag: " + msgpath}
	case want != "" && !ok:
		return 0, 0, &inErr{2, "dispatch: --msg names no regular member of the bag: " + msgpath}
	}
	return k, size, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// bagErr: what a read of a bag's stream failed of. The sender's check (inErr), the hub's write (1), else what
// tar.Reader says: cut 4, the system's 1, not a tar 2.
func bagErr(err error) error {
	var ie *inErr
	var we *hubWriteErr
	switch {
	case errors.As(err, &ie):
		return ie
	case errors.As(err, &we):
		return we
	}
	code, msg := tarErr(err)
	if code == 1 {
		return err
	}
	return &inErr{code, msg}
}

// --- reading ---------------------------------------------------------------------------------------------

// msgFile is a stored message open for a content read, its trailer checked against its size.
type msgFile struct {
	f    *os.File
	l    layout
	head []byte // the header block, its blank line included
	all  []byte // the whole file, when it is small
}

// smallMsg: a file this small is read whole, in one read.
const smallMsg = 4096

// errDamaged: a message file whose size is not its trailer's Total-Size, or whose trailer does not parse.
type errDamaged struct {
	stat  int64
	total string
}

func (e *errDamaged) Error() string { return fmt.Sprintf("damaged: stat=%d total=%s", e.stat, e.total) }

// openMsg opens a message file for a content read. An *errDamaged when it is torn or truncated; the open's
// error when it is not there.
func openMsg(p string) (*msgFile, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := fi.Size()
	m := &msgFile{f: f}
	var tb []byte
	if size <= smallMsg {
		m.all = make([]byte, size)
		if _, err := io.ReadFull(f, m.all); err != nil {
			f.Close()
			return nil, err
		}
		if size >= trailerSize {
			tb = m.all[size-trailerSize:]
		}
	} else {
		tb = make([]byte, trailerSize)
		if _, err := f.ReadAt(tb, size-trailerSize); err != nil {
			f.Close()
			return nil, err
		}
	}
	l, ok := parseTrailer(tb)
	if !ok || l.total != size {
		f.Close()
		total := "unparsable"
		if ok {
			total = strconv.FormatInt(l.total, 10)
		}
		return nil, &errDamaged{size, total}
	}
	m.l = l
	if m.all != nil {
		m.head = m.all[:l.hdr]
	} else {
		m.head = make([]byte, l.hdr)
		if _, err := f.ReadAt(m.head, 0); err != nil {
			f.Close()
			return nil, err
		}
	}
	return m, nil
}

func (m *msgFile) Close() error { return m.f.Close() }

// header: the header block's lines, its blank line left out.
func (m *msgFile) header() []string {
	return strings.Split(strings.TrimSuffix(string(m.head), "\n\n"), "\n")
}

func (m *msgFile) section(off, n int64) *io.SectionReader {
	if m.all != nil {
		return io.NewSectionReader(bytes.NewReader(m.all), off, n)
	}
	return io.NewSectionReader(m.f, off, n)
}

// body: the message body's bytes.
func (m *msgFile) body() io.Reader { return m.section(m.l.msgOff, m.l.msgSize) }

// payload: the header's Payload ("file" with its name, "bag", or "" with none) and its bytes.
func (m *msgFile) payload() (kind, name string, r *io.SectionReader, n int64) {
	v := hdrGet(m.header(), "Payload")
	end := m.l.total - trailerSize
	switch {
	case v == "bag":
		return "bag", "", m.section(m.l.hdr, end-m.l.hdr), end - m.l.hdr
	case strings.HasPrefix(v, "file "):
		start := m.l.hdr + m.l.msgSize
		return "file", strings.TrimPrefix(v, "file "), m.section(start, end-start), end - start
	}
	return "", "", nil, 0
}

// hasPayload: a stored message's header block names a payload (a header-only read: no check).
func hasPayload(hdr []string) bool { return hdrGet(hdr, "Payload") != "" }

// bagMember finds the member path in a bag's tar (ra, n bytes), the last of that name: its data, or 3 none ·
// 2 not a regular, non-sparse member.
func bagMember(ra io.ReaderAt, n int64, path string) (io.Reader, int) {
	want := bagName(path)
	c := &eofSeen{r: io.NewSectionReader(ra, 0, n)}
	tr := tar.NewReader(c)
	var off, size int64
	found, ok := false, false
	for {
		hd, err := tr.Next()
		if err != nil {
			break
		}
		if bagName(hd.Name) == want {
			found, ok = true, hd.Typeflag == tar.TypeReg && !sparseMember(hd)
			off, size = c.n, hd.Size
		}
	}
	switch {
	case !found:
		return nil, 3
	case !ok:
		return nil, 2
	}
	return io.NewSectionReader(ra, off, size), 0
}

// --- damaged files ---------------------------------------------------------------------------------------

// damagedDir: where a mailbox's damaged message files go; the reaper deletes them after MSG_TTL.
const damagedDir = ".damaged"

// setAsideDamaged moves a damaged message file out of its mailbox, with its journal line.
func (h *Hub) setAsideDamaged(box, name string, d *errDamaged) {
	dd := filepath.Join(box, damagedDir)
	os.Mkdir(dd, 0o2770)
	if err := os.Rename(filepath.Join(box, name), filepath.Join(dd, name)); err != nil {
		h.logf("damaged %s %s: not moved: %v", box, name, err)
		return
	}
	mbox := strings.ReplaceAll(filepath.Base(box), "~", "/")
	h.logf("damaged %s %s stat=%d total=%s", mbox, strings.TrimSuffix(name, "."+msgExt), d.stat, d.total)
	h.journalf("damaged %s %s stat=%d total=%s", mbox, strings.TrimSuffix(name, "."+msgExt), d.stat, d.total)
}
