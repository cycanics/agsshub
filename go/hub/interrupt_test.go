package hub

// An interrupted publication, a raw read, field values, what readers see while publications land,
// concurrent publications, a bag's tree killed mid-copy, and a mailbox removed mid-publication.

import (
	"archive/tar"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// dieAt makes the next call of testHook at point end its session with exit 137, as a crash there would:
// the deferred lock releases run, as the kernel releases a dead process's locks; nothing else of it does.
func dieAt(t *testing.T, point string) {
	fired := false
	testHook = func(p string) {
		if p == point && !fired {
			fired = true
			panic(exitPanic{137})
		}
	}
	t.Cleanup(func() { testHook = func(string) {} })
}

func noHook() { testHook = func(string) {} }

// view: a subscription's event seqs, "1 2 3".
func (x *th) view(sub, owner string) string { return seqs(x.dis(owner, "recv "+sub+" --peek").out) }

// stages: the hub's publication stages (inbox/.stage.*).
func (x *th) stages() []string {
	var st []string
	for _, n := range globNamesAll(filepath.Join(x.state, "inbox")) {
		if strings.HasPrefix(n, ".stage.") {
			st = append(st, filepath.Join(x.state, "inbox", n))
		}
	}
	return st
}

// leftOf: what publications to topic left: the entries of topics/<topic>/, then those of inbox/<topic>/ and
// the hub's publication stages, each as .stage.*.
// The topic's record is its own (event ids), not a publication's: it is left out.
func (x *th) leftOf(topic string) string {
	st := globNamesAll(filepath.Join(x.state, "inbox", fsName(topic)))
	for range x.stages() {
		st = append(st, ".stage.*")
	}
	var td []string
	for _, n := range globNamesAll(filepath.Join(x.state, "topics", fsName(topic))) {
		if n != "record" {
			td = append(td, n)
		}
	}
	return strings.Join(td, " ") + "|" + strings.Join(st, " ")
}

// reapStages ages the publication stages past the reaper's hour and sweeps: how many there were, and are.
func (x *th) reapStages() string {
	left := x.stages()
	// a stage is one file with what it was linked to: backdating it backdates those, so their arrival
	// stamps are put back after the sweep
	kept := map[string]time.Time{}
	filepath.Walk(filepath.Join(x.state, "inbox"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, "."+msgExt) {
			kept[p] = fi.ModTime()
		}
		return nil
	})
	old := time.Now().Add(-2 * time.Hour)
	for _, p := range left {
		os.Chtimes(p, old, old)
	}
	x.h.Reap()
	for p, t := range kept {
		os.Chtimes(p, t, t)
	}
	return fmt.Sprint(len(left), " ", len(x.stages()))
}

// restart: a new hub over the same state, as after a crash: what it held in memory (records, done) is gone.
func (x *th) restart() {
	x.h = New(x.h.cfg)
}

