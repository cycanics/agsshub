package expiry

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	t1 = "2026-11-01T05:00:00Z"
	t2 = "2026-11-01T06:00:00Z"
	t3 = "2026-11-01T07:00:00Z"
)

func row(c int64) Row { return Row{c, t1, t2} }

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s:\n got [%v]\nwant [%v]", what, got, want)
	}
}

func notice(verbs map[string]Row) Notice {
	return Notice{From: "a@b.ssh", Msgid: "expired-x-1", Date: t3, Verbs: verbs}
}

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		name   string
		header []string
		kind   Kind
		verb   string
	}{
		{"a Gap field", []string{"From: a", "Gap: restarted"}, KindGap, ""},
		{"Gap wins over Subject", []string{"Subject: in.x", "Gap: r"}, KindGap, ""},
		{"no Subject", []string{"From: a"}, KindNoSubject, ""},
		{"a blank Subject", []string{"Subject: "}, KindNoSubject, ""},
		{"a Subject of blanks", []string{"Subject:  \t "}, KindNoSubject, ""},
		{"a verb", []string{"Subject: in.action"}, KindVerb, "in.action"},
		{"the first word of free text, space", []string{"Subject: in.x and more"}, KindVerb, "in.x"},
		{"the first word of free text, tab", []string{"Subject: in.y\tmore"}, KindVerb, "in.y"},
		{"the alphabet", []string{"Subject: aZ0._/-"}, KindVerb, "aZ0._/-"},
		{"64 characters", []string{"Subject: " + strings.Repeat("a", 64)}, KindVerb, strings.Repeat("a", 64)},
		{"65 characters", []string{"Subject: " + strings.Repeat("a", 65)}, KindOthers, ""},
		{"outside the alphabet", []string{"Subject: héllo"}, KindOthers, ""},
		{"a word that looks special", []string{"Subject: [gap]"}, KindOthers, ""},
		{"a word that looks like a prefix row", []string{"Subject: out.*"}, KindOthers, ""},
		{"the first Subject counts", []string{"Subject: one", "Subject: two"}, KindVerb, "one"},
		{"a folded line is not a field", []string{"Re: x", " Subject: no"}, KindNoSubject, ""},
	} {
		k, v := Classify(c.header)
		if k != c.kind || v != c.verb {
			t.Errorf("%s: got %d %q want %d %q", c.name, k, v, c.kind, c.verb)
		}
	}
}

func TestTablesBytes(t *testing.T) {
	n := Notice{From: "commhub@home.ssh", Msgid: "expired-3f2a-1", Date: "2026-11-08T05:00:00Z", Expired: 17,
		Verbs: map[string]Row{"in.action": {2, "2026-11-01T06:02:00Z", "2026-11-01T08:15:00Z"}, "out.a": {10, "2026-11-01T05:10:00Z", "2026-11-01T09:40:00Z"}},
		Gap:   &Row{1, "2026-11-01T05:01:00Z", "2026-11-01T05:01:00Z"}, Others: &Row{3, "2026-11-01T05:30:00Z", "2026-11-01T07:10:00Z"},
		NoSubject: &Row{1, "2026-11-01T07:00:00Z", "2026-11-01T07:00:00Z"}}
	tables := "verb\tcount\toldest\tnewest\n" +
		"in.action\t2\t2026-11-01T06:02:00Z\t2026-11-01T08:15:00Z\n" +
		"out.a\t10\t2026-11-01T05:10:00Z\t2026-11-01T09:40:00Z\n" +
		"\n" +
		"special\tcount\toldest\tnewest\n" +
		"[gap]\t1\t2026-11-01T05:01:00Z\t2026-11-01T05:01:00Z\n" +
		"[others]\t3\t2026-11-01T05:30:00Z\t2026-11-01T07:10:00Z\n" +
		"[no-subject]\t1\t2026-11-01T07:00:00Z\t2026-11-01T07:00:00Z\n"
	eq(t, "both tables, one blank line between, none at the end", Tables(n), tables)
	eq(t, "raw", RenderRaw(n), "From: commhub@home.ssh\nDate: 2026-11-08T05:00:00Z\nExpired: 17\n\n"+tables)
	onlyV := Notice{Verbs: map[string]Row{"a": row(1)}}
	eq(t, "no special rows: its table is left out", Tables(onlyV), "verb\tcount\toldest\tnewest\na\t1\t"+t1+"\t"+t2+"\n")
	onlyS := Notice{Others: &Row{1, t1, t2}}
	eq(t, "no verb rows: its table is left out", Tables(onlyS), "special\tcount\toldest\tnewest\n[others]\t1\t"+t1+"\t"+t2+"\n")
}

