package hub

// help: what the hub supports and what an address handles; register_description: an identity says
// what it handles (docs/help.md).

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cycanics/agsshub/go/descriptor"
)

func (h *Hub) describedPath(id string) string { return filepath.Join(h.described, fsName(id)) }

// loadDescription reads a stored description once, so it is one version; nil when there is none.
func loadDescription(path string) *descriptor.Record {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	r, ok := descriptor.ParseRecord(b)
	if !ok {
		return nil
	}
	return r
}

func (s *Session) seeAllows(see string) bool {
	for _, p := range words(see) {
		if bashMatch(p, s.me) {
			return true
		}
	}
	return false
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func (s *Session) helpHub(asJSON bool, row Role) {
	h := s.h
	name := "help.txt"
	if asJSON {
		name = "help.json"
	}
	b, err := os.ReadFile(filepath.Join(h.cfg.Lib, name))
	if err != nil {
		s.die(1, "dispatch: this hub has no %s (install.sh)", name)
	}
	if asJSON {
		s.printf(`{"caller":"%s","role":"%s","receives":"%s","sends":"%s","hub":%s}`+"\n", s.me, idRole(s.me), row.In, row.Out, strings.TrimRight(string(b), "\n"))
		return
	}
	s.printf("Caller: %s\nRole: %s\nReceives: %s\nSends: %s\n\n%s", s.me, idRole(s.me), row.In, row.Out, b)
}

// doHelp is help [<target>] [--json]. Arguments, then grammar and role (2, nothing read), then a
// publisher's limit, then access: no access, no such target and split brain are one answer (3), with
// resolve's reason kept out of it, so that a caller without access learns nothing.
func (s *Session) doHelp(a []string) {
	h := s.h
	asJSON := false
	t := ""
	for _, w := range a {
		switch {
		case w == "--json":
			asJSON = true
		case strings.HasPrefix(w, "-") || t != "":
			s.die(2, "dispatch: unexpected arg '%s'", w)
		default:
			t = w
		}
	}
	myRow, _ := roleRow(idRole(s.me))
	if t == "" {
		s.helpHub(asJSON, myRow)
		return
	}
	isID := validIdent(t)
	role := ""
	if isID {
		role = idRole(t)
	} else if !validAlias(t) {
		s.die(2, "dispatch: bad target")
	}
	if role == "sub" {
		s.die(2, "dispatch: a subscription has no description: help its topic")
	}
	noAccess := func() { s.die(3, "dispatch: no such target, or no access: %s", t) }
	if myRow.In == "no" && role != "topic" {
		noAccess()
	}

	if !isID {
		af := filepath.Join(h.aliases, t)
		at := ""
		if isFile(af) {
			at = recGet(af, "type")
		}
		known := false
		for _, bt := range backendTypes {
			known = known || at == bt
		}
		if !known {
			noAccess()
		}
		if asJSON {
			s.printf(`{"target":"%s","type":"%s","described":null,"descriptor":null}`+"\n", t, at)
		} else {
			s.printf("Target: %s\nType: %s\nDescribed: no\n\nAn alias for delivery through %s: it has no description.\n", t, at, at)
		}
		return
	}

	addr := t
	parent, _, inst := instSplit(t)
	if inst {
		addr = parent
	}
	pol := filepath.Join(h.topicDir(t), "policy")
	file := h.describedPath(addr)
	if role == "topic" {
		file = filepath.Join(h.topicDir(t), "descriptor")
	}
	rec := loadDescription(file)

	org := idOrg(t)
	dflt := "*@" + org + idSuffix
	ok := org == idOrg(s.me) || (rec != nil && s.seeAllows(rec.See))
	if !ok && role == "topic" && isFile(pol) {
		ok = policyAllows(pol, "publish", s.me, "") || policyAllows(pol, "subscribe", s.me, dflt)
	}
	if !ok {
		noAccess()
	}
	typ, _, _, resolved := h.resolve(t)
	if !resolved || (typ != "native" && typ != "topic") {
		noAccess()
	}

	row, _ := roleRow(role)
	var jf, tf strings.Builder
	fmt.Fprintf(&jf, `"target":"%s","type":"%s","role":"%s","receives":"%s"`, t, typ, role, row.In)
	fmt.Fprintf(&tf, "Target: %s\nType: %s\nRole: %s\nReceives: %s\n", t, typ, role, row.In)
	if typ == "topic" {
		pub, sb := policyAllows(pol, "publish", s.me, ""), myRow.In != "no" && policyAllows(pol, "subscribe", s.me, dflt)
		fmt.Fprintf(&jf, `,"publish":%v,"subscribe":%v`, pub, sb)
		fmt.Fprintf(&tf, "Publish: %s\nSubscribe: %s\n", yesNo(pub), yesNo(sb))
	} else {
		claim := "lapsed"
		if inst {
			f := h.instRecord(t)
			switch {
			case !isFile(f):
				claim = "none"
			case h.claimLive(f):
				claim = "live"
			}
		} else {
			for _, f := range h.claimFind(t) {
				if h.claimLive(f) {
					claim = "live"
				}
			}
		}
		live := h.follows.following(t)
		fmt.Fprintf(&jf, `,"claim":"%s","live":%v`, claim, live)
		fmt.Fprintf(&tf, "Claim: %s\nLive: %s\n", claim, yesNo(live))
		if inst {
			fmt.Fprintf(&jf, `,"address":"%s"`, addr)
			fmt.Fprintf(&tf, "Address: %s\n", addr)
		} else {
			capable := h.instCapable(t)
			fmt.Fprintf(&jf, `,"instances":%v`, capable)
			fmt.Fprintf(&tf, "Instances: %s\n", yesNo(capable))
		}
	}

	var view descriptor.View
	if rec != nil {
		view = rec.Whole
		if inst {
			view = rec.Instance
		}
	}
	if asJSON || view.Default == "json" {
		if rec == nil {
			s.printf(`{%s,"described":null,"descriptor":null}`+"\n", jf.String())
		} else {
			s.printf(`{%s,"described":"%s","descriptor":%s}`+"\n", jf.String(), rec.Date, view.JSON)
		}
		return
	}
	if rec == nil {
		s.printf("%sDescribed: no\n\nNo description registered: this address has not said what requests it handles.\n", tf.String())
		return
	}
	s.printf("%sDescribed: %s\n\n%s", tf.String(), rec.Date, view.Text)
}

// doRegisterDescription is register_description [--see PAT]... < descriptor.json | --clear. The hub
// is the addressee here, so it checks what it is given; the write happens under the registry lock,
// with the claim checked again, so it cannot outlive a hub-mint rm.
func (s *Session) doRegisterDescription(a []string) {
	h := s.h
	p := &args{s: s, a: a}
	clear := false
	var see []string
	for p.more() {
		switch p.peek() {
		case "--clear":
			clear = true
			p.shift()
		case "--see":
			see = append(see, p.value("--see"))
		default:
			p.unexpected()
		}
	}
	if clear && len(see) > 0 {
		s.die(2, "dispatch: --clear takes no --see")
	}
	if parent, _, inst := instSplit(s.me); inst {
		s.die(3, "dispatch: %s is an instance: its address %s registers the description", s.me, parent)
	}
	if !isDir(h.described) {
		s.die(1, "dispatch: this hub keeps no descriptions (install.sh)")
	}
	when := isoTime(h.epoch())
	var data []byte
	if !clear {
		in, err := io.ReadAll(io.LimitReader(s.in, descriptor.MaxBytes+1))
		if err != nil {
			s.die(1, "dispatch: cannot read the description")
		}
		st, err := descriptor.Build(in, descriptor.Options{See: see})
		if err != nil {
			s.die(2, "dispatch: description refused: %v", err)
		}
		data = append([]byte("Date: "+when+"\n"), st.Bytes()...)
	}
	unlock, err := h.lockRegistry()
	if err != nil {
		s.die(1, "dispatch: registry busy; retry")
	}
	defer unlock()
	if !s.claimHeld() {
		s.die(3, "dispatch: %s no longer belongs to this session's key (rebound or released since it logged in); reconnect", s.me)
	}
	dest := h.describedPath(s.me)
	if clear {
		os.Remove(dest)
		h.journalf("description cleared by %s", s.me)
		s.printf("cleared %s\n", s.me)
		return
	}
	tmp := filepath.Join(h.described, fmt.Sprintf(".new.%d.%d", os.Getpid(), stageSeq.Add(1)))
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		os.Remove(tmp)
		s.die(1, "dispatch: cannot write the description")
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		s.die(1, "dispatch: cannot write the description")
	}
	h.journalf("description registered by %s", s.me)
	s.printf("described %s %s\n", s.me, when)
}

// reapDescribed: descriptions with no claim left, and temp files nobody finished. Under the registry
// lock, like the claims it looks at.
func (h *Hub) reapDescribed(now int64) {
	for _, n := range globNames(h.described) {
		if len(h.claimFind(n)) == 0 {
			os.Remove(filepath.Join(h.described, n))
			h.journalf("reap description %s (no claim)", n)
		}
	}
	tmps, _ := filepath.Glob(filepath.Join(h.described, ".new.*"))
	for _, f := range tmps {
		olderThan(f, now, 60, false)
	}
}
