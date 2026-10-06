package fixwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// What the SDK does with hostile input and a misbehaving server: it stays
// bounded, and never takes the app down.

func TestNoRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
	}))
	t.Cleanup(other.Close)
	userClient := &http.Client{}
	h, f := testClient(t, Options{HTTPClient: userClient})
	f.answer = func(int, *http.Request) (int, http.Header) {
		return http.StatusTemporaryRedirect, http.Header{"Location": {other.URL + "/v1/logs"}}
	}
	h.CaptureMessage("to the DSN's host only")
	flush(t, h)
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("a redirect was followed %d times, with the key", n)
	}
	if got := len(f.requests("")); got != 1 {
		t.Errorf("%d sends of a redirected request, want 1 (dropped)", got)
	}
	if userClient.CheckRedirect != nil {
		t.Error("the app's client was changed")
	}
}

func TestRetryAfterBounds(t *testing.T) {
	backoffUnit = 10 * time.Millisecond
	t.Cleanup(func() { backoffUnit = time.Second })
	h, f := testClient(t, Options{})
	f.answer = func(n int, r *http.Request) (int, http.Header) {
		// Past maxWait (and past what a Duration holds): dropped, not parked.
		return http.StatusServiceUnavailable, http.Header{"Retry-After": {"99999999999999999"}}
	}
	h.CaptureMessage("unavailable")
	flush(t, h)
	if got := len(f.requests("")); got != 1 {
		t.Errorf("%d sends", got)
	}
	// A 5xx with Retry-After pauses everything, for a day at most.
	tr := h.Client().transport
	if wait := tr.pausedFor(categoryFeedback, time.Now()); wait <= maxPause-time.Minute || wait > maxPause {
		t.Errorf("a 503 paused everything for %s", wait)
	}

	h, _ = testClient(t, Options{})
	tr = h.Client().transport
	now := time.Now()
	tr.limit("99999999999999999:error, 60:made-up;also-made-up, -5:log, x:span, 86401:session", now)
	tr.mu.Lock()
	until, sessions, paused := tr.until["error"], tr.until["session"], len(tr.until)
	tr.mu.Unlock()
	if until.Sub(now) != maxPause || sessions.Sub(now) != maxPause || paused != 2 {
		t.Errorf("paused %v for %s", tr.until, until.Sub(now))
	}
	for in, want := range map[string]time.Duration{"120": 2 * time.Minute, "0": 0, "86400": maxPause, "86401": maxPause,
		"99999999999999999999": maxPause, "-1": 0, "+5": 0, "soon": 0, "Wed, 21 Oct 2026 07:28:00 GMT": 0} {
		if got, _ := seconds(in); got != want {
			t.Errorf("seconds(%q) = %s", in, got)
		}
	}
	// Retry-After may be an HTTP date.
	for in, want := range map[string]time.Duration{
		now.Add(90 * time.Second).UTC().Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Hour).UTC().Format(http.TimeFormat):       0,
		now.Add(48 * time.Hour).UTC().Format(http.TimeFormat):   maxPause,
		"Wed, 21 Oct 2026 07:28:00":                             -1, "120": 2 * time.Minute,
	} {
		got, ok := retryAfterOf(in, now)
		if want < 0 && ok || want >= 0 && (got < want-time.Second || got > want) {
			t.Errorf("retryAfterOf(%q) = %s, %v", in, got, ok)
		}
	}
}

// A 429 without Fixwire-Rate-Limits pauses everything for Retry-After, a
// minute at least.
func TestTooManyRequestsPausesAll(t *testing.T) {
	for retryAfter, want := range map[string]time.Duration{"": time.Minute, "5": time.Minute, "600": 10 * time.Minute,
		time.Now().Add(20 * time.Minute).UTC().Format(http.TimeFormat): 20 * time.Minute} {
		h, f := testClient(t, Options{})
		f.answer = func(int, *http.Request) (int, http.Header) {
			return http.StatusTooManyRequests, http.Header{"Retry-After": {retryAfter}}
		}
		h.CaptureMessage("limited")
		tr := h.Client().transport
		deadline := time.Now().Add(5 * time.Second)
		for len(f.requests("")) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		if wait := tr.pausedFor(categoryCheckIn, time.Now()); wait < want-5*time.Second || wait > want {
			t.Errorf("Retry-After %q paused everything for %s, want %s", retryAfter, wait, want)
		}
		h.Client().Close(0)
	}
}

// A request without an answer, or with a 5xx, is tried 3 more times, about
// 1, 2 and 4 units apart.
func TestRetries(t *testing.T) {
	backoffUnit = 20 * time.Millisecond
	t.Cleanup(func() { backoffUnit = time.Second })
	h, f := testClient(t, Options{})
	var mu sync.Mutex
	var at []time.Time
	f.answer = func(int, *http.Request) (int, http.Header) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		return http.StatusBadGateway, nil
	}
	h.CaptureMessage("down")
	flush(t, h)
	mu.Lock()
	defer mu.Unlock()
	if len(at) != maxAttempts {
		t.Fatalf("%d sends, want 1 and 3 retries", len(at))
	}
	for i := 1; i < len(at); i++ {
		if gap, want := at[i].Sub(at[i-1]), backoffUnit<<(i-1); gap < want || gap > want+time.Second {
			t.Errorf("retry %d after %s, want %s", i, gap, want)
		}
	}
}

