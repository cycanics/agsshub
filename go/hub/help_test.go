package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"agsshub/descriptor"
)

const (
	descMixed = `{
  "descriptor": 1, "version": "0.5", "summary": "Runs sessions.",
  "requests": [
    {"subject":"cmd.create","form":"structured","delivery":["live"],"summary":"Start one.","scope":"address","schema":{"type":"object"},
     "reply":{"content_type":"application/json","statuses":[200,404,500]}},
    {"subject":"in.message","form":"structured","delivery":["spool"],"summary":"A message.","scope":"both","schema":{}},
    {"subject":null,"form":"text","delivery":["spool"],"summary":"Anything.","scope":"instance","guide":"Say what you need.\nAnd by when."}
  ]
}`
	descProg = `{"descriptor":1,"summary":"Only for programs.","requests":[{"subject":"a.b","form":"structured","delivery":["live"],"summary":"Do it.","schema":{}}]}`
	descText = `{"descriptor":1,"summary":"For people.","requests":[{"subject":null,"form":"text","delivery":["spool"],"summary":"Anything.","guide":"Say it."}]}`
	descNone = `{"descriptor":1,"summary":"Takes nothing.","requests":[]}`

	hSvc    = "svc.one@acme.ssh"
	hGroup  = "group.s@acme.ssh"
	hInst   = "group.s+abc@acme.ssh"
	hOut    = "svc.out@corp.ssh"
	hTopic  = "topic+ev@acme.ssh"
	hMissed = "svc.nobody@acme.ssh"
	noDesc  = "No description registered: this address has not said what requests it handles.\n"
)

func helpHub(t *testing.T) *th {
	x := newTH(t)
	x.mustMint(hSvc, "one")
	x.mustMint(hGroup, "grp", "--instances")
	x.mustMint(hOut, "out")
	return x
}

func (x *th) describe(id, stdin string, flags ...string) {
	x.t.Helper()
	r := x.dis(id, strings.TrimSpace("register_description "+strings.Join(flags, " ")), stdin)
	if r.code != 0 {
		x.t.Fatalf("register_description %s: %d %s", id, r.code, r.err)
	}
}

