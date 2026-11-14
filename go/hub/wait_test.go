package hub

// --wait and --durable (docs/wake-live-event-id.md, Wait for pickup, Durability).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWait(t *testing.T) {
	x := twoRuns(t)
	os.WriteFile(filepath.Join(x.state, "budgets", "run"), []byte("subject=run\nrate=100\nburst=100\n"), 0o640)
	r := x.dis(alice, "send "+bob+" --id w1 --wait 0.2 -- x")
	eq(t, "not picked within <s>: exit 8, stored", fmt.Sprint(r.code, "|", r.out, "|", exists(filepath.Join(x.h.mbox(bob), "w1.msg"))), "8|stored, not picked within 0.2\n|true")
	eq(t, "the waiter gone with it", len(x.h.waits.m), 0)
	for i, pick := range []struct{ what, cmd string }{
		{"a listing", "recv --peek"},
		{"--peek:full", "recv --peek:full"},
		{"--peek:rfc5322", "recv --id MID --peek:rfc5322"},
		{"--id", "recv --id MID --peek"},
		{"--header:stderr: a part=body chunk", "recv --id MID --header:stderr --peek"},
	} {
		mid := fmt.Sprint("w-", i)
		b := x.start(x.adm(alice), "send "+bob+" --id "+mid+" --wait 5 -- x", "")
		waitFor(t, 2*time.Second, "stored", func() bool { return exists(filepath.Join(x.h.mbox(bob), mid+".msg")) })
		x.dis(bob, strings.Replace(pick.cmd, "MID", mid, 1))
		eq(t, "picked by "+pick.what+": exit 0", b.wait(t, 3*time.Second), 0)
		eq(t, "(stdout)", b.out.String(), "picked\n")
	}
	b := x.start(x.adm(alice), "send "+bob+" --id wf --wait 5 --file f", "data")
	waitFor(t, 2*time.Second, "stored", func() bool { return exists(filepath.Join(x.h.mbox(bob), "wf.msg")) })
	x.dis(bob, "recv --id wf --peek:header")
	x.dis(bob, "recv --peek:header")
	time.Sleep(50 * time.Millisecond)
	eq(t, "--peek:header is no pickup", b.out.String(), "")
	x.dis(bob, "recv --id wf --peek:file")
	eq(t, "picked by --peek:file", b.wait(t, 3*time.Second), 0)
	bag := string(makeTar(t, map[string]string{"a": "b"}))
	b = x.start(x.adm(alice), "send_bag "+bob+" --id wb --wait 5", bag)
	waitFor(t, 2*time.Second, "stored", func() bool { return exists(filepath.Join(x.h.mbox(bob), "wb.msg")) })
	x.dis(bob, "recv_bag --id wb --peek")
	eq(t, "picked by recv_bag", b.wait(t, 3*time.Second), 0)
	b = x.start(x.adm(alice), "send_bag "+bob+" --id wb2 --wait 5", bag)
	waitFor(t, 2*time.Second, "stored", func() bool { return exists(filepath.Join(x.h.mbox(bob), "wb2.msg")) })
	x.dis(bob, "recv --id wb2 --peek:bag a")
	eq(t, "picked by --peek:bag", b.wait(t, 3*time.Second), 0)
	f := x.start(x.adm(bob), "recv --follow --peek", "")
	waitFor(t, 2*time.Second, "the follow", x.followed(bob))
	r = x.dis(alice, "send "+bob+" --id wl --wait 5 -- y")
	eq(t, "picked by a follow", fmt.Sprint(r.code, "|", r.out), "0|picked\n")
	f.cancel()
	f.wait(t, 2*time.Second)
	r = x.dis(alice, "send "+bob+" --id wl --wait 5 -- y")
	eq(t, "a dup: dup, exit 0, no wait", fmt.Sprint(r.code, "|", r.out), "0|dup wl (dropped)\n")
	code(t, "an ack is no pickup", func() result {
		b := x.start(x.adm(alice), "send "+bob+" --id wa --wait 0.3 -- z", "")
		waitFor(t, 2*time.Second, "stored", func() bool { return exists(filepath.Join(x.h.mbox(bob), "wa.msg")) })
		x.dis(bob, "recv_ack wa")
		return result{code: b.wait(t, 3*time.Second)}
	}(), 8)
	// refusals
	x.mkTopic(evTopic, alice)
	os.WriteFile(filepath.Join(x.state, "aliases", "carol"), []byte("type=email\naddr=c@x\n"), 0o640)
	x.mustMint("publisher+p@acme.ssh", "pubk")
	for _, c := range []struct{ what, cmd string }{
		{"to a topic", "send " + evTopic + " --wait 1 -- x"},
		{"to an alias", "send carol --wait 1 -- x"},
		{"to a publisher", "send publisher+p@acme.ssh --wait 1 -- x"},
		{"with --live", "send " + bob + " --live --wait 1 -m"},
		{"0", "send " + bob + " --wait 0 -- x"},
		{"past 600", "send " + bob + " --wait 600.001 -- x"},
		{"four decimals", "send " + bob + " --wait 1.0001 -- x"},
		{"malformed", "send " + bob + " --wait 1s -- x"},
		{"no value", "send " + bob + " -m --wait"},
		{"send_bag to a topic", "send_bag " + evTopic + " --wait 1"},
	} {
		code(t, "--wait "+c.what+": 2", x.dis(alice, c.cmd, "x"), 2)
	}
	eq(t, "600 is in range: held until the sender leaves", x.start(x.adm(alice), "send "+bob+" --id w600 --wait 600 -- x", "").cancelAndCode(t), 8)
}

