package hub

// Test harness: a hub over a temp data dir: sessions run in-process (Dispatch, Registrar), the operator's
// claims come from the real lib/mint (what hub-mint runs), so every test also checks that the daemon reads
// what the operator tools write.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type th struct {
	t     *testing.T
	h     *Hub
	dir   string
	state string
	lib   string // the repo's lib/
	keys  map[string]tkey
	mu    sync.Mutex
	logs  syncBuf // the hub's log lines
}

type tkey struct {
	pub    ssh.PublicKey
	signer ssh.Signer
	line   string // "ssh-ed25519 AAAA… name\n"
	blob   string
}

func newTH(t *testing.T) *th {
	t.Helper()
	dir := t.TempDir()
	x := &th{t: t, dir: dir, state: filepath.Join(dir, "state"), keys: map[string]tkey{}}
	wd, _ := os.Getwd()
	x.lib = filepath.Join(wd, "..", "..", "lib")
	for _, d := range []string{"claims/operator", "claims/instance", "claims/.ibyfpr", "orgs", "budgets", "aliases", "topics", "subs", "inbox", "flood", "described"} {
		os.MkdirAll(filepath.Join(x.state, d), 0o2770)
	}
	os.WriteFile(filepath.Join(x.state, "orgs", "ws-a"), []byte("acme\npersonal\n"), 0o644)
	os.WriteFile(filepath.Join(x.state, "orgs", "ws-b"), []byte("acme\npersonal\n"), 0o644)
	os.WriteFile(filepath.Join(x.state, "orgs", "ws-c"), []byte("corp\n"), 0o644)
	x.h = New(Config{Root: x.state, Lib: x.lib, Registrars: filepath.Join(dir, "registrars"),
		Logf: func(f string, a ...any) { t.Logf(f, a...); x.logs.Write([]byte(fmt.Sprintf(f, a...) + "\n")) }})
	ctx, cancel := context.WithCancel(context.Background())
	watching := make(chan struct{})
	go func() { x.h.Watch(ctx); close(watching) }()
	t.Cleanup(func() { cancel(); <-watching })
	return x
}

// key makes (once) an ed25519 key named name.
func (x *th) key(name string) tkey {
	x.mu.Lock()
	defer x.mu.Unlock()
	if k, ok := x.keys[name]; ok {
		return k
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		x.t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(pub)
	signer, _ := ssh.NewSignerFromKey(priv)
	blob := keyBlob(sp)
	k := tkey{pub: sp, signer: signer, blob: blob, line: "ssh-ed25519 " + blob + " " + name + "\n"}
	x.keys[name] = k
	return k
}

func (x *th) keyid(name string) string { return keyID(x.key(name).blob) }

// mint runs the operator's lib/mint (hub-mint minus its root check).
func (x *th) mint(stdin string, args ...string) (string, int) {
	x.t.Helper()
	cmd := exec.Command(filepath.Join(x.lib, "mint"), args...)
	cmd.Env = append(os.Environ(), "HUB_ROOT="+x.state)
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		x.t.Fatalf("mint: %v", err)
	}
	return out.String(), code
}

func (x *th) mustMint(id, keyName string, flags ...string) {
	x.t.Helper()
	out, code := x.mint(x.key(keyName).line, append([]string{"add", id}, flags...)...)
	if code != 0 {
		x.t.Fatalf("mint add %s: %d %s", id, code, out)
	}
}

