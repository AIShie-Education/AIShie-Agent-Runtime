package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

// RESTOptions configure a RESTCaller.
type RESTOptions struct {
	// BaseURL is Core's base, such as https://lms.example.edu.
	BaseURL string
	// Token is the agent's token. It goes in the Authorization header and
	// nowhere else.
	Token string
	// HTTPClient carries the requests; one with DefaultTimeout when nil. A
	// copy of it is used that follows no redirect.
	HTTPClient *http.Client
	// Catalogue maps MCP tool names to REST routes. Required.
	Catalogue *Catalogue
	// MaxResponseBytes bounds one answer; DefaultMaxResponseBytes when 0.
	MaxResponseBytes int64
}

// RESTCaller is a Caller over Core's REST API (transport: rest): each tool
// at its catalogue route, path parameters taken from the arguments, a GET's
// other arguments in the query string, a POST's in a JSON body with the
// idempotency key in the Idempotency-Key header (§1.2). Core's REST answer
// holds the envelope's fields whatever its status, so the envelope comes
// from the body; the errors are Caller's, as over MCP.
//
// The envelopes are MCP's, but for the note MCP adds to a proposed one,
// which REST does not carry.
//
// Where a route cannot carry the arguments exactly (a path parameter that
// is missing or not a string, a query value Core would read back as
// something else), the call goes to POST /v1/tools/{name} instead, which
// Core takes for every tool with every argument in the body: the same call,
// so Core, not this caller, judges the arguments, as over MCP.
type RESTCaller struct {
	base   string
	token  string
	client *http.Client
	cat    *Catalogue
	max    int64
}

// NewRESTCaller returns a caller for one agent.
func NewRESTCaller(o RESTOptions) *RESTCaller {
	c := &RESTCaller{base: strings.TrimRight(o.BaseURL, "/"), token: o.Token, client: withoutRedirects(o.HTTPClient), cat: o.Catalogue, max: o.MaxResponseBytes}
	if c.max <= 0 {
		c.max = DefaultMaxResponseBytes
	}
	return c
}

// idempotencyKeyArg is the argument that carries a write's key over MCP,
// and the Idempotency-Key header over REST.
const idempotencyKeyArg = "idempotency_key"

// Call makes tool's call at its REST route.
func (c *RESTCaller) Call(ctx context.Context, tool string, args json.RawMessage) (*Envelope, error) {
	if c.cat == nil {
		return nil, &ProtocolError{Message: "the REST transport has no catalogue"}
	}
	t, ok := c.cat.Tool(tool)
	if !ok {
		return nil, &ProtocolError{Message: fmt.Sprintf("%s is not in Core's catalogue", clip(tool, 64))}
	}
	r, err := buildRequest(t, args)
	if err != nil {
		return nil, err
	}
	return c.send(ctx, r)
}

// restRequest is one call as REST carries it.
type restRequest struct {
	method string
	path   string // escaped, with the query
	body   []byte // nil for a GET
	key    string
}

// field is one top-level argument, as written.
type field struct {
	name string
	raw  json.RawMessage
}

// pathParam is how Core's routes name their parameters.
var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// buildRequest lays out t's call with args: at t's route when it can carry
// them exactly, else at POST /v1/tools/{name}.
func buildRequest(t CatalogueTool, args json.RawMessage) (restRequest, error) {
	fields, err := objectFields(args)
	if err != nil {
		return restRequest{}, &ProtocolError{Message: fmt.Sprintf("%s: the arguments: %v", t.MCPName, err)}
	}
	var r restRequest
	// Over MCP, Core takes the key out of a write's arguments; a read's
	// stays in them, and the schema refuses it. REST does the same.
	if t.Kind == KindWrite {
		if i := indexOf(fields, idempotencyKeyArg); i >= 0 {
			var key string
			if json.Unmarshal(fields[i].raw, &key) == nil {
				// A header's value loses its leading and trailing blanks on
				// the way: Core would be given another key than MCP gives it.
				if !validHeaderValue(key) || strings.Trim(key, " \t") != key {
					return restRequest{}, &ProtocolError{Message: t.MCPName + ": the idempotency key cannot be sent in a header as it is"}
				}
				r.key = key
				fields = slices.Delete(fields, i, i+1)
			}
		}
	}
	if path, rest, ok := fillPath(t.Path, fields); ok {
		switch t.Method {
		case http.MethodGet:
			if query, ok := queryFor(t, rest); ok {
				r.method, r.path = http.MethodGet, path
				if query != "" {
					r.path += "?" + query
				}
				return r, nil
			}
		case http.MethodPost:
			r.method, r.path, r.body = http.MethodPost, path, objectBody(rest)
			return r, nil
		}
	}
	r.method, r.path, r.body = http.MethodPost, "/v1/tools/"+url.PathEscape(t.Name), objectBody(fields)
	return r, nil
}