// A 429's retry counts toward the same 4 sends as those after a 5xx.
func TestRetriesCountTooManyRequests(t *testing.T) {
	backoffUnit = 10 * time.Millisecond
	t.Cleanup(func() { backoffUnit = time.Second })
	limited := http.Header{"Fixwire-Rate-Limits": {"0:error"}} // a pause that ends at once
	for name, answer := range map[string]func(int, *http.Request) (int, http.Header){
		"429s": func(int, *http.Request) (int, http.Header) { return http.StatusTooManyRequests, limited },
		"429s and 5xxs": func(n int, _ *http.Request) (int, http.Header) {
			if n%2 == 0 {
				return http.StatusTooManyRequests, limited
			}
			return http.StatusServiceUnavailable, nil
		},
		"a 429 last": func(n int, _ *http.Request) (int, http.Header) {
			if n == 3 {
				return http.StatusTooManyRequests, limited
			}
			return http.StatusBadGateway, nil
		},
	} {
		h, f := testClient(t, Options{})
		f.answer = answer
		h.CaptureMessage("limited")
		flush(t, h)
		if got := len(f.requests("/v1/logs")); got != 4 {
			t.Errorf("%s: %d sends, want 4 in all", name, got)
		}
	}
}

// During an outage, as many requests wait to be sent again as the queue
// holds; Close drops them at once.
func TestWaitingRequestsBounded(t *testing.T) {
	h, _ := testClient(t, Options{MaxQueue: 3})
	c := h.Client()
	tr := c.transport
	for range 10 {
		tr.mu.Lock()
		tr.pending++
		tr.mu.Unlock()
		tr.later(&request{category: categoryError}, time.Hour)
	}
	tr.mu.Lock()
	waiting := len(tr.waiting)
	tr.mu.Unlock()
	if waiting != 3 {
		t.Errorf("%d requests waiting, want 3", waiting)
	}
	c.Close(time.Millisecond)
	if !tr.flush(0) || len(tr.waiting) != 0 {
		t.Errorf("after Close: %d pending, %d waiting", tr.pending, len(tr.waiting))
	}
	if c.CaptureCheckIn(CheckIn{Monitor: "late"}) != "" {
		t.Error("sent after Close")
	}
}

