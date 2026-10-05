package fixwire

import (
	"time"
)

// Init sets up the SDK: the current hub gets a client for opts. Without a
// DSN (and FIXWIRE_DSN) the SDK stays off.
func Init(opts Options) error {
	c, err := NewClient(opts)
	if err != nil {
		return err
	}
	CurrentHub().BindClient(c)
	return nil
}

// Flush waits until what was captured is sent, or timeout; false when time
// ran out. Call it before a short-lived program exits.
func Flush(timeout time.Duration) bool { return CurrentHub().Flush(timeout) }

// Close flushes and stops the SDK.
func Close(timeout time.Duration) { CurrentHub().Client().Close(timeout) }

// CaptureException sends an error; its event id, or "" when it was not
// sent.
func CaptureException(err error) string { return CurrentHub().CaptureException(err) }

// CaptureMessage sends a message.
func CaptureMessage(message string) string { return CurrentHub().CaptureMessage(message) }

// CaptureEvent sends an event as it is.
func CaptureEvent(e *Event) string { return CurrentHub().CaptureEvent(e) }

// CaptureCheckIn reports a run of a scheduled job (see Client.CaptureCheckIn
// and WithMonitor).
func CaptureCheckIn(ci CheckIn) string { return CurrentHub().Client().CaptureCheckIn(ci) }

// CaptureFeedback sends what someone said about an error or an AI answer.
func CaptureFeedback(f Feedback) string { return CurrentHub().CaptureFeedback(f) }

// Recover reports a panic and lets it go on, so the program behaves as it
// would without Fixwire:
//
//	defer fixwire.Recover()
func Recover() {
	if r := recover(); r != nil {
		hub := CurrentHub()
		hub.RecoverPanic(r)
		hub.Flush(2 * time.Second)
		panic(r)
	}
}

// AddBreadcrumb records something that happened.
func AddBreadcrumb(b Breadcrumb) { CurrentHub().AddBreadcrumb(b) }

// ConfigureScope changes the current scope.
func ConfigureScope(f func(*Scope)) { CurrentHub().ConfigureScope(f) }

// WithScope runs f with a copy of the current scope.
func WithScope(f func(*Scope)) { CurrentHub().WithScope(f) }

// SetUser sets who the work is for.
func SetUser(u User) { CurrentHub().Scope().SetUser(u) }

// SetTag sets a searchable tag.
func SetTag(key, value string) { CurrentHub().Scope().SetTag(key, value) }

// SetContext sets a named group of details.
func SetContext(name string, values map[string]any) { CurrentHub().Scope().SetContext(name, values) }
