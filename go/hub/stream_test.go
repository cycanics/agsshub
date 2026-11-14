package hub

// The read stream (docs/wake-live-event-id.md, The stream): a parser that checks a recv stdout is a valid
// HTTP/1.1 chunked stream, every chunk with ;id= and ;part=, and helpers over its chunks.

import (
	"fmt"
	"strconv"
	"strings"
)

type tchunk struct {
	id, part, data string
}

// stream parses s: its chunks, and the final chunk's extensions ("" for a bare 0, ";limit", ";end=gone"),
// or final "-" when the stream has no final chunk yet (a running or cut follow). An error for anything
// that is not a valid chunked stream from its first byte.
func stream(s string) (cs []tchunk, final string, err error) {
	for len(s) > 0 {
		line, rest, ok := strings.Cut(s, "\r\n")
		if !ok {
			return cs, "-", fmt.Errorf("a size line without CRLF: %q", firstN(s, 80))
		}
		sz, ext, _ := strings.Cut(line, ";")
		if ext != "" {
			ext = ";" + ext
		}
		n, perr := strconv.ParseUint(sz, 16, 63)
		if perr != nil || sz != strconv.FormatUint(n, 16) {
			return cs, "-", fmt.Errorf("bad size line %q", line)
		}
		if n == 0 {
			if rest != "\r\n" {
				return cs, "-", fmt.Errorf("bytes after the final chunk: %q", firstN(rest, 80))
			}
			return cs, ext, nil
		}
		if uint64(len(rest)) < n+2 || rest[n:n+2] != "\r\n" {
			return cs, "-", fmt.Errorf("chunk %q: data short or not CRLF-terminated", line)
		}
		c := tchunk{data: rest[:n]}
		for _, kv := range strings.Split(ext, ";")[1:] {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "id":
				c.id = v
			case "part":
				c.part = v
			default:
				return cs, "-", fmt.Errorf("chunk %q: unknown extension %q", line, kv)
			}
		}
		switch {
		case c.part == "info" && c.id == "" && ext == ";part=info": // a status line: no msgid
		case c.id == "" || c.part == "" || c.part == "info" || ext != ";id="+c.id+";part="+c.part:
			return cs, "-", fmt.Errorf("chunk %q: not ;id=…;part=… (or ;part=info alone)", line)
		}
		cs = append(cs, c)
		s = rest[n+2:]
	}
	return cs, "-", nil
}

// chunks: the chunks of s; a malformed stream gives one chunk of part "BAD" carrying the error.
func chunks(s string) []tchunk {
	cs, _, err := stream(s)
	if err != nil {
		return append(cs, tchunk{part: "BAD", data: err.Error()})
	}
	return cs
}

// final: the final chunk's extensions ("" for 0), "-" for none, "BAD: …" for a malformed stream.
func final(s string) string {
	_, f, err := stream(s)
	if err != nil {
		return "BAD: " + err.Error()
	}
	return f
}

// ids: the msgids of a listing's messages (part=msg or, with --header:stderr, part=id chunks), in order,
// less the hub's rendered wake and position notices.
func ids(listing string) string {
	var out []string
	for _, c := range chunks(listing) {
		switch {
		case c.part == "BAD":
			out = append(out, "BAD:"+c.data)
		case c.part != "msg" && c.part != "id":
		case strings.HasPrefix(c.id, HeadPrefix), strings.HasPrefix(c.id, WakePrefix):
		default:
			out = append(out, c.id)
		}
	}
	return strings.Join(out, " ")
}

// allIDs: every chunk's msgid and part, in order: "id/part …".
func allIDs(s string) string {
	var out []string
	for _, c := range chunks(s) {
		out = append(out, c.id+"/"+c.part)
	}
	return strings.Join(out, " ")
}

// msgOf: mid's part=msg chunk's data (header block, blank line, body), or "".
func msgOf(listing, mid string) string {
	for _, c := range chunks(listing) {
		if c.id == mid && c.part == "msg" {
			return c.data
		}
	}
	return ""
}

// partOf: the data of mid's chunks of part, concatenated.
func partOf(s, mid, part string) string {
	var b strings.Builder
	for _, c := range chunks(s) {
		if c.id == mid && c.part == part {
			b.WriteString(c.data)
		}
	}
	return b.String()
}

// bodyOf: mid's body in a listing: its msg chunk after the header block.
func bodyOf(listing, mid string) string {
	_, b, _ := strings.Cut(msgOf(listing, mid), "\n\n")
	return b
}

// empty is the stream of an empty listing: the final chunk alone.
const empty = "0\r\n\r\n"
