# Nightly report (a cron job)

```sh
FIXWIRE_DSN=https://<key>@<host> go run ./nightly-report
```

The job builds a report per account. One account (`globex`) has no
invoices: the job reports that failure with the account as a tag, carries
on with the others, sends a summary warning and exits 1.

What arrives in Fixwire:

- **Check-ins** for the `nightly-report` monitor: `in_progress` when the
  run starts and `error` when it ends, with its duration. The first
  check-in creates the monitor (every night at 3, Berlin time, 10 minutes'
  margin, 30 minutes at most), so Fixwire also notices a night the job does
  not run at all.
- **The failure**, tagged `account: globex`, with the chain `building the
  report for globex` caused by `no invoices`.
- **The summary warning**, without the account's tag: each account had
  its own scope (`fixwire.WithScope`).
- **A trace for the run**, with a span per account; the failure is linked
  to it.

How it is wired, in `main.go`:

```go
fixwire.Init(fixwire.Options{Release: "nightly-report@1.0.0", TracesSampleRate: 1})
defer fixwire.Recover() // a panic is reported before the program dies

err := fixwire.WithMonitor(ctx, "nightly-report", &fixwire.MonitorConfig{
	Schedule: fixwire.CrontabSchedule("0 3 * * *"), Timezone: "Europe/Berlin",
	CheckInMargin: 10, MaxRuntime: 30,
}, run)
fixwire.Close(5 * time.Second) // send what is left before exiting
```
