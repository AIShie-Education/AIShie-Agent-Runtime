package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

func TestHistoryReadSurvivesCheckAskDenial(t *testing.T) {
	access := &testAccess{auth: completeAuthorization()}
	s := testService(t, access, GeneratorFunc(func(context.Context, GenerationInput, DraftSink) (store.ChatResult, error) {
		return store.ChatResult{Body: "answer"}, nil
	}))
	created, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "question", IdempotencyKey: "create"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		c, _ := s.Get(t.Context(), "actor", "course", created.Conversation.ID)
		return c.State == store.ChatAnswered
	})

	access.mu.Lock()
	access.auth = nil
	access.mu.Unlock()
	if _, err := s.Get(t.Context(), "actor", "course", created.Conversation.ID); err != nil {
		t.Fatalf("history get was denied: %v", err)
	}
	if _, err := s.Messages(t.Context(), "actor", "course", created.Conversation.ID, 50, 0); err != nil {
		t.Fatalf("history messages were denied: %v", err)
	}
	auth, err := s.CheckAccess(t.Context(), "actor", "course", "agent")
	if err != nil || auth == nil || auth.Agent.AgentID != "agent" {
		t.Fatalf("history stream access = %+v %v", auth, err)
	}
	if _, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "new", IdempotencyKey: "new"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("new conversation after denial = %v", err)
	}
	if _, err := s.Append(t.Context(), "actor", "course", created.Conversation.ID, AppendInput{Body: "another", IdempotencyKey: "another"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("ask after denial = %v", err)
	}
	if _, err := s.Messages(t.Context(), "other", "course", created.Conversation.ID, 50, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner history read = %v", err)
	}
}
