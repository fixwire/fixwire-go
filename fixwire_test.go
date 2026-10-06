package fixwire

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseDSN(t *testing.T) {
	for _, c := range []struct{ in, key, base string }{
		{"https://fw_pk_live_abc@ingest.fixwire.dev", "fw_pk_live_abc", "https://ingest.fixwire.dev"},
		{"http://publickey@127.0.0.1:9000/", "publickey", "http://127.0.0.1:9000"},
		{" https://k@self-hosted.example.com/fixwire ", "k", "https://self-hosted.example.com/fixwire"},
	} {
		d, err := ParseDSN(c.in)
		if err != nil || d.Key != c.key || d.BaseURL != c.base {
			t.Errorf("ParseDSN(%q) = %+v, %v", c.in, d, err)
		}
	}
	for _, bad := range []string{"", "ingest.fixwire.dev", "https://ingest.fixwire.dev", "ftp://k@host", "https://@host"} {
		if _, err := ParseDSN(bad); !errors.Is(err, ErrInvalidDSN) {
			t.Errorf("ParseDSN(%q) = %v, want ErrInvalidDSN", bad, err)
		}
	}
}

func TestDisabledWithoutDSN(t *testing.T) {
	t.Setenv("FIXWIRE_DSN", "")
	c, err := NewClient(Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHub(c, nil)
	if id := h.CaptureException(errors.New("x")); id != "" {
		t.Errorf("a client without a DSN sent %q", id)
	}
	if !h.Flush(time.Second) {
		t.Error("flush of a disabled client")
	}
}

// Init never panics: a broken DSN, given or from FIXWIRE_DSN, is returned
// and the SDK stays off.
func TestInitWithBrokenDSN(t *testing.T) {
	bound := CurrentHub().Client()
	t.Cleanup(func() { CurrentHub().BindClient(bound) })
	CurrentHub().BindClient(nil)
	for _, c := range []struct{ option, env string }{
		{"ingest.fixwire.dev", ""}, {"https://ingest.fixwire.dev", ""}, {"ftp://k@host", ""},
		{"https://k%0D%0A@host", ""}, {"https://k@[::1", ""}, {"%", ""}, {"", "https://@host"},
	} {
		t.Setenv("FIXWIRE_DSN", c.env)
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Init(%q, FIXWIRE_DSN=%q) panicked: %v", c.option, c.env, r)
				}
			}()
			err = Init(Options{DSN: c.option, Release: "api@1.0.0"})
		}()
		if !errors.Is(err, ErrInvalidDSN) {
			t.Errorf("Init(%q, FIXWIRE_DSN=%q) = %v, want ErrInvalidDSN", c.option, c.env, err)
		}
		if CurrentHub().Client() != nil || CaptureMessage("x") != "" || !Flush(time.Second) {
			t.Errorf("the SDK is on after Init(%q, FIXWIRE_DSN=%q)", c.option, c.env)
		}
		Close(time.Second)
	}
}

type cartError struct{ ID int }

func (e *cartError) Error() string { return fmt.Sprintf("cart %d is empty", e.ID) }

