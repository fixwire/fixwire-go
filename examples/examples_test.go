// The examples are real programs: each is built, run against a fake ingest
// and checked for what Fixwire receives, so they keep working.
package examples_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ingest is a fake Fixwire: it keeps each request's decoded body by path.
type ingest struct {
	*httptest.Server
	mu     sync.Mutex
	bodies map[string][]map[string]any
}

func newIngest(t *testing.T) *ingest {
	t.Helper()
	in := &ingest{bodies: map[string][]map[string]any{}}
	in.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer publickey" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			body = zr
		}
		var m map[string]any
		if err := json.NewDecoder(body).Decode(&m); err != nil {
			t.Error(err)
		}
		in.mu.Lock()
		in.bodies[r.URL.Path] = append(in.bodies[r.URL.Path], m)
		in.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(in.Close)
	return in
}

func (in *ingest) dsn() string { return strings.Replace(in.URL, "http://", "http://publickey@", 1) }

func (in *ingest) get(path string) []map[string]any {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.bodies[path]
}

// event is an error or a message, its attributes read.
type event struct {
	record map[string]any
	attrs  map[string]any
}

func (in *ingest) events() []event {
	var out []event
	for _, b := range in.get("/v1/logs") {
		for _, rl := range b["resourceLogs"].([]any) {
			for _, sl := range rl.(map[string]any)["scopeLogs"].([]any) {
				for _, r := range sl.(map[string]any)["logRecords"].([]any) {
					rec := r.(map[string]any)
					out = append(out, event{rec, kv(rec["attributes"])})
				}
			}
		}
	}
	return out
}

func (in *ingest) spans() []map[string]any {
	var out []map[string]any
	for _, b := range in.get("/v1/traces") {
		for _, rs := range b["resourceSpans"].([]any) {
			for _, ss := range rs.(map[string]any)["scopeSpans"].([]any) {
				for _, s := range ss.(map[string]any)["spans"].([]any) {
					sp := s.(map[string]any)
					sp["attributes"] = kv(sp["attributes"])
					out = append(out, sp)
				}
			}
		}
	}
	return out
}

func kv(list any) map[string]any {
	out := map[string]any{}
	items, _ := list.([]any)
	for _, it := range items {
		m := it.(map[string]any)
		out[m["key"].(string)] = plain(m["value"].(map[string]any))
	}
	return out
}

func plain(v map[string]any) any {
	switch {
	case v["stringValue"] != nil:
		return v["stringValue"]
	case v["boolValue"] != nil:
		return v["boolValue"]
	case v["intValue"] != nil:
		n, _ := strconv.ParseInt(v["intValue"].(string), 10, 64)
		return n
	case v["arrayValue"] != nil:
		var out []any
		vals, _ := v["arrayValue"].(map[string]any)["values"].([]any)
		for _, x := range vals {
			out = append(out, plain(x.(map[string]any)))
		}
		return out
	case v["kvlistValue"] != nil:
		return kv(v["kvlistValue"].(map[string]any)["values"])
	}
	return v["doubleValue"]
}

