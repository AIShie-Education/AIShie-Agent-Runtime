package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/httpx"
)

// wireResponse is generateContent's answer, as far as the runtime reads it.
type wireResponse struct {
	Candidates     []wireCandidate     `json:"candidates"`
	PromptFeedback *wirePromptFeedback `json:"promptFeedback"`
	UsageMetadata  json.RawMessage     `json:"usageMetadata"`
	ModelVersion   string              `json:"modelVersion"`
	ResponseID     string              `json:"responseId"`
}

type wireCandidate struct {
	Content      *wireContentIn `json:"content"`
	FinishReason string         `json:"finishReason"`
}

// wireContentIn keeps each part as it came, so that a part the adapter
// carries verbatim is carried byte for byte.
type wireContentIn struct {
	Parts []json.RawMessage `json:"parts"`
}

type wirePromptFeedback struct {
	BlockReason string `json:"blockReason"`
}

type wirePartIn struct {
	Text             *string             `json:"text"`
	Thought          bool                `json:"thought"`
	ThoughtSignature string              `json:"thoughtSignature"`
	FunctionCall     *wireFunctionCallIn `json:"functionCall"`
}

type wireFunctionCallIn struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type wireUsage struct {
	PromptTokenCount        count `json:"promptTokenCount"`
	CachedContentTokenCount count `json:"cachedContentTokenCount"`
	CandidatesTokenCount    count `json:"candidatesTokenCount"`
	ToolUsePromptTokenCount count `json:"toolUsePromptTokenCount"`
	ThoughtsTokenCount      count `json:"thoughtsTokenCount"`
}

// count is a token count. Proto3's JSON writes 32-bit counts as numbers and
// 64-bit ones as strings, so either is read.
type count int64

func (c *count) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		*c = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("a token count is not an integer: %w", err)
	}
	*c = count(n)
	return nil
}

// promptBlocked prefixes RawStop when there was no candidate because the
// prompt itself was blocked: what follows is promptFeedback.blockReason, not
// a finishReason.
const promptBlocked = "promptFeedback:"

// decodeResponse translates a 2xx answer. Only the first candidate is read;
// the runtime never asks for more.
func decodeResponse(body []byte, maker string) (*llm.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, httpx.DecodeError(err)
	}
	usage, err := usageOf(w.UsageMetadata)
	if err != nil {
		return nil, httpx.DecodeError(err)
	}
	out := &llm.Response{Parts: []llm.Part{}, Usage: usage, Model: w.ModelVersion, RequestID: w.ResponseID}
	if len(w.Candidates) == 0 {
		out.Stop = llm.StopError
		if w.PromptFeedback != nil && w.PromptFeedback.BlockReason != "" {
			out.Stop, out.RawStop = llm.StopContentFilter, promptBlocked+w.PromptFeedback.BlockReason
		}
		return out, nil
	}
	c := w.Candidates[0]
	if c.Content != nil {
		for _, raw := range c.Content.Parts {
			p, err := partOf(raw, maker)
			if err != nil {
				return nil, httpx.DecodeError(err)
			}
			out.Parts = append(out.Parts, p)
		}
	}
	out.Stop, out.RawStop = stopOf(c.FinishReason), c.FinishReason
	return out, nil
}

// partOf translates one part. A signature is kept on the part it came
// with, stamped with maker.
func partOf(raw json.RawMessage, maker string) (llm.Part, error) {
	var in wirePartIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return llm.Part{}, fmt.Errorf("a part: %w", err)
	}
	switch {
	case in.Thought:
		text := ""
		if in.Text != nil {
			text = *in.Text
		}
		return verbatim(raw, text, maker)
	case in.FunctionCall != nil:
		fc := in.FunctionCall
		args, argsErr := argsOf(fc.Args)
		p := llm.Part{Type: llm.PartToolCall, ID: fc.ID, Name: fc.Name, Args: args, ArgsError: argsErr}
		keep(&p, opaque{ThoughtSignature: in.ThoughtSignature, ID: fc.ID}, maker)
		return p, nil
	case in.Text != nil:
		p := llm.Part{Type: llm.PartText, Text: *in.Text}
		keep(&p, opaque{ThoughtSignature: in.ThoughtSignature}, maker)
		return p, nil
	}
	// A kind of part the runtime does not read (code execution, an image)
	// is carried verbatim like a thought: its maker sees again the history
	// it wrote, signature and all, and no other adapter reads it.
	return verbatim(raw, "", maker)
}

// verbatim is a reasoning part holding a Gemini part as it came, without
// its whitespace.
func verbatim(raw json.RawMessage, text, maker string) (llm.Part, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return llm.Part{}, fmt.Errorf("a part: %w", err)
	}
	return llm.Part{Type: llm.PartReasoning, Text: text, Maker: maker, Opaque: buf.Bytes()}, nil
}

// keep stamps o on p when there is anything in it.
func keep(p *llm.Part, o opaque, maker string) {
	if o == (opaque{}) {
		return
	}
	b, _ := json.Marshal(o) // two strings always encode
	p.Maker, p.Opaque = maker, b
}

// argsOf is a call's arguments as the runtime holds them, always an object
// (rule 1): missing or null arguments are {}, and anything else that is not
// an object is kept in ArgsError, for the loop to answer with an error.
func argsOf(raw json.RawMessage) (json.RawMessage, string) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return json.RawMessage("{}"), ""
	}
	if trimmed[0] == '{' {
		var buf bytes.Buffer
		if json.Compact(&buf, trimmed) == nil {
			return buf.Bytes(), ""
		}
	}
	return json.RawMessage("{}"), string(trimmed)
}

// stopOf maps a finishReason (§3.4). A function call is told from the
// parts, by Normalize (rule 3), since finishReason stays STOP. A reason not
// listed here is error, never end: text the provider did not say was
// finished is not posted as an answer.
func stopOf(reason string) llm.Stop {
	switch reason {
	case "STOP":
		return llm.StopEnd
	case "MAX_TOKENS":
		return llm.StopMaxTokens
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION":
		return llm.StopContentFilter
	case "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "TOO_MANY_TOOL_CALLS":
		return llm.StopToolError
	}
	// MISSING_THOUGHT_SIGNATURE (a bug of the runtime's), MALFORMED_RESPONSE,
	// OTHER, IMAGE_OTHER, NO_IMAGE, FINISH_REASON_UNSPECIFIED, none at all,
	// or one newer than this adapter.
	return llm.StopError
}

// usageOf maps usageMetadata (§3.5). Input is the prompt and any tool-use
// prompt; the cached tokens are within the prompt, as Google documents
// promptTokenCount. Output counts thoughts beside the candidates, since
// both are billed as output (the handout marks this [UNVERIFIED]).
func usageOf(raw json.RawMessage) (llm.Usage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return llm.Usage{}, nil
	}
	var u wireUsage
	if err := json.Unmarshal(trimmed, &u); err != nil {
		return llm.Usage{}, fmt.Errorf("usageMetadata: %w", err)
	}
	return llm.Usage{
		Input:     int64(u.PromptTokenCount + u.ToolUsePromptTokenCount),
		CacheRead: int64(u.CachedContentTokenCount),
		Output:    int64(u.CandidatesTokenCount + u.ThoughtsTokenCount),
		Reasoning: int64(u.ThoughtsTokenCount),
		Raw:       raw,
	}, nil
}
