package hub

// The registrar: a workspace's registrar key registers `run` identities pinned to
// that workspace. The workspace comes from the key's enrolment, never from the client.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (s *Session) registrar() {
	h := s.h
	if !validWS(s.ws) {
		s.die(2, "registrar: bad or missing ws")
	}
	for _, d := range []string{filepath.Join(h.claims, s.ws), h.byfpr, h.tmpd, h.tomb, h.seriald} {
		os.MkdirAll(d, 0o2770)
	}
	a := words(s.raw)
	verb := ""
	if len(a) > 0 {
		verb = a[0]
	}
	switch verb {
	case "register":
		s.regRegister(a)
	case "release":
		s.regRelease(a)
	case "list":
		s.regList()
	default:
		s.die(2, "usage: register <identity> [--ttl N] [--mode connection|session|persistent] | release <identity> | list")
	}
}

func (s *Session) serialPath(fq string) string {
	return filepath.Join(s.h.seriald, s.ws+"__"+fsName(fq))
}

func (s *Session) tombPath(fq string) string { return filepath.Join(s.h.tomb, s.ws+"__"+fsName(fq)) }

// wsIdent: the identity this workspace registers for a request; exits 2 (malformed) or 3 (not
// this workspace's to make). A pinned role's /<ws> is appended here; a written one must be ours.
func (s *Session) wsIdent(req string) string {
	if !strings.Contains(req, "@") || !validSuffix(req) {
		s.die(2, "registrar: need <role>[.<project>][+<instance>]@<org>%s", idSuffix)
	}
	p := idSplit(req)
	row, ok := roleRow(p.Role)
	if !ok {
		s.die(2, "registrar: %s", identProblem(req))
	}
	switch row.Minter {
	case "hub":
		s.die(3, "registrar: role %s is the hub's own; it cannot be claimed", p.Role)
	case "operator":
		s.die(3, "registrar: role %s is minted by the hub operator (hub-mint), not by a workspace", p.Role)
	}
	fq := req
	if row.Shape == "pinned" {
		if p.WS == "" {
			fq = p.Name + "/" + s.ws + "@" + p.Org + idSuffix // the hub appends the workspace
		} else if validWS(p.WS) && p.WS != s.ws {
			s.die(3, "registrar: %s cannot mint an identity pinned to %s", s.ws, p.WS)
		}
	}
	if why := identProblem(fq); why != "" {
		s.die(2, "registrar: %s", why)
	}
	if !s.h.wsMayClaimOrg(s.ws, p.Org) {
		s.die(3, "registrar: %s is not enrolled for org %s", s.ws, p.Org)
	}
	return fq
}

