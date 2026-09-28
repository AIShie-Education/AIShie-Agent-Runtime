package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Env is the process's settings from its environment (§8.3).
type Env struct {
	// DatabaseURL is the runtime's own PostgreSQL (never Core's); empty
	// keeps state in memory, for one worker and for tests.
	DatabaseURL string
	// ConfigPaths are the files and directories Load reads.
	ConfigPaths []string
	// HTTPAddr is where /healthz, /metrics and /status listen.
	HTTPAddr string
	// CoreBaseURLAllowlist is which Core installations an agent's
	// core.base_url may point at; empty allows any.
	CoreBaseURLAllowlist []string
	// EgressProxy is the proxy every outbound call goes through, when set.
	EgressProxy string
	// LogRedactExtra are patterns redacted from logs beside the built-in
	// ones, each checked to compile.
	LogRedactExtra []string
	// LogLevel is debug, info, warn or error.
	LogLevel string
	// LogFormat is json or text.
	LogFormat string
	// SecretsDir is where secret:// references are looked for first.
	SecretsDir string
	// WorkerID names this process in leases: the host name and process id
	// unless set.
	WorkerID string
	// ShutdownGrace bounds how long answers in progress get on SIGTERM.
	ShutdownGrace time.Duration
	// PricesPath is the price table; when set, it is used instead of
	// runtime.prices_ref.
	PricesPath string
	// KMSKeyID names the key that seals the secrets kept in the store
	// (package vault): local:<dir>/<name>. Empty, no sealed secret is
	// opened or made.
	KMSKeyID string
}

// Defaults of the environment's settings.
const (
	DefaultHTTPAddr      = "127.0.0.1:9090"
	DefaultLogLevel      = "info"
	DefaultLogFormat     = "json"
	DefaultShutdownGrace = 15 * time.Second
)

// envVars are the variables FromEnv reads, with what each is, for EnvHelp.
var envVars = []struct{ name, help string }{
	{"DATABASE_URL", "the runtime's own PostgreSQL, never Core's; unset keeps state in memory (one worker, tests)"},
	{"CONFIG", "comma-separated YAML files and directories (*.yaml, *.yml) of agents and the runtime's settings"},
	{"HTTP_ADDR", "where /healthz, /metrics and /status listen (default " + DefaultHTTPAddr + ")"},
	{"CORE_BASE_URL_ALLOWLIST", "comma-separated Core origins (https://lms.example.edu) or host patterns (*.example.edu) an agent may point at; unset allows any"},
	{"EGRESS_PROXY", "proxy for every outbound call (http://, https://, socks5://)"},
	{"LOG_REDACT_EXTRA", "comma-separated regular expressions redacted from logs beside the built-in token and key shapes"},
	{"LOG_LEVEL", "debug, info, warn or error (default " + DefaultLogLevel + ")"},
	{"LOG_FORMAT", "json or text (default " + DefaultLogFormat + ")"},
	{"SECRETS_DIR", "where secret://a/b is looked for as the file a/b before the variable AISHIE_SECRET_A_B"},
	{"WORKER_ID", "this process's name in leases (default hostname-pid)"},
	{"SHUTDOWN_GRACE", "how long answers in progress get on SIGTERM, such as 15s (default 15s)"},
	{"PRICES", "the price table; overrides the runtime's prices_ref"},
	{"KMS_KEY_ID", "the key that seals the secrets kept in the store: local:<dir>/<name>, a 32-byte key, base64, in that file, the directory's other files kept for secrets an older key sealed"},
}

// EnvHelp lists the environment variables FromEnv reads, for the help
// command.
func EnvHelp() string {
	var b strings.Builder
	for _, v := range envVars {
		fmt.Fprintf(&b, "  %-24s %s\n", v.name, v.help)
	}
	return b.String()
}

