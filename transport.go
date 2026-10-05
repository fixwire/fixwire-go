package fixwire

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log"
	"math"
	"net/http"
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
// rate-limited request waits before it is dropped.
const (
	maxAttempts = 4
	maxWait     = 5 * time.Minute
)

// backoffUnit is the first retry's wait, halved (tests shorten it).
var backoffUnit = time.Second

// transport sends requests one at a time from a bounded queue, and backs
// off where Fixwire says to (Fixwire-Rate-Limits, Retry-After).
type transport struct {
	dsn    DSN
	client *http.Client
	debug  bool

	queue chan *request
	stop  chan struct{}
	done  chan struct{}

	mu      sync.Mutex
	until   map[string]time.Time // category ("" for all) → paused until
	pending int
	idle    *sync.Cond
}

func newTransport(dsn DSN, opts Options) *transport {
	t := &transport{
		dsn: dsn, client: opts.HTTPClient, debug: opts.Debug,
		queue: make(chan *request, opts.MaxQueue), stop: make(chan struct{}), done: make(chan struct{}),
		until: map[string]time.Time{},
	}
	t.idle = sync.NewCond(&t.mu)
	go t.run()
	return t
}

func (t *transport) logf(format string, args ...any) {
	if t.debug {
		log.Printf("fixwire: "+format, args...)
	}
}

// send queues a request; false when the queue is full or closed.
func (t *transport) send(r *request) bool {
	t.mu.Lock()
	t.pending++
	t.mu.Unlock()
	select {
	case <-t.stop:
	case t.queue <- r:
		return true
	default:
		t.logf("queue full, dropping a %s request", r.category)
	}
	t.finish()
	return false
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

// later puts a request back after d.
func (t *transport) later(r *request, d time.Duration) {
	time.AfterFunc(d, func() {
		select {
		case <-t.stop:
			t.finish()
		case t.queue <- r:
		default:
			t.finish()
		}
	})
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
		t.later(r, max(backoff, retryAfter))
	default:
		t.logf("%s request refused: %d", r.category, status)
		t.finish()
	}
}

// post sends a request, gzipped, and reads the rate limits of the answer.
func (t *transport) post(r *request) (status int, retryAfter time.Duration, err error) {
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	_, _ = zw.Write(r.body)
	_ = zw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && s > 0 {
		retryAfter = time.Duration(s) * time.Second
	}
	t.limit(res.Header.Get("Fixwire-Rate-Limits"), now)
	if res.StatusCode == http.StatusTooManyRequests && res.Header.Get("Fixwire-Rate-Limits") == "" {
		t.limit(strconv.Itoa(max(int(retryAfter.Seconds()), 60))+":", now)
	}
	return res.StatusCode, retryAfter, nil
}

// limit reads Fixwire-Rate-Limits: "<seconds>:<category;…>, …", no
// categories meaning all of them.
func (t *transport) limit(header string, now time.Time) {
	if header == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, part := range strings.Split(header, ",") {
		secs, cats, _ := strings.Cut(strings.TrimSpace(part), ":")
		n, err := strconv.Atoi(secs)
		if err != nil || n <= 0 {
			continue
		}
		until := now.Add(time.Duration(n) * time.Second)
		names := strings.Split(cats, ";")
		if cats == "" {
			names = []string{""}
		}
		for _, c := range names {
			if until.After(t.until[c]) {
				t.until[c] = until
			}
		}
	}
}

// flush waits until every queued request is sent or dropped, or timeout.
func (t *transport) flush(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		t.mu.Lock()
		for t.pending > 0 {
			t.idle.Wait()
		}
		t.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (t *transport) close() {
	select {
	case <-t.stop:
	default:
		close(t.stop)
		<-t.done
	}
}
