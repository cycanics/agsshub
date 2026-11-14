package hub

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Constants. Those the operator tools read too are in lib/common.sh (TestTablesMatchCommonSh).
const (
	TombstoneSecs = 600              // a *different* key must wait this long to reclaim a released name
	FileMaxBytes  = 64 * 1024 * 1024 // per-file spool cap
	MsgTTLSecs    = 7 * 24 * 3600    // uncollected message GC age (reaper)
	FloodRateMT   = 500              // default refill, millitokens/s = 1 message / 2 s
	FloodBurstMT  = 12000            // default bucket cap = 12 messages
	TopicRate     = 10               // default publish rate per topic, messages/s
	TopicBurst    = 100              // default publish burst per topic, messages
	SubTTLSecs    = 30 * 24 * 3600   // a subscription lives this long unless renewed by re-`sub`
	SubTTLMax     = 90 * 24 * 3600   //
	OpTTLSecs     = 10 * 365 * 24 * 3600
	OpMinter      = "operator"        // claims/<minter>/: a workspace id, or this for hub-mint
	InstMinter    = "instance"        // ... or this for an instance its address's holder registered
	InstTTLSecs   = 3600              // an instance's claim lives this long unless renewed (60 s .. 1 day)
	FieldMaxBytes = 4096              // the whole header block a client may send
	BagMaxBytes   = 256 * 1024 * 1024 // a bag's stored bytes
	msgExt        = "msg"             // message filename extension; msg_id is the basename
	followPoll    = 30 * time.Second  // a follow re-checks this often even without a nudge
	liveAcceptDef = "5"               // live_accept_secs default
	wakeSecsDef   = "60"              // wake_secs default: how long an ask entry holds its senders
	liveRetryDef  = "5"               // live_retry_secs default: the retry-after hint
)

// Config is where the hub keeps its state (README, "Claim registry, on disk"; docs/topics.md, "On disk").
type Config struct {
	Root       string // /var/lib/hub
	Lib        string // /usr/local/lib/hub (backend plugins: backends/<type>/send; help.json, help.txt)
	Registrars string // /etc/hub/registrars/authorized_keys (hub-enroll)
	Logf       func(format string, args ...any)
}

// Hub is the daemon's state: the paths of the data dir, and the in-process registries.
type Hub struct {
	cfg                                         Config
	claims, inbox, aliases, flood, budgets      string
	byfpr, ibyfpr, tmpd, tomb, seriald, journal string
	topics, subs, settings, orgs, described     string

	follows  *followRegistry
	live     *exchangeTable
	admMu    keyedMutex // the admission lock, per addr+inst (docs/identities.md, Admitting and rejecting)
	asks     *askTable
	waits    waitTable // --wait's waiters
	trans    *transientTable
	wsum     wakeSum
	tsMu     sync.Mutex
	watching atomic.Bool
	ts       map[string]*topicState // per topic: its lock, record and done (docs/topics.md, Event ids)
	floodMu  keyedMutex
	regMu    sync.Mutex // registry writers in this process (the flock covers other processes)
	jourMu   sync.Mutex
	bootID   string     // the kernel's boot id, read at start: the record's says which boot wrote it
	arrMu    keyedMutex // per mailbox: a message's arrival stamp and its link, one store at a time
	arrLast  atomic.Int64

	now func() time.Time

	Verbose bool // log what the watcher sees
}

