// Package redact masks secrets and personal data on the device, with the
// same detectors, keys and results as the Fixwire server (a port of its
// redact package, kept identical by the shared vectors test).
//
// Detectors are tuned for precision on telemetry: trace ids, UUIDs, hashes
// and timestamps must survive, so checksummed values (cards, IBANs,
// identity numbers) are validated before they are masked.
package redact

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Filtered replaces the value of a sensitive key.
const Filtered = "[Filtered]"

// Finding is one match.
type Finding struct {
	Detector   string
	Start, End int
}

// Options configures a Redactor.
type Options struct {
	// Detectors to run (default DefaultDetectors).
	Detectors []string
	// Patterns are extra regular expressions by name (customer rules).
	Patterns map[string]string
	// SensitiveKeys replace the default JSON key fragments whose values are
	// filtered whole (compared case-insensitively without "-" and "_").
	SensitiveKeys []string
}

// DefaultSensitiveKeys are key fragments whose values are always filtered.
var DefaultSensitiveKeys = []string{
	"password", "passwd", "pwd", "secret", "apikey", "accesskey", "token", "credential", "privatekey",
	"authorization", "cookie", "sessionid", "csrf", "xsrf", "cvv", "cvc", "ssn", "creditcard", "cardnumber",
}

// Redactor masks sensitive values. It is safe for concurrent use.
type Redactor struct {
	detectors []detector
	keys      []string
}

// New returns a redactor, failing on an unknown detector or a bad pattern.
func New(opts Options) (*Redactor, error) {
	names := opts.Detectors
	if names == nil {
		names = DefaultDetectors
	}
	r := &Redactor{keys: DefaultSensitiveKeys}
	if opts.SensitiveKeys != nil {
		r.keys = nil
		for _, k := range opts.SensitiveKeys {
			r.keys = append(r.keys, normalizeKey(k))
		}
	}
	for _, name := range names {
		i := slices.IndexFunc(registry, func(d detector) bool { return d.name == name })
		if i < 0 {
			return nil, fmt.Errorf("redact: unknown detector %q", name)
		}
		r.detectors = append(r.detectors, registry[i])
	}
	custom := make([]string, 0, len(opts.Patterns))
	for name := range opts.Patterns {
		custom = append(custom, name)
	}
	sort.Strings(custom)
	for _, name := range custom {
		re, err := regexp.Compile(opts.Patterns[name])
		if err != nil {
			return nil, fmt.Errorf("redact: pattern %q: %w", name, err)
		}
		r.detectors = append(r.detectors, detector{name: name, re: re})
	}
	return r, nil
}

// Default returns a redactor with the default detectors and keys.
func Default() *Redactor {
	r, err := New(Options{})
	if err != nil {
		panic(err) // the defaults are valid
	}
	return r
}

