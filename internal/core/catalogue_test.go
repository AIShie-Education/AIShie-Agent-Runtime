package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

func readCatalogue(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCatalogueSnapshot holds the pinned catalogue to what it is: 167
// tools, 63 reads, 99 writes and five ephemeral writes (conversation.draft,
// the transcription service's document_text.queue and .renew, and the agent
// runtime's agent_runtime.rendition_claim and .rendition_renew), every name
// within [a-z_]+ and at most 64 characters, which every provider takes; the
// longest, 34, is agent_runtime_rendition_upload_url, a site service's,
// offered to no model (§1.2 counts an earlier Core's).
func TestCatalogueSnapshot(t *testing.T) {
	c, err := ParseCatalogue(readCatalogue(t))
	if err != nil {
		t.Fatal(err)
	}
	tools := c.Tools()
	if len(tools) != 167 || c.Len() != 167 {
		t.Fatalf("%d tools, want 167", len(tools))
	}
	name := regexp.MustCompile(`^[a-z_]+$`)
	reads, writes, ephemeral, longest := 0, 0, 0, 0
	for _, tl := range tools {
		switch tl.Kind {
		case KindRead:
			reads++
			if tl.Method != http.MethodGet || !tl.Read() {
				t.Errorf("%s is a read at %s", tl.Name, tl.Method)
			}
		case KindWrite:
			writes++
			if tl.Method != http.MethodPost || tl.Read() {
				t.Errorf("%s is a write at %s", tl.Name, tl.Method)
			}
		case KindEphemeral:
			ephemeral++
			if tl.Method != http.MethodPost || tl.Read() || !slices.Contains([]string{ToolDraft, "document_text_queue", "document_text_renew",
				"agent_runtime_rendition_claim", "agent_runtime_rendition_renew"}, tl.MCPName) {
				t.Errorf("%s is an ephemeral write at %s", tl.Name, tl.Method)
			}
		default:
			t.Errorf("%s is of kind %q", tl.Name, tl.Kind)
		}
		if !name.MatchString(tl.MCPName) || len(tl.MCPName) > 64 {
			t.Errorf("MCP name %q", tl.MCPName)
		}
		if tl.MCPName != strings.ReplaceAll(tl.Name, ".", "_") {
			t.Errorf("%s is %s over MCP", tl.Name, tl.MCPName)
		}
		if !strings.HasPrefix(tl.Path, "/v1/") || !json.Valid(tl.InputSchema) || !json.Valid(tl.OutputSchema) {
			t.Errorf("%s: path %q or its schemas", tl.Name, tl.Path)
		}
		longest = max(longest, len(tl.MCPName))
	}
	if reads != 63 || writes != 99 || ephemeral != 5 || longest != 34 {
		t.Fatalf("%d reads, %d writes, %d ephemeral, the longest name %d; want 63, 99, 5, 34", reads, writes, ephemeral, longest)
	}
	if !sort.SliceIsSorted(tools, func(i, j int) bool { return tools[i].MCPName < tools[j].MCPName }) {
		t.Error("Tools is not sorted by MCP name")
	}

	answer, ok := c.Tool("conversation_answer")
	if !ok || answer.Name != "conversation.answer" || answer.Kind != KindWrite || answer.Method != http.MethodPost ||
		answer.Path != "/v1/courses/{course_id}/conversations/{conversation_id}/answer" {
		t.Fatalf("conversation_answer: %+v", answer)
	}
	if _, ok := c.Tool("conversation.answer"); ok {
		t.Error("a tool was found by its registry name")
	}
	tools[0].Name = "changed"
	if c.Tools()[0].Name == "changed" {
		t.Error("Tools gives away the catalogue's own slice")
	}
	golden(t, "catalogue.sha256", []byte(c.Hash()+"\n"))
}

// An Actor names the owner me_get names, and none when me_get names none.
func TestActorOwner(t *testing.T) {
	for raw, want := range map[string]string{
		`{"id":"a1","kind":"agent","display_name":"Helper","status":"active","owner_actor_id":"p1"}`: "p1",
		`{"id":"a1","kind":"agent","display_name":"Helper","status":"active"}`:                       "",
		`{"id":"a1","kind":"agent","display_name":"Helper","status":"active","owner_actor_id":null}`: "",
	} {
		var a Actor
		if err := json.Unmarshal([]byte(raw), &a); err != nil || a.OwnerActorID != want {
			t.Errorf("%s: owner %q, %v; want %q", raw, a.OwnerActorID, err, want)
		}
	}
}

// reordered writes v as JSON with every object's keys in reverse order and
// an indent of its own.
func reordered(v any, indent string, depth int, w *bytes.Buffer) {
	pad := "\n" + strings.Repeat(indent, depth+1)
	end := "\n" + strings.Repeat(indent, depth)
	switch v := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
		w.WriteString("{")
		for i, k := range keys {
			if i > 0 {
				w.WriteString(",")
			}
			kb, _ := json.Marshal(k)
			w.WriteString(pad)
			w.Write(kb)
			w.WriteString(" :  ")
			reordered(v[k], indent, depth+1, w)
		}
		w.WriteString(end + "}")
	case []any:
		w.WriteString("[")
		for i, e := range v {
			if i > 0 {
				w.WriteString(",")
			}
			w.WriteString(pad)
			reordered(e, indent, depth+1, w)
		}
		w.WriteString(end + "]")
	default:
		// HTML characters escaped, unlike the original: the same strings.
		b, _ := json.Marshal(v)
		w.Write(b)
	}
}

