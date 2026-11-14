package hub

// Instances: the form only on --instances addresses, registration by the address, a new key evicting
// the old, release, expiry, the address ending them. Admission and the wake notice: wake_test.go.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const par = "group.sess@expa.ssh"

func instRig(t *testing.T) *th {
	x := newTH(t)
	x.mustMint("commhub@home.ssh", "hubk")
	x.mustMint("group.demo@expa.ssh", "grpk")
	os.WriteFile(filepath.Join(x.state, "budgets", "commhub"), []byte("subject=commhub\nrate=50\nburst=500\n"), 0o640)
	out, c := x.mint(x.key("park").line, "add", par, "--instances")
	if c != 0 || !strings.Contains(out, " instances") {
		t.Fatalf("hub-mint add --instances: %d %s", c, out)
	}
	return x
}

// idis: a session of an instance, by its key.
func (x *th) idis(inst, serial, keyName, cmd string, stdin ...string) result {
	return x.disA(Admission{FQ: inst, Serial: serial, KeyID: x.keyid(keyName)}, cmd, stdin...)
}

var serialRE = regexp.MustCompile(` serial=([0-9]+) `)

func (x *th) register(id, keyName string, extra ...string) (string, result) {
	r := x.dis(par, strings.TrimSpace("register "+id+" "+strings.Join(extra, " ")), x.key(keyName).line)
	m := serialRE.FindStringSubmatch(r.out)
	if m == nil {
		return "", r
	}
	return m[1], r
}

