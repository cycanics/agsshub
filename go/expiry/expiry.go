// Package expiry keeps a mailbox's expiry notices (docs/expiry-and-subject.md): what the reaper expired
// uncollected, counted per sender. The hub calls it in-process. The stored form is JSON, rendered when
// read, and it is never a .msg, so nothing expires it.
//
//	<box>/.expired/<sender>.json     one notice, written by rename; '/' in the sender becomes '~'
//	<box>/.expired/.claim.<msgid>    a message claimed for counting; the reaper deletes it after
//	<box>/.expired/.lock             flock: one writer or clearer at a time, across processes
//	<box>/.expired/.bad.<name>       a notice file that could not be read
package expiry

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// Dir is a mailbox's notice directory.
	Dir = ".expired"
	// ClaimPrefix names a claimed message inside Dir.
	ClaimPrefix = ".claim."
	// Prefix starts every notice's msgid; a client's --id may not.
	Prefix = "expired-"
	// MaxVerbs is how many verb rows a notice stores; a new verb past it counts in others.
	MaxVerbs = 1024

	maxFileBytes   = 1 << 20  // far above 1,024 rows; a larger file is not one we wrote
	maxHeaderBytes = 64 << 10 // a header block is capped at 4 KiB of the client's and a few lines of the hub's
	shownVerbChars = 32
	shownVerbRows  = 16
	timeForm       = "2006-01-02T15:04:05Z"
	lockName       = ".lock"
	badPrefix      = ".bad."
	tmpPrefix      = ".tmp."
	jsonExt        = ".json"
)

var (
	verbRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,64}$`)
	// the longest identity is 204 bytes; the limit leaves room in a file name for ".bad." and ".json"
	senderRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+/@-]{0,239}$`)
	msgidRE  = regexp.MustCompile(`^expired-([0-9a-f]{16})-([0-9]{1,19})$`)
	tmpSeq   atomic.Uint64
)

