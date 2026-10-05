package fixwire

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIngest records what the SDK sends.
type fakeIngest struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []received
	// answer, when set, picks the status and headers of each answer.
	answer func(n int, r *http.Request) (int, http.Header)
}

type received struct {
	path, auth, encoding, agent string
	body                        map[string]any
}

func newIngest(t *testing.T) *fakeIngest {
	t.Helper()
	f := &fakeIngest{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("gzip: %v", err)
				return
			}
			body = zr
		}
		var m map[string]any
		if err := json.NewDecoder(body).Decode(&m); err != nil {
			t.Errorf("%s: %v", r.URL.Path, err)
		}
		f.mu.Lock()
		n := len(f.reqs)
		f.reqs = append(f.reqs, received{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Encoding"),
			r.Header.Get("User-Agent"), m})
		answer := f.answer
		f.mu.Unlock()
		status := http.StatusOK
		if answer != nil {
			var h http.Header
			status, h = answer(n, r)
			for k, v := range h {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(f.Close)
	return f
}

// dsn is the fake's DSN.
func (f *fakeIngest) dsn() string { return strings.Replace(f.URL, "http://", "http://publickey@", 1) }

// requests are what was received on path ("" for all).
func (f *fakeIngest) requests(path string) []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []received
	for _, r := range f.reqs {
		if path == "" || r.path == path {
			out = append(out, r)
		}
	}
	return out
}

// testClient is a client sending to a fake ingest, bound to a hub of its
// own.
func testClient(t *testing.T, opts Options) (*Hub, *fakeIngest) {
	t.Helper()
	f := newIngest(t)
	if opts.DSN == "" {
		opts.DSN = f.dsn()
	}
	if opts.ServiceName == "" {
		opts.ServiceName = "shop"
	}
	c, err := NewClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(time.Second) })
	return NewHub(c, nil), f
}

func flush(t *testing.T, h *Hub) {
	t.Helper()
	if !h.Flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
}

// logRecords are the log records of OTLP logs bodies, with their resource
// attributes.
func logRecords(t *testing.T, reqs []received) (records []map[string]any, resource map[string]any) {
	t.Helper()
	for _, r := range reqs {
		for _, rl := range r.body["resourceLogs"].([]any) {
			rl := rl.(map[string]any)
			resource = kv(rl["resource"].(map[string]any)["attributes"])
			for _, sl := range rl["scopeLogs"].([]any) {
				for _, rec := range sl.(map[string]any)["logRecords"].([]any) {
					records = append(records, rec.(map[string]any))
				}
			}
		}
	}
	return records, resource
}

// spans are the spans of OTLP traces bodies.
func spans(t *testing.T, reqs []received) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range reqs {
		for _, rs := range r.body["resourceSpans"].([]any) {
			for _, ss := range rs.(map[string]any)["scopeSpans"].([]any) {
				for _, s := range ss.(map[string]any)["spans"].([]any) {
					out = append(out, s.(map[string]any))
				}
			}
		}
	}
	return out
}

// kv reads OTLP key-values into plain values.
func kv(list any) map[string]any {
	out := map[string]any{}
	items, _ := list.([]any)
	for _, it := range items {
		m := it.(map[string]any)
		out[m["key"].(string)] = plain(m["value"].(map[string]any))
	}
	return out
}

// plain reads an OTLP AnyValue.
func plain(v map[string]any) any {
	switch {
	case v["stringValue"] != nil:
		return v["stringValue"]
	case v["boolValue"] != nil:
		return v["boolValue"]
	case v["intValue"] != nil:
		n, _ := strconv.ParseInt(v["intValue"].(string), 10, 64)
		return n
	case v["doubleValue"] != nil:
		return v["doubleValue"]
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
	return nil
}