// Flush and Close return in time with a server that never answers, and
// leave nothing behind.
func TestFlushAndCloseBounded(t *testing.T) {
	release := make(chan struct{})
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(stuck.Close)
	t.Cleanup(func() { close(release) })
	c, err := NewClient(Options{DSN: strings.Replace(stuck.URL, "http://", "http://k@", 1), HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHub(c, nil)
	h.CaptureMessage("stuck")
	before := runtime.NumGoroutine()
	for range 50 {
		if h.Flush(time.Millisecond) {
			t.Fatal("flushed with a request in flight")
		}
	}
	if n := runtime.NumGoroutine() - before; n > 5 {
		t.Errorf("%d goroutines left by timed-out flushes", n)
	}
	start := time.Now()
	c.Close(50 * time.Millisecond)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Close took %s", took)
	}
	if !h.Flush(0) {
		t.Error("requests pending after Close")
	}
}

// The SDK's debug lines go to stderr: never through the log package to a
// slog handler of the SDK's, where they would be captured, and logged, …
func TestDebugLinesNotCaptured(t *testing.T) {
	var debug bytes.Buffer
	debugLog = log.New(&debug, "", 0)
	t.Cleanup(func() { debugLog = log.New(os.Stderr, "", log.LstdFlags) })
	h, f := testClient(t, Options{Debug: true, ErrorBudget: ErrorBudget{PerIssueBurst: 1}})
	bound := CurrentHub().Client()
	CurrentHub().BindClient(h.Client())
	logger, writer, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(NewSlogHandler(nil, &SlogOptions{EventLevel: slog.LevelInfo})))
	t.Cleanup(func() {
		CurrentHub().BindClient(bound)
		slog.SetDefault(logger)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
	CaptureMessage("again")
	CaptureMessage("again") // over the budget: a debug line
	flush(t, h)
	if !strings.Contains(debug.String(), "over the error budget") {
		t.Errorf("debug lines %q", debug.String())
	}
	if got := len(f.requests("/v1/logs")); got != 1 {
		t.Errorf("%d events, want the one captured", got)
	}
}

type loudValue struct{}

func (loudValue) String() string               { panic("String failed") }
func (loudValue) MarshalJSON() ([]byte, error) { panic("MarshalJSON failed") }

// Errors and values whose own methods panic (a nil *cartError's Error)
// don't take the app down.
func TestPanickingMethods(t *testing.T) {
	for _, opts := range []Options{{TracesSampleRate: 1}, {TracesSampleRate: 1, DisableRedaction: true}} {
		h, _ := testClient(t, opts)
		var typedNil *cartError
		if h.CaptureException(typedNil) != "" {
			t.Error("captured a nil error's message")
		}
		ctx := NewContext(context.Background(), h)
		slog.New(NewSlogHandler(nil, nil)).ErrorContext(ctx, "failed", "err", typedNil)
		h.Scope().SetExtra("v", loudValue{})
		h.CaptureMessage("loud")
		span, _ := StartSpan(ctx, "job", WithAttributes(map[string]any{"v": loudValue{}}))
		span.Finish()
		flush(t, h)
	}
}

// A value without a JSON form costs itself, not the event.
func TestNonFiniteFloats(t *testing.T) {
	for _, opts := range []Options{{}, {DisableRedaction: true}} {
		h, f := testClient(t, opts)
		h.Scope().SetExtra("ratio", math.NaN())
		h.Scope().SetExtra("ceiling", math.Inf(1))
		h.CaptureException(errors.New("division"))
		flush(t, h)
		recs, _ := logRecords(t, f.requests("/v1/logs"))
		if len(recs) != 1 {
			t.Fatalf("records %v", recs)
		}
		a := kv(recs[0]["attributes"])
		if a["exception.message"] != "division" || a["ratio"] != "NaN" || a["ceiling"] != "Infinity" {
			t.Errorf("redaction off %v: %v", opts.DisableRedaction, a)
		}
	}
}

func TestCyclicValues(t *testing.T) {
	h, f := testClient(t, Options{DisableRedaction: true})
	loop := map[string]any{}
	loop["a"], loop["b"], loop["c"] = loop, loop, []any{loop}
	h.Scope().SetExtra("loop", loop)
	deep := map[string]any{}
	for range 1000 {
		deep = map[string]any{"in": deep}
	}
	h.Scope().SetExtra("deep", deep)
	h.CaptureMessage("cycle")
	flush(t, h)
	recs, _ := logRecords(t, f.requests("/v1/logs"))
	if len(recs) != 1 {
		t.Fatalf("records %v", recs)
	}
	a := kv(recs[0]["attributes"])
	if loop := a["loop"].(map[string]any); loop["a"] != "[Circular ~]" || loop["c"].([]any)[0] != "[Circular ~]" {
		t.Errorf("loop %v", a["loop"])
	}
	// 10 levels of maps, the 11th "[Object]".
	level := a["deep"]
	for range maxDepth - 1 {
		level = level.(map[string]any)["in"]
	}
	if level.(map[string]any)["in"] != "[Object]" {
		t.Errorf("the 11th level is %v", level)
	}
}

type node struct {
	Name     string  `json:"name"`
	Next     *node   `json:"next,omitempty"`
	Children []*node `json:"children,omitempty"`
	secret   string
}

type unreadableValue struct{}

func (unreadableValue) MarshalJSON() ([]byte, error) { panic("MarshalJSON failed") }

// Values are walked within bounds: 10 levels, 100 items, 10,000 maps and
// lists; what can't be read is "[Unreadable]".
func TestValueBounds(t *testing.T) {
	h, _ := testClient(t, Options{})
	c := h.Client()

	wide := make([]int, 1000)
	if got := c.plain(wide).([]any); len(got) != maxBreadth {
		t.Errorf("%d items of 1000", len(got))
	}
	wideMap := map[int]bool{}
	for i := range 1000 {
		wideMap[i] = true
	}
	if got := c.plain(wideMap).(map[string]any); len(got) != maxBreadth {
		t.Errorf("%d keys of 1000", len(got))
	}

	ring := &node{Name: "a", secret: "x"}
	ring.Next = &node{Name: "b", Next: ring}
	got := c.plain(ring).(map[string]any)
	if got["name"] != "a" || got["next"].(map[string]any)["next"] != "[Circular ~]" || got["secret"] != nil {
		t.Errorf("ring %v", got)
	}

	deep := []any{}
	for range 20 {
		deep = []any{deep}
	}
	level := c.plain(deep)
	for range maxDepth {
		level = level.([]any)[0]
	}
	if level != "[Array]" {
		t.Errorf("the 11th level is %v", level)
	}

	// Shared children: the walk stops at 10,000 maps and lists.
	shared := &node{Name: "leaf"}
	for range 9 {
		parent := &node{}
		for range 10 {
			parent.Children = append(parent.Children, shared)
		}
		shared = parent
	}
	start := time.Now()
	walked := 0
	var count func(v any)
	count = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			walked++
			for _, e := range x {
				count(e)
			}
		case []any:
			walked++
			for _, e := range x {
				count(e)
			}
		}
	}
	count(c.plain(shared))
	if walked > maxObjects || time.Since(start) > 5*time.Second {
		t.Errorf("walked %d maps and lists in %s", walked, time.Since(start))
	}

	if got := c.plain(map[string]any{"v": unreadableValue{}, "ok": 1}).(map[string]any); got["v"] != unreadable || got["ok"] != int64(1) {
		t.Errorf("unreadable %v", got)
	}
	for v, want := range map[float64]string{math.NaN(): "NaN", math.Inf(1): "Infinity", math.Inf(-1): "-Infinity"} {
		if got := c.plain(v); got != want {
			t.Errorf("%v is %v", v, got)
		}
	}
	if got := c.plain([]any{float32(0.1), 2.5, uint8(7), time.Duration(3)}); fmt.Sprint(got) != "[0.1 2.5 7 3]" {
		t.Errorf("numbers %v", got)
	}
}