func TestCaptureException(t *testing.T) {
	h, f := testClient(t, Options{Release: "shop@1.2.0", Environment: "staging", ServerName: "web-1"})
	h.Scope().SetUser(User{ID: "user-1", Username: "ada"})
	h.Scope().SetTag("plan", "team")
	h.Scope().SetContext("order", map[string]any{"id": 42, "items": 3})
	h.AddBreadcrumb(Breadcrumb{Category: "cart", Message: "checkout started"})

	err := fmt.Errorf("checkout: %w", &cartError{ID: 7})
	id := h.CaptureException(err)
	if len(id) != 32 {
		t.Fatalf("event id %q", id)
	}
	flush(t, h)

	reqs := f.requests("/v1/logs")
	if len(reqs) != 1 || reqs[0].auth != "Bearer publickey" || reqs[0].encoding != "gzip" || reqs[0].agent != "fixwire.go/"+sdkVersion {
		t.Fatalf("requests %+v", reqs)
	}
	recs, res := logRecords(t, reqs)
	for k, want := range map[string]any{
		"service.name": "shop", "service.version": "shop@1.2.0", "deployment.environment.name": "staging",
		"host.name": "web-1", "telemetry.sdk.name": "fixwire.go", "telemetry.sdk.language": "go",
	} {
		if res[k] != want {
			t.Errorf("resource %s = %v, want %v", k, res[k], want)
		}
	}
	rec := recs[0]
	if rec["eventName"] != "exception" || rec["severityNumber"] != float64(17) {
		t.Errorf("record %v", rec)
	}
	a := kv(rec["attributes"])
	if a["fixwire.event_id"] != id || a["exception.type"] != "error" || a["exception.message"] != "checkout: cart 7 is empty" {
		t.Errorf("attributes %v", a)
	}
	if a["user.id"] != "user-1" || a["user.name"] != "ada" || a["fixwire.tags"].(map[string]any)["plan"] != "team" {
		t.Errorf("user and tags %v", a)
	}
	if order := a["fixwire.contexts"].(map[string]any)["order"].(map[string]any); order["id"] != int64(42) {
		t.Errorf("contexts %v", a["fixwire.contexts"])
	}
	if crumbs := a["fixwire.breadcrumbs"].([]any); len(crumbs) != 1 || crumbs[0].(map[string]any)["message"] != "checkout started" {
		t.Errorf("breadcrumbs %v", crumbs)
	}
	if _, ok := a["fixwire.handled"]; ok {
		t.Error("a handled error says fixwire.handled")
	}
	chain := a["fixwire.exceptions"].([]any)
	if len(chain) != 2 {
		t.Fatalf("chain %v", chain)
	}
	outer, cause := chain[0].(map[string]any), chain[1].(map[string]any)
	if outer["type"] != "error" || cause["type"] != "fixwire.cartError" || cause["module"] != sdkModule || cause["message"] != "cart 7 is empty" {
		t.Errorf("chain %v", chain)
	}
	if m := outer["mechanism"].(map[string]any); m["type"] != "generic" || m["handled"] != true {
		t.Errorf("mechanism %v", m)
	}
	frames, _ := outer["frames"].([]any)
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	// The newest frame is the caller of CaptureException: the SDK's frames
	// (this test is one) are left out, so testing's runner is.
	newest := frames[len(frames)-1].(map[string]any)
	if newest["module"] != "testing" || newest["in_app"] != false {
		t.Errorf("newest frame %v", newest)
	}
}

func TestCaptureMessageAndLevels(t *testing.T) {
	h, f := testClient(t, Options{})
	h.CaptureMessage("disk almost full")
	h.WithScope(func(s *Scope) {
		s.SetLevel(LevelWarning)
		h.CaptureMessage("slow query")
	})
	flush(t, h)
	recs, _ := logRecords(t, f.requests("/v1/logs"))
	if len(recs) != 2 {
		t.Fatalf("records %v", recs)
	}
	for i, want := range []struct {
		body string
		sev  float64
	}{{"disk almost full", 9}, {"slow query", 13}} {
		r := recs[i]
		if r["eventName"] != "fixwire.message" || plain(r["body"].(map[string]any)) != want.body || r["severityNumber"] != want.sev {
			t.Errorf("record %d: %v", i, r)
		}
	}
}

func TestBeforeSendAndSampling(t *testing.T) {
	h, f := testClient(t, Options{BeforeSend: func(e *Event) *Event {
		if strings.Contains(e.Message, "noise") {
			return nil
		}
		e.Tags = map[string]string{"seen": "yes"}
		return e
	}})
	if id := h.CaptureMessage("noise"); id != "" {
		t.Errorf("dropped event got id %q", id)
	}
	h.CaptureMessage("signal")
	flush(t, h)
	recs, _ := logRecords(t, f.requests("/v1/logs"))
	if len(recs) != 1 || kv(recs[0]["attributes"])["fixwire.tags"].(map[string]any)["seen"] != "yes" {
		t.Errorf("records %v", recs)
	}
}

func TestRecoverPanic(t *testing.T) {
	h, f := testClient(t, Options{})
	func() {
		defer func() {
			if r := recover(); r != nil {
				h.RecoverPanic(r)
			}
		}()
		var items []int
		_ = items[len(items)] // panics: index out of range
	}()
	func() {
		defer func() { h.RecoverPanic(recover()) }()
		panic("out of stock")
	}()
	flush(t, h)
	recs, _ := logRecords(t, f.requests("/v1/logs"))
	if len(recs) != 2 {
		t.Fatalf("records %v", recs)
	}
	first, second := kv(recs[0]["attributes"]), kv(recs[1]["attributes"])
	if first["fixwire.handled"] != false || recs[0]["severityNumber"] != float64(21) || !strings.Contains(first["exception.message"].(string), "index out of range") {
		t.Errorf("runtime panic %v", first)
	}
	if second["exception.type"] != "panic" || second["exception.message"] != "out of stock" {
		t.Errorf("panic value %v", second)
	}
	m := second["fixwire.exceptions"].([]any)[0].(map[string]any)["mechanism"].(map[string]any)
	if m["type"] != "panic" || m["handled"] != false {
		t.Errorf("mechanism %v", m)
	}
}

