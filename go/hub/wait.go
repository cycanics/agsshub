package hub

// Wait for pickup (docs/wake-live-event-id.md, Wait for pickup): `--wait <s>` holds a send after storing
// until the mailbox owner is first shown the message, or <s> passes. The waiter is in memory, tied to the
// connected sender, and registered before the link, so a pickup between the link and the wait counts.

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type waiter struct {
	picked chan struct{}
	gone   chan struct{} // closed when a reject takes the mailbox, with this message in it
}

type waitTable struct {
	mu sync.Mutex
	m  map[string]map[*waiter]struct{} // <mailbox dir>/<msgid> -> its waiters
}

func waitKey(box, mid string) string { return box + "/" + mid }

// waitAdd registers a waiter for mid in box.
func (h *Hub) waitAdd(box, mid string) *waiter {
	w := &waiter{picked: make(chan struct{}), gone: make(chan struct{})}
	k := waitKey(box, mid)
	h.waits.mu.Lock()
	if h.waits.m == nil {
		h.waits.m = map[string]map[*waiter]struct{}{}
	}
	if h.waits.m[k] == nil {
		h.waits.m[k] = map[*waiter]struct{}{}
	}
	h.waits.m[k][w] = struct{}{}
	h.waits.mu.Unlock()
	return w
}

// waitDrop forgets w: its sender stopped waiting.
func (h *Hub) waitDrop(box, mid string, w *waiter) {
	k := waitKey(box, mid)
	h.waits.mu.Lock()
	delete(h.waits.m[k], w)
	if len(h.waits.m[k]) == 0 {
		delete(h.waits.m, k)
	}
	h.waits.mu.Unlock()
}

// picked: a read by the mailbox owner delivered mid's body or payload (not --peek:header, not the ack): its waiters
// are answered. One map lookup when nobody waits.
func (h *Hub) picked(box, mid string) {
	k := waitKey(box, mid)
	h.waits.mu.Lock()
	ws := h.waits.m[k]
	delete(h.waits.m, k)
	h.waits.mu.Unlock()
	for w := range ws {
		close(w.picked)
	}
}

// waitsGone: box was rejected, its mail discarded: every sender waiting on a pickup in it ends with 3.
func (h *Hub) waitsGone(box string) {
	prefix := box + "/"
	h.waits.mu.Lock()
	defer h.waits.mu.Unlock()
	for k, ws := range h.waits.m {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		delete(h.waits.m, k)
		for w := range ws {
			close(w.gone)
		}
	}
}

var waitRE = regexp.MustCompile(`^([0-9]{1,3})(\.([0-9]{1,3}))?$`)

// parseWait: --wait <s>, decimal seconds with up to 3 decimals, 0.001 to 600; false when not.
func parseWait(v string) (time.Duration, bool) {
	m := waitRE.FindStringSubmatch(v)
	if m == nil {
		return 0, false
	}
	i, _ := strconv.ParseInt(m[1], 10, 64)
	f, _ := strconv.ParseInt((m[3] + "000")[:3], 10, 64)
	ms := i*1000 + f
	if ms < 1 || ms > 600000 {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}