func TestPropagatedHeadersBounded(t *testing.T) {
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	for _, c := range []struct{ tracestate, baggage string }{
		{strings.Repeat("a", 513), strings.Repeat("b", 8193)},
		{"fw=1\r\nX-Injected: 1", "user=1\nX-Injected: 1"},
		{"fw=1\x00", "user=1\x7f"},
		{"fw=1,\x08other=2", "user=1,\vplan=team"}, // the control characters either side of tab
		{"fw=1,\t" + strings.Repeat("a", 507), "user=1,\t" + strings.Repeat("b", 8185)},
	} {
		s, _ := StartSpan(context.Background(), "GET /", ContinueTrace(parent, c.tracestate, c.baggage))
		if s.TraceID == "" || s.Tracestate() != "" || s.Baggage() != "" {
			t.Errorf("passed on %q, %q", s.Tracestate(), s.Baggage())
		}
	}
	// At the limits, they go on whole; a tab is W3C's list whitespace.
	for _, c := range []struct{ tracestate, baggage string }{
		{"fw=" + strings.Repeat("a", 509), "user=" + strings.Repeat("b", 8187)},
		{"fw=1,\t" + strings.Repeat("a", 506), "user=1,\t" + strings.Repeat("b", 8184)},
		{"fw=1 ,\tother=2\t", "user=1,\tplan=team"},
	} {
		s, _ := StartSpan(context.Background(), "GET /", ContinueTrace(parent, c.tracestate, c.baggage))
		if len(c.tracestate) > 512 || len(c.baggage) > 8192 || s.Tracestate() != c.tracestate || s.Baggage() != c.baggage {
			t.Errorf("passed on %d of %d bytes, %d of %d", len(s.Tracestate()), len(c.tracestate), len(s.Baggage()), len(c.baggage))
		}
	}
}

func TestTraceparentStrict(t *testing.T) {
	const trace, parent = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	if tr, p, sampled, ok := parseTraceparent(" 00-" + trace + "-" + parent + "-01 "); !ok || tr != trace || p != parent || !sampled {
		t.Errorf("a well-formed traceparent: %s %s %v %v", tr, p, sampled, ok)
	}
	for _, bad := range []string{
		"01-" + trace + "-" + parent + "-01",                  // a version but 00
		"00-" + trace + "-" + parent + "-01-extra",            // more than version 00 holds
		"00-" + strings.ToUpper(trace) + "-" + parent + "-01", // upper-case hex
		"00-" + trace + "-" + parent + "-0g",
		"00-" + trace + "-" + parent + "-1",
		"00-" + trace + "-" + parent[:15] + "-01",
		"00-" + trace[:31] + "-" + parent + "-01",
		"00-" + trace + "-+0f067aa0ba902b7-01",
	} {
		if _, _, _, ok := parseTraceparent(bad); ok {
			t.Errorf("parseTraceparent(%q) accepted", bad)
		}
	}
}

// Trace headers go to a target's host and its subdomains, or to the URLs
// starting with a target, compared without user info, query and fragment.
func TestPropagationTargets(t *testing.T) {
	c := &Client{opts: Options{TracePropagationTargets: []string{
		"example.com", "internal.test:8443", "https://api.partner.io/v2", "/same-origin", "",
	}}}
	for u, want := range map[string]bool{
		"https://example.com/x":                      true,
		"https://API.Example.COM/x":                  true,
		"http://example.com:8080/x":                  true,
		"https://badexample.com/x":                   false,
		"https://example.com.evil.net/x":             false,
		"https://evil.net/?next=example.com":         false,
		"https://evil.net/#example.com":              false,
		"https://example.com@evil.net/":              false,
		"https://internal.test:8443/":                true,
		"https://svc.internal.test:8443/":            true,
		"https://internal.test/":                     false,
		"https://internal.test:9000/":                false,
		"https://api.partner.io/v2/orders?id=1":      true,
		"https://user:pw@api.partner.io/v2/x":        true,
		"https://api.partner.io/v1/orders":           false,
		"https://evil.net/https://api.partner.io/v2": false,
		"https://evil.net/same-origin":               false,
		"/same-origin/x":                             false,
		"not a url":                                  false,
	} {
		if got := c.ShouldPropagate(u); got != want {
			t.Errorf("ShouldPropagate(%q) = %v", u, got)
		}
	}
	if (&Client{opts: Options{TracePropagationTargets: []string{"[::1]:8080"}}}).ShouldPropagate("http://[::1]:8080/") != true {
		t.Error("an IPv6 host with its port")
	}
	if (&Client{}).ShouldPropagate("https://example.com/") {
		t.Error("no targets, yet trace headers")
	}
}

