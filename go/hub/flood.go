package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	rateRE    = regexp.MustCompile(`^([0-9]{1,6})(\.([0-9]{1,3}))?$`)
	burstRE   = regexp.MustCompile(`^[1-9][0-9]{0,6}$`)
	settingRE = regexp.MustCompile(`^([0-9]{1,4})(\.([0-9]{1,3}))?$`)
	topicNumR = regexp.MustCompile(`^[0-9]+$`)
)

// rateMT: millitokens per second for a decimal rate with up to 3 decimals; false for 0 or malformed.
func rateMT(s string) (int64, bool) {
	m := rateRE.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	i, _ := strconv.ParseInt(m[1], 10, 64)
	f := (m[3] + "000")[:3]
	fv, _ := strconv.ParseInt(f, 10, 64)
	v := i*1000 + fv
	return v, v > 0
}

// mtRate: the decimal rate of a millitoken rate.
func mtRate(mt int64) string {
	i := mt / 1000
	f := strings.TrimRight(fmt.Sprintf("%03d", mt%1000), "0")
	if f == "" {
		return strconv.FormatInt(i, 10)
	}
	return fmt.Sprintf("%d.%s", i, f)
}

// budgetFor: the sender's own budget, else (an instance) its address's, else its role's, else the
// default. The most specific budget applies whole; a field it leaves out, or a malformed one, is
// the default.
func (h *Hub) budgetFor(sender string) (rate, burst int64) {
	files := []string{filepath.Join(h.budgets, fsName(sender))}
	if strings.Contains(sender, "+") {
		if parent, _, ok := instSplit(sender); ok {
			files = append(files, filepath.Join(h.budgets, fsName(parent)))
		}
	}
	files = append(files, filepath.Join(h.budgets, idRole(sender)))
	for _, f := range files {
		if !isFile(f) {
			continue
		}
		r, _ := readRec(f)
		rate, ok := rateMT(r["rate"])
		if !ok {
			rate = FloodRateMT
		}
		burst = FloodBurstMT
		if burstRE.MatchString(r["burst"]) {
			b, _ := strconv.ParseInt(r["burst"], 10, 64)
			burst = b * 1000
		}
		return rate, burst
	}
	return FloodRateMT, FloodBurstMT
}

func (h *Hub) budgetShow(sender string) string {
	r, b := h.budgetFor(sender)
	return fmt.Sprintf("%s/s, burst %d", mtRate(r), b/1000)
}

// floodTake consumes one token from the from->to bucket (flood/<from>__<to>, '/' -> '_'), sized by
// the sender's budget unless rate/burst are given. The bucket is a file, with
// its integer-second refill, so state carries over and `rm -f flood/*` still resets it; a mutex per
// bucket makes the read-modify-write atomic between goroutines.
func (h *Hub) floodTake(from, to string, rateBurst ...int64) bool {
	var rate, burst int64
	if len(rateBurst) == 2 {
		rate, burst = rateBurst[0], rateBurst[1]
	} else {
		rate, burst = h.budgetFor(from)
	}
	key := strings.ReplaceAll(from, "/", "_") + "__" + strings.ReplaceAll(to, "/", "_")
	unlock := h.floodMu.lock(key)
	defer unlock()
	os.MkdirAll(h.flood, 0o2770)
	f := filepath.Join(h.flood, key)
	now := h.epoch()
	r, _ := readRec(f)
	tok, ok := atoi64(r["tokens"])
	if r["tokens"] == "" || !ok {
		tok = burst
	}
	last, ok := atoi64(r["last"])
	if r["last"] == "" || !ok {
		last = now
	}
	tok += (now - last) * rate
	if tok > burst {
		tok = burst
	}
	took := tok >= 1000
	if took {
		tok -= 1000
	}
	os.WriteFile(f, []byte(fmt.Sprintf("tokens=%d\nlast=%d\nrate=%d\nburst=%d\n", tok, now, rate, burst)), 0o660)
	return took
}

// reapFlood removes the buckets that are full again (refilled to their burst): an idle bucket is the
// state a missing one stands for. A bucket without its rate and burst is judged by the default budget.
func (h *Hub) reapFlood(now int64) {
	for _, key := range globNames(h.flood) {
		unlock := h.floodMu.lock(key)
		f := filepath.Join(h.flood, key)
		r, ok := readRec(f)
		tok, okT := atoi64(r["tokens"])
		last, okL := atoi64(r["last"])
		rate, okR := atoi64(r["rate"])
		burst, okB := atoi64(r["burst"])
		if !okR || !okB {
			rate, burst = FloodRateMT, FloodBurstMT
		}
		if ok && (!okT || !okL || tok+(now-last)*rate >= burst) {
			os.Remove(f)
		}
		unlock()
	}
}

// --- operator settings: /var/lib/hub/settings ---------------------------------------------------

// settingSecs: the operator's <key> (decimal seconds, up to 3 decimals, 0.001 up to maxMS), else def.
// Returns the value as written (it is quoted in messages) and as a duration.
func (h *Hub) settingSecs(key, def string, maxMS int64) (string, time.Duration) {
	v := recGet(h.settings, key)
	if m := settingRE.FindStringSubmatch(v); m != nil {
		i, _ := strconv.ParseInt(m[1], 10, 64)
		f := (m[3] + "000")[:3]
		fv, _ := strconv.ParseInt(f, 10, 64)
		ms := i*1000 + fv
		if ms >= 1 && ms <= maxMS {
			return v, time.Duration(ms) * time.Millisecond
		}
	}
	d, _ := strconv.ParseInt(def, 10, 64)
	return def, time.Duration(d) * time.Second
}

func (h *Hub) liveAcceptSecs() (string, time.Duration) {
	return h.settingSecs("live_accept_secs", liveAcceptDef, 60000)
}

func (h *Hub) wakeSecs() (string, time.Duration) {
	return h.settingSecs("wake_secs", wakeSecsDef, 600000)
}
