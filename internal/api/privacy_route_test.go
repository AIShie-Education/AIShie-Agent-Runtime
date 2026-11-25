package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
)

type askCall struct {
	actor, agent, course string
}

type stubAskPermission struct {
	mu    sync.Mutex
	allow func(actor, agent, course string) bool
	err   error
	calls []askCall
}

func (s *stubAskPermission) CheckAsk(_ context.Context, actor, agent, course string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, askCall{actor: actor, agent: agent, course: course})
	if s.err != nil {
		return false, s.err
	}
	return s.allow != nil && s.allow(actor, agent, course), nil
}

func (s *stubAskPermission) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// hostPrivacyTutor is a course agent owned by Ken, not by the student Yuki.
// Its runtime row therefore cannot be read through any existing owner route.
func hostPrivacyTutor(t *testing.T, h *hostWorld) (fakecore.Actor, HostedAgent) {
	t.Helper()
	kenSeat, err := h.fc.Seat(h.ken.ID, h.co.ID, fakecore.SeatOptions{Preset: "instructor"})
	h.ok(err)
	tutor, err := h.fc.AddAgent("Ken's course tutor", h.ken.ID)
	h.ok(err)
	h.tokens = append(h.tokens, tutor.Token)
	_, err = h.fc.Seat(tutor.ID, h.co.ID, fakecore.SeatOptions{Preset: "course_tutor", Principal: kenSeat.ID})
	h.ok(err)
	return tutor, h.host(h.ken, tutor.ID)
}

func allowPrivacyAsker(h *hostWorld, tutor fakecore.Actor) *stubAskPermission {
	permission := &stubAskPermission{allow: func(actor, agent, course string) bool {
		return actor == h.yuki.ID && agent == tutor.ID && course == h.co.ID
	}}
	h.s.o.AskPermission = permission
	return permission
}

func privacyPath(agentID, courseID string) string {
	return "agents/" + agentID + "/privacy?course_id=" + courseID
}

func patchPrivacyModel(h *hostWorld, id, version, body string) answer {
	h.t.Helper()
	return h.call("PATCH", "agents/"+id, h.ken, body, "If-Match", version)
}

// A student who Core says may ask another person's course agent gets only the
// provider disclosure. The owner, another agent and another course are all
// refused even though the runtime row and its model exist.
func TestAgentPrivacyAuthorization(t *testing.T) {
	h, _, _ := newModelWorld(t, schoolRuntime())
	tutor, hosted := hostPrivacyTutor(t, h)
	if a := patchPrivacyModel(h, hosted.ID, `"1"`, ownOpenAI); a.code != http.StatusOK {
		t.Fatalf("model: %d %s", a.code, a.body)
	}
	permission := allowPrivacyAsker(h, tutor)
	a := h.call("GET", privacyPath(tutor.ID, h.co.ID), h.yuki, "")
	wantSecured(t, a, "no-store")
	var got AgentPrivacy
	a.decode(t, &got)
	openAI := "OpenAI"
	want := AgentPrivacy{AgentID: tutor.ID, Primary: &PrimaryModelProvider{Plan: "own", Provider: "openai", ProviderLabel: &openAI}}
	if a.code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed asker: %d %s", a.code, a.body)
	}
	for _, secret := range []string{ownKey, "gpt-4.1-mini", "sk-proj", "base_url", "key_hint", "owner_actor_id", hosted.ID, h.ken.ID} {
		if strings.Contains(a.body, secret) {
			t.Errorf("the disclosure contains %q: %s", secret, a.body)
		}
	}

	otherCourse := h.fc.AddCourse("OTHER")
	unknown := "0192f3c1-0000-7000-8000-00000000abcd"
	for _, tc := range []struct {
		name, agent, course string
		as                  fakecore.Actor
	}{
		{"the owner is not silently authorized", tutor.ID, h.co.ID, h.ken},
		{"another agent of the same asker", h.helper.ID, h.co.ID, h.yuki},
		{"the same agent through another course", tutor.ID, otherCourse.ID, h.yuki},
		{"an unknown agent", unknown, h.co.ID, h.yuki},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := h.call("GET", privacyPath(tc.agent, tc.course), tc.as, "")
			wantRefused(t, a, http.StatusNotFound, CodeNotFound, ReasonAgentNotFound)
			if strings.Contains(a.body, "openai") || strings.Contains(a.body, "provider") {
				t.Errorf("a refusal discloses a provider: %s", a.body)
			}
		})
	}
	if n := permission.count(); n != 5 {
		t.Errorf("Core authorization calls = %d, want one allowed and four refused", n)
	}
}