// Strings are at most MaxValueLength bytes of UTF-8, cut on a character
// boundary and ending in "...", masked before the cut.
func TestStringsCut(t *testing.T) {
	h, f := testClient(t, Options{TracesSampleRate: 1})
	c := h.Client()
	exact := strings.Repeat("é", 512) // 1024 bytes
	over := "a" + exact               // 1025 bytes, the cut inside an é
	for in, want := range map[string]string{
		exact:                     exact,
		over:                      "a" + strings.Repeat("é", 510) + "...",
		strings.Repeat("x", 1025): strings.Repeat("x", 1021) + "...",
		"short":                   "short",
	} {
		got := c.text(in)
		if got != want || len(got) > 1024 || !utf8.ValidString(got) {
			t.Errorf("%d bytes cut to %d: %q", len(in), len(got), got[max(0, len(got)-12):])
		}
	}

	// A secret the cut goes through is masked first: none of it is sent.
	key := "-----BEGIN " + "RSA PRIVATE KEY-----\n" + strings.Repeat("MIIEowIBAAKCAQEA", 120) + "\n-----END " + "RSA PRIVATE KEY-----"
	jwt := "eyJ" + "hbGciOiJIUzI1NiJ9." + "eyJ" + "zdWIiOiIxMjM0NTY3ODkwIn0." + strings.Repeat("dozjgNryP4J3jVmNHl0w5N", 3)
	pad := strings.Repeat("p", 950) + " " // the cut falls inside the secret
	h.Scope().SetExtra("key", pad+key)
	h.Scope().SetExtra("nested", map[string]any{"jwt": []any{pad + jwt}})
	h.CaptureMessage(pad + key)
	span, _ := StartSpan(NewContext(context.Background(), h), pad+jwt)
	span.SetError(errors.New(pad + key))
	span.SetAttribute("long", over)
	span.Finish()
	flush(t, h)
	recs, _ := logRecords(t, f.requests("/v1/logs"))
	a := kv(recs[0]["attributes"])
	sent := []string{plain(recs[0]["body"].(map[string]any)).(string), a["key"].(string),
		a["nested"].(map[string]any)["jwt"].([]any)[0].(string)}
	sp := spans(t, f.requests("/v1/traces"))[0]
	sent = append(sent, sp["name"].(string), sp["status"].(map[string]any)["message"].(string))
	for _, s := range sent {
		if !strings.HasPrefix(s, pad+"[REDACTED:") || len(s) > 1024 || strings.Contains(s, "MIIE") || strings.Contains(s, "eyJ") {
			t.Errorf("sent %d bytes: %q", len(s), s[min(len(s), 940):])
		}
	}
	if long := kv(sp["attributes"])["long"].(string); long != "a"+strings.Repeat("é", 510)+"..." {
		t.Errorf("a span attribute of %d bytes", len(long))
	}

	// MaxValueLength is the app's to set.
	h, _ = testClient(t, Options{MaxValueLength: 10})
	if got := h.Client().text("abcdefghijklmnop"); got != "abcdefg..." {
		t.Errorf("MaxValueLength 10: %q", got)
	}
}

// The app's own configuration (release, environment, service and server
// names, a monitor's slug and config) is cut to MaxValueLength but sent as
// given; feedback is the app's data, masked and cut.
func TestConfigurationCutNotMasked(t *testing.T) {
	long, want := strings.Repeat("x", 1025), strings.Repeat("x", 1021)+"..."
	release, environment := "api@1.2.3.example", "ops@example.com"
	h, f := testClient(t, Options{Release: release, Environment: environment, ServerName: long, ServiceName: "svc-" + long})
	if masked := h.Client().text(release); masked == release {
		t.Fatalf("%q is not masked as data either: the test shows nothing", release)
	}
	h.CaptureMessage("m")
	end := h.Clone().StartRequestSession()
	end()
	monitor, schedule, timezone := environment+"-"+long, "0 3 * * * "+long, environment+long
	h.Client().CaptureCheckIn(CheckIn{Monitor: monitor, Config: &MonitorConfig{Schedule: CrontabSchedule(schedule), Timezone: timezone}})
	h.CaptureFeedback(Feedback{Message: "mail ada@example.com " + long, Name: long, Email: "ada@example.com",
		URL: "https://shop.example/?q=" + long, Source: long, Score: 1})
	flush(t, h)

	_, resource := logRecords(t, f.requests("/v1/logs"))
	if resource["service.version"] != release || resource["deployment.environment.name"] != environment ||
		resource["host.name"] != want || resource["service.name"] != "svc-"+strings.Repeat("x", 1017)+"..." {
		t.Errorf("resource %v", resource)
	}
	if s := f.requests("/v1/sessions"); len(s) != 1 || s[0].body["release"] != release || s[0].body["environment"] != environment {
		t.Errorf("sessions %v", s)
	}
	checkIns := f.requests("/v1/check-ins/" + monitor[:1021] + "...")
	if len(checkIns) != 1 {
		t.Fatalf("check-ins %v", len(f.requests("")))
	}
	config := checkIns[0].body["monitor_config"].(map[string]any)
	if checkIns[0].body["environment"] != environment || config["timezone"] != timezone[:1021]+"..." ||
		config["schedule"].(map[string]any)["value"] != schedule[:1021]+"..." {
		t.Errorf("check-in %v", checkIns[0].body)
	}
	fb := f.requests("/v1/feedback")
	if len(fb) != 1 {
		t.Fatalf("feedback %v", fb)
	}
	b := fb[0].body
	if msg, _ := b["message"].(string); !strings.HasPrefix(msg, "mail [REDACTED:email] x") || len(msg) != 1024 {
		t.Errorf("feedback message of %d bytes: %.40q", len(msg), msg)
	}
	if u, _ := b["url"].(string); len(u) > 1024 || !strings.HasSuffix(u, "...") {
		t.Errorf("feedback url of %d bytes", len(u))
	}
	if b["name"] != want || b["email"] != "[REDACTED:email]" || b["source"] != want ||
		b["release"] != release || b["environment"] != environment {
		t.Errorf("feedback %v", b)
	}
}

