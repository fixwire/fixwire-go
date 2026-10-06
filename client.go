package fixwire

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	mrand "math/rand/v2"
	"net"
	"net/url"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fixwire/fixwire-go/internal/redact"
)

// Client sends to one project. Most programs use the one Init sets up,
// through the package's functions or a Hub.
type Client struct {
	opts      Options
	dsn       DSN
	transport *transport
	sessions  *aggregates
	budget    *budget
	redactor  *redact.Redactor
	enabled   bool
}

// NewClient returns a client; without a DSN it is disabled and sends
// nothing.
func NewClient(opts Options) (*Client, error) {
	opts = opts.withDefaults()
	c := &Client{opts: opts}
	if opts.DSN == "" {
		return c, nil
	}
	dsn, err := ParseDSN(opts.DSN)
	if err != nil {
		return nil, err
	}
	if !opts.DisableRedaction {
		if c.redactor, err = redact.New(redact.Options{SensitiveKeys: opts.SensitiveKeys}); err != nil {
			return nil, err
		}
	}
	c.dsn, c.enabled = dsn, true
	c.budget = newBudget(opts.ErrorBudget)
	c.transport = newTransport(dsn, opts)
	if opts.sessionsOn() {
		c.sessions = newAggregates(c, opts.SessionInterval)
	}
	return c, nil
}

// Options are the client's options, defaults filled in.
func (c *Client) Options() Options { return c.opts }

// capture sends an event with what the scope knows; its id, or "" when it
// was not sent.
func (c *Client) capture(e *Event, scope *Scope) string {
	if c == nil || !c.enabled {
		return ""
	}
	if scope != nil {
		scope.applyTo(e)
		// The session counts the error whether or not it is sent.
		switch {
		case len(e.Exceptions) > 0:
			scope.markSession(!e.Exceptions[0].Mechanism.Handled)
		case e.Level == LevelError || e.Level == LevelFatal:
			scope.markSession(false)
		}
	}
	ok, suppressed := c.budget.allow(issueOf(e), time.Now())
	if !ok {
		c.transport.logf("dropped an event: over the error budget")
		return ""
	}
	if c.opts.SampleRate < 1 && mrand.Float64() >= c.opts.SampleRate {
		return ""
	}
	e.suppressed = suppressed
	if e.EventID == "" {
		e.EventID = newID(16)
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if e.Level == "" {
		e.Level = LevelError
		if len(e.Exceptions) == 0 {
			e.Level = LevelInfo
		}
	}
	if !c.opts.SendDefaultPII {
		e.User.IPAddress = ""
	} else if e.User.IPAddress == "" && e.Request != nil {
		e.User.IPAddress = e.Request.ip
	}
	if c.opts.BeforeSend != nil {
		if e = c.opts.BeforeSend(e); e == nil {
			return ""
		}
	}
	record := c.eventRecord(e)
	body, err := c.encodeLogs(record)
	// An event is at most 1 MB: past it, its breadcrumbs go, then its
	// contexts (Go frames carry no local variables), then the event.
	for _, key := range []string{"fixwire.breadcrumbs", "fixwire.contexts"} {
		if err != nil || len(body) <= maxEventBytes {
			break
		}
		shed(record, key)
		body, err = c.encodeLogs(record)
	}
	if err != nil {
		c.transport.logf("encoding an event: %v", err)
		return ""
	}
	if len(body) > maxEventBytes {
		c.transport.logf("dropped an event of %d bytes", len(body))
		return ""
	}
	if !c.transport.send(&request{path: "/v1/logs", contentType: "application/json", category: categoryError, body: body}) {
		return ""
	}
	return e.EventID
}

// scrub readies m, of JSON's own types (app values through plain), to be
// sent, in place: secrets and personal data masked, but in the keys of skip,
// then strings cut to MaxValueLength. Redaction reads the part of a string
// kept and the next 16 kB.
func (c *Client) scrub(m map[string]any, skip ...string) map[string]any {
	kept := map[string]any{}
	for _, k := range skip {
		if v, ok := m[k]; ok {
			kept[k] = v
			delete(m, k)
		}
	}
	limit := c.opts.MaxValueLength
	eachString(m, func(s string) string { return ahead(s, limit) })
	if c.redactor != nil {
		m = c.redact(m)
	}
	eachString(m, func(s string) string { return cut(s, limit) })
	for k, v := range kept {
		m[k] = v
	}
	return m
}

// redact masks m in place. Where redaction fails, the values go as
// [Filtered], never as they are.
func (c *Client) redact(m map[string]any) (out map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			c.transport.logf("redaction failed, sending [Filtered]: %v", r)
			out = make(map[string]any, len(m))
			for k := range m {
				out[c.mask(k)] = redact.Filtered
			}
		}
	}()
	v, _ := c.redactor.Walk(m)
	out, _ = v.(map[string]any)
	return out
}

// mask masks secrets and personal data in s; [Filtered] where redaction
// fails.
func (c *Client) mask(s string) (masked string) {
	if c.redactor == nil || s == "" {
		return s
	}
	defer func() {
		if recover() != nil {
			masked = redact.Filtered
		}
	}()
	masked, _ = c.redactor.Mask(s)
	return masked
}

