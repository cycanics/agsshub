// Package descriptor checks a service's description when it is registered, and turns it into what
// the hub stores and serves without parsing it again (docs/help.md). One
// implementation: the hub in-process, hub-topic through cmd/hub-descriptor.
package descriptor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	MaxBytes     = 64 * 1024
	MaxDepth     = 32
	MaxOneLine   = 200
	MaxPatterns  = 16
	maxPatternCh = 128
)

var patternRE = regexp.MustCompile(`^[a-z0-9._+/@*?-]{1,` + strconv.Itoa(maxPatternCh) + `}$`)

// Options are what the registering side adds to the descriptor.
type Options struct {
	See   []string // --see patterns
	Topic bool     // a topic's description: no live, no scope, an empty instance view
}

// View is one answer: its default format, its compact JSON and its text rendering.
type View struct {
	Default string // "json" or "text"
	JSON    []byte
	Text    []byte // empty when Default is "json"
}

// Stored is a registered description, without the Date the hub stamps.
type Stored struct {
	See             []string
	Whole, Instance View
}

// LiveRefused: a stored description (its whole JSON) says live: false, so the hub refuses every --live
// to the identity.
func LiveRefused(whole []byte) bool {
	var top struct {
		Live *bool `json:"live"`
	}
	if json.Unmarshal(whole, &top) != nil || top.Live == nil {
		return false
	}
	return !*top.Live
}

// CheckPatterns refuses a --see list a reader could not evaluate safely.
func CheckPatterns(see []string) error {
	if len(see) > MaxPatterns {
		return fmt.Errorf("more than %d --see patterns", MaxPatterns)
	}
	for _, p := range see {
		if !patternRE.MatchString(p) {
			return fmt.Errorf("bad --see pattern (up to %d characters of [a-z0-9._+/@*?-]): %.64q", maxPatternCh, p)
		}
	}
	return nil
}

// Build checks in and returns what to store; an error is the reason it is refused.
func Build(in []byte, o Options) (*Stored, error) {
	if err := CheckPatterns(o.See); err != nil {
		return nil, err
	}
	if len(in) == 0 {
		return nil, errors.New("empty")
	}
	if len(in) > MaxBytes {
		return nil, fmt.Errorf("over %d bytes", MaxBytes)
	}
	if !utf8.Valid(in) {
		return nil, errors.New("not UTF-8")
	}
	if bytes.HasPrefix(in, []byte("\xef\xbb\xbf")) {
		return nil, errors.New("starts with a BOM")
	}
	if !json.Valid(in) {
		return nil, errors.New("not exactly one JSON value")
	}
	if err := walk(in); err != nil {
		return nil, err
	}
	top, order, err := object(in, "the description")
	if err != nil {
		return nil, err
	}
	d, err := check(top, o)
	if err != nil {
		return nil, err
	}

	var whole bytes.Buffer
	if err := json.Compact(&whole, in); err != nil {
		return nil, err
	}
	st := &Stored{See: o.See}
	st.Whole = makeView(whole.Bytes(), d, d.requests)
	if o.Topic {
		st.Instance = View{Default: "text"}
		return st, nil
	}
	var kept []req
	for _, r := range d.requests {
		if r.scope != "address" {
			kept = append(kept, r)
		}
	}
	inst, err := instanceJSON(top, order, kept)
	if err != nil {
		return nil, err
	}
	st.Instance = makeView(inst, d, kept)
	return st, nil
}

// Bytes is the stored form after the hub's Date line. The byte counts come first, so a
// reader needs no parsing.
func (s *Stored) Bytes() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "See: %s\n", strings.Join(s.See, " "))
	head := func(name string, v View) {
		fmt.Fprintf(&b, "%s-Default: %s\n%s-Json-Bytes: %d\n%s-Text-Bytes: %d\n", name, v.Default, name, len(v.JSON), name, len(v.Text))
	}
	head("Whole", s.Whole)
	head("Instance", s.Instance)
	b.WriteByte('\n')
	b.Write(s.Whole.JSON)
	b.Write(s.Whole.Text)
	b.Write(s.Instance.JSON)
	b.Write(s.Instance.Text)
	return b.Bytes()
}

// Record is a stored file read back: everything from one read, so one version.
type Record struct {
	Date, See string
	Whole     View
	Instance  View
}

