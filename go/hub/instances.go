package hub

// Instances (docs/identities.md, Instances): an address minted --instances registers its instances'
// keys, and admits or rejects them as targets (admission.go); its follow carries the wake notice.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

var ttlRE = regexp.MustCompile(`^[0-9]+$`)

// instMine: the instance of s.me that a names (its id, or the whole identity). 2 malformed · 3 not ours.
func (s *Session) instMine(a string) (string, int) {
	if a == "" {
		return "", 2
	}
	if strings.Contains(a, "@") {
		if !validIdent(a) {
			return "", 2
		}
		if parent, _, ok := instSplit(a); !ok || parent != s.me {
			return "", 3
		}
		return a, 0
	}
	if !validInstID(a) {
		return "", 2
	}
	return instanceOf(s.me, a), 0
}

// instAddress: the caller must be an address that takes instances, not one itself.
func (s *Session) instAddress() {
	if parent, _, ok := instSplit(s.me); ok {
		s.die(3, "dispatch: %s is an instance; its address %s manages instances", s.me, parent)
	}
	if !s.h.instCapable(s.me) {
		s.die(3, "dispatch: %s takes no instances (hub-mint add … --instances)", s.me)
	}
}

// fingerprint is ssh-keygen -l's SHA256:… for a key line's type and blob; empty when it is no key.
func fingerprint(ktype, kblob string) string {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(ktype + " " + kblob))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(pub)
}

// doRegister: `register <instance> [--ttl S]` binds <address>+<instance> to the key on stdin. The
// same key renews; another key (a new start or resume) takes over: the previous key's connection is
// refused from its next command and its follow ends.
func (s *Session) doRegister(a []string) {
	h := s.h
	p := &args{s: s, a: a}
	arg := p.shift()
	ttl := strconv.Itoa(InstTTLSecs)
	for p.more() {
		switch p.peek() {
		case "--ttl":
			ttl = p.value("--ttl")
		default:
			p.unexpected()
		}
	}
	s.instAddress()
	fq, rc := s.instMine(arg)
	switch rc {
	case 2:
		s.die(2, "dispatch: register <instance>: an id matching %s", instRE.String())
	case 3:
		s.die(3, "dispatch: %s is not an instance of %s", arg, s.me)
	}
	if !ttlRE.MatchString(ttl) {
		s.die(2, "dispatch: bad --ttl")
	}
	t, err := strconv.ParseInt(ttl, 10, 64)
	if err != nil || t > 86400 {
		t = 86400
	}
	if t < 60 {
		t = 60
	}
	line, _ := s.in.ReadString('\n')
	f := strings.Fields(line)
	ktype, kblob := "", ""
	if len(f) > 0 {
		ktype = f[0]
	}
	if len(f) > 1 {
		kblob = f[1]
	}
	if ktype != "ssh-ed25519" || kblob == "" {
		s.die(2, "dispatch: register needs the instance's ssh-ed25519 public key on stdin")
	}
	idir := filepath.Join(h.claims, InstMinter)
	if !isDir(idir) || !isDir(h.ibyfpr) {
		s.die(1, "dispatch: this hub keeps no instances (install.sh)")
	}
	keyid := keyID(kblob)
	fpr := fingerprint(ktype, kblob)
	if fpr == "" {
		s.die(2, "dispatch: not a valid ssh-ed25519 key")
	}
	now := h.epoch()
	exp := now + t
	dest := h.instRecord(fq)
	rel := "../" + InstMinter + "/" + fsName(fq)
	unlock, err := h.lockRegistry()
	if err != nil {
		s.die(1, "dispatch: registry busy; retry")
	}
	defer unlock()
	if other, bound := h.keyBoundElsewhere(keyid, rel); bound {
		s.die(1, "dispatch: key already bound to %s", other)
	}
	pser := recGet(h.claimPath(s.me, OpMinter), "serial")
	var serial, old string
	cur, have := readRec(dest)
	if have && isFile(dest) && cur["keyid"] == keyid && cur["parent_serial"] == pser {
		serial = cur["serial"] // the same key: renewed
	} else {
		if have && isFile(dest) {
			old = cur["keyid"]
		}
		// one counter for every instance: a stale session never matches a new generation
		seqf := filepath.Join(idir, ".seq")
		n, _ := atoi64(strings.TrimSpace(readString(seqf)))
		serial = strconv.FormatInt(n+1, 10)
		tmpSeq := fmt.Sprintf("%s.%d.%d", seqf, os.Getpid(), stageSeq.Add(1))
		if os.WriteFile(tmpSeq, []byte(serial+"\n"), 0o640) == nil {
			os.Rename(tmpSeq, seqf)
		}
	}
	id := idSplit(fq)
	recBody := fmt.Sprintf("name=%s\norg=%s\nws=%s\nfq=%s\ntype=%s\nblob=%s\nkeyid=%s\nfpr=%s\nmode=%s\nserial=%s\nregistered=%d\nexpires=%d\nparent=%s\nparent_serial=%s\n",
		id.Name, id.Org, InstMinter, fq, ktype, kblob, keyid, fpr, "session", serial, now, exp, s.me, pser)
	tmp := filepath.Join(idir, fmt.Sprintf(".new.%d.%d", os.Getpid(), stageSeq.Add(1)))
	if err := os.WriteFile(tmp, []byte(recBody), 0o640); err != nil {
		os.Remove(tmp)
		s.die(1, "dispatch: cannot write the registry")
	}
	os.Chmod(tmp, 0o640)
	if old != "" && old != keyid { // the previous key is out first
		os.Remove(filepath.Join(h.ibyfpr, old))
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		s.die(1, "dispatch: cannot write the registry")
	}
	link := filepath.Join(h.ibyfpr, keyid)
	os.Remove(link)
	os.Symlink(rel, link)
	h.journalf("instance %s %s serial=%s ttl=%d by %s", fq, fpr, serial, t, s.me)
	// register answers a pending ask as admit; registry lock, then the admission lock
	unlockAdm := h.admMu.lock(fq)
	h.asks.mu.Lock()
	pending := h.asks.m[fq] != nil
	h.asks.mu.Unlock()
	if pending && h.instBox(fq) {
		h.askEnd(fq, nil, 0)
	}
	unlockAdm()
	h.nudgeAddr(fq)
	if old != "" {
		h.follows.nudge(fq) // the previous run's follow ends now
	}
	s.printf("registered %s fpr=%s serial=%s expires=%s\n", fq, fpr, serial, isoTime(exp))
}

