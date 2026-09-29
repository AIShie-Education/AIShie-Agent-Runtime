package anthropic

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/httpx"
)

func httpResponse(body []byte) *httpx.Response {
	h := http.Header{}
	h.Set("Request-Id", "req_011CSHoEeqs5C35K2UUqR7Fy")
	return &httpx.Response{Status: http.StatusOK, Header: h, Body: body}
}

func encodeResponse(t *testing.T, r *llm.Response) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestResponseGolden holds the translation of Anthropic's answers to
// golden files: testdata/golden/response_*.anthropic.json in,
// response_*.internal.json out.
func TestResponseGolden(t *testing.T) {
	for _, name := range []string{
		"text",                // end_turn; cache fields null, as some servers send them
		"parallel_tools",      // two calls in one turn
		"thinking",            // thinking and redacted_thinking kept verbatim, with their maker
		"cache_usage",         // input counts cached tokens; the split stays in raw
		"max_tokens_cut_call", // the call cut off at max_tokens is dropped, the complete one kept
		"refusal",             // a refused turn's calls never run
		"bad_input",           // input that is not an object, and a call with no id and no input
	} {
		t.Run(name, func(t *testing.T) {
			a := newAdapter(t, llm.Config{})
			r := httpResponse(readGolden(t, "response_"+name+".anthropic.json"))
			if name == "bad_input" {
				r.Header = http.Header{} // no request-id: the message id stands in
			}
			resp, err := a.decodeResponse(r)
			if err != nil {
				t.Fatalf("decodeResponse: %v", err)
			}
			checkGolden(t, "response_"+name+".internal.json", encodeResponse(t, resp))
		})
	}
}

// TestStopReasonsGolden maps every stop reason Anthropic documents, and
// ones it does not, in testdata/golden/response_stop_reasons.internal.json.
func TestStopReasonsGolden(t *testing.T) {
	a := newAdapter(t, llm.Config{})
	out := map[string]*llm.Response{}
	for _, reason := range []string{
		"end_turn", "stop_sequence", "tool_use", "max_tokens", "refusal",
		"model_context_window_exceeded", "pause_turn", "", "a_reason_from_the_future",
	} {
		content := `[{"type":"text","text":"Partial answer"}]`
		if reason == "tool_use" {
			content = `[{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_01","name":"grade_list","input":{}}]`
		}
		stop := `"` + reason + `"`
		if reason == "" {
			stop = "null"
		}
		body := `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":` + content +
			`,"stop_reason":` + stop + `,"usage":{"input_tokens":10,"output_tokens":2}}`
		resp, err := a.decodeResponse(httpResponse([]byte(body)))
		if err != nil {
			t.Fatalf("%q: %v", reason, err)
		}
		out[reason] = resp
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "response_stop_reasons.internal.json", b)
}

func TestStops(t *testing.T) {
	cases := []struct {
		reason string
		parts  string
		want   llm.Stop
	}{
		{"end_turn", `[{"type":"text","text":"Hi"}]`, llm.StopEnd},
		{"stop_sequence", `[{"type":"text","text":"Hi"}]`, llm.StopEnd},
		{"tool_use", `[{"type":"tool_use","id":"t","name":"x","input":{}}]`, llm.StopToolCalls},
		// Rule 3: a call means tool_calls, whatever the reason said.
		{"end_turn", `[{"type":"tool_use","id":"t","name":"x","input":{}}]`, llm.StopToolCalls},
		// tool_use with nothing to call cannot go on.
		{"tool_use", `[{"type":"text","text":"Hi"}]`, llm.StopError},
		{"max_tokens", `[{"type":"text","text":"Hi"}]`, llm.StopMaxTokens},
		{"max_tokens", `[{"type":"text","text":"Hi"},{"type":"tool_use","id":"t","name":"x","input":{"a":"b"}}]`, llm.StopMaxTokens},
		{"max_tokens", `[{"type":"tool_use","id":"t","name":"x","input":{}},{"type":"text","text":"and then"}]`, llm.StopToolCalls},
		{"refusal", `[{"type":"tool_use","id":"t","name":"x","input":{}}]`, llm.StopRefusal},
		{"model_context_window_exceeded", `[]`, llm.StopContextOverflow},
		// The context ran out as the model wrote a call: it is cut off like
		// one at max_tokens, and a complete call before it still runs.
		{"model_context_window_exceeded", `[{"type":"text","text":"Hi"},{"type":"tool_use","id":"t","name":"x","input":{"a":"b"}}]`, llm.StopContextOverflow},
		{"model_context_window_exceeded", `[{"type":"tool_use","id":"t","name":"x","input":{}},{"type":"tool_use","id":"u","name":"x","input":{"a":"b"}}]`, llm.StopToolCalls},
		{"pause_turn", `[{"type":"server_tool_use","id":"s","name":"web_search","input":{}}]`, llm.StopError},
		{"something_new", `[{"type":"text","text":"Hi"}]`, llm.StopError},
	}
	a := newAdapter(t, llm.Config{})
	for _, c := range cases {
		body := `{"type":"message","content":` + c.parts + `,"stop_reason":"` + c.reason + `"}`
		resp, err := a.decodeResponse(httpResponse([]byte(body)))
		if err != nil {
			t.Fatalf("%s %s: %v", c.reason, c.parts, err)
		}
		if resp.Stop != c.want || resp.RawStop != c.reason {
			t.Errorf("%s %s: stop %q, raw %q; want %q, %q", c.reason, c.parts, resp.Stop, resp.RawStop, c.want, c.reason)
		}
		if calls := resp.ToolCalls(); resp.Stop != llm.StopToolCalls && len(calls) != 0 {
			t.Errorf("%s %s: stop %q with calls %+v left to run", c.reason, c.parts, resp.Stop, calls)
		}
	}
}

