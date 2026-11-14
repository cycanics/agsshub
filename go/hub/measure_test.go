package hub

// Measurements the design asks the build to report (docs/wake-live-event-id.md): publish latency against
// body size and subscription count, and the wake notice's render cost against instance and unread count. Timings are logged, never asserted; HUB_MEASURE=1 runs them.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func measuring(t *testing.T) {
	if os.Getenv("HUB_MEASURE") == "" {
		t.Skip("HUB_MEASURE=1 runs the measurements")
	}
}

func median(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func TestMeasurePublish(t *testing.T) {
	measuring(t)
	x := twoRuns(t)
	os.WriteFile(filepath.Join(x.state, "budgets", "run"), []byte("subject=run\nrate=1000\nburst=100000\n"), 0o640)
	topic := func(name string, subs int) string {
		tp := "topic+" + name + "@acme.ssh"
		x.mkTopic(tp, alice)
		os.WriteFile(filepath.Join(x.h.topicDir(tp), "policy"), []byte("topic="+tp+"\npublish="+alice+"\nrate=1000\nburst=100000\n"), 0o640)
		for i := 0; i < subs; i++ { // distinct owners: one subscription each
			owner := alice
			if i == 1 {
				owner = bob
			}
			if i >= 2 { // more subscriptions than owners: records written as the hub would
				sub := fmt.Sprintf("sub.%s+%08x@acme.ssh", name, i)
				rp := filepath.Join(x.h.topicDir(tp), "subs", sub)
				os.WriteFile(rp, []byte(fmt.Sprintf("sub=%s\ntopic=%s\nowner=%s\ncreated=1\nexpires=9999999999\n", sub, tp, alice)), 0o640)
				x.h.subPlace(tp, sub)
				continue
			}
			x.dis(owner, "sub "+tp)
		}
		return tp
	}
	var holds []time.Duration
	lockHeld = func(_ string, d time.Duration) { holds = append(holds, d) }
	defer func() { lockHeld = func(string, time.Duration) {} }()
	run := func(what, tp, cmd, stdin string, n int) {
		holds = nil
		var ds []time.Duration
		for i := 0; i < n; i++ {
			t0 := time.Now()
			r := x.dis(alice, "send"+cmd, stdin)
			ds = append(ds, time.Since(t0))
			if r.code != 0 {
				t.Fatalf("%s: %+v", what, r.code)
			}
		}
		t.Logf("MEASURE publish %-34s median %8.2f ms, topic lock held median %8.2f ms (n=%d)", what,
			float64(median(ds).Microseconds())/1000, float64(median(holds).Microseconds())/1000, n)
	}
	for _, sz := range []int{1 << 10, 1 << 20, 16 << 20, 64 << 20} {
		tp := topic(fmt.Sprintf("size%d", sz), 1)
		n := 9
		if sz >= 16<<20 {
			n = 3
		}
		run(fmt.Sprintf("--file %d MiB-ish (%d B), 1 sub", sz>>20, sz), tp, " "+tp+" --file f", strings.Repeat("x", sz), n)
	}
	tp := topic("bag", 1)
	members := map[string]string{}
	for i := 0; i < 4; i++ {
		members[fmt.Sprintf("m%d.bin", i)] = strings.Repeat("y", 63<<20)
	}
	bag := string(makeTar(t, members))
	t.Logf("MEASURE bag stream %d B", len(bag))
	run("send_bag 252 MiB, 4 members, 1 sub", tp, "_bag "+tp+"", bag, 3)
	for _, subs := range []int{1, 10, 100} {
		tp := topic(fmt.Sprintf("subs%d", subs), subs)
		run(fmt.Sprintf("1 KiB -m, %d subs", subs), tp, " "+tp+" -m", strings.Repeat("z", 1024), 9)
		run(fmt.Sprintf("16 MiB --file, %d subs", subs), tp, " "+tp+" --file f", strings.Repeat("z", 16<<20), 3)
	}
	tpb := topic("bagsubs", 100)
	run("send_bag 252 MiB, 100 subs", tpb, "_bag "+tpb+"", bag, 2)
}

func TestMeasureWakeRender(t *testing.T) {
	measuring(t)
	for _, insts := range []int{10, 100, 1000} {
		for _, unread := range []int{0, 1, 10} {
			x := instRig(t)
			for i := 0; i < 5000; i++ { // other addresses' mailboxes: not read
				os.MkdirAll(x.h.mbox(fmt.Sprintf("group.other%04d@expa.ssh", i)), 0o2770)
			}
			for i := 0; i < insts; i++ {
				box := x.h.mbox(inst(fmt.Sprintf("i%04d", i)))
				os.MkdirAll(box, 0o2770)
				for j := 0; j < unread; j++ {
					os.WriteFile(filepath.Join(box, fmt.Sprintf("m%03d.msg", j)),
						[]byte(fmt.Sprintf("From: s%d@home.ssh\nDate: 2026-11-07T10:%02d:00Z\nSerial: 1\nSubject: v\n\nbody\n", j%3, j%60)), 0o640)
				}
			}
			var ds []time.Duration
			lines := 0
			for k := 0; k < 7; k++ {
				t0 := time.Now()
				w := x.h.renderWake(par)
				ds = append(ds, time.Since(t0))
				lines = w.n
			}
			want := insts
			if unread == 0 {
				want = 0
			}
			eq(t, "lines", lines, want)
			t.Logf("MEASURE wake render (5000 other mailboxes) %4d instances × %2d unread: median %8.3f ms, %d lines", insts, unread, float64(median(ds).Microseconds())/1000, lines)
		}
	}
}