func TestInstancesRegistration(t *testing.T) {
	x := instRig(t)
	hub := "commhub@home.ssh"
	i1 := "group.sess+a1b2c3d4@expa.ssh"
	mlist, _ := x.mint("", "list")
	has(t, "hub-mint list", mlist, par+" serial=1 ")
	r := x.dis(hub, "send group.demo+a1b2c3d4@expa.ssh --id x-1 -- x")
	code(t, "the instance form of an address that takes none: no such target", r, 3)
	has(t, "saying why", r.err, "group.demo@expa.ssh takes no instances")
	code(t, "nor --live: live not supported (9)", x.dis(hub, "send group.demo+a1b2c3d4@expa.ssh --live -m", string(frame([]byte("x"), 4, ""))), 9)
	code(t, "nor a bag", x.dis("group.demo@expa.ssh", "send_bag commhub+a1b2c3d4@home.ssh", string(makeTar(t, map[string]string{"a": "b"}))), 3)
	if exists(filepath.Join(x.state, "inbox", "group.demo+a1b2c3d4@expa.ssh")) {
		t.Error("a mailbox was made for it")
	}
	code(t, "an address that takes no instances registers none", x.dis("group.demo@expa.ssh", "register a1b2c3d4", x.key("i1k").line), 3)
	_, c := x.mint(x.key("i1k").line, "add", i1)
	eq(t, "the operator mints no instance", c, 3)
	for _, bad := range []string{"A1B2C3D4", "a+b", "-ab", strings.Repeat("a", 33)} {
		code(t, "instance id '"+bad+"' outside the grammar", x.dis(par, "register "+bad, x.key("i1k").line), 2)
	}
	code(t, "another address's instance -> 3", x.dis(par, "register group.demo+a1b2c3d4@expa.ssh", x.key("i1k").line), 3)
	ents, _ := os.ReadDir(filepath.Join(x.state, "claims/instance"))
	eq(t, "(nothing registered by the refusals)", len(ents), 0)

	ser, r := x.register("a1b2c3d4 --ttl 600", "i1k")
	has(t, "the address registers an instance with a fresh key", r.out, "registered "+i1+" fpr=")
	eq(t, "serial from the instance counter", ser, "1")
	a, ok := x.h.admitHub(x.key("i1k").pub)
	eq(t, "the daemon admits that key as the instance", ok && a.FQ == i1 && a.Serial == "1", true)
	eq(t, "its session is the instance", x.idis(i1, "1", "i1k", "id").out, i1+" serial=1\n")
	has(t, "the full identity names it too", x.dis(par, "register "+i1+" --ttl 600", x.key("i1k").line).out, "registered "+i1+" ")
	has(t, "the same key again renews: same serial", x.dis(par, "register a1b2c3d4", x.key("i1k").line).out, " serial=1 ")
	code(t, "an instance registers none", x.idis(i1, "1", "i1k", "register x0", x.key("i1k2").line), 3)
	code(t, "one key, one instance", x.dis(par, "register b0b0b0b0", x.key("i1k").line), 1)
	_, c = x.mint(x.key("i1k").line, "add", "foreman@expa.ssh")
	eq(t, "an instance's key is bound for hub-mint too", c, 1)
	code(t, "and for the registrar", x.reg("ws-a", "register run.t+ik@acme.ssh --ttl 600", x.key("i1k").line), 1)
	os.Symlink("../instance/"+i1, filepath.Join(x.state, "claims/.ibyfpr", x.keyid("stray")))
	if _, ok := x.h.admitHub(x.key("stray").pub); ok {
		t.Error("an index entry its record disagrees with admits a key")
	}
	os.Remove(filepath.Join(x.state, "claims/.ibyfpr", x.keyid("stray")))
	os.Symlink("../operator/"+par, filepath.Join(x.state, "claims/.ibyfpr", x.keyid("park")))
	os.Remove(filepath.Join(x.state, "claims/.byfpr", x.keyid("park")))
	if _, ok := x.h.admitHub(x.key("park").pub); ok {
		t.Error("the instance index admits an operator claim")
	}
	os.Symlink("../operator/"+par, filepath.Join(x.state, "claims/.byfpr", x.keyid("park")))
	os.Remove(filepath.Join(x.state, "claims/.ibyfpr", x.keyid("park")))

	f := x.start(Admission{FQ: i1, Serial: "1", KeyID: x.keyid("i1k")}, "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "its follow", x.followed(i1))
	ser, r = x.register("a1b2c3d4", "i1k2")
	eq(t, "a new run registers a new key: next serial", ser, "2")
	eq(t, "re-registering evicts the previous connection: its follow ends at once", f.wait(t, 2*time.Second), 0)
	has(t, "saying so", f.err.String(), "is gone")
	code(t, "and its next command is refused", x.idis(i1, "1", "i1k", "id"), 3)
	if _, ok := x.h.admitHub(x.key("i1k").pub); ok {
		t.Error("its key is still in")
	}
	eq(t, "the new one is in", x.idis(i1, "2", "i1k2", "id").out, i1+" serial=2\n")
	has(t, "the previous run's key is free again", x.dis(par, "register b0b0b0b0", x.key("i1k").line).out, "registered group.sess+b0b0b0b0@expa.ssh ")
	x.dis(par, "release b0b0b0b0")

	eq(t, "the address releases an instance", x.dis(par, "release a1b2c3d4").out, "released "+i1+"\n")
	if _, ok := x.h.admitHub(x.key("i1k2").pub); ok {
		t.Error("a released instance's key is in")
	}
	code(t, "its session is refused", x.idis(i1, "2", "i1k2", "id"), 3)
	code(t, "releasing it again -> 3", x.dis(par, "release a1b2c3d4"), 3)
	ser, _ = x.register("a1b2c3d4", "i1k3")
	eq(t, "an instance releases itself", x.idis(i1, ser, "i1k3", "release").out, "released "+i1+"\n")
	x.register("a1b2c3d4 --ttl 600", "i1k4")
	rewriteKey(filepath.Join(x.state, "claims/instance", i1), "expires", "1")
	x.h.Reap()
	if exists(filepath.Join(x.state, "claims/instance", i1)) {
		t.Error("the reaper left an expired instance claim")
	}
	if _, ok := x.h.admitHub(x.key("i1k4").pub); ok {
		t.Error("and its key")
	}
	ser, _ = x.register("a1b2c3d4", "i1k5")
	x.mint(x.key("park").line, "add", par)
	mlist, _ = x.mint("", "list")
	has(t, "renewing the address keeps it taking instances", mlist, par+" serial=1 ")
	eq(t, "and keeps its instances", x.idis(i1, ser, "i1k5", "id").out, i1+" serial="+ser+"\n")
	x.mint(x.key("park").line, "add", par, "--no-instances")
	if _, ok := x.h.admitHub(x.key("i1k5").pub); ok {
		t.Error("--no-instances left an instance in")
	}
	code(t, "and the instance form is no target any more", x.dis(hub, "send "+i1+" -- x"), 3)
	x.mint(x.key("park").line, "add", par, "--instances")
	ser6, _ := x.register("a1b2c3d4", "i1k6")
	f6 := x.start(Admission{FQ: i1, Serial: ser6, KeyID: x.keyid("i1k6")}, "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "its follow", x.followed(i1))
	x.mint(x.key("park2").line, "add", par, "--replace") // hub-mint evicts; the daemon observes
	if _, ok := x.h.admitHub(x.key("i1k6").pub); ok {
		t.Error("rebinding the address left its holder's instance in")
	}
	eq(t, "their follows end at once (observed)", f6.wait(t, 300*time.Millisecond), 0)
	x.mint(x.key("park").line, "add", par, "--replace")
	ser, _ = x.register("a1b2c3d4", "i1k7")
	rewriteKey(filepath.Join(x.state, "claims/operator", par), "serial", "99")
	if _, ok := x.h.admitHub(x.key("i1k7").pub); ok {
		t.Error("an instance whose address moved on (another serial) is admitted")
	}
	code(t, "nor its open session", x.idis(i1, ser, "i1k7", "id"), 3)
	x.h.Reap()
	if exists(filepath.Join(x.state, "claims/instance", i1)) {
		t.Error("the reaper left an orphaned instance")
	}
	rewriteKey(filepath.Join(x.state, "claims/operator", par), "serial", "3")
	os.WriteFile(filepath.Join(x.state, "budgets", par), []byte("subject="+par+"\nrate=7\nburst=9\n"), 0o640)
	rate, burst := x.h.budgetFor(i1)
	eq(t, "budgets: an instance spends its address's budget before its role's", fmt.Sprint(rate, " ", burst), "7000 9000")
}
