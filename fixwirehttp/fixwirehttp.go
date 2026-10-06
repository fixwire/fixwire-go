// Package fixwirehttp reports what happens in net/http servers and clients
// to Fixwire.
//
// The middleware gives each request its own scope (with the request on
// it), reports panics, counts the request's session and, with tracing on,
// times it as a server span named after the route the ServeMux matched:
//
//	fixwire.Init(fixwire.Options{TracesSampleRate: 0.2})
//	handler := fixwirehttp.New(fixwirehttp.Options{}).Handle(mux)
//	http.ListenAndServe(":8080", handler)
//
// Handlers reach the request's hub with fixwire.HubFromContext(r.Context()).
//
// The transport times outgoing requests as client spans of the request's
// trace, and passes the trace on to TracePropagationTargets:
//
//	client := &http.Client{Transport: fixwirehttp.NewTransport(nil)}
package fixwirehttp

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	fixwire "github.com/fixwire/fixwire-go"
)

// Options configure the middleware.
type Options struct {
	// Repanic lets a panic go on after it is reported, for a recovering
	// middleware further out; by default the middleware answers 500.
	Repanic bool
	// WaitForDelivery waits, after a panic, until the report is sent (up to
	// Timeout): for platforms that stop the process when it answers.
	WaitForDelivery bool
	// Timeout bounds WaitForDelivery (default 2 s).
	Timeout time.Duration
}

// Handler is the middleware.
type Handler struct{ opts Options }

// New returns the middleware.
func New(opts Options) *Handler {
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Second
	}
	return &Handler{opts: opts}
}

// HandleFunc wraps a handler function.
func (h *Handler) HandleFunc(next http.HandlerFunc) http.HandlerFunc {
	return h.Handle(next).ServeHTTP
}

// Handle wraps a handler.
func (h *Handler) Handle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub := fixwire.HubFromContext(r.Context()).Clone()
		client := hub.Client()
		sendPII := client != nil && client.Options().SendDefaultPII
		ctx := fixwire.NewContext(r.Context(), hub)

		attrs := map[string]any{
			"http.request.method": r.Method, "url.path": r.URL.Path, "url.scheme": scheme(r),
			"server.address": r.Host, "user_agent.original": r.UserAgent(),
		}
		span, ctx := fixwire.StartSpan(ctx, r.Method, fixwire.WithOp("http.server"), fixwire.WithAttributes(attrs),
			fixwire.ContinueTrace(r.Header.Get("traceparent"), r.Header.Get("tracestate"), r.Header.Get("baggage")))
		r = r.WithContext(ctx)
		scope := hub.Scope()
		scope.SetRequest(r, sendPII)
		scope.SetSpan(span)
		endSession := hub.StartRequestSession()
		rw := &responseWriter{ResponseWriter: w}

		defer func() {
			rec := recover()
			if rec != nil && rec != http.ErrAbortHandler { //nolint:errorlint // ErrAbortHandler is a sentinel panic value
				hub.RecoverPanic(rec)
				if !rw.wrote {
					rw.status = http.StatusInternalServerError
				}
			}
			finish(span, r, rw.status)
			endSession()
			if rec != nil && h.opts.WaitForDelivery {
				hub.Flush(h.opts.Timeout)
			}
			switch {
			case rec == nil:
			case rec == http.ErrAbortHandler || h.opts.Repanic: //nolint:errorlint // as above
				panic(rec)
			case !rw.wrote:
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(rw, r)
	})
}

// finish names the span after the route the mux matched and ends it.
func finish(span *fixwire.Span, r *http.Request, status int) {
	if path := routePath(r.Pattern); path != "" {
		span.Name = r.Method + " " + path
		span.SetAttribute("http.route", path)
	}
	if status == 0 {
		status = http.StatusOK
	}
	span.SetAttribute("http.response.status_code", status)
	if status >= 500 {
		span.SetError(nil)
	}
	span.Finish()
}

// routePath is the path of a ServeMux pattern: "/items/{id}" of
// "GET example.com/items/{id}".
func routePath(pattern string) string {
	if _, rest, ok := strings.Cut(pattern, " "); ok {
		pattern = strings.TrimSpace(rest)
	}
	if i := strings.IndexByte(pattern, '/'); i > 0 {
		pattern = pattern[i:]
	}
	return pattern
}

func scheme(r *http.Request) string {
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		return "https"
	}
	return "http"
}

// responseWriter notes the status the handler answered with.
type responseWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *responseWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

// Flush flushes when the writer under it can.
func (w *responseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.wrote {
			w.status, w.wrote = http.StatusOK, true
		}
		f.Flush()
	}
}

// Hijack hands the connection over when the writer under it can.
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		w.wrote = true
		return h.Hijack()
	}
	return nil, nil, errors.New("fixwirehttp: the response writer cannot be hijacked")
}

// Unwrap is for http.ResponseController.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