func names(rows []shownRow) string {
	var o []string
	for _, r := range rows {
		o = append(o, fmt.Sprintf("%s=%d", r.name, r.Count))
	}
	return strings.Join(o, " ")
}

func TestLongVerbs(t *testing.T) {
	v32 := strings.Repeat("a", 32)
	rows, others := groupRows(notice(map[string]Row{v32: row(1)}))
	eq(t, "32 characters stay", names(rows), v32+"=1")
	eq(t, "and nothing in others", others == nil, true)

	long := "aaaa.bbbb-cccc_dddd/eeee.ffffffff.gggg" // 38 characters
	rows, _ = groupRows(notice(map[string]Row{long: row(2)}))
	eq(t, "over 32: its longest prefix ending in a separator within 32", names(rows), "aaaa.bbbb-cccc_dddd/eeee.*=2")

	sepAt32 := strings.Repeat("a", 31) + ".bbb"
	rows, _ = groupRows(notice(map[string]Row{sepAt32: row(1)}))
	eq(t, "a separator at character 32 counts", names(rows), strings.Repeat("a", 31)+".*=1")
	sepAt33 := strings.Repeat("a", 32) + ".bbb"
	rows, others = groupRows(notice(map[string]Row{sepAt33: row(4)}))
	eq(t, "one at character 33 does not: no prefix, others", names(rows)+fmt.Sprint(others.Count), "4")

	a, b := "pre.xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", "pre.yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"
	n := notice(map[string]Row{a: {1, t2, t2}, b: {2, t1, t3}})
	rows, _ = groupRows(n)
	eq(t, "rows of one name sum", names(rows), "pre.*=3")
	eq(t, "lowest oldest, highest newest", rows[0].Oldest+rows[0].Newest, t1+t3)
}

func nVerbs(n int) map[string]Row {
	m := map[string]Row{}
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("verb%02d", i)] = row(int64(i + 1))
	}
	return m
}

