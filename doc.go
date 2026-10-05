// Package fixwire is the Fixwire SDK for Go: errors, panics, traces, slog,
// release health, cron check-ins and feedback, sent over Fixwire protocol v1
// (OpenTelemetry's OTLP plus a few Fixwire endpoints).
//
//	err := fixwire.Init(fixwire.Options{
//		DSN:     "https://fw_pk_live_…@ingest.eu.fixwire.io",
//		Release: "api@1.4.0",
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer fixwire.Close(2 * time.Second)
//
//	if err := charge(order); err != nil {
//		fixwire.CaptureException(err)
//	}
//
// Requests get their own scope from the fixwirehttp middleware; in other
// code, pass the Hub along in a context (NewContext, HubFromContext).
package fixwire
