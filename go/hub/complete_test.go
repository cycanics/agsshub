package hub

// docs/complete-bodies.md: a send's body counts only if it arrived whole and was stored.

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// test/tars (made once by GNU tar, Python, busybox, Go, git archive and by hand), by what a send_bag of each
// must answer.
var (
	tarsWhole = []string{"busybox", "git_archive", "go", "hand_ustar", "py_stream", "py_ustar",
		"sparse_gnu", "sparse_pax", "sparse_pax10", "ustar_long", "zeros_last", "cut_zeros_two", "marker_then_garbage"}
	tarsLongSegment = []string{"gnu", "pax", "py_gnu", "py_pax"} // whole, with a path segment longer than a file name: names are not judged
	tarsCut         = []string{"cut_after_last_nomarker", "cut_member_boundary", "cut_mid_header", "cut_mid_member", "cut_zeros_boundary",
		"cut_zeros_one", "go_noclose", "hand_nomarker", "hand_onezero", "hand_partzero", "zero_bytes", "text"}
	tarsBad = []string{"garbage", "member_then_garbage", "lone_then_member"}
)

func tarFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "tars", name+".tar"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The marker check leans on how tar.Reader reads: hold the stdlib to it.
func TestTarEndMarker(t *testing.T) {
	for want, names := range map[int][]string{0: append(append(tarsWhole, tarsLongSegment...), "empty_T", "empty_dot"), 4: tarsCut, 2: tarsBad} {
		for _, n := range names {
			code := 0
			var ie *inErr
			if _, _, err := copyBag(io.Discard, strings.NewReader(tarFile(t, n)), ""); errors.As(err, &ie) && ie.msg != "dispatch: empty bag" {
				code = ie.code
			}
			eq(t, n, code, want)
		}
	}
	// A member ending in zeros, cut right after it, looks like a marker to anything that reads last bytes.
	z := tarFile(t, "zeros_last")
	for _, cut := range []string{"cut_zeros_boundary", "cut_zeros_one"} {
		f := tarFile(t, cut)
		eq(t, cut+" is zeros_last cut", strings.HasPrefix(z, f), true)
		eq(t, cut+" ends in zeros", strings.Trim(f[len(f)-512:], "\x00"), "")
	}
}

const (
	cTopic = "topic+bodies@acme.ssh"
	cAlias = "backend-x"
)

func completeRig(t *testing.T) *th {
	x := twoRuns(t)
	os.WriteFile(filepath.Join(x.state, "budgets", "run"), []byte("subject=run\nrate=1000\nburst=100000\n"), 0o640)
	x.mkTopic(cTopic, alice)
	return x
}

func (x *th) box(id string) string { return filepath.Join(x.state, "inbox", fsName(id)) }

// step runs one send and holds it to its exit code.
func (x *th) step(t *testing.T, what, me, cmd, stdin string, want int) result {
	t.Helper()
	r := x.dis(me, cmd, stdin)
	if r.code != want {
		t.Errorf("%s: exit %d want %d (%q %q)", what, r.code, want, r.out, r.err)
	}
	return r
}