// Options: where a notice's time comes from and where trouble is logged.
type Options struct {
	Now  func() time.Time
	Logf func(format string, args ...any)
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// Row is one table row's raw figures.
type Row struct {
	Count  int64  `json:"count"`
	Oldest string `json:"oldest"`
	Newest string `json:"newest"`
}

func (r Row) plus(o Row) Row {
	r.Count += o.Count
	if o.Oldest < r.Oldest {
		r.Oldest = o.Oldest
	}
	if o.Newest > r.Newest {
		r.Newest = o.Newest
	}
	return r
}

func plusPtr(a *Row, b Row) *Row {
	if a == nil {
		return &b
	}
	r := a.plus(b)
	return &r
}

// Notice is the stored form.
type Notice struct {
	From      string         `json:"from"`
	Msgid     string         `json:"msgid"`
	Date      string         `json:"date"`
	Expired   int64          `json:"expired"`
	Verbs     map[string]Row `json:"verbs,omitempty"`
	Gap       *Row           `json:"gap,omitempty"`
	Others    *Row           `json:"others,omitempty"`
	NoSubject *Row           `json:"no_subject,omitempty"`
}

// Kind says where a counted message goes.
type Kind int

// The four groups of a message, in the order they are tried.
const (
	KindGap Kind = iota
	KindNoSubject
	KindVerb
	KindOthers
)

// Classify places a stored message by its header lines: a Gap field; no Subject, or a blank one; the
// first word of Subject when it is a verb; otherwise others. The verb is set for KindVerb.
func Classify(header []string) (Kind, string) {
	subject, have := "", false
	for _, l := range header {
		if strings.HasPrefix(l, "Gap:") {
			return KindGap, ""
		}
		if v, ok := strings.CutPrefix(l, "Subject:"); ok && !have {
			subject, have = strings.TrimPrefix(v, " "), true
		}
	}
	words := strings.FieldsFunc(subject, func(r rune) bool { return r == ' ' || r == '\t' })
	if len(words) == 0 {
		return KindNoSubject, ""
	}
	if verbRE.MatchString(words[0]) {
		return KindVerb, words[0]
	}
	return KindOthers, ""
}

// add counts rows in n: a new verb past MaxVerbs goes to others.
func (n *Notice) add(k Kind, verb string, r Row) {
	n.Expired += r.Count
	switch k {
	case KindGap:
		n.Gap = plusPtr(n.Gap, r)
	case KindNoSubject:
		n.NoSubject = plusPtr(n.NoSubject, r)
	case KindOthers:
		n.Others = plusPtr(n.Others, r)
	default:
		if have, ok := n.Verbs[verb]; ok {
			n.Verbs[verb] = have.plus(r)
		} else if len(n.Verbs) >= MaxVerbs {
			n.Others = plusPtr(n.Others, r)
		} else {
			if n.Verbs == nil {
				n.Verbs = map[string]Row{}
			}
			n.Verbs[verb] = r
		}
	}
}

// --- rendering: a pure function of the rows ---------------------------------------------------------

type shownRow struct {
	name string
	Row
}

func lastSeparator(s string) int { return strings.LastIndexAny(s, "./-_") }

// bestSharedPrefix: the longest prefix ending in a separator that two or more rows share, the first in
// byte order among the longest; empty when they share none. A prefix row (<prefix>*) has its prefix.
func bestSharedPrefix(rows map[string]Row) string {
	seen := map[string]int{}
	for name := range rows {
		stem := strings.TrimSuffix(name, "*")
		for i := 0; i < len(stem); i++ {
			if strings.IndexByte("./-_", stem[i]) >= 0 {
				seen[stem[:i+1]]++
			}
		}
	}
	best := ""
	for p, c := range seen {
		if c < 2 {
			continue
		}
		if len(p) > len(best) || (len(p) == len(best) && p < best) {
			best = p
		}
	}
	return best
}

// groupRows is the rendering of the verb table: a long verb is cut to its prefix, then, over 16 rows,
// rows merge by shared prefix, or the smallest goes to others. Each step leaves fewer rows.
func groupRows(n Notice) ([]shownRow, *Row) {
	rows := map[string]Row{}
	others := n.Others
	put := func(name string, r Row) {
		if have, ok := rows[name]; ok {
			r = have.plus(r)
		}
		rows[name] = r
	}
	verbs := make([]string, 0, len(n.Verbs))
	for v := range n.Verbs {
		verbs = append(verbs, v)
	}
	sort.Strings(verbs)
	for _, v := range verbs {
		switch r := n.Verbs[v]; {
		case len(v) <= shownVerbChars:
			put(v, r)
		case lastSeparator(v[:shownVerbChars]) >= 0:
			put(v[:lastSeparator(v[:shownVerbChars])+1]+"*", r)
		default:
			others = plusPtr(others, r)
		}
	}
	for len(rows) > shownVerbRows {
		if p := bestSharedPrefix(rows); p != "" {
			names := make([]string, 0, len(rows))
			for name := range rows {
				if strings.HasPrefix(strings.TrimSuffix(name, "*"), p) {
					names = append(names, name)
				}
			}
			var sum *Row
			for _, name := range names {
				sum = plusPtr(sum, rows[name])
				delete(rows, name)
			}
			rows[p+"*"] = *sum
			continue
		}
		worst := ""
		for name, r := range rows {
			if worst == "" || r.Count < rows[worst].Count || (r.Count == rows[worst].Count && name > worst) {
				worst = name
			}
		}
		others = plusPtr(others, rows[worst])
		delete(rows, worst)
	}
	out := make([]shownRow, 0, len(rows))
	for name, r := range rows {
		out = append(out, shownRow{name, r})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, others
}

func writeRow(b *strings.Builder, name string, r Row) {
	fmt.Fprintf(b, "%s\t%d\t%s\t%s\n", name, r.Count, r.Oldest, r.Newest)
}

// Tables renders the notice's tables: TAB-separated, one blank line between two, none at the end; a table
// with no rows is left out.
func Tables(n Notice) string {
	var b strings.Builder
	rows, others := groupRows(n)
	if len(rows) > 0 {
		b.WriteString("verb\tcount\toldest\tnewest\n")
		for _, r := range rows {
			writeRow(&b, r.name, r.Row)
		}
	}
	if n.Gap != nil || others != nil || n.NoSubject != nil {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("special\tcount\toldest\tnewest\n")
		if n.Gap != nil {
			writeRow(&b, "[gap]", *n.Gap)
		}
		if others != nil {
			writeRow(&b, "[others]", *others)
		}
		if n.NoSubject != nil {
			writeRow(&b, "[no-subject]", *n.NoSubject)
		}
	}
	return b.String()
}

// RenderRaw is a notice as recv streams it: header block, blank line, tables; in a subscription too it
// carries nothing about the session.
func RenderRaw(n Notice) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\nDate: %s\n", n.From, n.Date)
	fmt.Fprintf(&b, "Expired: %d\n\n%s", n.Expired, Tables(n))
	return b.String()
}

// Probe is the zero-count notice a follow of a notice_probe identity prints first. It is never stored; its
// msgid's nanoseconds are a whole second, which a stored notice's never are (stamp).
func Probe(identity string, o Options) Notice {
	t := o.now()
	return Notice{From: identity, Msgid: fmt.Sprintf("%s%s-%d", Prefix, senderHash(identity), t.Unix()*int64(time.Second)), Date: t.UTC().Format(timeForm)}
}

// IsProbe: msgid has the form of a probe shown to identity.
func IsProbe(msgid, identity string) bool {
	m := msgidRE.FindStringSubmatch(msgid)
	if m == nil || m[1] != senderHash(identity) {
		return false
	}
	ns, err := strconv.ParseInt(m[2], 10, 64)
	return err == nil && ns > 0 && ns%int64(time.Second) == 0
}

// --- on disk -----------------------------------------------------------------------------------------

// FileName is a sender's notice file.
func FileName(sender string) string { return strings.ReplaceAll(sender, "/", "~") + jsonExt }

func senderHash(sender string) string {
	h := sha256.Sum256([]byte(sender))
	return hex.EncodeToString(h[:8])
}

func validTime(s string) bool { _, err := time.Parse(timeForm, s); return err == nil }

func validRow(r Row) bool { return r.Count > 0 && validTime(r.Oldest) && validTime(r.Newest) }

func validRowPtr(r *Row) bool { return r == nil || validRow(*r) }

// check: what a notice file must hold to be read as this sender's notice.
func (n *Notice) check(name string) error {
	if !senderRE.MatchString(n.From) || FileName(n.From) != name {
		return errors.New("its sender does not match its name")
	}
	m := msgidRE.FindStringSubmatch(n.Msgid)
	if m == nil || m[1] != senderHash(n.From) {
		return errors.New("its msgid is not its sender's")
	}
	if !validTime(n.Date) || n.Expired <= 0 {
		return errors.New("its date or total is malformed")
	}
	for v, r := range n.Verbs {
		if !verbRE.MatchString(v) || !validRow(r) {
			return errors.New("a verb row is malformed")
		}
	}
	if !validRowPtr(n.Gap) || !validRowPtr(n.Others) || !validRowPtr(n.NoSubject) {
		return errors.New("a special row is malformed")
	}
	return nil
}

// badFile: what the file holds is not a notice. Any other read error (a full fd table, an I/O error) says
// nothing about the file: it stays where it is.
type badFile struct{ error }

func isBad(err error) bool { var b badFile; return errors.As(err, &b) }

// readNotice: fs.ErrNotExist when there is no file, a badFile when it is not a notice.
func readNotice(dir, name string) (*Notice, error) {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, badFile{errors.New("not a regular file")}
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFileBytes {
		return nil, badFile{errors.New("too large")}
	}
	var n Notice
	if err := json.Unmarshal(b, &n); err != nil {
		return nil, badFile{err}
	}
	if err := n.check(name); err != nil {
		return nil, badFile{err}
	}
	return &n, nil
}

// setAside moves a bad notice file away (the caller holds the lock): the count starts again.
func setAside(dir, name string, why error, o Options) {
	if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, badPrefix+name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		o.logf("expiry: %s: bad notice %s (%v), and it cannot be moved: %v", dir, name, why, err)
		return
	}
	o.logf("expiry: %s: bad notice %s moved to %s%s: %v", dir, name, badPrefix, name, why)
}

