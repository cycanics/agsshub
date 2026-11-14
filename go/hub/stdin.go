package hub

// A send's stdin as streams: framing (--chunked) and --size are checked as the body goes by, in the one pass
// that stores it. A failure is sticky: every later read returns it, so a parser that stops at it (the
// header block of -m:rfc5322) is told why.

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// chunkReader decodes a framed stdin (RFC 9112 chunked), at most cap decoded bytes. EOF only after the last
// chunk and a trailer without Abort.
type chunkReader struct {
	r     *bufio.Reader
	left  int64 // bytes left in the current chunk
	total int64
	cap   int64
	done  bool
	err   error
}

func newChunkReader(r *bufio.Reader, cap int64) *chunkReader { return &chunkReader{r: r, cap: cap} }

func (c *chunkReader) fail(code int, msg string) (int, error) {
	c.err = &inErr{code, msg}
	return 0, c.err
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.done {
		return 0, io.EOF
	}
	if c.left == 0 {
		line, ok := readLineN(c.r, 256)
		if !ok {
			return c.fail(4, "dispatch: chunked body ended before its last chunk")
		}
		size, ok := chunkSize(line)
		if !ok {
			return c.fail(2, "dispatch: bad chunk size line")
		}
		if size == 0 {
			return c.trailer()
		}
		if c.total += size; c.total > c.cap {
			return c.fail(2, "dispatch: body exceeds cap")
		}
		c.left = size
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if n == 0 && err != nil {
		if err == io.EOF {
			return c.fail(4, "dispatch: chunked body ended inside a chunk")
		}
		c.err = err
		return 0, err
	}
	if c.left == 0 {
		crlf, ok := readLineN(c.r, 2)
		if !ok {
			c.fail(4, "dispatch: chunked body ended after a chunk")
		} else if strings.TrimSuffix(crlf, "\r") != "" {
			c.fail(2, "dispatch: chunk data not followed by CRLF")
		}
		if c.err != nil {
			return n, nil // the bytes are the sender's; the error comes at the next read
		}
	}
	return n, nil
}

func (c *chunkReader) trailer() (int, error) {
	for n := 0; ; {
		line, ok := readLineN(c.r, 1024)
		if !ok {
			return c.fail(4, "dispatch: chunked body ended inside its trailer")
		}
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			c.done = true
			return 0, io.EOF
		}
		if n++; n > 32 {
			return c.fail(2, "dispatch: trailer too long")
		}
		if rest, ab := isAbort(line); ab {
			return c.fail(4, "dispatch: sender aborted:"+rest)
		}
	}
}

// sizedReader: a stdin of exactly n bytes. Fewer: 4 (a retry can fix that); more: 2 (ssh adds no bytes, so a
// retry cannot), found by one more read at the end.
type sizedReader struct {
	r    io.Reader
	n    int64
	got  int64
	done bool
	err  error
}

func (s *sizedReader) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.done {
		return 0, io.EOF
	}
	if s.got == s.n {
		var one [1]byte
		k, err := io.ReadFull(s.r, one[:])
		if k > 0 {
			s.err = &inErr{2, fmt.Sprintf("dispatch: body longer than --size %d", s.n)}
			return 0, s.err
		}
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			s.err = err
			return 0, err
		}
		s.done = true
		return 0, io.EOF
	}
	if int64(len(p)) > s.n-s.got {
		p = p[:s.n-s.got]
	}
	k, err := s.r.Read(p)
	s.got += int64(k)
	if k == 0 && err != nil {
		if err == io.EOF {
			s.err = &inErr{4, fmt.Sprintf("dispatch: body ended after %d of %d bytes", s.got, s.n)}
		} else {
			s.err = err
		}
		return 0, s.err
	}
	return k, nil
}

// stickyErr: the check a stream wrapper failed, if it did: a parse that stopped early learns why.
func stickyErr(r io.Reader) *inErr {
	switch v := r.(type) {
	case *chunkReader:
		if ie, ok := v.err.(*inErr); ok {
			return ie
		}
	case *sizedReader:
		if ie, ok := v.err.(*inErr); ok {
			return ie
		}
	case *bufio.Reader:
	}
	return nil
}