func (s *Session) regRegister(a []string) {
	h := s.h
	req := ""
	if len(a) > 1 {
		req = a[1]
	}
	fq := s.wsIdent(req)
	id := idSplit(fq)
	ttl, mode := "3600", "session"
	for i := 2; i < len(a); i += 2 {
		v := ""
		if i+1 < len(a) {
			v = a[i+1]
		}
		switch a[i] {
		case "--ttl":
			ttl = v
		case "--mode":
			mode = v
		default:
			s.die(2, "registrar: unexpected arg '%s'", a[i])
		}
	}
	if !ttlRE.MatchString(ttl) {
		s.die(2, "registrar: bad ttl")
	}
	t, err := strconv.ParseInt(ttl, 10, 64)
	if err != nil || t > 86400 {
		t = 86400
	}
	if t < 60 {
		t = 60
	}
	switch mode {
	case "connection", "session", "persistent":
	default:
		s.die(2, "registrar: bad mode")
	}
	pub, _ := io.ReadAll(io.LimitReader(s.in, 64*1024))
	parts := strings.Fields(string(pub))
	ktype, kblob := "", ""
	if len(parts) > 0 {
		ktype = parts[0]
	}
	if len(parts) > 1 {
		kblob = parts[1]
	}
	if ktype != "ssh-ed25519" || kblob == "" {
		s.die(2, "registrar: need an ssh-ed25519 pubkey on stdin")
	}
	keyid := keyID(kblob)
	fpr := fingerprint(ktype, kblob)
	now := h.epoch()
	exp := now + t
	dest := h.claimPath(fq, s.ws)
	sp, tp := s.serialPath(fq), s.tombPath(fq)
	rel := "../" + s.ws + "/" + fsName(fq)

	unlock, err := h.lockRegistry()
	if err != nil {
		s.die(1, "registrar: registry busy; retry")
	}
	defer unlock()
	if other, live := h.claimLiveElsewhere(fq, s.ws); live {
		s.die(1, "claim-conflict: %s is live from %s — a second live claim would be split brain", fq, other)
	}
	if other, bound := h.keyBoundElsewhere(keyid, rel); bound {
		s.die(1, "registrar: key already bound to %s", other)
	}
	renew, reclaim := false, false
	oldKeyid := ""
	if existsFollow(dest) {
		cur, _ := readRec(dest)
		oldKeyid = cur["keyid"]
		curExp, expOK := atoi64(cur["expires"])
		switch {
		case oldKeyid == keyid:
			renew = true
		case cur["expires"] != "" && expOK && curExp < now:
			reclaim = true // a different key, but the claim has expired
		default:
			s.die(1, "claim-conflict: %s held by %s until %s", fq, cur["fpr"], isoTimeStr(cur["expires"]))
		}
	} else if isFile(tp) {
		tr, _ := readRec(tp)
		until, uok := atoi64(tr["until"])
		if tr["keyid"] != keyid && tr["until"] != "" && uok && now < until {
			s.die(1, "claim-conflict: %s tombstoned until %s", fq, isoTime(until))
		}
		os.Remove(tp)
	}
	// Generation serial: unchanged on same-key renew, else next value (survives release via sp).
	var serial string
	if renew {
		serial = recGet(dest, "serial")
	} else {
		n, _ := atoi64(strings.TrimSpace(readString(sp)))
		serial = strconv.FormatInt(n+1, 10)
	}
	tmp := filepath.Join(h.tmpd, fmt.Sprintf("%d.%d", os.Getpid(), stageSeq.Add(1)))
	body := fmt.Sprintf("name=%s\norg=%s\nws=%s\nfq=%s\ntype=%s\nblob=%s\nkeyid=%s\nfpr=%s\nmode=%s\nserial=%s\nregistered=%d\nexpires=%d\n",
		id.Name, id.Org, s.ws, fq, ktype, kblob, keyid, fpr, mode, serial, now, exp)
	if err := os.WriteFile(tmp, []byte(body), 0o640); err != nil {
		os.Remove(tmp)
		s.die(1, "registrar: cannot write %s", dest)
	}
	if renew || reclaim {
		if reclaim && oldKeyid != "" {
			os.Remove(filepath.Join(h.byfpr, oldKeyid)) // evict the old key's index
		}
		if reclaim {
			os.Remove(h.describedPath(fq)) // the new holder has not said this
		}
		if err := os.Rename(tmp, dest); err != nil {
			os.Remove(tmp)
			s.die(1, "registrar: cannot write %s", dest)
		}
	} else {
		err := os.Link(tmp, dest) // first writer wins
		os.Remove(tmp)
		if err != nil {
			s.die(1, "claim-conflict: %s held by %s until %s", fq, recGet(dest, "fpr"), isoTimeStr(recGet(dest, "expires")))
		}
	}
	link := filepath.Join(h.byfpr, keyid)
	os.Remove(link)
	os.Symlink(rel, link) // 1:1 key -> claim index (checked above)
	writeSmall(sp, serial+"\n")
	h.journalf("claim %s %s serial=%s mode=%s ttl=%d", fq, fpr, serial, mode, t)
	if reclaim {
		h.follows.nudge(fq) // a held follow of the old generation ends now
	}
	s.printf("registered %s fpr=%s serial=%s mode=%s expires=%s\n", fq, fpr, serial, mode, isoTime(exp))
}

func isoTimeStr(e string) string {
	if v, ok := atoi64(e); ok {
		return isoTime(v)
	}
	return ""
}

// writeSmall replaces a small file whole (through a temp file and a rename).
func writeSmall(p, content string) {
	tmp := fmt.Sprintf("%s.%d.%d", p, os.Getpid(), stageSeq.Add(1))
	if os.WriteFile(tmp, []byte(content), 0o640) == nil {
		if os.Rename(tmp, p) != nil {
			os.Remove(tmp)
		}
	}
}

func (s *Session) regRelease(a []string) {
	h := s.h
	req := ""
	if len(a) > 1 {
		req = a[1]
	}
	fq := s.wsIdent(req)
	dest := h.claimPath(fq, s.ws)
	if !existsFollow(dest) {
		s.die(3, "no such claim: %s", fq)
	}
	unlock, err := h.lockRegistry()
	if err != nil {
		s.die(1, "registrar: registry busy; retry")
	}
	defer unlock()
	keyid := recGet(dest, "keyid")
	writeSmall(s.tombPath(fq), fmt.Sprintf("keyid=%s\nuntil=%d\n", keyid, h.epoch()+TombstoneSecs))
	os.Remove(filepath.Join(h.byfpr, keyid))
	os.Remove(dest)
	os.Remove(h.describedPath(fq))
	h.journalf("release %s (minted by %s)", fq, s.ws)
	h.follows.nudge(fq) // a held follow of it ends now
	s.printf("released %s\n", fq)
}

func (s *Session) regList() {
	h := s.h
	found := false
	dir := filepath.Join(h.claims, s.ws)
	for _, n := range globNames(dir) {
		rp := filepath.Join(dir, n)
		if !isFile(rp) {
			continue
		}
		r, _ := readRec(rp)
		s.printf("%s serial=%s mode=%s expires=%s\n", r["fq"], r["serial"], r["mode"], isoTimeStr(r["expires"]))
		found = true
	}
	if !found {
		s.printf("(no claims for %s)\n", s.ws)
	}
}