// ParseRecord reads a stored file; false when it is not one.
func ParseRecord(b []byte) (*Record, bool) {
	end := bytes.Index(b, []byte("\n\n"))
	if end < 0 {
		return nil, false
	}
	r := &Record{}
	n := map[string]int{}
	for _, line := range strings.Split(string(b[:end]), "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			k, v = strings.TrimSuffix(line, ":"), ""
		}
		switch k {
		case "Date":
			r.Date = v
		case "See":
			r.See = v
		case "Whole-Default":
			r.Whole.Default = v
		case "Instance-Default":
			r.Instance.Default = v
		default:
			c, err := strconv.Atoi(v)
			if err != nil || c < 0 {
				return nil, false
			}
			n[k] = c
		}
	}
	rest := b[end+2:]
	take := func(k string) ([]byte, bool) {
		c := n[k]
		if c > len(rest) {
			return nil, false
		}
		out := rest[:c]
		rest = rest[c:]
		return out, true
	}
	var ok [4]bool
	r.Whole.JSON, ok[0] = take("Whole-Json-Bytes")
	r.Whole.Text, ok[1] = take("Whole-Text-Bytes")
	r.Instance.JSON, ok[2] = take("Instance-Json-Bytes")
	r.Instance.Text, ok[3] = take("Instance-Text-Bytes")
	return r, ok[0] && ok[1] && ok[2] && ok[3]
}

// walk checks what encoding/json lets through: repeated keys, depth, lone surrogate escapes and a
// number that overflows a double. in is already valid JSON.
func walk(in []byte) error {
	type frame struct {
		object bool
		keys   map[string]bool
		isKey  bool
	}
	var stack []*frame
	for i := 0; i < len(in); {
		switch c := in[i]; {
		case c == '{' || c == '[':
			if len(stack) == MaxDepth {
				return fmt.Errorf("nested deeper than %d", MaxDepth)
			}
			stack = append(stack, &frame{object: c == '{', keys: map[string]bool{}, isKey: c == '{'})
			i++
		case c == '}' || c == ']':
			stack = stack[:len(stack)-1]
			i++
		case c == ',':
			if top := stack[len(stack)-1]; top.object {
				top.isKey = true
			}
			i++
		case c == '"':
			end, err := stringEnd(in, i)
			if err != nil {
				return err
			}
			if len(stack) > 0 && stack[len(stack)-1].isKey {
				top := stack[len(stack)-1]
				var k string
				if err := json.Unmarshal(in[i:end], &k); err != nil {
					return err
				}
				if top.keys[k] {
					return fmt.Errorf("repeated key: %.64q", k)
				}
				top.keys[k] = true
				top.isKey = false
			}
			i = end
		case c == '-' || (c >= '0' && c <= '9'):
			end := i
			for end < len(in) && bytes.IndexByte([]byte("+-.eE0123456789"), in[end]) >= 0 {
				end++
			}
			if f, _ := strconv.ParseFloat(string(in[i:end]), 64); math.IsInf(f, 0) {
				return fmt.Errorf("number overflows a double: %.32s", in[i:end])
			}
			i = end
		default:
			i++
		}
	}
	return nil
}

// stringEnd returns the offset after the string starting at in[i], refusing a lone surrogate.
func stringEnd(in []byte, i int) (int, error) {
	unit := func(j int) (uint64, bool) {
		if j+6 > len(in) || in[j] != '\\' || in[j+1] != 'u' {
			return 0, false
		}
		v, err := strconv.ParseUint(string(in[j+2:j+6]), 16, 16)
		return v, err == nil
	}
	for j := i + 1; j < len(in); {
		switch in[j] {
		case '"':
			return j + 1, nil
		case '\\':
			if in[j+1] != 'u' {
				j += 2
				continue
			}
			v, _ := unit(j)
			switch {
			case v >= 0xD800 && v <= 0xDBFF:
				lo, ok := unit(j + 6)
				if !ok || lo < 0xDC00 || lo > 0xDFFF {
					return 0, errors.New("a lone surrogate escape")
				}
				j += 12
			case v >= 0xDC00 && v <= 0xDFFF:
				return 0, errors.New("a lone surrogate escape")
			default:
				j += 6
			}
		default:
			j++
		}
	}
	return 0, errors.New("unterminated string")
}

// object decodes a JSON object, with its keys in the order they came.
func object(raw []byte, what string) (map[string]json.RawMessage, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if d, ok := t.(json.Delim); err != nil || !ok || d != '{' {
		return nil, nil, fmt.Errorf("%s is not an object", what)
	}
	m := map[string]json.RawMessage{}
	var order []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		k := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		m[k] = v
		order = append(order, k)
	}
	return m, order, nil
}

type req struct {
	raw                         json.RawMessage
	subject                     *string
	form, summary, guide, scope string
	files                       string
	delivery                    []string
}

type desc struct {
	summary, version string
	requests         []req
}

func onlyKeys(m map[string]json.RawMessage, what string, allowed ...string) error {
	for k := range m {
		found := false
		for _, a := range allowed {
			found = found || a == k
		}
		if !found {
			return fmt.Errorf("%s: unknown key %.64q", what, k)
		}
	}
	return nil
}

func str(raw json.RawMessage, what string) (string, error) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", fmt.Errorf("%s: not a string", what)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s, nil
}

