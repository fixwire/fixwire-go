# Examples

Real programs, each with its own README. `examples_test.go` builds and runs
them against a fake ingest and checks what Fixwire receives, so they keep
working (`go test ./...` here).

| Example | Shows |
|---|---|
| [shop-api](shop-api) | `fixwirehttp` middleware: a scope, a session and a server span per request named after its route; the signed-in user; handled errors with context; panics reported as crashes (answered 500); 404s not reported; a database span; a traced call to another service with trace headers sent only to it; `slog` records as breadcrumbs; flushing on SIGTERM |
| [nightly-report](nightly-report) | A cron job: check-ins to a monitor (created from the first one), one scope per account, carrying on after a failure, a summary warning, a trace for the run, `Recover`, `Close` before exiting |
