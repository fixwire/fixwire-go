package redact

import (
	"math/big"
	"regexp"
	"strings"
)

// detector finds one kind of sensitive value. A cheap literal prefilter
// skips the regular expression on text that cannot match, and validate
// rejects look-alikes (Luhn, mod-97, checksums), so precision stays high on
// telemetry full of ids, hashes and timestamps.
type detector struct {
	name string
	// prefilter lists substrings one of which must appear (any case, unless
	// caseSensitive); empty means always run.
	prefilter     []string
	caseSensitive bool
	re            *regexp.Regexp
	// group is the submatch to mask (0: the whole match).
	group    int
	validate func(string) bool
	// scan replaces re with a hand-written scanner returning spans.
	scan func(string) [][2]int
	// may is a cheaper prefilter than literals, when there are none.
	may func(string) bool
}

// Detector names.
const (
	AWSAccessKey     = "aws_access_key"
	GCPAPIKey        = "gcp_api_key"
	AzureKey         = "azure_storage_key"
	GitHubToken      = "github_token"
	StripeKey        = "stripe_key"
	SlackToken       = "slack_token"
	SlackWebhook     = "slack_webhook"
	OpenAIKey        = "openai_key"
	AnthropicKey     = "anthropic_key"
	JWT              = "jwt"
	PrivateKey       = "private_key"
	FixwireSecretKey = "fixwire_secret_key"
	URLCredentials   = "url_credentials"
	HTTPAuth         = "http_auth"
	SecretAssigned   = "secret_assignment"
	Email            = "email"
	Phone            = "phone"
	CreditCard       = "credit_card"
	IBAN             = "iban"
	USSSN            = "us_ssn"
	TRTCKN           = "tr_tckn"
	IPv4             = "ipv4"
)

var registry = []detector{
	{name: PrivateKey, prefilter: []string{"PRIVATE KEY-----"}, caseSensitive: true,
		re: regexp.MustCompile(`-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----[\s\S]*?-----END (?:[A-Z ]+ )?PRIVATE KEY-----`)},
	{name: AWSAccessKey, prefilter: []string{"AKIA", "ASIA", "ABIA", "ACCA"}, caseSensitive: true,
		re: regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`)},
	{name: GCPAPIKey, prefilter: []string{"AIza"}, caseSensitive: true,
		re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}`)},
	{name: AzureKey, prefilter: []string{"accountkey="},
		re: regexp.MustCompile(`(?i)AccountKey=([A-Za-z0-9+/]{86}==)`), group: 1},
	{name: GitHubToken, prefilter: []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}, caseSensitive: true,
		re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{60,255})\b`)},
	{name: StripeKey, prefilter: []string{"sk_live_", "sk_test_", "rk_live_", "rk_test_", "whsec_"}, caseSensitive: true,
		re: regexp.MustCompile(`\b(?:(?:sk|rk)_(?:live|test)_[0-9A-Za-z]{16,247}|whsec_[A-Za-z0-9+/=]{24,})`)},
	{name: SlackToken, prefilter: []string{"xox"}, caseSensitive: true,
		re: regexp.MustCompile(`\bxox[abposr]-[0-9A-Za-z-]{10,250}\b`)},
	{name: SlackWebhook, prefilter: []string{"hooks.slack.com/services/"}, caseSensitive: true,
		re: regexp.MustCompile(`https://hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+`)},
	{name: AnthropicKey, prefilter: []string{"sk-ant-"}, caseSensitive: true,
		re: regexp.MustCompile(`\bsk-ant-(?:api|admin)\d{2}-[A-Za-z0-9_\-]{80,}`)},
	{name: OpenAIKey, prefilter: []string{"sk-"}, caseSensitive: true,
		re: regexp.MustCompile(`\bsk-(?:(?:proj|svcacct|admin)-[A-Za-z0-9_\-]{40,}|[A-Za-z0-9]{20}T3BlbkFJ[A-Za-z0-9]{20})`)},
	{name: JWT, prefilter: []string{"eyJ"}, caseSensitive: true,
		re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{name: FixwireSecretKey, prefilter: []string{"_sk_live_", "_sk_test_"}, caseSensitive: true,
		re: regexp.MustCompile(`\b[a-z]{2,4}_sk_(?:live|test)_[0-9A-Za-z]{38}\b`)},
	// The password in scheme://user:password@host (the user stays).
	{name: URLCredentials, prefilter: []string{"://"}, caseSensitive: true,
		re: regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.\-]*://[^\s/?#@:]*:([^\s/?#@]+)@`), group: 1, validate: unmasked},
	// Bearer and Basic credentials outside a header (messages, breadcrumbs).
	{name: HTTPAuth, prefilter: []string{"bearer", "basic"},
		re: regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+([A-Za-z0-9._~+/\-]{12,}=*)`), group: 1, validate: credentialLike},
	// A value given to a secret's name, in text, config and URLs. The name may
	// end a longer one (access_token, client_secret, csrfToken, PHPSESSID,
	// X-Amz-Signature); an OAuth code counts in a query or fragment only.
	{name: SecretAssigned, prefilter: []string{"pass", "pwd", "secret", "key", "token", "credential", "sess", "sig", "code"},
		re: regexp.MustCompile(`(?i)(?:password|passwd|pwd|secret(?:[_-]?key)?|private[_-]?key|token|api[_-]?key|access[_-]?key|credentials?|sess(?:ion)?[_-]?id|sig(?:nature)?|[?&#]code)["']?\s*[:=]\s*["']?([^\s"',;&]{6,})`), group: 1,
		validate: unmasked},
	{name: Email, prefilter: []string{"@"}, caseSensitive: true, scan: emailSpans},
	{name: CreditCard, scan: cardSpans},
	{name: IBAN, may: mayHoldIBAN, re: regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,3})?\b`), validate: validIBAN},
	{name: USSSN, prefilter: []string{"-"}, caseSensitive: true, scan: ssnSpans},
	{name: TRTCKN, scan: tcknSpans},
	{name: Phone, prefilter: []string{"+"}, caseSensitive: true,
		re: regexp.MustCompile(`\+\d(?:[ .\-()]?\d){7,14}\b`), validate: validPhone},
	{name: IPv4, prefilter: []string{"."}, caseSensitive: true,
		re: regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\b`)},
}

