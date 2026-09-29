package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// DefaultProtocol is the MCP revision the runtime pins (§1.2, §4). Core
// answers initialize for it with it.
const DefaultProtocol = "2025-11-25"

// MCPOptions configure an MCPCaller.
type MCPOptions struct {
	// BaseURL is Core's base, such as https://lms.example.edu; the endpoint
	// is BaseURL+"/mcp".
	BaseURL string
	// Token is the agent's token (ais_…). It goes in the Authorization
	// header and nowhere else: never in an error.
	Token string
	// Protocol is the pinned MCP revision; DefaultProtocol when empty.
	Protocol string
	// HTTPClient carries the requests; one with DefaultTimeout when nil. A
	// copy of it is used that follows no redirect.
	HTTPClient *http.Client
	// ClientName and ClientVersion are the clientInfo sent with initialize;
	// aishie-runtime and the build's version when empty.
	ClientName, ClientVersion string
	// MaxResponseBytes bounds one answer; DefaultMaxResponseBytes when 0.
	MaxResponseBytes int64
}

// MCPCaller is a Caller over MCP: JSON-RPC 2.0 on Core's stateless
// streamable HTTP (§1.2), one message per request. It is written here rather
// than taken from the SDK, whose client folds a 401 and a 429 into generic
// errors: the runtime stops an agent on the one and waits out the other.
//
// It initializes when first used, and on each call after until that
// succeeds. Core is stateless and keeps nothing between requests, but
// initialize brings Core's instructions and confirms the pinned revision.
// Initialize and notifications/initialized count against Core's per-actor
// limit, once per MCPCaller, without passing through the agent's bucket.
// It is safe for concurrent use.
type MCPCaller struct {
	endpoint string
	token    string
	protocol string
	client   *http.Client
	name     string
	version  string
	max      int64

	ids atomic.Int64
	// initLock is held while initializing: a channel, so that a caller
	// waiting for another's initialize can give up when its context ends.
	initLock chan struct{}
	ready    atomic.Bool

	mu           sync.Mutex
	instructions string
	negotiated   string
}

// NewMCPCaller returns a caller for one agent. It makes no request until
// the first call.
func NewMCPCaller(o MCPOptions) *MCPCaller {
	c := &MCPCaller{
		endpoint: strings.TrimRight(o.BaseURL, "/") + "/mcp",
		token:    o.Token,
		protocol: o.Protocol,
		client:   withoutRedirects(o.HTTPClient),
		name:     o.ClientName,
		version:  o.ClientVersion,
		max:      o.MaxResponseBytes,
		initLock: make(chan struct{}, 1),
	}
	if c.protocol == "" {
		c.protocol = DefaultProtocol
	}
	if c.name == "" {
		c.name = clientName
	}
	if c.version == "" {
		c.version = defaultVersion()
	}
	if c.max <= 0 {
		c.max = DefaultMaxResponseBytes
	}
	return c
}

// Instructions are what Core told the agent at initialize; "" before it.
func (c *MCPCaller) Instructions() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.instructions
}

// Protocol is the revision Core answered initialize with, which is always
// the pinned one; "" before initialize.
func (c *MCPCaller) Protocol() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.negotiated
}

// Initialize sends initialize and then notifications/initialized, unless
// that has been done. Call does it itself; this is for checking a token and
// the revision up front. Its errors are Call's.
func (c *MCPCaller) Initialize(ctx context.Context) error {
	if c.ready.Load() {
		return nil
	}
	select {
	case c.initLock <- struct{}{}:
	case <-ctx.Done():
		return &TransientError{Err: ctx.Err()}
	}
	defer func() { <-c.initLock }()
	if c.ready.Load() {
		return nil
	}
	params := map[string]any{
		"protocolVersion": c.protocol,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": c.name, "version": c.version},
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		Instructions    string `json:"instructions"`
	}
	if err := c.request(ctx, "initialize", params, &res); err != nil {
		return err
	}
	if res.ProtocolVersion != c.protocol {
		return &ProtocolError{Message: fmt.Sprintf("Core answered initialize with MCP revision %q; the runtime pins %q",
			quote(res.ProtocolVersion, c.token, 40), c.protocol)}
	}
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		return err
	}
	c.mu.Lock()
	c.instructions = res.Instructions
	c.negotiated = res.ProtocolVersion
	c.mu.Unlock()
	c.ready.Store(true)
	return nil
}

