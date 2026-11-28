package core

import (
	"context"
	"encoding/json"
	"testing"
)

type chatCheckCallerFunc func(context.Context, string, json.RawMessage) (*Envelope, error)

func (f chatCheckCallerFunc) Call(ctx context.Context, tool string, args json.RawMessage) (*Envelope, error) {
	return f(ctx, tool, args)
}

func TestRuntimeCheckAskDecodesCoreGrantJSON(t *testing.T) {
	const raw = `{"may_ask":true,"grant":{"opener_member_id":"11111111-1111-7111-8111-111111111111","respondent_member_id":"22222222-2222-7222-8222-222222222222","respondent_principal_member_id":"33333333-3333-7333-8333-333333333333","conversation_ask_level":"autonomous","action_decide_level":"confirm_required","conversation_answer_level":"autonomous","answers_course":true}}`
	s := NewRuntimeService(chatCheckCallerFunc(func(_ context.Context, tool string, _ json.RawMessage) (*Envelope, error) {
		if tool != ToolRuntimeCheckAsk {
			t.Fatalf("tool = %s", tool)
		}
		return &Envelope{Status: StatusExecuted, Result: json.RawMessage(raw)}, nil
	}))
	got, err := s.CheckAsk(t.Context(), "actor", "agent", "course")
	if err != nil {
		t.Fatal(err)
	}
	if !got.MayAsk || got.Grant == nil {
		t.Fatalf("result = %+v", got)
	}
	g := got.Grant
	if g.OpenerMemberID != "11111111-1111-7111-8111-111111111111" || g.RespondentMemberID != "22222222-2222-7222-8222-222222222222" ||
		g.RespondentPrincipalMemberID != "33333333-3333-7333-8333-333333333333" || g.ConversationAskLevel != "autonomous" || g.ActionDecideLevel != "confirm_required" || g.ConversationAnswerLevel != "autonomous" ||
		g.AnswersCourse == nil || !*g.AnswersCourse {
		t.Fatalf("grant = %+v", g)
	}
}

func TestRuntimeCheckChatDecisionJSON(t *testing.T) {
	s := NewRuntimeService(chatCheckCallerFunc(func(_ context.Context, tool string, args json.RawMessage) (*Envelope, error) {
		if tool != ToolRuntimeCheckChatDecision {
			t.Fatalf("tool = %s", tool)
		}
		var in map[string]any
		if err := json.Unmarshal(args, &in); err != nil {
			t.Fatal(err)
		}
		if in["subject"] != "answer" || in["operation"] != "decide" || in["opener_actor_id"] != "opener" {
			t.Fatalf("args=%v", in)
		}
		return &Envelope{Status: StatusExecuted, Result: json.RawMessage(`{"allowed":true,"action_decide_level":"pending_review"}`)}, nil
	}))
	allowed, err := s.CheckChatDecision(t.Context(), "reviewer", "agent", "course", "opener", "answer", "decide")
	if err != nil || allowed == nil || !allowed.Allowed || allowed.ActionDecideLevel != "pending_review" {
		t.Fatalf("allowed=%v err=%v", allowed, err)
	}
}

func TestRuntimeCheckChatDecisionFalseDropsLevel(t *testing.T) {
	s := NewRuntimeService(chatCheckCallerFunc(func(context.Context, string, json.RawMessage) (*Envelope, error) {
		return &Envelope{Status: StatusExecuted, Result: json.RawMessage(`{"allowed":false,"action_decide_level":"autonomous"}`)}, nil
	}))
	got, err := s.CheckChatDecision(t.Context(), "reviewer", "agent", "course", "opener", "question", "review")
	if err != nil || got.Allowed || got.ActionDecideLevel != "" {
		t.Fatalf("false decision=%+v %v", got, err)
	}
}

func TestRuntimeCheckAskFalseRevealsNoAdditiveFields(t *testing.T) {
	s := NewRuntimeService(chatCheckCallerFunc(func(context.Context, string, json.RawMessage) (*Envelope, error) {
		return &Envelope{Status: StatusExecuted, Result: json.RawMessage(`{"may_ask":false,"grant":{"opener_member_id":"must-not-survive","respondent_member_id":"must-not-survive","action_decide_level":"autonomous","conversation_answer_level":"autonomous"}}`)}, nil
	}))
	got, err := s.CheckAsk(t.Context(), "actor", "agent", "course")
	if err != nil {
		t.Fatal(err)
	}
	if got.MayAsk || got.Grant != nil {
		t.Fatalf("false result leaked grant: %+v", got)
	}
}