// A publication killed at each point, then the hub restarted: what each subscription has, what a retry
// does (a new publication: the hub's next seq), and what is left on disk: only its stage, which the reaper
// removes. A seq whose record was written is used: a gap readers see.
func TestInterruptedPublication(t *testing.T) {
	x := twoRuns(t)
	k := 0
	for _, point := range []string{"stage", "stored", "locked", "recorded", "linked", "fanned"} {
		for _, retry := range []bool{false, true} {
			k++
			tx := fmt.Sprintf("topic+crash%d@acme.ssh", k)
			x.mkTopic(tx, alice)
			xa := strings.TrimSpace(x.dis(alice, "sub "+tx).out)
			xb := strings.TrimSpace(x.dis(bob, "sub "+tx).out)
			first, _ := x.firstSub(tx, xa)
			x.dis(alice, "send "+tx+" -m", "x\n")
			dieAt(t, point)
			died := x.dis(alice, "send "+tx+" -m", "x\n").code
			noHook()
			x.restart()
			left := x.leftOf(tx)
			reaped := x.reapStages()
			out := "-"
			if retry {
				out = fmt.Sprint(strings.HasSuffix(x.dis(alice, "send "+tx+" -m", "x\n").out, " subs=2\n"))
			}
			x.dis(alice, "send "+tx+" -m", "x\n")
			va, vb := x.view(xa, alice), x.view(xb, bob)
			vf, vo := va, vb // the first subscription the fan-out reached, and the other
			if first != xa {
				vf, vo = vb, va
			}
			got := fmt.Sprintf("%d|%s|%s|%s|%s|%s", died, left, reaped, out, vf, vo)
			crash := "137|policy subs|.stage.*|1 0|"
			switch {
			case point != "recorded" && point != "linked" && point != "fanned" && !retry:
				eq(t, "killed at '"+point+"': none has it, no seq used", got, crash+"-|1 2|1 2")
			case point != "recorded" && point != "linked" && point != "fanned":
				eq(t, "killed at '"+point+"', retried: a new publication", got, crash+"true|1 2 3|1 2 3")
			case point == "recorded" && !retry:
				eq(t, "killed after the record: its seq used, a gap in both", got, crash+"-|1 3|1 3")
			case point == "recorded":
				eq(t, "killed after the record, retried", got, crash+"true|1 3 4|1 3 4")
			case point == "linked" && !retry:
				eq(t, "killed after linking the first: only it has it; the other sees the gap", got, crash+"-|1 2 3|1 3")
			case point == "linked":
				eq(t, "killed after linking the first, retried: a new publication to both", got, crash+"true|1 2 3 4|1 3 4")
			case !retry:
				eq(t, "killed after the fan-out: both have it", got, crash+"-|1 2 3|1 2 3")
			default:
				eq(t, "killed after the fan-out, retried: a new publication in both", got, crash+"true|1 2 3 4|1 2 3 4")
			}
			eq(t, point+": nothing left after the next publications", x.leftOf(tx), "policy subs|")
		}
	}
}

func TestRawReadUnderAck(t *testing.T) {
	x := twoRuns(t)
	tn := "topic+raw@acme.ssh"
	x.mkTopic(tn, alice)
	ns := strings.TrimSpace(x.dis(bob, "sub "+tn).out)
	r1 := pubMsgid(x.dis(alice, "send "+tn+" -m:rfc5322", "Subject: s\n\nthe body\n").out)
	rd := x.dis(bob, "recv "+ns+" --id "+r1+" --peek:rfc5322")
	eq(t, "a raw read: whole", fmt.Sprint(rd.code, "|", dateless(rd.out)), "0|From: "+alice+"\nDate: D\nSerial: 1\nSubject: s\n\nthe body")
	code(t, "acked", x.dis(bob, "recv_ack "+ns+" "+r1), 0)
	code(t, "after it, no such message", x.dis(bob, "recv "+ns+" --id "+r1+" --peek:rfc5322"), 3)
}

func TestFieldValues(t *testing.T) {
	x := twoRuns(t)
	bad := ""
	for _, v := range []string{"a\rb", "a\x01b", "a\x1bb", "a\x7fb"} {
		r := x.dis(alice, "send "+bob+" -m:rfc5322", "Subject: "+v+"\n\nb\n")
		if r.code != 2 || !strings.Contains(r.err, "field Subject: a control character in its value") {
			bad += fmt.Sprintf("%q=%d ", v, r.code)
		}
	}
	eq(t, "a value with a CR, or any control character but TAB -> 2", bad, "")
	code(t, "a folded continuation too", x.dis(alice, "send "+bob+" -m:rfc5322", "Subject: one\n two\r\n\nb\n"), 2)
	tn := "topic+fields@acme.ssh"
	x.mkTopic(tn, alice)
	code(t, "and a Gap's reason", x.dis(alice, "send "+tn+" -m:rfc5322", "Gap: start\r\n\n"), 2)
	bad = ""
	for _, c := range []struct{ what, in string }{
		{"a Latin-1 byte", "Re: caf\xe9\n\nb\n"},
		{"a Subject", "Subject: caf\xe9\n\nb\n"},
		{"a cut sequence", "Deadline: a\xc3\n\nb\n"},
		{"a folded continuation", "Subject: one\n tw\xffo\n\nb\n"},
	} {
		r := x.dis(alice, "send "+bob+" -m:rfc5322", c.in)
		if r.code != 2 || !strings.Contains(r.err, "is not valid UTF-8") {
			bad += fmt.Sprintf("%s=%d ", c.what, r.code)
		}
	}
	eq(t, "a value that is not valid UTF-8 -> 2 (D34)", bad, "")
	f := x.start(x.adm(bob), "recv --follow --peek")
	waitFor(t, 2*time.Second, "bob follows", x.followed(bob))
	code(t, "a live send's header block too", x.dis(alice, "send "+bob+" --live -m:rfc5322", "Re: caf\xe9\n\nb\n"), 2)
	f.cancel()
	f.wait(t, 2*time.Second)
	bag := string(makeTar(t, map[string]string{"caf\xe9.txt": "m\n", "cafe.txt": "m\n"}))
	code(t, "the same bag with a UTF-8 --msg path is taken", x.dis(alice, "send_bag "+bob+" --id fv-b --msg cafe.txt", bag), 0)
	x.dis(bob, "recv_ack fv-b")
	r := x.dis(alice, "send_bag "+bob+" --msg caf\xe9.txt", bag)
	eq(t, "a bag's --msg path not valid UTF-8 -> 2", fmt.Sprint(r.code, " ", strings.Contains(r.err, "bad --msg path")), "2 true")
	eq(t, "nothing stored", ids(x.dis(bob, "recv --peek").out), "")
	code(t, "a TAB, and UTF-8, stay a value's own", x.dis(alice, "send "+bob+" --id fv-1 -m:rfc5322", "Subject: a\tb caf\xc3\xa9\n\nb\n"), 0)
	x.dis(alice, "send "+bob+" --id fv-2 -m:rfc5322", "Subject: a\x00b\n\nb\n")
	eq(t, "a NUL in a header block is dropped", hdrGet(headerBlock(filepath.Join(x.state, "inbox", fsName(bob), "fv-2.msg")), "Subject"), "ab")
}