// Call is tools/call. Core's answer is the envelope, from structuredContent,
// else from the text of content[0] (§2.1); isError says only that the status
// is neither executed nor proposed, and is still an envelope and a nil
// error. The errors are Caller's.
func (c *MCPCaller) Call(ctx context.Context, tool string, args json.RawMessage) (*Envelope, error) {
	if err := c.Initialize(ctx); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	params := struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{tool, args}
	if err := c.request(ctx, "tools/call", params, &res); err != nil {
		return nil, err
	}
	if env, ok := decodeEnvelope(res.StructuredContent); ok {
		return env, nil
	}
	if len(res.Content) > 0 {
		if env, ok := decodeEnvelope([]byte(res.Content[0].Text)); ok {
			return env, nil
		}
	}
	return nil, &ProtocolError{Message: tool + ": the result holds no envelope"}
}

// MCPTool is one tool as tools/list describes it.
type MCPTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

// maxListPages bounds tools/list's paging: Core sends one page.
const maxListPages = 100

// ListTools is tools/list, every page. The runtime's catalogue comes from
// GET /v1/tools (FetchCatalogue); this is for checking what an agent is
// offered over MCP.
func (c *MCPCaller) ListTools(ctx context.Context) ([]MCPTool, error) {
	if err := c.Initialize(ctx); err != nil {
		return nil, err
	}
	var tools []MCPTool
	cursor := ""
	for range maxListPages {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Tools      []MCPTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := c.request(ctx, "tools/list", params, &page); err != nil {
			return nil, err
		}
		tools = append(tools, page.Tools...)
		if page.NextCursor == "" {
			return tools, nil
		}
		if page.NextCursor == cursor {
			return nil, &ProtocolError{Message: "tools/list: the next page's cursor is this page's"}
		}
		cursor = page.NextCursor
	}
	return nil, &ProtocolError{Message: fmt.Sprintf("tools/list: more than %d pages", maxListPages)}
}

// rpcRequest is one JSON-RPC message. A notification has no id.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResponse is an answer; a message with a method is the server's own
// request or notification instead.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

// request sends one request and decodes its result into out.
func (c *MCPCaller) request(ctx context.Context, method string, params, out any) error {
	id := c.ids.Add(1)
	body, err := encode(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return &ProtocolError{Message: fmt.Sprintf("%s: the request does not encode: %v", method, err)}
	}
	resp, err := c.post(ctx, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	result, err := c.answer(resp, id)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(result, out); err != nil {
		return &ProtocolError{Message: fmt.Sprintf("%s: the result does not decode: %v", method, err)}
	}
	return nil
}

// notify sends a notification, which Core accepts with a 202 and no body.
func (c *MCPCaller) notify(ctx context.Context, method string) error {
	body, err := encode(rpcRequest{JSONRPC: "2.0", Method: method})
	if err != nil {
		return &ProtocolError{Message: fmt.Sprintf("%s: %v", method, err)}
	}
	resp, err := c.post(ctx, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		drain(resp)
		return nil
	}
	_, err = c.answer(resp, 0)
	return err
}

// encode writes v as JSON without escaping HTML, so that the arguments go
// out as the caller wrote them.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func (c *MCPCaller) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &ProtocolError{Message: "the request cannot be made: " + redact(err.Error(), c.token)}
	}
	if !validHeaderValue(c.token) {
		return nil, &ProtocolError{Message: "the token cannot be sent in a header"}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", c.protocol)
	req.Header.Set("User-Agent", userAgent(c.name, c.version))
	resp, err := forCall(ctx, c.client).Do(req)
	if err != nil {
		return nil, sendError(ctx, err, c.token)
	}
	if redirected(req, resp) {
		drain(resp)
		return nil, errRedirected(resp.StatusCode)
	}
	return resp, nil
}

// answer reads the answer to request id and returns its result. The
// caller closes the body.
func (c *MCPCaller) answer(resp *http.Response, id int64) (json.RawMessage, error) {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		drain(resp)
		return nil, ErrUnauthenticated
	case http.StatusTooManyRequests:
		return nil, rateLimited(resp, c.max)
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode <= 299
	if ok && isEventStream(resp.Header.Get("Content-Type")) {
		return c.stream(resp, id)
	}
	body, err := readBody(resp, c.max)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout:
		return nil, serverError(resp.StatusCode, body, c.token)
	case !ok:
		var m rpcResponse
		if json.Unmarshal(body, &m) == nil && m.Error != nil {
			return nil, c.rpcError(m.Error, resp.StatusCode)
		}
		return nil, &ProtocolError{Message: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, said(body, c.token))}
	case len(bytes.TrimSpace(body)) == 0:
		return nil, &ProtocolError{Message: fmt.Sprintf("HTTP %d with no answer", resp.StatusCode)}
	}
	return c.match(body, id, true)
}

