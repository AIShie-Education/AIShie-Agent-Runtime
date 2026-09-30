package config

import (
	"encoding/base64"
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

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
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
	// CoreBaseURL is the Core hosted agents connect to (docs/design.md
	// §11.2), within CoreBaseURLAllowlist when that is set. Empty, no
	// hosted agent runs.
	CoreBaseURL string
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
	// APIAddr is where the runtime's JSON API listens (docs/design.md
	// §11.4), apart from HTTPAddr, so that /status is never reachable
	// through it; empty, there is no API.
	APIAddr string
	// APIAudience is the audience Core's assertions must name for this
	// runtime, byte for byte: one of Core's RUNTIME_AUDIENCES. Required
	// with APIAddr, as CoreBaseURL is, which is the assertions' issuer.
	APIAudience string
	// CoreAssertionKey is Core's assertion key, pinned: an Ed25519 public
	// key, as a JWK's x. Empty, the key is fetched from Core's
	// /v1/auth/keys.
	CoreAssertionKey string
	// AdminActorIDs, when set, narrow the runtime's administrators to
	// those of Core's (platform_role root or admin) that it names.
	AdminActorIDs []string
	// APITrustedProxies are the addresses (CIDRs, or single addresses) of
	// the proxies in front of the API, whose X-Forwarded-For is believed
	// for the address the audit records.
	APITrustedProxies []string
	// OCR is how the text of scanned PDFs and images is recognized for
	// the models that cannot take the files (package ocr); its zero
	// fields are its defaults.
	OCR ocr.Config
	// Office is how presentations and documents are converted to PDF by
	// LibreOffice (package office); its zero fields are its defaults.
	Office office.Config
	// PDFPartPages is how many pages of a PDF one file part holds, when it
	// has more; 0 is office.DefaultPartPages.
	PDFPartPages int
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
	{"CORE_BASE_URL", "the Core the hosted agents connect to, such as https://lms.example.edu, within CORE_BASE_URL_ALLOWLIST; with DATABASE_URL, the registry of hosted agents runs them"},
	{"EGRESS_PROXY", "proxy for every outbound call (http://, https://, socks5://)"},
	{"LOG_REDACT_EXTRA", "comma-separated regular expressions redacted from logs beside the built-in token and key shapes"},
	{"LOG_LEVEL", "debug, info, warn or error (default " + DefaultLogLevel + ")"},
	{"LOG_FORMAT", "json or text (default " + DefaultLogFormat + ")"},
	{"SECRETS_DIR", "where secret://a/b is looked for as the file a/b before the variable AISHIE_SECRET_A_B"},
	{"WORKER_ID", "this process's name in leases (default hostname-pid)"},
	{"SHUTDOWN_GRACE", "how long answers in progress get on SIGTERM, such as 15s (default 15s)"},
	{"PRICES", "the price table; overrides the runtime's prices_ref"},
	{"KMS_KEY_ID", "the key that seals the secrets kept in the store: local:<dir>/<name>, a 32-byte key, base64, in that file, the directory's other files kept for secrets an older key sealed"},
	{"API_ADDR", "where the JSON API for the front end listens, such as 127.0.0.1:9091, apart from HTTP_ADDR; unset, there is none"},
	{"API_AUDIENCE", "the audience Core's assertions name for this runtime, exactly as in Core's RUNTIME_AUDIENCES, such as https://lms.example.edu/runtime; required with API_ADDR, as CORE_BASE_URL is"},
	{"CORE_ASSERTION_KEY", "Core's assertion key, pinned (an Ed25519 public key as a JWK's x); unset, it is fetched from CORE_BASE_URL/v1/auth/keys"},
	{"ADMIN_ACTOR_IDS", "comma-separated Core actor ids: the runtime's administrators are those of Core's (platform_role root or admin) named here; unset, all of Core's"},
	{"API_TRUSTED_PROXIES", "comma-separated addresses or CIDRs of the proxies in front of the API, whose X-Forwarded-For the audit believes"},
	{"OCR", "auto, on or off: recognize the text of scanned PDFs and of images for models that cannot take the files (default auto: on when tesseract, pdftoppm and prlimit are installed, as in the image; on refuses to start without them)"},
	{"OCR_LANGUAGES", "tesseract's languages, joined by + (default " + ocr.DefaultLanguages + ")"},
	{"OCR_MAX_PAGES", fmt.Sprintf("the pages of a PDF recognized, the rest said to be left out (default %d)", ocr.DefaultMaxPages)},
	{"OCR_DPI", fmt.Sprintf("the resolution a PDF's pages are rendered at, 72 to 600 (default %d)", ocr.DefaultDPI)},
	{"OCR_PAGE_TIMEOUT", "how long rendering or recognizing one page may take, such as 2m (default 90s)"},
	{"OCR_TIMEOUT", "how long one file may take in all, such as 30m (default 15m)"},
	{"OCR_MEMORY_MB", fmt.Sprintf("the address space each OCR program may take, in MB (default %d)", ocr.DefaultMemoryMB)},
	{"OCR_CONCURRENCY", fmt.Sprintf("the files this process recognizes at once, 1 to 8 (default %d)", ocr.DefaultConcurrency)},
	{"OCR_QUEUE", fmt.Sprintf("the files that may wait for their turn; past them a file is not started, and the model is told to ask later (default %d)", ocr.DefaultQueue)},
	{"OCR_WAIT", "how long a question about a file being recognized waits for its text before the model is told to ask again, never past half the answer's time left; 0 waits not at all (default " + ocr.DefaultWait.String() + ")"},
	{"OFFICE_PDF", "auto, on or off: convert presentations and documents (.pptx, .ppt, .odp, .docx, .doc, .odt, .rtf) to PDF with LibreOffice, and older workbooks to .xlsx (default auto: on when soffice and prlimit are installed, as in the image; on refuses to start without them)"},
	{"OFFICE_PDF_TIMEOUT", "how long converting one file may take, such as 3m (default " + office.DefaultTimeout.String() + ")"},
	{"OFFICE_PDF_MAX_PAGES", fmt.Sprintf("the most pages a PDF LibreOffice makes has, the rest of a document left out and said so (default %d)", office.DefaultMaxPages)},
	{"OFFICE_PDF_MEMORY_MB", fmt.Sprintf("the address space LibreOffice may take, in MB (default %d)", office.DefaultMemoryMB)},
	{"PDF_PART_PAGES", fmt.Sprintf("the pages of a PDF given to a model as one file part, when it has more: a longer one is given in parts of its pages, where poppler's pdftocairo is installed, and never more than the provider takes (default %d)", office.DefaultPartPages)},
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
		CoreBaseURL: strings.TrimRight(get("CORE_BASE_URL"), "/"),
		APIAddr:     get("API_ADDR"),
		APIAudience: get("API_AUDIENCE"),

		CoreAssertionKey:  get("CORE_ASSERTION_KEY"),
		AdminActorIDs:     splitList(get("ADMIN_ACTOR_IDS")),
		APITrustedProxies: splitList(get("API_TRUSTED_PROXIES")),
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if host, port, err := net.SplitHostPort(e.HTTPAddr); err != nil || port == "" || !isPort(port) || strings.ContainsAny(host, "/ ") {
		bad("HTTP_ADDR: %q is not host:port, such as 127.0.0.1:9090", e.HTTPAddr)
	}
	e.CoreBaseURLAllowlist = splitList(get("CORE_BASE_URL_ALLOWLIST"))
	origins, msgs := parseAllowlist(e.CoreBaseURLAllowlist)
	for _, m := range msgs {
		bad("CORE_BASE_URL_ALLOWLIST: %s", m)
	}
	if e.CoreBaseURL != "" {
		if u, msg := parseCoreURL(e.CoreBaseURL); msg != "" {
			bad("CORE_BASE_URL: %s", msg)
		} else if len(origins) > 0 && !allowed(u, origins) {
			bad("CORE_BASE_URL: %s is not within CORE_BASE_URL_ALLOWLIST", u.Host)
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
	checkAPI(&e, bad)
	e.ShutdownGrace = DefaultShutdownGrace
	if v := get("SHUTDOWN_GRACE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			bad("SHUTDOWN_GRACE: %q is not a duration such as 15s", v)
		} else {
			e.ShutdownGrace = d
		}
	}
	e.OCR = ocrFromEnv(get, bad)
	e.Office = officeFromEnv(get, bad)
	if v := get("PDF_PART_PAGES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			bad("PDF_PART_PAGES: %q is not a whole number from 1 to 1000", v)
		} else {
			e.PDFPartPages = n
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Env{}, err
	}
	return e, nil
}

// ocrFromEnv reads OCR's settings (OCR_*); an unset one is its default.
func ocrFromEnv(get func(string) string, bad func(string, ...any)) ocr.Config {
	c := ocr.Config{Mode: strings.ToLower(get("OCR")), Languages: get("OCR_LANGUAGES")}
	whole := func(name string, into *int, least int) {
		v := get(name)
		if v == "" {
			return
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < least {
			bad("%s: %q is not a whole number of at least %d", name, v, least)
			return
		}
		*into = n
	}
	duration := func(name string, into *time.Duration, zero time.Duration) {
		v := get(name)
		if v == "" {
			return
		}
		d, err := time.ParseDuration(v)
		switch {
		case err != nil || d < 0:
			bad("%s: %q is not a duration such as 30s", name, v)
		case d == 0 && zero == 0:
			bad("%s: it must be more than 0", name)
		case d == 0:
			*into = zero
		default:
			*into = d
		}
	}
	whole("OCR_MAX_PAGES", &c.MaxPages, 1)
	whole("OCR_DPI", &c.DPI, 1)
	whole("OCR_MEMORY_MB", &c.MemoryMB, 1)
	whole("OCR_CONCURRENCY", &c.Concurrency, 1)
	whole("OCR_QUEUE", &c.Queue, 1)
	duration("OCR_PAGE_TIMEOUT", &c.PageTimeout, 0)
	duration("OCR_TIMEOUT", &c.Timeout, 0)
	// OCR_WAIT=0 is no wait, which ocr.Config says as less than 0.
	duration("OCR_WAIT", &c.Wait, -1)
	if err := c.Check(); err != nil {
		for _, e := range strings.Split(err.Error(), "\n") {
			bad("%s", strings.Replace(e, "ocr: ", "OCR: ", 1))
		}
	}
	return c
}

// officeFromEnv reads the conversion's settings (OFFICE_PDF*); an unset one
// is its default.
func officeFromEnv(get func(string) string, bad func(string, ...any)) office.Config {
	c := office.Config{Mode: strings.ToLower(get("OFFICE_PDF"))}
	for _, w := range []struct {
		name string
		into *int
	}{{"OFFICE_PDF_MAX_PAGES", &c.MaxPages}, {"OFFICE_PDF_MEMORY_MB", &c.MemoryMB}} {
		v := get(w.name)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			bad("%s: %q is not a whole number of at least 1", w.name, v)
			continue
		}
		*w.into = n
	}
	if v := get("OFFICE_PDF_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			bad("OFFICE_PDF_TIMEOUT: %q is not a duration such as 3m", v)
		} else {
			c.Timeout = d
		}
	}
	if err := c.Check(); err != nil {
		for _, e := range strings.Split(err.Error(), "\n") {
			bad("%s", strings.Replace(e, "office: ", "OFFICE_PDF: ", 1))
		}
	}
	return c
}

