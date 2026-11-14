package hub

// The reaper, run every minute: expire claims (tombstoning the key),
// instances at expiry or once their address no longer stands behind them, subscriptions, uncollected
// mail, and what a killed session left.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// Reap runs one sweep.
func (h *Hub) Reap() {
	now := h.epoch()
	if !h.watching.Load() { // without inotify an operator's change goes unseen: the summaries are rebuilt from disk
		h.sumDropAll()
	}
	if unlock, err := h.lockRegistry(); err == nil {
		h.reapClaims(now)
		h.reapDescribed(now)
		unlock()
	}
	h.reapFlood(now)
	for _, n := range globNames(h.tomb) {
		tp := filepath.Join(h.tomb, n)
		if !isFile(tp) {
			continue
		}
		u := recGet(tp, "until")
		if v, ok := atoi64(u); u == "" || ok && v < now {
			os.Remove(tp)
		}
	}
	// subscriptions: expired ones go, their mailboxes with them; a link whose record is gone too
	for _, n := range globNames(h.subs) {
		l := filepath.Join(h.subs, n)
		if fi, err := os.Lstat(l); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		rp, err := filepath.EvalSymlinks(l)
		if err != nil || !isFile(rp) {
			os.Remove(l)
			h.dropSubBox(filepath.Join(h.inbox, n))
			h.follows.nudge(strings.ReplaceAll(n, "~", "/"))
			continue
		}
		e, ok := atoi64(recGet(rp, "expires"))
		if ok && e < now {
			sub := recGet(rp, "sub")
			os.Remove(rp)
			os.Remove(l)
			h.dropSubBox(filepath.Join(h.inbox, n))
			h.journalf("reap %s (subscription expired)", sub)
			h.follows.nudge(sub)
		}
	}
	// decode temp files and publication stages left by a session that died mid-body or mid-fan-out
	for _, n := range globNamesAll(h.inbox) {
		if strings.HasPrefix(n, ".chunked.") || strings.HasPrefix(n, ".stage.") {
			olderThan(filepath.Join(h.inbox, n), now, 3600, true)
		}
	}
	// uncollected mail, damaged files, stale stages
	for _, box := range globNames(h.inbox) {
		bd := filepath.Join(h.inbox, box)
		if !isDir(bd) {
			continue
		}
		h.reapMailbox(box, bd, now)
	}
}

// reapMailbox sweeps one mailbox; a panic is logged and ends only this mailbox's sweep.
func (h *Hub) reapMailbox(box, bd string, now int64) {
	defer func() {
		if r := recover(); r != nil {
			h.logf("reaper: panic in mailbox %s: %v\n%s", box, r, debug.Stack())
			h.journalf("reap: panic in mailbox %s: %v", box, r)
		}
	}()
	var old []string
	for _, n := range globNamesAll(bd) {
		p := filepath.Join(bd, n)
		switch {
		case strings.HasPrefix(n, ".stage."):
			olderThan(p, now, 3600, true)
		case n == damagedDir: // set aside by a content read: kept for MSG_TTL, for the operator
			for _, d := range globNamesAll(p) {
				olderThan(filepath.Join(p, d), now, MsgTTLSecs, false)
			}
		case strings.HasPrefix(n, "."):
		case strings.HasSuffix(n, "."+msgExt):
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && now-fi.ModTime().Unix() > MsgTTLSecs {
				old = append(old, strings.TrimSuffix(n, "."+msgExt))
			}
		}
	}
	h.expireMail(strings.ReplaceAll(box, "~", "/"), bd, old)
}

// reapClaims: claims under every minter (a lapsed persistent claim stays, for renewal), with the
// registry locked, so a renewal cannot land between reading an expiry and removing the claim.
func (h *Hub) reapClaims(now int64) {
	for _, m := range globNames(h.claims) {
		dir := filepath.Join(h.claims, m)
		for _, n := range globNames(dir) {
			rp := filepath.Join(dir, n)
			if !isFile(rp) {
				continue
			}
			r, _ := readRec(rp)
			exp, expOK := atoi64(r["expires"])
			if m == InstMinter {
				// an instance: gone at expiry (no tombstone: its address registers the next key at
				// will), or as soon as its address no longer stands behind it
				if (r["expires"] != "" && expOK && exp < now) || !h.instanceParentOK(rp) {
					h.journalf("reap %s (instance, expired or orphaned)", r["fq"])
					h.instDrop(rp)
				}
				continue
			}
			if r["expires"] == "" || !expOK || exp >= now || r["mode"] == "persistent" {
				continue
			}
			writeSmall(filepath.Join(h.tomb, r["ws"]+"__"+n), fmt.Sprintf("keyid=%s\nuntil=%d\n", r["keyid"], now+TombstoneSecs))
			os.Remove(filepath.Join(h.byfpr, r["keyid"]))
			os.Remove(rp)
			h.journalf("reap %s (expired, minted by %s)", r["fq"], r["ws"])
			h.follows.nudge(r["fq"]) // a held follow of it ends now
		}
	}
}

// olderThan removes p when its mtime is more than age seconds before now.
func olderThan(p string, now, age int64, recursive bool) {
	fi, err := os.Lstat(p)
	if err != nil || now-fi.ModTime().Unix() <= age {
		return
	}
	if recursive {
		os.RemoveAll(p)
	} else {
		os.Remove(p)
	}
}

// RunReaper sweeps every interval until ctx ends.
func (h *Hub) RunReaper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.Reap()
		}
	}
}