func TestSize(t *testing.T) {
	x := completeRig(t)
	step := func(what, cmd, stdin string, want int) result {
		t.Helper()
		return x.step(t, what, alice, cmd, stdin, want)
	}
	stored := func(what, id string, want bool) {
		t.Helper()
		eq(t, what+": stored", existsFollow(filepath.Join(x.box(bob), id+".msg")), want)
	}
	to := "send " + bob + " "

	// every form: exact, short, long
	eq(t, "exact -m", step("exact -m", to+"--id s1 --size 5 -m", "hello", 0).out, "sent -> "+bob+" msgid=s1\n")
	r := step("short -m", to+"--id s2 --size 6 -m", "hello", 4)
	eq(t, "what a short body says", r.err, "dispatch: body ended after 5 of 6 bytes\n")
	stored("short -m", "s2", false)
	eq(t, "the retry stores", step("retry", to+"--id s2 --size 5 -m", "hello", 0).out, "sent -> "+bob+" msgid=s2\n")
	r = step("long -m", to+"--id s3 --size 4 -m", "hello", 2)
	eq(t, "what a long body says", r.err, "dispatch: body longer than --size 4\n")
	stored("long -m", "s3", false)
	step("a NUL as the extra byte", to+"--id s4 --size 5 -m", "hello\x00", 2)
	stored("a NUL as the extra byte", "s4", false)
	step("counted before trailing newlines go", to+"--id s5 --size 7 -m", "hello\n\n", 0)
	step("only newlines", to+"--id s6 --size 2 -m", "\n\n", 2)

	rfc := "Subject: v\n\nbody\n"
	step("exact rfc5322", to+fmt.Sprintf("--id r1 --size %d -m:rfc5322", len(rfc)), rfc, 0)
	step("short rfc5322", to+fmt.Sprintf("--id r2 --size %d -m:rfc5322", len(rfc)+1), rfc, 4)
	r = step("cut inside the header block", to+"--id r3 --size 40 -m:rfc5322", "Subject: v\nRe:", 4)
	has(t, "(says it ended)", r.err, "ended after 14 of 40")
	stored("cut inside the header block", "r3", false)
	step("long rfc5322", to+fmt.Sprintf("--id r4 --size %d -m:rfc5322", len(rfc)-1), rfc, 2)
	step("whole, no blank line", to+"--id r5 --size 10 -m:rfc5322", "Subject: v", 2)
	step("no --size, cut in the header block: what came, as ever", to+"--id r6 -m:rfc5322", "Subject: v", 2)

	eq(t, "exact --file", step("exact --file", to+"--id f1 --size 3 --file a.bin", "a\x00b", 0).out, "sent -> "+bob+" msgid=f1 file=a.bin\n")
	step("its bytes", "id", "", 0)
	eq(t, "--file's bytes", x.dis(bob, "recv --id f1 --peek:file", "").out, "a\x00b")
	step("short --file", to+"--id f2 --size 4 --file a.bin", "abc", 4)
	stored("short --file", "f2", false)
	step("long --file", to+"--id f3 --size 2 --file a.bin", "abc", 2)
	stored("long --file", "f3", false)
	step("--size 0 --file", to+"--id f4 --size 0 --file e.bin", "", 0)
	step("--size 0 --file, a NUL", to+"--id f5 --size 0 --file e.bin", "\x00", 2)
	step("the retry of a short --file", to+"--id f2 --size 3 --file a.bin", "abc", 0)
	step("no --size: --file stores what came (until stage 3)", to+"--id f6 --file a.bin", "abc", 0)

	pub := "send " + cTopic + " "
	t1, t2, t4 := ev(idA, 1), ev(idA, 2), ev(idA, 4)
	eq(t, "exact on a topic", step("exact on a topic", pub+"--id "+t1+" --size 3 -m", "abc", 0).out, "published -> "+cTopic+" msgid="+t1+" subs=0\n")
	step("short on a topic", pub+"--id "+t2+" --size 4 -m", "abc", 4)
	step("long on a topic", pub+"--id "+t2+" --size 2 -m", "abc", 2)
	eq(t, "the id stays free", step("retry on a topic", pub+"--id "+t2+" --size 3 -m", "abc", 0).out, "published -> "+cTopic+" msgid="+t2+" subs=0\n")
	step("--file on a topic, short", pub+"--id "+t4+" --size 4 --file a.bin", "abc", 4)
	step("--file on a topic", pub+"--id "+t4+" --size 3 --file a.bin", "abc", 0)
	eq(t, "a repeated msgid on a topic: dup", step("repeat on a topic", pub+"--id "+t4+" --size 3 --file a.bin", "abc", 0).out, "dup\n")
	eq(t, "and on a mailbox", step("dup", to+"--id s1 --size 5 -m", "hello", 0).out, "dup s1 (dropped)\n")

	// refusals, all before reading
	for _, c := range []struct{ what, cmd, msg string }{
		{"with --chunked", to + "--size 3 --chunked -m", "--size: not with"},
		{"with --live", to + "--size 3 --live -m", "--size: not with"},
		{"with an inline message", to + "--size 3 -- abc", "--size: not with"},
		{"on send_bag", "send_bag " + bob + " --size 3", "unexpected arg '--size'"},
		{"malformed", to + "--size 3x -m", "bad --size"},
		{"negative", to + "--size -3 -m", "bad --size"},
		{"leading zeros", to + "--size 03 -m", "bad --size"},
		{"13 digits", to + "--size 1000000000000 -m", "bad --size"},
		{"empty stand-in", to + "--size -m", "bad --size"},
		{"over the cap", to + fmt.Sprintf("--size %d -m", FileMaxBytes+1), "body exceeds cap"},
		{"over the cap, --file", to + fmt.Sprintf("--size %d --file a", FileMaxBytes+1), "body exceeds cap"},
		{"0 with -m", to + "--size 0 -m", "nothing to send"},
		{"0 with -m:rfc5322", to + "--size 0 -m:rfc5322", "nothing to send"},
		{"last argument", to + "-m --size", "--size needs a value"},
		{"no form", to + "--size 3", "--size needs -m"},
	} {
		r := step(c.what, c.cmd, "abc", 2)
		has(t, c.what+" (message)", r.err, c.msg)
	}
	step("at the cap, a short body", to+fmt.Sprintf("--id capped --size %d --file a", FileMaxBytes), "abc", 4)
}