// prefixes counts the listings that are not 1..k.
func prefixes(listings []string) int {
	bad := 0
	for _, l := range listings {
		var w []string
		for j := range strings.Fields(l) {
			w = append(w, fmt.Sprint(j+1))
		}
		if l != strings.Join(w, " ") {
			bad++
		}
	}
	return bad
}

func TestReadersSeeNumberOrder(t *testing.T) {
	x := twoRuns(t)
	tv := "topic+order@acme.ssh"
	x.mkTopic(tv, alice)
	vs := strings.TrimSpace(x.dis(bob, "sub "+tv).out)
	f := x.start(x.adm(bob), "recv "+vs+" --follow --peek", "")
	waitFor(t, 2*time.Second, "the follow", x.followed(vs))
	var listings []string
	var lmu sync.Mutex
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			l := seqs(x.dis(bob, "recv "+vs+" --peek").out)
			lmu.Lock()
			listings = append(listings, l)
			lmu.Unlock()
		}
	}()
	var wg sync.WaitGroup
	for i := 1; i <= 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); x.dis(alice, "send "+tv+" -m", "x\n") }()
	}
	wg.Wait()
	close(stop)
	<-done
	waitFor(t, 5*time.Second, "12 entries", func() bool { return strings.Count(f.out.String(), ";id=event-") == 12 })
	eq(t, "a follow sees 12 concurrent publications in number order, none missing", seqs(f.out.String()), "1 2 3 4 5 6 7 8 9 10 11 12")
	lmu.Lock()
	n := len(listings)
	eq(t, fmt.Sprintf("each of %d listings taken meanwhile shows 1..k: no number while a smaller one is missing", n), prefixes(listings), 0)
	lmu.Unlock()
	f.cancel()
}

// Publishing to one topic and reading it at once, and publishing one id twice at once.
func TestConcurrentPublications(t *testing.T) {
	x := twoRuns(t)
	tc := "topic+both@acme.ssh"
	x.mkTopic(tc, alice)
	cs := strings.TrimSpace(x.dis(bob, "sub "+tc).out)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var listings []string
	for i := 1; i <= 16; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); x.dis(alice, "send "+tc+" -m", "x\n") }()
		go func() {
			defer wg.Done()
			l := seqs(x.dis(bob, "recv "+cs+" --peek").out)
			mu.Lock()
			listings = append(listings, l)
			mu.Unlock()
		}()
	}
	wg.Wait()
	g := x.dis(bob, "recv "+cs+" --peek").out
	eq(t, "16 publications at once: numbers 1..16, each once", seqs(g), "1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16")
	eq(t, fmt.Sprintf("each of %d listings taken meanwhile shows 1..k", len(listings)), prefixes(listings), 0)
	// one msgid, published twice at once: one publication, one dup
	var outs [2]string
	same := ev(idB, 1)
	wg.Add(2)
	go func() { defer wg.Done(); outs[0] = x.dis(alice, "send "+tc+" -m --id "+same, "x\n").out }()
	go func() { defer wg.Done(); outs[1] = x.dis(alice, "send "+tc+" -m --id "+same, "x\n").out }()
	wg.Wait()
	o := outs[0] + outs[1]
	eq(t, "one msgid twice at once: one publication", strings.Count(o, "published -> "+tc+" msgid="+same+" subs=1"), 1)
	eq(t, "and one dup", strings.Count(o, "dup\n"), 1)
	eq(t, "the subscription has it once", strings.Count(x.dis(bob, "recv "+cs+" --peek").out, ";id="+same+";"), 1)
}

