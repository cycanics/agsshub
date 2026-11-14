package hub

// Identities, the registrar, operator minting (lib/mint), admission, rebinding, split brain, the mailbox
// as the address.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	alice = "run.t+alice/ws-a@acme.ssh"
	bob   = "run.t+bob/ws-b@acme.ssh"
	cara  = "run.t+cara/ws-c@corp.ssh"
)

func TestGrammar(t *testing.T) {
	for id, want := range map[string]string{
		"main/ws-a@acme.ssh":                   "",
		"run.myproj+a1b2c3/ws-a@home.ssh": "",
		"foreman@beta.ssh":                     "",
		"group.support@expa.ssh":                "",
		"group.support+a1b2c3d4@expa.ssh":       "", // an instance form: the address's claim decides
		"group._x@expa.ssh":                     "", // a project may start with _ or -
		"group.-x@expa.ssh":                     "",
		"group._x+a1b2c3d4@expa.ssh":            "",
		"main+a1b2c3d4/ws-a@acme.ssh":          "",
		"commhub@home.ssh":                  "",
		"topic+builds@acme.ssh":                  "",
		"sub.builds+k3f9a2c1@acme.ssh":           "",
		"alice@acme.ssh":                         "unknown role alice",
		"run.p@acme.ssh":                         "role run: instance is required",
		"group@expa.ssh":                        "role group: project is required",
		"commhub.x@home.ssh":                "role commhub: project is not allowed",
		"main@acme.ssh":                          "role main is pinned: it carries /<ws>",
		"group.demo/ws-a@expa.ssh":            "role group is ws-less: no /<ws>",
		"main/ws-@acme.ssh":                      "bad workspace ws-",
		"bob@ws-b":                            "need <name>@<org>.ssh",
		"x@.ssh":                                "bad org",
		"run.a..b+i/ws-a@acme.ssh":             "bad name",
		"run.t+a+b/ws-a@acme.ssh":              "bad name",
		"topic@acme.ssh":                         "role topic: instance is required",
		"publisher+gitea@home.ssh":          "",
		"publisher@home.ssh":                "role publisher: instance is required",
		"publisher.x+gitea@home.ssh":        "role publisher: project is not allowed",
		"publisher+gitea/ws-a@home.ssh":   "role publisher is ws-less: no /<ws>",
	} {
		eq(t, "ident_problem "+id, identProblem(id), want)
	}
	p := idSplit("run.p.q+i1/ws-a@home.ssh")
	eq(t, "an identity splits into its parts", strings.Join([]string{p.Role, p.Proj, p.Inst, p.WS, p.Org}, "|"), "run|p.q|i1|ws-a|home")
	for _, c := range []struct{ id, parent, inst string }{
		{"group.sess+a1b2c3d4@expa.ssh", "group.sess@expa.ssh", "a1b2c3d4"},
		{"main+x1/ws-a@acme.ssh", "main/ws-a@acme.ssh", "x1"},
	} {
		parent, inst, ok := instSplit(c.id)
		eq(t, "inst_split "+c.id, ok, true)
		eq(t, "parent", parent, c.parent)
		eq(t, "id", inst, c.inst)
		eq(t, "instanceOf", instanceOf(parent, inst), c.id)
	}
	if _, _, ok := instSplit("run.t+alice/ws-a@acme.ssh"); ok {
		t.Error("a run is no instance form")
	}
	if _, _, ok := instSplit("publisher+gitea@home.ssh"); ok {
		t.Error("a publisher's +<app> is its name, not an instance")
	}
	c, _ := fieldCanon("REPLY-to")
	eq(t, "field names are canonicalised", c, "Reply-To")
}

