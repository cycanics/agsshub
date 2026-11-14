package hub

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// keyID: sha256 of the base64 key blob as text — the name of its .byfpr link.
func keyID(blob string) string {
	s := sha256.Sum256([]byte(blob))
	return hex.EncodeToString(s[:])
}

func keyBlob(pub ssh.PublicKey) string { return base64.StdEncoding.EncodeToString(pub.Marshal()) }

func (h *Hub) claimPath(fq, minter string) string {
	return filepath.Join(h.claims, minter, fsName(fq))
}

// claimFind: every claim record for fq, across minters, in minter order.
func (h *Hub) claimFind(fq string) []string {
	var out []string
	fn := fsName(fq)
	for _, m := range globNames(h.claims) {
		p := filepath.Join(h.claims, m, fn)
		if isFile(p) {
			out = append(out, p)
		}
	}
	return out
}

// claimLive: the record's expiry is in the future.
func (h *Hub) claimLive(path string) bool {
	e, ok := atoi64(recGet(path, "expires"))
	return ok && e > h.epoch()
}

// claimLiveElsewhere: split brain — a live claim for fq under a minter other than minter.
func (h *Hub) claimLiveElsewhere(fq, minter string) (string, bool) {
	for _, f := range h.claimFind(fq) {
		d := filepath.Base(filepath.Dir(f))
		if d == minter {
			continue
		}
		if h.claimLive(f) {
			return d, true
		}
	}
	return "", false
}

// keyBoundElsewhere: the key index is 1:1 across both indexes; the identity keyid is bound to, if
// its link is not rel (its own record).
func (h *Hub) keyBoundElsewhere(keyid, rel string) (string, bool) {
	for _, l := range []string{filepath.Join(h.byfpr, keyid), filepath.Join(h.ibyfpr, keyid)} {
		t, err := os.Readlink(l)
		if err != nil || !existsFollow(l) { // unbound, or a dangling link left by a crash
			continue
		}
		if t == rel {
			continue
		}
		return recGet(l, "fq"), true
	}
	return "", false
}

var errRegistryBusy = errors.New("registry busy")

// lockRegistry: one registry writer at a time. Goroutines of this daemon queue on a
// mutex; other processes (hub-mint) on the flock of claims/.lock.
func (h *Hub) lockRegistry() (func(), error) {
	h.regMu.Lock()
	l := filepath.Join(h.claims, ".lock")
	f, err := os.Open(l)
	if errors.Is(err, os.ErrNotExist) {
		if c, cerr := os.OpenFile(l, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640); cerr == nil {
			c.Close()
		}
		f, err = os.Open(l)
	}
	if err != nil {
		h.regMu.Unlock()
		return nil, errRegistryBusy
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) || time.Now().After(deadline) {
			f.Close()
			h.regMu.Unlock()
			return nil, errRegistryBusy
		}
		time.Sleep(10 * time.Millisecond)
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
		h.regMu.Unlock()
	}, nil
}

// wsMayClaimOrg: the orgs a workspace was enrolled for (hub-enroll), one per line.
func (h *Hub) wsMayClaimOrg(ws, org string) bool {
	b, err := os.ReadFile(filepath.Join(h.orgs, ws))
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l == org {
			return true
		}
	}
	return false
}

// --- instances: whether an address takes them, and the address behind one ------------------------

func (h *Hub) instCapable(addr string) bool {
	return recGet(h.claimPath(addr, OpMinter), "instances") == "1"
}

func (h *Hub) instRecord(fq string) string { return h.claimPath(fq, InstMinter) }

// instanceParentOK: the address behind an instance's claim record is live, takes instances, and is
// the generation (serial) whose holder registered it.
func (h *Hub) instanceParentOK(recPath string) bool {
	r, _ := readRec(recPath)
	p, ps := r["parent"], r["parent_serial"]
	if p == "" || ps == "" {
		return false
	}
	pr := h.claimPath(p, OpMinter)
	pv, _ := readRec(pr)
	if pv["instances"] != "1" || pv["serial"] != ps {
		return false
	}
	e, ok := atoi64(pv["expires"])
	return ok && e > h.epoch()
}

