# fixwire for Go

The Fixwire SDK for Go: errors and panics, traces, release health, cron
monitors and feedback. Standard library only; Go 1.23 or newer.

```sh
go get github.com/fixwire/fixwire-go
```

```go
import fixwire "github.com/fixwire/fixwire-go"

func main() {
	err := fixwire.Init(fixwire.Options{
		DSN:     "https://fw_pk_live_…@ingest.eu.fixwire.io",
		Release: "api@1.4.0",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer fixwire.Close(2 * time.Second) // sends what is left before exiting

	if err := charge(order); err != nil {
		fixwire.CaptureException(err)
	}
}
```

The DSN is your project's publishable key and the ingest host,
`https://<key>@<host>`. Without the `DSN` option the SDK reads
`FIXWIRE_DSN`; without either it does nothing. `FIXWIRE_RELEASE` and
`FIXWIRE_ENVIRONMENT` work the same way.

**What's different**
- Secrets and personal data are masked on the device, with the same rules
  as the Fixwire server (`DisableRedaction` turns it off).
- A crash loop costs a few events and a count, not your quota
  (`ErrorBudget`).
- Captures never block: one goroutine sends from a bounded queue, retries
  with backoff and honours rate limits, pausing only the kind of data a
  limit names.
- It speaks the Fixwire protocol: errors, messages and spans travel as
  OpenTelemetry's OTLP/HTTP (JSON), with structured stack traces,
  breadcrumbs and redaction on top.

## Errors

Go errors carry no stack, so an error gets the stack of the line that
captured it. One made by a package that records its stack (a `StackTrace()`
or `Callers()` method, as `pkg/errors` and `go-errors` have) keeps its own.
A `fmt.Errorf("…: %w", err)` chain is sent as the chain of causes (of an
`errors.Join`, the first error's).

```go
fixwire.ConfigureScope(func(s *fixwire.Scope) {
	s.SetUser(fixwire.User{ID: "user-1"})
	s.SetTag("plan", "team")
})
fixwire.AddBreadcrumb(fixwire.Breadcrumb{Category: "cart", Message: "checkout started"})
fixwire.CaptureMessage("disk usage above 90%")
```

Panics: `defer fixwire.Recover()` at the top of a goroutine reports a panic
and lets it go on, so the program behaves as it would without Fixwire.

## HTTP servers and clients

```go
import "github.com/fixwire/fixwire-go/fixwirehttp"

handler := fixwirehttp.New(fixwirehttp.Options{}).Handle(mux)
```

Each request gets its own scope (with the request on it), so what a
handler sets stays with that request; handlers reach it with
`fixwire.HubFromContext(r.Context())`. Panics are reported and answered
with a 500 (`Repanic` hands them on instead). Each request is counted for
release health, and with tracing on it is a server span named after the
route the `ServeMux` matched (`GET /items/{id}`).

```go
client := &http.Client{Transport: fixwirehttp.NewTransport(nil)}
```

Outgoing requests become client spans and breadcrumbs. Trace headers go
only to the URLs in `TracePropagationTargets`.

## Tracing

```go
fixwire.Init(fixwire.Options{TracesSampleRate: 0.2})

span, ctx := fixwire.StartSpan(ctx, "SELECT carts", fixwire.WithOp("db.query"))
defer span.Finish()
```

A span without a parent in the process (a request, a job) is sent with the
spans under it when it finishes. `fixwire.ContinueTrace(traceparent,
tracestate, baggage)` continues a caller's trace; its sampling decision
holds.

## Logs (slog)

```go
logger := slog.New(fixwire.NewSlogHandler(slog.NewJSONHandler(os.Stderr, nil), nil))
logger.ErrorContext(ctx, "charge failed", "err", err, "order", order.ID)
```

Records at `Info` and above become breadcrumbs; `Error` and above are sent
as events, as the error in their `err` attribute when there is one. The
records still go to the handler you wrap.

## Cron jobs

```go
err := fixwire.WithMonitor(ctx, "nightly-report",
	&fixwire.MonitorConfig{Schedule: fixwire.CrontabSchedule("0 3 * * *"), Timezone: "Europe/Berlin"},
	func(ctx context.Context) error { return report(ctx) })
```

The monitor notices runs that fail, take too long or never happen.
`fixwire.CaptureCheckIn` sends check-ins by hand.

## Feedback

Rate an AI answer, or say what went wrong with a crash. A negative score
opens a `user_feedback` issue for the agent:

```go
fixwire.CaptureFeedback(fixwire.Feedback{Message: "Refunded the wrong order", Score: -1, TraceID: runTraceID})
```

## Options

| Option | Default | |
|---|---|---|
| `DSN` | `FIXWIRE_DSN` | Where to send; nothing is sent without one |
| `Release`, `Environment` | `FIXWIRE_RELEASE`, `production` | Release health needs a release |
| `ServiceName` | `OTEL_SERVICE_NAME`, else `api` of `api@1.4.0` | |
| `SampleRate` | 1 | Share of errors sent |
| `TracesSampleRate` | 0 | Share of new traces kept |
| `TracePropagationTargets` | none | URLs that receive trace headers |
| `BeforeSend`, `BeforeBreadcrumb` | | Change or drop events and breadcrumbs |
| `SendDefaultPII` | off | Send the user's IP address and identifying headers |
| `DisableRedaction`, `SensitiveKeys` | on, the server's keys | On-device masking |
| `ErrorBudget` | 10 per issue, then 1 a minute; 600 a minute | |
| `InAppInclude`, `InAppExclude` | the main module | Which frames are your code |
| `ContextLines` | 5 | Source lines around in-app frames, when the files are there |

## Examples

[examples](examples) holds real programs, run by its tests against a fake
ingest: an HTTP API ([shop-api](examples/shop-api)) and a cron job
([nightly-report](examples/nightly-report)).

## License

MIT.
