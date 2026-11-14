package hub

// The operator tools the daemon does not run itself: lib/mint's refusals, and the arithmetic
// bin/hub-budget takes from lib/common.sh. test/dev-hub.sh drives bin/hub-* as root on the installed image.

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestMintRefusals(t *testing.T) {
	x := newTH(t)
	x.reg("ws-a", "register run.t+alice@acme.ssh --ttl 600", x.key("alice").line)
	mk := x.key("mk").line
	for _, c := range []struct {
		what, id string
		want     int
		says     string
	}{
		{"a group needs its name", "group@expa.ssh", 2, "project is required"},
		{"an instance form is never minted", "group.demo+x@expa.ssh", 3, "the holder of that address registers it"},
		{"a group is ws-less", "group.demo/ws-a@expa.ssh", 2, "is ws-less"},
		{"commhub takes no project", "commhub.x@home.ssh", 2, "project is not allowed"},
		{"a run is its workspace's to mint", "run.p+i9/ws-a@acme.ssh", 3, "minted by its workspace's registrar"},
		{"a hub role is nobody's", "topic+x@acme.ssh", 3, "is the hub's own"},
		{"a pinned operator role names its workspace", "main@acme.ssh", 2, "is pinned"},
		{"a workspace enrolled for the org", "main/ws-zzz@acme.ssh", 3, "is not enrolled for org acme"},
	} {
		out, rc := x.mint(mk, "add", c.id)
		eq(t, c.what, rc, c.want)
		has(t, c.what+": says why", out, c.says)
		eq(t, c.what+": no record", exists(filepath.Join(x.state, "claims", "operator", fsName(c.id))), false)
	}
	_, rc := x.mint(mk, "add", "main/ws-a@acme.ssh")
	eq(t, "main on an enrolled workspace", rc, 0)
	_, rc = x.mint(x.key("alice").line, "add", "group.demo@expa.ssh")
	eq(t, "a key bound to a registrar's identity is refused", rc, 1)
	_, rc = x.mint(mk, "add", "commhub@home.ssh")
	eq(t, "a key bound to another identity is refused", rc, 1)

	x.mustMint("commhub@home.ssh", "hubk")
	out, rc := x.mint(x.key("hubk").line, "add", "commhub@home.ssh", "--ttl", "3600")
	eq(t, "the same key renews", rc, 0)
	has(t, "serial kept", out, "serial=1")
	out, rc = x.mint(x.key("hubk2").line, "add", "commhub@home.ssh")
	eq(t, "a different key needs --replace", rc, 1)
	has(t, "says so", out, "--replace rebinds it")
	_, rc = x.mint("", "add", "svc.x@acme.ssh")
	eq(t, "no key on stdin", rc, 2)
	_, rc = x.mint(x.key("k1").line, "add", "svc.x@acme.ssh", "--ttl", "59")
	eq(t, "a ttl under 60 s", rc, 2)
	_, rc = x.mint("", "rm", "svc.never@acme.ssh")
	eq(t, "removing what is not on record", rc, 3)
	out, _ = x.mint("", "list")
	has(t, "list", out, "commhub@home.ssh serial=1 ")
	_, rc = x.mint("", "frob")
	eq(t, "an unknown verb", rc, 2)
}

func TestCommonRateArithmetic(t *testing.T) {
	x := newTH(t)
	eq(t, "rates are decimal messages/second", x.bash(`echo "$(rate_mt 0.5) $(rate_mt 2) $(rate_mt 1.25) $(mt_rate 1250)"; rate_mt 0 || echo refused`), "500 2000 1250 1.25\nrefused\n")
}

// A description may come slowly: nothing is written until the whole of it has.
func TestRegisterDescriptionFromASlowSender(t *testing.T) {
	x := helpHub(t)
	reg := x.start(x.adm(hSvc), "register_description")
	time.Sleep(100 * time.Millisecond)
	eq(t, "no temp file while stdin is still coming", fmt.Sprint(globNamesAll(filepath.Join(x.state, "described"))), "[]")
	fmt.Fprint(reg.in, descNone)
	reg.in.Close()
	eq(t, "and a slow sender registers", reg.wait(t, 2*time.Second), 0)
	has(t, "what it sent", x.dis(hSvc, "help "+hSvc).out, "Takes nothing.")
}