// instDrop: an instance's claim and key link go; its follow ends, its sessions are refused from their
// next command. Called with the registry locked.
func (h *Hub) instDrop(recPath string) {
	r, _ := readRec(recPath)
	k, fq := r["keyid"], r["fq"]
	link := filepath.Join(h.ibyfpr, k)
	if t, err := os.Readlink(link); err == nil && t == "../"+InstMinter+"/"+filepath.Base(recPath) {
		os.Remove(link)
	}
	os.Remove(recPath)
	if fq != "" {
		h.follows.nudge(fq)
		h.nudgeAddr(fq) // a claim change: its address's wake notice
	}
}

// --- authentication --------------------------------------------------------------------------------

// Admission is what a key logs in as: the identity, the serial of its claim, and the key id.
// Conn: the follow id `<conn>.<session>` — the ssh connection's id as the server logs it (`conn <id>:
// accepted …`) and the session channel's — unique per session even when channels share one connection
// (ControlMaster). Empty (a call outside the server): Dispatch draws one from the same counters.
type Admission struct{ FQ, Serial, KeyID, Conn string }

// admitHub: the offered key holds a live claim, through its key index.
func (h *Hub) admitHub(pub ssh.PublicKey) (Admission, bool) {
	blob := keyBlob(pub)
	keyid := keyID(blob)
	link := filepath.Join(h.byfpr, keyid)
	inst := false
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		link = filepath.Join(h.ibyfpr, keyid) // an instance's key
		inst = true
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return Admission{}, false // unknown key -> refused (no oracle)
		}
	}
	recPath, err := filepath.EvalSymlinks(link)
	if err != nil || !isFile(recPath) {
		return Admission{}, false
	}
	r, ok := readRec(recPath)
	if !ok || r["keyid"] != keyid { // the index must agree with the record
		return Admission{}, false
	}
	if inst {
		// An instance key admits only an instance's claim, and only while its address stands behind it.
		idir, err := filepath.EvalSymlinks(filepath.Join(h.claims, InstMinter))
		if err != nil || filepath.Dir(recPath) != idir || r["ws"] != InstMinter {
			return Admission{}, false
		}
		if parent, _, ok := instSplit(r["fq"]); !ok || parent != r["parent"] {
			return Admission{}, false
		}
		if !h.instanceParentOK(recPath) {
			return Admission{}, false
		}
	}
	exp, ok := atoi64(r["expires"])
	if !ok || exp <= h.epoch() { // expired -> refused (the reaper cleans)
		return Admission{}, false
	}
	if r["type"] != pub.Type() {
		return Admission{}, false
	}
	return Admission{FQ: r["fq"], Serial: r["serial"], KeyID: keyid}, true
}

// admitRegistrar: a workspace's registrar key, as enrolled by hub-enroll in the registrars'
// authorized_keys: the workspace is the last word of `command="/usr/local/lib/hub/registrar <ws>"`, a
// name nothing runs. Returns the workspace.
func (h *Hub) admitRegistrar(pub ssh.PublicKey) (string, bool) {
	b, err := os.ReadFile(h.cfg.Registrars)
	if err != nil {
		return "", false
	}
	want := pub.Marshal()
	for len(b) > 0 {
		k, _, opts, rest, err := ssh.ParseAuthorizedKey(b)
		if err != nil {
			break
		}
		b = rest
		if string(k.Marshal()) != string(want) {
			continue
		}
		for _, o := range opts {
			cmd, ok := strings.CutPrefix(o, "command=")
			if !ok {
				continue
			}
			cmd = strings.Trim(cmd, `"`)
			f := strings.Fields(cmd)
			if len(f) == 2 && filepath.Base(f[0]) == "registrar" && validWS(f[1]) {
				return f[1], true
			}
		}
	}
	return "", false
}
