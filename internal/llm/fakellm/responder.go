package fakellm

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// DefaultResponder is a model with two rules, enough for an answer to go
// all the way through the runtime and Core:
//
//   - asked a question that mentions an assignment ("assignment",
//     "homework", or one by its short name, HW3), with assignment_list
//     offered and no tool result yet, it calls assignment_list with {};
//   - otherwise it answers "Answer: " and the question's first 200
//     characters, saying how many tool results it was given.
//
// The question is the asker's newest message, however the results that
// followed it came: as tool messages, as the text the adapter makes of them
// for a server it cannot tell to stop calling tools (ForceAnswer), or with
// the files that came with them. Every answer carries usage, counted at
// four bytes a token.
func DefaultResponder(req ChatRequest) ChatResponse {
	question, results := read(req.Messages)
	lastIsUser := len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "user"
	if lastIsUser && results == 0 && offered(req, "assignment_list") && mentionsAssignment.MatchString(question) {
		call := ToolCall{ID: "call_assignment_list", Type: "function", Function: FunctionCall{Name: "assignment_list", Arguments: "{}"}}
		return ChatResponse{
			Choices: []Choice{{Message: ChatMessage{Role: "assistant", ToolCalls: []ToolCall{call}}, FinishReason: "tool_calls"}},
			Usage:   usage(req, call.Function.Name+call.Function.Arguments),
		}
	}
	answer := "Answer: " + prefix(question, 200)
	switch results {
	case 0:
	case 1:
		answer += " (from 1 tool result)"
	default:
		answer += fmt.Sprintf(" (from %d tool results)", results)
	}
	return ChatResponse{
		Choices: []Choice{{Message: ChatMessage{Role: "assistant", Content: answer}, FinishReason: "stop"}},
		Usage:   usage(req, answer),
	}
}

var mentionsAssignment = regexp.MustCompile(`(?i)assignment|homework|\bhw ?\d`)

// read finds the question in a conversation, and counts the tool results
// given since. A user message is not the question when it holds results as
// text (each "[result of …]" or "[error from …]", as llm.FlattenToolHistory
// writes them) or comes straight after tool messages (the files that came
// with the results).
func read(msgs []ChatMessage) (question string, results int) {
	prev := ""
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			results++
		case "user":
			text := m.Text()
			switch n := flattenedResults(text); {
			case n > 0:
				results += n
			case prev == "tool":
			default:
				question = text
			}
		}
		prev = m.Role
	}
	return question, results
}

// flattenedResults counts the tool results a message gives as text.
func flattenedResults(text string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "[result of ") || strings.HasPrefix(line, "[error from ") {
			n++
		}
	}
	return n
}

// Reply is a response that answers text and stops.
func Reply(text string) ChatResponse {
	return ChatResponse{
		Choices: []Choice{{Message: ChatMessage{Role: "assistant", Content: text}, FinishReason: "stop"}},
		Usage:   &Usage{PromptTokens: 10, CompletionTokens: tokens(text), TotalTokens: 10 + tokens(text)},
	}
}

// CallTools is a response that makes calls, in order, with ids call_1,
// call_2, ….
func CallTools(calls ...FunctionCall) ChatResponse {
	msg := ChatMessage{Role: "assistant"}
	for i, c := range calls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: fmt.Sprintf("call_%d", i+1), Type: "function", Function: c})
	}
	return ChatResponse{
		Choices: []Choice{{Message: msg, FinishReason: "tool_calls"}},
		Usage:   &Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20},
	}
}

// Failure is a response that fails with status and an OpenAI-shaped error.
func Failure(status int, code, message string) ChatResponse {
	return ChatResponse{Status: status, Error: &Error{Message: message, Type: code, Code: code}}
}

// offered reports whether tool is declared and the model may call it.
func offered(req ChatRequest, tool string) bool {
	if strings.Trim(string(req.ToolChoice), `"`) == "none" {
		return false
	}
	for _, t := range req.Tools {
		if t.Function.Name == tool {
			return true
		}
	}
	return false
}

// prefix is s's first n characters.
func prefix(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func usage(req ChatRequest, completion string) *Usage {
	in := 0
	for _, m := range req.Messages {
		in += len(m.Text())
		for _, c := range m.ToolCalls {
			in += len(c.Function.Name) + len(c.Function.Arguments)
		}
	}
	u := &Usage{PromptTokens: (in+3)/4 + 1, CompletionTokens: tokens(completion)}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	return u
}

func tokens(s string) int { return (len(s)+3)/4 + 1 }
