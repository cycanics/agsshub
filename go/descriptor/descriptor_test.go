package descriptor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtures = "../../test/descriptors"

// One file per rule: good/ must build, bad/ must be refused. -v logs each refusal's reason, to see
// a fixture refused for the wrong rule. A name with "topic" is built as a topic's description.
func TestFixtures(t *testing.T) {
	good, _ := filepath.Glob(fixtures + "/good/*")
	bad, _ := filepath.Glob(fixtures + "/bad/*")
	if len(good) == 0 || len(bad) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range good {
		b, _ := os.ReadFile(f)
		st, err := Build(b, Options{Topic: strings.Contains(f, "topic")})
		if err != nil {
			t.Errorf("%s refused: %v", filepath.Base(f), err)
			continue
		}
		for _, v := range []View{st.Whole, st.Instance} {
			if len(v.JSON) > 0 && (!json.Valid(v.JSON) || bytes.ContainsRune(v.JSON, '\n')) {
				t.Errorf("%s: view not compact JSON: %s", filepath.Base(f), v.JSON)
			}
		}
	}
	for _, f := range bad {
		b, _ := os.ReadFile(f)
		_, err := Build(b, Options{Topic: strings.Contains(f, "topic")})
		if err == nil {
			t.Errorf("%s accepted", filepath.Base(f))
			continue
		}
		t.Logf("%s: %v", filepath.Base(f), err)
	}
}

func TestSizeLimits(t *testing.T) {
	if _, err := Build(nil, Options{}); err == nil {
		t.Error("empty accepted")
	}
	head := `{"descriptor":1,"summary":"s","requests":[],"version":"1"}`
	big := []byte(head + strings.Repeat(" ", MaxBytes+1-len(head)))
	if _, err := Build(big, Options{}); err == nil {
		t.Error("over the limit accepted")
	}
	if _, err := Build(big[:MaxBytes], Options{}); err != nil {
		t.Errorf("at the limit refused: %v", err)
	}
}