func isControl(r rune) bool { return r < 32 || r == 127 || (r >= 0x80 && r <= 0x9F) }

func oneLine(raw json.RawMessage, what string) (string, error) {
	s, err := str(raw, what)
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", fmt.Errorf("%s: empty", what)
	}
	if utf8.RuneCountInString(s) > MaxOneLine {
		return "", fmt.Errorf("%s: over %d characters", what, MaxOneLine)
	}
	if strings.TrimSpace(s) != s {
		return "", fmt.Errorf("%s: outer spaces", what)
	}
	for _, r := range s {
		if isControl(r) {
			return "", fmt.Errorf("%s: a control character", what)
		}
	}
	return s, nil
}

func check(top map[string]json.RawMessage, o Options) (*desc, error) {
	if err := onlyKeys(top, "the description", "descriptor", "version", "summary", "requests", "live"); err != nil {
		return nil, err
	}
	if v, ok := top["live"]; ok {
		switch string(v) {
		case "false":
		case "true":
			if o.Topic {
				return nil, errors.New("live: a topic takes no live send")
			}
		default:
			return nil, errors.New("live: must be true or false")
		}
	}
	for _, k := range []string{"descriptor", "summary", "requests"} {
		if _, ok := top[k]; !ok {
			return nil, fmt.Errorf("missing key: %s", k)
		}
	}
	if string(top["descriptor"]) != "1" {
		return nil, errors.New("descriptor: must be the literal 1")
	}
	d := &desc{}
	var err error
	if d.summary, err = oneLine(top["summary"], "summary"); err != nil {
		return nil, err
	}
	if v, ok := top["version"]; ok {
		if d.version, err = oneLine(v, "version"); err != nil {
			return nil, err
		}
	}
	if len(top["requests"]) == 0 || top["requests"][0] != '[' {
		return nil, errors.New("requests: not an array")
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(top["requests"], &raws); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i, raw := range raws {
		r, err := checkReq(raw, fmt.Sprintf("requests[%d]", i), o)
		if err != nil {
			return nil, err
		}
		key := "\x00" + r.files
		if r.subject != nil {
			key = *r.subject + "\x00" + r.files
		}
		if seen[key] {
			return nil, fmt.Errorf("requests[%d]: the same subject and files as an earlier request", i)
		}
		seen[key] = true
		d.requests = append(d.requests, *r)
	}
	return d, nil
}

func checkReq(raw json.RawMessage, p string, o Options) (*req, error) {
	m, _, err := object(raw, p)
	if err != nil {
		return nil, err
	}
	if err := onlyKeys(m, p, "subject", "form", "delivery", "summary", "schema", "guide", "reply", "files", "scope"); err != nil {
		return nil, err
	}
	for _, k := range []string{"subject", "form", "delivery", "summary"} {
		if _, ok := m[k]; !ok {
			return nil, fmt.Errorf("%s: missing key: %s", p, k)
		}
	}
	r := &req{raw: raw, scope: "both"}
	if string(m["subject"]) != "null" {
		s, err := oneLine(m["subject"], p+".subject")
		if err != nil {
			return nil, err
		}
		r.subject = &s
	}
	if r.summary, err = oneLine(m["summary"], p+".summary"); err != nil {
		return nil, err
	}
	if r.form, err = str(m["form"], p+".form"); err != nil {
		return nil, err
	}
	if len(m["delivery"]) == 0 || m["delivery"][0] != '[' {
		return nil, fmt.Errorf("%s.delivery: not an array", p)
	}
	var dv []json.RawMessage
	if err := json.Unmarshal(m["delivery"], &dv); err != nil {
		return nil, err
	}
	if len(dv) == 0 {
		return nil, fmt.Errorf("%s.delivery: empty", p)
	}
	live := false
	seen := map[string]bool{}
	for _, v := range dv {
		s, err := str(v, p+".delivery")
		if err != nil {
			return nil, err
		}
		if s != "spool" && s != "live" {
			return nil, fmt.Errorf("%s.delivery: must be spool or live", p)
		}
		if seen[s] {
			return nil, fmt.Errorf("%s.delivery: a repeat", p)
		}
		seen[s] = true
		live = live || s == "live"
		r.delivery = append(r.delivery, s)
	}
	if o.Topic && live {
		return nil, fmt.Errorf("%s.delivery: a topic takes no live send", p)
	}
	if g, ok := m["guide"]; ok {
		if r.guide, err = str(g, p+".guide"); err != nil {
			return nil, err
		}
		if r.guide == "" {
			return nil, fmt.Errorf("%s.guide: empty", p)
		}
		for _, c := range r.guide {
			if isControl(c) && c != '\n' && c != '\t' {
				return nil, fmt.Errorf("%s.guide: a control character", p)
			}
		}
	}
	schema, hasSchema := m["schema"]
	switch r.form {
	case "structured":
		if !hasSchema {
			return nil, fmt.Errorf("%s: structured needs schema", p)
		}
		if r.subject == nil {
			return nil, fmt.Errorf("%s: structured needs a subject", p)
		}
		if schema[0] != '{' {
			return nil, fmt.Errorf("%s.schema: not an object", p)
		}
	case "text":
		if hasSchema {
			return nil, fmt.Errorf("%s: text takes no schema", p)
		}
		if r.guide == "" {
			return nil, fmt.Errorf("%s: text needs guide", p)
		}
	default:
		return nil, fmt.Errorf("%s.form: must be structured or text", p)
	}
	if rp, ok := m["reply"]; ok {
		if !live {
			return nil, fmt.Errorf("%s.reply: only on a live request", p)
		}
		if err := checkReply(rp, p+".reply"); err != nil {
			return nil, err
		}
	}
	if f, ok := m["files"]; ok {
		if r.files, err = str(f, p+".files"); err != nil {
			return nil, err
		}
		if r.files != "file" && r.files != "bag" {
			return nil, fmt.Errorf("%s.files: must be file or bag", p)
		}
		if r.subject != nil {
			return nil, fmt.Errorf("%s.files: files travel with no subject", p)
		}
		if r.files == "bag" && live {
			return nil, fmt.Errorf("%s.files: a bag has no live send", p)
		}
	}
	if sc, ok := m["scope"]; ok {
		if o.Topic {
			return nil, fmt.Errorf("%s.scope: a topic has no instances", p)
		}
		if r.scope, err = str(sc, p+".scope"); err != nil {
			return nil, err
		}
		if r.scope != "address" && r.scope != "instance" && r.scope != "both" {
			return nil, fmt.Errorf("%s.scope: must be address, instance or both", p)
		}
	}
	return r, nil
}

var statusRE = regexp.MustCompile(`^[245][0-9][0-9]$`)

func checkReply(raw json.RawMessage, p string) error {
	m, _, err := object(raw, p)
	if err != nil {
		return err
	}
	if err := onlyKeys(m, p, "content_type", "statuses", "schema"); err != nil {
		return err
	}
	for _, k := range []string{"content_type", "statuses"} {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("%s: missing key: %s", p, k)
		}
	}
	if _, err := oneLine(m["content_type"], p+".content_type"); err != nil {
		return err
	}
	if len(m["statuses"]) == 0 || m["statuses"][0] != '[' {
		return fmt.Errorf("%s.statuses: not an array", p)
	}
	var sts []json.RawMessage
	if err := json.Unmarshal(m["statuses"], &sts); err != nil {
		return err
	}
	for _, v := range sts {
		if !statusRE.Match(v) {
			return fmt.Errorf("%s.statuses: %.16s is not a 2xx, 4xx or 5xx code", p, v)
		}
	}
	if sc, ok := m["schema"]; ok && sc[0] != '{' {
		return fmt.Errorf("%s.schema: not an object", p)
	}
	return nil
}

