// hub-bench drives a real agsshub-hubd over SSH on this machine and reports latency and throughput per
// operation (docs/wake-live-event-id.md, Tests). test/bench.sh builds the daemon and runs it.
//
//	hub-bench -hubd <agsshub-hubd> -lib <repo lib/> [-old] [-quick]
//
// -old drives the previous hub's command forms (no --durable, chat or wake notice; any --id on a topic;
// recv -m:rfc5322 <id> --keep and --follow --keep, where this hub takes --id <id> --peek:rfc5322 and
// --follow --peek). Every operation is one SSH session on a connection per identity, as a client's control master
// gives it; latency runs from the session's open to its exit status.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

var (
	hubd  = flag.String("hubd", "", "the agsshub-hubd binary")
	lib   = flag.String("lib", "", "the repo's lib/ (lib/mint, help.json)")
	old   = flag.Bool("old", false, "the previous hub's forms")
	quick = flag.Bool("quick", false, "fewer iterations")
	only  = flag.String("only", "", "run only scenarios whose name contains this")
)

const (
	sender = "commhub@home.ssh"
	rcpt   = "group.demo@expa.ssh"
	addr   = "group.sess@expa.ssh"
	topicT = "topic+bench%d@expa.ssh"
)

type rig struct {
	dir, root, port string
	proc            *exec.Cmd
	keys            map[string]ssh.Signer
	conns           map[string]*ssh.Client
	hostKey         string
}

func main() {
	flag.Parse()
	if *hubd == "" || *lib == "" {
		fmt.Fprintln(os.Stderr, "hub-bench -hubd <binary> -lib <lib dir>")
		os.Exit(2)
	}
	r := &rig{keys: map[string]ssh.Signer{}, conns: map[string]*ssh.Client{}}
	var err error
	if r.dir, err = os.MkdirTemp("", "hub-bench."); err != nil {
		fail(err)
	}
	defer os.RemoveAll(r.dir)
	r.setup()
	defer r.stop()
	la0 := loadavg()
	var rows []row
	run := func(name string, f func() row) {
		if *only != "" && !strings.Contains(name, *only) {
			return
		}
		rw := f()
		rw.name = name
		rows = append(rows, rw)
		fmt.Fprintf(os.Stderr, "%-44s %s\n", name, rw.cells())
	}
	n := func(full, q int) int {
		if *quick {
			return q
		}
		return full
	}
	kib := bytes.Repeat([]byte("k"), 1024)
	run("send -m 1 KiB", func() row { return r.sendN(n(300, 30), "send "+rcpt+" --id %s -m", kib) })
	if !*old {
		run("send -m 1 KiB --durable", func() row { return r.sendN(n(300, 30), "send "+rcpt+" --id %s --durable -m", kib) })
	}
	mib := bytes.Repeat([]byte("f"), 1<<20)
	run("send --file 1 MiB", func() row { return r.sendN(n(50, 5), "send "+rcpt+" --id %s --file f.bin", mib) })
	m64 := bytes.Repeat([]byte("F"), 64<<20)
	run("send --file 64 MiB", func() row { return r.sendN(n(7, 2), "send "+rcpt+" --id %s --file f.bin", m64) })
	m64 = nil
	b64 := makeBag(64<<20 - 8192)
	run("send_bag 64 MiB", func() row { return r.sendN(n(7, 2), "send_bag "+rcpt+" --id %s --msg m.md", b64) })
	b64 = nil
	b252 := makeBag(252<<20 - 8192)
	run("send_bag 252 MiB", func() row { return r.sendN(n(5, 2), "send_bag "+rcpt+" --id %s --msg m.md", b252) })
	b252 = nil
	for i, subs := range []int{1, 10, 100} {
		tp := r.topic(i, subs)
		form := "send " + tp + " -m"
		if *old {
			form = "send " + tp + " --id %s -m"
		}
		run(fmt.Sprintf("publish -m 1 KiB, %d subs", subs), func() row { return r.sendN(n(300, 30), form, kib) })
	}
	run("recv --id --peek:rfc5322 (1 KiB)", func() row { return r.readN(n(300, 30), "raw") })
	run("recv --peek (100 x 1 KiB)", func() row { return r.readN(n(100, 10), "list") })
	if !*old {
		run("recv, consuming (100 x 1 KiB)", func() row { return r.readN(n(100, 10), "consume") })
		run("recv_ack --before-including, 500 backlog, 1 call", func() row { return r.ackN(n(20, 3), 500, true, 1) })
		run("recv_ack -m:ids, 500 backlog, 2 calls", func() row { return r.ackN(n(20, 3), 500, false, 2) })
		run("recv_ack -m:ids, 500 backlog, 1 call", func() row { return r.ackN(n(20, 3), 500, false, 1) })
	}
	run("send --live --chunked round trip", func() row { return r.liveN(n(300, 30), false) })
	if !*old {
		run("chat round trip", func() row { return r.liveN(n(300, 30), true) })
		run("wake render, 1000 inst x 10 unread", func() row { return r.wake(n(20, 5)) })
	}
	la1 := loadavg()
	hub := "new"
	if *old {
		hub = "old"
	}
	fmt.Printf("hub=%s loadavg before: %s after: %s cpus=%d\n", hub, la0, la1, cpus())
	fmt.Printf("| %s | n | median ms | p95 ms | throughput |\n|---|---|---|---|---|\n", "operation ("+hub+")")
	for _, rw := range rows {
		fmt.Printf("| %s | %d | %s |\n", rw.name, len(rw.ds), strings.ReplaceAll(rw.cells(), "  ", " | "))
	}
}

