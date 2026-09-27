// Package redact keeps tokens and keys out of logs and every other text the
// runtime shows (Core's docs/agent-runtime.md §6.1, §8.3): Core's tokens and
// invitations, the providers' key shapes, credentials in headers and URLs,
// and whatever the operator adds with LOG_REDACT_EXTRA.
//
// Redaction is a net under the rule that secrets are never logged in the
// first place, not a licence to log them: it finds what has a recognisable
// shape, and a key without one passes through.
package redact

import (
	"regexp"
	"strings"
)

// Placeholder replaces everything redacted.
const Placeholder = "[redacted]"

// A rule replaces what its pattern matches. With keep > 0, the match up to
// the end of submatch keep stays, and so does submatch tail when tail > 0;
// what lies between becomes Placeholder. With keep 0 the whole match does.
type rule struct {
	re   *regexp.Regexp
	keep int
	tail int
}

// builtin are the shapes redacted everywhere, in order: credentials in a URL
// and in header-like "name: value" pairs first, so that the value goes whole
// whatever it looks like, then bearer tokens, then the token and key shapes
// of §6.1.
var builtin = []rule{
	// scheme://user:password@host: the password.
	{re: regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^/\s:@\[\]]*:)[^/\s@\[\]]+(@)`), keep: 1, tail: 2},
	// Authorization: <scheme> <credentials>, as a header, in JSON, and in
	// a printed http.Header (map[Authorization:[Basic …]]) or its JSON
	// ({"Authorization":["Basic …"]}).
	{re: regexp.MustCompile(`(?i)\b((?:proxy-)?authorization["']?\s*[:=]\s*["']?)` + headerValue(`(?:[ \t]+[^\s"',;\[\]]+)?`)), keep: 1},
	// Headers and fields whose value is a key: x-api-key, api-key,
	// x-goog-api-key, api_key, and AWS's secret access key and session
	// token (X-Amz-Security-Token).
	{re: regexp.MustCompile(`(?i)\b((?:(?:x-goog-|x-)?api[-_]?key|x-amz-security-token|(?:aws[-_]?)?secret[-_]?access[-_]?key|(?:aws[-_]?)?session[-_]?token)["']?\s*[:=]\s*["']?)` + headerValue("")), keep: 1},
	// Bearer <token>.
	{re: regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]+`), keep: 1},
	// Core's API tokens and invitations.
	{re: regexp.MustCompile(`aisinv_[A-Za-z0-9_-]+`)},
	{re: regexp.MustCompile(`ais_[A-Za-z0-9_-]+`)},
	// OpenAI, Anthropic, DeepSeek and most compatible providers' keys
	// (sk-, sk-proj-, sk-ant-, …). The letter or digit before is excluded
	// so that words such as "task-management" stay as written; a key long
	// or marked enough to be one goes wherever it is (their keys have 32
	// or more characters after "sk-").
	{re: regexp.MustCompile(`(^|[^A-Za-z0-9])sk-[A-Za-z0-9_-]{8,}`), keep: 1},
	{re: regexp.MustCompile(`sk-(?:ant|proj|or|svcacct|admin)-[A-Za-z0-9_-]{8,}|sk-[A-Za-z0-9_-]{32,}`)},
	// Google API keys.
	{re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{20,}`)},
	// AWS access key ids, long-term and temporary.
	{re: regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`)},
}

// headerValue matches a header's value after its name: in a JSON list
// (["v"]), or a word in optional brackets ([v], as a printed http.Header
// has it), with more after it as more says.
func headerValue(more string) string {
	return `(?:\[\s*["'][^"'\]\n]*["']\s*\]|\[?[^\s"',;&\[\]]+` + more + `\]?)`
}

// Redactor removes secrets from text: the built-in shapes and its extra
// patterns. It is safe for concurrent use.
type Redactor struct {
	rules []rule
}

// New returns a Redactor for the built-in shapes and extra, whose every
// match is replaced whole. Nil patterns are ignored.
func New(extra []*regexp.Regexp) *Redactor {
	r := &Redactor{rules: append([]rule(nil), builtin...)}
	for _, re := range extra {
		if re != nil {
			r.rules = append(r.rules, rule{re: re})
		}
	}
	return r
}

var defaultRedactor = New(nil)

// String is s with the built-in shapes redacted, for the places that are
// not logs: the status page, errors shown to an owner.
func String(s string) string { return defaultRedactor.String(s) }

// String is s with every secret the Redactor knows redacted.
func (r *Redactor) String(s string) string {
	for _, ru := range r.rules {
		s = ru.apply(s)
	}
	return s
}

// Contains reports whether s holds anything the Redactor would redact.
func (r *Redactor) Contains(s string) bool { return r.String(s) != s }

func (ru rule) apply(s string) string {
	if ru.keep == 0 {
		return ru.re.ReplaceAllLiteralString(s, Placeholder)
	}
	matches := ru.re.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		// The kept prefix ends where submatch keep does, and the kept
		// suffix begins where submatch tail does; between them is the
		// secret.
		keepEnd := m[2*ru.keep+1]
		if keepEnd < 0 {
			keepEnd = m[0]
		}
		tailStart := m[1]
		if ru.tail > 0 && m[2*ru.tail] >= 0 {
			tailStart = m[2*ru.tail]
		}
		b.WriteString(s[last:keepEnd])
		b.WriteString(Placeholder)
		b.WriteString(s[tailStart:m[1]])
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}
