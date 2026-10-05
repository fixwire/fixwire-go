package fixwire

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	mrand "math/rand/v2"
	"strings"
	"time"

	"github.com/fixwire/fixwire/sdks/go/internal/redact"
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
	body, err := c.encodeLogs(c.eventRecord(e))
	if err != nil {
		c.transport.logf("encoding an event: %v", err)
		return ""
	}
	if !c.transport.send(&request{path: "/v1/logs", contentType: "application/json", category: categoryError, body: body}) {
		return ""
	}
	return e.EventID
}

// scrub masks secrets and personal data in m, but for the keys in skip;
// m comes back in JSON's own types.
func (c *Client) scrub(m map[string]any, skip ...string) map[string]any {
	if c.redactor == nil {
		return m
	}
	kept := map[string]any{}
	for _, k := range skip {
		if v, ok := m[k]; ok {
			kept[k] = v
			delete(m, k)
		}
	}
	plain, _ := c.redactor.Walk(jsonOf(m))
	out, _ := plain.(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range kept {
		out[k] = v
	}
	return out
}

// mask masks secrets and personal data in s.
func (c *Client) mask(s string) string {
	if c.redactor == nil || s == "" {
		return s
	}
	s, _ = c.redactor.Mask(s)
	return s
}

// jsonOf is v as JSON decodes it (maps, slices, strings, json.Number, …).
func jsonOf(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if dec.Decode(&out) != nil {
		return nil
	}
	return out
}

// ShouldPropagate reports whether trace headers may go to url: it holds one
// of the TracePropagationTargets.
func (c *Client) ShouldPropagate(url string) bool {
	if c == nil {
		return false
	}
	for _, t := range c.opts.TracePropagationTargets {
		if t != "" && strings.Contains(url, t) {
			return true
		}
	}
	return false
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

// newID is n random bytes in hex: 16 for event and trace ids, 8 for span
// ids.
func newID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
