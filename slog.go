package fixwire

import (
	"context"
	"log/slog"
	"strings"
)

// SlogOptions configure the slog handler.
type SlogOptions struct {
	// BreadcrumbLevel: records at or above it become breadcrumbs (default
	// Info).
	BreadcrumbLevel slog.Leveler
	// EventLevel: records at or above it are sent as events (default
	// Error); one with an error attribute is sent as that error.
	EventLevel slog.Leveler
}

// SlogHandler turns log records into breadcrumbs and events, on the hub
// in the record's context, and passes them on to the next handler.
type SlogHandler struct {
	next   slog.Handler
	crumb  slog.Leveler
	event  slog.Leveler
	attrs  []slog.Attr
	groups string
}

// NewSlogHandler wraps next (nil: records go to Fixwire only):
//
//	logger := slog.New(fixwire.NewSlogHandler(slog.NewJSONHandler(os.Stderr, nil), nil))
func NewSlogHandler(next slog.Handler, opts *SlogOptions) *SlogHandler {
	h := &SlogHandler{next: next, crumb: slog.LevelInfo, event: slog.LevelError}
	if opts != nil && opts.BreadcrumbLevel != nil {
		h.crumb = opts.BreadcrumbLevel
	}
	if opts != nil && opts.EventLevel != nil {
		h.event = opts.EventLevel
	}
	return h
}

// Enabled reports whether Fixwire or the next handler wants the level.
func (h *SlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= min(h.crumb.Level(), h.event.Level()) {
		return true
	}
	return h.next != nil && h.next.Enabled(ctx, level)
}

// Handle records r on the hub in ctx, then passes it on.
func (h *SlogHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= min(h.crumb.Level(), h.event.Level()) {
		h.record(ctx, r)
	}
	if h.next != nil && h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

func (h *SlogHandler) record(ctx context.Context, r slog.Record) {
	hub := HubFromContext(ctx)
	data := map[string]any{}
	var err error
	// The handler's attributes carry their groups already.
	for _, a := range h.attrs {
		h.flatten(data, "", a, &err)
	}
	r.Attrs(func(a slog.Attr) bool {
		h.flatten(data, h.groups, a, &err)
		return true
	})
	if r.Level < h.event.Level() {
		hub.AddBreadcrumb(Breadcrumb{Timestamp: r.Time, Type: "log", Category: "slog", Message: r.Message,
			Level: levelOf(r.Level), Data: data})
		return
	}
	c := hub.Client()
	if c == nil {
		return
	}
	e := &Event{Level: levelOf(r.Level), Message: r.Message, Extra: data, Timestamp: r.Time}
	if err != nil {
		frames := stack(c.opts)
		// The newest frames are slog's own.
		for len(frames) > 0 && strings.HasPrefix(frames[len(frames)-1].Module, "log/slog") {
			frames = frames[:len(frames)-1]
		}
		e.Exceptions = exceptionsOf(err, frames, Mechanism{Type: "slog", Handled: true}, c.opts)
	}
	if span := SpanFromContext(ctx); span != nil {
		e.TraceID, e.SpanID = span.TraceID, span.SpanID
	}
	c.capture(e, hub.Scope())
}

// flatten puts a (in groups) into data with dotted keys; the first error
// value is kept apart.
func (h *SlogHandler) flatten(data map[string]any, groups string, a slog.Attr, err *error) {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		prefix := groups
		if a.Key != "" {
			prefix += a.Key + "."
		}
		for _, g := range v.Group() {
			h.flatten(data, prefix, g, err)
		}
		return
	}
	if a.Key == "" {
		return
	}
	if e, ok := v.Any().(error); ok && *err == nil {
		*err = e
		data[groups+a.Key] = e.Error()
		return
	}
	data[groups+a.Key] = v.Any()
}

// WithAttrs returns a handler that adds attrs to every record.
func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr{}, h.attrs...), groupAttrs(h.groups, attrs)...)
	if h.next != nil {
		c.next = h.next.WithAttrs(attrs)
	}
	return &c
}

// WithGroup returns a handler that puts later attributes in a group.
func (h *SlogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.groups = h.groups + name + "."
	if h.next != nil {
		c.next = h.next.WithGroup(name)
	}
	return &c
}

// groupAttrs nests attrs in the dotted groups, so that attributes added
// before a later WithGroup keep their own.
func groupAttrs(groups string, attrs []slog.Attr) []slog.Attr {
	if groups == "" {
		return attrs
	}
	names := strings.Split(strings.TrimSuffix(groups, "."), ".")
	out := []slog.Attr{{Key: names[len(names)-1], Value: slog.GroupValue(attrs...)}}
	for i := len(names) - 2; i >= 0; i-- {
		out = []slog.Attr{{Key: names[i], Value: slog.GroupValue(out...)}}
	}
	return out
}

func levelOf(l slog.Level) Level {
	switch {
	case l >= slog.LevelError+4:
		return LevelFatal
	case l >= slog.LevelError:
		return LevelError
	case l >= slog.LevelWarn:
		return LevelWarning
	case l >= slog.LevelInfo:
		return LevelInfo
	}
	return LevelDebug
}

var _ slog.Handler = (*SlogHandler)(nil)