func readString(p string) string { b, _ := os.ReadFile(p); return string(b) }

// doRelease: the address drops one of its instances; an instance drops itself. The mailbox stays.
func (s *Session) doRelease(a []string) {
	h := s.h
	if len(a) > 1 {
		s.die(2, "dispatch: unexpected arg '%s'", a[1])
	}
	arg := ""
	if len(a) == 1 {
		arg = a[0]
	}
	var fq string
	if _, id, ok := instSplit(s.me); ok {
		if arg != "" && arg != s.me && arg != id {
			s.die(3, "dispatch: an instance releases only itself")
		}
		fq = s.me
	} else {
		s.instAddress()
		var rc int
		fq, rc = s.instMine(arg)
		switch rc {
		case 2:
			s.die(2, "dispatch: release <instance>")
		case 3:
			s.die(3, "dispatch: %s is not an instance of %s", arg, s.me)
		}
	}
	rp := h.instRecord(fq)
	unlock, err := h.lockRegistry()
	if err != nil {
		s.die(1, "dispatch: registry busy; retry")
	}
	defer unlock()
	if !isFile(rp) {
		s.die(3, "dispatch: %s holds no claim", fq)
	}
	h.instDrop(rp)
	h.journalf("release %s by %s", fq, s.me)
	s.printf("released %s\n", fq)
}

// doInstances: every instance of s.me with a claim, a follower or a mailbox.
func (s *Session) doInstances(a []string) {
	h := s.h
	if len(a) > 0 {
		s.die(2, "dispatch: unexpected arg '%s'", a[0])
	}
	s.instAddress()
	seen := map[string]bool{}
	idir := filepath.Join(h.claims, InstMinter)
	for _, n := range globNames(idir) {
		rp := filepath.Join(idir, n)
		if r, ok := readRec(rp); ok && isFile(rp) && r["parent"] == s.me {
			seen[r["fq"]] = true
		}
	}
	id := idSplit(s.me)
	head, tail := fsName(id.Name)+"+", fsName(strings.TrimPrefix(s.me, id.Name))
	for _, n := range globNames(h.inbox) {
		if !strings.HasPrefix(n, head) || !strings.HasSuffix(n, tail) || len(n) < len(head)+len(tail) {
			continue
		}
		fq := strings.ReplaceAll(n, "~", "/")
		if parent, _, ok := instSplit(fq); ok && parent == s.me {
			seen[fq] = true
		}
	}
	list := make([]string, 0, len(seen))
	for fq := range seen {
		list = append(list, fq)
	}
	sort.Strings(list)
	any := false
	for _, fq := range list {
		rp := h.instRecord(fq)
		mail := len(msgFiles(h.mbox(fq)))
		holder := "none"
		if h.follows.following(fq) {
			holder = "live"
		}
		claim := "none"
		if isFile(rp) {
			claim = "lapsed"
			if h.claimLive(rp) {
				claim = "live"
			}
		}
		s.printf("%s\tmail=%d\tholder=%s\tclaim=%s\n", fq, mail, holder, claim)
		any = true
	}
	if !any {
		s.println("(no instances)")
	}
}