// New makes a hub over cfg.
func New(cfg Config) *Hub {
	if cfg.Root == "" {
		cfg.Root = "/var/lib/hub"
	}
	if cfg.Lib == "" {
		cfg.Lib = "/usr/local/lib/hub"
	}
	if cfg.Registrars == "" {
		cfg.Registrars = "/etc/hub/registrars/authorized_keys"
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	h := &Hub{cfg: cfg, now: time.Now}
	r := cfg.Root
	h.claims = filepath.Join(r, "claims")
	h.inbox = filepath.Join(r, "inbox")
	h.aliases = filepath.Join(r, "aliases")
	h.flood = filepath.Join(r, "flood")
	h.budgets = filepath.Join(r, "budgets")
	h.topics = filepath.Join(r, "topics")
	h.subs = filepath.Join(r, "subs")
	h.described = filepath.Join(r, "described")
	h.settings = filepath.Join(r, "settings")
	h.orgs = filepath.Join(r, "orgs")
	h.byfpr = filepath.Join(h.claims, ".byfpr")
	h.ibyfpr = filepath.Join(h.claims, ".ibyfpr")
	h.tmpd = filepath.Join(h.claims, ".tmp")
	h.tomb = filepath.Join(h.claims, ".tomb")
	h.seriald = filepath.Join(h.claims, ".serial")
	h.journal = filepath.Join(h.claims, ".journal")
	h.follows = newFollowRegistry()
	h.live = newExchangeTable()
	h.asks = &askTable{m: map[string]*ask{}}
	h.trans = &transientTable{m: map[string]map[string]*transient{}}
	h.ts = map[string]*topicState{}
	h.wsum.m = map[string]map[string]map[string]mailInfo{}
	h.bootID = readBootID(cfg.Logf)
	return h
}

func (h *Hub) logf(format string, args ...any) { h.cfg.Logf(format, args...) }

func (h *Hub) epoch() int64 { return h.now().Unix() }

// mbox: an identity's mailbox, inbox/<fs name>.
func (h *Hub) mbox(id string) string { return filepath.Join(h.inbox, fsName(id)) }

// --- key=value records -----------------------------------------------------------------------

// recPairs reads a record as bash's `while IFS='=' read -r k v` does: one pair per newline-ended
// line, split at the first '='; a last line without its newline is not read.
func recPairs(path string) [][2]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out [][2]string
	for len(b) > 0 {
		i := strings.IndexByte(string(b), '\n')
		if i < 0 {
			break
		}
		line := string(b[:i])
		b = b[i+1:]
		k, v, _ := strings.Cut(line, "=")
		out = append(out, [2]string{k, v})
	}
	return out
}

// recGet: the first value of key in the record at path, empty when absent.
func recGet(path, key string) string {
	for _, kv := range recPairs(path) {
		if kv[0] == key {
			return kv[1]
		}
	}
	return ""
}

// rec is a parsed record for repeated lookups.
type rec map[string]string

func readRec(path string) (rec, bool) {
	pairs := recPairs(path)
	if pairs == nil {
		if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
			return nil, false
		}
	}
	r := rec{}
	for _, kv := range pairs {
		if _, seen := r[kv[0]]; !seen {
			r[kv[0]] = kv[1]
		}
	}
	return r, true
}

// isFile is bash's [ -f path ]: a regular file, symlinks followed.
func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

// existsFollow is bash's [ -e path ]: symlinks followed.
func existsFollow(path string) bool { _, err := os.Stat(path); return err == nil }

// globNames lists the non-hidden entries of dir in byte order, as an LC_ALL=C glob does.
func globNames(dir string) []string {
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	names, _ := f.Readdirnames(-1)
	f.Close()
	out := names[:0]
	for _, n := range names {
		if !strings.HasPrefix(n, ".") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func atoi64(s string) (int64, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

// --- time ----------------------------------------------------------------------------------------

func isoTime(epoch int64) string { return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z") }

// mintMsgid: a hub-minted message id, sortable (time-prefixed), random tail for uniqueness.
func (h *Hub) mintMsgid() string {
	return fmt.Sprintf("%s-%06d", h.now().UTC().Format("20060102T150405"), randUint64()%1000000)
}

func randUint64() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return binary.LittleEndian.Uint64(b[:])
}

func randHex(nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}

// --- the journal: claims/.journal, best effort ---------------------------------------------------

func (h *Hub) journalf(format string, args ...any) {
	h.jourMu.Lock()
	defer h.jourMu.Unlock()
	f, err := os.OpenFile(h.journal, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", isoTime(h.epoch()), fmt.Sprintf(format, args...))
}

// --- keyed mutex: one lock per key, created on demand -------------------------------------------

// arrival: a message's arrival stamp, set as its file's mtime as it lands in a mailbox (the listing order).
// Strictly rising in this process: a wall clock stepped back while the hub runs does not reorder arrivals.
func (h *Hub) arrival() time.Time {
	for {
		last, now := h.arrLast.Load(), time.Now().UnixNano()
		if now <= last {
			now = last + 1
		}
		if h.arrLast.CompareAndSwap(last, now) {
			return time.Unix(0, now)
		}
	}
}

// stamp sets the arrival stamp on f, the file about to land.
func (h *Hub) stamp(f string) error {
	t := h.arrival()
	return os.Chtimes(f, t, t)
}

type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyedEntry
}

type keyedEntry struct {
	sync.Mutex
	n int
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedEntry{}
	}
	e := k.m[key]
	if e == nil {
		e = &keyedEntry{}
		k.m[key] = e
	}
	e.n++
	k.mu.Unlock()
	e.Lock()
	return func() {
		e.Unlock()
		k.mu.Lock()
		if e.n--; e.n == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}
