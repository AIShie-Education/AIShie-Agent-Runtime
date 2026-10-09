package toolset

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

// TestLiveCore runs a student's own agent's toolset against a real Core
// (scripts/ci-core.sh start, then E2E_CORE_URL and E2E_ROOT_TOKEN, root's
// signed-in session; or make live-core): the agent is hosted by its id, its
// token issued to the site's agent runtime (AIShie-Core #52); the live
// catalogue passes Check, a delegate's seat is offered the twelve reads,
// and what the model would write, strict nulls and a wrong course
// included, reaches Core and is executed. A document's files come back
// from Core's own file store as text and as a file part, a Word file as
// its text beside the PDF rendition the runtime made of it, and no URL
// reaches the model, the rendition's included.
func TestLiveCore(t *testing.T) {
	base, root := os.Getenv("E2E_CORE_URL"), os.Getenv("E2E_ROOT_TOKEN")
	if base == "" || root == "" {
		t.Skip("E2E_CORE_URL and E2E_ROOT_TOKEN are not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := &liveREST{t: t, base: strings.TrimRight(base, "/")}
	run := fmt.Sprint(time.Now().UnixNano())

	// An admin, an instructor and a student, each signed in with a password
	// (person), a course, and in it a published assignment and four
	// documents: Markdown text, a Markdown file, a PDF file and a Word file.
	_, admin := c.person(root, run, "Admin", map[string]any{"platform_role": "admin"})
	satoID, sato := c.person(admin, run, "Sato", nil)
	yukiID, yuki := c.person(admin, run, "Yuki", nil)
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
	// upload puts a file in the course as Core's front end does, and gives
	// it, named, as a document's one file in files, which Core has taken
	// since AIShie-Core #49: a Core with #61 (#54) refuses upload_token
	// alone.
	upload := func(filename, contentType string, data []byte) map[string]any {
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
		return map[string]any{"files": []map[string]any{{"upload_token": up["upload_token"].(string), "filename": filename}}}
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
	mdDoc := document("Week 1 notes", upload("Week 1 notes.md", "text/markdown", []byte(markdown)))
	pdfDoc := document("Lab sheet", upload("Lab sheet.pdf", "application/pdf", pdf))
	handout := doctexttest.DOCX(doctexttest.Doc{Blocks: []doctexttest.Block{{Text: "Hash tables", Heading: 1}, {Text: "Open addressing and chaining."}}})
	wordDoc := document("Week 5 handout", upload("Week 5 handout.docx", doctexttest.DOCXType, handout))

	// The site's agent runtime, with a credential of the agent_runtime
	// service's own, makes the Word file's PDF rendition as it makes every
	// Office file's (AIShie-Core's migration 0026), here with no
	// LibreOffice: the PDF is the lab sheet's bytes.
	svc := c.runtimeService(root, run)
	renderPDF(ctx, t, svc, courseID, handout, pdf)

	// Yuki's own agent, hosted by the site's agent runtime by its id
	// (AIShie-Core #52): the runtime is issued its one token, and Yuki
	// holds none. It is seated as her delegate once Sato approves.
	agentID := c.result(yuki, "POST", "/v1/me/agents", map[string]any{"display_name": "Yuki's helper", "hosting": core.HostingRuntime})["actor_id"].(string)
	issued, err := svc.IssueToken(ctx, agentID, "")
	if err != nil {
		t.Fatal(err)
	}
	agent := issued.Token
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
	set, err := cat.Build(perms, config.Tools{}, ReadOnly, toolschema.OpenAIStrict, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A student's delegate reads what she reads, her group sets and peer
	// forms among them, and is offered no write in a conversation that may
	// have none.
	reads := []string{"assignment_get", "assignment_list", "component_tree", "course_get", "document_get", "document_list",
		"grade_get", "grade_list", "gradebook_get", "group_set_get", "group_set_list", "peer_form_get", "submission_get",
		"submission_list", "submission_roster"}
	if !slices.Equal(set.Names(), reads) {
		t.Fatalf("a delegate is offered %v, want %v", set.Names(), reads)
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
		call("12", "document_get", `{"document_id":"`+wordDoc+`","version_id":null}`),
		call("13", "group_set_list", `{"include_archived":null}`),
		call("14", "peer_form_get", `{"assignment_id":"`+hw+`"}`),
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
	// The Word file is given as its text. Core shows its PDF rendition,
	// done, at a URL of its own, which reaches the model no more than the
	// file's.
	word := contentOf(t, parts[11])
	if rec := word["file"].(map[string]any); rec["given_as"] != givenText || !strings.Contains(fmt.Sprint(word["file_text"]), "Open addressing and chaining.") {
		t.Errorf("the Word file: %v, text %q", rec, word["file_text"])
	}
	if r := renditionOf(t, c.result(sato, "GET", C+"/documents/"+wordDoc, nil)); r.State != core.RenditionDone || r.PageCount != 1 || r.DownloadURL == "" {
		t.Errorf("the Word file's rendition, as Sato is shown it: %+v", r)
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

// runtimeService is the site's agent runtime's client of Core, with a
// credential of the agent_runtime service's own, which root issues for the
// test and revokes after it. It replaces none: whatever else holds one on
// this Core keeps it.
func (c *liveREST) runtimeService(root, run string) *core.RuntimeService {
	c.t.Helper()
	issued := c.result(root, "POST", "/v1/services/agent_runtime/credentials", map[string]any{"label": "toolset live " + run})
	credential, id := issued["token"].(string), issued["credential_id"].(string)
	if !strings.HasPrefix(credential, core.ServiceTokenPrefix) {
		c.t.Fatalf("Core issued the agent runtime no credential (%s…): is it older than AIShie-Core #52?", core.ServiceTokenPrefix)
	}
	c.t.Cleanup(func() { c.result(root, "POST", "/v1/services/agent_runtime/credentials/"+id+"/revoke", nil) })
	return core.NewRuntimeService(core.RuntimeCaller(core.RuntimeOptions{BaseURL: c.base,
		Credential: func(context.Context) (string, error) { return credential, nil }}))
}

// renderPDF makes the PDF rendition of the Office file of the course's that
// is queued, original, as the runtime's renditions worker does: claimed,
// the file fetched from the claim's URL, its PDF uploaded, and done. It
// claims what other courses queued too, and leaves it to lapse.
func renderPDF(ctx context.Context, t *testing.T, svc *core.RuntimeService, courseID string, original, pdf []byte) {
	t.Helper()
	var claim *core.ClaimedRendition
	for deadline := time.Now().Add(30 * time.Second); claim == nil; {
		if time.Now().After(deadline) {
			t.Fatal("the Word file's rendition was never claimed")
		}
		claimed, err := svc.ClaimRenditions(ctx, core.MaxRenditionClaims, time.Minute, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for i := range claimed {
			if claimed[i].CourseID == courseID {
				claim = &claimed[i]
			}
		}
	}
	if claim.Source != core.RenditionOfDocumentFile || claim.FileID == "" || claim.ByteSize != int64(len(original)) {
		t.Fatalf("the claim: source %s, file %q, %d bytes", claim.Source, claim.FileID, claim.ByteSize)
	}
	cl := core.ClaimOfRendition(*claim)
	f, err := svc.RenditionFile(ctx, cl)
	if err != nil {
		t.Fatal(err)
	}
	if got := fetch(ctx, t, http.MethodGet, f.DownloadURL, nil, nil); !bytes.Equal(got, original) {
		t.Fatalf("the claimed file is %d bytes, not the Word file's %d", len(got), len(original))
	}
	if _, err := svc.RenewRendition(ctx, cl, time.Minute); err != nil {
		t.Fatal(err)
	}
	up, err := svc.RenditionUploadURL(ctx, cl)
	if err != nil {
		t.Fatal(err)
	}
	fetch(ctx, t, http.MethodPut, up.UploadURL, up.Headers, pdf)
	key := core.RenditionKey(cl, 1)
	done := core.RenditionCompletion{Status: core.RenditionDone, UploadToken: up.UploadToken, PageCount: 1}
	for range 2 { // the second, a replay under the same key
		r, err := svc.CompleteRendition(ctx, cl, key, done)
		if err != nil || r.State != core.RenditionDone || r.ByteSize != int64(len(pdf)) {
			t.Fatalf("the rendition completed: %+v %v", r, err)
		}
	}
	if _, err := svc.RenewRendition(ctx, cl, time.Minute); !core.IsReason(err, core.ReasonLeaseLost) {
		t.Fatalf("a claim renewed once done: %v", err)
	}
}

// fetch makes a request of a file's URL, with no credential but the URL,
// and returns the body; anything but a 2xx fails the test.
func fetch(ctx context.Context, t *testing.T, method, u string, headers map[string]string, body []byte) []byte {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s a file's URL: HTTP %d", method, resp.StatusCode)
	}
	return b
}

// renditionOf is the rendition of the one file of a document_get result's
// version, as Core shows it.
func renditionOf(t *testing.T, doc map[string]any) core.RenditionView {
	t.Helper()
	var d struct {
		Version struct {
			Files []struct {
				Rendition *core.RenditionView `json:"rendition"`
			} `json:"files"`
		} `json:"version"`
	}
	b, _ := json.Marshal(doc)
	if err := json.Unmarshal(b, &d); err != nil || len(d.Version.Files) != 1 || d.Version.Files[0].Rendition == nil {
		t.Fatalf("no rendition in %s", b)
	}
	return *d.Version.Files[0].Rendition
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

// person is registrar registering a person, with the fields more adds (a
// platform role, say), who then signs in with a password as people do:
// people hold no API tokens, only agents do. They are given an email of
// the run's, invited to choose a password (actor.invite), choose one as the
// front end's page for invitations does (POST /v1/auth/invite), and sign in
// with it (POST /v1/auth/login). It returns their actor's id and that
// session, which Core takes as a bearer token.
func (c *liveREST) person(registrar, run, name string, more map[string]any) (id, session string) {
	c.t.Helper()
	c.n++
	email := fmt.Sprintf("person-%s-%d@live.test", run, c.n)
	body := map[string]any{"kind": "human", "display_name": name, "email": email}
	maps.Copy(body, more)
	id = c.result(registrar, "POST", "/v1/actors", body)["actor_id"].(string)
	invite := c.result(registrar, "POST", "/v1/actors/"+id+"/invite", nil)["token"].(string)
	password := rand.Text()
	c.session("/v1/auth/invite", map[string]string{"token": invite, "password": password})
	return id, c.session("/v1/auth/login", map[string]string{"email": email, "password": password})
}

// session posts body to path, where Core signs someone in, and returns the
// session Core sets as its cookie.
func (c *liveREST) session(path string, body map[string]string) string {
	c.t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(c.base+path, "application/json", bytes.NewReader(b))
	if err != nil {
		c.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, ck := range resp.Cookies() {
		if ck.Name == "ais_session" && ck.Value != "" {
			return ck.Value
		}
	}
	c.t.Fatalf("POST %s: HTTP %d, and no session: %s", path, resp.StatusCode, raw)
	return ""
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