func TestBagEndMarker(t *testing.T) {
	x := completeRig(t)
	step := func(what, cmd, stdin string, want int) result {
		t.Helper()
		return x.step(t, what, alice, cmd, stdin, want)
	}
	for _, n := range tarsWhole {
		r := step("whole: "+n, "send_bag "+bob+" --id "+n, tarFile(t, n), 0)
		eq(t, n, r.out, "sent bag -> "+bob+" msgid="+n+"\n")
	}
	for _, n := range tarsLongSegment {
		step("long segment, not judged: "+n, "send_bag "+bob+" --id "+n, tarFile(t, n), 0)
	}
	for _, n := range append(tarsWhole, tarsLongSegment...) {
		eq(t, "stored exactly as received: "+n, x.dis(bob, "recv_bag --id "+n+" --peek").out == tarFile(t, n), true)
	}
	for _, format := range []string{"gnu", "pax", "ustar"} { // long paths the bag does take
		dir := t.TempDir()
		long := filepath.Join(strings.Repeat("a", 60), strings.Repeat("b", 60))
		os.MkdirAll(filepath.Join(dir, long), 0o755)
		os.WriteFile(filepath.Join(dir, long, "c.txt"), []byte("c\n"), 0o644)
		tb, err := exec.Command("tar", "--format="+format, "-C", dir, "-cf", "-", ".").Output()
		if err != nil {
			t.Fatal(err)
		}
		id := "long-" + format
		eq(t, id, step(id, "send_bag "+bob+" --id "+id, string(tb), 0).out, "sent bag -> "+bob+" msgid="+id+"\n")
		eq(t, id, x.dis(bob, "recv --id "+id+" --peek:bag c.txt", "").code, 3) // the member's whole path names it
		eq(t, id+": fetched by its path", x.dis(bob, "recv --id "+id+" --peek:bag ./"+long+"/c.txt", "").out, "c\n")
	}
	for _, n := range tarsCut {
		id := "c-" + n
		r := step("cut: "+n, "send_bag "+bob+" --id "+id, tarFile(t, n), 4)
		eq(t, n+" says", r.err, "dispatch: tar ended before its end marker\n")
		eq(t, n+": nothing stored", exists(filepath.Join(x.box(bob), id+".msg")), false)
		eq(t, n+": the id is free, the retry stores", step("retry: "+n, "send_bag "+bob+" --id "+id, tarFile(t, "go"), 0).out, "sent bag -> "+bob+" msgid="+id+"\n")
	}
	for _, n := range tarsBad {
		r := step("bad: "+n, "send_bag "+bob+" --id b-"+n, tarFile(t, n), 2)
		eq(t, n+" says", r.err, "dispatch: bad tar\n")
	}
	has(t, "an empty tree is still an empty bag", step("empty_T", "send_bag "+bob+" --id e1", tarFile(t, "empty_T"), 2).err, "empty bag")
	has(t, "a tree of only ./ too", step("empty_dot", "send_bag "+bob+" --id e2", tarFile(t, "empty_dot"), 2).err, "empty bag")
	// the sender's paths are its own: nothing is extracted, so nothing in them can fail
	file := craftTar(member{name: "a", body: "x", typ: 0x30}, member{name: "a/b", body: "y", typ: 0x30})
	step("a file where a folder would go", "send_bag "+bob+" --id p1", file, 0)
	eq(t, "a member ending in zeros comes through", x.dis(bob, "recv_bag --id zeros_last --peek", "").code, 0)
	// topics: a cut takes no record, keeps the id free
	step("cut on a topic", "send_bag "+cTopic+" --id "+ev(idA, 1), tarFile(t, "cut_member_boundary"), 4)
	eq(t, "nothing stored, nothing left", x.leftOf(cTopic), "policy subs|")
	eq(t, "the retry publishes", step("retry on a topic", "send_bag "+cTopic+" --id "+ev(idA, 1), tarFile(t, "go"), 0).out, "published -> "+cTopic+" msgid="+ev(idA, 1)+" subs=0\n")
	// framed: the frame's end does not stand in for the tar's
	cut := tarFile(t, "cut_member_boundary")
	step("framed, no marker", "send_bag "+bob+" --chunked --id fr1", string(frame([]byte(cut), 4096, "")), 4)
	step("framed, whole", "send_bag "+bob+" --chunked --id fr1", string(frame([]byte(tarFile(t, "go")), 4096, "")), 0)
}

