package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// Tool kinds in the catalogue. An ephemeral tool changes state that is no
// action (an answer's draft): it takes no idempotency key, is recorded
// nowhere and is never proposed.
const (
	KindRead      = "read"
	KindWrite     = "write"
	KindEphemeral = "ephemeral"
)

// CatalogueTool is one tool of GET /v1/tools.
type CatalogueTool struct {
	// Name is the registry's, dotted: conversation.answer.
	Name string `json:"name"`
	// MCPName is Name with dots turned to underscores, as MCP offers it
	// (§1.2): conversation_answer.
	MCPName     string `json:"mcp_name"`
	Description string `json:"description"`
	// Kind is read or write.
	Kind string `json:"kind"`
	// Method and Path are its REST route, such as POST and
	// /v1/courses/{course_id}/conversations/{conversation_id}/answer.
	Method string `json:"method"`
	Path   string `json:"path"`
	// InputSchema is the tool's own, without idempotency_key, which a write
	// takes beside it (in the Idempotency-Key header over REST).
	InputSchema json.RawMessage `json:"input_schema"`
	// OutputSchema is the result's, inside the envelope.
	OutputSchema json.RawMessage `json:"output_schema"`
}

// Read reports whether the tool is a read, which Core does not record.
func (t CatalogueTool) Read() bool { return t.Kind == KindRead }

// Catalogue is Core's tool catalogue, GET /v1/tools: what each tool is
// called, what it takes and where its REST route is. Its hash shows when
// Core's tools change (§8.2).
type Catalogue struct {
	tools []CatalogueTool // by MCPName
	index map[string]int
	hash  string
}