func TestPatterns(t *testing.T) {
	d := []byte(`{"descriptor":1,"summary":"s","requests":[]}`)
	for _, ok := range [][]string{{"*@acme.ssh"}, {"group.*@*.ssh", "run.x+?/ws-a@b.ssh"}} {
		if _, err := Build(d, Options{See: ok}); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	for _, p := range []string{"", "a b", "a\nDate: x", "*(*(a))b", "@(a|b)", "[ab]", "A", "!x", strings.Repeat("a", 129)} {
		if _, err := Build(d, Options{See: []string{p}}); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
	if _, err := Build(d, Options{See: make17()}); err == nil {
		t.Error("17 patterns accepted")
	}
}

func make17() []string {
	s := make([]string, 17)
	for i := range s {
		s[i] = "a"
	}
	return s
}

const mixed = `{
  "descriptor": 1, "version": "0.5", "summary": "S",
  "requests": [
    {"subject":"cmd.x","form":"structured","delivery":["live"],"summary":"X","scope":"address","schema":{"type":"object","n":1.50}},
    {"subject":"in.y","form":"structured","delivery":["spool","live"],"summary":"Y","scope":"instance","schema":{},"guide":"one\n\ttwo\n"},
    {"subject":null,"form":"text","delivery":["spool"],"summary":"Z","scope":"both","guide":"say","files":"file"}
  ]
}`

func TestViews(t *testing.T) {
	st, err := Build([]byte(mixed), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Whole.Default != "text" || st.Instance.Default != "text" {
		t.Errorf("defaults: %s %s", st.Whole.Default, st.Instance.Default)
	}
	if !bytes.Contains(st.Whole.JSON, []byte(`"cmd.x"`)) || bytes.Contains(st.Instance.JSON, []byte(`"cmd.x"`)) {
		t.Errorf("an address request belongs only to the whole view: %s", st.Instance.JSON)
	}
	if !bytes.Contains(st.Whole.JSON, []byte(`"n":1.50`)) {
		t.Errorf("compacting changed a value: %s", st.Whole.JSON)
	}
	want := "S\nVersion: 0.5\n\n" +
		"in.y\tstructured\tspool,live\tinstance\t-\tY\n(no subject)\ttext\tspool\tboth\tfile\tZ\n\n" +
		"in.y:\n  one\n  \ttwo\n\n(no subject):\n  say\n"
	if string(st.Instance.Text) != want {
		t.Errorf("rendering:\n%q\nwant\n%q", st.Instance.Text, want)
	}
	var top struct {
		Requests []json.RawMessage `json:"requests"`
		Version  string            `json:"version"`
	}
	if err := json.Unmarshal(st.Instance.JSON, &top); err != nil || len(top.Requests) != 2 || top.Version != "0.5" {
		t.Errorf("instance JSON: %v %s", err, st.Instance.JSON)
	}
}

func TestDefaultFormat(t *testing.T) {
	s := func(sc string) string {
		return `{"subject":"a` + sc + `","form":"structured","delivery":["live"],"summary":"x","scope":"` + sc + `","schema":{}}`
	}
	cases := []struct {
		name, reqs, whole, inst string
	}{
		{"none", ``, "text", "text"},
		{"all structured", s("both"), "json", "json"},
		{"structured for the address only", s("address"), "json", "text"},
		{"a text request", s("both") + `,{"subject":null,"form":"text","delivery":["spool"],"summary":"x","guide":"g","scope":"address"}`, "text", "json"},
	}
	for _, c := range cases {
		st, err := Build([]byte(`{"descriptor":1,"summary":"s","requests":[`+c.reqs+`]}`), Options{})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if st.Whole.Default != c.whole || st.Instance.Default != c.inst {
			t.Errorf("%s: whole %s, instance %s", c.name, st.Whole.Default, st.Instance.Default)
		}
		if (st.Whole.Default == "json") != (len(st.Whole.Text) == 0) {
			t.Errorf("%s: text stored against the default", c.name)
		}
	}
	st, _ := Build([]byte(`{"descriptor":1,"summary":"s","requests":[]}`), Options{})
	if string(st.Whole.Text) != "s\n\nTakes no requests.\n" {
		t.Errorf("no requests: %q", st.Whole.Text)
	}
}

func TestRecordRoundTrip(t *testing.T) {
	st, err := Build([]byte(mixed), Options{See: []string{"*@a.ssh", "*@b.ssh"}})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := ParseRecord(append([]byte("Date: 2026-11-05T00:00:00Z\n"), st.Bytes()...))
	if !ok || r.See != "*@a.ssh *@b.ssh" || r.Date != "2026-11-05T00:00:00Z" {
		t.Fatalf("%v %+v", ok, r)
	}
	if !bytes.Equal(r.Whole.JSON, st.Whole.JSON) || !bytes.Equal(r.Instance.Text, st.Instance.Text) || r.Instance.Default != "text" {
		t.Errorf("record differs: %+v", r)
	}
	r2, ok := ParseRecord(append([]byte("Date: d\n"), mustBuild(t, Options{}).Bytes()...))
	if !ok || r2.See != "" {
		t.Errorf("empty See: %v %+v", ok, r2)
	}
	if _, ok := ParseRecord([]byte("Date: d\nWhole-Json-Bytes: 99\n\nshort")); ok {
		t.Error("a short file parsed")
	}
}

func mustBuild(t *testing.T, o Options) *Stored {
	t.Helper()
	st, err := Build([]byte(mixed), o)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// live: an optional boolean at the top; false makes the hub refuse every --live (exit 9); a topic takes no true.
func TestLive(t *testing.T) {
	for _, c := range []struct {
		in      string
		refused bool
	}{
		{`{"descriptor":1,"summary":"s","live":false,"requests":[]}`, true},
		{`{"descriptor":1,"summary":"s","live":true,"requests":[]}`, false},
		{`{"descriptor":1,"summary":"s","requests":[]}`, false},
	} {
		st, err := Build([]byte(c.in), Options{})
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got := LiveRefused(st.Whole.JSON); got != c.refused {
			t.Errorf("%s: LiveRefused %v", c.in, got)
		}
		if got := LiveRefused(st.Instance.JSON); got != c.refused {
			t.Errorf("%s: the instance view: LiveRefused %v", c.in, got)
		}
	}
	if _, err := Build([]byte(`{"descriptor":1,"summary":"s","live":true,"requests":[]}`), Options{Topic: true}); err == nil {
		t.Error("a topic with live: true was taken")
	}
	if _, err := Build([]byte(`{"descriptor":1,"summary":"s","live":false,"requests":[]}`), Options{Topic: true}); err != nil {
		t.Errorf("a topic with live: false: %v", err)
	}
}