// The mistakes a first build made: each a whole tar, or a store that must say what happened.
func TestBagEdges(t *testing.T) {
	x := completeRig(t)
	step := func(what, cmd, stdin string, want int) result {
		t.Helper()
		return x.step(t, what, alice, cmd, stdin, want)
	}
	// data after the marker, more than a pipe holds: tar stops reading, its feeder dies of SIGPIPE
	step("a megabyte after the marker", "send_bag "+bob+" --id m1", tarFile(t, "go")+strings.Repeat("\x00", 1<<20), 0)
	step("garbage after the marker", "send_bag "+bob+" --id m2", tarFile(t, "go")+strings.Repeat("x", 1<<20), 0)
	// what tar echoes of the sender's text does not count as what tar says
	for i, kw := range []string{"Unexpected EOF", "Cannot x", "A lone zero block at 1"} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(&tar.Header{Name: "a", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, Format: tar.FormatPAX, PAXRecords: map[string]string{kw: "v"}})
		tw.Write([]byte("a"))
		tw.Close()
		step("a pax keyword: "+kw, "send_bag "+bob+fmt.Sprintf(" --id k%d", i), buf.String(), 0)
	}
	// modes are not taken from the archive
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0})
	tw.WriteHeader(&tar.Header{Name: "d/f", Typeflag: tar.TypeReg, Mode: 0, Size: 1})
	tw.Write([]byte("f"))
	tw.Close()
	step("mode 0 members", "send_bag "+bob+" --id z1 --msg d/f", buf.String(), 0)
	eq(t, "data after the marker stored as received", x.dis(bob, "recv_bag --id m2 --peek").out == tarFile(t, "go")+strings.Repeat("x", 1<<20), true)
	// a publisher the policy refuses is refused before its body is judged
	r := x.step(t, "refused, cut", bob, "send_bag "+cTopic, tarFile(t, "cut_mid_member"), 3)
	has(t, "says so", r.err, "may not publish")
	x.step(t, "refused, --file", bob, "send "+cTopic+" --file f", "x", 3)
}

// A subscription mailbox that cannot take a delivery is skipped: its number is used, so its reader sees a
// gap, in a listing and in a follow; the others get the message; the publisher gets exit 0, subs= counting the
// deliveries; nothing stays behind (docs/topics.md, Publishing).
func TestSkippedMailboxIsAGap(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode")
	}
	x := completeRig(t)
	sa := strings.TrimSpace(x.dis(alice, "sub "+cTopic).out)
	sb := strings.TrimSpace(x.dis(bob, "sub "+cTopic).out)
	jf := filepath.Join(x.state, "claims", ".journal")
	os.WriteFile(jf, nil, 0o640)
	refused := func(what string, refuse bool) {
		m := os.FileMode(0o770)
		if refuse {
			m = 0o500
		}
		if err := os.Chmod(x.box(sb), m); err != nil {
			t.Fatal(what, err)
		}
	}
	t.Cleanup(func() { refused("cleanup", false) })
	f := x.start(x.adm(bob), "recv "+sb+" --follow --peek", "")
	waitFor(t, 2*time.Second, "the follow", x.followed(sb))
	entries := func(n int) string {
		waitFor(t, 3*time.Second, fmt.Sprint(n, " entries"), func() bool { return strings.Count(f.out.String(), ";id=event-") >= n })
		return seqs(f.out.String())
	}

	refused("tf1", true)
	r := x.dis(alice, "send "+cTopic+" --id "+ev(idA, 1)+" -m", "body")
	eq(t, "exit 0, counting the deliveries", fmt.Sprint(r.code, "|", r.out, "|", r.err), "0|published -> "+cTopic+" msgid="+ev(idA, 1)+" subs=1\n|")
	eq(t, "the one that can has it", x.view(sa, alice), "1")
	eq(t, "the skipped one has nothing", x.view(sb, bob), "")
	eq(t, "nothing left", x.leftOf(cTopic), "policy subs|")
	has(t, "the journal counts the failed link", readString(jf), " publish "+cTopic+" "+ev(idA, 1)+" from "+alice+" subs=1 failed=1\n")
	refused("tf1", false)
	eq(t, "the next reaches both", x.dis(alice, "send "+cTopic+" -m", "body").out, "published -> "+cTopic+" msgid="+ev(idA, 2)+" subs=2\n")
	eq(t, "a listing of the skipped one: number 1 is missing", x.view(sb, bob), "2")
	eq(t, "its follow: the same", entries(1), "2")
	eq(t, "the other: contiguous", x.view(sa, alice), "1 2")

	// a bag, the same; its tree goes too
	refused("tb1", true)
	r = x.dis(alice, "send_bag "+cTopic, tarFile(t, "go"))
	eq(t, "a bag: exit 0", fmt.Sprint(r.code, "|", r.out), "0|published -> "+cTopic+" msgid="+ev(idA, 3)+" subs=1\n")
	eq(t, "nothing left in the skipped one", exists(filepath.Join(x.box(sb), ev(idA, 3))), false)
	refused("tb1", false)
	code(t, "tf3", x.dis(alice, "send "+cTopic+" -m", "body"), 0)
	eq(t, "the skipped bag's number is missing", x.view(sb, bob), "2 4")
	eq(t, "on the follow too", entries(2), "2 4")
	eq(t, "nothing left after a bag", x.leftOf(cTopic), "policy subs|")
	f.cancel()
}

