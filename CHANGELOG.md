# Changelog

All notable changes to the Fixwire Go SDK are listed here. Versions follow [Semantic
Versioning](https://semver.org); before 1.0, a minor version may change the
API.

## [Unreleased]

- Requests to Fixwire never follow a redirect, so the key in `Authorization` goes to the DSN's host only; a DSN key with spaces or control characters is refused.
- `Close` ends the request in flight and drops what is queued, so it returns in time; a `Flush` that times out leaves no goroutine behind.
- Retries stay bounded: `Retry-After` and `Fixwire-Rate-Limits` are clamped, at most `MaxQueue` requests wait to be sent again, and unknown categories are ignored.
- Debug lines go to stderr directly, never through `log` to a `slog` handler of the SDK's, where they were captured.
- A panic in an error's or a value's own methods (`Error` of a nil pointer, `String`, `MarshalJSON`) no longer reaches the app.
- A NaN or an infinity no longer costs an event its other attributes; maps and lists that hold themselves or nest past 64 levels are cut (with `DisableRedaction` they overflowed the stack).
- Redaction stays linear on text with many findings and on maps with many keys that mask alike.
- Breadcrumbs are added in constant time and shared by cloned scopes (one per request); the error budget forgets issues in constant time and reads a message's first kilobyte only.
- A caller's `tracestate` over 512 bytes or `baggage` over 8 KB, or with control characters, is not passed on.
- Source context is read outside the cache's lock, from regular files only; errors' own stacks are capped at 100 frames.
- Release health keeps at most 5000 counts between sends; past that, sessions are counted without their user.

## [0.1.0] - 2026-10-06

First release.

- Errors with their wrapped chain, panics (`Recover`, and in `fixwirehttp`), messages, breadcrumbs and scopes carried in `context.Context`.
- Tracing with W3C trace context; `fixwirehttp` middleware (a server span per request, named after its route) and transport (client spans, trace headers to the propagation targets only).
- Release health (request sessions), cron monitors (`WithMonitor`) and feedback.
- A `log/slog` handler: records as breadcrumbs and events.
- On-device redaction with the server's rules; an error budget for crash loops; rate limits honoured per kind of data.
- Examples run against a fake ingest in CI: an HTTP API and a cron job.