func TestRedaction(t *testing.T) {
	h, f := testClient(t, Options{})
	h.Scope().SetExtra("password", "hunter2hunter2")
	h.Scope().SetExtra("note", "card 4111 1111 1111 1111 declined")
	h.CaptureException(errors.New("mail to ada@example.com bounced"))
	flush(t, h)
	recs, _ := logRecords(t, f.requests("/v1/logs"))
	a := kv(recs[0]["attributes"])
	if a["password"] != "[Filtered]" || a["note"] != "card [REDACTED:credit_card] declined" ||
		a["exception.message"] != "mail to [REDACTED:email] bounced" {
		t.Errorf("redacted %v", a)
	}

	h2, f2 := testClient(t, Options{DisableRedaction: true})
	h2.CaptureException(errors.New("mail to ada@example.com bounced"))
	flush(t, h2)
	recs, _ = logRecords(t, f2.requests("/v1/logs"))
	if a := kv(recs[0]["attributes"]); a["exception.message"] != "mail to ada@example.com bounced" {
		t.Errorf("redaction off %v", a)
	}
}

func TestTracing(t *testing.T) {
	h, f := testClient(t, Options{TracesSampleRate: 1})
	ctx := NewContext(context.Background(), h)
	root, ctx := StartSpan(ctx, "POST /checkout", WithOp("http.server"), WithAttributes(map[string]any{"http.request.method": "POST"}))
	child, cctx := StartSpan(ctx, "SELECT carts", WithOp("db.query"))
	if SpanFromContext(cctx) != child || child.TraceID != root.TraceID || child.ParentSpanID != root.SpanID {
		t.Fatalf("child %+v of %+v", child, root)
	}
	child.SetError(errors.New("deadlock"))
	child.Finish()
	if len(f.requests("/v1/traces")) != 0 {
		t.Fatal("a child was sent before its segment")
	}
	root.Finish()
	late, _ := StartSpan(ctx, "after", WithOp("task"))
	late.Finish()
	flush(t, h)

	reqs := f.requests("/v1/traces")
	if len(reqs) != 2 {
		t.Fatalf("%d traces requests, want the segment's and the late span's", len(reqs))
	}
	got := spans(t, reqs)
	byName := map[string]map[string]any{}
	for _, s := range got {
		byName[s["name"].(string)] = s
	}
	r, c := byName["POST /checkout"], byName["SELECT carts"]
	if r["kind"] != float64(KindServer) || r["flags"] != float64(0x101) || r["parentSpanId"] != nil {
		t.Errorf("root %v", r)
	}
	if kv(r["attributes"])["fixwire.op"] != "http.server" || kv(r["attributes"])["http.request.method"] != "POST" {
		t.Errorf("root attributes %v", r["attributes"])
	}
	if c["kind"] != float64(KindClient) || c["parentSpanId"] != root.SpanID ||
		c["status"].(map[string]any)["code"] != float64(2) || c["status"].(map[string]any)["message"] != "deadlock" {
		t.Errorf("child %v", c)
	}
	if byName["after"]["parentSpanId"] != root.SpanID {
		t.Errorf("late span %v", byName["after"])
	}

	// An error captured under a span is linked to it.
	h.Scope().SetSpan(root)
	h.CaptureMessage("linked")
	flush(t, h)
	recs, _ := logRecords(t, f.requests("/v1/logs"))
	if recs[0]["traceId"] != root.TraceID || recs[0]["spanId"] != root.SpanID {
		t.Errorf("record %v", recs[0])
	}
}