// A publication's work does not grow with earlier failures: after failed links, the next does the same steps
// as one before them.
func TestPublishWorkIgnoresEarlierFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode")
	}
	x := completeRig(t)
	x.dis(alice, "sub "+cTopic)
	sb := strings.TrimSpace(x.dis(bob, "sub "+cTopic).out)
	steps := func(id string) string {
		var got []string
		testHook = func(p string) { got = append(got, p) }
		defer noHook()
		code(t, id, x.dis(alice, "send "+cTopic+" -m", "x"), 0)
		return strings.Join(got, " ")
	}
	before := steps("w0")
	eq(t, "one publication to two subscriptions", before, "stage stored locked recorded deliver linked deliver linked fanned")
	os.Chmod(x.box(sb), 0o500)
	for i := 1; i <= 20; i++ {
		code(t, "failing", x.dis(alice, "send "+cTopic+" -m", "x"), 0)
	}
	os.Chmod(x.box(sb), 0o770)
	eq(t, "after 20 failed links, the same steps", steps("w1"), before)
	eq(t, "and nothing kept for them", x.leftOf(cTopic), "policy subs|")
}

// A publication whose message cannot be stored answers exit 1, and nothing is delivered or left.
func TestStoreFailureIsNothingPublished(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode")
	}
	x := completeRig(t)
	sb := strings.TrimSpace(x.dis(bob, "sub "+cTopic).out)
	code(t, "s0", x.dis(alice, "send "+cTopic+" --id "+ev(idA, 1)+" -- x"), 0)
	inbox := filepath.Join(x.state, "inbox")
	fi, _ := os.Stat(inbox)
	os.Chmod(inbox, 0o500)
	r := x.dis(alice, "send "+cTopic+" --id "+ev(idA, 2)+" -- x")
	os.Chmod(inbox, fi.Mode())
	eq(t, "cannot store: exit 1", fmt.Sprint(r.code, "|", r.out, "|", r.err), "1||"+errStore+"\n")
	eq(t, "nothing left", x.leftOf(cTopic), "policy subs|")
	eq(t, "the retry publishes, no dup", x.dis(alice, "send "+cTopic+" --id "+ev(idA, 2)+" -- x").out, "published -> "+cTopic+" msgid="+ev(idA, 2)+" subs=1\n")
	eq(t, "the subscription has them, contiguous", x.view(sb, bob), "1 2")
}

func TestTarErrClasses(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{io.ErrUnexpectedEOF, 4},
		{tar.ErrHeader, 2},
		{tar.ErrFieldTooLong, 2},
		{errors.New("archive/tar: sparse file contains unreferenced data"), 2},
		{&fs.PathError{Op: "read", Path: "x", Err: syscall.EIO}, 1},
	} {
		got, _ := tarErr(c.err)
		eq(t, c.err.Error(), got, c.want)
	}
}

// A tar with a large member, cut anywhere: tar seeks over a member when it can, and says something else when
// the cut falls in its first record.
func TestTarCutAnywhere(t *testing.T) {
	x := completeRig(t)
	big := string(makeTar(t, map[string]string{"a.txt": "a\n", "d/b.bin": strings.Repeat("x", 40000)}))
	var whole int // where the end marker's second block ends: from there on, a cut is no cut
	for i := 0; i+1024 <= len(big); i += 512 {
		if strings.Trim(big[i:i+1024], "\x00") == "" {
			whole = i + 1024
			break
		}
	}
	for n := 0; n < len(big); n += 317 {
		want := 4
		if n >= whole {
			want = 0
		}
		x.step(t, fmt.Sprintf("cut at %d of %d", n, len(big)), alice, fmt.Sprintf("send_bag %s --id cut%d", bob, n), big[:n], want)
	}
}

