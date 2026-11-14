package hub

// The reaper's bounds (D11).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A full bucket (back at its burst) is idle: the reaper removes it; a spent one stays.
func TestReaperFloodBuckets(t *testing.T) {
	x := twoRuns(t)
	x.dis(alice, "send "+bob+" -- one")
	x.dis(bob, "send "+alice+" -- two")
	n := func() int { return len(globNames(filepath.Join(x.state, "flood"))) }
	eq(t, "two buckets", n(), 2)
	x.h.Reap()
	eq(t, "spent ones stay", n(), 2)
	f := filepath.Join(x.state, "flood", strings.ReplaceAll(alice, "/", "_")+"__"+strings.ReplaceAll(bob, "/", "_"))
	has(t, "a bucket records its rate and burst", readString(f), fmt.Sprintf("rate=%d\nburst=%d\n", FloodRateMT, FloodBurstMT))
	later := x.h.epoch() + FloodBurstMT/FloodRateMT + 1
	x.h.now = func() time.Time { return time.Unix(later, 0) }
	x.h.Reap()
	x.h.now = time.Now
	eq(t, "refilled to the burst: removed", n(), 0)
	os.WriteFile(filepath.Join(x.state, "flood", "old__x"), []byte("tokens=12000\nlast=1\n"), 0o660)
	x.h.Reap()
	eq(t, "an old bucket without rate and burst: the default's judge, full: removed", n(), 0)
	os.WriteFile(filepath.Join(x.state, "budgets", "run"), []byte("subject=run\nrate=0.001\nburst=5\n"), 0o640)
	x.dis(alice, "send "+bob+" -- three")
	x.h.Reap()
	eq(t, "a slow budget's spent bucket stays", n(), 1)
}

// At most 256 ask entries per address; one more: exit 5. Joining a pending ask is no new entry.
func TestAskCap(t *testing.T) {
	x := instRig(t)
	x.settings("wake_secs=30\n")
	var wg sync.WaitGroup
	held := make([]*bg, askCap)
	for i := range held {
		wg.Add(1)
		go func() {
			defer wg.Done()
			held[i] = x.start(x.adm(wHub), fmt.Sprintf("send %s -- x", inst(fmt.Sprintf("q%03d", i))), "")
		}()
	}
	wg.Wait()
	waitFor(t, 5*time.Second, "256 asks", func() bool { x.h.asks.mu.Lock(); defer x.h.asks.mu.Unlock(); return len(x.h.asks.m) == askCap })
	r := x.dis(wHub, "send "+inst("over")+" -- x")
	code(t, "the 257th: rate limited", r, 5)
	has(t, "(says so)", r.err, "asks waiting")
	j := x.start(x.adm(wHub), "send "+inst("q000")+" -- again", "")
	time.Sleep(50 * time.Millisecond)
	select {
	case <-j.done:
		t.Error("a sender joining a pending ask was refused")
	default:
	}
	x.dis(par, "admit q000")
	eq(t, "the joiner is answered with the ask", j.wait(t, 3*time.Second), 0)
	eq(t, "an ask ended frees a place: held, then the sender leaves", x.start(x.adm(wHub), "send "+inst("over")+" -- x", "").cancelAndCode(t), 6)
	for _, b := range held {
		b.cancel()
	}
}