// --- setup ---------------------------------------------------------------------------------------------

func (r *rig) setup() {
	r.root = filepath.Join(r.dir, "state")
	for _, d := range []string{"claims/operator", "claims/instance", "claims/.ibyfpr", "claims/.byfpr", "claims/.tmp", "claims/.tomb",
		"claims/.serial", "orgs", "budgets", "aliases", "topics", "subs", "inbox", "flood", "described"} {
		must(os.MkdirAll(filepath.Join(r.root, d), 0o2770))
	}
	must(os.WriteFile(filepath.Join(r.root, "claims", ".journal"), nil, 0o660))
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(priv, "")
	must(err)
	r.hostKey = filepath.Join(r.dir, "host_key")
	must(os.WriteFile(r.hostKey, pem.EncodeToMemory(blk), 0o600))
	r.mint(sender)
	r.mint(rcpt)
	if !*old {
		r.mint(addr, "--instances")
	}
	for _, b := range []string{"commhub", "group"} {
		must(os.WriteFile(filepath.Join(r.root, "budgets", b), []byte("subject="+b+"\nrate=100000\nburst=1000000\n"), 0o640))
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	r.port = fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	r.start()
}

func (r *rig) start() {
	r.proc = exec.Command(*hubd, "-listen", "127.0.0.1:"+r.port, "-hostkey", r.hostKey, "-root", r.root, "-lib", *lib,
		"-registrars", filepath.Join(r.dir, "registrars"), "-reap-interval", "0", "-max-sessions", "64")
	r.proc.Stdout, r.proc.Stderr = io.Discard, io.Discard
	must(r.proc.Start())
	for i := 0; ; i++ {
		c, err := net.Dial("tcp", "127.0.0.1:"+r.port)
		if err == nil {
			c.Close()
			break
		}
		if i > 200 {
			fail(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.conns = map[string]*ssh.Client{}
}

func (r *rig) stop() {
	for _, c := range r.conns {
		c.Close()
	}
	if r.proc != nil && r.proc.Process != nil {
		r.proc.Process.Signal(os.Interrupt)
		r.proc.Wait()
	}
}

// mint binds a new key to id through the operator's lib/mint.
func (r *rig) mint(id string, flags ...string) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sp, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	r.keys[id] = signer
	cmd := exec.Command(filepath.Join(*lib, "mint"), append([]string{"add", id}, flags...)...)
	cmd.Env = append(os.Environ(), "HUB_ROOT="+r.root)
	cmd.Stdin = bytes.NewReader(ssh.MarshalAuthorizedKey(sp))
	if out, err := cmd.CombinedOutput(); err != nil {
		fail(fmt.Errorf("mint %s: %v %s", id, err, out))
	}
}

func (r *rig) conn(id string) *ssh.Client {
	connMu.Lock()
	defer connMu.Unlock()
	if c := r.conns[id]; c != nil {
		return c
	}
	c, err := ssh.Dial("tcp", "127.0.0.1:"+r.port, &ssh.ClientConfig{User: "hub", Auth: []ssh.AuthMethod{ssh.PublicKeys(r.keys[id])},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
	must(err)
	r.conns[id] = c
	return c
}

// topic makes a topic with subs subscriptions (their records written as the hub writes them).
func (r *rig) topic(i, subs int) string {
	tp := fmt.Sprintf(topicT, i)
	td := filepath.Join(r.root, "topics", tp)
	must(os.MkdirAll(filepath.Join(td, "subs"), 0o2770))
	must(os.WriteFile(filepath.Join(td, "policy"), []byte("topic="+tp+"\npublish="+sender+"\nrate=100000\nburst=1000000\n"), 0o640))
	for k := 0; k < subs; k++ {
		sub := fmt.Sprintf("sub.bench%d+%08x@expa.ssh", i, k)
		rec := fmt.Sprintf("sub=%s\ntopic=%s\nowner=%s\nmode=live\ncreated=1\nexpires=9999999999\n", sub, tp, rcpt)
		must(os.WriteFile(filepath.Join(td, "subs", sub), []byte(rec), 0o640))
		must(os.Symlink("../topics/"+tp+"/subs/"+sub, filepath.Join(r.root, "subs", sub)))
		must(os.MkdirAll(filepath.Join(r.root, "inbox", sub), 0o2770))
	}
	return tp
}

// --- operations --------------------------------------------------------------------------------------------

// sess runs one command as id: its exit code.
func (r *rig) sess(id, cmd string, stdin io.Reader, stdout io.Writer) int {
	s, err := r.conn(id).NewSession()
	must(err)
	defer s.Close()
	s.Stdin, s.Stdout = stdin, stdout
	var stderr bytes.Buffer
	s.Stderr = &stderr
	err = s.Run(cmd)
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		if ee.ExitStatus() != 0 && ee.ExitStatus() != 8 {
			fmt.Fprintf(os.Stderr, "  %q: exit %d %s", cmd, ee.ExitStatus(), stderr.String())
		}
		return ee.ExitStatus()
	}
	must(err)
	return 0
}

var (
	seq    int
	connMu sync.Mutex // the connection map
)

func nextID() string { seq++; return fmt.Sprintf("b%07d", seq) }

// sendN: n sends of body (one form, %s its --id), timed; the mailboxes emptied between, untimed.
func (r *rig) sendN(n int, form string, body []byte) row {
	var rw row
	rw.bytes = int64(len(body))
	for i := 0; i < n; i++ {
		cmd := form
		if strings.Contains(form, "%s") {
			cmd = fmt.Sprintf(form, nextID())
		}
		t0 := time.Now()
		if c := r.sess(sender, cmd, bytes.NewReader(body), io.Discard); c != 0 {
			fail(fmt.Errorf("%s: exit %d", cmd, c))
		}
		rw.ds = append(rw.ds, time.Since(t0))
		if len(body) > 1<<20 || i%50 == 49 {
			r.empty()
		}
	}
	r.empty()
	return rw
}

// empty removes every stored message, outside the timings.
func (r *rig) empty() {
	boxes, _ := filepath.Glob(filepath.Join(r.root, "inbox", "*"))
	for _, b := range boxes {
		ms, _ := filepath.Glob(filepath.Join(b, "*.msg"))
		for _, m := range ms {
			os.RemoveAll(m)
			os.RemoveAll(strings.TrimSuffix(m, ".msg")) // the previous hub's attachment folders
		}
	}
}

// readN: n reads by the recipient: a raw read of one 1 KiB message, a --peek listing of 100, or a
// consuming listing of 100 (refilled before each read, outside the timing).
func (r *rig) readN(n int, kind string) row {
	kib := bytes.Repeat([]byte("k"), 1024)
	count := 1
	if kind != "raw" {
		count = 100
	}
	var ids []string
	fill := func() {
		for i := 0; i < count; i++ {
			id := nextID()
			ids = append(ids, id)
			r.sess(sender, "send "+rcpt+" --id "+id+" -m", bytes.NewReader(kib), io.Discard)
		}
	}
	fill()
	var rw row
	for i := 0; i < n; i++ {
		cmd := "recv --peek"
		switch {
		case kind == "raw" && *old:
			cmd = "recv -m:rfc5322 " + ids[0] + " --keep"
		case kind == "raw":
			cmd = "recv --id " + ids[0] + " --peek:rfc5322"
		case kind == "consume":
			cmd = "recv"
			if i > 0 {
				fill()
			}
		}
		var out bytes.Buffer
		t0 := time.Now()
		r.sess(rcpt, cmd, nil, &out)
		rw.ds = append(rw.ds, time.Since(t0))
		rw.bytes = int64(out.Len())
	}
	r.empty()
	return rw
}

// ackN: n acks of a backlog of count 1 KiB messages (refilled before each, outside the timing): one
// `recv_ack --before-including <last>`, or `recv_ack -m:ids` in calls equal parts, timed together.
func (r *rig) ackN(n, count int, before bool, calls int) row {
	kib := bytes.Repeat([]byte("k"), 1024)
	var rw row
	for i := 0; i < n; i++ {
		ids := make([]string, count)
		for k := range ids {
			ids[k] = nextID()
			r.sess(sender, "send "+rcpt+" --id "+ids[k]+" -m", bytes.NewReader(kib), io.Discard)
		}
		var out bytes.Buffer
		t0 := time.Now()
		if before {
			if c := r.sess(rcpt, "recv_ack --before-including "+ids[count-1], nil, &out); c != 0 {
				fail(fmt.Errorf("recv_ack --before-including: exit %d", c))
			}
		} else {
			for k := 0; k < calls; k++ {
				part := ids[k*count/calls : (k+1)*count/calls]
				if c := r.sess(rcpt, "recv_ack -m:ids", strings.NewReader(strings.Join(part, "\n")+"\n"), &out); c != 0 {
					fail(fmt.Errorf("recv_ack -m:ids: exit %d", c))
				}
			}
		}
		rw.ds = append(rw.ds, time.Since(t0))
		if got := strings.Count(out.String(), "acked "); got != count {
			fail(fmt.Errorf("recv_ack: %d acked of %d", got, count))
		}
	}
	r.empty()
	return rw
}

var liveLineRE = regexp.MustCompile(`^LIVE ([0-9a-f]{16}) \S+ \S+`)

// liveN: n round trips through the recipient's follow and an accept per LIVE line: send --live --chunked,
// or a raw chat (ping one way, pong back, both close).
func (r *rig) liveN(n int, chat bool) row {
	s, err := r.conn(rcpt).NewSession()
	must(err)
	pr, pw := io.Pipe()
	s.Stdout = pw
	follow := "recv --follow --peek"
	if *old {
		follow = "recv --follow --keep"
	}
	must(s.Start(follow))
	var wg sync.WaitGroup
	go func() {
		sc := bufio.NewScanner(pr)
		live := *old // the old hub prints LIVE lines bare; this one in a part=live chunk, right after its size line
		for sc.Scan() {
			l := sc.Text()
			if !*old {
				was := live
				live = strings.HasSuffix(l, ";part=live") // bufio.ScanLines drops the CR
				if !was {
					continue
				}
			}
			m := liveLineRE.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			wg.Add(1)
			go func(sid string) {
				defer wg.Done()
				as, err := r.conn(rcpt).NewSession()
				if err != nil {
					return
				}
				defer as.Close()
				in, _ := as.StdinPipe()
				out, _ := as.StdoutPipe()
				as.Start("accept " + sid)
				if chat {
					br := bufio.NewReader(out)
					for { // the header block, then the ping
						l, err := br.ReadString('\n')
						if err != nil || l == "ping\n" {
							break
						}
					}
					io.WriteString(in, "Status: 200\n\npong\n")
					in.Close()
					io.Copy(io.Discard, br)
				} else {
					io.WriteString(in, "Status: 200\n\n"+frame("pong"))
					in.Close()
					io.Copy(io.Discard, out)
				}
				as.Wait()
			}(m[1])
		}
	}()
	time.Sleep(200 * time.Millisecond) // the follow is up
	var rw row
	for i := 0; i < n; i++ {
		t0 := time.Now()
		if chat {
			cs, err := r.conn(sender).NewSession()
			must(err)
			in, _ := cs.StdinPipe()
			out, _ := cs.StdoutPipe()
			must(cs.Start("chat " + rcpt))
			io.WriteString(in, "ping\n")
			b, _ := io.ReadAll(io.LimitReader(out, int64(len("Status: 200\n\npong\n"))))
			if string(b) != "Status: 200\n\npong\n" {
				fail(fmt.Errorf("chat: got %q", b))
			}
			in.Close()
			io.Copy(io.Discard, out)
			if err := cs.Wait(); err != nil {
				fail(fmt.Errorf("chat: %v", err))
			}
			cs.Close()
		} else {
			var out bytes.Buffer
			if c := r.sess(sender, "send "+rcpt+" --live --chunked --id "+nextID()+" -m", strings.NewReader(frame("ping")), &out); c != 0 {
				fail(fmt.Errorf("send --live: exit %d", c))
			}
			if !strings.HasSuffix(out.String(), "pong") {
				fail(fmt.Errorf("send --live: got %q", out.String()))
			}
		}
		rw.ds = append(rw.ds, time.Since(t0))
	}
	s.Close()
	pw.Close()
	wg.Wait()
	return rw
}

// wake: 1000 instances of addr with 10 unread each; the address's listing (its wake notice) after a daemon
// restart (the summary built from disk), then warm.
func (r *rig) wake(n int) row {
	const insts, unread = 1000, 10
	for b := 0; b < insts; b += 100 {
		var ids []string
		for i := b; i < b+100; i++ {
			ids = append(ids, fmt.Sprintf("i%04d", i))
		}
		if c := r.sess(addr, "admit "+strings.Join(ids, " "), nil, io.Discard); c != 0 {
			fail(fmt.Errorf("admit: exit %d", c))
		}
	}
	work := make(chan string)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cmd := range work {
				r.sessLocked(sender, cmd)
			}
		}()
	}
	for i := 0; i < insts; i++ {
		for j := 0; j < unread; j++ {
			work <- fmt.Sprintf("send group.sess+i%04d@expa.ssh --id m%d -- x", i, j)
		}
	}
	close(work)
	wg.Wait()
	r.stop()
	r.start()
	var rw row
	var out bytes.Buffer
	t0 := time.Now()
	r.sess(addr, "recv --peek", nil, &out)
	cold := time.Since(t0)
	lines := strings.Count(out.String(), "\tmail=10\t")
	if lines != insts {
		fail(fmt.Errorf("wake notice: %d lines", lines))
	}
	for i := 0; i < n; i++ {
		out.Reset()
		t0 := time.Now()
		r.sess(addr, "recv --peek", nil, &out)
		rw.ds = append(rw.ds, time.Since(t0))
	}
	rw.note = fmt.Sprintf("cold %.1f ms, %d KiB", ms(cold), out.Len()/1024)
	return rw
}

// sessLocked is sess from several goroutines, its output dropped.
func (r *rig) sessLocked(id, cmd string) {
	s, err := r.conn(id).NewSession()
	must(err)
	s.Run(cmd)
	s.Close()
}

// --- helpers --------------------------------------------------------------------------------------------

func frame(s string) string { return fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(s), s) }

// makeBag: a tar of about size bytes: m.md and data members of up to 63 MiB each.
func makeBag(size int) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "m.md", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6})
	tw.Write([]byte("body\n\n"))
	chunk := bytes.Repeat([]byte("B"), 63<<20)
	for k := 0; size > 0; k++ {
		n := len(chunk)
		if n > size {
			n = size
		}
		tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("d%d.bin", k), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(n)})
		tw.Write(chunk[:n])
		size -= n
	}
	tw.Close()
	return b.Bytes()
}

type row struct {
	name  string
	ds    []time.Duration
	bytes int64
	note  string
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func (rw row) cells() string {
	if len(rw.ds) == 0 {
		return "-"
	}
	s := append([]time.Duration(nil), rw.ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var sum time.Duration
	for _, d := range s {
		sum += d
	}
	med, p95 := s[len(s)/2], s[(len(s)*95+99)/100-1]
	tp := fmt.Sprintf("%.0f ops/s", float64(len(s))/sum.Seconds())
	if rw.bytes >= 1<<20 {
		tp = fmt.Sprintf("%.0f MiB/s", float64(rw.bytes)*float64(len(s))/sum.Seconds()/(1<<20))
	}
	if rw.note != "" {
		tp += "; " + rw.note
	}
	return fmt.Sprintf("%.2f  %.2f  %s", ms(med), ms(p95), tp)
}

func loadavg() string {
	b, _ := os.ReadFile("/proc/loadavg")
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return "?"
	}
	return strings.Join(f[:3], " ")
}

func cpus() int {
	b, _ := os.ReadFile("/proc/cpuinfo")
	return strings.Count(string(b), "\nprocessor") + 1
}

func must(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "hub-bench:", err)
	os.Exit(1)
}
