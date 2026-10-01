package fakecore

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
)

// runtimeClient is the runtime's client of the fake's agent runtime
// service, with the credential token.
func (w *fakeWorld) runtimeClient(token string) *core.RuntimeService {
	return core.NewRuntimeService(core.RuntimeCaller(core.RuntimeOptions{BaseURL: w.srv.URL, HTTPClient: w.srv.Client(),
		Credential: func(context.Context) (string, error) { return token, nil }}))
}

// The runtime's client of the agent runtime's service, against the fake:
// the owner checked, the agent read, its one token issued (the one before
// revoked, a Core's replay without the token issued again under a new
// key) and revoked; an mcp agent, a suspended one and nobody refused as
// Core refuses them; a credential not the runtime's refused.
func TestRuntimeService(t *testing.T) {
	w := newFakeWorld(t, Options{})
	ctx := t.Context()
	rs := w.runtimeClient(w.svc.Token)

	owns, a, err := rs.CheckOwner(ctx, w.satoA.ID, w.tutorA.ID)
	if err != nil || !owns || a.AgentID != w.tutorA.ID || a.Hosting != core.HostingRuntime || !a.Hostable || a.OwnerActorID != w.satoA.ID ||
		a.LiveSeats != 1 || !a.SiteChat || a.RuntimeToken == nil {
		t.Fatalf("CheckOwner(Sato, tutor) = %v %+v %v", owns, a, err)
	}
	for _, c := range [][2]string{{w.people[0].ID, w.tutorA.ID}, {w.satoA.ID, w.people[0].ID}, {w.satoA.ID, "0192f3c1-0000-7000-8000-00000000abcd"}} {
		if owns, a, err := rs.CheckOwner(ctx, c[0], c[1]); err != nil || owns || a != nil {
			t.Errorf("CheckOwner(%s, %s) = %v %+v %v", c[0], c[1], owns, a, err)
		}
	}
	if _, err := rs.Agent(ctx, w.people[0].ID); !core.IsNotFound(err) {
		t.Errorf("Agent of a person: %v", err)
	}

	before := w.fc.RuntimeToken(w.tutorA.ID)
	issued, err := rs.IssueToken(ctx, w.tutorA.ID, "")
	if err != nil || issued.Token == "" || issued.AgentID != w.tutorA.ID || len(issued.Replaced) != 1 || issued.Replaced[0] != before.CredentialID {
		t.Fatalf("IssueToken = %+v, %v", issued, err)
	}
	if now := w.fc.RuntimeToken(w.tutorA.ID); now.Token != issued.Token || now.CredentialID != issued.CredentialID {
		t.Errorf("the tutor's runtime token is %+v, not the one issued", now)
	}
	if creds := w.fc.Credentials(w.tutorA.ID); creds[0].Label != core.RuntimeTokenLabel {
		t.Errorf("the token's label: %q", creds[0].Label)
	}
	var keys []string
	for _, c := range w.fc.Calls() {
		if c.Tool == core.ToolRuntimeIssueToken {
			keys = append(keys, c.IdempotencyKey)
		}
	}
	if len(keys) != 1 || keys[0] == "" {
		t.Errorf("issue_token's keys: %q", keys)
	}

	// Core answering the replay of a call carried out before, without the
	// token: issued again under a new key.
	replays := 0
	w.fc.Inject(func(c InjectedCall) *Injection {
		if c.Tool != core.ToolRuntimeIssueToken || replays > 0 {
			return nil
		}
		replays++
		return &Injection{Status: http.StatusOK, Body: []byte(`{"status":"executed","replayed":true,"action_id":"0192f3c1-0000-7000-8000-00000000abcd",` +
			`"review_state":"none","result":{"agent_id":"` + w.tutorA.ID + `","credential_id":"0192f3c1-0000-7000-8000-00000000abce","token_prefix":"abcdefghijkl"}}`),
			ContentType: "application/json"}
	})
	again, err := rs.IssueToken(ctx, w.tutorA.ID, "AIshie agent runtime")
	w.fc.Inject(nil)
	if err != nil || again.Token == "" || again.CredentialID == issued.CredentialID || replays != 1 {
		t.Fatalf("IssueToken after a replay without the token = %+v, %v (%d replays)", again, err, replays)
	}

	revoked, err := rs.RevokeToken(ctx, w.tutorA.ID)
	if err != nil || len(revoked) != 1 || revoked[0] != again.CredentialID || w.fc.SiteChat(w.tutorA.ID) {
		t.Fatalf("RevokeToken = %v, %v", revoked, err)
	}
	if revoked, err := rs.RevokeToken(ctx, w.tutorA.ID); err != nil || len(revoked) != 0 {
		t.Errorf("RevokeToken of none = %v, %v", revoked, err)
	}

	mcpID, _, _ := w.mcpAgent()
	for _, c := range []struct {
		name, id, reason string
		suspend          string
	}{
		{"an mcp agent", mcpID, core.ReasonNotRuntimeHosted, ""},
		{"a suspended agent", w.tutorA.ID, core.ReasonAgentSuspended, w.tutorA.ID},
		{"an agent whose owner is suspended", w.tutorA.ID, core.ReasonOwnerSuspended, w.satoA.ID},
	} {
		if c.suspend != "" {
			w.ok(w.fc.SuspendActor(c.suspend))
		}
		a, err := rs.Agent(ctx, c.id)
		if err != nil || a.Hostable || a.Reason != c.reason {
			t.Errorf("%s: Agent = %+v, %v", c.name, a, err)
		}
		if _, err := rs.IssueToken(ctx, c.id, ""); !core.IsReason(err, c.reason) {
			t.Errorf("%s: IssueToken = %v", c.name, err)
		}
		if c.suspend != "" {
			w.ok(w.fc.ReactivateActor(c.suspend))
		}
	}
	if _, err := rs.IssueToken(ctx, "0192f3c1-0000-7000-8000-00000000abcd", ""); !core.IsNotFound(err) {
		t.Errorf("IssueToken of nobody: %v", err)
	}

	// Credentials that are not the runtime's.
	revokedSvc := w.fc.IssueRuntimeServiceToken("old")
	w.ok(w.fc.RevokeServiceToken(revokedSvc.CredentialID))
	for name, token := range map[string]string{
		"a revoked credential":               revokedSvc.Token,
		"the transcription service's":        w.fc.IssueServiceToken("transcriber").Token,
		"an unknown credential, as a site's": "aissvc_aaaaaaaaaaaa_NotACredentialOfThisCore0000000000000",
	} {
		_, err := w.runtimeClient(token).Agent(ctx, w.tutorA.ID)
		if !core.CredentialRefused(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := w.runtimeClient(w.tutorA.Token).Agent(ctx, w.tutorA.ID); err == nil || core.CredentialRefused(err) {
		t.Errorf("an agent's token as the credential: %v", err)
	}
}

// A Core from before an agent's hosting has no agent runtime service: the
// client says so, and calls nothing.
func TestRuntimeServiceCoreTooOld(t *testing.T) {
	w := newFakeWorld(t, Options{WithoutHosting: true})
	_, err := w.runtimeClient(w.svc.Token).Agent(t.Context(), w.tutorA.ID)
	if !core.IsReason(err, core.ReasonCoreTooOld) {
		t.Fatalf("Agent against a Core without the service: %v", err)
	}
	for _, c := range w.fc.Calls() {
		if c.Tool == core.ToolRuntimeAgent {
			t.Errorf("called %s", c.Tool)
		}
	}
	var se *core.ServiceError
	if !errors.As(err, &se) || se.Tool != core.ToolRuntimeAgent {
		t.Errorf("the error: %#v", err)
	}
}
