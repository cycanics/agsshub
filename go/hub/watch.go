package hub

// The daemon observes the operator tools' files instead of being told: hub-mint (add, --replace,
// rm, --no-instances), or anything else that changes a claim record or a
// subscription link wakes the followers it concerns, which then re-check that their mailbox is
// still theirs. What the operator writes otherwise (budgets, topics, aliases, settings, orgs,
// registrar keys) is read where it is used, every time.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const watchMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_TO | unix.IN_MOVED_FROM |
	unix.IN_CLOSE_WRITE | unix.IN_ATTRIB | unix.IN_ONLYDIR

// Watch follows changes under claims/ and subs/ until ctx ends. Without inotify it returns its
// error; the followers' 30 s poll then remains the only re-check.
func (h *Hub) Watch(ctx context.Context) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	h.watching.Store(true) // the reaper's sweep need not drop the wake summaries
	defer h.watching.Store(false)
	stop, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK) // wakes the poll when ctx ends
	if err != nil {
		return err
	}
	woke := make(chan struct{})
	cancelWake := context.AfterFunc(ctx, func() { unix.Write(stop, []byte{1, 0, 0, 0, 0, 0, 0, 0}); close(woke) })
	defer func() {
		if !cancelWake() {
			<-woke // the write has happened before the fd closes
		}
		unix.Close(stop)
	}()
	w := &watcher{h: h, fd: fd, dirs: map[int32]string{}, verbose: h.Verbose}
	w.scan()
	last := time.Now()
	buf := make([]byte, 64*1024)
	for ctx.Err() == nil {
		if time.Since(last) > 30*time.Second { // directories that appeared since (a fresh install)
			w.scan()
			last = time.Now()
		}
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(stop), Events: unix.POLLIN}}
		n, err := unix.Poll(pfd, 30000)
		if err != nil && err != unix.EINTR {
			return err
		}
		if n <= 0 || pfd[0].Revents&unix.POLLIN == 0 {
			continue
		}
		nr, err := unix.Read(fd, buf)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return err
		}
		w.events(buf[:nr])
	}
	return nil
}

type watcher struct {
	h       *Hub
	fd      int
	verbose bool
	dirs    map[int32]string // watch descriptor -> directory (only the Watch goroutine uses it)
}

// scan adds a watch for claims/, each minter's directory, and subs/ (adding one twice is harmless).
func (w *watcher) scan() {
	h := w.h
	add := func(d string) {
		wd, err := unix.InotifyAddWatch(w.fd, d, watchMask)
		if err != nil {
			if !errors.Is(err, unix.ENOENT) {
				h.logf("watch %s: %v", d, err)
			}
			return
		}
		if w.dirs[int32(wd)] != d && w.verbose {
			h.logf("watch %s: wd %d", d, wd)
		}
		w.dirs[int32(wd)] = d
	}
	add(h.claims)
	for _, m := range globNames(h.claims) {
		if d := filepath.Join(h.claims, m); isDir(d) {
			add(d)
		}
	}
	add(h.subs)
}

func (w *watcher) events(b []byte) {
	h := w.h
	for len(b) >= unix.SizeofInotifyEvent {
		ev := (*unix.InotifyEvent)(unsafe.Pointer(&b[0]))
		end := unix.SizeofInotifyEvent + int(ev.Len)
		if end > len(b) {
			return
		}
		name := strings.TrimRight(string(b[unix.SizeofInotifyEvent:end]), "\x00")
		b = b[end:]
		if w.verbose {
			h.logf("watch event wd %d (%s) %q mask %#x", ev.Wd, w.dirs[ev.Wd], name, ev.Mask)
		}
		if ev.Mask&unix.IN_Q_OVERFLOW != 0 { // events were lost: everyone re-checks
			h.sumDropAll()
			h.follows.nudgeMatching(func(string) bool { return true })
			w.scan()
			continue
		}
		dir := w.dirs[ev.Wd]
		if ev.Mask&unix.IN_IGNORED != 0 {
			delete(w.dirs, ev.Wd)
		}
		if dir == "" || name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		id := strings.ReplaceAll(name, "~", "/")
		switch {
		case dir == h.claims: // a minter's directory came or went
			if ev.Mask&unix.IN_ISDIR != 0 {
				w.scan()
			}
		case dir == h.subs:
			h.follows.nudge(id)
		default:
			h.follows.nudge(id)
			if filepath.Base(dir) == OpMinter { // an address: its instances' sessions depend on it
				h.sumDrop(id) // and its instances may have ended (hub-mint rm, --no-instances)
				h.follows.nudgeMatching(func(f string) bool {
					p, _, ok := instSplit(f)
					return ok && p == id
				})
			}
		}
	}
}