func TestCatalogueHashIgnoresLayout(t *testing.T) {
	raw := readCatalogue(t)
	c, err := ParseCatalogue(raw)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	var re bytes.Buffer
	reordered(v, "\t", 0, &re)
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"re-indented, keys reversed": re.Bytes(), "compact": compact.Bytes()} {
		if bytes.Equal(b, raw) {
			t.Fatalf("%s: the copy is the original", name)
		}
		c2, err := ParseCatalogue(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c2.Hash() != c.Hash() {
			t.Errorf("%s hashes %s, the original %s", name, c2.Hash(), c.Hash())
		}
	}

	changed := bytes.Replace(raw, []byte(`"kind": "read"`), []byte(`"kind": "write"`), 1)
	c3, err := ParseCatalogue(changed)
	if err != nil {
		t.Fatal(err)
	}
	if c3.Hash() == c.Hash() {
		t.Error("a tool changed its kind and the hash did not change")
	}
}

func TestCanonicalJSON(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{" { \"z\" : [ 1.0 , 1e3 , -0 , 10.50 ] , \"a\" : { } } ", `{"a":{},"z":[1.0,1e3,-0,10.50]}`},
		{`{"s":"x<y>&z","t":"é\n","u":"é"}`, `{"s":"x<y>&z","t":"é\n","u":"é"}`},
		{`[true,false,null,{"B":0,"A":0,"a":0}]`, `[true,false,null,{"A":0,"B":0,"a":0}]`},
		{`"just a string"`, `"just a string"`},
	} {
		var b bytes.Buffer
		if err := canonicalJSON(&b, []byte(tc.in)); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if b.String() != tc.want {
			t.Errorf("%s: got %s, want %s", tc.in, b.String(), tc.want)
		}
	}
	for _, bad := range []string{`{"a":1,"a":2}`, `{"a":1} {}`, `{"a":`, ``, `[1,]`} {
		var b bytes.Buffer
		if err := canonicalJSON(&b, []byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestParseCatalogueRefuses(t *testing.T) {
	for _, bad := range []string{
		`not json`,
		`{}`,
		`{"tools":[{"kind":"read"}]}`,
		`{"tools":[{"name":"a.b","kind":"read"},{"name":"a_b","kind":"read"}]}`,
		`{"tools":[],"tools":[]}`,
	} {
		if _, err := ParseCatalogue([]byte(bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

func TestFetchCatalogue(t *testing.T) {
	raw := readCatalogue(t)
	want, _ := ParseCatalogue(raw)
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tools" || r.Header.Get("Authorization") != "" {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write(raw)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	c, err := FetchCatalogue(ctx, nil, srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if c.Hash() != want.Hash() || c.Len() != 167 {
		t.Fatalf("fetched %d tools, hash %s", c.Len(), c.Hash())
	}

	status = http.StatusServiceUnavailable
	var te *TransientError
	if _, err := FetchCatalogue(ctx, srv.Client(), srv.URL); !errors.As(err, &te) || te.Status != 503 {
		t.Fatalf("503: %v", err)
	}
	status = http.StatusNotFound
	if _, err := FetchCatalogue(ctx, srv.Client(), srv.URL); err == nil || errors.As(err, &te) {
		t.Fatalf("404: %v", err)
	}
	moved := httptest.NewServer(http.RedirectHandler(srv.URL+"/v1/tools", http.StatusFound))
	defer moved.Close()
	if _, err := FetchCatalogue(ctx, moved.Client(), moved.URL); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("a redirect is not followed: %v", err)
	}
	srv.Close()
	if _, err := FetchCatalogue(ctx, nil, srv.URL); !errors.As(err, &te) {
		t.Fatalf("nothing listening: %v", err)
	}
}

// TestCatalogueTakes: an argument a tool takes is one its input schema
// names, as the snapshot's document_text_renew names file_id; not one a
// Core from before names, nor any of a tool it does not offer.
func TestCatalogueTakes(t *testing.T) {
	snap := testCatalogue(t)
	if !snap.Takes(ToolTextRenew, "file_id") || !snap.Takes(ToolTextRenew, "lease_id") || snap.Takes(ToolTextRenew, "on") {
		t.Error("the snapshot's document_text_renew")
	}
	if snap.Takes("no_such_tool", "file_id") || (*Catalogue)(nil).Takes(ToolTextRenew, "file_id") {
		t.Error("a tool not offered takes nothing")
	}
	before, err := ParseCatalogue([]byte(`{"tools":[{"name":"document_text.renew","kind":"ephemeral","method":"POST",` +
		`"path":"/v1/services/document_text/versions/{version_id}/renew","input_schema":{"type":"object","properties":` +
		`{"version_id":{"type":"string"},"lease_id":{"type":"string"}}},"output_schema":{}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if before.Takes(ToolTextRenew, "file_id") || !before.Takes(ToolTextRenew, "lease_id") {
		t.Error("a Core's from before files: file_id")
	}
}
