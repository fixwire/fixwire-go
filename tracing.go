package fixwire

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SpanKind is OpenTelemetry's span kind.
type SpanKind int

// The span kinds.
const (
	KindInternal SpanKind = 1
	KindServer   SpanKind = 2
	KindClient   SpanKind = 3
	KindProducer SpanKind = 4
	KindConsumer SpanKind = 5
)

// Span is a timed piece of work in a trace. A span without a parent in
// this process (a request, a job) is a segment: it is sent with the spans
// under it when it finishes.
type Span struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Name         string
	Op           string
	Kind         SpanKind
	// Sampled says whether the trace is kept: an unsampled span still
	// carries the trace to the services it calls.
	Sampled bool
	Start   time.Time
	End     time.Time

	mu           sync.Mutex
	attrs        map[string]any
	failed       bool
	message      string
	remoteParent bool
	tracestate   string
	baggage      string
	segment      *Span
	children     []*Span
	sent         bool
	client       *Client
}

// maxChildren bounds the spans a segment keeps until it is sent;
// maxAttributes a span's attributes, fixwire.op among them.
const (
	maxChildren   = 1000
	maxAttributes = 128
)

type spanKey struct{}

// SpanFromContext is the span in ctx, or nil.
func SpanFromContext(ctx context.Context) *Span {
	if s, ok := ctx.Value(spanKey{}).(*Span); ok {
		return s
	}
	return nil
}

// SpanOption sets up a span.
type SpanOption func(*Span)

// WithOp sets the span's operation (http.server, db.query, task, …).
func WithOp(op string) SpanOption { return func(s *Span) { s.Op = op } }

// WithKind sets the span's kind.
func WithKind(k SpanKind) SpanOption { return func(s *Span) { s.Kind = k } }

// WithAttributes sets attributes (OpenTelemetry's semantic conventions).
func WithAttributes(attrs map[string]any) SpanOption {
	return func(s *Span) {
		for k, v := range attrs {
			s.setAttribute(k, v)
		}
	}
}

// ContinueTrace makes the span continue a caller's trace, from its W3C
// traceparent, tracestate and baggage headers. A malformed traceparent
// starts a new trace.
func ContinueTrace(traceparent, tracestate, baggage string) SpanOption {
	return func(s *Span) {
		trace, parent, sampled, ok := parseTraceparent(traceparent)
		if !ok {
			return
		}
		s.TraceID, s.ParentSpanID, s.Sampled, s.remoteParent = trace, parent, sampled, true
		s.tracestate, s.baggage = passable(tracestate, maxTracestate), passable(baggage, maxBaggage)
	}
}

// maxTracestate and maxBaggage bound what a caller's headers may carry on
// to every call (the W3C limits).
const (
	maxTracestate = 512
	maxBaggage    = 8192
)

// passable is a caller's header as it may be passed on: "" when it is
// longer than limit or holds a control character. A tab is W3C's list
// whitespace, not one.
func passable(h string, limit int) string {
	if len(h) > limit {
		return ""
	}
	for i := 0; i < len(h); i++ {
		if h[i] < ' ' && h[i] != '\t' || h[i] == 0x7f {
			return ""
		}
	}
	return h
}

// StartSpan starts a span under the one in ctx (or a new trace), and
// returns it with a context carrying it. Finish it when the work ends.
func StartSpan(ctx context.Context, name string, opts ...SpanOption) (*Span, context.Context) {
	s := &Span{Name: name, SpanID: newID(8), Start: time.Now(), Kind: KindInternal, attrs: map[string]any{},
		client: HubFromContext(ctx).Client()}
	for _, o := range opts {
		o(s)
	}
	switch parent := SpanFromContext(ctx); {
	case s.remoteParent:
		s.segment = s
	case parent != nil:
		s.TraceID, s.ParentSpanID, s.Sampled = parent.TraceID, parent.SpanID, parent.Sampled
		s.tracestate, s.baggage, s.segment = parent.tracestate, parent.baggage, parent.segment
	default:
		s.TraceID, s.segment = newID(16), s
		rate := 0.0
		if s.client != nil {
			rate = s.client.opts.TracesSampleRate
		}
		s.Sampled = sampleTrace(s.TraceID, rate)
	}
	if s.Kind == KindInternal {
		s.Kind = kindOf(s.Op)
	}
	return s, context.WithValue(ctx, spanKey{}, s)
}

// kindOf is the kind an operation implies.
func kindOf(op string) SpanKind {
	switch {
	case op == "http.server" || strings.HasSuffix(op, ".server"):
		return KindServer
	case op == "http.client" || strings.HasPrefix(op, "db") || strings.HasSuffix(op, ".client"):
		return KindClient
	case strings.HasSuffix(op, ".publish"):
		return KindProducer
	case strings.HasSuffix(op, ".process"):
		return KindConsumer
	}
	return KindInternal
}

// sampleTrace decides a new trace the way every Fixwire SDK does: kept
// when its id's last 56 bits, as a fraction of 2^56, are at least
// 1 - rate (fixwire-protocol §9).
func sampleTrace(traceID string, rate float64) bool {
	switch {
	case rate <= 0:
		return false
	case rate >= 1:
		return true
	case len(traceID) < 14:
		return false
	}
	n, err := strconv.ParseUint(traceID[len(traceID)-14:], 16, 64)
	if err != nil {
		return false
	}
	return float64(n)/float64(uint64(1)<<56) >= 1-rate
}

