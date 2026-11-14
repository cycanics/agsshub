package hub

// Bags (README, "Bags"): one message carrying a tar, stored exactly as received as its message file's
// payload (msgfile.go). The hub judges only completeness and its own disk.

import (
	"errors"
	"io"
	"os"
	"sort"
	"syscall"
)

const errTarCut = "dispatch: tar ended before its end marker"

// hubIO: an error from the system (a read of the temp file, a write, a link), not from what the sender wrote.
func hubIO(err error) bool {
	var e syscall.Errno
	return errors.As(err, &e)
}

// tarErr: what a tar.Reader's error means for a bag. Cut: 4 · the system's: 1 · else not a tar: 2 (ErrHeader,
// and what the reader says of a tar that parses but is malformed).
func tarErr(err error) (int, string) {
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return 4, errTarCut
	case hubIO(err):
		return 1, errStore
	}
	return 2, "dispatch: bad tar"
}

// globNamesAll lists every entry of dir, hidden ones too.
func globNamesAll(dir string) []string {
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer f.Close()
	names, _ := f.Readdirnames(-1)
	sort.Strings(names)
	return names
}
