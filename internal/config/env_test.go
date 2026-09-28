package config

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestFromEnvDefaults(t *testing.T) {
	e, err := FromEnv(envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if e.HTTPAddr != "127.0.0.1:9090" || e.LogLevel != "info" || e.LogFormat != "json" || e.ShutdownGrace != 15*time.Second ||
		e.DatabaseURL != "" || e.ConfigPaths != nil || e.CoreBaseURLAllowlist != nil || e.LogRedactExtra != nil ||
		!strings.HasPrefix(e.WorkerID, host+"-") || e.Level() != slog.LevelInfo {
		t.Fatalf("defaults: %+v", e)
	}
}

func TestFromEnvEverything(t *testing.T) {
	e, err := FromEnv(envOf(map[string]string{
		"DATABASE_URL":            "postgres:///aishie",
		"CONFIG":                  " examples/runtime.yaml, examples/agents ,",
		"HTTP_ADDR":               ":9191",
		"CORE_BASE_URL_ALLOWLIST": "https://lms.example.edu, *.example.edu",
		"EGRESS_PROXY":            "http://user:pw@proxy.internal:3128",
		"LOG_REDACT_EXTRA":        `school-[0-9]{4,8}, key=[^,;]+ ,(a|b),`,
		"LOG_LEVEL":               "DEBUG",
		"LOG_FORMAT":              "text",
		"SECRETS_DIR":             "/run/secrets",
		"WORKER_ID":               "w1",
		"SHUTDOWN_GRACE":          "30s",
		"PRICES":                  "/etc/aishie/prices.yaml",
		"KMS_KEY_ID":              "local:/secrets/kek/v1",
		"CORE_BASE_URL":           "https://lms.example.edu/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if e.DatabaseURL != "postgres:///aishie" || strings.Join(e.ConfigPaths, "|") != "examples/runtime.yaml|examples/agents" ||
		e.HTTPAddr != ":9191" || strings.Join(e.CoreBaseURLAllowlist, "|") != "https://lms.example.edu|*.example.edu" ||
		e.EgressProxy != "http://user:pw@proxy.internal:3128" || e.LogLevel != "debug" || e.Level() != slog.LevelDebug ||
		e.LogFormat != "text" || e.SecretsDir != "/run/secrets" || e.WorkerID != "w1" || e.ShutdownGrace != 30*time.Second ||
		e.PricesPath != "/etc/aishie/prices.yaml" || e.KMSKeyID != "local:/secrets/kek/v1" || e.CoreBaseURL != "https://lms.example.edu" {
		t.Fatalf("%+v", e)
	}
	// Commas inside braces and brackets belong to the pattern.
	if strings.Join(e.LogRedactExtra, "|") != `school-[0-9]{4,8}|key=[^,;]+|(a|b)` {
		t.Fatalf("patterns %q", e.LogRedactExtra)
	}
	res, err := e.RedactPatterns()
	if err != nil || len(res) != 3 || !res[0].MatchString("school-12345") {
		t.Fatalf("RedactPatterns: %v %v", res, err)
	}
}

func TestFromEnvRefuses(t *testing.T) {
	for _, tc := range []struct {
		key, value, want string
	}{
		{"HTTP_ADDR", "9090", "HTTP_ADDR"},
		{"HTTP_ADDR", "localhost:http", "HTTP_ADDR"},
		{"CORE_BASE_URL_ALLOWLIST", "https://a.edu,ftp://b.edu", "entry 2"},
		{"EGRESS_PROXY", "proxy.internal:3128", "EGRESS_PROXY"},
		{"EGRESS_PROXY", "ftp://user:hunter2@proxy.internal", "EGRESS_PROXY"},
		{"LOG_REDACT_EXTRA", `ok,secret-(value`, "pattern 2: missing closing )"},
		{"LOG_REDACT_EXTRA", `ok,secret-value|`, "pattern 2 matches empty text"},
		{"LOG_REDACT_EXTRA", `x*`, "pattern 1 matches empty text"},
		{"LOG_LEVEL", "verbose", "LOG_LEVEL"},
		{"LOG_FORMAT", "xml", "LOG_FORMAT"},
		{"SHUTDOWN_GRACE", "15", "SHUTDOWN_GRACE"},
		{"SHUTDOWN_GRACE", "-1s", "SHUTDOWN_GRACE"},
		{"KMS_KEY_ID", "WlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlo=", "KMS_KEY_ID: not local:"},
		{"KMS_KEY_ID", "/secrets/kek/v1", "KMS_KEY_ID: not local:"},
		{"CORE_BASE_URL", "lms.example.edu", "CORE_BASE_URL: must be an absolute URL"},
		{"CORE_BASE_URL", "http://lms.example.edu", "CORE_BASE_URL: must be https"},
		{"CORE_BASE_URL", "https://root:hunter2@lms.example.edu", "CORE_BASE_URL: must hold no user name or password"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			_, err := FromEnv(envOf(map[string]string{tc.key: tc.value}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
			// A proxy's password, or a pattern that spells a secret, is
			// never repeated.
			for _, secret := range []string{"hunter2", "secret-(value", "WlpaWlpa"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("the error repeats %q: %v", secret, err)
				}
			}
		})
	}
	_, err := FromEnv(envOf(map[string]string{"LOG_LEVEL": "loud", "LOG_FORMAT": "xml"}))
	if err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") || !strings.Contains(err.Error(), "LOG_FORMAT") {
		t.Fatalf("every problem at once: %v", err)
	}
}

func TestCoreBaseURLWithinTheAllowlist(t *testing.T) {
	for _, c := range []struct {
		url, allowlist string
		ok             bool
	}{
		{"https://lms.example.edu", "", true},
		{"https://lms.example.edu", "https://lms.example.edu", true},
		{"https://lms.example.edu", "*.example.edu", true},
		{"https://lms.other.edu", "https://lms.example.edu,*.example.edu", false},
		{"http://127.0.0.1:18090", "http://127.0.0.1:18090", true},
	} {
		_, err := FromEnv(envOf(map[string]string{"CORE_BASE_URL": c.url, "CORE_BASE_URL_ALLOWLIST": c.allowlist}))
		if (err == nil) != c.ok || (err != nil && !strings.Contains(err.Error(), "is not within CORE_BASE_URL_ALLOWLIST")) {
			t.Errorf("CORE_BASE_URL=%s within %q: %v", c.url, c.allowlist, err)
		}
	}
}

func TestEnvHelp(t *testing.T) {
	help := EnvHelp()
	for _, v := range []string{"DATABASE_URL", "CONFIG", "HTTP_ADDR", "CORE_BASE_URL_ALLOWLIST", "EGRESS_PROXY", "LOG_REDACT_EXTRA",
		"LOG_LEVEL", "LOG_FORMAT", "SECRETS_DIR", "WORKER_ID", "SHUTDOWN_GRACE", "PRICES", "KMS_KEY_ID", "CORE_BASE_URL"} {
		if !strings.Contains(help, "  "+v+" ") {
			t.Errorf("EnvHelp lacks %s", v)
		}
	}
}

func TestLevels(t *testing.T) {
	for level, want := range map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		if got := (Env{LogLevel: level}).Level(); got != want {
			t.Errorf("%s: %v", level, got)
		}
	}
	if _, err := (Env{LogRedactExtra: []string{"("}}).RedactPatterns(); err == nil {
		t.Fatal("a pattern that does not compile")
	}
}
