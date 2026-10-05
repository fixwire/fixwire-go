package fixwire

import (
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client budgets: a crash loop costs a few events and a count, not the
// quota. Each issue (a cheap fingerprint of the event) may send a burst,
// then so many a minute, within a budget for all of them; occurrences held
// back are counted and ride on the issue's next event (fixwire.suppressed),
// so issue counts stay right. The server's grouping is the real one; the
// fingerprint only drives the budgets.

// ErrorBudget bounds the errors and messages sent.
type ErrorBudget struct {
	// PerIssueBurst events of one issue are sent at once (default 10),
	// then PerIssuePerMinute (default 1).
	PerIssueBurst     int
	PerIssuePerMinute float64
	// PerMinute bounds events across issues (default 600).
	PerMinute float64
	// Disabled sends every event.
	Disabled bool
}

func (b ErrorBudget) withDefaults() ErrorBudget {
	if b.PerIssueBurst <= 0 {
		b.PerIssueBurst = 10
	}
	if b.PerIssuePerMinute <= 0 {
		b.PerIssuePerMinute = 1
	}
	if b.PerMinute <= 0 {
		b.PerMinute = 600
	}
	return b
}

// maxIssues bounds the issues the budget remembers (least recently seen
// go first); topFrames is how many in-app frames name an issue.
const (
	maxIssues = 1024
	topFrames = 5
)

type bucket struct {
	tokens     float64
	updated    time.Time
	suppressed int
	seen       time.Time
}

// take refills the bucket at perMinute up to burst and takes a token.
func (b *bucket) take(burst, perMinute float64, now time.Time) bool {
	b.tokens = min(burst, b.tokens+now.Sub(b.updated).Minutes()*perMinute)
	b.updated = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

type budget struct {
	opts   ErrorBudget
	mu     sync.Mutex
	issues map[string]*bucket
	all    bucket
}

func newBudget(opts ErrorBudget) *budget {
	opts = opts.withDefaults()
	return &budget{opts: opts, issues: map[string]*bucket{}, all: bucket{tokens: opts.PerMinute, updated: time.Now()}}
}

// allow reports whether an event of the issue may be sent, and the
// occurrences held back since the last one sent.
func (b *budget) allow(issue string, now time.Time) (ok bool, suppressed int) {
	if b.opts.Disabled {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	bk := b.issues[issue]
	if bk == nil {
		if len(b.issues) >= maxIssues {
			b.forgetOldest()
		}
		bk = &bucket{tokens: float64(b.opts.PerIssueBurst), updated: now}
		b.issues[issue] = bk
	}
	bk.seen = now
	if bk.take(float64(b.opts.PerIssueBurst), b.opts.PerIssuePerMinute, now) && b.all.take(b.opts.PerMinute, b.opts.PerMinute, now) {
		suppressed, bk.suppressed = bk.suppressed, 0
		return true, suppressed
	}
	bk.suppressed++
	return false, 0
}

func (b *budget) forgetOldest() {
	var oldest string
	var at time.Time
	for k, v := range b.issues {
		if oldest == "" || v.seen.Before(at) {
			oldest, at = k, v.seen
		}
	}
	delete(b.issues, oldest)
}

// variable are the parts of a message that change between occurrences:
// hex, UUIDs, long hex ids, numbers and emails.
var variable = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b|\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b|` +
	`\b[0-9a-fA-F]{16,}\b|\d+(?:\.\d+)?|\S+@\S+\.\w+`)

// issueOf is the event's fingerprint for the budgets: its exception types
// and top in-app frames (or its message without the parts that vary), and
// its custom fingerprint.
func issueOf(e *Event) string {
	var parts []string
	if len(e.Exceptions) > 0 {
		for _, x := range e.Exceptions {
			parts = append(parts, x.Type)
		}
		frames := e.Exceptions[0].Frames
		var app []Frame
		for _, f := range frames {
			if f.InApp {
				app = append(app, f)
			}
		}
		if len(app) == 0 {
			app = frames
		}
		for _, f := range app[max(0, len(app)-topFrames):] {
			parts = append(parts, f.Module+"|"+f.Function)
		}
		if len(frames) == 0 {
			parts = append(parts, variable.ReplaceAllString(e.Exceptions[0].Message, "<*>"))
		}
	} else {
		parts = append(parts, variable.ReplaceAllString(e.Message, "<*>"))
	}
	if len(e.Fingerprint) > 0 {
		parts = append(parts, strings.Join(e.Fingerprint, "\x1f"))
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.Join(parts, "\x1e")))
	return strconv.FormatUint(h.Sum64(), 16)
}
