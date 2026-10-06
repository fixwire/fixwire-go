package fixwire

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The SDK's name and version, as OpenTelemetry's telemetry.sdk.* say it.
const (
	sdkName    = "fixwire.go"
	sdkVersion = "0.1.0"
)

// Severity numbers of the levels (OpenTelemetry's).
var severity = map[Level]int{LevelDebug: 5, LevelInfo: 9, LevelWarning: 13, LevelError: 17, LevelFatal: 21}

// maxDepth bounds the lists and maps a value nests: deeper ones are cut, as
// is one that holds itself.
const maxDepth = 64

// value is v as an OTLP JSON AnyValue.
func value(v any) map[string]any { return valueIn(v, nil) }

// enter adds a list or map to the path of those a value is in; false when
// it is too deep or on the path already.
func enter(path []uintptr, c any) ([]uintptr, bool) {
	p := reflect.ValueOf(c).Pointer()
	if len(path) >= maxDepth || slices.Contains(path, p) {
		return path, false
	}
	return append(path, p), true
}

// valueIn is value for v in the lists and maps of path.
func valueIn(v any, path []uintptr) map[string]any {
	switch x := v.(type) {
	case nil:
		return map[string]any{"stringValue": ""}
	case string:
		return map[string]any{"stringValue": x}
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return value(n)
		}
		f, _ := x.Float64()
		return value(f)
	case float64:
		// OTLP's JSON spells the floats JSON has no numbers for.
		switch {
		case math.IsNaN(x):
			return map[string]any{"doubleValue": "NaN"}
		case math.IsInf(x, 1):
			return map[string]any{"doubleValue": "Infinity"}
		case math.IsInf(x, -1):
			return map[string]any{"doubleValue": "-Infinity"}
		}
		return map[string]any{"doubleValue": x}
	case float32:
		return value(float64(x))
	case bool:
		return map[string]any{"boolValue": x}
	case int:
		return map[string]any{"intValue": strconv.Itoa(x)}
	case int64:
		return map[string]any{"intValue": strconv.FormatInt(x, 10)}
	case int32:
		return map[string]any{"intValue": strconv.FormatInt(int64(x), 10)}
	case uint64:
		return map[string]any{"intValue": strconv.FormatUint(x, 10)}
	case []any:
		path, ok := enter(path, x)
		if !ok {
			return map[string]any{"stringValue": "[cut]"}
		}
		vals := make([]any, len(x))
		for i, e := range x {
			vals[i] = valueIn(e, path)
		}
		return map[string]any{"arrayValue": map[string]any{"values": vals}}
	case []string:
		vals := make([]any, len(x))
		for i, e := range x {
			vals[i] = value(e)
		}
		return map[string]any{"arrayValue": map[string]any{"values": vals}}
	case map[string]any:
		path, ok := enter(path, x)
		if !ok {
			return map[string]any{"stringValue": "[cut]"}
		}
		return map[string]any{"kvlistValue": map[string]any{"values": attributesIn(x, path)}}
	case map[string]string:
		m := make(map[string]any, len(x))
		for k, s := range x {
			m[k] = s
		}
		return valueIn(m, path)
	case time.Time:
		return map[string]any{"stringValue": x.UTC().Format(time.RFC3339Nano)}
	case error:
		return map[string]any{"stringValue": x.Error()}
	case fmt.Stringer:
		return map[string]any{"stringValue": x.String()}
	}
	// Other values: numbers of other kinds, slices and maps of other types,
	// structs: through their JSON form.
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16:
		return value(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return value(int64(rv.Uint()))
	}
	b, err := json.Marshal(v)
	if err != nil {
		// Not fmt: it would follow a map or list that holds itself forever.
		return map[string]any{"stringValue": err.Error()}
	}
	var plain any
	if json.Unmarshal(b, &plain) == nil {
		switch p := plain.(type) {
		case map[string]any, []any, string, bool, float64:
			return valueIn(p, path)
		}
	}
	return map[string]any{"stringValue": string(b)}
}

// attributes are OTLP key-values, in key order; nil and empty values are
// left out.
func attributes(m map[string]any) []any { return attributesIn(m, nil) }

func attributesIn(m map[string]any, path []uintptr) []any {
	keys := slices.Sorted(maps.Keys(m))
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		if empty(m[k]) {
			continue
		}
		out = append(out, map[string]any{"key": k, "value": valueIn(m[k], path)})
	}
	return out
}

func empty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case []string:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	case map[string]string:
		return len(x) == 0
	}
	return false
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