func entries(dir string) []string {
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer f.Close()
	names, _ := f.Readdirnames(-1)
	sort.Strings(names)
	return names
}

func noticeFiles(dir string) []string {
	var out []string
	for _, n := range entries(dir) {
		if !strings.HasPrefix(n, ".") && strings.HasSuffix(n, jsonExt) {
			out = append(out, n)
		}
	}
	return out
}

func lock(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o660)
	if err != nil {
		return nil, err
	}
	f.Chmod(0o660) // whatever the umask; only its owner can, and only its creator needs to
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() { unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}

func syncDir(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// EnsureDir makes the mailbox's notice directory, 2770 whatever the umask. Mkdir, not MkdirAll: a mailbox
// renamed away is not brought back.
func EnsureDir(box string) error {
	d := filepath.Join(box, Dir)
	err := os.Mkdir(d, 0o770)
	if errors.Is(err, fs.ErrExist) {
		if fi, serr := os.Stat(d); serr == nil && fi.IsDir() {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	return os.Chmod(d, os.ModeSetgid|0o770)
}

// write stores n under its sender's name (the caller holds the lock): a temp file, synced, then a rename;
// the directory and the mailbox are synced before it returns, so a claim can be deleted after.
func write(box, dir string, n *Notice) error {
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf("%s%d.%d", tmpPrefix, os.Getpid(), tmpSeq.Add(1)))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o660)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	err = f.Chmod(0o660)
	if err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, FileName(n.From))); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	return syncDir(box)
}

