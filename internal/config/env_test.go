package config

import (
	"strings"
	"testing"
	"time"
)

func envOf(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestFromEnvDefaults(t *testing.T) {
	e, err := FromEnv(envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if e.HTTPAddr != DefaultHTTPAddr || e.LogLevel != DefaultLogLevel || e.LogFormat != DefaultLogFormat || e.ShutdownGrace != DefaultShutdownGrace {
		t.Errorf("defaults = %+v", e)
	}
	if e.CoreServiceCredential != DefaultCoreServiceCredential || e.RunnerPort != DefaultRunnerPort || e.RunnerBin != DefaultRunnerBin || e.RunnerEntry != DefaultRunnerEntry {
		t.Errorf("service/runner defaults = %+v", e)
	}
	if e.RunnerAddress() != "http://127.0.0.1:3001" {
		t.Errorf("RunnerAddress = %q", e.RunnerAddress())
	}
}

func TestFromEnvValues(t *testing.T) {
	e, err := FromEnv(envOf(map[string]string{
		"HTTP_ADDR": "127.0.0.1:9999", "LOG_LEVEL": "DEBUG", "LOG_FORMAT": "text", "SHUTDOWN_GRACE": "3s",
		"CORE_BASE_URL": "http://127.0.0.1:8080/", "EGRESS_PROXY": "http://proxy.internal:3128",
		"RUNNER_URL": "http://127.0.0.1:4000/", "RUNNER_PORT": "4001", "RUNNER_BIN": "node22", "RUNNER_ENTRY": "dist/run.js",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if e.HTTPAddr != "127.0.0.1:9999" || e.LogLevel != "debug" || e.LogFormat != "text" || e.ShutdownGrace != 3*time.Second {
		t.Errorf("values = %+v", e)
	}
	if e.CoreBaseURL != "http://127.0.0.1:8080" || e.RunnerAddress() != "http://127.0.0.1:4000" || e.RunnerPort != 4001 || e.RunnerBin != "node22" || e.RunnerEntry != "dist/run.js" {
		t.Errorf("core/runner values = %+v", e)
	}
}

func TestFromEnvRejects(t *testing.T) {
	base := map[string]string{
		"API_ADDR": "127.0.0.1:9091", "API_AUDIENCE": "https://lms.example.edu/runtime", "CORE_BASE_URL": "https://lms.example.edu",
		"DATABASE_URL": "postgres:///runtime", "KMS_KEY_ID": "local:/tmp/kek",
	}
	for _, tc := range []struct{ name, value, want string }{
		{"HTTP_ADDR", "not an address", "HTTP_ADDR"},
		{"EGRESS_PROXY", "file:///tmp/proxy", "EGRESS_PROXY"},
		{"RUNNER_URL", "https://user:secret@example.test", "RUNNER_URL"},
		{"RUNNER_PORT", "70000", "RUNNER_PORT"},
		{"LOG_LEVEL", "verbose", "LOG_LEVEL"},
		{"SHUTDOWN_GRACE", "forever", "SHUTDOWN_GRACE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FromEnv(envOf(map[string]string{tc.name: tc.value}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v", err)
			}
		})
	}
	for _, missing := range []string{"API_AUDIENCE", "CORE_BASE_URL", "DATABASE_URL", "KMS_KEY_ID"} {
		values := make(map[string]string, len(base))
		for k, v := range base {
			values[k] = v
		}
		delete(values, missing)
		_, err := FromEnv(envOf(values))
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("without %s: %v", missing, err)
		}
	}
}

func TestEnvHelpOmitsMedia(t *testing.T) {
	help := EnvHelp()
	for _, want := range []string{"RUNNER_URL", "RUNNER_PORT", "RUNNER_BIN", "RUNNER_ENTRY"} {
		if !strings.Contains(help, want) {
			t.Errorf("EnvHelp lacks %s", want)
		}
	}
	for _, old := range []string{"OCR", "OFFICE_PDF", "TRANSCRIBE", "RENDITIONS", "PDF_PART_PAGES"} {
		if strings.Contains(help, old) {
			t.Errorf("EnvHelp retains %s", old)
		}
	}
}
