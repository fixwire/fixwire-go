package fixwire

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The kinds of data, as the protocol's rate limits name them.
const (
	categoryError    = "error"
	categoryLog      = "log"
	categorySpan     = "span"
	categorySession  = "session"
	categoryCheckIn  = "check_in"
	categoryFeedback = "feedback"
)

// request is one request to Fixwire, kept until it is sent or dropped.
type request struct {
	path, contentType, category string
	body                        []byte
	attempts                    int
}

// maxAttempts bounds the sends of one request; maxWait is the longest a
// rate-limited request waits before it is dropped; maxPause bounds the
// seconds an answer may pause sending for.
const (
	maxAttempts = 4
	maxWait     = 5 * time.Minute
	maxPause    = 24 * time.Hour
)

// backoffUnit is the first retry's wait, halved (tests shorten it).
var backoffUnit = time.Second

// debugLog writes the Debug lines to stderr itself: through the log
// package they could reach a slog handler of the SDK's and be captured.
var debugLog = log.New(os.Stderr, "", log.LstdFlags)

// transport sends requests one at a time from a bounded queue, and backs
// off where Fixwire says to (Fixwire-Rate-Limits, Retry-After).
type transport struct {
	dsn    DSN
	client *http.Client
	debug  bool

	queue  chan *request
	stop   chan struct{}
	done   chan struct{}
	ctx    context.Context // ends the request in flight on close
	cancel context.CancelFunc
	zw     *gzip.Writer // the sending goroutine's, reused

	mu      sync.Mutex
	until   map[string]time.Time // category ("" for all) → paused until
	pending int
	waiting map[*request]*time.Timer // requests to send again later
	closed  bool
	idle    *sync.Cond
}

func newTransport(dsn DSN, opts Options) *transport {
	// The SDK's own copy of the client never follows a redirect: the key in
	// Authorization goes to the DSN's host only.
	client := *opts.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t := &transport{
		dsn: dsn, client: &client, debug: opts.Debug,
		queue: make(chan *request, opts.MaxQueue), stop: make(chan struct{}), done: make(chan struct{}),
		until: map[string]time.Time{}, waiting: map[*request]*time.Timer{},
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	t.idle = sync.NewCond(&t.mu)
	go t.run()
	return t
}

func (t *transport) logf(format string, args ...any) {
	if t.debug {
		debugLog.Printf("fixwire: "+format, args...)
	}
}

// send queues a request; false when the queue is full or closed.
func (t *transport) send(r *request) bool {
	t.mu.Lock()
	ok := t.put(r)
	if ok {
		t.pending++
	}
	t.mu.Unlock()
	if !ok {
		t.logf("queue full, dropping a %s request", r.category)
	}
	return ok
}

// put queues r, with t.mu held; false when the queue is full or closed.
func (t *transport) put(r *request) bool {
	if t.closed {
		return false
	}
	select {
	case t.queue <- r:
		return true
	default:
		return false
	}
}

func (t *transport) finish() {
	t.mu.Lock()
	t.pending--
	if t.pending <= 0 {
		t.pending = 0
		t.idle.Broadcast()
	}
	t.mu.Unlock()
}

// later puts a request back after d. As many wait as the queue holds, so
// that an outage costs bounded memory; the rest are dropped.
func (t *transport) later(r *request, d time.Duration) {
	t.mu.Lock()
	if t.closed || len(t.waiting) >= cap(t.queue) {
		t.mu.Unlock()
		t.logf("too many requests waiting, dropping a %s request", r.category)
		t.finish()
		return
	}
	t.waiting[r] = time.AfterFunc(d, func() {
		t.mu.Lock()
		delete(t.waiting, r)
		ok := t.put(r)
		t.mu.Unlock()
		if !ok {
			t.finish()
		}
	})
	t.mu.Unlock()
}

func (t *transport) run() {
	defer close(t.done)
	for {
		select {
		case <-t.stop:
			return
		case r := <-t.queue:
			t.deliver(r)
		}
	}
}

// pausedFor is how long a category is still paused (0 when it isn't).
func (t *transport) pausedFor(category string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return max(t.until[category].Sub(now), t.until[""].Sub(now), 0)
}

func (t *transport) deliver(r *request) {
	now := time.Now()
	if wait := t.pausedFor(r.category, now); wait > 0 {
		if wait > maxWait {
			t.logf("dropping a %s request: paused for %s", r.category, wait.Round(time.Second))
			t.finish()
			return
		}
		t.later(r, wait)
		return
	}
	status, retryAfter, err := t.post(r)
	switch {
	case err == nil && status < 300:
		t.finish()
	case err != nil || status == http.StatusTooManyRequests || status >= 500:
		r.attempts++
		if r.attempts >= maxAttempts {
			t.logf("dropping a %s request after %d attempts (%d, %v)", r.category, r.attempts, status, err)
			t.finish()
			return
		}
		backoff := time.Duration(math.Pow(2, float64(r.attempts))) * backoffUnit
		if wait := max(backoff, retryAfter); wait <= maxWait {
			t.later(r, wait)
			return
		}
		t.logf("dropping a %s request: retry after %s", r.category, retryAfter)
		t.finish()
	default:
		t.logf("%s request refused: %d", r.category, status)
		t.finish()
	}
}

// post sends a request, gzipped, and reads the rate limits of the answer.
func (t *transport) post(r *request) (status int, retryAfter time.Duration, err error) {
	var body bytes.Buffer
	if t.zw == nil {
		t.zw = gzip.NewWriter(&body)
	} else {
		t.zw.Reset(&body)
	}
	_, _ = t.zw.Write(r.body)
	_ = t.zw.Close()
	ctx, cancel := context.WithTimeout(t.ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.dsn.URL(r.path), &body)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+t.dsn.Key)
	req.Header.Set("Content-Type", r.contentType)
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("User-Agent", sdkName+"/"+sdkVersion)
	res, err := t.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	_ = res.Body.Close()
	now := time.Now()
	if d, ok := seconds(res.Header.Get("Retry-After")); ok {
		retryAfter = d
	}
	t.limit(res.Header.Get("Fixwire-Rate-Limits"), now)
	if res.StatusCode == http.StatusTooManyRequests && res.Header.Get("Fixwire-Rate-Limits") == "" {
		t.limit(strconv.Itoa(max(int(retryAfter.Seconds()), 60))+":", now)
	}
	return res.StatusCode, retryAfter, nil
}

