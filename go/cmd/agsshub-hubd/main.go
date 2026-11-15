// agsshub-hubd — the agsshub hub as one daemon with its own SSH server (docs/hub.md).
//
//	agsshub-hubd [-listen :2422] [-hostkey /etc/hub/ssh_host_ed25519_key] [-root /var/lib/hub] …
//
// Users: `hub` (identity keys holding a live claim) and `reg` (workspace registrar keys).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cycanics/agsshub/go/hub"
)

func main() {
	var (
		listen     = flag.String("listen", ":2422", "address to listen on")
		hostKey    = flag.String("hostkey", "/etc/hub/ssh_host_ed25519_key", "host private key (OpenSSH format)")
		root       = flag.String("root", envOr("HUB_ROOT", "/var/lib/hub"), "state: claims, inbox, budgets, topics, …")
		lib        = flag.String("lib", "/usr/local/lib/hub", "backends/<type>/send plugins, help.json, help.txt")
		registrars = flag.String("registrars", "/etc/hub/registrars/authorized_keys", "registrar keys (hub-enroll)")
		maxSess    = flag.Int("max-sessions", 64, "session channels per connection")
		maxStart   = flag.Int("max-startups", 100, "connections in their handshake at once")
		alive      = flag.Duration("alive-interval", 15*time.Second, "keepalive probe after this much silence (0: off)")
		aliveMax   = flag.Int("alive-count", 4, "unanswered probes before a connection is dropped")
		reapEvery  = flag.Duration("reap-interval", time.Minute, "reaper sweep interval (0: off)")
		verbose    = flag.Bool("v", false, "log every command (verb and exit code, never content)")
	)
	flag.Parse()
	log.SetFlags(0)
	syscall.Umask(0o007)

	signer, err := hub.LoadHostKey(*hostKey)
	if err != nil {
		fatal("host key: %v", err)
	}
	h := hub.New(hub.Config{Root: *root, Lib: *lib, Registrars: *registrars, Logf: log.Printf})
	h.Verbose = *verbose
	srv := hub.NewServer(h, hub.ServerConfig{
		HostKey: signer, MaxSessions: *maxSess, MaxStartups: *maxStart, AliveInterval: *alive, AliveCountMax: *aliveMax, Verbose: *verbose,
	})
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		fatal("listen: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		if err := h.Watch(ctx); err != nil {
			log.Printf("watch: %v (claim changes are noticed by the 30 s poll only)", err)
		}
	}()
	if *reapEvery > 0 {
		go h.RunReaper(ctx, *reapEvery)
	}
	log.Printf("agsshub-hubd listening on %s, state %s", l.Addr(), *root)
	err = srv.Serve(ctx, l)
	if err != nil {
		fatal("serve: %v", err)
	}
	log.Printf("agsshub-hubd stopped")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agsshub-hubd: "+format+"\n", args...)
	os.Exit(1)
}