func TestSixteenRows(t *testing.T) {
	rows, others := groupRows(notice(nVerbs(16)))
	eq(t, "16 rows stay", len(rows), 16)
	eq(t, "with nothing in others", others == nil, true)

	rows, others = groupRows(notice(nVerbs(17)))
	eq(t, "17 rows with no shared prefix: 16 stay", len(rows), 16)
	eq(t, "the one with the lowest count goes to others", others.Count, 1)
	if strings.Contains(names(rows), "verb00=") {
		t.Errorf("verb00 stayed: %s", names(rows))
	}

	m := nVerbs(17)
	m["zz"], m["yy"] = row(1), row(1)
	m["verb00"] = row(9)
	rows, others = groupRows(notice(m))
	eq(t, "19 rows, three of count 1: 16 stay", len(rows), 16)
	// a tie on count: the last in byte order goes first, zz, then yy; then verb01, the next lowest
	eq(t, "the ties and the next lowest are gone", strings.Contains(names(rows), "zz=") || strings.Contains(names(rows), "yy=") || strings.Contains(names(rows), "verb01="), false)
	eq(t, "three went to others", others.Count, 1+1+2)

	m = nVerbs(16)
	m["in.a"], m["in.b"], m["in.sub.c"], m["in.sub.d"] = row(1), row(1), row(1), row(1)
	delete(m, "verb00")
	delete(m, "verb01")
	rows, others = groupRows(notice(m))
	// 18 rows: the longest shared prefix merges first (in.sub.), leaving 17; then in. takes in.sub.* along
	eq(t, "the longest shared prefix merges first, then all of the next", strings.Contains(names(rows), "in.*=4"), true)
	eq(t, "and no prefix row is left under it", strings.Contains(names(rows), "in.sub.*"), false)
	eq(t, "nothing went to others", others == nil, true)
	eq(t, "the merges leave 15: the last one overshoots 16", len(rows), 15)

	m = nVerbs(15)
	m["a.one"], m["a.two"], m["b.one"], m["b.two"] = row(1), row(1), row(1), row(1)
	rows, _ = groupRows(notice(m))
	// 19 rows: a. and b. tie on length; a. merges first, leaving 18; b. next, 17; then the smallest goes
	eq(t, "a tie between prefixes of one length: both merge in byte order", strings.Contains(names(rows), "a.*=2") && strings.Contains(names(rows), "b.*=2"), true)

	m = nVerbs(16)
	m["a.one"], m["a.two"] = row(5), row(5)
	delete(m, "verb00")
	rows, _ = groupRows(notice(m))
	eq(t, "merge stops at 16", len(rows), 16)
	eq(t, "a.* is its rows", strings.Contains(names(rows), "a.*=10"), true)
	m = nVerbs(15)
	m["c.x"], m["c.y"], m["b.x"], m["b.y"] = row(5), row(5), row(5), row(5)
	delete(m, "verb00")
	delete(m, "verb01")
	rows, _ = groupRows(notice(m))
	// 17 rows: only one merge is needed; of b. and c., the first in byte order
	eq(t, "the prefix first in byte order merges", strings.Contains(names(rows), "b.*=10") && strings.Contains(names(rows), "c.x=5"), true)
}

func TestAPrefixRowMergesWithItsOwnVerb(t *testing.T) {
	m := nVerbs(15)
	long := "out." + strings.Repeat("x", 40)
	m[long], m["out.a"] = row(1), row(1)
	rows, _ := groupRows(notice(m))
	eq(t, "a cut verb and a verb under it", len(rows), 16)
	eq(t, "share one row", strings.Contains(names(rows), "out.*=2"), true)
}

func TestSameRowsSameBytes(t *testing.T) {
	var keys []string
	verbs := nVerbs(40)
	for _, v := range []string{"in.a", "in.b", "in.c.d", "in.c.e", "out.x", "out.y", "a-b", "a-c", strings.Repeat("l", 40), "m/" + strings.Repeat("n", 40)} {
		verbs[v] = row(3)
	}
	for v := range verbs {
		keys = append(keys, v)
	}
	want := ""
	for i := 0; i < 30; i++ {
		rand.Shuffle(len(keys), func(a, b int) { keys[a], keys[b] = keys[b], keys[a] })
		n := Notice{From: "s@o.ssh"}
		for _, k := range keys {
			n.add(KindVerb, k, verbs[k])
		}
		got := Tables(n)
		if want == "" {
			want = got
		} else if got != want {
			t.Fatalf("the same rows rendered differently:\n%s\n---\n%s", got, want)
		}
	}
	rows, _ := groupRows(notice(verbs))
	if len(rows) > 16 {
		t.Errorf("over 16 rows: %d", len(rows))
	}
}

func TestNeverOver16AndNothingLost(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	seps := "./-_"
	for trial := 0; trial < 200; trial++ {
		verbs := map[string]Row{}
		var total int64
		for i := 0; i < 1+r.Intn(200); i++ {
			v := fmt.Sprintf("v%d", r.Intn(50))
			for j := r.Intn(4); j > 0; j-- {
				v += string(seps[r.Intn(4)]) + strings.Repeat("x", r.Intn(15))
			}
			if _, ok := verbs[v]; !ok && verbRE.MatchString(v) {
				verbs[v] = row(1)
				total++
			}
		}
		rows, others := groupRows(notice(verbs))
		if len(rows) > 16 {
			t.Fatalf("trial %d: %d rows", trial, len(rows))
		}
		var sum int64
		for _, x := range rows {
			sum += x.Count
		}
		if others != nil {
			sum += others.Count
		}
		if sum != total {
			t.Fatalf("trial %d: counted %d of %d", trial, sum, total)
		}
	}
}