// seconds reads a header's whole seconds, at most maxPause; false for
// none, zero or negative ones, or anything else (an HTTP date).
func seconds(s string) (time.Duration, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0, false
	}
	return time.Duration(min(n, int(maxPause/time.Second))) * time.Second, true
}

// limit reads Fixwire-Rate-Limits: "<seconds>:<category;…>, …", no
// categories meaning all of them. Categories this SDK doesn't send are
// left out.
func (t *transport) limit(header string, now time.Time) {
	if header == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, part := range strings.Split(header, ",") {
		secs, cats, _ := strings.Cut(strings.TrimSpace(part), ":")
		d, ok := seconds(secs)
		if !ok {
			continue
		}
		until := now.Add(d)
		names := strings.Split(cats, ";")
		if cats == "" {
			names = []string{""}
		}
		for _, c := range names {
			switch c {
			case "", categoryError, categoryLog, categorySpan, categorySession, categoryCheckIn, categoryFeedback:
			default:
				continue
			}
			if until.After(t.until[c]) {
				t.until[c] = until
			}
		}
	}
}

// flush waits until every queued request is sent or dropped, or timeout.
func (t *transport) flush(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	// Wakes the wait below at the deadline; nothing is left waiting after.
	timer := time.AfterFunc(timeout, func() {
		t.mu.Lock()
		t.idle.Broadcast()
		t.mu.Unlock()
	})
	defer timer.Stop()
	t.mu.Lock()
	defer t.mu.Unlock()
	for t.pending > 0 {
		if !time.Now().Before(deadline) {
			return false
		}
		t.idle.Wait()
	}
	return true
}

// close stops sending: the request in flight is ended, and what is queued
// or waiting is dropped.
func (t *transport) close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	dropped := 0
	for r, timer := range t.waiting {
		if timer.Stop() {
			dropped++
		}
		delete(t.waiting, r)
	}
	t.mu.Unlock()
	t.cancel()
	close(t.stop)
	<-t.done
	for {
		select {
		case <-t.queue:
			dropped++
		default:
			for range dropped {
				t.finish()
			}
			return
		}
	}
}