// parseTraceparent reads "00-<trace id>-<parent id>-<flags>": version 00, a
// non-zero trace id of 32 lower-case hex digits, a non-zero parent id of
// 16, and 2 of flags. Anything else is not used.
func parseTraceparent(h string) (trace, parent string, sampled, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(h), "-", 5)
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return "", "", false, false
	}
	if !hexString(parts[1]) || !hexString(parts[2]) || !hexString(parts[3]) ||
		strings.Trim(parts[1], "0") == "" || strings.Trim(parts[2], "0") == "" {
		return "", "", false, false
	}
	flags, _ := strconv.ParseUint(parts[3], 16, 8)
	return parts[1], parts[2], flags&1 == 1, true
}

// hexString reports whether s is lower-case hex digits, as W3C trace
// context writes them.
func hexString(s string) bool {
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && (s[i] < 'a' || s[i] > 'f') {
			return false
		}
	}
	return true
}

// Traceparent is the W3C traceparent header that continues the span's
// trace in a service it calls.
func (s *Span) Traceparent() string {
	flags := "00"
	if s.Sampled {
		flags = "01"
	}
	return "00-" + s.TraceID + "-" + s.SpanID + "-" + flags
}

// Tracestate is the caller's tracestate, passed on.
func (s *Span) Tracestate() string { return s.tracestate }

// Baggage is the caller's baggage, passed on.
func (s *Span) Baggage() string { return s.baggage }

// SetAttribute sets an attribute. A span holds at most 128: past them, new
// keys are left out.
func (s *Span) SetAttribute(key string, value any) {
	s.mu.Lock()
	s.setAttribute(key, value)
	s.mu.Unlock()
}

// setAttribute sets an attribute, with s.mu held, leaving room for
// fixwire.op.
func (s *Span) setAttribute(key string, value any) {
	if _, ok := s.attrs[key]; ok || len(s.attrs) < maxAttributes-1 {
		s.attrs[key] = value
	}
}

// SetError marks the span failed.
func (s *Span) SetError(err error) {
	s.mu.Lock()
	s.failed = true
	if err != nil {
		s.message = err.Error()
	}
	s.mu.Unlock()
}

// Finish ends the span. A segment is sent with the spans finished under it;
// a span finishing after its segment is sent alone.
func (s *Span) Finish() {
	s.mu.Lock()
	if !s.End.IsZero() {
		s.mu.Unlock()
		return
	}
	s.End = time.Now()
	s.mu.Unlock()
	if !s.Sampled || s.client == nil || !s.client.enabled {
		return
	}
	seg := s.segment
	seg.mu.Lock()
	if seg == s {
		spans := append(seg.children, s)
		seg.children, seg.sent = nil, true
		seg.mu.Unlock()
		s.client.sendSpans(spans)
		return
	}
	if seg.sent {
		seg.mu.Unlock()
		s.client.sendSpans([]*Span{s})
		return
	}
	if len(seg.children) < maxChildren {
		seg.children = append(seg.children, s)
	}
	seg.mu.Unlock()
}

// json is the span as OTLP JSON, redacted by c.
func (s *Span) json(c *Client) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	attrs := make(map[string]any, len(s.attrs)+1)
	for k, v := range s.attrs {
		attrs[k] = c.plain(v)
	}
	attrs["fixwire.op"] = s.Op
	attrs = c.scrub(attrs)
	flags := 0x100
	if s.remoteParent {
		flags |= 0x200
	}
	if s.Sampled {
		flags |= 1
	}
	status := map[string]any{"code": 1}
	if s.failed {
		status = map[string]any{"code": 2, "message": c.text(s.message)}
	}
	out := map[string]any{
		"traceId": s.TraceID, "spanId": s.SpanID, "name": c.text(s.Name), "kind": int(s.Kind),
		"startTimeUnixNano": nanos(s.Start), "endTimeUnixNano": nanos(s.End), "attributes": attributes(attrs),
		"status": status, "flags": flags,
	}
	if s.ParentSpanID != "" {
		out["parentSpanId"] = s.ParentSpanID
	}
	return out
}

// sendSpans sends finished spans as OTLP traces exports of at most maxItems
// spans and maxRequestBytes; a span too large for one is dropped alone.
func (c *Client) sendSpans(spans []*Span) {
	defer c.guard()
	defer enterCapture()()
	export := func(items []json.RawMessage) ([]byte, error) {
		return json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
			"resource": c.resource(), "scopeSpans": []any{map[string]any{"scope": scope(), "spans": items}},
		}}})
	}
	none, err := export([]json.RawMessage{})
	if err != nil {
		c.transport.logf("encoding spans: %v", err)
		return
	}
	envelope := len(none) // the bytes of a request but its spans
	var batch []json.RawMessage
	size := envelope
	send := func() {
		if len(batch) == 0 {
			return
		}
		if body, err := export(batch); err != nil {
			c.transport.logf("encoding spans: %v", err)
		} else {
			c.transport.send(&request{path: "/v1/traces", contentType: "application/json", category: categorySpan, body: body})
		}
		batch, size = nil, envelope
	}
	for _, s := range spans {
		item, err := json.Marshal(s.json(c))
		if err != nil || envelope+len(item) > maxRequestBytes {
			c.transport.logf("dropped a span of %d bytes (%v)", len(item), err)
			continue
		}
		if len(batch) == maxItems || size+len(item)+1 > maxRequestBytes {
			send()
		}
		batch, size = append(batch, item), size+len(item)+1
	}
	send()
}