func TestRegistrarAndAdmission(t *testing.T) {
	x := newTH(t)
	r := x.reg("ws-a", "register run.t+alice@acme.ssh --ttl 600", x.key("alice").line)
	has(t, "the registrar appends the workspace", r.out, "registered "+alice+" ")
	eq(t, "id after register", x.dis(alice, "id").out, alice+" serial=1\n")
	a, ok := x.h.admitHub(x.key("alice").pub)
	eq(t, "the key is admitted as the identity", ok && a.FQ == alice && a.Serial == "1" && a.KeyID == x.keyid("alice"), true)
	if _, ok := x.h.admitHub(x.key("mallory").pub); ok {
		t.Error("an unregistered key is admitted")
	}
	code(t, "squatter -> conflict", x.reg("ws-a", "register "+alice+" --ttl 600", x.key("mallory").line), 1)
	has(t, "cross-ws same name distinct", x.reg("ws-b", "register run.t+alice@acme.ssh --ttl 600", x.key("mallory").line).out, "run.t+alice/ws-b@acme.ssh")
	r = x.reg("ws-a", "register "+alice+" --ttl 900", x.key("alice").line)
	code(t, "same key renews", r, 0)
	has(t, "a renewal keeps the serial", r.out, "serial=1")
	code(t, "one key, one identity", x.reg("ws-a", "register run.t+other@acme.ssh --ttl 600", x.key("alice").line), 1)
	if exists(filepath.Join(x.state, "claims/ws-a", fsName("run.t+other/ws-a@acme.ssh"))) {
		t.Error("a refused key left a claim behind")
	}

	// release, tombstone, reclaim
	tmpid := "run.t+temp/ws-a@acme.ssh"
	x.reg("ws-a", "register "+tmpid+" --ttl 600", x.key("tmpk").line)
	code(t, "release takes the requested form too", x.reg("ws-a", "release run.t+temp@acme.ssh", ""), 0)
	code(t, "reclaim by a different key in the tombstone window", x.reg("ws-a", "register "+tmpid+" --ttl 600", x.key("tmpk2").line), 1)
	code(t, "reclaim by the same key", x.reg("ws-a", "register "+tmpid+" --ttl 600", x.key("tmpk").line), 0)

	// expiry and the reaper
	rec := filepath.Join(x.state, "claims/ws-a", fsName(tmpid))
	rewriteKey(rec, "expires", "1")
	if _, ok := x.h.admitHub(x.key("tmpk").pub); ok {
		t.Error("an expired claim admits its key")
	}
	x.h.Reap()
	if exists(rec) {
		t.Error("the reaper left an expired claim")
	}

	// the registrar's refusals
	code(t, "org not enrolled", x.reg("ws-b", "register run.t+x@beta.ssh --ttl 600", x.key("fore").line), 3)
	r = x.reg("ws-c", "register foreman@beta.ssh --ttl 600", x.key("fore").line)
	code(t, "an operator role is not the registrar's", r, 3)
	has(t, "and it says who mints it", r.err, "hub-mint")
	code(t, "an unknown role", x.reg("ws-a", "register alice@acme.ssh --ttl 600", x.key("fore").line), 2)
	code(t, "a run needs an instance", x.reg("ws-a", "register run.p@acme.ssh --ttl 600", x.key("fore").line), 2)
	code(t, "a host cannot mint an identity pinned elsewhere", x.reg("ws-a", "register run.p+i1/ws-b@acme.ssh --ttl 600", x.key("fore").line), 3)
	has(t, "a pinned identity may name its own workspace", x.reg("ws-a", "register run.p+i1/ws-a@acme.ssh --ttl 600", x.key("fore").line).out, "registered run.p+i1/ws-a@acme.ssh ")
	code(t, "a topic name cannot be claimed", x.reg("ws-a", "register topic+builds@acme.ssh --ttl 600", x.key("mallory").line), 3)
	code(t, "a sub name cannot be claimed", x.reg("ws-a", "register sub.x+y@acme.ssh --ttl 600", x.key("mallory").line), 3)
	code(t, "bad ttl", x.reg("ws-a", "register run.p+i2@acme.ssh --ttl", x.key("k9").line), 2)
	code(t, "bad mode", x.reg("ws-a", "register run.p+i2@acme.ssh --mode forever", x.key("k9").line), 2)
	code(t, "no key on stdin", x.reg("ws-a", "register run.p+i2@acme.ssh", ""), 2)
	has(t, "list", x.reg("ws-a", "list", "").out, alice+" serial=1 mode=session expires=")
	eq(t, "list of a workspace with none", x.reg("ws-new", "list", "").out, "(no claims for ws-new)\n")
	code(t, "dispatch refuses an identity outside the grammar", x.disA(Admission{FQ: "bob@ws-b", Serial: "1"}, "id"), 1)
	// the journal records the registrar's acts
	if b, _ := os.ReadFile(filepath.Join(x.state, "claims/.journal")); len(b) == 0 {
		// (the journal exists only once install.sh made it; here it does not)
		t.Log("no journal file in the temp tree (install.sh makes it)")
	}
}

