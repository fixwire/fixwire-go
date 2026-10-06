# Changelog

All notable changes to the Fixwire Go SDK are listed here. Versions follow [Semantic
Versioning](https://semver.org); before 1.0, a minor version may change the
API.

## [0.1.0] - 2026-10-06

First release.

- Errors with their wrapped chain, panics (`Recover`, and in `fixwirehttp`), messages, breadcrumbs and scopes carried in `context.Context`.
- Tracing with W3C trace context; `fixwirehttp` middleware (a server span per request, named after its route) and transport (client spans, trace headers to the propagation targets only).
- Release health (request sessions), cron monitors (`WithMonitor`) and feedback.
- A `log/slog` handler: records as breadcrumbs and events.
- On-device redaction with the server's rules; an error budget for crash loops; rate limits honoured per kind of data.
- Examples run against a fake ingest in CI: an HTTP API and a cron job.
