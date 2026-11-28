package core

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// CheckAsk uses Core's read-only service route with all three ids in the
// path, the runtime credential in Authorization, no query and no body.
func TestRuntimeServiceCheckAsk(t *testing.T) {
	const (
		actor  = "0192f3c1-0000-7000-8000-00000000000a"
		agent  = "0192f3c1-0000-7000-8000-00000000000b"
		course = "0192f3c1-0000-7000-8000-00000000000c"
		token  = "aissvc_runtime_test_credential"
	)
	path := "/v1/services/agent_runtime/actors/" + actor + "/courses/" + course + "/agents/" + agent + "/may-ask"
	cat := testCatalogue(t)
	answer := true
	srv, seen := newFakeREST(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			writeJSON(w, http.StatusNotFound, map[string]any{"status": "error", "error": map[string]any{"code": "not_found", "message": "wrong route"}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "executed", "result": map[string]any{"may_ask": answer}})
	})
	credentialCalls := 0
	svc := NewRuntimeService(RuntimeCaller(RuntimeOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Catalogue: cat, Once: true,
		Credential: func(context.Context) (string, error) { credentialCalls++; return token, nil }}))
	mayAsk, err := svc.CheckAsk(t.Context(), actor, agent, course)
	if err != nil || mayAsk == nil || !mayAsk.MayAsk {
		t.Fatalf("CheckAsk = %v, %v", mayAsk, err)
	}
	requests := seen()
	if len(requests) != 1 || requests[0].Method != http.MethodGet || requests[0].Path != path || requests[0].Query != "" || requests[0].Body != "" ||
		requests[0].Header.Get("Authorization") != "Bearer "+token || requests[0].Header.Get("Idempotency-Key") != "" {
		t.Errorf("the request: %+v", requests)
	}
	answer = false
	if mayAsk, err = svc.CheckAsk(t.Context(), actor, agent, course); err != nil || mayAsk == nil || mayAsk.MayAsk {
		t.Errorf("a refused ask = %v, %v", mayAsk, err)
	}
	if credentialCalls != 2 {
		t.Errorf("the credential was resolved %d times", credentialCalls)
	}
}

// A Core before the companion route fails closed as core_too_old before the
// runtime resolves its credential or sends a request to a made-up endpoint.
func TestRuntimeServiceCheckAskCoreTooOld(t *testing.T) {
	current := testCatalogue(t)
	cat := &Catalogue{index: map[string]int{}, hash: current.hash}
	for _, tool := range current.tools {
		if tool.MCPName == ToolRuntimeCheckAsk {
			continue
		}
		cat.index[tool.MCPName] = len(cat.tools)
		cat.tools = append(cat.tools, tool)
	}
	credentialCalls := 0
	svc := NewRuntimeService(RuntimeCaller(RuntimeOptions{BaseURL: "http://127.0.0.1:1", Catalogue: cat, Once: true,
		Credential: func(context.Context) (string, error) { credentialCalls++; return "aissvc_unused", nil }}))
	mayAsk, err := svc.CheckAsk(t.Context(), "actor", "agent", "course")
	if mayAsk != nil && mayAsk.MayAsk || !IsReason(err, ReasonCoreTooOld) || credentialCalls != 0 {
		t.Fatalf("CheckAsk on an old Core = %v, %v; credential calls %d", mayAsk, err, credentialCalls)
	}
	var se *ServiceError
	if !errors.As(err, &se) || se.Tool != ToolRuntimeCheckAsk {
		t.Errorf("the error: %#v", err)
	}
}