// text is a string as it is sent: masked, then cut to MaxValueLength.
func (c *Client) text(s string) string {
	return cut(c.mask(ahead(s, c.opts.MaxValueLength)), c.opts.MaxValueLength)
}

// ShouldPropagate reports whether trace headers may go to rawURL: it
// matches one of the TracePropagationTargets (see Options).
func (c *Client) ShouldPropagate(rawURL string) bool {
	if c == nil || len(c.opts.TracePropagationTargets) == 0 {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	// The URL as compared: without user info, query and fragment.
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	compared := scheme + "://" + strings.ToLower(u.Host) + u.EscapedPath()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	for _, t := range c.opts.TracePropagationTargets {
		switch {
		case strings.Contains(t, "://"):
			if strings.HasPrefix(compared, t) {
				return true
			}
		case t == "" || strings.HasPrefix(t, "/"):
			// A path is a browser page's own origin's: servers have none.
		case hostMatches(t, host, port):
			return true
		}
	}
	return false
}

// hostMatches reports whether a URL's host and port match target, a host
// with a port if it has one: that host, or one of its subdomains.
func hostMatches(target, host, port string) bool {
	th, tp := strings.ToLower(target), ""
	if h, p, err := net.SplitHostPort(th); err == nil {
		th, tp = h, p
	}
	th = strings.Trim(th, "[]")
	if th == "" || tp != "" && tp != port {
		return false
	}
	return host == th || strings.HasSuffix(host, "."+th)
}

// The size of what is sent: an error or a message is at most maxEventBytes
// of JSON; spans go maxItems to a request of at most maxRequestBytes.
const (
	maxEventBytes   = 1 << 20
	maxItems        = 100
	maxRequestBytes = 5 << 20
)

// shed leaves an attribute out of an OTLP record.
func shed(record map[string]any, key string) {
	attrs, _ := record["attributes"].([]any)
	record["attributes"] = slices.DeleteFunc(attrs, func(a any) bool {
		kv, _ := a.(map[string]any)
		return kv["key"] == key
	})
}

// sendJSON queues a Fixwire JSON request.
func (c *Client) sendJSON(path, category string, v any) bool {
	if c == nil || !c.enabled {
		return false
	}
	body, err := json.Marshal(v)
	if err != nil {
		c.transport.logf("encoding a %s request: %v", category, err)
		return false
	}
	return c.transport.send(&request{path: path, contentType: "application/json", category: category, body: body})
}

// sdk names the SDK in Fixwire JSON bodies.
func sdk() map[string]string { return map[string]string{"name": sdkName, "version": sdkVersion} }

// Flush waits until what was captured is sent, or timeout; false when time
// ran out.
func (c *Client) Flush(timeout time.Duration) bool {
	if c == nil || !c.enabled {
		return true
	}
	if c.sessions != nil {
		c.sessions.send()
	}
	return c.transport.flush(timeout)
}

// Close flushes and stops the client.
func (c *Client) Close(timeout time.Duration) {
	if c == nil || !c.enabled {
		return
	}
	if c.sessions != nil {
		c.sessions.stop()
	}
	c.Flush(timeout)
	c.transport.close()
}

// guard keeps a panic from reaching the app: one in the SDK, or in the
// app's own methods it calls (Error, String, MarshalJSON, BeforeSend).
// Deferred where the app calls in.
func (c *Client) guard() {
	if r := recover(); r != nil && c != nil && c.transport != nil {
		c.transport.logf("recovered from a panic: %v", r)
	}
}

// capturing holds the goroutines capturing now, so that what the app logs
// from the code a capture calls (BeforeSend, an error's Error method) is not
// captured again, and again.
var capturing struct {
	n   atomic.Int32 // captures under way, on any goroutine
	mu  sync.Mutex
	ids map[uint64]int // goroutine → captures under way on it
}

// enterCapture marks the calling goroutine as capturing until leave.
func enterCapture() (leave func()) {
	id := goroutineID()
	capturing.mu.Lock()
	if capturing.ids == nil {
		capturing.ids = map[uint64]int{}
	}
	capturing.ids[id]++
	capturing.n.Add(1)
	capturing.mu.Unlock()
	return func() {
		capturing.mu.Lock()
		if capturing.ids[id]--; capturing.ids[id] <= 0 {
			delete(capturing.ids, id)
		}
		capturing.n.Add(-1)
		capturing.mu.Unlock()
	}
}

// inCapture reports whether the calling goroutine is capturing: at once
// when no goroutine is.
func inCapture() bool {
	if capturing.n.Load() == 0 {
		return false
	}
	id := goroutineID()
	capturing.mu.Lock()
	defer capturing.mu.Unlock()
	return capturing.ids[id] > 0
}

// goroutineID is the calling goroutine's id, from the first line of its
// stack ("goroutine 18 [running]:").
func goroutineID() uint64 {
	var buf [64]byte
	b := bytes.TrimPrefix(buf[:runtime.Stack(buf[:], false)], []byte("goroutine "))
	if i := bytes.IndexByte(b, ' '); i > 0 {
		b = b[:i]
	}
	id, _ := strconv.ParseUint(string(b), 10, 64)
	return id
}

// newID is n random bytes in hex: 16 for event and trace ids, 8 for span
// ids.
func newID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