func TestContinueTrace(t *testing.T) {
	h, f := testClient(t, Options{TracesSampleRate: 0})
	ctx := NewContext(context.Background(), h)
	s, _ := StartSpan(ctx, "GET /", ContinueTrace("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "fw=1", "user=1"))
	if s.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || s.ParentSpanID != "00f067aa0ba902b7" || !s.Sampled {
		t.Fatalf("span %+v", s)
	}
	if tp := s.Traceparent(); tp != "00-4bf92f3577b34da6a3ce929d0e0e4736-"+s.SpanID+"-01" || s.Tracestate() != "fw=1" || s.Baggage() != "user=1" {
		t.Errorf("propagation %s %s %s", tp, s.Tracestate(), s.Baggage())
	}
	s.Finish()
	flush(t, h)
	if got := spans(t, f.requests("/v1/traces")); len(got) != 1 || got[0]["flags"] != float64(0x301) {
		t.Errorf("spans %v", got)
	}

	// The caller's decision holds: not sampled stays not sampled.
	s, _ = StartSpan(ctx, "GET /", ContinueTrace("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", "", ""))
	if s.Sampled {
		t.Error("an unsampled caller's trace was sampled")
	}
	for _, bad := range []string{"", "00-xyz-00f067aa0ba902b7-01", "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01"} {
		if _, _, _, ok := parseTraceparent(bad); ok {
			t.Errorf("parseTraceparent(%q) accepted", bad)
		}
	}
}

func TestSampleTrace(t *testing.T) {
	// The rule every Fixwire SDK shares: kept when the last 56 bits, as a
	// fraction of 2^56, are at least 1 - rate.
	for _, c := range []struct {
		id   string
		rate float64
		want bool
	}{
		{"4bf92f3577b34da6ffffffffffffffff", 0.01, true},
		{"4bf92f3577b34da6a000000000000000", 0.5, false},
		{"4bf92f3577b34da6a080000000000000", 0.5, true},
		{"4bf92f3577b34da6a07ffffffffff000", 0.5, false},
		{"4bf92f3577b34da6a000000000000000", 1, true},
		{"4bf92f3577b34da6ffffffffffffffff", 0, false},
	} {
		if got := sampleTrace(c.id, c.rate); got != c.want {
			t.Errorf("sampleTrace(%s, %v) = %v", c.id, c.rate, got)
		}
	}
}

func TestSessions(t *testing.T) {
	h, f := testClient(t, Options{Release: "shop@1.2.0"})
	for i, outcome := range []string{"ok", "ok", "handled", "panic"} {
		rh := h.Clone()
		rh.Scope().SetUser(User{ID: fmt.Sprint("user-", i%2)})
		end := rh.StartRequestSession()
		switch outcome {
		case "handled":
			rh.CaptureException(errors.New("x"))
		case "panic":
			func() {
				defer func() { rh.RecoverPanic(recover()) }()
				panic("boom")
			}()
		}
		end()
		end() // once
	}
	flush(t, h)
	reqs := f.requests("/v1/sessions")
	if len(reqs) != 1 {
		t.Fatalf("sessions requests %v", reqs)
	}
	body := reqs[0].body
	if body["release"] != "shop@1.2.0" || body["environment"] != "production" || body["sdk"].(map[string]any)["name"] != "fixwire.go" {
		t.Errorf("body %v", body)
	}
	totals := map[string]float64{}
	dids := map[string]bool{}
	for _, a := range body["aggregates"].([]any) {
		a := a.(map[string]any)
		dids[a["did"].(string)] = true
		for _, k := range []string{"exited", "errored", "crashed"} {
			totals[k] += a[k].(float64)
		}
	}
	if totals["exited"] != 2 || totals["errored"] != 1 || totals["crashed"] != 1 || len(dids) != 2 {
		t.Errorf("totals %v, users %v", totals, dids)
	}
	if !dids[deviceID(User{ID: "user-0"})] || len(deviceID(User{ID: "user-0"})) != 32 {
		t.Errorf("dids %v", dids)
	}

	// No release, no sessions.
	h2, _ := testClient(t, Options{})
	if h2.Client().sessions != nil {
		t.Error("sessions without a release")
	}
}

func TestCheckInsAndFeedback(t *testing.T) {
	h, f := testClient(t, Options{Release: "shop@1.2.0"})
	ctx := NewContext(context.Background(), h)
	config := &MonitorConfig{Schedule: CrontabSchedule("0 3 * * *"), CheckInMargin: 5, Timezone: "Europe/Berlin"}
	err := WithMonitor(ctx, "nightly report", config, func(context.Context) error { return errors.New("no data") })
	if err == nil || err.Error() != "no data" {
		t.Fatalf("err %v", err)
	}
	h.CaptureFeedback(Feedback{Message: "The refund was wrong, mail ada@example.com", Score: -3, TraceID: "4bf92f3577b34da6a3ce929d0e0e4736"})
	if id := h.CaptureFeedback(Feedback{Message: "  "}); id != "" {
		t.Errorf("empty feedback sent: %s", id)
	}
	flush(t, h)

	checkIns := f.requests("/v1/check-ins/nightly%20report")
	if len(checkIns) == 0 {
		checkIns = f.requests("/v1/check-ins/nightly report")
	}
	if len(checkIns) != 2 {
		t.Fatalf("check-ins %v", f.requests(""))
	}
	start, end := checkIns[0].body, checkIns[1].body
	if start["status"] != "in_progress" || start["monitor_config"].(map[string]any)["schedule"].(map[string]any)["value"] != "0 3 * * *" {
		t.Errorf("start %v", start)
	}
	if end["status"] != "error" || end["check_in_id"] != start["check_in_id"] || end["duration"] == nil || end["monitor_config"] != nil {
		t.Errorf("end %v", end)
	}

	fb := f.requests("/v1/feedback")
	if len(fb) != 1 {
		t.Fatalf("feedback %v", fb)
	}
	b := fb[0].body
	if b["score"] != float64(-1) || b["message"] != "The refund was wrong, mail [REDACTED:email]" ||
		b["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || b["source"] != "api" || b["release"] != "shop@1.2.0" {
		t.Errorf("feedback %v", b)
	}
}

func TestRateLimitsAndRetries(t *testing.T) {
	backoffUnit = 10 * time.Millisecond
	t.Cleanup(func() { backoffUnit = time.Second })
	var limited atomic.Bool
	h, f := testClient(t, Options{})
	f.answer = func(n int, r *http.Request) (int, http.Header) {
		switch n {
		case 0:
			return http.StatusServiceUnavailable, nil // retried
		case 1:
			limited.Store(true)
			return http.StatusOK, http.Header{"Fixwire-Rate-Limits": {"3600:error"}}
		}
		return http.StatusOK, nil
	}
	h.CaptureMessage("first")
	flush(t, h)
	if got := len(f.requests("/v1/logs")); got != 2 || !limited.Load() {
		t.Fatalf("%d sends, want a retry", got)
	}
	// Errors are paused for an hour (past maxWait: dropped); feedback isn't.
	h.CaptureMessage("dropped")
	h.CaptureFeedback(Feedback{Score: 1})
	flush(t, h)
	if got := len(f.requests("/v1/logs")); got != 2 {
		t.Errorf("%d logs sends while paused", got)
	}
	if got := len(f.requests("/v1/feedback")); got != 1 {
		t.Errorf("%d feedback sends", got)
	}

	// A refused request is dropped, not retried.
	h2, f2 := testClient(t, Options{})
	f2.answer = func(int, *http.Request) (int, http.Header) { return http.StatusBadRequest, nil }
	h2.CaptureMessage("bad")
	flush(t, h2)
	if got := len(f2.requests("")); got != 1 {
		t.Errorf("%d sends of a refused request", got)
	}
}

func TestSlogHandler(t *testing.T) {
	h, f := testClient(t, Options{})
	ctx := NewContext(context.Background(), h)
	var out strings.Builder
	logger := slog.New(NewSlogHandler(slog.NewTextHandler(&out, nil), nil)).With("service", "billing").WithGroup("req")
	logger.DebugContext(ctx, "ignored")
	logger.InfoContext(ctx, "charging", "amount", 500)
	logger.ErrorContext(ctx, "charge failed", "err", fs.ErrPermission, "attempt", 2)
	flush(t, h)
	if !strings.Contains(out.String(), "charge failed") {
		t.Errorf("the next handler got %q", out.String())
	}
	recs, _ := logRecords(t, f.requests("/v1/logs"))
	if len(recs) != 1 {
		t.Fatalf("records %v", recs)
	}
	a := kv(recs[0]["attributes"])
	if a["exception.type"] != "error" || a["exception.message"] != "permission denied" || plain(recs[0]["body"].(map[string]any)) != "charge failed" {
		t.Errorf("event %v", a)
	}
	if a["service"] != "billing" || a["req.attempt"] != int64(2) || a["req.err"] != "permission denied" {
		t.Errorf("attributes %v", a)
	}
	crumbs := a["fixwire.breadcrumbs"].([]any)
	if len(crumbs) != 1 || crumbs[0].(map[string]any)["message"] != "charging" || crumbs[0].(map[string]any)["data"].(map[string]any)["req.amount"] != int64(500) {
		t.Errorf("breadcrumbs %v", crumbs)
	}
	m := a["fixwire.exceptions"].([]any)[0].(map[string]any)["mechanism"].(map[string]any)
	if m["type"] != "slog" {
		t.Errorf("mechanism %v", m)
	}
}

func TestInApp(t *testing.T) {
	opts := Options{InAppInclude: []string{"github.com/acme/vendored"}, InAppExclude: []string{"github.com/acme/shop/gen"}}
	gomodcache := os.Getenv("GOMODCACHE")
	for _, c := range []struct {
		module, file string
		want         bool
	}{
		{"net/http", "/usr/local/go/src/net/http/server.go", false},
		{"github.com/acme/vendored/x", gomodcache + "/github.com/acme/vendored@v1.0.0/x.go", true},
		{"github.com/acme/shop/gen", "/app/gen/x.go", false},
	} {
		if got := inApp(c.module, c.file, opts); got != c.want {
			t.Errorf("inApp(%s) = %v", c.module, got)
		}
	}
}

// stackError records its stack where it is made, as pkg/errors does.
type stackError struct {
	msg string
	pcs []uintptr
}

func (e *stackError) Error() string         { return e.msg }
func (e *stackError) StackTrace() []uintptr { return e.pcs }

func newStackError(msg string) error {
	pcs := make([]uintptr, 32)
	return &stackError{msg: msg, pcs: pcs[:runtime.Callers(1, pcs)]}
}

func TestErrorsWithTheirOwnStack(t *testing.T) {
	err := fmt.Errorf("load: %w", newStackError("disk gone"))
	captured := []Frame{{Function: "capturedHere"}}
	chain := exceptionsOf(err, captured, Mechanism{Type: "generic", Handled: true}, Options{MaxStackFrames: 100})
	if len(chain) != 2 || chain[0].Frames[0].Function != "capturedHere" {
		t.Fatalf("chain %+v", chain)
	}
	// The SDK's frames are left out of stacks, so newStackError's caller (a
	// test function, in the SDK's module) is too; the runner's stays.
	frames := chain[1].Frames
	if len(frames) == 0 || frames[len(frames)-1].Module != "testing" {
		t.Errorf("the cause's own stack %+v", frames)
	}
}

func TestErrorBudget(t *testing.T) {
	h, f := testClient(t, Options{ErrorBudget: ErrorBudget{PerIssueBurst: 3, PerIssuePerMinute: 1}})
	sent := 0
	for i := range 20 {
		// The same issue: only the order number changes.
		if h.CaptureMessage(fmt.Sprintf("order %d failed", 1000+i)) != "" {
			sent++
		}
	}
	if sent != 3 {
		t.Fatalf("%d of a crash loop's events sent, want the burst of 3", sent)
	}
	if h.CaptureMessage("another issue") == "" {
		t.Error("another issue was held back")
	}
	// A minute later the issue has a token again, and its event carries
	// what was held back.
	h.Client().budget.issues[issueOf(&Event{Message: "order 1 failed"})].updated = time.Now().Add(-time.Minute)
	if h.CaptureMessage("order 2000 failed") == "" {
		t.Fatal("no token after a minute")
	}
	flush(t, h)
	recs, _ := logRecords(t, f.requests("/v1/logs"))
	last := kv(recs[len(recs)-1]["attributes"])
	if last["fixwire.suppressed"] != int64(17) {
		t.Errorf("suppressed %v, want 17", last["fixwire.suppressed"])
	}

	// Different stacks are different issues; the variable parts of a
	// message are not.
	a := &Event{Exceptions: []Exception{{Type: "x", Frames: []Frame{{Module: "main", Function: "a", InApp: true}}}}}
	b := &Event{Exceptions: []Exception{{Type: "x", Frames: []Frame{{Module: "main", Function: "b", InApp: true}}}}}
	if issueOf(a) == issueOf(b) {
		t.Error("two call paths are one issue")
	}
	if issueOf(&Event{Message: "user ada@example.com: 3 retries"}) != issueOf(&Event{Message: "user bob@example.org: 12 retries"}) {
		t.Error("message templates differ")
	}
}
