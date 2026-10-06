package fixwire

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Hub pairs a client with a stack of scopes. The package's functions use
// the current hub; a request has a clone of it, with its own scope, in its
// context (NewContext, HubFromContext).
type Hub struct {
	mu     sync.RWMutex
	client *Client
	scopes []*Scope
}

// NewHub returns a hub for client and scope.
func NewHub(client *Client, scope *Scope) *Hub {
	if scope == nil {
		scope = NewScope()
	}
	return &Hub{client: client, scopes: []*Scope{scope}}
}

var current atomic.Pointer[Hub]

func init() { current.Store(NewHub(nil, nil)) }

// CurrentHub is the hub the package's functions use.
func CurrentHub() *Hub { return current.Load() }

type hubKey struct{}

// NewContext returns ctx carrying hub.
func NewContext(ctx context.Context, hub *Hub) context.Context {
	return context.WithValue(ctx, hubKey{}, hub)
}

// HubFromContext is the hub in ctx, else the current hub.
func HubFromContext(ctx context.Context) *Hub {
	if ctx != nil {
		if h, ok := ctx.Value(hubKey{}).(*Hub); ok && h != nil {
			return h
		}
	}
	return CurrentHub()
}

// Client is the hub's client (nil before Init).
func (h *Hub) Client() *Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.client
}

// BindClient makes client the hub's.
func (h *Hub) BindClient(client *Client) {
	h.mu.Lock()
	h.client = client
	h.mu.Unlock()
}

// Scope is the hub's current scope.
func (h *Hub) Scope() *Scope {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.scopes[len(h.scopes)-1]
}

// Clone is a hub with the same client and a copy of the current scope, for
// work that runs apart (a request, a goroutine).
func (h *Hub) Clone() *Hub { return NewHub(h.Client(), h.Scope().Clone()) }

// ConfigureScope changes the current scope.
func (h *Hub) ConfigureScope(f func(*Scope)) { f(h.Scope()) }

// WithScope runs f with a copy of the current scope: what f sets on it is
// gone afterwards.
func (h *Hub) WithScope(f func(*Scope)) {
	s := h.Scope().Clone()
	h.mu.Lock()
	h.scopes = append(h.scopes, s)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.scopes = h.scopes[:len(h.scopes)-1]
		h.mu.Unlock()
	}()
	f(s)
}

// CaptureException sends an error with the stack where it was captured;
// its event id, or "" when it was not sent.
func (h *Hub) CaptureException(err error) string {
	c := h.Client()
	if err == nil || c == nil {
		return ""
	}
	defer c.guard()
	defer enterCapture()()
	e := &Event{Exceptions: exceptionsOf(err, stack(c.opts), Mechanism{Type: "generic", Handled: true}, c.opts)}
	return c.capture(e, h.Scope())
}

// CaptureMessage sends a message (level info unless the scope says
// otherwise).
func (h *Hub) CaptureMessage(message string) string {
	c := h.Client()
	if c == nil {
		return ""
	}
	defer c.guard()
	defer enterCapture()()
	return c.capture(&Event{Message: message}, h.Scope())
}

// CaptureEvent sends an event as it is, with what the scope knows.
func (h *Hub) CaptureEvent(e *Event) string {
	c := h.Client()
	if c == nil || e == nil {
		return ""
	}
	defer c.guard()
	defer enterCapture()()
	return c.capture(e, h.Scope())
}

// RecoverPanic sends a recovered panic as an unhandled, fatal error, with
// the panicking goroutine's stack. Call it in the deferred function that
// recovered.
func (h *Hub) RecoverPanic(r any) string {
	c := h.Client()
	if r == nil || c == nil {
		return ""
	}
	defer c.guard()
	defer enterCapture()()
	e := &Event{Level: LevelFatal, Exceptions: exceptionsOf(panicError(r), panicStack(c.opts), Mechanism{Type: "panic", Handled: false}, c.opts)}
	if _, ok := r.(error); !ok {
		e.Exceptions[0].Type = "panic" // panic("…") and other values
	}
	return c.capture(e, h.Scope())
}

// AddBreadcrumb records something that happened on the current scope.
func (h *Hub) AddBreadcrumb(b Breadcrumb) {
	c := h.Client()
	defer c.guard()
	max := 100
	if c != nil {
		max = c.opts.MaxBreadcrumbs
		if c.opts.BeforeBreadcrumb != nil {
			p := beforeBreadcrumb(c.opts.BeforeBreadcrumb, &b)
			if p == nil {
				return
			}
			b = *p
		}
	}
	if max < 0 {
		return
	}
	h.Scope().AddBreadcrumb(b, max)
}

// beforeBreadcrumb runs the app's BeforeBreadcrumb as a capture: what it
// logs is not captured again.
func beforeBreadcrumb(f func(*Breadcrumb) *Breadcrumb, b *Breadcrumb) *Breadcrumb {
	defer enterCapture()()
	return f(b)
}

// Flush waits until what was captured is sent, or timeout.
func (h *Hub) Flush(timeout time.Duration) bool { return h.Client().Flush(timeout) }
