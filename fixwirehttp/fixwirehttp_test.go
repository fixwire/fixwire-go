package fixwirehttp_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	fixwire "github.com/fixwire/fixwire-go"
	"github.com/fixwire/fixwire-go/fixwirehttp"
)

// ingest records the bodies the SDK sends, by path.
type ingest struct {
	mu     sync.Mutex
	bodies map[string][]map[string]any
}

func setup(t *testing.T, opts fixwire.Options) (*fixwire.Hub, *ingest) {
	t.Helper()
	in := &ingest{bodies: map[string][]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var m map[string]any
		if err := json.NewDecoder(zr).Decode(&m); err != nil {
			t.Error(err)
		}
		in.mu.Lock()
		in.bodies[r.URL.Path] = append(in.bodies[r.URL.Path], m)
		in.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	opts.DSN = strings.Replace(srv.URL, "http://", "http://publickey@", 1)
	c, err := fixwire.NewClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(time.Second) })
	return fixwire.NewHub(c, nil), in
}

func (in *ingest) get(path string) []map[string]any {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.bodies[path]
}

// attrs reads OTLP key-values (strings, ints and bools are enough here).
func attrs(list any) map[string]any {
	out := map[string]any{}
	items, _ := list.([]any)
	for _, it := range items {
		m := it.(map[string]any)
		v := m["value"].(map[string]any)
		for _, k := range []string{"stringValue", "intValue", "boolValue"} {
			if x, ok := v[k]; ok {
				out[m["key"].(string)] = x
			}
		}
		if _, ok := v["kvlistValue"]; ok {
			out[m["key"].(string)] = "kvlist"
		}
	}
	return out
}

func spans(bodies []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, b := range bodies {
		for _, rs := range b["resourceSpans"].([]any) {
			for _, ss := range rs.(map[string]any)["scopeSpans"].([]any) {
				for _, s := range ss.(map[string]any)["spans"].([]any) {
					s := s.(map[string]any)
					out[s["name"].(string)] = s
				}
			}
		}
	}
	return out
}

func records(bodies []map[string]any) []map[string]any {
	var out []map[string]any
	for _, b := range bodies {
		for _, rl := range b["resourceLogs"].([]any) {
			for _, sl := range rl.(map[string]any)["scopeLogs"].([]any) {
				for _, r := range sl.(map[string]any)["logRecords"].([]any) {
					out = append(out, r.(map[string]any))
				}
			}
		}
	}
	return out
}

func serve(t *testing.T, hub *fixwire.Hub, handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req.WithContext(fixwire.NewContext(req.Context(), hub)))
	return rec
}

func TestMiddleware(t *testing.T) {
	hub, in := setup(t, fixwire.Options{Release: "shop@1.2.0", TracesSampleRate: 1})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		h := fixwire.HubFromContext(r.Context())
		h.Scope().SetTag("item", r.PathValue("id"))
		h.CaptureException(errors.New("price missing"))
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /checkout", func(http.ResponseWriter, *http.Request) { panic("out of stock") })
	handler := fixwirehttp.New(fixwirehttp.Options{}).Handle(mux)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items/42?ref=mail", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.Header.Set("Authorization", "Bearer secret")
	if rec := serve(t, hub, handler, req); rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
	rec := serve(t, hub, handler, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/checkout", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a panic answered %d", rec.Code)
	}
	hub.Flush(5 * time.Second)

	recs := records(in.get("/v1/logs"))
	if len(recs) != 2 {
		t.Fatalf("records %v", recs)
	}
	first, second := attrs(recs[0]["attributes"]), attrs(recs[1]["attributes"])
	if first["fixwire.transaction"] != "GET /items/{id}" || first["url.full"] != "http://example.com/items/42" ||
		first["url.query"] != "ref=mail" || first["http.request.method"] != "GET" || first["fixwire.tags"] != "kvlist" {
		t.Errorf("handled error %v", first)
	}
	if _, ok := first["http.request.header.authorization"]; ok {
		t.Error("the Authorization header was sent")
	}
	if recs[0]["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace %v", recs[0]["traceId"])
	}
	if second["fixwire.handled"] != false || second["exception.message"] != "out of stock" || second["fixwire.transaction"] != "POST /checkout" {
		t.Errorf("panic %v", second)
	}

	s := spans(in.get("/v1/traces"))
	item, checkout := s["GET /items/{id}"], s["POST /checkout"]
	if item == nil || checkout == nil {
		t.Fatalf("spans %v", s)
	}
	if a := attrs(item["attributes"]); a["http.route"] != "/items/{id}" || a["http.response.status_code"] != "202" || a["fixwire.op"] != "http.server" {
		t.Errorf("item span %v", a)
	}
	if item["parentSpanId"] != "00f067aa0ba902b7" || item["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("item span %v", item)
	}
	if checkout["status"].(map[string]any)["code"] != float64(2) || attrs(checkout["attributes"])["http.response.status_code"] != "500" {
		t.Errorf("checkout span %v", checkout)
	}

	sessions := in.get("/v1/sessions")
	if len(sessions) != 1 {
		t.Fatalf("sessions %v", sessions)
	}
	var errored, crashed float64
	for _, a := range sessions[0]["aggregates"].([]any) {
		a := a.(map[string]any)
		errored += a["errored"].(float64)
		crashed += a["crashed"].(float64)
	}
	if errored != 1 || crashed != 1 {
		t.Errorf("aggregates %v", sessions[0]["aggregates"])
	}
}

func TestMiddlewareRepanic(t *testing.T) {
	hub, _ := setup(t, fixwire.Options{})
	handler := fixwirehttp.New(fixwirehttp.Options{Repanic: true}).HandleFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	defer func() {
		if r := recover(); r != "boom" {
			t.Errorf("recovered %v", r)
		}
	}()
	serve(t, hub, handler, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	t.Error("the panic did not go on")
}

func TestTransport(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("traceparent")
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	// The URLs under /internal/ (compared without their query) are a target.
	hub, in := setup(t, fixwire.Options{TracesSampleRate: 1, TracePropagationTargets: []string{upstream.URL + "/internal/"}})
	client := &http.Client{Transport: fixwirehttp.NewTransport(nil)}

	ctx := fixwire.NewContext(context.Background(), hub)
	root, ctx := fixwire.StartSpan(ctx, "job", fixwire.WithOp("task"))
	for _, path := range []string{"/internal/prices", "/partner/prices"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+path+"?sku=1", nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if req.Header.Get("traceparent") != "" {
			t.Error("the caller's request was changed")
		}
	}
	root.Finish()
	hub.Flush(5 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	if tp := seen["/internal/prices"]; !strings.HasPrefix(tp, "00-"+root.TraceID+"-") || strings.Contains(tp, root.SpanID) {
		t.Errorf("the target got traceparent %q (want its client span's)", tp)
	}
	if tp := seen["/partner/prices"]; tp != "" {
		t.Errorf("a service that is no target got traceparent %q", tp)
	}
	s := spans(in.get("/v1/traces"))
	span := s["GET "+upstream.URL+"/internal/prices"]
	if span == nil {
		t.Fatalf("spans %v", s)
	}
	if a := attrs(span["attributes"]); a["fixwire.op"] != "http.client" || a["http.response.status_code"] != "502" || a["url.full"] != upstream.URL+"/internal/prices" {
		t.Errorf("client span %v", a)
	}
	if span["status"].(map[string]any)["code"] != float64(2) || span["parentSpanId"] != root.SpanID {
		t.Errorf("client span %v", span)
	}
}