// An error or a message over 1 MB leaves out its breadcrumbs, then its
// contexts; still over, it is dropped.
func TestLargeEvents(t *testing.T) {
	kb := strings.Repeat("k", 1000)
	big := map[string]any{} // 300 kB
	for i := range 100 {
		big[fmt.Sprint("f", i)] = []any{kb, kb, kb}
	}
	h, f := testClient(t, Options{})
	for range 5 {
		h.AddBreadcrumb(Breadcrumb{Message: "step", Data: big})
	}
	h.Scope().SetContext("small", map[string]any{"id": 1})
	h.CaptureMessage("crumbs too large")
	h.Scope().ClearBreadcrumbs()
	for i := range 5 {
		h.Scope().SetContext(fmt.Sprint("big", i), big)
	}
	h.CaptureMessage("contexts too large")
	flush(t, h)
	recs, _ := logRecords(t, f.requests("/v1/logs"))
	if len(recs) != 2 {
		t.Fatalf("%d records", len(recs))
	}
	if a := kv(recs[0]["attributes"]); a["fixwire.breadcrumbs"] != nil || a["fixwire.contexts"] == nil {
		t.Errorf("breadcrumbs kept or contexts lost: %v", a["fixwire.contexts"])
	}
	if a := kv(recs[1]["attributes"]); a["fixwire.contexts"] != nil || plain(recs[1]["body"].(map[string]any)) != "contexts too large" {
		t.Errorf("contexts kept")
	}

	h, f = testClient(t, Options{})
	for i := range 5 {
		h.Scope().SetExtra(fmt.Sprint("x", i), big)
	}
	if h.CaptureMessage("extras too large") != "" {
		t.Error("an event over 1 MB was sent")
	}
	flush(t, h)
	if n := len(f.requests("")); n != 0 {
		t.Errorf("%d requests", n)
	}
}

// Spans go 100 to a request of at most 5 MB; one that can't fit is dropped
// alone. A span holds at most 128 attributes.
func TestSpanRequestsBounded(t *testing.T) {
	h, f := testClient(t, Options{TracesSampleRate: 1})
	ctx := NewContext(context.Background(), h)
	root, ctx := StartSpan(ctx, "job", WithOp("task"))
	for i := range 300 {
		child, _ := StartSpan(ctx, fmt.Sprint("step ", i))
		child.Finish()
	}
	huge, _ := StartSpan(ctx, "huge")
	kb := strings.Repeat("k", 1000)
	list := make([]any, 100)
	for i := range list {
		list[i] = kb
	}
	for i := range 200 {
		huge.SetAttribute(fmt.Sprint("a", i), list)
	}
	huge.Finish()
	root.Finish()
	flush(t, h)
	reqs := f.requests("/v1/traces")
	total := 0
	for _, r := range reqs {
		n := len(spans(t, []received{r}))
		total += n
		if n > maxItems {
			t.Errorf("a request of %d spans", n)
		}
	}
	if total != 301 || len(reqs) != 4 {
		t.Errorf("%d spans in %d requests, want 301 (the huge one dropped) in 4", total, len(reqs))
	}

	s, _ := StartSpan(ctx, "wide")
	for i := range 200 {
		s.SetAttribute(fmt.Sprint("a", i), i)
	}
	s.SetAttribute("a0", "kept")
	if len(s.attrs) != maxAttributes-1 || s.attrs["a0"] != "kept" {
		t.Errorf("%d attributes", len(s.attrs))
	}
}

// What the app logs while the SDK captures (from BeforeSend, from an error's
// Error method) is not captured again; other goroutines' records still are.
func TestSlogReentrance(t *testing.T) {
	logger := slog.New(NewSlogHandler(nil, nil)) // to the current hub
	var calls atomic.Int32
	hub, f := testClient(t, Options{BeforeSend: func(e *Event) *Event {
		calls.Add(1)
		logger.Error("logged from BeforeSend")
		return e
	}, BeforeBreadcrumb: func(b *Breadcrumb) *Breadcrumb {
		logger.Info("logged from BeforeBreadcrumb")
		return b
	}})
	bound := CurrentHub().Client()
	CurrentHub().BindClient(hub.Client())
	t.Cleanup(func() { CurrentHub().BindClient(bound) })
	h := CurrentHub()

	h.CaptureException(&noisyError{logger})
	h.AddBreadcrumb(Breadcrumb{Message: "crumb"})
	logger.Error("from the app")
	flush(t, h)
	if n := len(f.requests("/v1/logs")); n != 2 || calls.Load() != 2 {
		t.Errorf("%d events, BeforeSend ran %d times; want the error's and the app's record", n, calls.Load())
	}

	// A capture under way elsewhere doesn't hold back this goroutine's.
	release, entered := make(chan struct{}), make(chan struct{})
	go func() {
		defer enterCapture()()
		close(entered)
		<-release
	}()
	<-entered
	if inCapture() {
		t.Error("another goroutine's capture marked this one")
	}
	close(release)
}

