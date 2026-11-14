package hub

// The mailbox lock: <mbox>/.lock, an flock held for one delivery into a mailbox or its removal at a
// time (a topic's fan-out, a store into an instance's mailbox, dropSubBox, dropTree, reject). Event ids
// and their order are the topic's (event.go); nothing here numbers anything.

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockBox: box's lock, held until the returned func is called; nil when the mailbox is gone or the lock
// cannot be had. It never makes the mailbox: one removed meanwhile stays removed. A lock had on a mailbox
// renamed away while it waited is not box's: it is let go, and box's taken again if there is one.
func lockBox(box string) func() {
	for tries := 0; tries < 8; tries++ {
		if !isDir(box) {
			return nil
		}
		p := filepath.Join(box, ".lock")
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
		if err != nil {
			return nil
		}
		for {
			if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); !errors.Is(err, unix.EINTR) {
				break
			}
		}
		if err != nil {
			f.Close()
			return nil
		}
		var held, now unix.Stat_t
		if unix.Fstat(int(f.Fd()), &held) == nil && unix.Stat(p, &now) == nil && held.Ino == now.Ino && held.Dev == now.Dev {
			return func() {
				unix.Flock(int(f.Fd()), unix.LOCK_UN)
				f.Close()
			}
		}
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}
	return nil
}

// dropSubBox removes a subscription's mailbox: under its lock, after the delivery in progress, renamed to a
// stage first, so no delivery finds it while it goes (the lock file goes with it: a delivery waiting on it
// finds the mailbox gone). A stage a crash leaves goes with the reaper.
func (h *Hub) dropSubBox(box string) {
	unlock := lockBox(box)
	gone := h.stageName(filepath.Dir(box))
	err := os.Rename(box, gone)
	if unlock != nil {
		unlock()
	}
	if err == nil {
		os.RemoveAll(gone)
	} else if !errors.Is(err, os.ErrNotExist) {
		os.RemoveAll(box)
	}
}