// ParseCatalogue reads GET /v1/tools' answer: {"tools":[…]}.
func ParseCatalogue(b []byte) (*Catalogue, error) {
	var doc struct {
		Tools []struct {
			Name         string          `json:"name"`
			Description  string          `json:"description"`
			Kind         string          `json:"kind"`
			Method       string          `json:"method"`
			Path         string          `json:"path"`
			InputSchema  json.RawMessage `json:"input_schema"`
			OutputSchema json.RawMessage `json:"output_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("core: the catalogue does not decode: %w", err)
	}
	if doc.Tools == nil {
		return nil, errors.New("core: the catalogue has no tools")
	}
	hash, err := canonicalHash(b)
	if err != nil {
		return nil, fmt.Errorf("core: the catalogue: %w", err)
	}
	c := &Catalogue{index: make(map[string]int, len(doc.Tools)), hash: hash}
	for _, t := range doc.Tools {
		if t.Name == "" {
			return nil, errors.New("core: the catalogue has a tool with no name")
		}
		ct := CatalogueTool{
			Name: t.Name, MCPName: mcpName(t.Name), Description: t.Description, Kind: t.Kind,
			Method: t.Method, Path: t.Path, InputSchema: t.InputSchema, OutputSchema: t.OutputSchema,
		}
		if _, dup := c.index[ct.MCPName]; dup {
			return nil, fmt.Errorf("core: the catalogue names %s twice over MCP", ct.MCPName)
		}
		c.index[ct.MCPName] = len(c.tools)
		c.tools = append(c.tools, ct)
	}
	sort.Slice(c.tools, func(i, j int) bool { return c.tools[i].MCPName < c.tools[j].MCPName })
	for i, t := range c.tools {
		c.index[t.MCPName] = i
	}
	return c, nil
}

// mcpName is Core's mcpapi.ToolName: the registry's name, dots turned to
// underscores.
func mcpName(name string) string { return strings.ReplaceAll(name, ".", "_") }

// maxCatalogueBytes bounds GET /v1/tools' answer; Core's is about 200 KB.
const maxCatalogueBytes = 16 << 20

// FetchCatalogue reads GET /v1/tools from Core at baseURL. It needs no
// token, and follows no redirect: the catalogue is the configured Core's. A
// network error or a 5xx is a *TransientError, as from a Caller.
func FetchCatalogue(ctx context.Context, client *http.Client, baseURL string) (*Catalogue, error) {
	client = withoutRedirects(client)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/v1/tools", nil)
	if err != nil {
		return nil, fmt.Errorf("core: GET /v1/tools: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent(clientName, defaultVersion()))
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("core: GET /v1/tools: %w", sendError(ctx, err, ""))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readBody(resp, maxCatalogueBytes)
	if err != nil {
		return nil, fmt.Errorf("core: GET /v1/tools: %w", err)
	}
	switch {
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("core: GET /v1/tools: %w", serverError(resp.StatusCode, body, ""))
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("core: GET /v1/tools: HTTP %d: %s", resp.StatusCode, said(body, ""))
	}
	return ParseCatalogue(body)
}

// Hash is the sha256, in hex, of the catalogue's canonical JSON: keys
// sorted by their bytes, numbers as written, no insignificant whitespace.
// The same catalogue hashes the same however it is indented or its keys
// ordered.
func (c *Catalogue) Hash() string { return c.hash }

// Tool is the tool Core offers over MCP as mcpName.
func (c *Catalogue) Tool(mcpName string) (CatalogueTool, bool) {
	i, ok := c.index[mcpName]
	if !ok {
		return CatalogueTool{}, false
	}
	return c.tools[i], true
}

// Tools are every tool, by MCP name.
func (c *Catalogue) Tools() []CatalogueTool {
	return append([]CatalogueTool(nil), c.tools...)
}

// Len is how many tools there are.
func (c *Catalogue) Len() int { return len(c.tools) }

// MaxWait is the longest a call of the tool Core offers as mcpName may wait
// for news, as its input schema says: wait_s's maximum, at most MaxWait (a
// wait_s without one is taken to be Core's, 0 to 25); 0 when the tool takes
// no wait_s, as on a Core from before 2c1fe1b, where a call with it is
// refused. It is read from the catalogue Core serves, never from the
// snapshot: a runtime is run against older Cores too.
func (c *Catalogue) MaxWait(mcpName string) time.Duration {
	if c == nil {
		return 0
	}
	t, ok := c.Tool(mcpName)
	if !ok {
		return 0
	}
	var schema struct {
		Properties map[string]*struct {
			Type    json.RawMessage `json:"type"`
			Maximum *float64        `json:"maximum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
		return 0
	}
	p := schema.Properties["wait_s"]
	if p == nil || !slices.Contains((&propSchema{Type: p.Type}).types(), "integer") {
		return 0
	}
	if p.Maximum == nil {
		return MaxWait
	}
	if *p.Maximum < 1 {
		return 0
	}
	return min(time.Duration(*p.Maximum)*time.Second, MaxWait)
}

// Drafts reports whether this Core takes the drafts of answers being
// written (ToolDraft): its catalogue offers conversation_draft, an
// ephemeral write. A Core from before it has no such tool, and is sent no
// draft. It is read from the catalogue Core serves, never from the
// snapshot, as MaxWait is.
func (c *Catalogue) Drafts() bool {
	if c == nil {
		return false
	}
	t, ok := c.Tool(ToolDraft)
	return ok && t.Kind == KindEphemeral
}

// canonicalHash is the sha256 of raw's canonical JSON.
func canonicalHash(raw []byte) (string, error) {
	var buf bytes.Buffer
	if err := canonicalJSON(&buf, raw); err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// canonicalJSON writes raw, one JSON value, with every object's keys sorted
// by their bytes, numbers exactly as written, strings as encoding/json
// writes them without escaping HTML, and no whitespace between tokens. An
// object that names a key twice is refused: which value counts would be
// anybody's guess.
func canonicalJSON(w *bytes.Buffer, raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := canonicalValue(w, dec); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("more than one JSON value")
	}
	return nil
}

func canonicalValue(w *bytes.Buffer, dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			return canonicalObject(w, dec)
		case '[':
			return canonicalArray(w, dec)
		}
		return fmt.Errorf("unexpected %v", v)
	case string:
		return writeString(w, v)
	case json.Number:
		w.WriteString(v.String())
	case bool:
		if v {
			w.WriteString("true")
		} else {
			w.WriteString("false")
		}
	case nil:
		w.WriteString("null")
	}
	return nil
}

func canonicalObject(w *bytes.Buffer, dec *json.Decoder) error {
	members := map[string][]byte{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("an object key is %v", tok)
		}
		if _, dup := members[key]; dup {
			return fmt.Errorf("an object names %q twice", key)
		}
		var value bytes.Buffer
		if err := canonicalValue(&value, dec); err != nil {
			return err
		}
		members[key] = value.Bytes()
	}
	if _, err := dec.Token(); err != nil { // '}'
		return err
	}
	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	w.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			w.WriteByte(',')
		}
		if err := writeString(w, k); err != nil {
			return err
		}
		w.WriteByte(':')
		w.Write(members[k])
	}
	w.WriteByte('}')
	return nil
}

func canonicalArray(w *bytes.Buffer, dec *json.Decoder) error {
	w.WriteByte('[')
	for i := 0; dec.More(); i++ {
		if i > 0 {
			w.WriteByte(',')
		}
		if err := canonicalValue(w, dec); err != nil {
			return err
		}
	}
	w.WriteByte(']')
	_, err := dec.Token() // ']'
	return err
}

func writeString(w *bytes.Buffer, s string) error {
	b, err := encode(s)
	if err != nil {
		return err
	}
	w.Write(b)
	return nil
}
