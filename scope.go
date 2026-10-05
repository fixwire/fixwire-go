package fixwire

import (
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Scope holds what is known about the work under way (the user, tags,
// contexts, breadcrumbs, the request, the span), added to every event
// captured with it. A request gets its own scope from the fixwirehttp
// middleware.
type Scope struct {
	mu          sync.RWMutex
	user        User
	tags        map[string]string
	contexts    map[string]map[string]any
	extra       map[string]any
	breadcrumbs []Breadcrumb
	level       Level
	fingerprint []string
	transaction string
	request     *Request
	span        *Span
	session     *requestSession
}

// NewScope returns an empty scope.
func NewScope() *Scope {
	return &Scope{tags: map[string]string{}, contexts: map[string]map[string]any{}, extra: map[string]any{}}
}

// Clone copies the scope: changes to the copy don't reach the original.
func (s *Scope) Clone() *Scope {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := &Scope{
		user: s.user, tags: maps.Clone(s.tags), contexts: map[string]map[string]any{}, extra: maps.Clone(s.extra),
		breadcrumbs: slices.Clone(s.breadcrumbs), level: s.level, fingerprint: slices.Clone(s.fingerprint),
		transaction: s.transaction, request: s.request, span: s.span, session: s.session,
	}
	for k, v := range s.contexts {
		c.contexts[k] = maps.Clone(v)
	}
	return c
}

// SetUser sets who the work is for.
func (s *Scope) SetUser(u User) {
	s.mu.Lock()
	s.user = u
	s.mu.Unlock()
}

// SetTag sets a searchable tag.
func (s *Scope) SetTag(key, value string) {
	s.mu.Lock()
	s.tags[key] = value
	s.mu.Unlock()
}

// SetTags sets several tags.
func (s *Scope) SetTags(tags map[string]string) {
	s.mu.Lock()
	maps.Copy(s.tags, tags)
	s.mu.Unlock()
}

// RemoveTag removes a tag.
func (s *Scope) RemoveTag(key string) {
	s.mu.Lock()
	delete(s.tags, key)
	s.mu.Unlock()
}

// SetContext sets a named group of details, such as "order"; nil removes
// it.
func (s *Scope) SetContext(name string, values map[string]any) {
	s.mu.Lock()
	if values == nil {
		delete(s.contexts, name)
	} else {
		s.contexts[name] = maps.Clone(values)
	}
	s.mu.Unlock()
}

// SetExtra sets a detail sent as it is.
func (s *Scope) SetExtra(key string, value any) {
	s.mu.Lock()
	s.extra[key] = value
	s.mu.Unlock()
}

// SetLevel sets the level of the events captured with the scope.
func (s *Scope) SetLevel(l Level) {
	s.mu.Lock()
	s.level = l
	s.mu.Unlock()
}

// SetFingerprint groups the events captured with the scope your way.
func (s *Scope) SetFingerprint(fp []string) {
	s.mu.Lock()
	s.fingerprint = slices.Clone(fp)
	s.mu.Unlock()
}

// SetTransaction names the route or task.
func (s *Scope) SetTransaction(name string) {
	s.mu.Lock()
	s.transaction = name
	s.mu.Unlock()
}

// SetRequest records the HTTP request the work serves. Without
// SendDefaultPII, only headers that can't identify the user are kept.
func (s *Scope) SetRequest(r *http.Request, sendPII bool) {
	if r == nil {
		return
	}
	req := &Request{Method: r.Method, Query: r.URL.RawQuery, Headers: map[string]string{}, src: r}
	if sendPII {
		req.ip = clientIP(r)
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	req.URL = scheme + "://" + r.Host + r.URL.Path
	for name, values := range r.Header {
		if len(values) == 0 || (!sendPII && sensitiveHeader(name)) {
			continue
		}
		req.Headers[name] = values[0]
	}
	s.mu.Lock()
	s.request = req
	s.mu.Unlock()
}

// sensitiveHeader reports whether a header may identify the user or
// carry a secret.
// clientIP is the address the request came from: the first of
// X-Forwarded-For, else X-Real-IP, else the connection's.
func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		first, _, _ := strings.Cut(f, ",")
		return strings.TrimSpace(first)
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func sensitiveHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "cookie", "set-cookie", "x-forwarded-for", "x-real-ip", "proxy-authorization", "x-api-key":
		return true
	}
	return false
}

// AddBreadcrumb records something that happened; the oldest go past max.
func (s *Scope) AddBreadcrumb(b Breadcrumb, max int) {
	if b.Timestamp.IsZero() {
		b.Timestamp = time.Now()
	}
	if b.Level == "" {
		b.Level = LevelInfo
	}
	s.mu.Lock()
	s.breadcrumbs = append(s.breadcrumbs, b)
	if over := len(s.breadcrumbs) - max; over > 0 {
		s.breadcrumbs = slices.Delete(s.breadcrumbs, 0, over)
	}
	s.mu.Unlock()
}

// ClearBreadcrumbs forgets the breadcrumbs.
func (s *Scope) ClearBreadcrumbs() {
	s.mu.Lock()
	s.breadcrumbs = nil
	s.mu.Unlock()
}

// SetSpan makes span the scope's current span: events captured with the
// scope link to its trace.
func (s *Scope) SetSpan(span *Span) {
	s.mu.Lock()
	s.span = span
	s.mu.Unlock()
}

// Span is the scope's current span, or nil.
func (s *Scope) Span() *Span {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.span
}

// applyTo fills in what the event doesn't say itself.
func (s *Scope) applyTo(e *Event) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e.User == (User{}) {
		e.User = s.user
	}
	if len(s.tags) > 0 {
		tags := maps.Clone(s.tags)
		maps.Copy(tags, e.Tags)
		e.Tags = tags
	}
	if len(s.contexts) > 0 {
		contexts := map[string]map[string]any{}
		maps.Copy(contexts, s.contexts)
		maps.Copy(contexts, e.Contexts)
		e.Contexts = contexts
	}
	if len(s.extra) > 0 {
		extra := maps.Clone(s.extra)
		maps.Copy(extra, e.Extra)
		e.Extra = extra
	}
	if len(e.Breadcrumbs) == 0 {
		e.Breadcrumbs = slices.Clone(s.breadcrumbs)
	}
	if e.Level == "" {
		e.Level = s.level
	}
	if len(e.Fingerprint) == 0 {
		e.Fingerprint = slices.Clone(s.fingerprint)
	}
	if e.Transaction == "" {
		e.Transaction = s.transaction
	}
	if e.Transaction == "" {
		e.Transaction = s.request.route()
	}
	if e.Request == nil {
		e.Request = s.request
	}
	if e.TraceID == "" && s.span != nil {
		e.TraceID, e.SpanID = s.span.TraceID, s.span.SpanID
	}
}
