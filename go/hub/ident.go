package hub

// The identity grammar and the role table (docs/identities.md).
//
//	<role>[.<project>][+<instance>][/<ws>]@<org>.ssh
//
// The same splits as lib/common.sh's, for the operator tools (first '@', first '/', first
// '+', first '.'), the same regular expressions, the same messages.

import (
	"bytes"
	"regexp"
	"strings"
)

var (
	roleRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	projRE  = regexp.MustCompile(`^[a-z0-9_-][a-z0-9._-]{0,63}$`) // a repo name: Gitea permits dots; may start with - or _
	instRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	orgRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`) // a Gitea org: dots permitted, '+' is not
	wsRE    = regexp.MustCompile(`^ws-[a-z0-9][a-z0-9-]{0,30}$`)
	fileRE  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`) // a leading - stays refused
	msgidRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	aliasRE = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
	sidRE   = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

const idSuffix = ".ssh" // fixed domain suffix carried by every identity

// Role is one row of the role table.
//
//	Shape:    pinned (carries /<ws>) · wsless (never does)
//	Minter:   workspace (its registrar) · operator (hub-mint) · hub (never claimable)
//	Proj/Inst: req · opt · no — an operator role with Inst "no" may take instances; a required
//	          instance is part of the name
//	In:       what it may receive: yes · no · policy (a topic: from the publishers its policy allows)
//	          · topic (a subscription: only from its topic)
//	Out:      whether it may send: yes · no
type Role struct{ Shape, Minter, Proj, Inst, In, Out string }

// Roles is the role table; ROLES in lib/common.sh is the operator tools' copy. A role not listed here is not an identity.
var Roles = map[string]Role{
	"main":      {"pinned", "operator", "no", "no", "yes", "yes"},
	"run":       {"pinned", "workspace", "opt", "req", "yes", "yes"},
	"foreman":   {"wsless", "operator", "no", "no", "yes", "yes"},
	"frontline": {"wsless", "operator", "no", "no", "yes", "yes"},
	"vmmanager": {"pinned", "operator", "no", "no", "yes", "yes"},
	"inference": {"pinned", "operator", "no", "no", "yes", "yes"},
	"group":     {"wsless", "operator", "req", "no", "yes", "yes"},
	"svc":       {"wsless", "operator", "req", "no", "yes", "yes"}, // a service: a group's capabilities, its own name
	"commhub":   {"wsless", "operator", "no", "no", "yes", "yes"},
	"publisher": {"wsless", "operator", "no", "req", "no", "yes"},
	"topic":     {"wsless", "hub", "opt", "req", "policy", "no"},
	"sub":       {"wsless", "hub", "req", "req", "topic", "no"},
}

// BACKEND_TYPES: the alias types, which `help` prints only from this set.
var backendTypes = []string{"email", "tg"}

// CLIENT_FIELDS: the fields a client may set, in their listing order. Gap is a gap marker's, and
// only a topic takes it (gapProblem).
var clientFields = []string{"reply-to", "re", "subject", "deadline", "gap"}

