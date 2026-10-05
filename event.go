package fixwire

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"
)

// Level is an event's or a breadcrumb's severity.
type Level string

// The levels.
const (
	LevelDebug   Level = "debug"
	LevelInfo    Level = "info"
	LevelWarning Level = "warning"
	LevelError   Level = "error"
	LevelFatal   Level = "fatal"
)

// Event is an error or a message as it is sent: BeforeSend may change it.
type Event struct {
	EventID   string
	Timestamp time.Time
	Level     Level
	// Message is a message's text (CaptureMessage).
	Message string
	// Exceptions is an error's chain, the outermost first.
	Exceptions  []Exception
	Tags        map[string]string
	Contexts    map[string]map[string]any
	Extra       map[string]any
	User        User
	Breadcrumbs []Breadcrumb
	// Fingerprint groups the event your way; "{{ default }}" stands for
	// Fixwire's own grouping.
	Fingerprint []string
	// Transaction is the route or task it happened in.
	Transaction string
	Request     *Request
	// TraceID and SpanID link it to the trace it happened in.
	TraceID, SpanID string

	suppressed int // occurrences the error budget held back
}

// Exception is one error of a chain.
type Exception struct {
	Type      string
	Message   string
	Module    string
	Mechanism Mechanism
	// Frames run from the oldest call to the newest (the failing line
	// last).
	Frames []Frame
}

// Mechanism says how an error was caught.
type Mechanism struct {
	Type    string
	Handled bool
}

// Frame is one stack frame.
type Frame struct {
	Function    string
	Module      string
	File        string
	AbsPath     string
	Line        int
	InApp       bool
	ContextLine string
	PreContext  []string
	PostContext []string
}

// User is who the event happened to.
type User struct {
	ID        string
	Email     string
	Username  string
	IPAddress string
}

// Breadcrumb is something that happened before an event.
type Breadcrumb struct {
	Timestamp time.Time
	Type      string
	Category  string
	Message   string
	Level     Level
	Data      map[string]any
}

// Request is the HTTP request an event happened in.
type Request struct {
	Method  string
	URL     string
	Query   string
	Headers map[string]string

	ip  string        // the client's address, sent with SendDefaultPII
	src *http.Request // read for the route the mux matched
}

// route is the route the request matched, as "GET /items/{id}"; "" when
// no ServeMux matched one.
func (r *Request) route() string {
	if r == nil || r.src == nil || r.src.Pattern == "" {
		return ""
	}
	pattern := r.src.Pattern
	if _, rest, ok := strings.Cut(pattern, " "); ok { // "GET /items/{id}"
		pattern = strings.TrimSpace(rest)
	}
	if i := strings.IndexByte(pattern, '/'); i > 0 { // "example.com/items"
		pattern = pattern[i:]
	}
	return r.src.Method + " " + pattern
}

// maxChain bounds the errors of a chain read from Unwrap.
const maxChain = 10

// exceptionsOf turns an error chain into exceptions, the outermost first.
// An error that recorded its stack (StackTrace or Callers, as pkg/errors
// and go-errors do) gets it; plain Go errors carry none, so the outermost
// gets frames, the stack of where it was captured.
func exceptionsOf(err error, frames []Frame, mechanism Mechanism, opts Options) []Exception {
	var out []Exception
	for e := err; e != nil && len(out) < maxChain; e = unwrapOne(e) {
		typ, module := errorType(e)
		out = append(out, Exception{Type: typ, Message: e.Error(), Module: module, Frames: errorStack(e, opts),
			Mechanism: Mechanism{Type: "chained", Handled: mechanism.Handled}})
	}
	if len(out) > 0 {
		if len(out[0].Frames) == 0 {
			out[0].Frames = frames
		}
		out[0].Mechanism = mechanism
	}
	return out
}

// unwrapOne is the next error of a chain: Unwrap's, or the first of a
// joined error's.
func unwrapOne(err error) error {
	if e := errors.Unwrap(err); e != nil {
		return e
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		if errs := j.Unwrap(); len(errs) > 0 {
			return errs[0]
		}
	}
	return nil
}

// errorType names an error's type. The standard library's plain errors
// (errors.New, fmt.Errorf, errors.Join) are all "error": their type says
// nothing about what failed.
func errorType(err error) (typ, module string) {
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.PkgPath() + "." + t.Name() {
	case "errors.errorString", "fmt.wrapError", "fmt.wrapErrors", "errors.joinError":
		return "error", ""
	}
	if t.Name() == "" {
		return "error", ""
	}
	return t.String(), t.PkgPath()
}

// panicError turns a recovered value into an error.
func panicError(r any) error {
	if err, ok := r.(error); ok {
		return err
	}
	return fmt.Errorf("%v", r)
}
