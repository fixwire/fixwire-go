package fixwire

import (
	"context"
	"math"
	"net/url"
	"strings"
	"time"
)

// CheckInStatus is how a scheduled job's run is going.
type CheckInStatus string

// The check-in statuses.
const (
	CheckInInProgress CheckInStatus = "in_progress"
	CheckInOK         CheckInStatus = "ok"
	CheckInError      CheckInStatus = "error"
)

// MonitorSchedule is when a job runs: a crontab ("0 3 * * *"), or an
// interval (Value units).
type MonitorSchedule struct {
	Type  string `json:"type"` // crontab or interval
	Value any    `json:"value"`
	Unit  string `json:"unit,omitempty"` // minute, hour, day, … for an interval
}

// CrontabSchedule runs a job on a crontab.
func CrontabSchedule(crontab string) MonitorSchedule {
	return MonitorSchedule{Type: "crontab", Value: crontab}
}

// IntervalSchedule runs a job every n units (minute, hour, day, week, month,
// year).
func IntervalSchedule(n int, unit string) MonitorSchedule {
	return MonitorSchedule{Type: "interval", Value: n, Unit: unit}
}

// MonitorConfig creates or updates the monitor a check-in is for.
type MonitorConfig struct {
	Schedule MonitorSchedule `json:"schedule"`
	// CheckInMargin is the minutes a check-in may be late.
	CheckInMargin int `json:"checkin_margin,omitempty"`
	// MaxRuntime is the minutes a run may take.
	MaxRuntime int `json:"max_runtime,omitempty"`
	// Timezone is the schedule's time zone, e.g. "Europe/Berlin".
	Timezone string `json:"timezone,omitempty"`
}

// CheckIn is a run of a scheduled job, reported to its monitor.
type CheckIn struct {
	// Monitor is the monitor's slug.
	Monitor string
	Status  CheckInStatus
	// ID ties the end of a run to its start; made when empty.
	ID string
	// Duration is how long the run took.
	Duration time.Duration
	// Config creates or updates the monitor.
	Config *MonitorConfig
}

// CaptureCheckIn reports a run of a scheduled job: in_progress when it
// starts, then ok or error with the returned id. Its id, or "" when it was
// not sent.
//
//	run := fixwire.CaptureCheckIn(fixwire.CheckIn{Monitor: "nightly-report", Status: fixwire.CheckInInProgress})
//	…
//	fixwire.CaptureCheckIn(fixwire.CheckIn{Monitor: "nightly-report", Status: fixwire.CheckInOK, ID: run, Duration: time.Since(start)})
func (c *Client) CaptureCheckIn(ci CheckIn) string {
	if c == nil || !c.enabled || strings.TrimSpace(ci.Monitor) == "" {
		return ""
	}
	defer c.guard()
	if ci.ID == "" {
		ci.ID = newID(16)
	}
	if ci.Status == "" {
		ci.Status = CheckInOK
	}
	body := map[string]any{"sdk": sdk(), "check_in_id": ci.ID, "status": ci.Status, "environment": c.opts.Environment}
	if ci.Duration > 0 {
		body["duration"] = ci.Duration.Seconds()
	}
	if ci.Config != nil {
		body["monitor_config"] = ci.Config
	}
	if !c.sendJSON("/v1/check-ins/"+url.PathEscape(ci.Monitor), categoryCheckIn, body) {
		return ""
	}
	return ci.ID
}

// WithMonitor runs job as a run of the monitor: in_progress, then ok, or
// error when it returns an error or panics (the panic goes on). config may
// be nil.
//
//	err := fixwire.WithMonitor(ctx, "nightly-report", nil, func(ctx context.Context) error { … })
func WithMonitor(ctx context.Context, monitor string, config *MonitorConfig, job func(context.Context) error) (err error) {
	c := HubFromContext(ctx).Client()
	id := c.CaptureCheckIn(CheckIn{Monitor: monitor, Status: CheckInInProgress, Config: config})
	start := time.Now()
	status := CheckInError
	defer func() {
		if id != "" {
			// A run is always measured: where the clock is coarser than the job (Windows), it took
			// less than a tick, not nothing.
			took := max(time.Since(start), time.Nanosecond)
			c.CaptureCheckIn(CheckIn{Monitor: monitor, Status: status, ID: id, Duration: took})
		}
	}()
	err = job(ctx)
	if err == nil {
		status = CheckInOK
	}
	return err
}

// Feedback is what someone said about an error or an AI answer: a message,
// a score from -1 (bad) to 1 (good), or both.
type Feedback struct {
	Message string
	Score   float64
	// TraceID ties it to a trace or an agent run (a negative score opens a
	// user_feedback issue); EventID to an error.
	TraceID string
	EventID string
	Name    string
	Email   string
	URL     string
	// Source is where it came from (api, widget, …); api when empty.
	Source string
}

// CaptureFeedback sends feedback. Its id, or "" when it holds neither a
// message nor a score.
func (h *Hub) CaptureFeedback(f Feedback) string {
	c := h.Client()
	if c == nil || !c.enabled {
		return ""
	}
	defer c.guard()
	f.Message = strings.TrimSpace(f.Message)
	if math.IsNaN(f.Score) || math.IsInf(f.Score, 0) {
		f.Score = 0
	}
	f.Score = max(-1, min(1, f.Score))
	if f.Message == "" && f.Score == 0 {
		return ""
	}
	scope := h.Scope()
	scope.mu.RLock()
	user, span := scope.user, scope.span
	scope.mu.RUnlock()
	if f.TraceID == "" && span != nil {
		f.TraceID = span.TraceID
	}
	if f.Name == "" {
		f.Name = user.Username
	}
	if f.Email == "" {
		f.Email = user.Email
	}
	if f.Source == "" {
		f.Source = "api"
	}
	id := newID(16)
	body := map[string]any{"sdk": sdk(), "feedback_id": id, "timestamp": float64(time.Now().UnixMilli()) / 1000,
		"source": f.Source, "environment": c.opts.Environment}
	for k, v := range map[string]string{"message": f.Message, "trace_id": f.TraceID, "event_id": f.EventID,
		"name": f.Name, "email": f.Email, "url": f.URL, "release": c.opts.Release} {
		if v != "" {
			body[k] = v
		}
	}
	if f.Score != 0 {
		body["score"] = f.Score
	}
	body = c.scrub(body, "sdk", "feedback_id", "timestamp", "event_id", "trace_id", "release", "environment", "source")
	if !c.sendJSON("/v1/feedback", categoryFeedback, body) {
		return ""
	}
	return id
}