var isoRE = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ`)

// strict: exactly one JSON object and a newline, with no line inside it.
func strict(t *testing.T, what, s string) map[string]any {
	t.Helper()
	if !strings.HasSuffix(s, "\n") || strings.Count(s, "\n") != 1 {
		t.Errorf("%s: not one line: %q", what, s)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("%s: %v: %s", what, err, s)
	}
	return m
}

func TestHelpFormats(t *testing.T) {
	x := helpHub(t)
	r := x.dis(hOut, "help "+hOut)
	eq(t, "no description: the text says so", r.out, "Target: "+hOut+"\nType: native\nRole: svc\nReceives: yes\nClaim: live\nLive: no\nInstances: no\nDescribed: no\n\n"+noDesc)
	eq(t, "and the exit", r.code, 0)
	r = x.dis(hOut, "help "+hOut+" --json")
	eq(t, "no description, JSON", r.out, `{"target":"`+hOut+`","type":"native","role":"svc","receives":"yes","claim":"live","live":false,"instances":false,"described":null,"descriptor":null}`+"\n")
	strict(t, "no description", r.out)
	eq(t, "--json first works too", x.dis(hOut, "help --json "+hOut).out, r.out)

	x.describe(hSvc, descProg)
	r = x.dis(hSvc, "help "+hSvc)
	m := strict(t, "a programmatic service answers JSON by default", r.out)
	eq(t, "with the whole description", m["descriptor"].(map[string]any)["summary"], "Only for programs.")
	eq(t, "dated", isoRE.MatchString(m["described"].(string)), true)
	eq(t, "and --json is the same", x.dis(hSvc, "help "+hSvc+" --json").out, r.out)

	x.describe(hSvc, descText)
	r = x.dis(hSvc, "help "+hSvc)
	has(t, "a text service answers text", r.out, "\n\nFor people.\n\n(no subject)\ttext\tspool\tboth\t-\tAnything.\n\n(no subject):\n  Say it.\n")
	eq(t, "with a date", isoRE.MatchString(r.out), true)
	strict(t, "and JSON on request", x.dis(hSvc, "help "+hSvc+" --json").out)

	x.describe(hSvc, descNone)
	has(t, "no requests are said so", x.dis(hSvc, "help "+hSvc).out, "\n\nTakes nothing.\n\nTakes no requests.\n")
	m = strict(t, "no requests", x.dis(hSvc, "help "+hSvc+" --json").out)
	eq(t, "is an empty list in JSON", fmt.Sprint(m["descriptor"].(map[string]any)["requests"]), "[]")

	x.describe(hSvc, "", "--clear")
	has(t, "--clear goes back to none", x.dis(hSvc, "help "+hSvc).out, noDesc)
	eq(t, "--clear with nothing there is fine", x.dis(hSvc, "register_description --clear").code, 0)

	r = x.dis(hSvc, "help")
	has(t, "the hub's own answer", r.out, "Caller: "+hSvc+"\nRole: svc\nReceives: yes\nSends: yes\n\nchat <peer> [--chunked]\n")
	m = strict(t, "the hub's own JSON", x.dis(hSvc, "help --json").out)
	eq(t, "names the caller", m["caller"], hSvc)

	os.WriteFile(filepath.Join(x.state, "aliases", "carol"), []byte("type=email\naddr=c@example.com\n"), 0o640)
	eq(t, "an alias, text", x.dis(hSvc, "help carol").out, "Target: carol\nType: email\nDescribed: no\n\nAn alias for delivery through email: it has no description.\n")
	eq(t, "an alias, JSON", x.dis(hSvc, "help carol --json").out, `{"target":"carol","type":"email","described":null,"descriptor":null}`+"\n")
	os.WriteFile(filepath.Join(x.state, "aliases", "odd"), []byte("type=\"x\naddr=y\n"), 0o640)
	eq(t, "an alias of a type the hub does not know is none", x.dis(hSvc, "help odd --json").code, 3)
}

func TestHelpLive(t *testing.T) {
	x := helpHub(t)
	a := Admission{FQ: hSvc, Serial: x.serial(hSvc)}
	b := x.start(a, "recv --follow", "")
	for i := 0; i < 200 && !x.h.follows.following(hSvc); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	eq(t, "a followed mailbox is live", strict(t, "live", x.dis(hOut, "help --json").out)["caller"], hOut)
	eq(t, "live", strict(t, "live", x.dis(hSvc, "help "+hSvc+" --json").out)["live"], true)
	has(t, "in text too", x.dis(hSvc, "help "+hSvc).out, "Live: yes\n")
	b.cancel()
}

func TestHelpAccess(t *testing.T) {
	x := helpHub(t)
	x.describe(hSvc, descProg)
	x.mustMint("svc.bare@acme.ssh", "bare")

	refusal := func(me, target string) string {
		r := x.dis(me, "help "+target+" --json")
		return fmt.Sprint(r.code, "|", r.out, "|", strings.ReplaceAll(r.err, target, "T"))
	}
	want := "3||dispatch: no such target, or no access: T\n"
	eq(t, "another org, a described target", refusal(hOut, hSvc), want)
	eq(t, "another org, an undescribed target", refusal(hOut, "svc.bare@acme.ssh"), want)
	eq(t, "another org, no such target", refusal(hOut, hMissed), want)
	eq(t, "own org, no such target", refusal(hSvc, hMissed), want)
	eq(t, "an instance of an address that takes none", refusal(hSvc, "svc.bare+x1@acme.ssh"), want)
	eq(t, "no such topic", refusal(hSvc, "topic+no@acme.ssh"), want)
	eq(t, "no such alias", refusal(hSvc, "nobody"), want)

	dup := filepath.Join(x.state, "claims", "ws-a")
	os.MkdirAll(dup, 0o2770)
	src, _ := os.ReadFile(filepath.Join(x.state, "claims", "operator", "svc.bare@acme.ssh"))
	os.WriteFile(filepath.Join(dup, "svc.bare@acme.ssh"), src, 0o640)
	eq(t, "split brain, for its own org too", refusal(hSvc, "svc.bare@acme.ssh"), want)

	eq(t, "bad grammar is usage", x.dis(hOut, "help Bad@Target").code, 2)
	eq(t, "an unknown role is usage", x.dis(hOut, "help nosuch.x@acme.ssh").code, 2)
	eq(t, "a subscription, existing or not, is usage", x.dis(hOut, "help sub.x+y@acme.ssh").code, 2)
	eq(t, "two targets are usage", x.dis(hOut, "help a b").code, 2)
	eq(t, "an unknown flag is usage", x.dis(hOut, "help --nope").code, 2)

	x.describe(hSvc, descProg, "--see", "*@corp.ssh", "--see", "svc.x@other.ssh")
	eq(t, "--see lets another org read", strict(t, "see", x.dis(hOut, "help "+hSvc).out)["target"], hSvc)
	x.describe(hSvc, descProg, "--see", "nobody@else.ssh")
	eq(t, "and is gone with a new registration", refusal(hOut, hSvc), want)
	os.Remove(filepath.Join(dup, "svc.bare@acme.ssh"))
	eq(t, "--see never narrows the identity's own org", x.dis("svc.bare@acme.ssh", "help "+hSvc+" --json").code, 0)
	x.describe(hSvc, descProg, "--see", "*@corp.ssh")
	x.describe(hSvc, "", "--clear")
	eq(t, "--clear ends a grant", refusal(hOut, hSvc), want)
}

func TestHelpPublisher(t *testing.T) {
	x := helpHub(t)
	pub := "publisher+app@acme.ssh"
	x.mustMint(pub, "pub")
	x.mkTopic(hTopic, pub)
	eq(t, "a publisher asks the hub", strict(t, "pub", x.dis(pub, "help --json").out)["receives"], "no")
	m := strict(t, "pub", x.dis(pub, "help "+hTopic+" --json").out)
	eq(t, "and about a topic", m["publish"], true)
	eq(t, "which it can never subscribe to", m["subscribe"], false)
	r := x.dis(pub, "help "+hSvc)
	eq(t, "not about an identity", fmt.Sprint(r.code, r.err), "3dispatch: no such target, or no access: "+hSvc+"\n")
	eq(t, "nor an alias", x.dis(pub, "help carol").code, 3)
	eq(t, "bad grammar is usage first", x.dis(pub, "help Bad@Target").code, 2)
	eq(t, "and a subscription", x.dis(pub, "help sub.x+y@acme.ssh").code, 2)
	r = x.dis(pub, "register_description", descNone)
	has(t, "it registers no description", fmt.Sprint(r.code, r.err), "3dispatch: "+pub+" is a publisher: it receives nothing, so register_description is not for it")
}

func TestHelpInstanceView(t *testing.T) {
	x := helpHub(t)
	x.describe(hGroup, descMixed)
	whole := x.dis(hGroup, "help "+hGroup)
	has(t, "the address shows every request, with its scope", whole.out, "cmd.create\tstructured\tlive\taddress\t-\tStart one.\n")
	has(t, "and its version", whole.out, "Runs sessions.\nVersion: 0.5\n\n")
	eq(t, "and takes instances", strings.Contains(whole.out, "Instances: yes\n"), true)

	r := x.dis(hGroup, "help "+hInst)
	eq(t, "an instance's text", r.out, "Target: "+hInst+"\nType: native\nRole: group\nReceives: yes\nClaim: none\nLive: no\nAddress: "+hGroup+"\nDescribed: "+isoRE.FindString(r.out)+"\n\n"+
		"Runs sessions.\nVersion: 0.5\n\nin.message\tstructured\tspool\tboth\t-\tA message.\n(no subject)\ttext\tspool\tinstance\t-\tAnything.\n\n(no subject):\n  Say what you need.\n  And by when.\n")
	if !strings.Contains(r.out, "Claim: none") || strings.Contains(r.out, "cmd.create") {
		t.Errorf("an address request reached an instance: %s", r.out)
	}
	eq(t, "help wakes nothing", x.h.follows.following(hInst), false)
	m := strict(t, "instance JSON", x.dis(hGroup, "help "+hInst+" --json").out)
	eq(t, "names its address", m["address"], hGroup)
	eq(t, "and has no instances key", m["instances"], nil)
	eq(t, "two requests", len(m["descriptor"].(map[string]any)["requests"].([]any)), 2)

	x.describe(hGroup, `{"descriptor":1,"summary":"S","requests":[{"subject":"x","form":"structured","delivery":["live"],"summary":"X","scope":"instance","schema":{}},{"subject":"y","form":"text","delivery":["spool"],"summary":"Y","scope":"address","guide":"g"}]}`)
	has(t, "each view has its own default: the address's is text", x.dis(hGroup, "help "+hGroup).out, "Instances: yes")
	strict(t, "and the instance's is JSON", x.dis(hGroup, "help "+hInst).out)

	x.reg2Instance(t)
	r = x.dis(hInst, "register_description", descNone)
	has(t, "an instance registers none", fmt.Sprint(r.code, r.err), "3dispatch: "+hInst+" is an instance")
	eq(t, "and reads its own view", strict(t, "own", x.dis(hInst, "help "+hInst).out)["claim"], "live")
}

func (x *th) reg2Instance(t *testing.T) {
	t.Helper()
	r := x.dis(hGroup, "register abc --ttl 600", x.key("inst").line)
	if r.code != 0 {
		t.Fatalf("register: %d %s", r.code, r.err)
	}
}

func TestDescriptionRefused(t *testing.T) {
	x := helpHub(t)
	x.describe(hSvc, descText)
	before := readString(x.h.describedPath(hSvc))
	for name, c := range map[string]struct{ cmd, in string }{
		"bad JSON":          {"register_description", "{"},
		"empty":             {"register_description", ""},
		"a bad rule":        {"register_description", `{"descriptor":2,"summary":"s","requests":[]}`},
		"over the limit":    {"register_description", strings.Repeat(" ", descriptor.MaxBytes+1)},
		"--see no value":    {"register_description --see", descNone},
		"an extglob":        {"register_description --see *(*(a))b", descNone},
		"--clear and see":   {"register_description --clear --see a", ""},
		"an unexpected arg": {"register_description --nope", descNone},
	} {
		r := x.dis(hSvc, c.cmd, c.in)
		eq(t, name+": refused", r.code, 2)
		eq(t, name+": nothing on stdout", r.out, "")
		if r.err == "" {
			t.Errorf("%s: no reason", name)
		}
	}
	eq(t, "the earlier description stays", readString(x.h.describedPath(hSvc)), before)
	r := x.dis(hSvc, "register_description", descNone)
	has(t, "stdout says what was done", r.out, "described "+hSvc+" ")
	r = x.dis(hSvc, "register_description --clear")
	eq(t, "and for --clear", r.out, "cleared "+hSvc+"\n")
}

func TestDescriptionEvents(t *testing.T) {
	x := helpHub(t)
	file := func(id string) bool { return isFile(x.h.describedPath(id)) }
	x.describe(hSvc, descText)

	// rebound: kept
	if out, code := x.mint(x.key("one2").line, "add", hSvc, "--replace"); code != 0 {
		t.Fatalf("rebind: %s", out)
	}
	eq(t, "a rebind keeps it", file(hSvc), true)
	eq(t, "and a session of the old key cannot write", x.disA(Admission{FQ: hSvc, Serial: "1", KeyID: x.keyid("one")}, "register_description", descNone).code, 3)

	// removed
	if out, code := x.mint("", "rm", hSvc); code != 0 {
		t.Fatalf("rm: %s", out)
	}
	eq(t, "hub-mint rm removes it", file(hSvc), false)

	// --no-instances: kept, its instance forms are gone
	x.describe(hGroup, descMixed)
	if out, code := x.mint(x.key("grp").line, "add", hGroup, "--no-instances"); code != 0 {
		t.Fatalf("no-instances: %s", out)
	}
	eq(t, "--no-instances keeps it", file(hGroup), true)
	eq(t, "and its instance forms are no target", x.dis(hGroup, "help "+hInst).code, 3)

	// a registrar's claim: released, reclaimed by another key, reaped
	run := "run.t+a/ws-a@acme.ssh"
	x.reg("ws-a", "register run.t+a@acme.ssh --ttl 600", x.key("a").line)
	x.describe(run, descText)
	x.reg("ws-a", "release run.t+a@acme.ssh", "")
	eq(t, "a release removes it", file(run), false)

	run2 := "run.t+b/ws-a@acme.ssh"
	x.reg("ws-a", "register run.t+b@acme.ssh --ttl 600", x.key("b").line)
	x.describe(run2, descText)
	rec := x.h.claimPath(run2, "ws-a")
	s := readString(rec)
	lapsed := regexp.MustCompile(`expires=\d+`).ReplaceAllString(s, "expires=1")
	os.WriteFile(rec, []byte(lapsed), 0o640)
	has(t, "a lapsed claim is shown", x.dis(hSvc2(t, x), "help "+run2).out, "Claim: lapsed\n")
	eq(t, "and keeps it", file(run2), true)
	x.reg("ws-a", "register run.t+b@acme.ssh --ttl 600", x.key("b2").line)
	eq(t, "a reclaim by another key removes it", file(run2), false)

	run3 := "run.t+c/ws-a@acme.ssh"
	x.reg("ws-a", "register run.t+c@acme.ssh --ttl 600", x.key("c").line)
	x.describe(run3, descText)
	rec = x.h.claimPath(run3, "ws-a")
	os.WriteFile(rec, []byte(regexp.MustCompile(`expires=\d+`).ReplaceAllString(readString(rec), "expires=1")), 0o640)
	x.h.Reap()
	eq(t, "the reaper's sweep removes it with the claim", file(run3), false)

	// an orphan, and a temp file nobody finished
	orphan := x.h.describedPath("svc.gone@acme.ssh")
	os.WriteFile(orphan, []byte("Date: x\n\n"), 0o640)
	tmp := filepath.Join(x.h.described, ".new.1.1")
	os.WriteFile(tmp, nil, 0o640)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(tmp, old, old)
	x.h.Reap()
	eq(t, "an orphan goes", isFile(orphan), false)
	eq(t, "and an old temp file", isFile(tmp), false)
	eq(t, "a live identity's stays", file(hGroup), true)
}

func hSvc2(t *testing.T, x *th) string {
	t.Helper()
	x.mustMint("svc.reader@acme.ssh", "reader")
	return "svc.reader@acme.ssh"
}

func TestHelpTopic(t *testing.T) {
	x := helpHub(t)
	x.mkTopic(hTopic, "svc.*@corp.ssh")
	eq(t, "own org, no description", x.dis(hSvc, "help "+hTopic).out, "Target: "+hTopic+"\nType: topic\nRole: topic\nReceives: policy\nPublish: no\nSubscribe: yes\nDescribed: no\n\n"+noDesc)
	m := strict(t, "another org its policy lets publish", x.dis(hOut, "help "+hTopic+" --json").out)
	eq(t, "may publish", m["publish"], true)
	eq(t, "may not subscribe", m["subscribe"], false)
	x.reg("ws-c", "register run.t+cara@corp.ssh --ttl 600", x.key("cara").line)
	eq(t, "and another org it does not, with a pattern that misses", x.dis(hOut, "help topic+no@acme.ssh").code, 3)

	st, err := descriptor.Build([]byte(`{"descriptor":1,"summary":"Events.","requests":[{"subject":"e","form":"structured","delivery":["spool"],"summary":"An event.","schema":{}}]}`), descriptor.Options{Topic: true})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(x.h.topicDir(hTopic), "descriptor"), append([]byte("Date: 2026-11-05T00:00:00Z\n"), st.Bytes()...), 0o640)
	strict(t, "a described topic", x.dis(hSvc, "help "+hTopic).out)
	has(t, "its date", x.dis(hSvc, "help "+hTopic+" --json").out, `"described":"2026-11-05T00:00:00Z"`)
}

// Every asker, target and form of help, and the refusals of register_description, against
// testdata/help-matrix.golden (dates taken out). HUB_UPDATE_GOLDEN=1 writes it.
func TestHelpMatrix(t *testing.T) {
	x := helpHub(t)
	x.mustMint("publisher+app@acme.ssh", "pub")
	x.mkTopic(hTopic, "publisher+app@acme.ssh")
	os.WriteFile(filepath.Join(x.state, "aliases", "carol"), []byte("type=email\naddr=c@example.com\n"), 0o640)
	x.describe(hGroup, descMixed)
	x.describe(hOut, descProg, "--see", "svc.one@*")
	st, _ := descriptor.Build([]byte(descText), descriptor.Options{Topic: true})
	os.WriteFile(filepath.Join(x.h.topicDir(hTopic), "descriptor"), append([]byte("Date: 2026-11-05T00:00:00Z\n"), st.Bytes()...), 0o640)

	var got strings.Builder
	add := func(what string, r result) {
		fmt.Fprintf(&got, "### %s\nexit %d\n--- stdout\n%s--- stderr\n%s", what, r.code, r.out, r.err)
	}
	for _, me := range []string{hSvc, hOut, hGroup, "publisher+app@acme.ssh"} {
		for _, target := range []string{"", hSvc, hOut, hGroup, hInst, "svc.none+x@acme.ssh", hMissed, hTopic, "topic+no@acme.ssh", "carol", "nobody", "sub.x+y@acme.ssh", "Bad@T"} {
			for _, flag := range []string{"", " --json"} {
				cmd := strings.TrimRight("help "+target, " ") + flag
				add(me+": "+cmd, x.dis(me, cmd))
			}
		}
	}
	for _, c := range []struct{ cmd, in string }{
		{"register_description", "{"},
		{"register_description", descNone + "x"},
		{"register_description --see", descNone},
		{"register_description --see (", descNone},
		{"register_description --clear --see a", ""},
		{"register_description --nope", descNone},
	} {
		add(c.cmd, x.dis(hSvc, c.cmd, c.in))
	}
	add("registers", x.dis(hSvc, "register_description --see *@a.ssh --see *@b.ssh", descMixed))
	fmt.Fprintf(&got, "### stored\n%s", readString(x.h.describedPath(hSvc)))
	add("clears", x.dis(hSvc, "register_description --clear"))

	norm := isoRE.ReplaceAllString(got.String(), "D")
	golden := filepath.Join("testdata", "help-matrix.golden")
	if os.Getenv("HUB_UPDATE_GOLDEN") != "" {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(golden, []byte(norm), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := strings.Split(readString(golden), "\n")
	for i, l := range strings.Split(norm, "\n") {
		if i >= len(want) || l != want[i] {
			w := ""
			if i < len(want) {
				w = want[i]
			}
			t.Fatalf("%s line %d:\n got  %q\n want %q", golden, i+1, l, w)
		}
	}
	eq(t, "as long as the golden", len(strings.Split(norm, "\n")), len(want))
}

// Every verb the hub takes is in help.json and help.txt, and no others, by its usage line.
func TestHelpIsComplete(t *testing.T) {
	x := helpHub(t)
	verbsOf := func(usage string) []string {
		u := strings.TrimPrefix(strings.TrimSpace(usage), "usage: ")
		return strings.Split(strings.TrimSuffix(u, " (see docs)"), "|")
	}
	g := verbsOf(x.dis(hSvc, "nonesuch").err)
	raw, err := os.ReadFile(filepath.Join(x.lib, "help.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "\n") != 1 || !strings.HasSuffix(string(raw), "\n") {
		t.Error("help.json is not one compact line")
	}
	var h struct {
		Verbs []struct {
			Verb, Usage, Summary string
			Needs                []string
		} `json:"verbs"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatal(err)
	}
	var js []string
	rendered := ""
	txt := readString(filepath.Join(x.lib, "help.txt"))
	for _, v := range h.Verbs {
		js = append(js, v.Verb)
		rendered += v.Usage + "\n  " + v.Summary
		if len(v.Needs) > 0 {
			rendered += " Needs: " + strings.Join(v.Needs, ", ") + "."
		}
		rendered += "\n"
		if v.Summary == "" || !strings.HasPrefix(v.Usage, v.Verb) {
			t.Errorf("%s: bad entry", v.Verb)
		}
	}
	eq(t, "help.txt says what help.json says", strings.HasPrefix(txt, rendered), true)
	gs, js2 := append([]string{}, g...), append([]string{}, js...)
	sortStrings(gs)
	sortStrings(js2)
	eq(t, "help.json has exactly the hub's verbs", js2, gs)
	eq(t, "the alias types agree", strings.TrimSpace(x.bash(`printf %s "$BACKEND_TYPES"`)), strings.Join(backendTypes, " "))
}

func sortStrings(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}
