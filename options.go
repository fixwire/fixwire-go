package fixwire

import (
	"net/http"
	"os"
	"strings"
	"time"
)

// Options configure the SDK. Only DSN is needed; without one (and without
// FIXWIRE_DSN) the SDK does nothing.
type Options struct {
	// DSN is the project's DSN; FIXWIRE_DSN when empty.
	DSN string
	// Release is the version of the app (FIXWIRE_RELEASE when empty), such
	// as "api@1.4.0" or a commit SHA.
	Release string
	// Environment is where it runs: "production" (the default,
	// FIXWIRE_ENVIRONMENT), "staging", …
	Environment string
	// ServerName names the machine; the host name when empty.
	ServerName string
	// ServiceName names the service: OTEL_SERVICE_NAME when empty, else the
	// name in a "name@version" release.
	ServiceName string

	// SampleRate is the share of errors and messages sent (default 1).
	SampleRate float64
	// TracesSampleRate is the share of new traces kept (default 0: no
	// tracing). Traces continued from a caller follow its decision.
	TracesSampleRate float64
	// TracePropagationTargets are where outgoing requests carry trace
	// headers (default none, so that no other service sees them). A URL is
	// compared without its user info, query and fragment. A target with
	// "://" matches the URLs that start with it ("https://api.example.com/v2");
	// any other is a host, with a port if it has one, and matches that host
	// and its subdomains ("example.com" matches api.example.com, not
	// badexample.com or example.com.evil.net). A path ("/api") is a
	// browser's own origin: here it matches nothing.
	TracePropagationTargets []string

	// ErrorBudget bounds the events sent per issue and per minute, so that a
	// crash loop costs a few events and a count.
	ErrorBudget ErrorBudget

	// BeforeSend may change an event, or drop it by returning nil.
	BeforeSend func(*Event) *Event
	// BeforeBreadcrumb may change a breadcrumb, or drop it by returning nil.
	BeforeBreadcrumb func(*Breadcrumb) *Breadcrumb
	// MaxBreadcrumbs bounds the breadcrumbs kept per scope (default 100).
	MaxBreadcrumbs int
	// MaxValueLength bounds each string sent, in bytes of UTF-8 (default
	// 1024): a longer one is cut on a character boundary and ends in "...".
	// Redaction runs before the cut, so a secret it goes through is masked.
	MaxValueLength int
	// MaxStackFrames bounds the frames sent per error (default 100): the
	// newest are kept.
	MaxStackFrames int
	// SendDefaultPII sends the user's IP address and request headers that
	// may identify them (off by default).
	SendDefaultPII bool
	// DisableRedaction stops masking secrets and personal data on the
	// device (the same rules as the server, on by default).
	DisableRedaction bool
	// SensitiveKeys replace the default key fragments (password, token,
	// cookie, …) whose values are filtered whole.
	SensitiveKeys []string
	// ContextLines is how many source lines around each frame are read
	// when the file is there (default 5; -1 turns it off).
	ContextLines int
	// InAppInclude marks frames of these package prefixes as the app's;
	// InAppExclude marks them as libraries. Frames of the standard library
	// and the module cache are libraries already.
	InAppInclude, InAppExclude []string

	// MaxQueue bounds the requests waiting to be sent (default 100).
	MaxQueue int
	// HTTPClient sends them (a client with a 10 s timeout by default).
	HTTPClient *http.Client
	// Debug logs what the SDK does to stderr.
	Debug bool

	// SessionInterval is how often request sessions are sent (default a
	// minute); AutoSessionTracking off turns them off.
	SessionInterval     time.Duration
	AutoSessionTracking *bool
}

func (o Options) withDefaults() Options {
	if o.DSN == "" {
		o.DSN = os.Getenv("FIXWIRE_DSN")
	}
	if o.Release == "" {
		o.Release = os.Getenv("FIXWIRE_RELEASE")
	}
	if o.Environment == "" {
		o.Environment = os.Getenv("FIXWIRE_ENVIRONMENT")
	}
	if o.Environment == "" {
		o.Environment = "production"
	}
	if o.ServerName == "" {
		o.ServerName, _ = os.Hostname()
	}
	if o.ServiceName == "" {
		o.ServiceName = os.Getenv("OTEL_SERVICE_NAME")
	}
	if name, _, ok := strings.Cut(o.Release, "@"); o.ServiceName == "" && ok {
		o.ServiceName = name // "api" of "api@1.4.0"
	}
	if o.SampleRate <= 0 || o.SampleRate > 1 {
		o.SampleRate = 1
	}
	if o.TracesSampleRate < 0 || o.TracesSampleRate > 1 {
		o.TracesSampleRate = 0
	}
	if o.MaxBreadcrumbs == 0 {
		o.MaxBreadcrumbs = 100
	}
	if o.MaxValueLength <= 0 {
		o.MaxValueLength = 1024
	}
	if o.MaxStackFrames <= 0 {
		o.MaxStackFrames = 100
	}
	if o.ContextLines == 0 {
		o.ContextLines = 5
	}
	if o.MaxQueue <= 0 {
		o.MaxQueue = 100
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if o.SessionInterval <= 0 {
		o.SessionInterval = time.Minute
	}
	return o
}

func (o Options) sessionsOn() bool {
	return o.Release != "" && (o.AutoSessionTracking == nil || *o.AutoSessionTracking)
}