// A cut send spends no token; a failed one neither reaches a wake.
func TestCutSendSpendsNoTokenAndAsksNothing(t *testing.T) {
	x := instRig(t)
	x.settings("wake_secs=30\n")
	hub := "commhub@home.ssh"
	i := "group.sess+c0ffee01@expa.ssh"
	flood := filepath.Join(x.state, "flood")
	floodFiles := func() int { e, _ := os.ReadDir(flood); return len(e) }
	before := floodFiles()
	code(t, "a cut bag", x.dis(hub, "send_bag "+i+" --id c1", tarFile(t, "cut_member_boundary")), 4)
	code(t, "a tar cut inside a member", x.dis(hub, "send_bag "+i+" --id c2", tarFile(t, "cut_mid_member")), 4)
	code(t, "garbage", x.dis(hub, "send_bag "+i+" --id c3", tarFile(t, "garbage")), 2)
	code(t, "--file --size, short", x.dis(hub, "send "+i+" --id c4 --size 9 --file f", "abc"), 4)
	code(t, "-m --size, short", x.dis(hub, "send "+i+" --id c5 --size 9 -m", "abc"), 4)
	code(t, "--chunked, cut", x.dis(hub, "send "+i+" --id c6 --chunked --file f", "5\r\nhel"), 4)
	eq(t, "no ask", len(x.h.asks.m), 0)
	eq(t, "no bucket touched", floodFiles(), before)
	b := x.heldSend(t, hub, "send_bag "+i+" --id c1", tarFile(t, "go"), "c0ffee01")
	eq(t, "a whole one asks once", len(x.h.asks.m), 1)
	x.dis(par, "admit c0ffee01")
	eq(t, "and is stored once admitted", b.wait(t, 3*time.Second), 0)
}

// --size holds on an alias too: the plugin gets stdin, or never runs.
func TestSizeOnAnAlias(t *testing.T) {
	x := completeRig(t)
	lib := filepath.Join(x.dir, "lib")
	os.MkdirAll(filepath.Join(lib, "backends", "x"), 0o755)
	plugin := "#!/bin/sh\ncat > \"$HUB_ADDR\"\necho sent-by-plugin\n"
	os.WriteFile(filepath.Join(lib, "backends", "x", "send"), []byte(plugin), 0o755)
	x.h.cfg.Lib = lib
	out := filepath.Join(x.dir, "plugin.out")
	os.WriteFile(filepath.Join(x.state, "aliases", cAlias), []byte("type=x\naddr="+out+"\n"), 0o640)
	r := x.dis(alice, "send "+cAlias+" --size 3 --file f", "abc")
	code(t, "exact", r, 0)
	eq(t, "the plugin's answer", r.out, "sent-by-plugin\n")
	eq(t, "the plugin's stdin", readString(out), "abc")
	os.Remove(out)
	code(t, "short: it never runs", x.dis(alice, "send "+cAlias+" --size 4 --file f", "abc"), 4)
	code(t, "long: it never runs", x.dis(alice, "send "+cAlias+" --size 2 --file f", "abc"), 2)
	eq(t, "(it did not)", exists(out), false)
	code(t, "no --size", x.dis(alice, "send "+cAlias+" --file f", "abc"), 0)
	eq(t, "the plugin's stdin, whole", readString(out), "abc")
}

func TestFileCapHoldsForANul(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 64 MB")
	}
	x := completeRig(t)
	big := string(make([]byte, FileMaxBytes+1)) // zeros: the extra byte a NUL
	r := x.dis(alice, "send "+bob+" --id big --file f", big)
	code(t, "over the cap by one NUL", r, 2)
	eq(t, "says", r.err, "dispatch: file exceeds cap\n")
	eq(t, "nothing stored", exists(filepath.Join(x.box(bob), "big.msg")), false)
}

// A failed store is no duplicate: an unwritable mailbox, which needs no root and no small disk.
func TestStoreFailureIsNoDup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode")
	}
	x := completeRig(t)
	x.dis(alice, "send "+bob+" --id seed -- x", "")
	os.Chmod(x.box(bob), 0o500)
	defer os.Chmod(x.box(bob), 0o770)
	for _, c := range []struct{ what, cmd, stdin string }{
		{"-m", "send " + bob + " --id n1 -m", "x"},
		{"-m:rfc5322", "send " + bob + " --id n2 -m:rfc5322", "Subject: v\n\nx"},
		{"--file", "send " + bob + " --id n3 --file f", "x"},
		{"send_bag", "send_bag " + bob + " --id n4", tarFile(t, "go")},
	} {
		r := x.step(t, c.what, alice, c.cmd, c.stdin, 1)
		eq(t, c.what+" says", r.err, errStore+"\n")
		eq(t, c.what+" says nothing on stdout", r.out, "")
	}
	os.Chmod(x.box(bob), 0o770)
	x.step(t, "the retry", alice, "send "+bob+" --id n1 -m", "x", 0)
}

// --- a real full disk: test/full-disk.sh runs this on a small tmpfs (HUB_TEST_SMALLFS) --------------------

