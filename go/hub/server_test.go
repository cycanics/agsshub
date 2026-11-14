package hub

// The SSH layer: (keepalive, MaxSessions, no TTY/forwarding), the exit
// status a closing client must still get, and sessions that end with their connection.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type tserver struct {
	x      *th
	srv    *Server
	addr   string
	cancel context.CancelFunc
}

func startServer(t *testing.T, x *th, cfg ServerConfig) *tserver {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hk, _ := ssh.NewSignerFromKey(priv)
	cfg.HostKey = hk
	srv := NewServer(x.h, cfg)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return &tserver{x: x, srv: srv, addr: l.Addr().String(), cancel: cancel}
}

func (ts *tserver) dial(t *testing.T, user, keyName string, via ...string) *ssh.Client {
	t.Helper()
	addr := ts.addr
	if len(via) > 0 {
		addr = via[0]
	}
	c, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(ts.x.key(keyName).signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial %s as %s: %v", user, keyName, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// run is `ssh hub <cmd>`: stdout, stderr, exit status.
func runSSH(c *ssh.Client, cmd, stdin string) (string, string, int, error) {
	s, err := c.NewSession()
	if err != nil {
		return "", "", -1, err
	}
	defer s.Close()
	var o, e strings.Builder
	s.Stdout, s.Stderr, s.Stdin = &o, &e, strings.NewReader(stdin)
	err = s.Run(cmd)
	code := 0
	if ee, ok := err.(*ssh.ExitError); ok {
		code, err = ee.ExitStatus(), nil
	}
	return o.String(), e.String(), code, err
}

func TestServerAuthAndExec(t *testing.T) {
	x := twoRuns(t)
	x.mustMint("commhub@home.ssh", "hubk")
	ts := startServer(t, x, ServerConfig{})
	c := ts.dial(t, "hub", "hubk")
	o, _, code, err := runSSH(c, "id", "")
	eq(t, "the key logs in as its identity", fmt.Sprint(o, code, err), "commhub@home.ssh serial=1\n0 <nil>")
	_, e, code, _ := runSSH(c, "send nobody@x.ssh -- hi", "")
	eq(t, "exit codes and stderr come back", fmt.Sprint(code, " ", e), "3 dispatch: no such target nobody@x.ssh\n")
	o, _, _, _ = runSSH(c, "send "+bob+" --id s-1 -- a    b", "")
	eq(t, "the raw command arrives verbatim", o, "sent -> "+bob+" msgid=s-1\n")
	has(t, "and the inline message too", x.dis(bob, "recv").out, "a    b")
	for _, who := range []struct{ user, key string }{{"hub", "stranger"}, {"root", "hubk"}, {"reg", "hubk"}} {
		_, err := ssh.Dial("tcp", ts.addr, &ssh.ClientConfig{User: who.user, Auth: []ssh.AuthMethod{ssh.PublicKeys(x.key(who.key).signer)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
		if err == nil {
			t.Errorf("%s with key %s was admitted", who.user, who.key)
		}
	}
	// the registrar: a key enrolled by hub-enroll (registrars' authorized_keys, command= carries the ws)
	os.WriteFile(x.h.cfg.Registrars, []byte(`restrict,command="/usr/local/lib/hub/registrar ws-a" `+strings.TrimSpace(x.key("regk").line)+"\n"), 0o644)
	rc := ts.dial(t, "reg", "regk")
	o, _, code, _ = runSSH(rc, "register run.t+new@acme.ssh --ttl 600", x.key("newk").line)
	eq(t, "the registrar registers through its own key", code, 0)
	has(t, "pinned to its workspace", o, "registered run.t+new/ws-a@acme.ssh ")
	nc := ts.dial(t, "hub", "newk")
	o, _, _, _ = runSSH(nc, "id", "")
	eq(t, "and the new key logs in", o, "run.t+new/ws-a@acme.ssh serial=1\n")
}

// A client that closes its session as soon as both directions are at EOF (OpenSSH does) still gets
// the exit status: it is sent before the hub's EOF.
func TestServerExitStatusBeforeEOF(t *testing.T) {
	x := twoRuns(t)
	x.mustMint("commhub@home.ssh", "hubk")
	ts := startServer(t, x, ServerConfig{})
	c := ts.dial(t, "hub", "hubk")
	var wg sync.WaitGroup
	var mu sync.Mutex
	missing := 0
	sem := make(chan struct{}, 32) // under MaxSessions
	for i := 0; i < 1500; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			ch, reqs, err := c.OpenChannel("session", nil)
			if err != nil {
				t.Error(err)
				return
			}
			ch.SendRequest("exec", true, ssh.Marshal(struct{ Command string }{"id"}))
			ch.CloseWrite()
			io.Copy(io.Discard, ch)
			ch.Close() // both directions done: close at once
			got := false
			for r := range reqs {
				if r.Type == "exit-status" {
					got = true
				}
			}
			if !got {
				mu.Lock()
				missing++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	eq(t, "sessions without an exit status", missing, 0)
}

func TestServerRefusals(t *testing.T) {
	x := twoRuns(t)
	x.mustMint("commhub@home.ssh", "hubk")
	ts := startServer(t, x, ServerConfig{MaxSessions: 2})
	c := ts.dial(t, "hub", "hubk")
	s, _ := c.NewSession()
	if err := s.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err == nil {
		t.Error("a pty was granted")
	}
	if err := s.Shell(); err == nil {
		t.Error("a shell was granted")
	}
	s.Close()
	if err := func() error { s, _ := c.NewSession(); defer s.Close(); return s.RequestSubsystem("sftp") }(); err == nil {
		t.Error("a subsystem was granted")
	}
	if _, err := c.Dial("tcp", "127.0.0.1:22"); err == nil {
		t.Error("a direct-tcpip channel was opened")
	}
	if _, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Error("remote forwarding was granted")
	}
	// MaxSessions: two open, the third refused, a slot freed is a slot again
	var held []ssh.Channel
	for i := 0; i < 2; i++ {
		ch, _, err := c.OpenChannel("session", nil)
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		held = append(held, ch)
	}
	if _, _, err := c.OpenChannel("session", nil); err == nil {
		t.Error("a third session was opened with MaxSessions 2")
	} else {
		has(t, "refused for resource shortage", err.Error(), "resource shortage")
	}
	held[0].Close()
	waitFor(t, 2*time.Second, "a free slot", func() bool {
		ch, _, err := c.OpenChannel("session", nil)
		if err == nil {
			ch.Close()
		}
		return err == nil
	})
}

// freezer: a TCP proxy that can stop forwarding, like a client behind a dead network.
type freezer struct {
	l      net.Listener
	mu     sync.Mutex
	frozen bool
	conns  []net.Conn
}

func newFreezer(t *testing.T, target string) *freezer {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	f := &freezer{l: l}
	go func() {
		for {
			a, err := l.Accept()
			if err != nil {
				return
			}
			b, err := net.Dial("tcp", target)
			if err != nil {
				a.Close()
				continue
			}
			f.mu.Lock()
			f.conns = append(f.conns, a, b)
			f.mu.Unlock()
			go f.pipe(a, b)
			go f.pipe(b, a)
		}
	}()
	t.Cleanup(func() {
		l.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, c := range f.conns {
			c.Close()
		}
	})
	return f
}

func (f *freezer) pipe(dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		for {
			f.mu.Lock()
			fr := f.frozen
			f.mu.Unlock()
			if !fr {
				break
			}
			time.Sleep(10 * time.Millisecond) // frozen: hold the bytes, answer nothing
		}
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (f *freezer) freeze() { f.mu.Lock(); f.frozen = true; f.mu.Unlock() }

// sshd's ClientAliveInterval/CountMax: a client that stops answering is dropped after
// interval × (count+1) of silence, and its follow ends; a quiet client that answers stays.
func TestServerKeepaliveDropsFrozenClient(t *testing.T) {
	x := twoRuns(t)
	x.mustMint("commhub@home.ssh", "hubk")
	ts := startServer(t, x, ServerConfig{AliveInterval: 100 * time.Millisecond, AliveCountMax: 2})
	fr := newFreezer(t, ts.addr)
	c := ts.dial(t, "hub", "hubk", fr.l.Addr().String())
	s, _ := c.NewSession()
	s.Stdout = io.Discard
	s.Start("recv --follow --peek")
	waitFor(t, 2*time.Second, "the follow", x.followed("commhub@home.ssh"))
	time.Sleep(600 * time.Millisecond) // quiet but answering: kept
	eq(t, "a quiet client that answers keepalives stays", x.h.follows.following("commhub@home.ssh"), true)
	t0 := time.Now()
	fr.freeze()
	waitFor(t, 3*time.Second, "the drop", func() bool { return !x.h.follows.following("commhub@home.ssh") })
	if d := time.Since(t0); d < 200*time.Millisecond {
		t.Errorf("dropped after %v, before its probes went unanswered", d)
	}
}

// A follow ends the moment its connection closes (not at its next wake, with the process left behind
// until then — dev-hub.sh counts that process).
func TestServerFollowEndsWithConnection(t *testing.T) {
	x := twoRuns(t)
	x.mustMint("group.demo@expa.ssh", "grpk")
	x.mustMint("commhub@home.ssh", "hubk")
	ts := startServer(t, x, ServerConfig{})
	c := ts.dial(t, "hub", "grpk")
	s, _ := c.NewSession()
	out := &syncBuf{}
	s.Stdout = out
	s.Start("recv --follow --peek")
	waitFor(t, 2*time.Second, "the follow", x.followed("group.demo@expa.ssh"))
	hub := ts.dial(t, "hub", "hubk")
	t0 := time.Now()
	runSSH(hub, "send group.demo@expa.ssh --id f-1 -- one", "")
	waitFor(t, 2*time.Second, "the message", func() bool { return strings.Contains(out.String(), ";id=f-1;") })
	t.Logf("send -> follower printed: %v", time.Since(t0))
	c.Close() // the client vanishes
	waitFor(t, time.Second, "the follower gone", func() bool { return !x.h.follows.following("group.demo@expa.ssh") })
	// it reattaches at once and sees the backlog again (--peek)
	c2 := ts.dial(t, "hub", "grpk")
	s2, _ := c2.NewSession()
	out2 := &syncBuf{}
	s2.Stdout = out2
	s2.Start("recv --follow --peek")
	waitFor(t, 2*time.Second, "the backlog", func() bool { return strings.Contains(out2.String(), ";id=f-1;") })
	// a newer follower replaces it: the old one ends with 0;end=replaced, exit 0
	c3 := ts.dial(t, "hub", "grpk")
	s3, _ := c3.NewSession()
	s3.Stdout = io.Discard
	s3.Start("recv --follow --peek")
	errc := make(chan error, 1)
	go func() { errc <- s2.Wait() }()
	select {
	case err := <-errc:
		eq(t, "the replaced follower exits 0", err, nil)
	case <-time.After(2 * time.Second):
		t.Error("the replaced follower did not end")
	}
	// by= names the new follow: `<conn>.<session>`, the connection id the hub logs it under
	m := regexp.MustCompile(`follow group\.demo@expa\.ssh conn=((\d+)\.\d+) replaced conn=((\d+)\.\d+)`).FindStringSubmatch(x.logs.String())
	if m == nil {
		t.Fatalf("no takeover log line in %q", x.logs.String())
	}
	has(t, "the new connection was accepted under that id", x.logs.String(), "conn "+m[2]+": accepted hub for group.demo@expa.ssh")
	eq(t, "two connections", m[2] != m[4], true)
	eq(t, "the old stream's final chunk names the new follow", final(out2.String()), ";end=replaced;by="+m[1])
	_ = filepath.Join
}

// D42: two follows multiplexed over one ssh connection (ControlMaster) get distinct follow ids: the
// takeover log and by= tell them apart.
func TestServerTakeoverOneConnection(t *testing.T) {
	x := twoRuns(t)
	x.mustMint("group.demo@expa.ssh", "grpk")
	ts := startServer(t, x, ServerConfig{})
	c := ts.dial(t, "hub", "grpk")
	s1, _ := c.NewSession()
	out1 := &syncBuf{}
	s1.Stdout = out1
	s1.Start("recv --follow --peek")
	waitFor(t, 2*time.Second, "the first follow", x.followed("group.demo@expa.ssh"))
	s2, _ := c.NewSession()
	s2.Stdout = io.Discard
	s2.Start("recv --follow --peek")
	errc := make(chan error, 1)
	go func() { errc <- s1.Wait() }()
	select {
	case err := <-errc:
		eq(t, "the replaced follow exits 0", err, nil)
	case <-time.After(2 * time.Second):
		t.Fatal("the replaced follow did not end")
	}
	m := regexp.MustCompile(`follow group\.demo@expa\.ssh conn=((\d+)\.(\d+)) replaced conn=((\d+)\.(\d+))`).FindStringSubmatch(x.logs.String())
	if m == nil {
		t.Fatalf("no takeover log line in %q", x.logs.String())
	}
	eq(t, "one ssh connection", m[2], m[5])
	eq(t, "two follow ids", m[1] != m[4], true)
	eq(t, "by= names the new follow", final(out1.String()), ";end=replaced;by="+m[1])
	s2.Close()
}