type noisyError struct{ logger *slog.Logger }

func (e *noisyError) Error() string {
	e.logger.Error("logged from Error")
	return "noisy"
}

func TestBeforeBreadcrumbPanics(t *testing.T) {
	h, _ := testClient(t, Options{BeforeBreadcrumb: func(*Breadcrumb) *Breadcrumb { panic("app bug") }})
	h.AddBreadcrumb(Breadcrumb{Message: "x"})
	if len(h.Scope().breadcrumbs) != 0 || inCapture() {
		t.Error("a breadcrumb went past a panicking BeforeBreadcrumb, or the goroutine stayed marked")
	}
}

// A sessions request holds at most 5000 aggregates.
func TestSessionRequestsBounded(t *testing.T) {
	h, f := testClient(t, Options{Release: "shop@1.0.0"})
	a := h.Client().sessions
	now := time.Now()
	for i := range maxAggregates + 10 {
		a.record("exited", deviceID(User{ID: fmt.Sprint(i)}), now)
		a.record("exited", "", now.Add(time.Duration(i)*time.Minute)) // counts without a user, a minute each
	}
	a.mu.Lock()
	counted := len(a.buckets)
	a.mu.Unlock()
	a.send()
	flush(t, h)
	reqs := f.requests("/v1/sessions")
	total := 0
	for _, r := range reqs {
		n := len(r.body["aggregates"].([]any))
		total += n
		if n > maxAggregates {
			t.Errorf("a request of %d aggregates", n)
		}
	}
	if counted <= maxAggregates || total != counted || len(reqs) != 2 {
		t.Errorf("%d of %d aggregates sent in %d requests", total, counted, len(reqs))
	}
}

func TestBreadcrumbsBounded(t *testing.T) {
	s := NewScope()
	start := time.Now()
	for i := range 50000 {
		s.AddBreadcrumb(Breadcrumb{Message: fmt.Sprint(i)}, 10000)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("50000 breadcrumbs took %s", took)
	}
	if len(s.breadcrumbs) != 10000 || s.breadcrumbs[0].Message != "40000" || s.breadcrumbs[9999].Message != "49999" {
		t.Fatalf("%d breadcrumbs, from %s", len(s.breadcrumbs), s.breadcrumbs[0].Message)
	}

	// A clone shares them until either adds one.
	s = NewScope()
	for i := range 3 {
		s.AddBreadcrumb(Breadcrumb{Message: fmt.Sprint(i)}, 3)
	}
	c := s.Clone()
	s.AddBreadcrumb(Breadcrumb{Message: "original"}, 3)
	c.AddBreadcrumb(Breadcrumb{Message: "clone"}, 3)
	if got := []string{s.breadcrumbs[0].Message, s.breadcrumbs[2].Message, c.breadcrumbs[0].Message, c.breadcrumbs[2].Message}; fmt.Sprint(got) != "[1 original 1 clone]" {
		t.Errorf("breadcrumbs %v", got)
	}
}

func TestBudgetBounded(t *testing.T) {
	b := newBudget(ErrorBudget{})
	now := time.Now()
	start := time.Now()
	for i := range 50000 {
		b.allow(fmt.Sprint("issue-", i), now)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("50000 issues took %s", took)
	}
	if len(b.issues) != maxIssues || b.seen.Len() != maxIssues || b.issues["issue-0"] != nil || b.issues["issue-49999"] == nil {
		t.Errorf("%d issues remembered", len(b.issues))
	}

	// Only a message's start names its issue.
	long := strings.Repeat("order 1 failed; ", 1<<18)
	start = time.Now()
	if issueOf(&Event{Message: long + "a"}) != issueOf(&Event{Message: long + "b"}) {
		t.Error("the end of a long message named its issue")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("4 MB messages took %s", took)
	}
}

func TestAggregatesBounded(t *testing.T) {
	a := &aggregates{buckets: map[aggregateKey]*aggregateCounts{}}
	now := time.Now()
	for i := range maxAggregates + 1000 {
		a.record("exited", deviceID(User{ID: fmt.Sprint(i)}), now)
	}
	total := 0
	for _, b := range a.buckets {
		total += b.exited
	}
	if len(a.buckets) != maxAggregates+1 || total != maxAggregates+1000 || a.buckets[aggregateKey{now.UTC().Truncate(time.Minute), ""}].exited != 1000 {
		t.Errorf("%d buckets, %d sessions", len(a.buckets), total)
	}
}

