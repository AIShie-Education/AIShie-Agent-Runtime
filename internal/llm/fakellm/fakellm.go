// Package fakellm is an OpenAI Chat Completions server for end-to-end tests
// and local runs: the runtime's openai_chat adapter talks to it as to any
// compatible server. It answers from a script (a queue of responses) and
// then from a Responder; DefaultResponder is a small rule-based model good
// enough for an answer to go all the way through Core.
//
// It checks what OpenAI checks of a conversation's shape (every tool
// message answers a call just made, tool_choice only with tools), so that
// a request a real provider would refuse is refused here too.
package fakellm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ChatRequest is what the server reads of a request.
type ChatRequest struct {
	Model               string          `json:"model"`
	Messages            []ChatMessage   `json:"messages"`
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`

	// Header is the HTTP request's headers, and Raw its body as it came,
	// for what the fields above do not hold.
	Header http.Header     `json:"-"`
	Raw    json.RawMessage `json:"-"`
}

// StreamOptions are a streamed request's: include_usage asks for the usage
// in a last chunk of its own.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatMessage is one message, in a request or a response. Content is a
// string, nil (null), or in a request the list of parts a message with
// files carries; Text reads either.
type ChatMessage struct {
	Role             string     `json:"role"`
	Content          any        `json:"content"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
}

// Text is the message's text: the string content, or its text parts
// joined.
func (m ChatMessage) Text() string {
	switch c := m.Content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			if part, ok := p.(map[string]any); ok && part["type"] == "text" {
				if s, ok := part["text"].(string); ok {
					if b.Len() > 0 {
						b.WriteString("\n")
					}
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// ToolCall is a call in an assistant message.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is a call's function: its name and its arguments as a JSON
// string.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is a declared tool.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is a declared tool's function: its name, description and
// JSON Schema.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

// ChatResponse is one answer. The server fills what is left empty: the
// id, object, created, model, each choice's index and role.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	// Usage nil sends none, as some servers do.
	Usage *Usage `json:"usage,omitempty"`

	// Status, when set and not 200, is the answer's HTTP status, with
	// Error as its body: a 429, a 500, a 400 context_length_exceeded.
	Status int    `json:"-"`
	Error  *Error `json:"error,omitempty"`
	// Header is added to the answer: Retry-After, say.
	Header http.Header `json:"-"`
	// Delay holds the answer back, or until the caller gives up.
	Delay time.Duration `json:"-"`
	// Every is the pause between the pieces of a streamed answer; the
	// server's StreamEvery when zero.
	Every time.Duration `json:"-"`
	// Stall holds a streamed answer back after its first piece of text, a
	// model stopping part way through, and one not streamed before it is
	// sent: that long, or until the caller gives up.
	Stall time.Duration `json:"-"`
}

// Choice is one completion.
type Choice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// Usage is the token counts. A model that thinks counts its thinking in
// CompletionTokens too, and says how much of them it was in
// CompletionTokensDetails, as OpenAI's and DeepSeek's do.
type Usage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// CompletionTokensDetails is what the completion tokens were.
type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Error is an error body's error.
type Error struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
}

// Responder answers a request the script does not.
type Responder func(req ChatRequest) ChatResponse

// Server is the fake. It is safe for concurrent use.
type Server struct {
	responder Responder
	// every is the pause between a streamed answer's pieces
	// (StreamEvery).
	every atomic.Int64

	mu       sync.Mutex
	script   []ChatResponse
	requests []ChatRequest
	served   int
	ts       *httptest.Server
	// gaveUp are when callers went away from answers not yet whole.
	gaveUp []time.Time
}

// New serves with r once the script is spent; nil r answers only from the
// script, and a request past its end gets a 500.
func New(r Responder) *Server { return &Server{responder: r} }

// NewScript answers with responses, in order, and then with 500s.
func NewScript(responses ...ChatResponse) *Server {
	s := New(nil)
	s.Enqueue(responses...)
	return s
}

// Enqueue adds responses to the script. The script is played before the
// Responder is asked.
func (s *Server) Enqueue(responses ...ChatResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = append(s.script, responses...)
}

// Remaining is the number of scripted responses not yet sent.
func (s *Server) Remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.script)
}

// Requests are copies of the requests received, in order, refused ones
// included.
func (s *Server) Requests() []ChatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ChatRequest, len(s.requests))
	for i, r := range s.requests {
		r.Header = r.Header.Clone()
		r.Raw = append(json.RawMessage(nil), r.Raw...)
		// Everything else is decoded afresh, so that no slice is shared.
		var fresh ChatRequest
		if json.Unmarshal(r.Raw, &fresh) == nil {
			fresh.Header, fresh.Raw = r.Header, r.Raw
			r = fresh
		}
		out[i] = r
	}
	return out
}

// GaveUp are when callers went away from answers not yet whole, in order:
// one held back (Delay, Stall), or a stream between its pieces.
func (s *Server) GaveUp() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.gaveUp...)
}

// wait waits d, or until r's caller gives up, which it notes, and reports
// as false.
func (s *Server) wait(r *http.Request, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		s.mu.Lock()
		s.gaveUp = append(s.gaveUp, time.Now())
		s.mu.Unlock()
		return false
	}
}

// StreamEvery sets the pause between the pieces of a streamed answer
// (stream: true), unless its response sets Every: none by default.
func (s *Server) StreamEvery(d time.Duration) *Server {
	s.every.Store(int64(d))
	return s
}