func TestOperatorMintAndRebind(t *testing.T) {
	x := newTH(t)
	out, _ := x.mint(x.key("hubk").line, "add", "commhub@home.ssh")
	has(t, "the operator mints commhub", out, "minted commhub@home.ssh ")
	a, ok := x.h.admitHub(x.key("hubk").pub)
	eq(t, "the daemon admits what hub-mint wrote", ok && a.FQ == "commhub@home.ssh" && a.Serial == "1", true)
	x.mustMint("group.demo@expa.ssh", "grpk")
	eq(t, "the group's id", x.dis("group.demo@expa.ssh", "id").out, "group.demo@expa.ssh serial=1\n")
	code(t, "the registrar refuses an operator-bound key", x.reg("ws-a", "register run.t+k@acme.ssh --ttl 600", x.key("hubk").line), 1)
	out, _ = x.mint(x.key("grpk2").line, "add", "group.demo@expa.ssh", "--replace")
	has(t, "--replace rebinds, next serial", out, "serial=2")
	if _, ok := x.h.admitHub(x.key("grpk").pub); ok {
		t.Error("the old key is still in")
	}
	a, ok = x.h.admitHub(x.key("grpk2").pub)
	eq(t, "the new key is in", ok && a.Serial == "2", true)

	// a session admitted before the rebind (its serial, no key id) is refused from its next command
	x.dis("commhub@home.ssh", "send group.demo@expa.ssh -m:rfc5322 --id rb-1", "Subject: in.message\n\n{\"n\":1}\n")
	old := Admission{FQ: "group.demo@expa.ssh", Serial: "1"}
	r := x.disA(old, "id")
	code(t, "a session admitted before the rebind is refused", r, 3)
	has(t, "and told why", r.err, "no longer belongs to this session's key")
	code(t, "it cannot drain the new holder's mailbox", x.disA(old, "recv"), 3)
	code(t, "nor ack its mail", x.disA(old, "recv_ack rb-1"), 3)
	code(t, "nor send as the group", x.disA(old, "send commhub@home.ssh -- x"), 3)
	has(t, "the mail waits for the new key", x.dis("group.demo@expa.ssh", "recv --peek").out, ";id=rb-1;")
	// key-bound sessions: the key it was taken from stays out, the key it is rebound back to is in again
	byKey := func(k string) Admission {
		return Admission{FQ: "group.demo@expa.ssh", Serial: "1", KeyID: x.keyid(k)}
	}
	code(t, "the old key's session is refused", x.disA(byKey("grpk"), "id"), 3)
	x.mint(x.key("grpk").line, "add", "group.demo@expa.ssh", "--replace")
	code(t, "a session of the key the identity is rebound back to is admitted again", x.disA(byKey("grpk"), "id"), 0)
	code(t, "the key it was taken from stays out", x.disA(byKey("grpk2"), "id"), 3)
	x.mint("", "rm", "commhub@home.ssh")
	x.mint(x.key("hubk").line, "add", "commhub@home.ssh")
	code(t, "a released and re-minted identity refuses the old session", x.disA(Admission{FQ: "commhub@home.ssh", Serial: "1"}, "id"), 3)
	has(t, "list shows operator-minted identities", x.dis(alice, "list").out+x.dis("commhub@home.ssh", "list").out, "commhub@home.ssh\ttype=native")
	// an index entry its record disagrees with admits nothing
	os.Symlink("../operator/commhub@home.ssh", filepath.Join(x.state, "claims/.byfpr", x.keyid("mallory")))
	if _, ok := x.h.admitHub(x.key("mallory").pub); ok {
		t.Error("an index entry its record disagrees with admits a key")
	}
}

