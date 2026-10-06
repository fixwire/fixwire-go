package fixwire

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Release health for servers: each request is a session, counted per
// minute and user and sent about every minute (fixwire-protocol §5).

// requestSession is the session of the request a scope serves.
type requestSession struct {
	mu     sync.Mutex
	status string // ok, errored, crashed
}

// markSession records an error on the scope's request session: errored,
// or crashed when it was not handled.
func (s *Scope) markSession(crashed bool) {
	s.mu.RLock()
	rs := s.session
	s.mu.RUnlock()
	if rs == nil {
		return
	}
	rs.mu.Lock()
	switch {
	case crashed:
		rs.status = "crashed"
	case rs.status == "ok":
		rs.status = "errored"
	}
	rs.mu.Unlock()
}

// StartRequestSession starts the session of the request the hub's scope
// serves; call the returned function when it ends. The fixwirehttp
// middleware does this.
func (h *Hub) StartRequestSession() (end func()) {
	c := h.Client()
	if c == nil || c.sessions == nil {
		return func() {}
	}
	rs := &requestSession{status: "ok"}
	scope := h.Scope()
	scope.mu.Lock()
	scope.session = rs
	scope.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			scope.mu.RLock()
			user := scope.user
			scope.mu.RUnlock()
			rs.mu.Lock()
			status := rs.status
			rs.mu.Unlock()
			c.sessions.record(status, deviceID(user), time.Now())
		})
	}
}

// deviceID is the user, hashed on the device: the first 16 bytes of the
// SHA-256 of their id (else email, else username), as hex. Never the raw
// id.
func deviceID(u User) string {
	id := u.ID
	if id == "" {
		id = u.Email
	}
	if id == "" {
		id = u.Username
	}
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}

type aggregateKey struct {
	minute time.Time
	did    string
}

type aggregateCounts struct{ exited, errored, crashed int }

// aggregates counts request sessions per minute and user.
type aggregates struct {
	c       *Client
	mu      sync.Mutex
	buckets map[aggregateKey]*aggregateCounts
	quit    chan struct{}
	once    sync.Once
}

func newAggregates(c *Client, interval time.Duration) *aggregates {
	a := &aggregates{c: c, buckets: map[aggregateKey]*aggregateCounts{}, quit: make(chan struct{})}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-a.quit:
				return
			case <-t.C:
				a.send()
			}
		}
	}()
	return a
}

// maxAggregates bounds the counts kept between sends (minutes × users),
// and so a send's size (the protocol's limit is 1 MB); past it, sessions
// are counted without their user.
const maxAggregates = 5000

func (a *aggregates) record(status, did string, at time.Time) {
	k := aggregateKey{minute: at.UTC().Truncate(time.Minute), did: did}
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.buckets[k]
	if b == nil && len(a.buckets) >= maxAggregates {
		k.did = ""
		b = a.buckets[k]
	}
	if b == nil {
		b = &aggregateCounts{}
		a.buckets[k] = b
	}
	switch status {
	case "crashed":
		b.crashed++
	case "errored":
		b.errored++
	default:
		b.exited++
	}
}

// send sends what was counted.
func (a *aggregates) send() {
	a.mu.Lock()
	buckets := a.buckets
	a.buckets = map[aggregateKey]*aggregateCounts{}
	a.mu.Unlock()
	if len(buckets) == 0 {
		return
	}
	out := make([]map[string]any, 0, len(buckets))
	for k, b := range buckets {
		agg := map[string]any{"started": k.minute.Format(time.RFC3339), "exited": b.exited, "errored": b.errored, "crashed": b.crashed}
		if k.did != "" {
			agg["did"] = k.did
		}
		out = append(out, agg)
	}
	// The counts without a user (past maxAggregates) may add a few more: a
	// request holds maxAggregates at most.
	for len(out) > 0 {
		n := min(len(out), maxAggregates)
		a.c.sendJSON("/v1/sessions", categorySession, map[string]any{
			"sdk": sdk(), "release": a.c.opts.Release, "environment": a.c.opts.Environment, "aggregates": out[:n],
		})
		out = out[n:]
	}
}

func (a *aggregates) stop() { a.once.Do(func() { close(a.quit) }) }
