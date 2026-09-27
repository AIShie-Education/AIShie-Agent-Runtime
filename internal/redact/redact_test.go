package redact

import (
	"regexp"
	"strings"
	"testing"
)

// Tokens shaped as the real ones are. None of them is live.
const (
	coreToken   = "ais_k7v2m4qhx3ab_9Jx2abcDEFghiJKLmnoPQRstuVWXyz0123456789_-abcd"
	inviteToken = "aisinv_q2w3e4r5t6y7_ZZxxYYwwVVuuTTssRRqqPPooNNmmLLkkJJiiHHggFFe"
	openaiKey   = "sk-proj-Ab3dEf6hIj9kLm2nOp5qRs8tUv1wXy4z"
	anthropic   = "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCd"
	googleKey   = "AIzaSyA1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q"
	awsKey      = "AKIAIOSFODNN7EXAMPLE"
	awsTempKey  = "ASIAY34FZKBOKMUTVV7A"
)

func TestString(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"core token", "token " + coreToken + " refused", "token [redacted] refused"},
		{"invitation", "invite=" + inviteToken, "invite=[redacted]"},
		{"openai key", "key " + openaiKey + ".", "key [redacted]."},
		{"anthropic key in JSON", `{"key":"` + anthropic + `"}`, `{"key":"[redacted]"}`},
		{"google key in a query", "https://generativelanguage.googleapis.com/v1beta/models?key=" + googleKey, "https://generativelanguage.googleapis.com/v1beta/models?key=[redacted]"},
		{"aws access key", "id " + awsKey, "id [redacted]"},
		{"aws temporary key", awsTempKey + " expired", "[redacted] expired"},
		{"bearer", "Authorization failed for Bearer abc.def-ghi_jkl", "Authorization failed for Bearer [redacted]"},
		{"lower-case bearer", "bearer xyz123", "bearer [redacted]"},
		{"authorization header", "Authorization: Basic dXNlcjpwYXNz", "Authorization: [redacted]"},
		{"authorization header with bearer", "Authorization: Bearer opaque-token-value", "Authorization: [redacted]"},
		{"authorization in JSON", `{"Authorization":"Bearer x"}`, `{"Authorization":"[redacted]"}`},
		{"printed http.Header", "map[Authorization:[Bearer zz] X-Api-Key:[azure0123456789]]", "map[Authorization:[redacted] X-Api-Key:[redacted]]"},
		{"x-api-key header", "x-api-key: 0123456789abcdef", "x-api-key: [redacted]"},
		{"api-key header", "api-key:azurekeyvalue", "api-key:[redacted]"},
		{"x-goog-api-key header", "X-Goog-Api-Key: something", "X-Goog-Api-Key: [redacted]"},
		{"api_key field", `{"api_key": "plain"}`, `{"api_key": "[redacted]"}`},
		{"url password", "proxy http://user:hunter2@proxy.internal:3128 down", "proxy http://user:[redacted]@proxy.internal:3128 down"},
		{"words that only look alike", "task-management and risk-assessment desk-lamp", "task-management and risk-assessment desk-lamp"},
		{"short sk- is not a key", "sk-short", "sk-short"},
		{"key after underscore", "OPENAI_sk-abcdefghijk", "OPENAI_[redacted]"},
		{"two keys", openaiKey + "," + anthropic, "[redacted],[redacted]"},
		{"nothing to redact", "conversation 0192 answered in 3.2s", "conversation 0192 answered in 3.2s"},
		{"placeholder stays", "Bearer [redacted] api-key: [redacted] http://u:[redacted]@h", "Bearer [redacted] api-key: [redacted] http://u:[redacted]@h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := String(tc.in); got != tc.want {
				t.Fatalf("String(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			if again := String(tc.want); again != tc.want {
				t.Fatalf("not idempotent: %q became %q", tc.want, again)
			}
		})
	}
}

func TestExtraPatterns(t *testing.T) {
	r := New([]*regexp.Regexp{regexp.MustCompile(`tenant-secret-[0-9]+`), nil})
	got := r.String("value tenant-secret-42 and " + coreToken)
	if got != "value [redacted] and [redacted]" {
		t.Fatalf("got %q", got)
	}
	if String("tenant-secret-42") != "tenant-secret-42" {
		t.Fatal("the default redactor has no extra patterns")
	}
	if !r.Contains("tenant-secret-1") || r.Contains("tenant-public") {
		t.Fatal("Contains")
	}
}

// secretParts are the pieces of each token that must never be seen: the
// whole, and the secret half after its public prefix.
func secretParts() []string {
	var parts []string
	for _, tok := range []string{coreToken, inviteToken, openaiKey, anthropic, googleKey, awsKey} {
		parts = append(parts, tok, tok[len(tok)/2:])
	}
	return parts
}

func assertClean(t *testing.T, out string) {
	t.Helper()
	for _, p := range secretParts() {
		if strings.Contains(out, p) {
			t.Fatalf("%q survived in:\n%s", p, out)
		}
	}
}
