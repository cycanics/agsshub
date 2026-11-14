package hub

// The SSH server: exec requests only, for two users — `hub` (an identity's key, admitted while it
// holds a live claim) and `reg` (a workspace's registrar key). ClientAliveInterval 15 /
// ClientAliveCountMax 4, MaxSessions 64, no TTY, no forwarding, no agent.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// ServerConfig tunes the connection layer.
type ServerConfig struct {
	HostKey          ssh.Signer
	MaxSessions      int           // session channels per connection
	AliveInterval    time.Duration // keepalive probe after this much silence
	AliveCountMax    int           // probes unanswered before the connection is dropped
	HandshakeTimeout time.Duration
	MaxStartups      int // connections in their handshake at once; more are closed
	Verbose          bool
}

// Server serves the hub over SSH.
type Server struct {
	h     *Hub
	cfg   ServerConfig
	sshc  *ssh.ServerConfig
	mu    sync.Mutex
	conns map[*ssh.ServerConn]struct{}
	wg    sync.WaitGroup

	startups chan struct{} // handshakes in progress
}

var errDenied = errors.New("denied")

// NewServer makes the SSH front of h.
func NewServer(h *Hub, cfg ServerConfig) *Server {
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 64
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 30 * time.Second
	}
	if cfg.MaxStartups <= 0 {
		cfg.MaxStartups = 100
	}
	s := &Server{h: h, cfg: cfg, conns: map[*ssh.ServerConn]struct{}{}, startups: make(chan struct{}, cfg.MaxStartups)}
	s.sshc = &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-agsshub-hubd",
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			switch c.User() {
			case "hub":
				if a, ok := h.admitHub(key); ok {
					return &ssh.Permissions{Extensions: map[string]string{"fq": a.FQ, "serial": a.Serial, "keyid": a.KeyID}}, nil
				}
			case "reg":
				if ws, ok := h.admitRegistrar(key); ok {
					return &ssh.Permissions{Extensions: map[string]string{"ws": ws}}, nil
				}
			}
			return nil, errDenied
		},
	}
	s.sshc.AddHostKey(cfg.HostKey)
	return s
}

// Serve accepts connections on l until ctx ends, then closes them.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()
		l.Close()
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
	}()
	for {
		nc, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.wg.Wait()
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(ctx, nc)
		}()
	}
}

// trackConn notes when the client last sent anything (keepalive accounting).
type trackConn struct {
	net.Conn
	last atomic.Int64
}

func (t *trackConn) Read(b []byte) (int, error) {
	n, err := t.Conn.Read(b)
	if n > 0 {
		t.last.Store(time.Now().UnixNano())
	}
	return n, err
}

func (s *Server) serveConn(ctx context.Context, nc net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			s.h.logf("connection %s: panic: %v\n%s", nc.RemoteAddr(), r, debug.Stack())
		}
	}()
	select {
	case s.startups <- struct{}{}:
	default:
		nc.Close() // too many connections that have not authenticated yet
		return
	}
	tc := &trackConn{Conn: nc}
	nc.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	sconn, chans, reqs, err := ssh.NewServerConn(tc, s.sshc)
	<-s.startups
	if err != nil {
		if s.cfg.Verbose && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "connection reset") {
			s.h.logf("refused %s: %v", nc.RemoteAddr(), err)
		}
		nc.Close()
		return
	}
	nc.SetDeadline(time.Time{})
	perms := sconn.Permissions.Extensions
	who := perms["fq"]
	if sconn.User() == "reg" {
		who = "registrar " + perms["ws"]
	}
	cid := connSeq.Add(1)
	s.h.logf("conn %d: accepted %s for %s from %s", cid, sconn.User(), who, nc.RemoteAddr())
	s.mu.Lock()
	s.conns[sconn] = struct{}{}
	s.mu.Unlock()
	cctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.conns, sconn)
		s.mu.Unlock()
		sconn.Close()
	}()
	go ssh.DiscardRequests(reqs) // global requests: forwarding refused; a client keepalive gets its reply
	go s.keepalive(cctx, sconn, tc)
	var open atomic.Int32
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.Prohibited, "only sessions")
			continue
		}
		if int(open.Load()) >= s.cfg.MaxSessions {
			nch.Reject(ssh.ResourceShortage, "too many sessions on this connection")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			continue
		}
		n := open.Add(1)
		sid := fmt.Sprintf("%d.%d", cid, chanSeq.Add(1))
		if s.cfg.Verbose {
			s.h.logf("session %s: open (%d on the connection)", sid, n)
		}
		go func() {
			defer open.Add(-1)
			s.serveSession(cctx, sconn, ch, creqs, sid)
		}()
	}
	if s.cfg.Verbose {
		s.h.logf("conn %d: closed", cid)
	}
}