// Invalid requests and a missing assertion never reach Core's authorization
// check. Core failures are not converted into permission grants or null
// disclosures, and a runtime without its service credential fails closed.
func TestAgentPrivacyFailsClosed(t *testing.T) {
	h, _, _ := newModelWorld(t, schoolRuntime())
	tutor, _ := hostPrivacyTutor(t, h)
	permission := allowPrivacyAsker(h, tutor)
	for _, tc := range []struct {
		name, method, path, body string
		code                     int
		reason                   string
	}{
		{"no course", "GET", "agents/" + tutor.ID + "/privacy", "", 400, ReasonMissingField},
		{"invalid course", "GET", privacyPath(tutor.ID, "course-1"), "", 400, ReasonInvalidField},
		{"invalid agent", "GET", privacyPath("agent-1", h.co.ID), "", 400, ReasonInvalidField},
		{"unknown query", "GET", privacyPath(tutor.ID, h.co.ID) + "&agent_id=x", "", 400, ReasonUnknownParameter},
		{"course twice", "GET", "agents/" + tutor.ID + "/privacy?course_id=" + h.co.ID + "&course_id=" + h.co.ID, "", 400, ReasonInvalidField},
		{"a member in the body", "GET", privacyPath(tutor.ID, h.co.ID), `{"provider":"openai"}`, 400, ReasonUnknownField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := permission.count()
			a := h.call(tc.method, tc.path, h.yuki, tc.body)
			wantRefused(t, a, tc.code, CodeInvalidArgument, tc.reason)
			if permission.count() != before {
				t.Error("an invalid request asked Core for an authorization decision")
			}
		})
	}
	before := permission.count()
	unauthenticated := h.get(Prefix+privacyPath(tutor.ID, h.co.ID), "")
	wantRefused(t, unauthenticated, http.StatusUnauthorized, CodeUnauthenticated, "assertion_missing")
	if permission.count() != before {
		t.Error("an unauthenticated request asked Core for an authorization decision")
	}

	permission.err = errors.New("Core is down")
	wantRefused(t, h.call("GET", privacyPath(tutor.ID, h.co.ID), h.yuki, ""), 503, CodeUnavailable, ReasonCoreUnavailable)
	permission.err = &core.ServiceError{Tool: "agent_runtime.check_ask", Reason: core.ReasonCoreTooOld}
	wantRefused(t, h.call("GET", privacyPath(tutor.ID, h.co.ID), h.yuki, ""), 422, CodeFailedPrecondition, ReasonCoreTooOld)

	withoutCredential := newHostWorld(t, fakecore.Options{}, nil)
	withoutCredential.s.o.Runtime = nil
	wantRefused(t, withoutCredential.call("GET", privacyPath(withoutCredential.helper.ID, withoutCredential.co.ID), withoutCredential.yuki, ""),
		503, CodeUnavailable, ReasonRuntimeMisconfigured)
}

// The school offer in force, its real display label and the possible own-key
// fallback come from the same effective configuration the worker runs.
func TestAgentPrivacySchoolOfferAndFallback(t *testing.T) {
	h, _, _ := newModelWorld(t, schoolRuntime())
	tutor, hosted := hostPrivacyTutor(t, h)
	allowPrivacyAsker(h, tutor)
	if a := patchPrivacyModel(h, hosted.ID, `"1"`, `{"model":{"school":{"offer":"standard"}}}`); a.code != 200 {
		t.Fatalf("school offer: %d %s", a.code, a.body)
	}
	if a := patchPrivacyModel(h, hosted.ID, `"2"`, ownOpenAI); a.code != 200 {
		t.Fatalf("fallback: %d %s", a.code, a.body)
	}
	a := h.call("GET", privacyPath(tutor.ID, h.co.ID), h.yuki, "")
	var got AgentPrivacy
	a.decode(t, &got)
	if a.code != 200 || got.Primary == nil || got.Primary.Plan != "school" || got.Primary.Provider != "deepseek" ||
		got.Primary.ProviderLabel == nil || *got.Primary.ProviderLabel != "DeepSeek" ||
		got.Primary.SchoolOfferLabel == nil || *got.Primary.SchoolOfferLabel != "School AI" ||
		got.Fallback == nil || got.Fallback.Provider != "openai" || got.Fallback.ProviderLabel == nil || *got.Fallback.ProviderLabel != "OpenAI" {
		t.Fatalf("school and fallback: %d %s", a.code, a.body)
	}
	for _, secret := range []string{"deepseek-chat", "gpt-4.1-mini", "standard", "secret://", "sealed://", "key_ref", "endpoint"} {
		if strings.Contains(a.body, secret) {
			t.Errorf("the disclosure contains %q: %s", secret, a.body)
		}
	}
	h.noSchoolKey(a)
}

