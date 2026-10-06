package fixwirehttp

import (
	"net/http"
	"strings"

	fixwire "github.com/fixwire/fixwire-go"
)

// Transport times outgoing requests as client spans of the trace in the
// request's context, passes the trace on to the client's
// TracePropagationTargets and leaves an "http" breadcrumb.
type Transport struct {
	// Base sends the requests (http.DefaultTransport when nil).
	Base http.RoundTripper
}

// NewTransport wraps base (http.DefaultTransport when nil).
func NewTransport(base http.RoundTripper) *Transport { return &Transport{Base: base} }

// RoundTrip sends req.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	ctx := req.Context()
	hub := fixwire.HubFromContext(ctx)
	client := hub.Client()
	plain := req.URL.Scheme + "://" + req.URL.Host + req.URL.Path

	parent := fixwire.SpanFromContext(ctx)
	var span *fixwire.Span
	if parent != nil && parent.Sampled {
		span, _ = fixwire.StartSpan(ctx, req.Method+" "+plain, fixwire.WithOp("http.client"),
			fixwire.WithAttributes(map[string]any{
				"http.request.method": req.Method, "url.full": plain, "server.address": req.URL.Hostname(),
			}))
	}
	if from := span; parent != nil && client.ShouldPropagate(req.URL.String()) && req.Header.Get("traceparent") == "" {
		if from == nil {
			from = parent
		}
		req = req.Clone(ctx) // a RoundTripper must not change the caller's request
		req.Header.Set("traceparent", from.Traceparent())
		if s := from.Tracestate(); s != "" {
			req.Header.Set("tracestate", s)
		}
		if b := from.Baggage(); b != "" && req.Header.Get("baggage") == "" {
			req.Header.Set("baggage", b)
		}
	}

	resp, err := base.RoundTrip(req)

	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if span != nil {
		if status != 0 {
			span.SetAttribute("http.response.status_code", status)
		}
		if err != nil || status >= 400 {
			span.SetError(err)
		}
		span.Finish()
	}
	data := map[string]any{"method": req.Method, "url": plain}
	if status != 0 {
		data["status_code"] = status
	}
	level := fixwire.LevelInfo
	if err != nil || status >= 500 {
		level = fixwire.LevelError
	}
	if !strings.HasPrefix(req.Header.Get("User-Agent"), "fixwire.go/") {
		hub.AddBreadcrumb(fixwire.Breadcrumb{Type: "http", Category: "http", Level: level, Data: data})
	}
	return resp, err
}
