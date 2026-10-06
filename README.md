<div align="center">

_Bugs reach production. Fixwire finds them first: errors, traces, logs and
AI agent runs in one place, an AI debugger on every plan, and your data
kept in Europe._

[![Discord](https://img.shields.io/badge/Discord-join%20us-5865F2?logo=discord&logoColor=white)](https://fixwire.io/discord)
[![Slack](https://img.shields.io/badge/Slack-community-4A154B?logo=slack&logoColor=white)](https://fixwire.io/slack)
[![X](https://img.shields.io/badge/X-follow%20us-000000?logo=x&logoColor=white)](https://fixwire.io/x)
[![Release](https://img.shields.io/github/v/release/fixwire/fixwire-go?label=release)](https://github.com/fixwire/fixwire-go/releases)
[![Go](https://img.shields.io/badge/go-1.23%20%7C%201.26%20%7C%201.27-blue?logo=go&logoColor=white)](https://github.com/fixwire/fixwire-go/blob/main/.github/workflows/ci.yml)
[![CI](https://github.com/fixwire/fixwire-go/actions/workflows/ci.yml/badge.svg)](https://github.com/fixwire/fixwire-go/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](https://github.com/fixwire/fixwire-go/blob/main/LICENSE)

<br/>

</div>

# Fixwire SDK for Go

Welcome to the official Go SDK for **[Fixwire](https://fixwire.io)**. It
captures errors and panics, traces, `log/slog` records, release health,
cron monitors and user feedback from your Go services and jobs.

## 📦 Getting started

### Prerequisites

- A Fixwire account and project: sign up at
  [fixwire.io](https://fixwire.io).
- Go 1.23 or newer. The SDK uses the standard library only.

### Installation

```sh
go get github.com/fixwire/fixwire-go
```

### Basic configuration

Call `Init` once, at the start of `main`:

```go
package main

import (
    "log"
    "time"

    fixwire "github.com/fixwire/fixwire-go"
)

func main() {
    err := fixwire.Init(fixwire.Options{
        DSN:              "https://fw_pk_live_…@ingest.eu.fixwire.io",
        Release:          "api@1.4.0",
        Environment:      "production",
        TracesSampleRate: 0.2, // keep a fifth of new traces
        // SendDefaultPII: true,   // also send users' IP addresses and identifying headers
        // DisableRedaction: true, // stop masking secrets and personal data on the device
    })
    if err != nil {
        log.Printf("fixwire is off: %v", err) // the app starts all the same
    }
    defer fixwire.Close(2 * time.Second) // sends what is left before exiting

    // … your app
}
```

The DSN is your project's publishable key and the ingest host:
`https://<publishable key>@<host>`. Without the `DSN` option the SDK reads
`FIXWIRE_DSN`; without either it does nothing, so the same code runs in
tests and on your laptop. `FIXWIRE_RELEASE` and `FIXWIRE_ENVIRONMENT` work
the same way. `Init` never panics: a broken DSN comes back as an error and
the SDK stays off.

### Quick usage example

```go
fixwire.CaptureMessage("Hello Fixwire!") // a message event, with the scope's user, tags and breadcrumbs

if err := charge(order); err != nil {
    fixwire.CaptureException(err) // an issue: the error, its chain of causes and the stack that captured it
}
```

Add who and what the work is for, and what happened before an error:

```go
fixwire.ConfigureScope(func(s *fixwire.Scope) {
    s.SetUser(fixwire.User{ID: "user-1"})
    s.SetTag("plan", "team")
})
fixwire.AddBreadcrumb(fixwire.Breadcrumb{Category: "cart", Message: "checkout started"})
fixwire.CaptureMessage("disk usage above 90%")
```

## ✨ Why Fixwire

- **Secrets stay on the device.** Secrets and personal data are masked
  before anything is sent, with the same rules as the Fixwire server
  (`DisableRedaction` turns it off).
- **A crash loop costs a few events and a count, not your quota.** Each
  issue sends a burst, then a few a minute; what is held back is counted
  (`ErrorBudget`).
- **It never gets in your app's way.** `Init` never panics, and captures
  never block: one goroutine sends from a bounded queue, retries with
  backoff and honours rate limits, pausing only the kind of data a limit
  names. Memory and time stay bounded, and a panic in your own callbacks or
  methods (`BeforeSend`, `Error`, `String`) is caught, not passed to you.
- **OpenTelemetry-native.** It speaks the Fixwire protocol
  (OpenTelemetry's OTLP/HTTP plus a few small JSON endpoints): errors,
  messages and spans travel as OTLP/HTTP JSON, with structured stack
  traces, breadcrumbs and redaction on top.
- **Trace headers only where you allow.** Outgoing requests carry them
  only to your `TracePropagationTargets`; none by default.
- **Your data stays in Europe.** Fixwire is hosted in Europe.
- **No dependencies.** The standard library only, from Go 1.23 on.

## 🧩 Integrations

| Integration | What it does | How to use |
| --- | --- | --- |
| `net/http` servers | A scope, a release-health session and (with tracing on) a server span per request, named after the route the `ServeMux` matched; panics reported and answered with a 500 | `fixwirehttp.New(fixwirehttp.Options{}).Handle(mux)` |
| `net/http` clients | Outgoing requests as client spans and breadcrumbs; trace headers to your `TracePropagationTargets` only | `&http.Client{Transport: fixwirehttp.NewTransport(nil)}` |
| `log/slog` | Records at `Info` and above become breadcrumbs; `Error` and above are sent as events | `slog.New(fixwire.NewSlogHandler(next, nil))` |
| Tracing | Spans with W3C trace context, sent with the segment they belong to | `fixwire.StartSpan(ctx, "SELECT carts", fixwire.WithOp("db.query"))` |
| Panics | Reported, then the panic goes on | `defer fixwire.Recover()` |
| Cron monitors | Check-ins that tell a monitor when a job ran, how long it took and how it ended | `fixwire.WithMonitor(ctx, "nightly-report", config, job)` |
| Feedback | What users say about an error or an AI answer | `fixwire.CaptureFeedback(fixwire.Feedback{…})` |
| `pkg/errors`, `go-errors` | Errors that record their own stack keep it | Nothing to do |

### Errors and panics

Go errors carry no stack, so an error gets the stack of the line that
captured it. One made by a package that records its stack (a `StackTrace()`
or `Callers()` method, as `pkg/errors` and `go-errors` have) keeps its own.
A `fmt.Errorf("…: %w", err)` chain is sent as the chain of causes (of an
`errors.Join`, the first error's).

`defer fixwire.Recover()` at the top of a goroutine reports a panic and
lets it go on, so the program behaves as it would without Fixwire.

### HTTP servers and clients

```go
import "github.com/fixwire/fixwire-go/fixwirehttp"

handler := fixwirehttp.New(fixwirehttp.Options{}).Handle(mux)
client := &http.Client{Transport: fixwirehttp.NewTransport(nil)}
```

Each request gets its own scope (with the request on it), so what a
handler sets stays with that request; handlers reach it with
`fixwire.HubFromContext(r.Context())`. Panics are reported and answered
with a 500 (`Repanic` hands them on instead; `WaitForDelivery` waits up to
`Timeout`, 2 s by default, for the report to be sent, for platforms that
stop the process once it answers). Each request is counted for release
health, and with tracing on it is a server span named after the route the
`ServeMux` matched (`GET /items/{id}`). A caller's `traceparent`,
`tracestate` and `baggage` continue its trace.

Outgoing requests through the transport become client spans and
breadcrumbs, and carry trace headers only to `TracePropagationTargets`.

### Tracing

```go
span, ctx := fixwire.StartSpan(ctx, "SELECT carts", fixwire.WithOp("db.query"))
defer span.Finish()
```

`TracesSampleRate` sets the share of new traces kept. A span without a parent in the
process (a request, a job) is sent with the spans under it when it
finishes. `fixwire.ContinueTrace(traceparent, tracestate, baggage)`
continues a caller's trace; its sampling decision holds.

### Logs with slog

```go
logger := slog.New(fixwire.NewSlogHandler(slog.NewJSONHandler(os.Stderr, nil), nil))
logger.ErrorContext(ctx, "charge failed", "err", err, "order", order.ID)
```

Records at `Info` and above become breadcrumbs; `Error` and above are sent
as events, as the error in their `err` attribute when there is one. The
records still go to the handler you wrap. `SlogOptions` change both levels.

### Cron monitors

```go
err := fixwire.WithMonitor(ctx, "nightly-report",
    &fixwire.MonitorConfig{Schedule: fixwire.CrontabSchedule("0 3 * * *"), Timezone: "Europe/Berlin"},
    func(ctx context.Context) error { return report(ctx) })
```

The monitor notices runs that fail, take too long or never happen. The
first check-in with a config creates it. `fixwire.CaptureCheckIn` sends
check-ins by hand.

### Feedback

Rate an AI answer, or say what went wrong with a crash. A negative score
opens a `user_feedback` issue for the agent:

```go
fixwire.CaptureFeedback(fixwire.Feedback{Message: "Refunded the wrong order", Score: -1, TraceID: runTraceID})
```

## ⚙️ Configuration

| Option | Default | What it does |
| --- | --- | --- |
| `DSN` | `FIXWIRE_DSN` | Where to send; nothing is sent without one |
| `Release` | `FIXWIRE_RELEASE` | The app's version (`api@1.4.0`, a commit SHA); release health needs one |
| `Environment` | `FIXWIRE_ENVIRONMENT`, else `production` | Where the app runs |
| `ServerName` | the host name | Names the machine |
| `ServiceName` | `OTEL_SERVICE_NAME`, else `api` of `api@1.4.0` | Names the service |
| `SampleRate` | 1 | Share of errors and messages sent |
| `TracesSampleRate` | 0 | Share of new traces kept; a trace continued from a caller follows its decision |
| `TracePropagationTargets` | none | Hosts and URL prefixes whose requests carry trace headers |
| `BeforeSend`, `BeforeBreadcrumb` | | Change an event or a breadcrumb, or drop it by returning `nil` |
| `ErrorBudget` | 10 per issue, then 1 a minute; 600 a minute in all | Bounds the events sent; `Disabled` sends every one |
| `MaxBreadcrumbs` | 100 | Breadcrumbs kept per scope |
| `MaxValueLength` | 1024 | Bytes of UTF-8 per string sent, cut on a character boundary and ending in `...` (masked before the cut) |
| `MaxStackFrames` | 100 | Frames sent per error, the newest kept |
| `SendDefaultPII` | off | Send the user's IP address and request headers that may identify them |
| `DisableRedaction` | off (masking on) | Stop masking secrets and personal data on the device |
| `SensitiveKeys` | the server's (`password`, `token`, `cookie`, …) | Key fragments whose values are filtered whole; replaces the defaults |
| `ContextLines` | 5 | Source lines around in-app frames, when the files are there; -1 turns it off |
| `InAppInclude`, `InAppExclude` | the main module | Package prefixes that are your code, or libraries; the standard library and the module cache are libraries already |
| `MaxQueue` | 100 | Requests waiting to be sent; past it, new data is dropped |
| `HTTPClient` | a client with a 10 s timeout | Sends to Fixwire; it never follows a redirect, so the key goes to the DSN's host only |
| `SessionInterval` | 1 minute | How often release health is sent |
| `AutoSessionTracking` | on, with a release | A pointer to `false` turns request sessions off |
| `Debug` | off | Logs what the SDK does to stderr |

### Trace propagation targets

Trace headers go only to `TracePropagationTargets`, compared with the URL
without its user info, query and fragment:

- a target with `://` matches the URLs that start with it
  (`https://api.example.com/v2`);
- any other is a host, with a port if it has one, and matches that host
  and its subdomains: `example.com` matches `api.example.com`, not
  `badexample.com` or `example.com.evil.net`;
- a path (`/api`) is a browser page's own origin; in a Go service it
  matches nothing.

### Before send

`BeforeSend` sees every error and message after the scope is applied, and
may change it or drop it:

```go
BeforeSend: func(e *fixwire.Event) *fixwire.Event {
    if e.Transaction == "GET /healthz" {
        return nil // never report the health check
    }
    e.User.Email = ""
    return e
},
```

A panic in `BeforeSend` or `BeforeBreadcrumb` is caught and the event
skipped; it never reaches your app.

### Sampling

`SampleRate` keeps a share of errors and messages. `TracesSampleRate`
decides each new trace from its trace id, the same way in every Fixwire
SDK, so the services of one trace agree. A trace continued from a caller
follows the caller's decision.

### Error budget

Each issue may send 10 events at once, then 1 a minute, within 600 a
minute across issues. Occurrences held back are counted and ride on the
issue's next event, so issue counts stay right. Change the numbers with
`ErrorBudget`, or set `Disabled` to send every event.

### Redaction

Secrets (keys, tokens, private keys, passwords in URLs) and personal data
(emails, card numbers, IBANs, phone numbers, IP addresses) are masked on
the device, with the same rules as the Fixwire server: in messages,
attributes, span names and status messages, breadcrumbs, feedback, URLs
and their queries, and the keys of maps. Redaction runs before a string is
cut, over the part kept and the next 16 kB, so a secret the cut goes
through is still masked; a value redaction fails on is sent as
`[Filtered]`. The values of sensitive keys (`SensitiveKeys`) are filtered
whole. Your app's own configuration (release, environment, service and
server names, a monitor's slug and config) is cut to `MaxValueLength` but
never masked: `api@1.2.3.example` stays a release.

### Limits

Values (contexts, extras, attributes, breadcrumb data) are walked 10
levels deep and 100 items wide; maps and lists that hold themselves are
cut. An event over 1 MB leaves out its breadcrumbs, then its contexts. A
span keeps 128 attributes, a segment 1,000 child spans. A request is sent
at most 4 times. A caller's `traceparent` is used only when it is well
formed, and its `tracestate` (512 bytes) and `baggage` (8 KB) are passed
on only within W3C's limits.

## 🧪 Examples

Real programs, run by their tests against a fake ingest, so they keep
working:

- [shop-api](https://github.com/fixwire/fixwire-go/tree/main/examples/shop-api):
  an HTTP API with the `fixwirehttp` middleware, the signed-in user,
  handled errors, panics, a traced call to another service and `slog`
  breadcrumbs.
- [nightly-report](https://github.com/fixwire/fixwire-go/tree/main/examples/nightly-report):
  a cron job with check-ins to a monitor, one scope per account, a trace
  for the run, `Recover` and `Close` before exiting.

## 📚 Documentation

The full guide lives in this README and the examples.

- [Configuration](https://github.com/fixwire/fixwire-go#%EF%B8%8F-configuration)
- [Examples](https://github.com/fixwire/fixwire-go/tree/main/examples)
- [Changelog](https://github.com/fixwire/fixwire-go/blob/main/CHANGELOG.md)
- [Security policy](https://github.com/fixwire/fixwire-go/blob/main/SECURITY.md)
- [Contributing guide](https://github.com/fixwire/fixwire-go/blob/main/CONTRIBUTING.md)

## 🚧 Coming from another error tracker?

The API follows the shape most error-tracking SDKs share: `Init`,
`CaptureException`, `CaptureMessage`, `SetUser`, `SetTag`,
`AddBreadcrumb`, `StartSpan`, scopes and hubs. Moving over is mostly a
change of import path and DSN. As Go has it, options are a struct, `Init`
returns an error, and a request's hub and spans travel in its
`context.Context` (`NewContext`, `HubFromContext`, `StartSpan(ctx, …)`).

## 🙌 Want to contribute?

We'd love your help, from a typo fix to a new integration. Read the
[contributing guide](https://github.com/fixwire/fixwire-go/blob/main/CONTRIBUTING.md),
then pick one of the
[open issues](https://github.com/fixwire/fixwire-go/issues) or a
[good first issue](https://github.com/fixwire/fixwire-go/issues?q=is%3Aopen+label%3A%22good+first+issue%22).

## 🛟 Need help?

- Questions: ask on [Discord](https://fixwire.io/discord) or
  [Slack](https://fixwire.io/slack).
- Bugs: open a [GitHub issue](https://github.com/fixwire/fixwire-go/issues).

Found a security issue? Please don't open an issue; follow the
[security policy](https://github.com/fixwire/fixwire-go/blob/main/SECURITY.md).

## 🔗 Resources

- [Website](https://fixwire.io)
- [Pricing](https://fixwire.io/pricing)
- [Discord](https://fixwire.io/discord)
- [Slack](https://fixwire.io/slack)
- [X](https://fixwire.io/x)
- [Changelog](https://github.com/fixwire/fixwire-go/blob/main/CHANGELOG.md)
- [Examples](https://github.com/fixwire/fixwire-go/tree/main/examples)
- [Security policy](https://github.com/fixwire/fixwire-go/blob/main/SECURITY.md)

## 📃 License

The SDK is open source under the MIT license; see
[LICENSE](https://github.com/fixwire/fixwire-go/blob/main/LICENSE).

## 😘 Contributors

Thanks to everyone who helps make Fixwire better!

<a href="https://github.com/fixwire/fixwire-go/graphs/contributors"><img src="https://contrib.rocks/image?repo=fixwire/fixwire-go" alt="Contributors" /></a>
