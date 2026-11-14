package hub

// Resolving a target, and the listing of targets. The spool itself: send.go, msgfile.go, recv.go.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

// resolveTarget: native for a claimed identity (live or lapsed — the mailbox is the address; two
// live claims is split brain), topic/sub for the hub's own addresses, else an alias's type.
func (s *Session) resolveTarget(t string) (typ, addr string, ok bool) {
	typ, addr, why, ok := s.h.resolve(t)
	if why != "" {
		s.errf("%s", why)
	}
	return typ, addr, ok
}

// resolve is resolveTarget without its stderr: why is what it would say. help keeps it to itself.
func (h *Hub) resolve(t string) (typ, addr, why string, ok bool) {
	if validIdent(t) {
		if hubRole(idRole(t)) {
			switch {
			case h.isTopic(t):
				return "topic", t, "", true
			case h.isSub(t):
				return "sub", t, "", true
			}
			return "", "", "", false
		}
		if parent, _, isInst := instSplit(t); isInst { // its mailbox is the address while its address takes instances
			if !h.instCapable(parent) {
				return "", "", "dispatch: " + parent + " takes no instances", false
			}
			return "native", t, "", true
		}
		n, live := 0, 0
		for _, f := range h.claimFind(t) {
			n++
			if h.claimLive(f) {
				live++
			}
		}
		if live > 1 {
			return "", "", fmt.Sprintf("ambiguous: %d live claims for %s — refusing to guess", live, t), false
		}
		if n >= 1 {
			return "native", t, "", true
		}
		return "", "", "", false
	}
	a := filepath.Join(h.aliases, t)
	if validAlias(t) && isFile(a) {
		r, _ := readRec(a)
		return r["type"], r["addr"], "", true
	}
	return "", "", "", false
}

var stageSeq atomic.Uint64

func (h *Hub) stageName(base string) string {
	return filepath.Join(base, fmt.Sprintf(".stage.%d.%d", os.Getpid(), stageSeq.Add(1)))
}

// fsynced: called with every path the hub fsyncs on a send's behalf; tests watch it. A no-op otherwise.
var fsynced = func(p string) {}

// syncPath fsyncs a file or directory (--durable).
func syncPath(p string) error {
	fsynced(p)
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (s *Session) doList(a []string) {
	h := s.h
	filter := ""
	if len(a) > 0 && a[0] == "--type" {
		if len(a) > 1 {
			filter = a[1]
		}
	}
	if filter == "" || filter == "native" {
		for _, m := range globNames(h.claims) {
			for _, n := range globNames(filepath.Join(h.claims, m)) {
				rp := filepath.Join(h.claims, m, n)
				if isFile(rp) {
					s.printf("%s\ttype=native\n", recGet(rp, "fq"))
				}
			}
		}
	}
	if filter == "" || filter == "topic" {
		s.topicList()
	}
	if filter == "" || filter == "sub" {
		s.subList()
	}
	for _, n := range globNames(h.aliases) {
		ap := filepath.Join(h.aliases, n)
		if !isFile(ap) {
			continue
		}
		r, _ := readRec(ap)
		if filter == "" || filter == r["type"] {
			s.printf("%s\ttype=%s\taddr=%s\n", n, r["type"], r["addr"])
		}
	}
}

// writeFile: b written to a new file at p, no fsync (docs/wake-live-event-id.md, Durability).
func writeFile(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