// objectFields reads args, a JSON object, keeping each member as written
// and in order. No arguments, or null, is the empty object, as Core has it.
// An object naming a key twice is refused, as Core refuses it.
func objectFields(args json.RawMessage) ([]field, error) {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var fields []field
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, _ := tok.(string)
		if seen[name] {
			return nil, fmt.Errorf("%q is given twice", clip(name, 64))
		}
		seen[name] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return nil, err
		}
		fields = append(fields, field{name, compact.Bytes()})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one JSON value")
	}
	return fields, nil
}

func indexOf(fields []field, name string) int {
	return slices.IndexFunc(fields, func(f field) bool { return f.name == name })
}

// objectBody writes fields back as one JSON object, in their order.
func objectBody(fields []field) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := encode(f.name)
		b.Write(name)
		b.WriteByte(':')
		b.Write(f.raw)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// fillPath fills pattern's parameters from fields, which lose them. It
// fails when one is missing, or is not a string a path segment can carry:
// "." and ".." are strings, but a path with them is another path, which
// Core's router would redirect to (a model writing document_id ".." would
// reach the course).
func fillPath(pattern string, fields []field) (string, []field, bool) {
	if pattern == "" || !strings.HasPrefix(pattern, "/") {
		return "", nil, false
	}
	rest := slices.Clone(fields)
	ok := true
	path := pathParam.ReplaceAllStringFunc(pattern, func(m string) string {
		name := m[1 : len(m)-1]
		i := indexOf(rest, name)
		var value string
		if i < 0 || json.Unmarshal(rest[i].raw, &value) != nil || value == "" || value == "." || value == ".." {
			ok = false
			return m
		}
		rest = slices.Delete(rest, i, i+1)
		return url.PathEscape(value)
	})
	return path, rest, ok
}

// queryFor writes fields as a query string that Core reads back as exactly
// these arguments (Core's httpapi buildArgs and coerce): repeated values
// for an array, text coerced by the property's schema. A null is left out,
// which is the same as absent where the schema allows null (§3.8: "omit or
// null"). It fails when some value would not come back as it is.
func queryFor(t CatalogueTool, fields []field) (string, bool) {
	props := properties(t.InputSchema)
	q := url.Values{}
	for _, f := range fields {
		s := props[f.name]
		var v any
		dec := json.NewDecoder(bytes.NewReader(f.raw))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			return "", false
		}
		if v == nil {
			if s == nil || !slices.Contains(s.types(), "null") {
				return "", false
			}
			continue
		}
		values, ok := queryValues(v)
		if !ok || !reflect.DeepEqual(coerce(s, values), v) {
			return "", false
		}
		q[f.name] = values
	}
	return q.Encode(), true
}

// queryValues is v as query-string text: a scalar as one value, an array
// of scalars as one value each. An object, an empty array, or an array
// holding anything but scalars has no such form.
func queryValues(v any) ([]string, bool) {
	if a, ok := v.([]any); ok {
		if len(a) == 0 {
			return nil, false
		}
		out := make([]string, len(a))
		for i, e := range a {
			s, ok := scalarText(e)
			if !ok {
				return nil, false
			}
			out[i] = s
		}
		return out, true
	}
	s, ok := scalarText(v)
	return []string{s}, ok
}

func scalarText(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case bool:
		if v {
			return "true", true
		}
		return "false", true
	}
	return "", false
}

// propSchema is as much of a property's schema as Core's coerce reads.
type propSchema struct {
	Type  json.RawMessage `json:"type"`
	Items *propSchema     `json:"items"`
}

