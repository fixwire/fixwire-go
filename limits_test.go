package fixwire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

	tr := h.Client().transport
	now := time.Now()
	tr.limit("99999999999999999:error, 60:made-up;also-made-up, -5:log, x:span", now)
	tr.mu.Lock()
	until, paused := tr.until["error"], len(tr.until)
	tr.mu.Unlock()
	if until.Sub(now) != maxPause || paused != 1 {
		t.Errorf("paused %v for %s", tr.until, until.Sub(now))
	}
	for in, want := range map[string]time.Duration{"120": 2 * time.Minute, "0": 0, "-1": 0, "soon": 0,
		"Wed, 21 Oct 2026 07:28:00 GMT": 0, "99999999999999999999": 0} {
		if got, _ := seconds(in); got != want {
			t.Errorf("seconds(%q) = %s", in, got)
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
		if a["exception.message"] != "division" || a["ratio"] == nil || a["ceiling"] == nil {
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
	if a := kv(recs[0]["attributes"]); a["loop"].(map[string]any)["a"] != "[cut]" {
		t.Errorf("loop %v", a["loop"])
	}
}

func TestPropagatedHeadersBounded(t *testing.T) {
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	for _, c := range []struct{ tracestate, baggage string }{
		{strings.Repeat("a", maxTracestate+1), strings.Repeat("b", maxBaggage+1)},
		{"fw=1\r\nX-Injected: 1", "user=1\nX-Injected: 1"},
	} {
		s, _ := StartSpan(context.Background(), "GET /", ContinueTrace(parent, c.tracestate, c.baggage))
		if s.TraceID == "" || s.Tracestate() != "" || s.Baggage() != "" {
			t.Errorf("passed on %q, %q", s.Tracestate(), s.Baggage())
		}
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
	if frames := errorStack(&stackError{msg: "deep", pcs: pcs}, Options{}); len(frames) > maxFrames {
		t.Errorf("%d frames", len(frames))
	}
}