// Start serves s on a loopback port until Close.
func (s *Server) Start() *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ts == nil {
		s.ts = httptest.NewServer(s.Handler())
	}
	return s
}

// URL is the base URL an adapter is given once s is started (it ends in
// /v1), and "" before.
func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ts == nil {
		return ""
	}
	return s.ts.URL + "/v1"
}

// Close stops a started server.
func (s *Server) Close() {
	s.mu.Lock()
	ts := s.ts
	s.ts = nil
	s.mu.Unlock()
	if ts != nil {
		ts.Close()
	}
}

// maxBody bounds a request, as a provider's does.
const maxBody = 32 << 20

// Handler serves POST …/chat/completions (under any prefix, /v1 or none)
// and GET …/models.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, Error{Message: "use POST", Type: "invalid_request_error"})
				return
			}
			s.complete(w, r)
		case strings.HasSuffix(r.URL.Path, "/models") && r.Method == http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{"object": "list",
				"data": []any{map[string]any{"id": "fake-model", "object": "model", "owned_by": "fakellm"}}})
		default:
			writeError(w, http.StatusNotFound, Error{Message: "no such endpoint: " + r.URL.Path, Type: "invalid_request_error"})
		}
	})
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		writeError(w, http.StatusBadRequest, Error{Message: "the body could not be read", Type: "invalid_request_error"})
		return
	}
	var req ChatRequest
	decodeErr := json.Unmarshal(body, &req)
	req.Header = r.Header.Clone()
	req.Raw = body

	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()

	if decodeErr != nil {
		writeError(w, http.StatusBadRequest, Error{Message: "the body is not a chat request: " + decodeErr.Error(), Type: "invalid_request_error"})
		return
	}
	if msg := Validate(req); msg != "" {
		writeError(w, http.StatusBadRequest, Error{Message: msg, Type: "invalid_request_error", Code: "invalid_request"})
		return
	}

	resp, ok := s.next(req)
	if !ok {
		writeError(w, http.StatusInternalServerError, Error{Message: "fakellm: the script is spent and there is no responder", Type: "server_error"})
		return
	}
	if resp.Delay > 0 && !s.wait(r, resp.Delay) {
		return
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	if resp.Status != 0 && resp.Status != http.StatusOK {
		e := Error{Message: http.StatusText(resp.Status), Type: "server_error"}
		if resp.Error != nil {
			e = *resp.Error
		}
		writeError(w, resp.Status, e)
		return
	}
	if req.Stream {
		s.stream(w, r, req, resp)
		return
	}
	if resp.Stall > 0 && !s.wait(r, resp.Stall) {
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// next is the script's next response, or the responder's, filled in.
func (s *Server) next(req ChatRequest) (ChatResponse, bool) {
	s.mu.Lock()
	var resp ChatResponse
	switch {
	case len(s.script) > 0:
		resp = s.script[0]
		s.script = s.script[1:]
	case s.responder != nil:
		s.mu.Unlock()
		resp = s.responder(req)
		s.mu.Lock()
	default:
		s.mu.Unlock()
		return ChatResponse{}, false
	}
	s.served++
	n := s.served
	s.mu.Unlock()

	if resp.ID == "" {
		resp.ID = fmt.Sprintf("chatcmpl-fake-%d", n)
	}
	if resp.Object == "" {
		resp.Object = "chat.completion"
	}
	if resp.Created == 0 {
		resp.Created = time.Now().Unix()
	}
	if resp.Model == "" {
		resp.Model = req.Model
	}
	resp.Choices = append([]Choice(nil), resp.Choices...)
	for i := range resp.Choices {
		resp.Choices[i].Index = i
		if resp.Choices[i].Message.Role == "" {
			resp.Choices[i].Message.Role = "assistant"
		}
	}
	return resp, true
}

// Validate is what the fake refuses, as OpenAI refuses it: a request with
// no model or no messages, stream_options without a stream, tool_choice or
// parallel_tool_calls without tools, and a conversation whose tool messages
// do not answer, one each, the calls of the assistant message just before
// them. It returns "" for a request it takes.
func Validate(req ChatRequest) string {
	switch {
	case req.Model == "":
		return "model is required"
	case len(req.Messages) == 0:
		return "messages must not be empty"
	case req.StreamOptions != nil && !req.Stream:
		return "stream_options is only allowed when stream is true"
	case len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" && len(req.Tools) == 0:
		return "tool_choice is only allowed when tools are given"
	case req.ParallelToolCalls != nil && len(req.Tools) == 0:
		return "parallel_tool_calls is only allowed when tools are given"
	}
	pending := map[string]bool{}
	for i, m := range req.Messages {
		switch m.Role {
		case "tool":
			if !pending[m.ToolCallID] {
				return fmt.Sprintf("messages[%d]: a tool message must answer a tool call of the assistant message before it (tool_call_id %q)", i, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
			continue
		case "system", "developer", "user", "assistant":
		default:
			return fmt.Sprintf("messages[%d]: unknown role %q", i, m.Role)
		}
		if len(pending) > 0 {
			return fmt.Sprintf("messages[%d]: an assistant message with tool_calls must be followed by tool messages answering each tool_call_id", i)
		}
		for _, c := range m.ToolCalls {
			if m.Role != "assistant" || c.ID == "" {
				return fmt.Sprintf("messages[%d]: tool calls belong to assistant messages and need an id", i)
			}
			pending[c.ID] = true
		}
	}
	if len(pending) > 0 {
		return "the last assistant message's tool calls are not all answered"
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, e Error) {
	writeJSON(w, status, map[string]any{"error": e})
}
