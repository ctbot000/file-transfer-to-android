package server

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type direction string

const (
	toPhone   direction = "send"
	fromPhone direction = "receive"
)

type transferState string

const (
	transferActive transferState = "active"
	transferDone   transferState = "done"
	transferFailed transferState = "failed"
)

// transfer is one file moving between the computer and a phone. A file
// download can span several requests: download managers split large files
// into parallel range requests and resume interrupted ones with a range
// request. Requests with the same key are merged into one transfer.
type transfer struct {
	owner *transfers
	id    int64
	key   string
	name  string
	dir   direction
	peer  string
	total int64 // -1 when unknown
	base  int64 // bytes the client already had when the first request began
	moved atomic.Int64

	// Guarded by owner.mu.
	parts   int
	broken  bool // a request ended before moving everything it promised
	state   transferState
	started time.Time
	ended   time.Time
}

func (t *transfer) add(n int64) {
	t.moved.Add(n)
	t.owner.changed.Store(true)
}

func (t *transfer) progress() int64 {
	p := t.base + t.moved.Load()
	if t.total >= 0 && p > t.total {
		return t.total
	}
	return p
}

type transferJSON struct {
	ID        int64         `json:"id"`
	Name      string        `json:"name"`
	Direction direction     `json:"direction"`
	Peer      string        `json:"peer"`
	Bytes     int64         `json:"bytes"`
	Total     int64         `json:"total"`
	State     transferState `json:"state"`
	Started   time.Time     `json:"started"`
	Ended     time.Time     `json:"ended,omitzero"`
}

// transfers records recent transfers for the desktop page's activity list.
type transfers struct {
	mu      sync.Mutex
	list    []*transfer // oldest first
	last    int64
	changed atomic.Bool // set on any change; cleared by whoever publishes it
}

// keepFinished is how many finished transfers stay in the activity list.
const keepFinished = 50

// begin registers one request of a transfer. A request joins the most
// recent transfer with the same key if that one is still running, or if
// it failed and this request resumes it (offset > 0).
func (ts *transfers) begin(key, name string, dir direction, peer string, total, offset int64) *transfer {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.changed.Store(true)
	if key != "" {
		for i := len(ts.list) - 1; i >= 0; i-- {
			t := ts.list[i]
			if t.key != key {
				continue
			}
			if t.state == transferActive || t.state == transferFailed && offset > 0 {
				t.parts++
				t.state = transferActive
				t.ended = time.Time{}
				return t
			}
			break
		}
	}
	ts.last++
	t := &transfer{
		owner:   ts,
		id:      ts.last,
		key:     key,
		name:    name,
		dir:     dir,
		peer:    peer,
		total:   total,
		base:    offset,
		parts:   1,
		state:   transferActive,
		started: time.Now(),
	}
	ts.list = append(ts.list, t)
	ts.trim()
	return t
}

// end finishes one request of t; ok reports whether the request moved
// everything it set out to. When it was the last request in flight, the
// transfer is settled and returned; otherwise end returns nil.
//
// A transfer made of one request is done when that request is ok. A file
// download (one with a key) is done once the file is covered, however many
// requests that took, and failed if a request broke off before that. One
// whose requests all succeeded without covering the file was a client
// reading part of it, such as a player seeking, not a download: it is
// dropped from the list rather than reported as failed.
func (ts *transfers) end(t *transfer, ok bool) *transferJSON {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.changed.Store(true)
	t.parts--
	t.broken = t.broken || !ok
	if t.parts > 0 {
		return nil
	}
	switch {
	case t.key == "" && ok, t.key != "" && t.progress() >= t.total:
		t.state = transferDone
	case t.key != "" && !t.broken:
		ts.list = slices.DeleteFunc(ts.list, func(x *transfer) bool { return x == t })
		return nil
	default:
		t.state = transferFailed
	}
	t.ended = time.Now()
	snap := t.snapshot()
	return &snap
}

// trim drops the oldest finished transfers beyond keepFinished.
func (ts *transfers) trim() {
	finished := 0
	for _, t := range ts.list {
		if t.state != transferActive {
			finished++
		}
	}
	kept := ts.list[:0]
	for _, t := range ts.list {
		if t.state != transferActive && finished > keepFinished {
			finished--
			continue
		}
		kept = append(kept, t)
	}
	clear(ts.list[len(kept):])
	ts.list = kept
}

// snapshot lists the transfers, newest first.
func (ts *transfers) snapshot() []transferJSON {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]transferJSON, 0, len(ts.list))
	for i := len(ts.list) - 1; i >= 0; i-- {
		out = append(out, ts.list[i].snapshot())
	}
	return out
}

// snapshot copies t for JSON; the caller holds owner.mu.
func (t *transfer) snapshot() transferJSON {
	return transferJSON{
		ID:        t.id,
		Name:      t.name,
		Direction: t.dir,
		Peer:      t.peer,
		Bytes:     t.progress(),
		Total:     t.total,
		State:     t.state,
		Started:   t.started,
		Ended:     t.ended,
	}
}

// trackedResponse counts a download's body bytes as they are written. The
// transfer is only registered once the response turns out to carry a body,
// so HEAD requests and "304 Not Modified" answers leave no trace.
type trackedResponse struct {
	http.ResponseWriter
	ts   *transfers
	get  bool
	key  string
	name string
	peer string
	size int64

	status   int
	promised int64 // the response's Content-Length, or -1
	written  int64
	failed   bool // a write failed: the client went away
	t        *transfer
}

func (tr *trackedResponse) WriteHeader(code int) {
	if tr.status == 0 {
		tr.status = code
		tr.promised = -1
		if n, err := strconv.ParseInt(tr.Header().Get("Content-Length"), 10, 64); err == nil {
			tr.promised = n
		}
		if tr.get && (code == http.StatusOK || code == http.StatusPartialContent) {
			var offset int64
			if code == http.StatusPartialContent {
				offset = rangeStart(tr.Header().Get("Content-Range"))
			}
			tr.t = tr.ts.begin(tr.key, tr.name, toPhone, tr.peer, tr.size, offset)
		}
	}
	tr.ResponseWriter.WriteHeader(code)
}

func (tr *trackedResponse) Write(p []byte) (int, error) {
	if tr.status == 0 {
		tr.WriteHeader(http.StatusOK)
	}
	n, err := tr.ResponseWriter.Write(p)
	tr.written += int64(n)
	tr.failed = tr.failed || err != nil
	if tr.t != nil {
		tr.t.add(int64(n))
	}
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (tr *trackedResponse) Unwrap() http.ResponseWriter { return tr.ResponseWriter }

// end settles this request's part of the transfer, if it had one, and
// returns the transfer when this settled it.
func (tr *trackedResponse) end() *transferJSON {
	if tr.t == nil {
		return nil
	}
	ok := !tr.failed && (tr.promised < 0 || tr.written == tr.promised)
	return tr.ts.end(tr.t, ok)
}

// rangeStart parses the first byte position out of a Content-Range value
// such as "bytes 100-199/1000".
func rangeStart(contentRange string) int64 {
	spec, ok := strings.CutPrefix(contentRange, "bytes ")
	if !ok {
		return 0
	}
	first, _, _ := strings.Cut(spec, "-")
	n, err := strconv.ParseInt(first, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