// match reads body as the answer to request id. whole says the body was
// read to its end, so that JSON that stops short was cut short.
func (c *MCPCaller) match(body []byte, id int64, whole bool) (json.RawMessage, error) {
	var m rpcResponse
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&m); err != nil {
		if whole && errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, &TransientError{Status: http.StatusOK, Err: errors.New("the answer was cut short")}
		}
		return nil, &ProtocolError{Message: "the answer is not JSON-RPC: " + quote(err.Error(), c.token, 200)}
	}
	if m.Method != "" {
		return nil, &ProtocolError{Message: "the answer is a request of the server's, not an answer: " + quote(m.Method, c.token, 64)}
	}
	if m.Error != nil {
		return nil, c.rpcError(m.Error, 0)
	}
	if !sameID(m.ID, id) {
		return nil, &ProtocolError{Message: fmt.Sprintf("the answer is to request %s, not %d", quote(string(m.ID), c.token, 40), id)}
	}
	if len(m.Result) == 0 || bytes.Equal(m.Result, []byte("null")) {
		return nil, &ProtocolError{Message: "the answer has neither a result nor an error"}
	}
	return m.Result, nil
}

func (c *MCPCaller) rpcError(e *rpcError, status int) *ProtocolError {
	msg := quote(e.Message, c.token, 400)
	if status != 0 {
		msg = fmt.Sprintf("HTTP %d: %s", status, msg)
	}
	return &ProtocolError{Code: e.Code, Message: msg}
}

func sameID(raw json.RawMessage, id int64) bool {
	return string(bytes.TrimSpace(raw)) == strconv.FormatInt(id, 10)
}

func isEventStream(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "text/event-stream"
}

// stream reads a text/event-stream answer up to the first event whose data
// is the answer to request id; other events (a priming event with no data,
// a server's notification) are passed over. Core sends no streams (§1.2),
// but the protocol lets a server answer so.
func (c *MCPCaller) stream(resp *http.Response, id int64) (json.RawMessage, error) {
	sc := bufio.NewScanner(&boundedReader{r: resp.Body, max: c.max})
	sc.Buffer(make([]byte, 0, 64<<10), int(min(c.max, 1<<30)))
	var data strings.Builder
	dispatch := func() (json.RawMessage, bool, error) {
		defer data.Reset()
		if data.Len() == 0 {
			return nil, false, nil
		}
		msg := strings.TrimSuffix(data.String(), "\n")
		var m rpcResponse
		// A request of the server's own may carry an id equal to this
		// request's: ids are each side's own. It is passed over.
		if json.Unmarshal([]byte(msg), &m) != nil || m.Method != "" {
			return nil, false, nil
		}
		// An error with a null id is to a request the server could not
		// read, and there is only one here.
		unread := m.Error != nil && (len(m.ID) == 0 || string(m.ID) == "null")
		if !sameID(m.ID, id) && !unread {
			return nil, false, nil
		}
		result, err := c.match([]byte(msg), id, false)
		return result, true, err
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if result, done, err := dispatch(); done {
				return result, err
			}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		if field == "data" {
			data.WriteString(strings.TrimPrefix(value, " "))
			data.WriteByte('\n')
		}
	}
	switch err := sc.Err(); {
	case errors.Is(err, errTooLarge), errors.Is(err, bufio.ErrTooLong):
		return nil, &ProtocolError{Message: fmt.Sprintf("the event stream: %v (%d bytes)", errTooLarge, c.max)}
	case err != nil:
		return nil, &TransientError{Status: resp.StatusCode, Err: fmt.Errorf("reading the event stream: %w", err)}
	}
	// A last event not followed by a blank line is taken all the same.
	if result, done, err := dispatch(); done {
		return result, err
	}
	return nil, &TransientError{Status: resp.StatusCode, Err: errors.New("the event stream ended without the answer")}
}