// FromEnv reads the settings from getenv (os.Getenv in production),
// applying defaults, and reports every value that is wrong at once. No
// error repeats a value that may hold a credential.
func FromEnv(getenv func(string) string) (Env, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	e := Env{
		DatabaseURL: get("DATABASE_URL"),
		ConfigPaths: splitList(get("CONFIG")),
		HTTPAddr:    or(get("HTTP_ADDR"), DefaultHTTPAddr),
		EgressProxy: get("EGRESS_PROXY"),
		LogLevel:    strings.ToLower(or(get("LOG_LEVEL"), DefaultLogLevel)),
		LogFormat:   strings.ToLower(or(get("LOG_FORMAT"), DefaultLogFormat)),
		SecretsDir:  get("SECRETS_DIR"),
		WorkerID:    or(get("WORKER_ID"), defaultWorkerID()),
		PricesPath:  get("PRICES"),
		KMSKeyID:    get("KMS_KEY_ID"),
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if host, port, err := net.SplitHostPort(e.HTTPAddr); err != nil || port == "" || !isPort(port) || strings.ContainsAny(host, "/ ") {
		bad("HTTP_ADDR: %q is not host:port, such as 127.0.0.1:9090", e.HTTPAddr)
	}
	e.CoreBaseURLAllowlist = splitList(get("CORE_BASE_URL_ALLOWLIST"))
	if _, msgs := parseAllowlist(e.CoreBaseURLAllowlist); len(msgs) > 0 {
		for _, m := range msgs {
			bad("CORE_BASE_URL_ALLOWLIST: %s", m)
		}
	}
	if e.EgressProxy != "" {
		u, err := url.Parse(e.EgressProxy)
		if err != nil || u.Host == "" || !slices.Contains([]string{"http", "https", "socks5", "socks5h"}, u.Scheme) {
			bad("EGRESS_PROXY: not a proxy URL such as http://proxy.internal:3128 (http, https, socks5 or socks5h)")
		}
	}
	e.LogRedactExtra = splitPatterns(getenv("LOG_REDACT_EXTRA"))
	for i, p := range e.LogRedactExtra {
		// The pattern may spell out the very secret it hides: say only
		// what is wrong with it.
		re, err := regexp.Compile(p)
		if err != nil {
			msg := "it does not compile"
			var se *syntax.Error
			if errors.As(err, &se) {
				msg = string(se.Code)
			}
			bad("LOG_REDACT_EXTRA: pattern %d: %s", i+1, msg)
			continue
		}
		if re.MatchString("") {
			// It would match between every two characters of every line.
			bad("LOG_REDACT_EXTRA: pattern %d matches empty text", i+1)
		}
	}
	switch e.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		bad("LOG_LEVEL: %q is not debug, info, warn or error", e.LogLevel)
	}
	if e.LogFormat != "json" && e.LogFormat != "text" {
		bad("LOG_FORMAT: %q is not json or text", e.LogFormat)
	}
	if k := e.KMSKeyID; k != "" && !strings.HasPrefix(k, "local:") && !strings.HasPrefix(k, "awskms:") && !strings.HasPrefix(k, "vault:") {
		// Not repeated: it may be the key itself, pasted where its name
		// belongs.
		bad("KMS_KEY_ID: not local:<dir>/<name>, such as local:/secrets/kek/v1")
	}
	e.ShutdownGrace = DefaultShutdownGrace
	if v := get("SHUTDOWN_GRACE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			bad("SHUTDOWN_GRACE: %q is not a duration such as 15s", v)
		} else {
			e.ShutdownGrace = d
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Env{}, err
	}
	return e, nil
}

// Level is LogLevel as a slog.Level.
func (e Env) Level() slog.Level {
	switch e.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// RedactPatterns compiles LogRedactExtra, for the log handler.
func (e Env) RedactPatterns() ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(e.LogRedactExtra))
	for i, p := range e.LogRedactExtra {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("LOG_REDACT_EXTRA: pattern %d does not compile", i+1)
		}
		out = append(out, re)
	}
	return out, nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// splitList splits a comma-separated list, dropping empty entries.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitPatterns splits comma-separated regular expressions at the commas
// outside brackets, braces and parentheses, and not escaped, so that a
// pattern may hold a{2,5} or [,;].
func splitPatterns(s string) []string {
	var out []string
	depth, start, inClass := 0, 0, false
	flush := func(end int) {
		if p := strings.TrimSpace(s[start:end]); p != "" {
			out = append(out, p)
		}
		start = end + 1
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\':
			i++
		case inClass:
			if c == ']' {
				inClass = false
			}
		case c == '[':
			inClass = true
		case c == '(' || c == '{':
			depth++
		case (c == ')' || c == '}') && depth > 0:
			depth--
		case c == ',' && depth == 0:
			flush(i)
		}
	}
	flush(len(s))
	return out
}

func isPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 0 && n <= 65535
}

func defaultWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	return host + "-" + strconv.Itoa(os.Getpid())
}