// subjectRE: --subject, one word.
var subjectRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,63}$`)

func roleRow(r string) (Role, bool) {
	if !roleRE.MatchString(r) {
		return Role{}, false
	}
	row, ok := Roles[r]
	return row, ok
}

func hubRole(r string) bool { row, ok := roleRow(r); return ok && row.Minter == "hub" }

// receivesNothing: id's role receives nothing (In "no" — a publisher), so no send
// reaches it.
func receivesNothing(id string) bool { row, ok := roleRow(idRole(id)); return ok && row.In == "no" }

func slotOK(rule, v string) bool {
	switch rule {
	case "req":
		return v != ""
	case "no":
		return v == ""
	case "opt":
		return true
	}
	return false
}

func slotWord(rule string) string {
	switch rule {
	case "req":
		return "required"
	case "no":
		return "not allowed"
	}
	return "optional"
}

// ID: the parts of an identity, without validation.
type ID struct{ Name, Role, Proj, Inst, WS, Org string }

func before(s, sep string) string {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i]
	}
	return s
}

func after(s, sep string) (string, bool) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[i+len(sep):], true
	}
	return "", false
}

func idSplit(i string) ID {
	var p ID
	l := before(i, "@") // ${1%%@*}
	p.Name = before(l, "/")
	org := i
	if a, ok := after(i, "@"); ok { // ${1#*@}
		org = a
	}
	p.Org = strings.TrimSuffix(org, idSuffix)
	if ws, ok := after(l, "/"); ok {
		p.WS = ws
	}
	h := before(p.Name, "+")
	p.Role = before(h, ".")
	if pr, ok := after(h, "."); ok {
		p.Proj = pr
	}
	if in, ok := after(p.Name, "+"); ok {
		p.Inst = in
	}
	return p
}

func idRole(i string) string { return idSplit(i).Role }
func idOrg(i string) string  { return idSplit(i).Org }
func idWS(i string) string   { return idSplit(i).WS }
func idName(i string) string { return idSplit(i).Name }

func validWS(ws string) bool { return wsRE.MatchString(ws) }

func validOrg(o string) bool { return orgRE.MatchString(o) && !strings.Contains(o, "..") }

// validName: <role>[.<project>][+<instance>], each part against its grammar.
func validName(n string) bool {
	if n == "" {
		return false
	}
	h := before(n, "+")
	role := before(h, ".")
	proj, inst := "", ""
	if strings.HasSuffix(h, ".") {
		return false // a '.' needs a project after it
	}
	if p, ok := after(h, "."); ok {
		proj = p
	}
	if strings.HasSuffix(n, "+") {
		return false // a '+' needs an instance after it
	}
	if in, ok := after(n, "+"); ok {
		inst = in
	}
	if !roleRE.MatchString(role) {
		return false
	}
	if proj != "" && (!projRE.MatchString(proj) || strings.Contains(proj, "..")) {
		return false
	}
	return inst == "" || instRE.MatchString(inst)
}

func validSuffix(i string) bool {
	d := i
	if a, ok := after(i, "@"); ok {
		d = a
	}
	return strings.HasSuffix(d, idSuffix)
}

// identProblem: why an identity is not valid, empty when it is. An operator role's
// identity with +<instance> is an instance form: whether its address takes instances is the
// address's claim's to say (instCapable), not the grammar's.
func identProblem(i string) string {
	if !strings.Contains(i, "@") || !validSuffix(i) {
		return "need <name>@<org>" + idSuffix
	}
	p := idSplit(i)
	if !validOrg(p.Org) {
		return "bad org"
	}
	if !validName(p.Name) || strings.Contains(p.WS, "/") {
		return "bad name"
	}
	row, ok := roleRow(p.Role)
	if !ok {
		return "unknown role " + p.Role
	}
	if !slotOK(row.Proj, p.Proj) {
		return "role " + p.Role + ": project is " + slotWord(row.Proj)
	}
	if !slotOK(row.Inst, p.Inst) && !(row.Inst == "no" && row.Minter == "operator") {
		return "role " + p.Role + ": instance is " + slotWord(row.Inst)
	}
	switch row.Shape {
	case "pinned":
		if p.WS == "" {
			return "role " + p.Role + " is pinned: it carries /<ws>"
		}
		if !validWS(p.WS) {
			return "bad workspace " + p.WS
		}
	case "wsless":
		if p.WS != "" || strings.Contains(before(i, "@"), "/") {
			return "role " + p.Role + " is ws-less: no /<ws>"
		}
	}
	return ""
}

func validIdent(i string) bool { return identProblem(i) == "" }

// fsName: an identity on disk, '/' -> '~' ('~' occurs in no identity part).
func fsName(i string) string { return strings.ReplaceAll(i, "/", "~") }

func validFile(f string) bool    { return fileRE.MatchString(f) && !strings.Contains(f, "..") }
func validMsgid(m string) bool   { return msgidRE.MatchString(m) && !strings.Contains(m, "..") }
func validAlias(a string) bool   { return aliasRE.MatchString(a) }
func validInstID(id string) bool { return instRE.MatchString(id) }

// fieldCanon: the canonical spelling of an allowed client field, or false.
func fieldCanon(name string) (string, bool) {
	want := asciiLower(name)
	for _, f := range clientFields {
		if f != want {
			continue
		}
		parts := strings.Split(f, "-")
		for i, s := range parts {
			if s != "" {
				parts[i] = strings.ToUpper(s[:1]) + s[1:]
			}
		}
		return strings.Join(parts, "-"), true
	}
	return "", false
}

// gapProblem: a gap marker (docs/topics.md, Continuity) is a publication whose one
// client field is Gap: <reason>, the reason on the field's own line (a listing shows only that line).
// It says why a header block (as parseRFC5322 returns it) carrying Gap is refused; empty when it
// carries none, or is a gap marker sent to a topic.
func gapProblem(hdr []byte, typ string) string {
	n, other, reason := 0, "", ""
	for _, line := range strings.Split(string(hdr), "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' { // a folded continuation
			continue
		}
		if name := before(line, ":"); name == "Gap" {
			n++
			reason = strings.TrimPrefix(line, "Gap:")
		} else if other == "" {
			other = name
		}
	}
	switch {
	case n == 0:
		return ""
	case typ != "topic":
		return "Gap is allowed only in a send to a topic"
	case other != "":
		return "a gap marker carries Gap alone, not " + other
	case n > 1:
		return "a gap marker carries one Gap field"
	case strings.Trim(reason, " \t\n\v\f\r") == "":
		return "Gap needs a reason: Gap: <reason>"
	}
	return ""
}

// hasGap: the header block carries a Gap field.
func hasGap(hdr []byte) bool {
	return bytes.HasPrefix(hdr, []byte("Gap:")) || bytes.Contains(hdr, []byte("\nGap:"))
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// --- instances (docs/identities.md, Instances) ------------------------------------------------

// instSplit: an instance form is an operator role's identity with +<instance>, the role having no
// instance slot of its own (a publisher's +<app> is its name). Returns the address it is an instance
// of and the instance id.
func instSplit(i string) (parent, id string, ok bool) {
	p := idSplit(i)
	if p.Inst == "" {
		return "", "", false
	}
	row, rok := roleRow(p.Role)
	if !rok || row.Minter != "operator" || row.Inst != "no" {
		return "", "", false
	}
	parent = before(p.Name, "+")
	if p.WS != "" {
		parent += "/" + p.WS
	}
	return parent + "@" + p.Org + idSuffix, p.Inst, true
}

// instanceOf: <address>+<id> where the grammar puts it (before /<ws> on a pinned address).
func instanceOf(addr, id string) string {
	p := idSplit(addr)
	s := p.Name + "+" + id
	if p.WS != "" {
		s += "/" + p.WS
	}
	return s + "@" + p.Org + idSuffix
}