// fillInodes takes every free inode on the small disk, then gives k back.
func fillInodes(t *testing.T, dir string, k int) {
	t.Helper()
	os.MkdirAll(dir, 0o700)
	n := 0
	var made []string
	for {
		p := filepath.Join(dir, fmt.Sprintf("i%d", n))
		f, err := os.Create(p)
		if err != nil {
			break
		}
		f.Close()
		made = append(made, p)
		n++
	}
	for i := 0; i < k && i < len(made); i++ {
		os.Remove(made[len(made)-1-i])
	}
}

// fillBlocks takes every free block on the small disk, then gives room bytes back.
func fillBlocks(t *testing.T, dir string, room int64) {
	t.Helper()
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "blocks")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 4096)
	var n int64
	for {
		m, err := f.Write(buf)
		n += int64(m)
		if err != nil {
			break
		}
	}
	if room > 0 {
		f.Truncate(n - room)
	}
}

const fullBlob = "0123456789abcdef" // 256 KB of it: what does not fit where a form's file does not

type fullForm struct{ what, cmd, stdin, box, file string }

func fullForms(t *testing.T, sub string) []fullForm {
	blob := strings.Repeat(fullBlob, 16*1024)
	rfc := "Subject: v\n\nbody\n"
	bag := string(makeTar(t, map[string]string{"a.txt": blob, "dir/b.txt": "b\n"}))
	return []fullForm{
		{"-m", "send " + bob + " --id ID -m", "body", bob, ""},
		{"-m --size", "send " + bob + " --id ID --size 4 -m", "body", bob, ""},
		{"-m:rfc5322", "send " + bob + " --id ID -m:rfc5322", rfc, bob, ""},
		{"-m:rfc5322 --size", "send " + bob + fmt.Sprintf(" --id ID --size %d -m:rfc5322", len(rfc)), rfc, bob, ""},
		{"-- inline", "send " + bob + " --id ID -- body", "", bob, ""},
		{"--file", "send " + bob + " --id ID --file f", blob, bob, "f"},
		{"--file --size", "send " + bob + fmt.Sprintf(" --id ID --size %d --file f", len(blob)), blob, bob, "f"},
		{"--chunked --file", "send " + bob + " --id ID --chunked --file f", string(frame([]byte(blob), 65536, "")), bob, "f"},
		{"send_bag", "send_bag " + bob + " --id ID", bag, bob, "a.txt"},
		{"send_bag --chunked", "send_bag " + bob + " --id ID --chunked", string(frame([]byte(bag), 65536, "")), bob, "a.txt"},
		{"topic -m", "send " + cTopic + " --id ID -m", "body", sub, ""},
		{"topic --file", "send " + cTopic + " --id ID --file f", blob, sub, "f"},
		{"topic send_bag", "send_bag " + cTopic + " --id ID", bag, sub, "a.txt"},
		{"instance -m", "send " + fullInst + " --id ID -m", "body", fullInst, ""},
		{"instance --file", "send " + fullInst + " --id ID --file f", blob, fullInst, "f"},
		{"instance send_bag", "send_bag " + fullInst + " --id ID", bag, fullInst, "a.txt"},
	}
}

// payloadFile: a stored message's file payload, or the bag member name in its bag payload.
func payloadFile(msg, name string) string {
	m, err := openMsg(msg)
	if err != nil {
		return ""
	}
	defer m.Close()
	kind, _, r, n := m.payload()
	if kind == "bag" {
		mr, _ := bagMember(r, n, name)
		if mr == nil {
			return ""
		}
		b, _ := io.ReadAll(mr)
		return string(b)
	}
	b, _ := io.ReadAll(r)
	return string(b)
}

// pubMsgid: the msgid a publish reports.
func pubMsgid(out string) string { v, _ := after(out, "msgid="); return before(v, " ") }

// fullInst: an instance with a claim, so a send to it is stored (its mailbox made at the first).
const fullInst = "group.sess+f1@expa.ssh"