// stamp marks n changed now: a new msgid, always later than the one it replaces.
func (n *Notice) stamp(o Options) {
	t := o.now()
	ns := t.UnixNano()
	if m := msgidRE.FindStringSubmatch(n.Msgid); m != nil {
		if prev, err := strconv.ParseInt(m[2], 10, 64); err == nil && ns <= prev {
			ns = prev + 1
		}
	}
	if ns%int64(time.Second) == 0 { // a whole second is a probe's
		ns++
	}
	n.Msgid = fmt.Sprintf("%s%s-%d", Prefix, senderHash(n.From), ns)
	n.Date = t.UTC().Format(timeForm)
}

// loadForWrite is the sender's notice to add to (the caller holds the lock): a bad one is set aside and the
// count starts again. One it cannot read now is an error: its count is not to be lost.
func loadForWrite(dir, sender string, o Options) (*Notice, error) {
	name := FileName(sender)
	n, err := readNotice(dir, name)
	switch {
	case err == nil:
		return n, nil
	case isBad(err):
		setAside(dir, name, err, o)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	return &Notice{From: sender}, nil
}

func dropTemps(dir string) {
	for _, n := range entries(dir) {
		if strings.HasPrefix(n, tmpPrefix) {
			os.Remove(filepath.Join(dir, n))
		}
	}
}

// --- counting ----------------------------------------------------------------------------------------

type counted struct {
	claim string
	kind  Kind
	verb  string
	row   Row
}

// headerLines: a stored message's lines up to its first blank line.
func headerLines(path string) ([]string, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, errors.New("not a regular file")
	}
	var out []string
	r := bufio.NewReader(io.LimitReader(f, maxHeaderBytes))
	for {
		line, err := r.ReadString('\n')
		if err != nil { // a last line without its newline is not read
			if errors.Is(err, io.EOF) {
				return out, fi, nil
			}
			return nil, nil, err
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			return out, fi, nil
		}
		out = append(out, line)
	}
}

func headerValue(lines []string, name string) string {
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, name+":"); ok {
			return strings.TrimPrefix(v, " ")
		}
	}
	return ""
}