func TestUsage(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want llm.Usage
	}{
		{"all fields", `{"input_tokens":57,"cache_creation_input_tokens":1290,"cache_read_input_tokens":10240,"output_tokens":12}`,
			llm.Usage{Input: 57 + 1290 + 10240, CacheRead: 10240, CacheWrite: 1290, Output: 12}},
		{"no cache fields", `{"input_tokens":57,"output_tokens":12}`, llm.Usage{Input: 57, Output: 12}},
		{"null cache fields", `{"input_tokens":57,"cache_creation_input_tokens":null,"cache_read_input_tokens":null,"output_tokens":12}`,
			llm.Usage{Input: 57, Output: 12}},
		{"no usage", `null`, llm.Usage{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := usage(json.RawMessage(c.raw))
			raw := got.Raw
			if got.Input != c.want.Input || got.CacheRead != c.want.CacheRead || got.CacheWrite != c.want.CacheWrite ||
				got.Output != c.want.Output || got.Reasoning != 0 || got.Estimated {
				t.Errorf("usage = %+v, want %+v", got, c.want)
			}
			if c.raw != "null" && string(raw) != c.raw {
				t.Errorf("raw = %s, want %s verbatim", raw, c.raw)
			}
			if c.raw == "null" && raw != nil {
				t.Errorf("raw = %s, want none", raw)
			}
		})
	}
}

// A server that answers 2xx with an error body is refused as the error it
// is, and one that answers something unreadable is a server error.
func TestErrorBodiesWithA2xx(t *testing.T) {
	a := newAdapter(t, llm.Config{})
	cases := []struct {
		body string
		kind llm.ErrorKind
	}{
		{`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, llm.ErrOverloaded},
		{`{"error":{"type":"rate_limit_error","message":"slow down"}}`, llm.ErrRateLimited},
		{`{"error":{"type":"api_error","message":"Internal server error"}}`, llm.ErrServer},
		{`{"error":{"message":"upstream: prompt is too long: 250000 tokens > 200000 maximum","code":400}}`, llm.ErrContextOverflow},
		{`<html>Bad gateway</html>`, llm.ErrServer},
		{`{"type":"message","content":[{"type":"text","text":1}]}`, llm.ErrServer},
	}
	for _, c := range cases {
		_, err := a.decodeResponse(httpResponse([]byte(c.body)))
		var le *llm.Error
		if !errors.As(err, &le) || le.Kind != c.kind {
			t.Errorf("%s: err = %v, want kind %s", c.body, err, c.kind)
		}
	}
}

func TestRequestID(t *testing.T) {
	a := newAdapter(t, llm.Config{})
	body := []byte(`{"id":"msg_01","type":"message","content":[{"type":"text","text":"Hi"}],"stop_reason":"end_turn"}`)
	resp, err := a.decodeResponse(httpResponse(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.RequestID != "req_011CSHoEeqs5C35K2UUqR7Fy" {
		t.Errorf("request id %q, want the request-id header's", resp.RequestID)
	}
	resp, err = a.decodeResponse(&httpx.Response{Status: 200, Header: http.Header{}, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if resp.RequestID != "msg_01" {
		t.Errorf("request id %q, want the message id without the header", resp.RequestID)
	}
}

// Blocks of features the runtime does not use are passed over, and empty
// text is not a part.
func TestUnknownBlocksArePassedOver(t *testing.T) {
	a := newAdapter(t, llm.Config{})
	body := `{"type":"message","content":[{"type":"text","text":""},{"type":"server_tool_use","id":"s","name":"web_search","input":{}},` +
		`{"type":"web_search_tool_result","tool_use_id":"s","content":[]},{"type":"text","text":"Done."}],"stop_reason":"end_turn"}`
	resp, err := a.decodeResponse(httpResponse([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Parts) != 1 || resp.Text() != "Done." {
		t.Errorf("parts = %+v, want the one text", resp.Parts)
	}
	if resp.Stop != llm.StopEnd {
		t.Errorf("stop = %q, want end", resp.Stop)
	}
}
