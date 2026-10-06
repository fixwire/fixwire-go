// Command nightly-report is a cron job reporting to Fixwire: check-ins tell
// its monitor when it ran and how it ended, a failing account is reported
// and the job carries on, and a summary warning goes out at the end.
//
//	FIXWIRE_DSN=https://<key>@<host> go run ./nightly-report
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	fixwire "github.com/fixwire/fixwire-go"
)

// ErrNoData is what an account without invoices gives.
var ErrNoData = errors.New("no invoices")

var accounts = []string{"acme", "globex", "initech"}

func main() {
	if err := fixwire.Init(fixwire.Options{Release: "nightly-report@1.0.0", TracesSampleRate: 1}); err != nil {
		log.Fatal(err)
	}
	// A panic anywhere in the job is reported before the program dies.
	defer fixwire.Recover()

	// The first check-in creates the monitor: every night at 3, in Berlin.
	monitor := &fixwire.MonitorConfig{
		Schedule: fixwire.CrontabSchedule("0 3 * * *"), Timezone: "Europe/Berlin",
		CheckInMargin: 10, MaxRuntime: 30,
	}
	err := fixwire.WithMonitor(context.Background(), "nightly-report", monitor, run)
	// A short-lived program sends what is left before it exits.
	fixwire.Close(5 * time.Second)
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	job, ctx := fixwire.StartSpan(ctx, "nightly-report", fixwire.WithOp("task"))
	defer job.Finish()
	// Errors captured from here on belong to the job's trace.
	fixwire.ConfigureScope(func(s *fixwire.Scope) { s.SetSpan(job) })

	failed := 0
	for _, account := range accounts {
		if err := report(ctx, account); err != nil {
			failed++
			// One scope per account, so its tag doesn't stay on the next.
			fixwire.WithScope(func(s *fixwire.Scope) {
				s.SetTag("account", account)
				fixwire.CaptureException(err)
			})
		}
	}
	if failed > 0 {
		fixwire.ConfigureScope(func(s *fixwire.Scope) { s.SetLevel(fixwire.LevelWarning) })
		fixwire.CaptureMessage(fmt.Sprintf("nightly report: %d of %d accounts failed", failed, len(accounts)))
		return fmt.Errorf("%d accounts failed", failed)
	}
	return nil
}

func report(ctx context.Context, account string) error {
	span, _ := fixwire.StartSpan(ctx, "report "+account, fixwire.WithOp("task"))
	defer span.Finish()
	fixwire.AddBreadcrumb(fixwire.Breadcrumb{Category: "report", Message: "building " + account})
	if account == "globex" {
		err := fmt.Errorf("building the report for %s: %w", account, ErrNoData)
		span.SetError(err)
		return err
	}
	return nil
}
