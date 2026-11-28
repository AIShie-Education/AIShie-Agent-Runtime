package config

import (
	"strings"
	"testing"
)

func TestRunnerEnv(t *testing.T) {
	e, err := FromEnv(envOf(map[string]string{"RUNNER_PORT": "3010", "RUNNER_BIN": "node22", "RUNNER_ENTRY": "dist/index.js"}))
	if err != nil {
		t.Fatal(err)
	}
	if e.RunnerPort != 3010 || e.RunnerBin != "node22" || e.RunnerEntry != "dist/index.js" || e.RunnerAddress() != "http://127.0.0.1:3010" {
		t.Errorf("runner env = %+v", e)
	}
	e, err = FromEnv(envOf(map[string]string{"RUNNER_URL": "http://127.0.0.1:3999/"}))
	if err != nil || e.RunnerAddress() != "http://127.0.0.1:3999" {
		t.Errorf("external runner = %+v, %v", e, err)
	}
	for _, tc := range []struct{ name, value string }{{"RUNNER_URL", "https://user:secret@example.test"}, {"RUNNER_PORT", "0"}} {
		_, err := FromEnv(envOf(map[string]string{tc.name: tc.value}))
		if err == nil || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("%s=%q: %v", tc.name, tc.value, err)
		}
	}
}