var connSeq, chanSeq atomic.Uint64

// keepalive: after each interval of silence a
// probe; more than AliveCountMax probes unanswered and the connection is dropped. Anything the
// client sends counts as an answer.
func (s *Server) keepalive(ctx context.Context, c *ssh.ServerConn, tc *trackConn) {
	if s.cfg.AliveInterval <= 0 {
		return
	}
	t := time.NewTicker(s.cfg.AliveInterval)
	defer t.Stop()
	seen, missed := tc.last.Load(), 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if cur := tc.last.Load(); cur != seen {
			seen, missed = cur, 0
			continue
		}
		if missed++; missed > s.cfg.AliveCountMax {
			s.h.logf("keepalive: %s did not answer %d probes; dropped", c.RemoteAddr(), s.cfg.AliveCountMax)
			c.Close()
			return
		}
		go c.SendRequest("keepalive@openssh.com", true, nil)
	}
}

// serveSession runs one session channel: its first exec request, nothing else. The command runs
// with a context that ends when the channel or the connection closes.
func (s *Server) serveSession(connCtx context.Context, c *ssh.ServerConn, ch ssh.Channel, reqs <-chan *ssh.Request, sid string) {
	ctx, cancel := context.WithCancel(connCtx)
	defer cancel()
	started := false
	for req := range reqs {
		if req.Type != "exec" || started {
			req.Reply(false, nil) // pty-req, shell, subsystem, env, x11, agent, signal: refused
			continue
		}
		var p struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &p); err != nil {
			req.Reply(false, nil)
			continue
		}
		started = true
		req.Reply(true, nil)
		go s.exec(ctx, connCtx, c, ch, p.Command, sid)
	}
	// the channel closed: by the client, or with its connection
	if s.cfg.Verbose {
		s.h.logf("session %s: closed", sid)
	}
}

func (s *Server) exec(ctx, connCtx context.Context, c *ssh.ServerConn, ch ssh.Channel, cmd, sid string) {
	defer func() {
		if r := recover(); r != nil {
			s.h.logf("exec %q: panic: %v\n%s", firstN(cmd, 80), r, debug.Stack())
			ch.Close()
		}
	}()
	closeWrite := sync.OnceValue(ch.CloseWrite) // a live request's end, or the session's: EOF once
	connGone := func() bool { return connCtx.Err() != nil }
	cio := IO{Stdin: ch, Stdout: ch, Stderr: ch.Stderr(), CloseOut: closeWrite, ConnGone: connGone}
	perms := c.Permissions.Extensions
	start := time.Now()
	verb := "recv"
	if w := words(cmd); len(w) > 0 {
		verb = w[0]
	}
	if s.cfg.Verbose {
		s.h.logf("session %s: exec %s %s", sid, c.User(), verb)
	}
	var code int
	switch c.User() {
	case "hub":
		code = s.h.Dispatch(ctx, Admission{FQ: perms["fq"], Serial: perms["serial"], KeyID: perms["keyid"], Conn: sid}, cmd, cio)
	case "reg":
		code = s.h.Registrar(ctx, perms["ws"], cmd, cio)
	default:
		code = 1
	}
	// exit-status before EOF: a client closes its session once both directions are at EOF (OpenSSH
	// does, with its stdin done), and a status sent after that close is lost — the client exits 255.
	_, err := ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
	closeWrite()
	ch.Close()
	if s.cfg.Verbose {
		s.h.logf("session %s: %s exit %d (%s)%s", sid, verb, code, time.Since(start).Round(time.Millisecond), errNote(err))
	}
}

func errNote(err error) string {
	if err != nil {
		return " [exit-status not delivered: " + err.Error() + "]"
	}
	return ""
}

// LoadHostKey reads an OpenSSH private key file.
func LoadHostKey(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}
