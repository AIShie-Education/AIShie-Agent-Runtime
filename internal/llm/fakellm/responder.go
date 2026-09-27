package fakellm

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// DefaultResponder is a model with two rules, enough for an answer to go
// all the way through the runtime and Core:
//
//   - asked a question that mentions an assignment, with assignment_list
//     offered and no tool result yet, it calls assignment_list with {};
//   - otherwise it answers "Answer: " and the question's first 200
//     characters, saying how many tool results it was given.
//
// Every answer carries usage, counted at four bytes a token.
func DefaultResponder(req ChatRequest) ChatResponse {
	question := lastUserText(req.Messages)
	results := 0
	for _, m := range req.Messages {
		if m.Role == "tool" {
			results++
		}
	}
	lastIsUser := len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "user"
	if lastIsUser && results == 0 && offered(req, "assignment_list") &&
		strings.Contains(strings.ToLower(question), "assignment") {
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

// lastUserText is the newest user message's text: the question, even
// after the tool results that followed it.
func lastUserText(msgs []ChatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Text()
		}
	}
	return ""
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