func (s *propSchema) types() []string {
	if s == nil {
		return nil
	}
	var one string
	if json.Unmarshal(s.Type, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(s.Type, &many)
	return many
}

func properties(schema json.RawMessage) map[string]*propSchema {
	var s struct {
		Properties map[string]*propSchema `json:"properties"`
	}
	_ = json.Unmarshal(schema, &s)
	return s.Properties
}

// coerce is Core's httpapi coerce: query-string text turned into the JSON
// type the schema asks for, or left as text.
func coerce(s *propSchema, values []string) any {
	if s == nil || len(values) == 0 {
		if len(values) == 0 {
			return ""
		}
		return values[0]
	}
	types := s.types()
	if slices.Contains(types, "array") {
		out := make([]any, len(values))
		for i, v := range values {
			out[i] = coerce(s.Items, []string{v})
		}
		return out
	}
	v := values[0]
	switch {
	case slices.Contains(types, "boolean") && (v == "true" || v == "false"):
		return v == "true"
	case slices.Contains(types, "integer"), slices.Contains(types, "number"):
		if !slices.Contains(types, "string") && isNumber(v) {
			return json.Number(v)
		}
	}
	return v
}

// isNumber is Core's: v is a JSON number, written as json.Number writes it.
func isNumber(v string) bool {
	var n json.Number
	return json.Valid([]byte(v)) && json.Unmarshal([]byte(v), &n) == nil && n.String() == v
}

func (c *RESTCaller) send(ctx context.Context, r restRequest) (*Envelope, error) {
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.base+r.path, body)
	if err != nil {
		return nil, &ProtocolError{Message: "the request cannot be made: " + redact(err.Error(), c.token)}
	}
	if !validHeaderValue(c.token) {
		return nil, &ProtocolError{Message: "the token cannot be sent in a header"}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent(clientName, defaultVersion()))
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.key != "" {
		req.Header.Set("Idempotency-Key", r.key)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, sendError(ctx, err, c.token)
	}
	defer func() { _ = resp.Body.Close() }()
	if redirected(req, resp) {
		return nil, errRedirected(resp.StatusCode)
	}
	return c.answer(resp)
}

// answer reads Core's REST answer; the caller closes the body. 200 executed, 202 proposed, 403 denied,
// and a failure's own status all carry the envelope's fields; a call never
// attempted carries only {"error": …}, which is the envelope of status
// error that MCP would have given.
func (c *RESTCaller) answer(resp *http.Response) (*Envelope, error) {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		drain(resp)
		return nil, ErrUnauthenticated
	case http.StatusTooManyRequests:
		return nil, rateLimited(resp, c.max)
	}
	body, err := readBody(resp, c.max)
	if err != nil {
		return nil, err
	}
	status := resp.StatusCode
	if env, ok := decodeEnvelope(body); ok {
		// On a 5xx, a body with a status is Core's only as a write's
		// recorded outcome, which names its action; anything else is
		// something in front of Core, and the call may be sent again.
		if status >= 500 && env.ActionID == "" {
			return nil, serverError(status, body, c.token)
		}
		return env, nil
	}
	var only struct {
		Error *Error `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	decErr := dec.Decode(&only)
	switch {
	case decErr == nil && only.Error != nil && only.Error.Code != "":
		switch {
		case status >= 200 && status <= 299:
			return nil, &ProtocolError{Message: fmt.Sprintf("HTTP %d without a status", status)}
		// Core's own fault is code internal, retried as over MCP; any
		// other code on a 5xx is something in front of Core.
		case status >= 500 && only.Error.Code != CodeInternal:
			return nil, serverError(status, body, c.token)
		}
		return &Envelope{Status: StatusError, Error: only.Error}, nil
	case status >= 500 || status == http.StatusRequestTimeout:
		return nil, serverError(status, body, c.token)
	case errors.Is(decErr, io.ErrUnexpectedEOF):
		return nil, &TransientError{Status: status, Err: errors.New("the answer was cut short")}
	}
	return nil, &ProtocolError{Message: fmt.Sprintf("HTTP %d: the answer is not an envelope: %s", status, said(body, c.token))}
}