// bash runs a snippet with lib/common.sh sourced, what the operator tools read.
func (x *th) bash(script string, args ...string) string {
	x.t.Helper()
	cmd := exec.Command("bash", append([]string{"-c", ". \"$0\"; " + script, filepath.Join(x.lib, "common.sh")}, args...)...)
	cmd.Env = append(os.Environ(), "HUB_ROOT="+x.state)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

type result struct {
	out, err string
	code     int
}

// reg runs a registrar command for workspace ws.
func (x *th) reg(ws, cmd, stdin string) result {
	var o, e bytes.Buffer
	code := x.h.Registrar(context.Background(), ws, cmd, IO{Stdin: strings.NewReader(stdin), Stdout: &o, Stderr: &e})
	return result{o.String(), e.String(), code}
}

func (x *th) serial(id string) string {
	for _, f := range x.h.claimFind(id) {
		if s := recGet(f, "serial"); s != "" {
			return s
		}
	}
	return "1"
}

// dis runs one command as me, held to its current serial: no key id.
func (x *th) dis(me, cmd string, stdin ...string) result {
	return x.disA(Admission{FQ: me, Serial: x.serial(me)}, cmd, stdin...)
}

func (x *th) disA(a Admission, cmd string, stdin ...string) result {
	in := ""
	if len(stdin) > 0 {
		in = stdin[0]
	}
	var o, e bytes.Buffer
	code := x.h.Dispatch(context.Background(), a, cmd, IO{Stdin: strings.NewReader(in), Stdout: &o, Stderr: &e})
	return result{o.String(), e.String(), code}
}

// --- sessions that stay open -------------------------------------------------------------------

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type bg struct {
	out, err *syncBuf
	in       *io.PipeWriter
	cancel   context.CancelFunc
	done     chan int
	code     int
}

// start runs a command in the background; its stdin stays open until closeIn (or stdin is given).
func (x *th) start(a Admission, cmd string, stdin ...string) *bg {
	return x.startOut(a, cmd, nil, stdin...)
}

// startOut: start with stdout written to out (nil: b.out).
func (x *th) startOut(a Admission, cmd string, out io.Writer, stdin ...string) *bg {
	ctx, cancel := context.WithCancel(context.Background())
	b := &bg{out: &syncBuf{}, err: &syncBuf{}, cancel: cancel, done: make(chan int, 1)}
	var stdout io.Writer = b.out
	if out != nil {
		stdout = out
	}
	var in io.Reader
	if len(stdin) > 0 {
		in = strings.NewReader(stdin[0])
	} else {
		pr, pw := io.Pipe()
		b.in = pw
		in = pr
		go func() { <-ctx.Done(); pw.Close() }()
	}
	go func() {
		b.done <- x.h.Dispatch(ctx, a, cmd, IO{Stdin: in, Stdout: stdout, Stderr: b.err})
	}()
	x.t.Cleanup(func() { cancel() })
	return b
}

func (x *th) adm(me string) Admission { return Admission{FQ: me, Serial: x.serial(me)} }

// wait for the command's exit, at most d.
func (b *bg) wait(t *testing.T, d time.Duration) int {
	t.Helper()
	select {
	case b.code = <-b.done:
		return b.code
	case <-time.After(d):
		t.Fatalf("command did not end within %v; stdout %q stderr %q", d, b.out.String(), b.err.String())
	}
	return -1
}

// waitFor polls until cond holds, at most d.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (x *th) followed(name string) func() bool {
	return func() bool { return x.h.follows.following(name) }
}

// --- assertions ----------------------------------------------------------------------------------

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: got [%v] want [%v]", what, got, want)
	}
}

func has(t *testing.T, what, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("%s: [%s] lacks [%s]", what, s, sub)
	}
}

func lacks(t *testing.T, what, s, sub string) {
	t.Helper()
	if strings.Contains(s, sub) {
		t.Errorf("%s: [%s] has [%s]", what, s, sub)
	}
}

func code(t *testing.T, what string, r result, want int) {
	t.Helper()
	if r.code != want {
		t.Errorf("%s: exit %d want %d (stdout %q stderr %q)", what, r.code, want, r.out, r.err)
	}
}

// frame: chunked encoding of b in chunks of cs, then the last chunk.
func frame(b []byte, cs int, ext string) []byte {
	var o bytes.Buffer
	for off := 0; off < len(b); off += cs {
		n := cs
		if len(b)-off < n {
			n = len(b) - off
		}
		fmt.Fprintf(&o, "%x%s\r\n", n, ext)
		o.Write(b[off : off+n])
		o.WriteString("\r\n")
	}
	o.WriteString("0\r\n\r\n")
	return o.Bytes()
}

// makeTar is `tar -C dir -cf - .` of files (path -> content): "./" first, directories before files.
func makeTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	dir := t.TempDir()
	for p, c := range files {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644)
	}
	out, err := exec.Command("tar", "-C", dir, "-cf", "-", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}