// resource describes the app, as every request carries it.
func (c *Client) resource() map[string]any {
	return map[string]any{"attributes": attributes(map[string]any{
		"service.name": c.opts.ServiceName, "service.version": c.opts.Release,
		"deployment.environment.name": c.opts.Environment, "host.name": c.opts.ServerName,
		"telemetry.sdk.name": sdkName, "telemetry.sdk.version": sdkVersion, "telemetry.sdk.language": "go",
	})}
}

func scope() map[string]any { return map[string]any{"name": sdkName, "version": sdkVersion} }

// encodeLogs is an OTLP logs export of records.
func (c *Client) encodeLogs(records ...map[string]any) ([]byte, error) {
	recs := make([]any, len(records))
	for i, r := range records {
		recs[i] = r
	}
	return json.Marshal(map[string]any{"resourceLogs": []any{map[string]any{
		"resource": c.resource(), "scopeLogs": []any{map[string]any{"scope": scope(), "logRecords": recs}},
	}}})
}

// eventRecord is an error or a message as a log record (sdks/PROTOCOL.md §4),
// redacted.
func (c *Client) eventRecord(e *Event) map[string]any {
	a := map[string]any{
		"fixwire.event_id": e.EventID, "fixwire.tags": e.Tags, "fixwire.transaction": e.Transaction,
		"fixwire.fingerprint": e.Fingerprint,
		"user.id":             e.User.ID, "user.email": e.User.Email, "user.name": e.User.Username,
		"client.address": e.User.IPAddress,
	}
	if e.suppressed > 0 {
		a["fixwire.suppressed"] = e.suppressed
	}
	if len(e.Contexts) > 0 {
		contexts := map[string]any{}
		for k, v := range e.Contexts {
			contexts[k] = v
		}
		a["fixwire.contexts"] = contexts
	}
	for k, v := range e.Extra {
		if _, taken := a[k]; !taken {
			a[k] = v
		}
	}
	if len(e.Breadcrumbs) > 0 {
		crumbs := make([]any, len(e.Breadcrumbs))
		for i, b := range e.Breadcrumbs {
			crumbs[i] = map[string]any{
				"timestamp": float64(b.Timestamp.UnixMicro()) / 1e6, "type": b.Type, "category": b.Category,
				"message": b.Message, "level": string(b.Level), "data": b.Data,
			}
		}
		a["fixwire.breadcrumbs"] = crumbs
	}
	if r := e.Request; r != nil {
		a["http.request.method"], a["url.full"], a["url.query"] = r.Method, r.URL, r.Query
		for name, v := range r.Headers {
			if strings.EqualFold(name, "User-Agent") {
				a["user_agent.original"] = v
			} else {
				a["http.request.header."+strings.ToLower(name)] = v
			}
		}
	}
	record := map[string]any{
		"timeUnixNano": nanos(e.Timestamp), "severityNumber": severity[e.Level], "severityText": strings.ToUpper(string(e.Level)),
	}
	if e.TraceID != "" {
		record["traceId"], record["spanId"] = e.TraceID, e.SpanID
	}
	if len(e.Exceptions) == 0 {
		record["eventName"] = "fixwire.message"
		record["body"] = value(c.mask(e.Message))
	} else {
		record["eventName"] = "exception"
		outer := e.Exceptions[0]
		a["exception.type"], a["exception.message"] = outer.Type, outer.Message
		chain := make([]any, len(e.Exceptions))
		handled := true
		for i, x := range e.Exceptions {
			frames := make([]any, len(x.Frames))
			for j, f := range x.Frames {
				frames[j] = map[string]any{
					"function": f.Function, "module": f.Module, "file": f.File, "abs_path": f.AbsPath, "line": f.Line,
					"in_app": f.InApp, "context_line": f.ContextLine, "pre_context": f.PreContext, "post_context": f.PostContext,
				}
			}
			chain[i] = map[string]any{
				"type": x.Type, "message": x.Message, "module": x.Module, "frames": frames,
				"mechanism": map[string]any{"type": x.Mechanism.Type, "handled": x.Mechanism.Handled},
			}
			handled = handled && x.Mechanism.Handled
		}
		a["fixwire.exceptions"] = chain
		if !handled {
			a["fixwire.handled"] = false
		}
		if e.Message != "" {
			record["body"] = value(c.mask(e.Message))
		}
	}
	record["attributes"] = attributes(c.scrub(a, "fixwire.event_id"))
	return record
}