// instanceJSON is the description with only the requests an instance handles, key order kept.
func instanceJSON(top map[string]json.RawMessage, order []string, kept []req) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range order {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:", k)
		if k != "requests" {
			if err := json.Compact(&b, top[k]); err != nil {
				return nil, err
			}
			continue
		}
		b.WriteByte('[')
		for j, r := range kept {
			if j > 0 {
				b.WriteByte(',')
			}
			if err := json.Compact(&b, r.raw); err != nil {
				return nil, err
			}
		}
		b.WriteByte(']')
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func makeView(js []byte, d *desc, rs []req) View {
	programmatic := len(rs) > 0
	for _, r := range rs {
		programmatic = programmatic && r.form == "structured"
	}
	if programmatic {
		return View{Default: "json", JSON: js}
	}
	return View{Default: "text", JSON: js, Text: render(d, rs)}
}

func render(d *desc, rs []req) []byte {
	var b bytes.Buffer
	b.WriteString(d.summary + "\n")
	if d.version != "" {
		b.WriteString("Version: " + d.version + "\n")
	}
	b.WriteByte('\n')
	if len(rs) == 0 {
		b.WriteString("Takes no requests.\n")
		return b.Bytes()
	}
	name := func(r req) string {
		if r.subject == nil {
			return "(no subject)"
		}
		return *r.subject
	}
	for _, r := range rs {
		files := r.files
		if files == "" {
			files = "-"
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\n", name(r), r.form, strings.Join(r.delivery, ","), r.scope, files, r.summary)
	}
	for _, r := range rs {
		if r.guide == "" {
			continue
		}
		b.WriteString("\n" + name(r) + ":\n")
		for _, line := range strings.Split(strings.TrimRight(r.guide, "\n"), "\n") {
			b.WriteString("  " + line + "\n")
		}
	}
	return b.Bytes()
}
