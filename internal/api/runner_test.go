package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	runtimeRunner "github.com/AIShie-Education/AIShie-Agent-Runtime/internal/runner"
)

type runnerProbeStub struct {
	result runtimeRunner.ProbeResult
	seen   []runtimeRunner.Model
}

func (s *runnerProbeStub) Probe(_ context.Context, m runtimeRunner.Model) (runtimeRunner.ProbeResult, error) {
	s.seen = append(s.seen, m)
	return s.result, nil
}

func TestKeysTestUsesRunnerProbe(t *testing.T) {
	status := http.StatusUnauthorized
	probe := &runnerProbeStub{result: runtimeRunner.ProbeResult{Result: runtimeRunner.ResultKeyRefused, HTTPStatus: &status, LatencyMS: 12}}
	f := newFixture(t, func(o *Options) { o.Runner = probe })
	const key = "sk-proj-test-key-that-must-not-leak"
	a := f.send(req{method: http.MethodPost, path: Prefix + "keys/test", token: f.core.assert(t, claims(yuki, "", nil)),
		body: strings.NewReader(`{"provider":"openai","model":"gpt-4.1-mini","key":"` + key + `"}`), headers: []string{"Content-Type", "application/json"}})
	var out KeyTest
	a.decode(t, &out)
	if a.code != http.StatusOK || out.Result != runtimeRunner.ResultKeyRefused || out.HTTPStatus == nil || *out.HTTPStatus != status || out.LatencyMS != 12 {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if len(probe.seen) != 1 || probe.seen[0].APIKey != key || probe.seen[0].Provider != "openai" || !probe.seen[0].Hosted {
		t.Errorf("probe models = %+v", probe.seen)
	}
	if strings.Contains(a.body, key) {
		t.Error("the response contains the key")
	}
}

func TestMediaRoutesAreGoneAndTranscriptionFeatureIsFalse(t *testing.T) {
	f := newFixture(t, nil)
	a := f.get(Prefix+"info", "")
	if a.code != http.StatusOK || !strings.Contains(a.body, `"transcription":false`) {
		t.Errorf("info: %d %s", a.code, a.body)
	}
	for _, path := range []string{"admin/settings", "admin/transcription/jobs", "admin/transcription/credential"} {
		if a := f.get(Prefix+path, f.core.assert(t, claims(yuki, "admin", nil))); a.code != http.StatusNotFound && a.code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d %s", path, a.code, a.body)
		}
	}
}