func Test1025Verbs(t *testing.T) {
	var n Notice
	for i := 0; i < 1025; i++ {
		n.add(KindVerb, fmt.Sprintf("v%04d", i), row(1))
	}
	eq(t, "1,024 verb rows stored", len(n.Verbs), 1024)
	eq(t, "the 1,025th in others", n.Others.Count, 1)
	eq(t, "the total has them all", n.Expired, 1025)
	n.add(KindVerb, "v0000", row(1))
	eq(t, "a verb it has still counts in its row", n.Verbs["v0000"].Count, 2)
	eq(t, "and not in others", n.Others.Count, 1)
}

func TestAVerbThatLooksSpecial(t *testing.T) {
	for _, s := range []string{"[gap]", "[others]", "[no-subject]", "out.*", "*"} {
		if verbRE.MatchString(s) {
			t.Errorf("%q would read as a real verb", s)
		}
		if k, _ := Classify([]string{"Subject: " + s}); k != KindOthers {
			t.Errorf("%q: not others", s)
		}
	}
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, Dir), 0o770)
	n := Notice{From: "a@b.ssh", Msgid: Prefix + senderHash("a@b.ssh") + "-1", Date: t1, Expired: 1, Verbs: map[string]Row{"[gap]": row(1)}}
	b, _ := json.Marshal(n)
	os.WriteFile(filepath.Join(dir, Dir, FileName(n.From)), b, 0o660)
	eq(t, "a stored row that imitates one is a bad file", len(List(dir, Options{})), 0)
}

// --- on disk ---------------------------------------------------------------------------------------

type box struct {
	t    *testing.T
	path string
	now  time.Time
	logs []string
}

func newBox(t *testing.T) *box {
	b := &box{t: t, path: filepath.Join(t.TempDir(), "mb"), now: time.Date(2026, 11, 8, 5, 0, 0, 1, time.UTC)}
	os.Mkdir(b.path, 0o770)
	if err := EnsureDir(b.path); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *box) o() Options {
	return Options{Now: func() time.Time { return b.now }, Logf: func(f string, a ...any) { b.logs = append(b.logs, fmt.Sprintf(f, a...)) }}
}

func (b *box) claim(mid, content string) {
	b.t.Helper()
	if err := os.WriteFile(filepath.Join(b.path, Dir, ClaimPrefix+mid), []byte(content), 0o640); err != nil {
		b.t.Fatal(err)
	}
}

func msg(from, date string, fields ...string) string {
	s := "From: " + from + "\n"
	if date != "" {
		s += "Date: " + date + "\n"
	}
	s += "Serial: 1\n"
	for _, f := range fields {
		s += f + "\n"
	}
	return s + "\nbody\nSubject: not.this\n"
}

func TestCountAndList(t *testing.T) {
	b := newBox(t)
	b.claim("m1", msg("z@o.ssh", t1, "Subject: in.x"))
	b.claim("m2", msg("z@o.ssh", t2, "Subject: in.x and more"))
	b.claim("m3", msg("z@o.ssh", t3, "Gap: r"))
	b.claim("m4", msg("a/ws-x@o.ssh", t1))
	b.claim("m5", msg("a/ws-x@o.ssh", t2, "Subject: nön"))
	done, err := Count(b.path, b.o())
	eq(t, "no error", err, nil)
	eq(t, "every claim done", strings.Join(done, " "), ".claim.m4 .claim.m5 .claim.m1 .claim.m2 .claim.m3")
	ns := List(b.path, b.o())
	eq(t, "two notices, in byte order of sender", len(ns), 2)
	eq(t, "first", ns[0].From, "a/ws-x@o.ssh")
	eq(t, "'/' becomes '~' in the name", FileName(ns[0].From), "a~ws-x@o.ssh.json")
	if _, err := os.Stat(filepath.Join(b.path, Dir, "a~ws-x@o.ssh.json")); err != nil {
		t.Error(err)
	}
	eq(t, "the body's lines are not fields", Tables(ns[1]),
		"verb\tcount\toldest\tnewest\nin.x\t2\t"+t1+"\t"+t2+"\n\nspecial\tcount\toldest\tnewest\n[gap]\t1\t"+t3+"\t"+t3+"\n")
	eq(t, "the total", ns[1].Expired, 3)
	eq(t, "Date is when it changed", ns[1].Date, "2026-11-08T05:00:00Z")
	if !msgidRE.MatchString(ns[1].Msgid) || !strings.HasPrefix(ns[1].Msgid, "expired-"+senderHash("z@o.ssh")+"-") {
		t.Errorf("msgid %s", ns[1].Msgid)
	}
	eq(t, "the msgid carries the nanoseconds", strings.HasSuffix(ns[1].Msgid, fmt.Sprint(b.now.UnixNano())), true)
}

