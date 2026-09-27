package toolset

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// TestLiveCore runs a student's own agent's toolset against a real Core
// (scripts/ci-core.sh start, then E2E_CORE_URL and E2E_ROOT_TOKEN): the live
// catalogue passes Check, a delegate's seat is offered the eleven reads, and
// what the model would write, strict nulls and a wrong course included,
// reaches Core and is executed. A document's files come back from Core's
// own file store as text and as a file part, and no URL reaches the model.
func TestLiveCore(t *testing.T) {
	base, root := os.Getenv("E2E_CORE_URL"), os.Getenv("E2E_ROOT_TOKEN")
	if base == "" || root == "" {
		t.Skip("E2E_CORE_URL and E2E_ROOT_TOKEN are not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := &liveREST{t: t, base: strings.TrimRight(base, "/")}
	run := fmt.Sprint(time.Now().UnixNano())

	// An admin, an instructor and a student, a course, and in it a
	// published assignment and three documents: Markdown text, a Markdown
	// file and a PDF file.
	adminID := c.result(root, "POST", "/v1/actors", map[string]any{"kind": "human", "display_name": "Admin " + run, "platform_role": "admin"})["actor_id"].(string)
	admin := c.result(root, "POST", "/v1/actors/"+adminID+"/tokens", map[string]any{"label": "t"})["token"].(string)
	actor := func(name string) (string, string) {
		id := c.result(admin, "POST", "/v1/actors", map[string]any{"kind": "human", "display_name": name})["actor_id"].(string)
		return id, c.result(admin, "POST", "/v1/actors/"+id+"/tokens", map[string]any{"label": "t"})["token"].(string)
	}
	satoID, sato := actor("Sato")
	yukiID, yuki := actor("Yuki")
	term := c.result(admin, "POST", "/v1/terms", map[string]any{"name": "T" + run, "starts_on": "2026-09-01", "ends_on": "2026-12-20"})["id"].(string)
	dept := c.result(admin, "POST", "/v1/departments", map[string]any{"name": "D" + run})["id"].(string)
	course := c.result(admin, "POST", "/v1/courses", map[string]any{"dept_id": dept, "term_id": term, "code": "CS" + run[len(run)-4:], "section": "A", "title": "Intro"})
	courseID := course["course_id"].(string)
	C := "/v1/courses/" + courseID
	c.result(admin, "POST", C+"/activate", nil)
	c.result(admin, "POST", C+"/instructors", map[string]any{"actor_id": satoID})
	bucket := c.result(sato, "POST", C+"/components", map[string]any{"parent_id": course["root_component_id"], "name": "Assignments", "weight": 40})["id"].(string)
	hw := c.result(sato, "POST", C+"/assignments", map[string]any{"title": "HW3", "points_possible": 100, "component_id": bucket})["id"].(string)
	c.result(sato, "POST", C+"/assignments/"+hw+"/publish", nil)
	yukiMember := c.result(sato, "POST", C+"/members", map[string]any{"actor_id": yukiID, "preset": "student"})["member_id"].(string)

	document := func(title string, body map[string]any) string {
		body["kind"], body["title"] = "material", title
		id := c.result(sato, "POST", C+"/documents", body)["document_id"].(string)
		c.result(sato, "POST", C+"/documents/"+id+"/publish", nil)
		return id
	}
	upload := func(contentType string, data []byte) string {
		up := c.result(sato, "GET", C+"/upload-url?kind=material&content_type="+url.QueryEscape(contentType), nil)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, up["upload_url"].(string), bytes.NewReader(data))
		for k, v := range up["headers"].(map[string]any) {
			req.Header.Set(k, v.(string))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("upload: HTTP %d", resp.StatusCode)
		}
		return up["upload_token"].(string)
	}
	textDoc := document("Syllabus", map[string]any{"body_md": "# Syllabus\nWeekly labs."})

	// Another course of Sato's, with a document of its own that Yuki's
	// agent must not reach from this one, whatever course it names.
	elsewhere := c.result(admin, "POST", "/v1/courses", map[string]any{"dept_id": dept, "term_id": term, "code": "EL" + run[len(run)-4:], "section": "B", "title": "Elsewhere"})["course_id"].(string)
	E := "/v1/courses/" + elsewhere
	c.result(admin, "POST", E+"/activate", nil)
	c.result(admin, "POST", E+"/instructors", map[string]any{"actor_id": satoID})
	otherDoc := c.result(sato, "POST", E+"/documents", map[string]any{"kind": "material", "title": "Answers", "body_md": "not for CS"})["document_id"].(string)
	c.result(sato, "POST", E+"/documents/"+otherDoc+"/publish", nil)
	mdDoc := document("Week 1 notes", map[string]any{"upload_token": upload("text/markdown", []byte(markdown))})
	pdfDoc := document("Lab sheet", map[string]any{"upload_token": upload("application/pdf", pdf)})

	// Yuki's own agent, seated as her delegate once Sato approves.
	agentID := c.result(yuki, "POST", "/v1/me/agents", map[string]any{"display_name": "Yuki's helper"})["actor_id"].(string)
	agent := c.result(yuki, "POST", "/v1/me/agents/"+agentID+"/tokens", map[string]any{"label": "runtime"})["token"].(string)
	seat := c.envelope(yuki, "POST", C+"/delegates", map[string]any{"actor_id": agentID, "preset": "delegate"})
	if seat.Status == core.StatusProposed {
		c.result(sato, "POST", C+"/actions/"+seat.ActionID+"/decide", map[string]any{"decision": "approve"})
	}

	mcp := &liveMCP{base: c.base, token: agent}
	client := core.NewClient(mcp)
	seats, err := client.Memberships(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var perms map[string]string
	for _, m := range seats {
		if m.CourseID == courseID {
			perms = m.Perms
		}
	}
	cat := c.catalogue()
	if err := cat.Check(); err != nil {
		t.Fatalf("the live catalogue fails Check: %v", err)
	}
	set, err := cat.Build(perms, config.Tools{}, toolschema.OpenAIStrict, nil)
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != len(DefaultAllow) {
		t.Fatalf("a delegate is offered %v, want the %d defaults", set.Names(), len(DefaultAllow))
	}

	// What a strict model writes: every property, null for what it leaves
	// out, and here and there another course.
	other := "0192f3c1-0000-7000-8000-000000000000"
	calls := []llm.Part{
		call("1", "course_get", `{"course_id":"`+other+`"}`),
		call("2", "document_list", `{"after":null,"kind":null,"limit":null,"include_archived":null}`),
		call("3", "document_get", `{"document_id":"`+textDoc+`","version_id":null}`),
		call("4", "document_get", `{"course_id":"`+other+`","document_id":"`+mdDoc+`","version_id":null}`),
		call("5", "document_get", `{"document_id":"`+pdfDoc+`","version_id":null}`),
		call("6", "assignment_list", `{"after":null,"limit":null}`),
		call("7", "assignment_get", `{"assignment_id":"`+hw+`"}`),
		call("8", "submission_list", `{"after":null,"assignment_id":null,"limit":null,"student_member_id":null}`),
		call("9", "grade_list", `{"after":null,"assignment_id":null,"limit":null,"student_member_id":null}`),
		call("10", "component_tree", `{}`),
		call("11", "gradebook_get", `{"student_member_id":"`+yukiMember+`","treat_ungraded_as_zero":null}`),
	}
	r := Runner{Client: client, Files: NewHTTPFetcher(nil), FileInput: true}
	parts, err := set.Run(ctx, r, courseID, calls)
	if err != nil {
		t.Fatal(err)
	}
	if mcp.calls.Load() != int32(len(calls))+1 {
		t.Errorf("%d calls to Core, want one per tool call and me_memberships", mcp.calls.Load())
	}
	for i, p := range parts[:len(calls)] {
		if p.IsError || !strings.HasPrefix(p.Content, `{"status":"executed"`) {
			t.Errorf("call %s (%s): %s", calls[i].ID, calls[i].Name, p.Content)
		}
		if strings.Contains(p.Content, "download_url") || strings.Contains(p.Content, "/v1/blobs/") {
			t.Errorf("call %s: a URL reached the model: %s", calls[i].ID, p.Content)
		}
	}
	if !strings.Contains(parts[0].Content, courseID) {
		t.Errorf("course_get did not read the conversation's course: %s", parts[0].Content)
	}
	if !strings.Contains(parts[2].Content, "Weekly labs.") {
		t.Errorf("the Markdown body is not in the result: %s", parts[2].Content)
	}
	md := contentOf(t, parts[3])
	if rec := md["file"].(map[string]any); rec["given_as"] != givenText || md["file_text"] != markdown {
		t.Errorf("the Markdown file: %v, text %q", rec, md["file_text"])
	}
	if rec := contentOf(t, parts[4])["file"].(map[string]any); rec["given_as"] != givenFile {
		t.Errorf("the PDF: %v", rec)
	}
	if len(parts) != len(calls)+1 || parts[len(calls)].File == nil || !bytes.Equal(parts[len(calls)].File.Data, pdf) ||
		parts[len(calls)].File.Name != "Lab sheet.pdf" {
		t.Errorf("the PDF is not the file part after the results")
	}

	// Another course's document, with that course named: the call goes to
	// the conversation's course, where Core finds no such document.
	before := mcp.calls.Load()
	parts, err = set.Run(ctx, r, courseID, []llm.Part{
		call("x", "document_get", `{"course_id":"`+elsewhere+`","document_id":"`+otherDoc+`","version_id":null}`),
		call("y", "document_get", `{"Course_Id":"`+elsewhere+`","document_id":"`+otherDoc+`"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := errorOf(t, parts[0]); !parts[0].IsError || code != core.CodeNotFound || strings.Contains(parts[0].Content, "not for CS") {
		t.Errorf("another course's document: %s", parts[0].Content)
	}
	if code, _ := errorOf(t, parts[1]); !parts[1].IsError || code != core.CodeInvalidArgument {
		t.Errorf("a course named under another key: %s", parts[1].Content)
	}
	if n := mcp.calls.Load() - before; n != 1 {
		t.Errorf("%d calls to Core, want 1: the second is refused before Core", n)
	}
}

// liveREST sets a course up through Core's REST API.
type liveREST struct {
	t    *testing.T
	base string
	n    int
}

func (c *liveREST) envelope(token, method, path string, body map[string]any) *core.Envelope {
	c.t.Helper()
	c.n++
	var rd io.Reader
	if method == http.MethodPost {
		if body == nil {
			body = map[string]any{}
		}
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("toolset-live-%d-%d", time.Now().UnixNano(), c.n))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var env core.Envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		c.t.Fatalf("%s %s: HTTP %d: %v", method, path, resp.StatusCode, err)
	}
	return &env
}

func (c *liveREST) result(token, method, path string, body map[string]any) map[string]any {
	c.t.Helper()
	env := c.envelope(token, method, path, body)
	if env.Status != core.StatusExecuted {
		c.t.Fatalf("%s %s: %s %+v", method, path, env.Status, env.Error)
	}
	var m map[string]any
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &m); err != nil {
			c.t.Fatal(err)
		}
	}
	return m
}

// catalogue reads GET /v1/tools as the toolset does.
func (c *liveREST) catalogue() *Catalogue {
	c.t.Helper()
	resp, err := http.Get(c.base + "/v1/tools")
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Tools []struct {
			Name, Description, Kind string
			InputSchema             json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		c.t.Fatal(err)
	}
	cat := &Catalogue{Hash: "live", Tools: map[string]CatalogueTool{}}
	for _, tool := range body.Tools {
		name := strings.ReplaceAll(tool.Name, ".", "_")
		cat.Tools[name] = CatalogueTool{Name: name, Description: tool.Description, Kind: tool.Kind, InputSchema: tool.InputSchema}
	}
	return cat
}

// liveMCP is the least MCP client that calls Core's tools: one stateless
// tools/call per request, the envelope from structuredContent. The
// runtime's own is core.MCPCaller.
type liveMCP struct {
	base, token string
	calls       atomic.Int32
}

func (m *liveMCP) Call(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
	m.calls.Add(1)
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.calls.Load(), "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &core.TransientError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, core.ErrUnauthenticated
	}
	var rpc struct {
		Result struct {
			StructuredContent *core.Envelope `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Code    int64  `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return nil, &core.ProtocolError{Message: err.Error()}
	}
	if rpc.Error != nil {
		return nil, &core.ProtocolError{Code: rpc.Error.Code, Message: rpc.Error.Message}
	}
	if rpc.Result.StructuredContent == nil {
		return nil, &core.ProtocolError{Message: "no structuredContent"}
	}
	return rpc.Result.StructuredContent, nil
}
