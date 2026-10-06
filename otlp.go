package fixwire

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The SDK's name and version, as OpenTelemetry's telemetry.sdk.* say it.
const (
	sdkName    = "fixwire.go"
	sdkVersion = "0.1.1"
)

// Severity numbers of the levels (OpenTelemetry's).
var severity = map[Level]int{LevelDebug: 5, LevelInfo: 9, LevelWarning: 13, LevelError: 17, LevelFatal: 21}

// value is v, of JSON's own types (as plain and scrub leave it), as an OTLP
// JSON AnyValue.
func value(v any) map[string]any {
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
		if s, ok := finite(x).(string); ok {
			return map[string]any{"stringValue": s}
		}
		return map[string]any{"doubleValue": x}
	case bool:
		return map[string]any{"boolValue": x}
	case int:
		return map[string]any{"intValue": strconv.Itoa(x)}
	case int64:
		return map[string]any{"intValue": strconv.FormatInt(x, 10)}
	case uint64:
		return map[string]any{"intValue": strconv.FormatUint(x, 10)}
	case []any:
		vals := make([]any, len(x))
		for i, e := range x {
			vals[i] = value(e)
		}
		return map[string]any{"arrayValue": map[string]any{"values": vals}}
	case map[string]any:
		return map[string]any{"kvlistValue": map[string]any{"values": attributes(x)}}
	}
	return map[string]any{"stringValue": unreadable}
}

// attributes are OTLP key-values, in key order; nil and empty values are
// left out.
func attributes(m map[string]any) []any {
	keys := slices.Sorted(maps.Keys(m))
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		if empty(m[k]) {
			continue
		}
		out = append(out, map[string]any{"key": k, "value": value(m[k])})
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

// eventRecord is an error or a message as a log record (fixwire-protocol §4),
// redacted.
func (c *Client) eventRecord(e *Event) map[string]any {
	a := map[string]any{
		"fixwire.event_id": e.EventID, "fixwire.tags": c.plain(e.Tags), "fixwire.transaction": e.Transaction,
		"fixwire.fingerprint": c.plain(e.Fingerprint),
		"user.id":             e.User.ID, "user.email": e.User.Email, "user.name": e.User.Username,
		"client.address": e.User.IPAddress,
	}
	if e.suppressed > 0 {
		a["fixwire.suppressed"] = e.suppressed
	}
	if len(e.Contexts) > 0 {
		a["fixwire.contexts"] = c.plain(e.Contexts)
	}
	for k, v := range e.Extra {
		if _, taken := a[k]; !taken {
			a[k] = c.plain(v)
		}
	}
	if crumbs := e.Breadcrumbs[max(0, len(e.Breadcrumbs)-max(c.opts.MaxBreadcrumbs, 0)):]; len(crumbs) > 0 {
		list := make([]any, len(crumbs))
		for i, b := range crumbs {
			list[i] = map[string]any{
				"timestamp": float64(b.Timestamp.UnixMicro()) / 1e6, "type": b.Type, "category": b.Category,
				"message": b.Message, "level": string(b.Level), "data": c.plain(b.Data),
			}
		}
		a["fixwire.breadcrumbs"] = list
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
		record["body"] = value(c.text(e.Message))
	} else {
		record["eventName"] = "exception"
		outer := e.Exceptions[0]
		a["exception.type"], a["exception.message"] = outer.Type, outer.Message
		chain := make([]any, min(len(e.Exceptions), maxChain))
		handled := true
		for i, x := range e.Exceptions[:len(chain)] {
			// Frames run from the oldest: the newest are kept.
			kept := x.Frames[max(0, len(x.Frames)-c.opts.MaxStackFrames):]
			frames := make([]any, len(kept))
			for j, f := range kept {
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
			record["body"] = value(c.text(e.Message))
		}
	}
	record["attributes"] = attributes(c.scrub(a, "fixwire.event_id"))
	return record
}