func TestNoDateTakesTheModificationTime(t *testing.T) {
	b := newBox(t)
	b.claim("m1", msg("z@o.ssh", ""))
	mt := time.Date(2026, 10, 1, 1, 2, 3, 0, time.UTC)
	os.Chtimes(filepath.Join(b.path, Dir, ClaimPrefix+"m1"), mt, mt)
	Count(b.path, b.o())
	eq(t, "oldest", List(b.path, b.o())[0].NoSubject.Oldest, "2026-10-01T01:02:03Z")
}

func TestAClaimWithNoFrom(t *testing.T) {
	b := newBox(t)
	b.claim("m1", "Date: "+t1+"\n\nbody\n")
	b.claim("m2", msg("../evil", t1))
	done, err := Count(b.path, b.o())
	eq(t, "no error", err, nil)
	eq(t, "done, for the reaper to delete", strings.Join(done, " "), ".claim.m1 .claim.m2")
	eq(t, "no notice", len(List(b.path, b.o())), 0)
	eq(t, "logged, each", len(b.logs), 2)
	eq(t, "saying so", strings.Contains(b.logs[0], "has no From"), true)
}

func TestTheLongestIdentityIsASender(t *testing.T) {
	b := newBox(t)
	long := "run." + strings.Repeat("p", 64) + "+" + strings.Repeat("i", 32) + "/ws-" + strings.Repeat("w", 31) + "@" + strings.Repeat("o", 64) + ".ssh"
	b.claim("m1", msg(long, t1))
	done, err := Count(b.path, b.o())
	eq(t, "no error", err, nil)
	eq(t, "done", len(done), 1)
	eq(t, "counted", len(List(b.path, b.o())), 1)
	b.claim("m2", "not a header\n\n")
	os.WriteFile(filepath.Join(b.path, Dir, FileName(long)), []byte("{"), 0o660)
	eq(t, "and read as bad under its own long name", len(List(b.path, b.o())), 0)
	if _, err := os.Stat(filepath.Join(b.path, Dir, ".bad."+FileName(long))); err != nil {
		t.Error(err)
	}
}

func TestCumulativeAndMsgidAdvances(t *testing.T) {
	b := newBox(t)
	b.claim("m1", msg("z@o.ssh", t1, "Subject: a"))
	Count(b.path, b.o())
	os.Remove(filepath.Join(b.path, Dir, ClaimPrefix+"m1"))
	first := List(b.path, b.o())[0].Msgid
	b.claim("m2", msg("z@o.ssh", t2, "Subject: a"))
	Count(b.path, b.o()) // the same clock
	n := List(b.path, b.o())[0]
	eq(t, "counts add", n.Verbs["a"].Count, 2)
	eq(t, "the total too", n.Expired, 2)
	if n.Msgid == first {
		t.Error("a change at the same instant kept the msgid")
	}
}