// Find returns the non-overlapping findings in s, leftmost first; when two
// overlap, the earlier detector in the registry wins.
func (r *Redactor) Find(s string) []Finding {
	var out []Finding // leftmost first, after each detector
	var lower string
	for _, d := range r.detectors {
		if len(d.prefilter) > 0 {
			hay := s
			if !d.caseSensitive {
				if lower == "" {
					lower = strings.ToLower(s)
				}
				hay = lower
			}
			hit := false
			for _, p := range d.prefilter {
				if strings.Contains(hay, p) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		if d.may != nil && !d.may(s) {
			continue
		}
		var found []Finding
		for _, span := range d.spans(s) {
			start, end := span[0], span[1]
			if d.validate != nil && !d.validate(s[start:end]) {
				continue
			}
			if overlaps(out, start, end) || overlaps(found, start, end) {
				continue
			}
			found = append(found, Finding{Detector: d.name, Start: start, End: end})
		}
		if len(found) > 0 {
			out = append(out, found...)
			sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
		}
	}
	return out
}

// spans returns the detector's candidate spans, leftmost first, from its
// scanner or its regular expression (the configured submatch).
func (d detector) spans(s string) [][2]int {
	if d.scan != nil {
		return d.scan(s)
	}
	var out [][2]int
	for _, m := range d.re.FindAllStringSubmatchIndex(s, -1) {
		start, end := m[0], m[1]
		if d.group > 0 && len(m) > 2*d.group+1 && m[2*d.group] >= 0 {
			start, end = m[2*d.group], m[2*d.group+1]
		}
		out = append(out, [2]int{start, end})
	}
	return out
}

// overlaps reports whether [start, end) overlaps one of fs, which are
// leftmost first and apart, so their ends are in order too: a binary
// search, as text can hold many findings.
func overlaps(fs []Finding, start, end int) bool {
	i := sort.Search(len(fs), func(i int) bool { return fs[i].End > start })
	return i < len(fs) && fs[i].Start < end
}

// Mask replaces each finding with [REDACTED:<detector>].
func (r *Redactor) Mask(s string) (string, []Finding) {
	fs := r.Find(s)
	if len(fs) == 0 {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, f := range fs {
		b.WriteString(s[last:f.Start])
		b.WriteString("[REDACTED:" + f.Detector + "]")
		last = f.End
	}
	b.WriteString(s[last:])
	return b.String(), fs
}

func normalizeKey(k string) string {
	return strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(k))
}

// sensitive reports whether a JSON key's value must be filtered whole.
func (r *Redactor) sensitive(key string) bool {
	k := normalizeKey(key)
	if k == "auth" {
		return true
	}
	for _, frag := range r.keys {
		if strings.Contains(k, frag) && (frag != "token" || !tokenCount(k)) {
			return true
		}
	}
	return false
}

// tokenCount reports keys that count LLM tokens rather than hold one:
// gen_ai.usage.input_tokens, max_tokens, token_count.
func tokenCount(k string) bool {
	return strings.HasSuffix(k, "tokens") || strings.Contains(k, "tokencount") || strings.Contains(k, "usage")
}

// Walk masks every string in v (maps, slices and strings as JSON decodes
// them) and filters the values of sensitive keys, in place. It returns v
// and the number of values masked.
func (r *Redactor) Walk(v any) (any, int) {
	n := 0
	v = r.walk(v, &n)
	return v, n
}

func (r *Redactor) walk(v any, n *int) any {
	switch x := v.(type) {
	case map[string]any:
		var renamed []string
		for k, val := range x {
			if masked, _ := r.Mask(k); masked != k {
				renamed = append(renamed, k)
			}
			if r.sensitive(k) && !empty(val) {
				// A typed attribute ({"type": …, "value": …}) keeps its shape.
				if typed, ok := val.(map[string]any); ok && typed["value"] != nil {
					if typed["value"] != Filtered {
						typed["value"], typed["type"] = Filtered, "string"
						*n++
					}
					continue
				}
				if s, ok := val.(string); !ok || s != Filtered {
					x[k] = Filtered
					*n++
				}
				continue
			}
			x[k] = r.walk(val, n)
		}
		// Keys hold data too ({"ada@example.com": 3}). Keys that mask alike
		// are numbered in key order: "[REDACTED:email] (2)".
		sort.Strings(renamed)
		next := map[string]int{} // masked key → the number to try next
		for _, k := range renamed {
			masked, fs := r.Mask(k)
			key := masked
			for i := max(next[masked], 2); ; i++ {
				if _, taken := x[key]; !taken {
					next[masked] = i
					break
				}
				key = masked + " (" + strconv.Itoa(i) + ")"
			}
			x[key] = x[k]
			delete(x, k)
			*n += len(fs)
		}
		return x
	case []any:
		// Some SDKs send maps as [key, value] pairs (headers, tags).
		if len(x) == 2 {
			if k, ok := x[0].(string); ok && r.sensitive(k) && !empty(x[1]) {
				x[1] = Filtered
				*n++
				return x
			}
		}
		for i := range x {
			x[i] = r.walk(x[i], n)
		}
		return x
	case string:
		masked, fs := r.Mask(x)
		*n += len(fs)
		return masked
	}
	return v
}

func empty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	}
	return false
}