// build compiles an example into the test's temporary directory.
func build(t *testing.T, pkg string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), filepath.Base(pkg))
	if out, err := exec.CommandContext(context.Background(), "go", "build", "-o", bin, pkg).CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, out)
	}
	return bin
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func TestShopAPI(t *testing.T) {
	in := newIngest(t)
	// The inventory service: sku_2 is sold out. It notes the trace each call carries.
	var mu sync.Mutex
	var traceparents []string
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		traceparents = append(traceparents, r.Header.Get("traceparent"))
		mu.Unlock()
		if r.URL.Query().Get("sku") == "sku_2" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer inventory.Close()

	port := freePort(t)
	cmd := exec.CommandContext(context.Background(), build(t, "./shop-api"))
	var logs bytes.Buffer
	cmd.Stderr = &logs
	cmd.Env = append(os.Environ(), "FIXWIRE_DSN="+in.dsn(), "PORT="+port, "INVENTORY_URL="+inventory.URL, "RELEASE=shop-api@1.0.0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	base := "http://127.0.0.1:" + port
	for i := 0; ; i++ {
		if c, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", "127.0.0.1:"+port); err == nil {
			_ = c.Close()
			break
		}
		if i > 100 {
			t.Fatalf("the API did not start:\n%s", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	call := func(method, path, user, body string) int {
		req, _ := http.NewRequestWithContext(context.Background(), method, base+path, strings.NewReader(body))
		if user != "" {
			req.Header.Set("X-User-Id", user)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	for _, c := range []struct {
		method, path, user, body string
		want                     int
	}{
		{"GET", "/products/sku_1", "", "", 200},
		{"GET", "/products/nope", "", "", 404},
		{"POST", "/orders", "user-1", `{"sku":"sku_1","card":"4242424242424242"}`, 201},
		{"POST", "/orders", "user-2", `{"sku":"sku_1","card":"4000000000000002"}`, 402},
		{"POST", "/orders", "user-3", `{"sku":"sku_2","card":"4242424242424242"}`, 409},
		{"GET", "/admin/report", "", "", 500},
	} {
		if got := call(c.method, c.path, c.user, c.body); got != c.want {
			t.Fatalf("%s %s = %d, want %d\n%s", c.method, c.path, got, c.want, logs.String())
		}
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the API did not stop cleanly: %v\n%s", err, logs.String())
	}

	// Three errors: the declined payment and the sold-out item (handled), the panic (a crash). No 404.
	byTx := map[string][]event{}
	for _, e := range in.events() {
		byTx[e.attrs["fixwire.transaction"].(string)] = append(byTx[e.attrs["fixwire.transaction"].(string)], e)
	}
	if len(in.events()) != 3 || len(byTx["POST /orders"]) != 2 || len(byTx["GET /admin/report"]) != 1 {
		t.Fatalf("events by transaction: %v", byTx)
	}
	var payment, soldOut event
	for _, e := range byTx["POST /orders"] {
		if e.attrs["user.id"] == "user-2" {
			payment = e
		} else {
			soldOut = e
		}
	}
	chain := payment.attrs["fixwire.exceptions"].([]any)
	if len(chain) != 2 || chain[1].(map[string]any)["type"] != "main.PaymentError" ||
		payment.attrs["fixwire.tags"].(map[string]any)["sku"] != "sku_1" ||
		payment.attrs["fixwire.contexts"].(map[string]any)["order"].(map[string]any)["sku"] != "sku_1" {
		t.Errorf("payment error: %v", payment.attrs)
	}
	// What led to it: the log line, then the call to the inventory service.
	crumbs, _ := payment.attrs["fixwire.breadcrumbs"].([]any)
	if len(crumbs) < 2 || crumbs[len(crumbs)-2].(map[string]any)["message"] != "order received" ||
		crumbs[len(crumbs)-1].(map[string]any)["category"] != "http" {
		t.Errorf("payment breadcrumbs: %v", crumbs)
	}
	if !strings.Contains(soldOut.attrs["exception.message"].(string), "inventory answered 409") || soldOut.attrs["user.id"] != "user-3" {
		t.Errorf("sold out: %v", soldOut.attrs)
	}
	crash := byTx["GET /admin/report"][0]
	if crash.attrs["fixwire.handled"] != false || !strings.Contains(crash.attrs["exception.message"].(string), "divide by zero") {
		t.Errorf("crash: %v", crash.attrs)
	}

	// A server span per request, named after its route; the lookup and the inventory calls under them.
	names := map[string]int{}
	var traceIDs []string
	for _, s := range in.spans() {
		names[s["name"].(string)]++
		if s["name"] == "POST /orders" {
			traceIDs = append(traceIDs, s["traceId"].(string))
		}
	}
	for name, want := range map[string]int{"GET /products/{id}": 2, "POST /orders": 3, "GET /admin/report": 1, "SELECT products": 2} {
		if names[name] != want {
			t.Errorf("%d %q spans, want %d (all: %v)", names[name], name, want, names)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(traceparents) != 3 {
		t.Fatalf("inventory calls: %v", traceparents)
	}
	for _, tp := range traceparents {
		if !strings.HasPrefix(tp, "00-") || !containsTrace(traceIDs, tp) {
			t.Errorf("the inventory call carried %q, not an order's trace %v", tp, traceIDs)
		}
	}

	// Release health: 3 requests ended well, 2 with an error, 1 crashed.
	var exited, errored, crashed float64
	for _, b := range in.get("/v1/sessions") {
		for _, a := range b["aggregates"].([]any) {
			a := a.(map[string]any)
			exited += a["exited"].(float64)
			errored += a["errored"].(float64)
			crashed += a["crashed"].(float64)
		}
	}
	if exited != 3 || errored != 2 || crashed != 1 {
		t.Errorf("sessions: %v exited, %v errored, %v crashed", exited, errored, crashed)
	}
}

func containsTrace(ids []string, traceparent string) bool {
	for _, id := range ids {
		if strings.Contains(traceparent, id) {
			return true
		}
	}
	return false
}

func TestNightlyReport(t *testing.T) {
	in := newIngest(t)
	cmd := exec.CommandContext(context.Background(), build(t, "./nightly-report"))
	cmd.Env = append(os.Environ(), "FIXWIRE_DSN="+in.dsn())
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("the job should fail with 1 of 3 accounts: %v\n%s", err, out)
	}

	checkIns := in.get("/v1/check-ins/nightly-report")
	if len(checkIns) != 2 || checkIns[0]["status"] != "in_progress" || checkIns[1]["status"] != "error" ||
		checkIns[0]["check_in_id"] != checkIns[1]["check_in_id"] {
		t.Fatalf("check-ins: %v", checkIns)
	}
	schedule := checkIns[0]["monitor_config"].(map[string]any)["schedule"].(map[string]any)
	if schedule["value"] != "0 3 * * *" {
		t.Errorf("monitor: %v", checkIns[0]["monitor_config"])
	}

	events := in.events()
	if len(events) != 2 {
		t.Fatalf("events: %v", events)
	}
	failure, summary := events[0], events[1]
	if failure.attrs["fixwire.tags"].(map[string]any)["account"] != "globex" ||
		!strings.Contains(failure.attrs["exception.message"].(string), "no invoices") {
		t.Errorf("failure: %v", failure.attrs)
	}
	if summary.record["severityNumber"] != float64(13) || summary.attrs["fixwire.tags"] != nil {
		t.Errorf("summary (a warning, without the account's tag): %v", summary)
	}

	spans := in.spans()
	if len(spans) != 4 {
		t.Fatalf("spans: %v", spans)
	}
	for _, s := range spans {
		if s["traceId"] != failure.record["traceId"] {
			t.Errorf("span %v is not in the job's trace %v", s["name"], failure.record["traceId"])
		}
	}
}