// DefaultDetectors are on unless a caller chooses otherwise. IP addresses
// are left out: in error messages they are usually servers worth seeing.
var DefaultDetectors = func() []string {
	var out []string
	for _, d := range registry {
		if d.name != IPv4 {
			out = append(out, d.name)
		}
	}
	return out
}()

// unmasked rejects values a scrubber already replaced.
func unmasked(v string) bool { return !strings.HasPrefix(v, "[REDACTED") && v != "[Filtered]" }

// credentialLike tells a token from a word after "basic": it has a digit,
// a base64 symbol, or capitals past its first letter ("dXNlcjpwYXNz", but
// not "Authentication").
func credentialLike(v string) bool {
	if strings.ContainsAny(v, "0123456789+/=") {
		return true
	}
	upper, lower := false, false
	for i := 1; i < len(v); i++ {
		upper = upper || (v[i] >= 'A' && v[i] <= 'Z')
		lower = lower || (v[i] >= 'a' && v[i] <= 'z')
	}
	return upper && lower
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// validCard checks the length, a known issuer prefix and the Luhn sum.
func validCard(s string) bool {
	d := digits(s)
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	known := false
	for _, p := range []string{"4", "51", "52", "53", "54", "55", "2221", "2720", "34", "37", "6011", "65", "35", "36", "38", "300", "305", "62"} {
		if strings.HasPrefix(d, p) {
			known = true
			break
		}
	}
	if !known {
		return false
	}
	sum, double := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if double {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0
}

// validIBAN checks the length (15–34) and the mod-97 checksum.
func validIBAN(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	rearranged := s[4:] + s[:4]
	var b strings.Builder
	for _, r := range rearranged {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteString(itoa(int(r-'A') + 10))
		default:
			return false
		}
	}
	n, ok := new(big.Int).SetString(b.String(), 10)
	return ok && new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}

// validSSN rejects numbers the US never issues.
func validSSN(s string) bool {
	area, group, serial := s[0:3], s[4:6], s[7:11]
	return area != "000" && area != "666" && area[0] != '9' && group != "00" && serial != "0000"
}

// validTCKN checks the Turkish identity number's two check digits.
func validTCKN(s string) bool {
	if len(s) != 11 || s[0] == '0' {
		return false
	}
	d := make([]int, 11)
	for i := range s {
		d[i] = int(s[i] - '0')
	}
	odd := d[0] + d[2] + d[4] + d[6] + d[8]
	even := d[1] + d[3] + d[5] + d[7]
	if ((odd*7-even)%10+10)%10 != d[9] {
		return false
	}
	sum := 0
	for i := range 10 {
		sum += d[i]
	}
	return sum%10 == d[10]
}

// validPhone wants an international number of 8 to 15 digits.
func validPhone(s string) bool {
	n := len(digits(s))
	return n >= 8 && n <= 15
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// Hand-written scanners for the detectors whose regular expressions would
// otherwise try every position of digit-heavy text.

func isWord(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// numberSpans returns runs of digits, optionally split by single spaces or
// dashes, that stand alone as words. Each is reported once with its digit
// count and separator, for the card, SSN and TCKN detectors.
type numberSpan struct {
	start, end int
	digits     int
	sep        byte // 0 when unbroken
	groups     []int
}

func numberSpans(s string) []numberSpan {
	var out []numberSpan
	for i := 0; i < len(s); {
		if !isDigit(s[i]) || (i > 0 && isWord(s[i-1])) {
			i++
			continue
		}
		span := numberSpan{start: i}
		group := 0
		j := i
		for j < len(s) {
			c := s[j]
			if isDigit(c) {
				span.digits++
				group++
				j++
				continue
			}
			if (c == ' ' || c == '-') && j+1 < len(s) && isDigit(s[j+1]) && (span.sep == 0 || span.sep == c) {
				span.sep = c
				span.groups = append(span.groups, group)
				group = 0
				j++
				continue
			}
			break
		}
		span.groups = append(span.groups, group)
		span.end = j
		if j == len(s) || !isWord(s[j]) {
			out = append(out, span)
		}
		i = j + 1
	}
	return out
}

func cardSpans(s string) [][2]int {
	var out [][2]int
	for _, n := range numberSpans(s) {
		if n.digits >= 13 && n.digits <= 19 && validCard(s[n.start:n.end]) {
			out = append(out, [2]int{n.start, n.end})
		}
	}
	return out
}

func ssnSpans(s string) [][2]int {
	var out [][2]int
	for _, n := range numberSpans(s) {
		if n.sep == '-' && len(n.groups) == 3 && n.groups[0] == 3 && n.groups[1] == 2 && n.groups[2] == 4 && validSSN(s[n.start:n.end]) {
			out = append(out, [2]int{n.start, n.end})
		}
	}
	return out
}

func tcknSpans(s string) [][2]int {
	var out [][2]int
	for _, n := range numberSpans(s) {
		if n.sep == 0 && n.digits == 11 && validTCKN(s[n.start:n.end]) {
			out = append(out, [2]int{n.start, n.end})
		}
	}
	return out
}

// emailSpans grows outwards from each "@" over the characters an address
// may hold, and keeps it if the domain ends in a dotted, alphabetic TLD.
func emailSpans(s string) [][2]int {
	var out [][2]int
	local := func(c byte) bool { return isWord(c) || c == '.' || c == '%' || c == '+' || c == '-' }
	domain := func(c byte) bool { return isWord(c) && c != '_' || c == '.' || c == '-' }
	for i := strings.IndexByte(s, '@'); i >= 0; {
		start, end := i, i+1
		for start > 0 && local(s[start-1]) {
			start--
		}
		for end < len(s) && domain(s[end]) {
			end++
		}
		for end > i+1 && (s[end-1] == '.' || s[end-1] == '-') {
			end--
		}
		dom := s[i+1 : end]
		dot := strings.LastIndexByte(dom, '.')
		if start < i && dot > 0 {
			tld := dom[dot+1:]
			ok := len(tld) >= 2 && len(tld) <= 24
			for k := 0; k < len(tld) && ok; k++ {
				ok = tld[k] >= 'a' && tld[k] <= 'z' || tld[k] >= 'A' && tld[k] <= 'Z'
			}
			for start < i && (s[start] == '.' || s[start] == '-') {
				start++
			}
			if ok && start < i {
				out = append(out, [2]int{start, end})
			}
		}
		next := strings.IndexByte(s[i+1:], '@')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return out
}

// mayHoldIBAN reports whether two capitals and two digits start a word.
func mayHoldIBAN(s string) bool {
	for i := 0; i+4 <= len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' && s[i+1] >= 'A' && s[i+1] <= 'Z' && isDigit(s[i+2]) && isDigit(s[i+3]) && (i == 0 || !isWord(s[i-1])) {
			return true
		}
	}
	return false
}