// Count finishes the mailbox's claims: reads their header blocks and, per sender, merges them into the
// notice under the lock and syncs it. It returns the claims done, for the reaper to delete, with each
// one's attachment folder: those counted, and those with no usable From, which are logged. Claims it could
// not count stay for the next sweep, and the error says so.
func Count(box string, o Options) ([]string, error) {
	dir := filepath.Join(box, Dir)
	var done []string
	var firstErr error
	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	bySender := map[string][]counted{}
	for _, name := range entries(dir) {
		if !strings.HasPrefix(name, ClaimPrefix) {
			continue
		}
		path := filepath.Join(dir, name)
		lines, fi, err := headerLines(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			o.logf("expiry: %s: cannot read %s: %v", box, name, err)
			note(err)
			continue
		}
		from := headerValue(lines, "From")
		if !senderRE.MatchString(from) {
			o.logf("expiry: %s: %s has no From: not counted", box, name)
			done = append(done, name)
			continue
		}
		when := headerValue(lines, "Date")
		if !validTime(when) {
			when = fi.ModTime().UTC().Format(timeForm)
		}
		k, v := Classify(lines)
		bySender[from] = append(bySender[from], counted{name, k, v, Row{1, when, when}})
	}
	senders := make([]string, 0, len(bySender))
	for s := range bySender {
		senders = append(senders, s)
	}
	sort.Strings(senders)
	for _, s := range senders {
		if err := mergeCounted(box, dir, s, bySender[s], o); err != nil {
			o.logf("expiry: %s: cannot count %d messages from %s: %v", box, len(bySender[s]), s, err)
			note(err)
			continue
		}
		for _, c := range bySender[s] {
			done = append(done, c.claim)
		}
	}
	return done, firstErr
}

func mergeCounted(box, dir, sender string, items []counted, o Options) error {
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	dropTemps(dir) // only a writer holding this lock leaves them, and one that still ran would hold it
	n, err := loadForWrite(dir, sender, o)
	if err != nil {
		return err
	}
	for _, c := range items {
		n.add(c.kind, c.verb, c.row)
	}
	n.stamp(o)
	return write(box, dir, n)
}

// --- reading -----------------------------------------------------------------------------------------

// List reads the mailbox's notices, in byte order of sender; nil when it has none. A bad file is set aside.
func List(box string, o Options) []Notice {
	dir := filepath.Join(box, Dir)
	var out []Notice
	for _, name := range noticeFiles(dir) {
		n, err := readNotice(dir, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil && !isBad(err) {
			o.logf("expiry: %s: cannot read %s now: %v", dir, name, err)
			continue
		}
		if err != nil {
			if unlock, lerr := lock(dir); lerr == nil {
				if n2, err2 := readNotice(dir, name); isBad(err2) {
					setAside(dir, name, err2, o)
				} else if err2 == nil {
					out = append(out, *n2)
				}
				unlock()
			}
			continue
		}
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out
}

// Current is the notice whose msgid is exactly msgid now; nil for an older one or none.
func Current(box, msgid string, o Options) *Notice {
	for _, n := range List(box, o) {
		if n.Msgid == msgid {
			return &n
		}
	}
	return nil
}

// Result is what a Clear did.
type Result int

// The outcomes of Clear.
const (
	Missing Result = iota // no notice from that sender, or a msgid it never had
	Cleared               // removed: the msgid was the current one
	Older                 // an older msgid: nothing changed, the newer one is due
	Failed                // it could not be removed
)

// Clear removes the notice msgid names, read again under the lock: only if the msgid is unchanged.
func Clear(box, msgid string, o Options) Result {
	m := msgidRE.FindStringSubmatch(msgid)
	if m == nil {
		return Missing
	}
	dir := filepath.Join(box, Dir)
	unlock, err := lock(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Missing
	}
	if err != nil {
		o.logf("expiry: %s: cannot lock to clear %s: %v", box, msgid, err)
		return Failed
	}
	defer unlock()
	unread := false
	for _, name := range noticeFiles(dir) {
		n, err := readNotice(dir, name)
		if err != nil {
			if isBad(err) {
				setAside(dir, name, err, o)
			} else if !errors.Is(err, fs.ErrNotExist) {
				o.logf("expiry: %s: cannot read %s now: %v", box, name, err)
				unread = true
			}
			continue
		}
		if senderHash(n.From) != m[1] {
			continue
		}
		if n.Msgid == msgid {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				o.logf("expiry: %s: cannot clear %s: %v", box, msgid, err)
				return Failed
			}
			return Cleared
		}
		asked, _ := strconv.ParseInt(m[2], 10, 64)
		have, _ := strconv.ParseInt(msgidRE.FindStringSubmatch(n.Msgid)[2], 10, 64)
		if asked < have {
			return Older
		}
		return Missing
	}
	if unread { // it may be the one asked for
		return Failed
	}
	return Missing
}