func TestClear(t *testing.T) {
	b := newBox(t)
	b.claim("m1", msg("z@o.ssh", t1, "Subject: a"))
	Count(b.path, b.o())
	os.Remove(filepath.Join(b.path, Dir, ClaimPrefix+"m1"))
	older := List(b.path, b.o())[0].Msgid
	b.now = b.now.Add(time.Second)
	b.claim("m2", msg("z@o.ssh", t2, "Subject: a"))
	Count(b.path, b.o())
	cur := List(b.path, b.o())[0].Msgid

	eq(t, "an older msgid changes nothing", Clear(b.path, older, b.o()), Older)
	eq(t, "the notice stays", List(b.path, b.o())[0].Msgid, cur)
	eq(t, "Current does not read an older one", Current(b.path, older, b.o()) == nil, true)
	eq(t, "Current reads this one", Current(b.path, cur, b.o()).Msgid, cur)
	eq(t, "a msgid it never had", Clear(b.path, Prefix+senderHash("z@o.ssh")+"-9999999999999999999", b.o()), Missing)
	eq(t, "another sender's", Clear(b.path, Prefix+senderHash("y@o.ssh")+"-1", b.o()), Missing)
	eq(t, "malformed", Clear(b.path, "expired-nope", b.o()), Missing)
	eq(t, "the current one", Clear(b.path, cur, b.o()), Cleared)
	eq(t, "gone", len(List(b.path, b.o())), 0)
	eq(t, "cleared twice: nothing from that sender", Clear(b.path, cur, b.o()), Missing)
	eq(t, "no notice directory at all", Clear(filepath.Join(b.path, "nope"), cur, b.o()), Missing)
}

func TestBadFile(t *testing.T) {
	for name, content := range map[string]string{
		"not JSON":   "{",
		"wrong from": `{"from":"other@o.ssh","msgid":"expired-0000000000000000-1","date":"` + t1 + `","expired":1}`,
		"empty":      "",
	} {
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			fn := FileName("z@o.ssh")
			os.WriteFile(filepath.Join(b.path, Dir, fn), []byte(content), 0o660)
			eq(t, "a reader skips it", len(List(b.path, b.o())), 0)
			if _, err := os.Stat(filepath.Join(b.path, Dir, ".bad."+fn)); err != nil {
				t.Error("not set aside:", err)
			}
			eq(t, "logged", len(b.logs) > 0, true)
			os.WriteFile(filepath.Join(b.path, Dir, fn), []byte(content), 0o660)
			b.claim("m1", msg("z@o.ssh", t1, "Subject: a"))
			done, err := Count(b.path, b.o())
			eq(t, "counting goes on", err, nil)
			eq(t, "its claim is done", len(done), 1)
			n := List(b.path, b.o())
			eq(t, "the count starts again", n[0].Expired, 1)
		})
	}
}

func TestAReadFailureIsNotABadFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	b := newBox(t)
	b.claim("m1", msg("z@o.ssh", t1, "Subject: a"))
	Count(b.path, b.o())
	os.Remove(filepath.Join(b.path, Dir, ClaimPrefix+"m1"))
	p := filepath.Join(b.path, Dir, FileName("z@o.ssh"))
	id := List(b.path, b.o())[0].Msgid
	os.Chmod(p, 0)
	eq(t, "not listed now", len(List(b.path, b.o())), 0)
	eq(t, "not cleared", Clear(b.path, id, b.o()), Failed)
	b.claim("m2", msg("z@o.ssh", t2, "Subject: a"))
	done, err := Count(b.path, b.o())
	eq(t, "its claim stays for the next sweep", len(done), 0)
	eq(t, "and says so", err != nil, true)
	eq(t, "not set aside", func() bool { _, e := os.Stat(filepath.Join(b.path, Dir, ".bad."+FileName("z@o.ssh"))); return e != nil }(), true)
	os.Chmod(p, 0o660)
	done, err = Count(b.path, b.o())
	eq(t, "it counts once it can be read", len(done), 1)
	eq(t, "no error", err, nil)
	eq(t, "its count is kept", List(b.path, b.o())[0].Expired, 2)
}

func TestClearCannotLockIsAFault(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens anything")
	}
	b := newBox(t)
	b.claim("m1", msg("z@o.ssh", t1))
	Count(b.path, b.o())
	id := List(b.path, b.o())[0].Msgid
	lk := filepath.Join(b.path, Dir, lockName)
	os.Chmod(lk, 0)
	eq(t, "a fault, not a missing notice", Clear(b.path, id, b.o()), Failed)
	os.Chmod(lk, 0o660)
	eq(t, "then it clears", Clear(b.path, id, b.o()), Cleared)
}

