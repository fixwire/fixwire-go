# Changelog

All notable changes to the Fixwire Go SDK are listed here. Versions follow [Semantic
Versioning](https://semver.org); before 1.0, a minor version may change the
API.

## [0.1.1] - 2026-10-06

- `Init` with a broken DSN returns the error and leaves the SDK off, never panicking; the README and examples log it and start the app all the same.
- Requests to Fixwire never follow a redirect, so the key in `Authorization` goes to the DSN's host only; a DSN key with spaces or control characters is refused.
- `Close` ends the request in flight and drops what is queued, so it returns in time; a `Flush` that times out leaves no goroutine behind.
- Retries stay bounded: `Retry-After` and `Fixwire-Rate-Limits` are clamped, at most `MaxQueue` requests wait to be sent again, and unknown categories are ignored.
- Debug lines go to stderr directly, never through `log` to a `slog` handler of the SDK's, where they were captured.
- A panic in an error's or a value's own methods (`Error` of a nil pointer, `String`, `MarshalJSON`) no longer reaches the app.
- A NaN or an infinity no longer costs an event its other attributes; maps and lists that hold themselves are cut (with `DisableRedaction` they overflowed the stack).
- Redaction stays linear on text with many findings and on maps with many keys that mask alike.
- Breadcrumbs are added in constant time and shared by cloned scopes (one per request); the error budget forgets issues in constant time and reads a message's first kilobyte only.
- A caller's `tracestate` over 512 bytes or `baggage` over 8 KB, or with a control character other than tab (W3C's list whitespace), is not passed on.
- Source context is read outside the cache's lock, from regular files only; errors' own stacks are capped at 100 frames.
- Release health keeps at most 5000 counts between sends; past that, sessions are counted without their user. A sessions request holds at most 5000 aggregates.
- Limits as every Fixwire SDK has them:
  - `MaxValueLength` (default 1024): strings are cut to that many bytes of UTF-8, on a character boundary, ending in `...`; redaction runs first, over the part kept and the next 16 kB, so a secret the cut goes through is masked. The app's own configuration (release, environment, service and server names, a monitor's slug and config) is cut but never masked: `api@1.2.3.example` stays a release.
  - `MaxStackFrames` (default 100): frames per error, the newest kept. A chain ends where it comes back to an error already in it.
  - Values (contexts, extras, attributes, breadcrumb data) are walked 10 levels deep, 100 items wide and 10,000 maps and lists at most, never through `encoding/json` on the app's whole value: `[Circular ~]`, `[Object]`/`[Array]` and `[Unreadable]` mark what is left out; NaN and the infinities are `"NaN"`, `"Infinity"`, `"-Infinity"`.
  - An event over 1 MB leaves out its breadcrumbs, then its contexts, then is dropped; spans go 100 to a request of at most 5 MB, a span too large for one dropped alone; a span keeps at most 128 attributes.
  - `Retry-After` may be an HTTP date; values past a day are a day; a `5xx` with `Retry-After` pauses everything for that long. Retries wait about 1 s, 2 s and 4 s.
  - An incoming `traceparent` must be version `00`, four fields, in lower-case hex; an upper-case one is ignored.
  - Source files of up to 10 MB give context lines, through a cache of at most 64 files and 32 MB.
- `TracePropagationTargets` match a host and its subdomains, or a URL prefix (with `://`), compared without user info, query and fragment: `example.com` no longer matches `badexample.com`, `example.com.evil.net` or a URL that merely mentions it.
- Span status messages are masked; a value redaction fails on is sent as `[Filtered]`.
- `secret_assignment` follows the server: a secret's name may end a longer one (`access_token`, `client_secret`, `csrfToken`, `PHPSESSID`, `X-Amz-Signature`), and an OAuth `code` in a query or fragment is masked.
- What the app logs through the slog handler while the SDK captures (from `BeforeSend`, `BeforeBreadcrumb` or an error's `Error`) is passed on, not captured again; a panicking `BeforeBreadcrumb` no longer reaches the app.

## [0.1.0] - 2026-10-06

First release.

- Errors with their wrapped chain, panics (`Recover`, and in `fixwirehttp`), messages, breadcrumbs and scopes carried in `context.Context`.
- Tracing with W3C trace context; `fixwirehttp` middleware (a server span per request, named after its route) and transport (client spans, trace headers to the propagation targets only).
- Release health (request sessions), cron monitors (`WithMonitor`) and feedback.
- A `log/slog` handler: records as breadcrumbs and events.
- On-device redaction with the server's rules; an error budget for crash loops; rate limits honoured per kind of data.
- Examples run against a fake ingest in CI: an HTTP API and a cron job.
