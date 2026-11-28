package chatwire

import (
	"context"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

func TestRunnerHistoryIncludesRevisionReasonAndEarlierAnswer(t *testing.T) {
	w := &Wiring{}
	in := chat.GenerationInput{
		Conversation: store.ChatConversation{ID: "conversation", CourseID: "course", AgentID: "agent", OpenerActorID: "actor"},
		Messages:     []store.ChatMessage{{ID: "question", Role: store.ChatRoleUser, Status: store.ChatPosted, Body: "Please improve it", Seq: 1}},
		Generation: store.ChatGeneration{UserMessageID: "question", RevisionReason: "Be concise and cite the page",
			RevisionBody: "The earlier verbose answer", RevisionOfGenerationID: "earlier-generation"},
	}
	eff := &config.Effective{Agent: config.Agent{Answer: config.Answer{HistoryMessages: 10}}}
	messages, _, err := w.runnerHistory(context.Background(), in, eff)
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, message := range messages {
		for _, part := range message.Parts {
			all.WriteString(part.Text)
			all.WriteByte('\n')
		}
	}
	text := all.String()
	if !strings.Contains(text, "Be concise and cite the page") || !strings.Contains(text, "The earlier verbose answer") {
		t.Fatalf("revision context missing from Runner history: %s", text)
	}
	last := messages[len(messages)-1]
	if last.Role != "user" || len(last.Parts) == 0 || last.Parts[len(last.Parts)-1].Text != "Please improve it" {
		t.Fatalf("original question is not last: %+v", last)
	}
}