// The source cache holds at most 64 files and 32 MB of lines.
func TestSourceCacheBounded(t *testing.T) {
	if maxSourceFiles != 64 || maxSourceCache != 32<<20 {
		t.Fatalf("bounds of %d files and %d bytes", maxSourceFiles, maxSourceCache)
	}
	reset := func() {
		source.Lock()
		source.files, source.bytes = map[string][]string{}, 0
		source.Unlock()
	}
	reset()
	t.Cleanup(reset)
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for i := range maxSourceFiles + 1 {
		sourceLines(write(fmt.Sprint(i, ".go"), "package x\n"))
	}
	source.Lock()
	if n := len(source.files); n != maxSourceFiles {
		t.Errorf("%d files cached", n)
	}
	source.Unlock()
	// Four files of 9 MB, 36 in all: the oldest make room.
	big, last := strings.Repeat(strings.Repeat("x", 1023)+"\n", 9<<10), ""
	for i := range 4 {
		last = write(fmt.Sprint("big", i, ".go"), big)
		if lines := sourceLines(last); len(lines) != 9<<10 {
			t.Fatalf("%d lines read", len(lines))
		}
	}
	source.Lock()
	defer source.Unlock()
	total := 0
	for _, lines := range source.files {
		total += linesSize(lines)
	}
	if total != source.bytes || total > maxSourceCache || len(source.files) > maxSourceFiles || source.files[last] == nil {
		t.Errorf("%d files of %d bytes cached (%d counted)", len(source.files), total, source.bytes)
	}
}

func TestDependencyFrames(t *testing.T) {
	saved := build
	t.Cleanup(func() { build = saved })
	build = func() buildInfo {
		return buildInfo{main: "github.com/acme/shop", deps: map[string]bool{"github.com/lib/pq": true}}
	}
	for module, want := range map[string]bool{
		"github.com/lib/pq": false, "github.com/lib/pq/oid": false, "github.com/lib/pqx": true, "github.com/acme/shop/cart": true,
	} {
		if got := inApp(module, "/src/x.go", Options{}); got != want {
			t.Errorf("inApp(%s) = %v", module, got)
		}
	}
}

func TestLongErrorStacks(t *testing.T) {
	real := make([]uintptr, 8)
	real = real[:runtime.Callers(0, real)]
	pcs := make([]uintptr, 10000)
	for i := range pcs {
		pcs[i] = real[i%len(real)]
	}
	if frames := errorStack(&stackError{msg: "deep", pcs: pcs}, Options{MaxStackFrames: 100}); len(frames) > 100 {
		t.Errorf("%d frames", len(frames))
	}
}

// pcOf is a function's address as a stack holds it (a return address).
func pcOf(f any) uintptr { return reflect.ValueOf(f).Pointer() + 1 }

// captureOnMarshal captures an error from inside encoding/json: the frames
// under it are the standard library's, which stacks keep (the SDK's own,
// tests included, are left out).
type captureOnMarshal struct{ h *Hub }

func (c captureOnMarshal) MarshalJSON() ([]byte, error) {
	c.h.CaptureException(errors.New("deep"))
	return []byte("1"), nil
}

// Of a deeper stack, the newest MaxStackFrames frames are kept (frames run
// from the oldest to the newest).
func TestStackFramesNewest(t *testing.T) {
	pcs := []uintptr{pcOf(strings.ToUpper)} // the newest
	for range 150 {
		pcs = append(pcs, pcOf(strings.ToLower))
	}
	pcs = append(pcs, pcOf(strings.TrimSpace)) // the oldest
	err := &stackError{msg: "deep", pcs: pcs}
	for _, limit := range []int{100, 5} {
		frames := errorStack(err, Options{MaxStackFrames: limit})
		if len(frames) != limit || frames[len(frames)-1].Function != "ToUpper" || frames[0].Function != "ToLower" {
			t.Errorf("MaxStackFrames %d: %d frames, from %+v", limit, len(frames), frames[0])
		}
	}

	// The same holds for the stack of where an error is captured: 101 of
	// encoding/json's, not the test runner's under them.
	h, f := testClient(t, Options{MaxStackFrames: 101})
	var v any = captureOnMarshal{h}
	for range 150 {
		v = []any{v}
	}
	if _, err := json.Marshal(v); err != nil {
		t.Fatal(err)
	}
	flush(t, h)
	recs, _ := logRecords(t, f.requests("/v1/logs"))
	frames := kv(recs[0]["attributes"])["fixwire.exceptions"].([]any)[0].(map[string]any)["frames"].([]any)
	if len(frames) != 101 {
		t.Fatalf("%d frames", len(frames))
	}
	for _, fr := range frames {
		if m := fr.(map[string]any)["module"]; m != "encoding/json" {
			t.Fatalf("a frame of %v kept: %v", m, fr)
		}
	}
}

type loopError struct{ next *loopError }

func (e *loopError) Error() string { return "loop" }
func (e *loopError) Unwrap() error { return e.next }

// A chain is at most 10 errors, and ends where it comes back to one.
func TestErrorChainBounds(t *testing.T) {
	err := errors.New("root")
	for i := range 10 {
		err = fmt.Errorf("wrap %d: %w", i, err)
	}
	if got := exceptionsOf(err, nil, Mechanism{}, Options{}); len(got) != maxChain || !strings.HasPrefix(got[0].Message, "wrap 9") {
		t.Errorf("an 11-deep chain gave %d", len(got))
	}
	a := &loopError{}
	a.next = &loopError{next: a}
	if got := exceptionsOf(a, nil, Mechanism{}, Options{}); len(got) != 2 {
		t.Errorf("a chain back to itself gave %d", len(got))
	}
}