// bagFiles: the files of a bag as recv_bag --keep hands them over, sorted ("a.txt d/b.txt").
func (x *th) bagFiles(sub, owner, id string) string {
	rd := tar.NewReader(strings.NewReader(x.dis(owner, "recv_bag "+sub+" "+id+" --keep").out))
	var names []string
	for {
		hd, err := rd.Next()
		if err != nil {
			break
		}
		if hd.Typeflag == tar.TypeReg {
			names = append(names, strings.TrimPrefix(hd.Name, "./"))
		}
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

// firstSub: the subscription of topic a fan-out reaches first, and its owner (alice's is sa, else bob's).
func (x *th) firstSub(topic, sa string) (string, string) {
	first := globNames(filepath.Join(x.state, "topics", fsName(topic), "subs"))[0]
	if first == sa {
		return first, alice
	}
	return first, bob
}

// A subscription mailbox removed while a publication runs (an unsub or the reaper) takes nothing: it is not
// made again, delivered to, counted in subs=, or counted as failed. Removed before its turn, and removed
// as its delivery starts, before its link.
func TestPublicationIntoARemovedMailbox(t *testing.T) {
	x := twoRuns(t)
	os.WriteFile(x.h.journal, nil, 0o660)
	for k, when := range []string{"before its turn", "before its link"} {
		tx := fmt.Sprintf("topic+gone%d@acme.ssh", k)
		x.mkTopic(tx, alice)
		xa := strings.TrimSpace(x.dis(alice, "sub "+tx).out)
		xb := strings.TrimSpace(x.dis(bob, "sub "+tx).out)
		first, _ := x.firstSub(tx, xa)
		other := xb
		if first == xb {
			other = xa
		}
		victim, point := other, "linked" // removed once the first one is linked
		if when == "before its link" {
			victim, point = first, "deliver" // removed as its own delivery starts
		}
		box := filepath.Join(x.state, "inbox", fsName(victim))
		fired := false
		testHook = func(p string) {
			if p == point && !fired {
				fired = true
				os.RemoveAll(box)
			}
		}
		out := strings.TrimSpace(x.dis(alice, "send "+tx+" -m", "x\n").out)
		noHook()
		e1 := x.h.topicDone(tx).String()
		eq(t, when+": the rest are counted, the removed one not", out, "published -> "+tx+" msgid="+e1+" subs=1")
		_, err := os.Lstat(box)
		eq(t, when+": its mailbox stays removed", os.IsNotExist(err), true)
		j, _ := os.ReadFile(filepath.Join(x.state, "claims", ".journal"))
		has(t, when+": journaled with no failure", string(j), "publish "+tx+" "+e1+" from "+alice+" subs=1\n")
	}
}

// A renewal gives a subscription back the link and mailbox a race left it without.
func TestRenewalRestoresSubscription(t *testing.T) {
	x := twoRuns(t)
	tb := "topic+again@acme.ssh"
	x.mkTopic(tb, alice)
	sb := strings.TrimSpace(x.dis(bob, "sub "+tb).out)
	os.Remove(filepath.Join(x.state, "subs", fsName(sb)))
	os.RemoveAll(x.h.mbox(sb))
	eq(t, "a renewal: the same name", strings.TrimSpace(x.dis(bob, "sub "+tb).out), sb)
	has(t, "and its mailbox takes publications again", x.dis(alice, "send "+tb+" -m", "x\n").out, "subs=1")
	eq(t, "readable", seqs(x.dis(bob, "recv "+sb+" --peek").out), "1")
}