// cancelAndCode ends a held command at once: its exit code.
func (b *bg) cancelAndCode(t *testing.T) int {
	time.Sleep(50 * time.Millisecond)
	b.cancel()
	return b.wait(t, 3*time.Second)
}

// --wait to an instance: its own session's read is the pickup.
func TestWaitInstance(t *testing.T) {
	x := instRig(t)
	serial, _ := x.register("a1", "ik")
	b := x.heldSend(t, wHub, "send "+inst("a1")+" --id i1 --wait 5 -- hi", "", "a1")
	x.dis(par, "admit a1")
	waitFor(t, 2*time.Second, "stored", func() bool { return exists(filepath.Join(x.h.mbox(inst("a1")), "i1.msg")) })
	x.idis(inst("a1"), serial, "ik", "recv --peek")
	eq(t, "an instance's pickup", b.wait(t, 3*time.Second), 0)
	eq(t, "(stdout)", b.out.String(), "picked\n")
}

// --durable fsyncs the message file and its mailbox directory, and inbox/ when the send made the mailbox;
// nothing else on the send or publish path is fsynced.
func TestDurable(t *testing.T) {
	x := twoRuns(t)
	var mu sync.Mutex
	var synced []string
	fsynced = func(p string) { mu.Lock(); synced = append(synced, p); mu.Unlock() }
	defer func() { fsynced = func(string) {} }()
	got := func() string {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for _, p := range synced {
			rel, _ := filepath.Rel(x.state, p)
			if strings.HasPrefix(filepath.Base(rel), ".stage.") {
				rel = "stage"
			}
			out = append(out, rel)
		}
		synced = nil
		return strings.Join(out, " ")
	}
	box := "inbox/" + fsName(bob)
	r := x.dis(alice, "send "+bob+" --id d1 --durable -- x")
	eq(t, "the first: the file, the mailbox, inbox/ (the send made the mailbox)", fmt.Sprint(r.code, " ", got()), "0 stage "+box+" inbox")
	x.dis(alice, "send "+bob+" --id d2 --durable --file f", "data")
	eq(t, "then: the file and the mailbox", got(), "stage "+box)
	x.dis(alice, "send_bag "+bob+" --id d3 --durable", string(makeTar(t, map[string]string{"a": "b"})))
	eq(t, "send_bag too", got(), "stage "+box)
	for _, c := range []struct{ cmd, in string }{
		{"send " + bob + " --id n1 -- x", ""}, {"send " + bob + " --id n2 --file f", "x"}, {"send " + bob + " --id n3 -m:rfc5322", "Subject: s\n\nb\n"},
		{"send_bag " + bob + " --id n4", string(makeTar(t, map[string]string{"a": "b"}))},
	} {
		code(t, c.cmd, x.dis(alice, c.cmd, c.in), 0)
	}
	x.mkTopic(evTopic, alice)
	x.dis(bob, "sub "+evTopic)
	x.dis(alice, "send "+evTopic+" -- x")
	x.dis(alice, "send_bag "+evTopic, string(makeTar(t, map[string]string{"a": "b"})))
	eq(t, "without --durable: no fsync on the send or publish path", got(), "")
	r = x.dis(alice, "send "+evTopic+" --durable -- x")
	eq(t, "on a topic: ignored, said on stderr", fmt.Sprint(r.code, "|", r.err, "|", got()), "0|durable: ignored\n|")
	os.WriteFile(filepath.Join(x.state, "aliases", "carol"), []byte("type=email\naddr=c@x\n"), 0o640)
	r = x.dis(alice, "send carol --durable -- x")
	has(t, "on an alias: ignored", r.err, "durable: ignored\n")
	code(t, "with --live: 2", x.dis(alice, "send "+bob+" --live --durable -m", "x"), 2)
	// a held send: synced once stored, after admit
	y := instRig(t)
	var ys []string
	fsynced = func(p string) { mu.Lock(); ys = append(ys, filepath.Base(p)); mu.Unlock() }
	b := y.heldSend(t, wHub, "send "+inst("h1")+" --id h --durable -- x", "", "h1")
	y.dis(par, "admit h1")
	code(t, "held, then admitted", result{code: b.wait(t, 3*time.Second)}, 0)
	mu.Lock()
	eq(t, "the file, the instance's mailbox, inbox/ (made by admit)", len(ys) == 3 && ys[1] == fsName(inst("h1")) && ys[2] == "inbox", true)
	mu.Unlock()
}