// A school-operated self-hosted server has no catalogue company label. Its
// offer label is already intended for display and is the truthful disclosure.
func TestAgentPrivacySchoolOwnServer(t *testing.T) {
	rt := schoolRuntime()
	rt.School.Offers = append(rt.School.Offers, config.SchoolOffer{ID: "gpu", Label: "CS GPU server", Adapter: "openai_chat",
		Model: "llama-private", Provider: "openai_compatible", BaseURL: "https://gpu.internal.example/v1", KeyRef: "secret://school/keys/gpu"})
	h, _, _ := newModelWorld(t, rt)
	tutor, hosted := hostPrivacyTutor(t, h)
	allowPrivacyAsker(h, tutor)
	if a := patchPrivacyModel(h, hosted.ID, `"1"`, `{"model":{"school":{"offer":"gpu"}}}`); a.code != 200 {
		t.Fatalf("school's own server: %d %s", a.code, a.body)
	}
	a := h.call("GET", privacyPath(tutor.ID, h.co.ID), h.yuki, "")
	var got AgentPrivacy
	a.decode(t, &got)
	if a.code != 200 || got.Primary == nil || got.Primary.Plan != "school" || got.Primary.Provider != "openai_compatible" ||
		got.Primary.ProviderLabel != nil || got.Primary.SchoolOfferLabel == nil || *got.Primary.SchoolOfferLabel != "CS GPU server" || got.Fallback != nil {
		t.Fatalf("school's own server: %d %s", a.code, a.body)
	}
	for _, secret := range []string{"gpu.internal.example", "llama-private", "secret://", "gpu\"",
		"base_url", "key_ref"} {
		if strings.Contains(a.body, secret) {
			t.Errorf("the disclosure contains %q: %s", secret, a.body)
		}
	}
	h.noSchoolKey(a)
}

// When an offer is withdrawn, the worker answers on its own-key fallback
// alone. The privacy contract follows that transition rather than naming the
// stale school provider and a fallback that is no longer secondary.
func TestAgentPrivacyWithdrawnOfferUsesEffectiveModel(t *testing.T) {
	h, _, fh := newModelWorld(t, schoolRuntime())
	tutor, hosted := hostPrivacyTutor(t, h)
	allowPrivacyAsker(h, tutor)
	patchPrivacyModel(h, hosted.ID, `"1"`, `{"model":{"school":{"offer":"standard"}}}`)
	patchPrivacyModel(h, hosted.ID, `"2"`, ownOpenAI)
	fh.mu.Lock()
	fh.yaml = &config.Config{}
	fh.mu.Unlock()
	a := h.call("GET", privacyPath(tutor.ID, h.co.ID), h.yuki, "")
	var got AgentPrivacy
	a.decode(t, &got)
	if a.code != 200 || got.Primary == nil || got.Primary.Plan != "own" || got.Primary.Provider != "openai" ||
		got.Primary.SchoolOfferLabel != nil || got.Fallback != nil {
		t.Fatalf("withdrawn offer: %d %s", a.code, a.body)
	}
}

// No configured answer model is a successful, explicit null disclosure; it
// does not fall back to an offer list or guess a provider.
func TestAgentPrivacyWithoutAModel(t *testing.T) {
	h, _, _ := newModelWorld(t, schoolRuntime())
	tutor, _ := hostPrivacyTutor(t, h)
	allowPrivacyAsker(h, tutor)
	a := h.call("GET", privacyPath(tutor.ID, h.co.ID), h.yuki, "")
	var got AgentPrivacy
	a.decode(t, &got)
	if a.code != 200 || got.AgentID != tutor.ID || got.Primary != nil || got.Fallback != nil ||
		!strings.Contains(a.body, `"primary":null,"fallback":null`) {
		t.Fatalf("no model: %d %s", a.code, a.body)
	}
}

// Operator-hosted YAML agents use the same contract and their per-course
// model override, so the provider is the one that answers in this course.
func TestAgentPrivacyYAMLAgentCourseOverride(t *testing.T) {
	h, _, fh := newModelWorld(t, config.Runtime{})
	allowPrivacyAsker(h, h.helper)
	doc := map[string]any{
		"agent": map[string]any{
			"id": "privacy-yaml", "display_name": "Course tutor", "tenant_id": "ten_privacy",
			"core":  map[string]any{"base_url": h.srv.URL, "agent_id": h.helper.ID},
			"model": map[string]any{"adapter": "openai_chat", "model": "gpt-test", "provider": "openai", "key_source": "own", "key_ref": "env://OPENAI"},
		},
		"courses": map[string]any{h.co.ID: map[string]any{
			"model": map[string]any{"adapter": "anthropic", "model": "claude-test", "provider": "anthropic", "key_source": "own", "key_ref": "env://ANTHROPIC"},
		}},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	agents, rejected := config.LoadDocuments(fh.yaml, nil, config.Source{Name: "privacy-test", Data: raw})
	if len(rejected) != 0 || len(agents) != 1 {
		t.Fatalf("YAML agent: %+v %+v", agents, rejected)
	}
	fh.mu.Lock()
	fh.yaml.Agents = agents
	fh.mu.Unlock()
	a := h.call("GET", privacyPath(h.helper.ID, h.co.ID), h.yuki, "")
	var got AgentPrivacy
	a.decode(t, &got)
	if a.code != 200 || got.Primary == nil || got.Primary.Provider != "anthropic" || got.Primary.ProviderLabel == nil ||
		*got.Primary.ProviderLabel != "Anthropic" || got.Primary.Plan != "own" || got.Fallback != nil {
		t.Fatalf("course override: %d %s", a.code, a.body)
	}
	if strings.Contains(a.body, "claude-test") || strings.Contains(a.body, "env://") {
		t.Errorf("the course disclosure leaks model configuration: %s", a.body)
	}
}
