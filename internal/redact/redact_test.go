package redact

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Text full of findings costs about as much per finding as text with one:
// overlap checks are binary searches, not scans of what was found.
func TestManyFindings(t *testing.T) {
	// Four times the findings take about four times as long, not sixteen:
	// compared with each other, so a slow machine (or -race) can't fail it.
	mask := func(n int) (string, []Finding, time.Duration) {
		s := strings.Repeat("a@b.cc password= ", n)
		start := time.Now()
		masked, fs := Default().Mask(s)
		return masked, fs, time.Since(start)
	}
	_, _, small := mask(15000)
	masked, fs, large := mask(60000) // 1 MB
	if large > 8*small+100*time.Millisecond {
		t.Errorf("4x the findings took %s against %s: not linear", large, small)
	}
	if len(fs) < 60000 || strings.Contains(masked, "a@b.cc") {
		t.Errorf("%d findings", len(fs))
	}
	for i := 1; i < len(fs); i++ {
		if fs[i].Start < fs[i-1].End {
			t.Fatalf("findings %v and %v overlap", fs[i-1], fs[i])
		}
	}
}

// Text built to make a secret's name match everywhere costs no more than
// other text: RE2 runs the pattern in one pass.
func TestHostileSecretNames(t *testing.T) {
	r := Default()
	for name, s := range map[string]string{
		"spaces after a name":     "token" + strings.Repeat(" ", 100000),
		"names, nothing assigned": strings.Repeat("sessid", 20000),
		"codes, nothing assigned": strings.Repeat("?code", 20000),
		"separators":              strings.Repeat("secret_key:= '", 10000),
		"short values":            strings.Repeat("password=abc ", 10000),
	} {
		start := time.Now()
		r.Mask(s)
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("%s: %d bytes took %s", name, len(s), took)
		}
	}
}

// Keys that mask alike are numbered in key order, each found in one step.
func TestManyKeysMaskingAlike(t *testing.T) {
	walk := func(n int) (any, int, time.Duration) {
		doc := map[string]any{}
		for i := range n {
			doc[fmt.Sprintf("user%05d@example.com", i)] = i
		}
		start := time.Now()
		out, masked := Default().Walk(doc)
		return out, masked, time.Since(start)
	}
	// Compared with a quarter as many keys, as in TestManyFindings.
	const n = 20000
	_, _, small := walk(n / 4)
	out, masked, large := walk(n)
	if large > 8*small+100*time.Millisecond {
		t.Errorf("renaming 4x the keys took %s against %s: not linear", large, small)
	}
	m := out.(map[string]any)
	if masked != n || len(m) != n || m["[REDACTED:email]"] != 0 || m["[REDACTED:email] (2)"] != 1 ||
		m["[REDACTED:email] ("+strconv.Itoa(n)+")"] != n-1 {
		t.Errorf("%d masked, %d keys", masked, len(m))
	}
}