// Each form meets the wall at each step of a store: exit 1, nothing stored, the retry stores; or exit 0 and
// all of it is there. Never dup.
func TestFullDisk(t *testing.T) {
	small := os.Getenv("HUB_TEST_SMALLFS")
	if small == "" {
		t.Skip("needs a small tmpfs: test/full-disk.sh")
	}
	old := os.Getenv("TMPDIR")
	os.Setenv("TMPDIR", small) // the hub's state; the helpers' temp files stay where they were
	x := completeRig(t)
	os.Setenv("TMPDIR", old)
	sub := strings.TrimSpace(x.dis(bob, "sub "+cTopic, "").out)
	// the address's operator claim as hub-mint writes it (the container has no ssh-keygen for lib/mint)
	os.WriteFile(x.h.claimPath(par, OpMinter), []byte(fmt.Sprintf("name=group.sess\norg=expa\nws=operator\nfq=%s\ntype=ssh-ed25519\nkeyid=k\nserial=1\nexpires=%d\nmode=persistent\ninstances=1\n", par, time.Now().Unix()+86400)), 0o640)
	if r := x.dis(par, "register f1", x.key("f1").line); r.code != 0 {
		t.Fatalf("register: %+v", r)
	}
	if r := x.dis(par, "admit f1"); r.code != 0 { // the instance's mailbox, before the disk fills
		t.Fatalf("admit: %+v", r)
	}
	filler := filepath.Join(x.dir, "filler")
	forms := fullForms(t, sub)
	blob := strings.Repeat(fullBlob, 16*1024)
	seq, ones, skipped := 0, 0, 0
	onesBy := map[string]int{}
	stemOf := func(box, id string) string { return id }
	wall := func(label string, fill func()) {
		t.Helper()
		for _, form := range forms {
			seq++
			id := fmt.Sprintf("id%d", seq)
			if form.box == sub { // a publication's msgid is its event's: the hub's next
				id = ""
			}
			cmd := strings.Replace(form.cmd, "--id ID", "--id "+id, 1)
			if id == "" {
				cmd = strings.Replace(form.cmd, " --id ID", "", 1)
			}
			what := label + " " + form.what + " " + id
			fill()
			before := len(msgFiles(x.box(form.box)))
			r := x.dis(alice, cmd, form.stdin)
			if form.box == sub {
				id = pubMsgid(r.out)
			}
			msg := filepath.Join(x.box(form.box), stemOf(form.box, id)+".msg")
			lacks(t, what+": never dup", r.out, "dup ")
			undelivered := false
			switch r.code {
			case 0:
				if !existsFollow(msg) { // a topic's subscription that could not take it: skipped
					undelivered = true
					skipped++
					eq(t, what+": only a topic skips a mailbox", form.box, sub)
					has(t, what+": published to none", r.out, " subs=0\n")
				}
			case 1:
				ones++
				onesBy[form.what]++
				eq(t, what+": nothing on stdout", r.out, "")
				has(t, what+": says retry", r.err, "retry")
				eq(t, what+": nothing stored", len(msgFiles(x.box(form.box))), before)
			default:
				t.Errorf("%s: exit %d (%q %q)", what, r.code, r.out, r.err)
			}
			os.RemoveAll(filler)
			switch {
			case r.code == 1:
				rr := x.dis(alice, cmd, form.stdin)
				code(t, what+": the retry", rr, 0)
				if form.box == sub {
					id = pubMsgid(rr.out)
				}
				msg = filepath.Join(x.box(form.box), stemOf(form.box, id)+".msg")
				eq(t, what+": stored after the retry", existsFollow(msg), true)
			case undelivered:
				rr := x.dis(alice, cmd, form.stdin)
				eq(t, what+": the retry is a fresh publication", fmt.Sprint(rr.code, "|", strings.HasSuffix(rr.out, " subs=1\n")), "0|true")
				id = pubMsgid(rr.out)
				msg = filepath.Join(x.box(form.box), stemOf(form.box, id)+".msg")
				eq(t, what+": and delivers it", existsFollow(msg), true)
			}
			if form.file != "" {
				eq(t, what+": the payload whole", payloadFile(msg, form.file) == blob, true)
			}
			for _, n := range globNamesAll(x.h.topicDir(cTopic)) {
				if strings.HasPrefix(n, ".record.") {
					t.Errorf("%s: a record temp file left behind", what)
				}
			}
			ents, _ := os.ReadDir(x.box(form.box))
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), ".stage") {
					t.Errorf("%s: a stage left behind", what)
				}
			}
			if n := len(x.stages()); n > 0 {
				t.Errorf("%s: %d publication stages left behind", what, n)
			}
			os.Remove(msg)
		}
	}
	for k := 0; k <= 10; k++ { // free inodes: from none up to enough
		wall(fmt.Sprintf("inodes=%d", k), func() { fillInodes(t, filler, k) })
	}
	for _, room := range []int64{0, 4096, 64 * 1024, 200 * 1024, 300 * 1024} { // free bytes: part of a file
		wall(fmt.Sprintf("room=%d", room), func() { fillBlocks(t, filler, room) })
	}
	if ones < len(forms) {
		t.Errorf("the wall was met %d times: the disk was not full", ones)
	}
	for _, f := range forms { // a hard link takes no inode and seldom a block: a subscription is rarely skipped now
		if onesBy[f.what] == 0 {
			t.Errorf("%s never met the wall: never exit 1", f.what)
		}
	}
	t.Logf("%d sends, %d exit 1, %d skipped a mailbox (its seq used: a gap shows)", seq, ones, skipped)
}