// checkAPI checks the API's settings (docs/deploying.md): with API_ADDR,
// an address that is not HTTP_ADDR's, the audience and Core its assertions
// are held to, and the database and key that hold what people give it.
func checkAPI(e *Env, bad func(string, ...any)) {
	if e.APIAddr != "" {
		host, port, err := net.SplitHostPort(e.APIAddr)
		switch {
		case err != nil || port == "" || !isPort(port) || strings.ContainsAny(host, "/ "):
			bad("API_ADDR: %q is not host:port, such as 127.0.0.1:9091", e.APIAddr)
		case e.APIAddr == e.HTTPAddr && port != "0":
			bad("API_ADDR: it must not be HTTP_ADDR: /status is never to be reached through the API's listener")
		}
		for _, need := range []struct{ name, value, why string }{
			{"API_AUDIENCE", e.APIAudience, "the audience Core's assertions name for this runtime, as in Core's RUNTIME_AUDIENCES"},
			{"CORE_BASE_URL", e.CoreBaseURL, "the Core whose assertions the API takes"},
			{"DATABASE_URL", e.DatabaseURL, "the API keeps the hosted agents in the runtime's PostgreSQL"},
			{"KMS_KEY_ID", e.KMSKeyID, "the API seals the tokens and keys people give it"},
		} {
			if need.value == "" {
				bad("%s: required with API_ADDR: %s", need.name, need.why)
			}
		}
	}
	if a := e.APIAudience; a != "" {
		u, err := url.Parse(a)
		if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
			u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") || u.Host != strings.ToLower(u.Host) ||
			u.Port() == defaultPort(u.Scheme) || u.RawPath != "" || u.String() != a {
			bad("API_AUDIENCE: not an http or https URL of a host and a path alone, written as Core writes it, such as https://lms.example.edu/runtime")
		}
	}
	if k := e.CoreAssertionKey; k != "" {
		if b, err := base64.RawURLEncoding.Strict().DecodeString(k); err != nil || len(b) != 32 {
			bad("CORE_ASSERTION_KEY: not an Ed25519 public key: 32 bytes in base64url, unpadded, as a JWK's x")
		}
	}
	for i, id := range e.AdminActorIDs {
		if !uuidRe.MatchString(id) {
			bad("ADMIN_ACTOR_IDS: entry %d is not an actor id (a UUID)", i+1)
		}
		e.AdminActorIDs[i] = strings.ToLower(id)
	}
	for i, p := range e.APITrustedProxies {
		if _, _, err := net.ParseCIDR(p); err != nil && net.ParseIP(p) == nil {
			bad("API_TRUSTED_PROXIES: entry %d is neither a CIDR nor an address", i+1)
		}
	}
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