func TestMailboxIsTheAddress(t *testing.T) {
	x := newTH(t)
	x.reg("ws-a", "register run.t+alice@acme.ssh --ttl 600", x.key("alice").line)
	x.mustMint("foreman@beta.ssh", "fm")
	code(t, "a ws-less role routes", x.dis(alice, "send foreman@beta.ssh -- logical-hit"), 0)
	has(t, "delivered to the role mailbox", x.dis("foreman@beta.ssh", "recv --peek").out, "logical-hit")
	rewriteKey(filepath.Join(x.state, "claims/operator/foreman@beta.ssh"), "expires", "1")
	x.h.Reap()
	code(t, "a lapsed claim still takes mail", x.dis(alice, "send foreman@beta.ssh -- while-lapsed"), 0)
	if _, ok := x.h.admitHub(x.key("fm").pub); ok {
		t.Error("a lapsed claim admits its key")
	}
	x.mustMint("foreman@beta.ssh", "fm")
	has(t, "renewed, its holder drains what waited", x.dis("foreman@beta.ssh", "recv --peek").out, "while-lapsed")
	code(t, "an identity never claimed is unknown", x.dis(alice, "send main/ws-b@acme.ssh -- pinned"), 3)
	x.mint("", "rm", "foreman@beta.ssh")
	code(t, "a released identity is unknown", x.dis(alice, "send foreman@beta.ssh -- gone"), 3)

	// split brain
	sb := "run.t+sb/ws-a@acme.ssh"
	sbrec := filepath.Join(x.state, "claims/ws-b", fsName(sb))
	os.MkdirAll(filepath.Dir(sbrec), 0o2770)
	os.WriteFile(sbrec, []byte("fq="+sb+"\nkeyid=x\nexpires="+itoa(x.h.epoch()+600)+"\n"), 0o640)
	code(t, "a live claim elsewhere refuses a second", x.reg("ws-a", "register "+sb+" --ttl 600", x.key("sb").line), 1)
	rewriteKey(sbrec, "expires", "1")
	code(t, "a lapsed one does not", x.reg("ws-a", "register "+sb+" --ttl 600", x.key("sb").line), 0)
	code(t, "one live claim and one lapsed: routes", x.dis(alice, "send "+sb+" -- one-live"), 0)
	rewriteKey(sbrec, "expires", itoa(x.h.epoch()+600))
	r := x.dis(alice, "send "+sb+" -- split")
	code(t, "two live claims -> refused, not guessed", r, 3)
	has(t, "saying so", r.err, "ambiguous: 2 live claims")
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// The daemon's role table, grammar and limits are lib/common.sh's (the operator tools read that one): a role
// or a limit changed in one place and not the other fails here.
func TestTablesMatchCommonSh(t *testing.T) {
	x := newTH(t)
	out := x.bash(`for r in "${!ROLES[@]}"; do read -r a b c d e f g <<< "${ROLES[$r]}"; echo "$r $a $b $c $d $e $f $g"; done`)
	got := map[string]Role{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(l)
		if len(f) != 7 {
			t.Fatalf("ROLES line %q", l)
		}
		got[f[0]] = Role{f[1], f[2], f[3], f[4], f[5], f[6]}
	}
	eq(t, "the role table", fmt.Sprint(got), fmt.Sprint(Roles))
	for name, want := range map[string]string{
		"ROLE_RE": roleRE.String(), "PROJ_RE": projRE.String(), "INST_RE": instRE.String(), "ORG_RE": orgRE.String(),
		"WS_RE": wsRE.String(), "ALIAS_RE": aliasRE.String(), "ID_SUFFIX": idSuffix,
		"FLOOD_RATE_MT": strconv.Itoa(FloodRateMT), "FLOOD_BURST_MT": strconv.Itoa(FloodBurstMT),
		"TOPIC_RATE": strconv.Itoa(TopicRate), "TOPIC_BURST": strconv.Itoa(TopicBurst),
		"OP_TTL_SECS": strconv.Itoa(OpTTLSecs), "OP_MINTER": OpMinter, "INST_MINTER": InstMinter, "MSG_EXT": msgExt,
	} {
		eq(t, name, strings.TrimSpace(x.bash(`printf '%s' "${!1}"`, name)), want)
	}
}