func TestModesWhateverTheUmask(t *testing.T) {
	old := unix.Umask(0o077)
	defer unix.Umask(old)
	b := newBox(t)
	fi, _ := os.Stat(filepath.Join(b.path, Dir))
	eq(t, ".expired", fi.Mode().Perm()|fi.Mode()&os.ModeSetgid, os.ModeSetgid|0o770)
	b.claim("m1", msg("z@o.ssh", t1))
	Count(b.path, b.o())
	fi, _ = os.Stat(filepath.Join(b.path, Dir, FileName("z@o.ssh")))
	eq(t, "the notice", fi.Mode().Perm(), os.FileMode(0o660))
	fi, _ = os.Stat(filepath.Join(b.path, Dir, lockName))
	eq(t, "its lock", fi.Mode().Perm(), os.FileMode(0o660))
}

func TestEnsureDirDoesNotBringAMailboxBack(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "renamed")
	if EnsureDir(gone) == nil {
		t.Error("it made the directory")
	}
	if _, err := os.Stat(gone); err == nil {
		t.Error("a renamed mailbox came back")
	}
}

func TestAWriterWaitsForTheLock(t *testing.T) {
	b := newBox(t)
	unlock, err := lock(filepath.Join(b.path, Dir))
	if err != nil {
		t.Fatal(err)
	}
	b.claim("m1", msg("z@o.ssh", t1))
	done := make(chan struct{})
	go func() { Count(b.path, Options{}); close(done) }()
	select {
	case <-done:
		t.Fatal("Count did not wait for the lock")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	<-done
}

func TestLeftoverTempFilesGo(t *testing.T) {
	b := newBox(t)
	tmp := filepath.Join(b.path, Dir, tmpPrefix+"1.1")
	os.WriteFile(tmp, []byte("half"), 0o660)
	b.claim("m1", msg("z@o.ssh", t1))
	Count(b.path, b.o())
	if _, err := os.Stat(tmp); err == nil {
		t.Error("a dead writer's temp file stayed")
	}
}

func TestAStoredNoticeIsNeverAProbe(t *testing.T) {
	whole := time.Unix(1793999999, 0)
	n := Notice{From: "a@b.ssh"}
	n.stamp(Options{Now: func() time.Time { return whole }})
	if IsProbe(n.Msgid, "a@b.ssh") {
		t.Errorf("a change on a whole second stamps %s", n.Msgid)
	}
	if want := "-1793999999000000001"; !strings.HasSuffix(n.Msgid, want) {
		t.Errorf("%s: want the next nanosecond", n.Msgid)
	}
}

func TestProbe(t *testing.T) {
	at := time.Unix(1793999999, 123456789)
	p := Probe("a@b.ssh", Options{Now: func() time.Time { return at }})
	h := senderHash("a@b.ssh")
	id := "expired-" + h + "-1793999999000000000"
	if p.Msgid != id {
		t.Errorf("msgid %s", p.Msgid)
	}
	want := "From: a@b.ssh\nDate: " + at.UTC().Format(timeForm) + "\nExpired: 0\n\n"
	if got := RenderRaw(p); got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
	for _, c := range []struct {
		msgid, who string
		want       bool
	}{
		{id, "a@b.ssh", true},
		{id, "c@b.ssh", false},
		{"expired-" + h + "-1793999999000000001", "a@b.ssh", false},
		{"expired-" + h + "-0", "a@b.ssh", false},
		{"expired-" + h + "-99999999999999999999", "a@b.ssh", false},
		{"expired-" + h[:15] + "-1793999999000000000", "a@b.ssh", false},
		{"expiredx-" + h + "-1793999999000000000", "a@b.ssh", false},
	} {
		if got := IsProbe(c.msgid, c.who); got != c.want {
			t.Errorf("IsProbe(%s, %s) = %v", c.msgid, c.who, got)
		}
	}
}
