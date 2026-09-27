package core

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetryAfterHeader(t *testing.T) {
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"7", 7 * time.Second},
		{" 2 ", 2 * time.Second},
		{"0.5", 500 * time.Millisecond},
		{"0", 0},
		{"-3", 0},
		{"NaN", 0},
		{"Inf", maxRetryAfter},
		{"999999", maxRetryAfter},
		{"soon", 0},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{now.Add(48 * time.Hour).Format(http.TimeFormat), maxRetryAfter},
	} {
		if got := retryAfterHeader(tc.in, now); got != tc.want {
			t.Errorf("Retry-After %q: %s, want %s", tc.in, got, tc.want)
		}
	}
}

// A token is redacted before what holds it is cut: a cut through the token
// would otherwise leave its start to be read.
func TestSaidRedactsBeforeItCuts(t *testing.T) {
	const token = "SecretTokenOfNoKnownShape0123456789"
	for _, pad := range []int{0, 150, 190, 195, 199, 250} {
		body := strings.Repeat("x", pad) + " " + token + " ais_OtherTokenOfCores tail"
		got := said([]byte(body), token)
		if strings.Contains(got, token[:5]) || strings.Contains(got, "ais_") || len(got) > 200+len("…") {
			t.Errorf("pad %d: %q", pad, got)
		}
	}
	if got := said(nil, token); got != "no body" {
		t.Errorf("no body: %q", got)
	}
	if got := quote("a\n\tb  c", "", 40); got != "a b c" {
		t.Errorf("quote: %q", got)
	}
}

func TestWithoutRedirects(t *testing.T) {
	own := &http.Client{Timeout: 3 * time.Second}
	c := withoutRedirects(own)
	if c == own || c.Timeout != own.Timeout || c.CheckRedirect == nil || own.CheckRedirect != nil {
		t.Fatalf("got %+v from %+v", c, own)
	}
	if d := withoutRedirects(nil); d.Timeout != DefaultTimeout || d.CheckRedirect == nil {
		t.Fatalf("the default: %+v", d)
	}
}
