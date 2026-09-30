package core

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// serviceToken is what the service's fake is called with. Nothing may
// repeat it.
const serviceToken = "aissvc_ixgrh7nbgpyd_ServiceTokenThatMustNotLeak0123"

// serviceSeen is one request the service's fake took.
type serviceSeen struct {
	method, path, query, key, auth string
	body                           map[string]any
}

// serviceCore answers the service's routes as Core #43 answered them when
// the runtime was built against it: each route's answer, by method and
// path, is the status and the body it wrote.
func serviceCore(t *testing.T, answers map[string]func(r *http.Request) (int, string)) (*Service, *[]serviceSeen) {
	t.Helper()
	var mu sync.Mutex
	var seen []serviceSeen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s := serviceSeen{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, key: r.Header.Get("Idempotency-Key"),
			auth: r.Header.Get("Authorization")}
		_ = json.Unmarshal(b, &s.body)
		mu.Lock()
		seen = append(seen, s)
		mu.Unlock()
		answer := answers[r.Method+" "+r.URL.Path]
		if answer == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no such route"}}`))
			return
		}
		status, body := answer(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := NewRESTCaller(RESTOptions{BaseURL: srv.URL, Token: serviceToken, Catalogue: testCatalogue(t), HTTPClient: srv.Client()})
	return NewService(c), &seen
}

const (
	versionID = "01a0f2de-656d-799f-8b6c-51265f8ba567"
	leaseID   = "11488618-d067-4794-8887-18faf084e29d"
)

func fixed(status int, body string) func(*http.Request) (int, string) {
	return func(*http.Request) (int, string) { return status, body }
}

// The four calls go to their routes with the service's token, the claim's
// in the body or the query, a key for the completion alone, and read what
// Core answers.
func TestServiceCalls(t *testing.T) {
	claimed := `{"status":"executed","result":{"claimed":[{"version_id":"` + versionID + `","document_id":"01a0f2de-656c-7264-a680-2f74dbe2bc72",` +
		`"course_id":"01a0f2de-64c0-7d69-bb75-c4a314bf88cd","lease_id":"` + leaseID + `","lease_expires_at":"2026-09-30T15:11:56.447508Z",` +
		`"attempt":2,"backfill":true,"content_type":"application/pdf","byte_size":17,"checksum":"sha256:b22d","download_url":"http://files/x?sig=1",` +
		`"download_expires_at":"2026-09-30T15:25:56.447508475Z"}]}}`
	s, seen := serviceCore(t, map[string]func(*http.Request) (int, string){
		"POST /v1/services/document_text/queue": fixed(200, claimed),
		"GET /v1/services/document_text/versions/" + versionID + "/file": fixed(200, `{"status":"executed","result":{"version_id":"`+versionID+
			`","content_type":"application/pdf","byte_size":17,"checksum":"sha256:b22d","download_url":"http://files/y","download_expires_at":`+
			`"2026-09-30T15:25:56.557331977Z","lease_expires_at":"2026-09-30T15:11:56.447508Z"}}`),
		"POST /v1/services/document_text/versions/" + versionID + "/renew": fixed(200,
			`{"status":"executed","result":{"lease_expires_at":"2026-09-30T15:12:56.594403153Z"}}`),
		"POST /v1/services/document_text/versions/" + versionID + "/complete": fixed(200,
			`{"status":"executed","action_id":"01a0f2de-6745-78c0-95c0-7231e5f6ca3f","review_state":"none","result":{"version_id":"`+versionID+
				`","status":"done","revision":4}}`),
	})
	ctx := t.Context()
	got, err := s.Queue(ctx, 3, 90*time.Second, 25*time.Second)
	if err != nil || len(got) != 1 {
		t.Fatalf("Queue = %+v, %v", got, err)
	}
	c := got[0]
	if c.VersionID != versionID || c.LeaseID != leaseID || c.Attempt != 2 || !c.Backfill || c.ContentType != "application/pdf" ||
		c.ByteSize != 17 || c.DownloadURL != "http://files/x?sig=1" || !c.LeaseExpiresAt.Equal(time.Date(2026, 9, 30, 15, 11, 56, 447508000, time.UTC)) {
		t.Errorf("claimed %+v", c)
	}
	f, err := s.File(ctx, versionID, leaseID)
	if err != nil || f.DownloadURL != "http://files/y" || f.ByteSize != 17 {
		t.Errorf("File = %+v, %v", f, err)
	}
	until, err := s.Renew(ctx, versionID, leaseID, 20*time.Second)
	if err != nil || until.Minute() != 12 {
		t.Errorf("Renew = %s, %v", until, err)
	}
	rev, err := s.Complete(ctx, versionID, leaseID, Completion{Status: TextDone, Body: "## 第 1 頁\n\ntext", Pages: 1, Model: "Flash-Lite"})
	if err != nil || rev != 4 {
		t.Errorf("Complete = %d, %v", rev, err)
	}
	if _, err := s.Complete(ctx, versionID, leaseID, Completion{Status: TextSkipped, Reason: "too_many_pages"}); err != nil {
		t.Errorf("Complete skipped: %v", err)
	}

	calls := *seen
	if len(calls) != 5 {
		t.Fatalf("%d calls", len(calls))
	}
	for _, sc := range calls {
		if sc.auth != "Bearer "+serviceToken {
			t.Errorf("%s %s: not with the service's token", sc.method, sc.path)
		}
	}
	if q := calls[0]; q.key != "" || q.body["max"] != 3.0 || q.body["lease_s"] != 90.0 || q.body["wait_s"] != 25.0 {
		t.Errorf("the queue: %+v", q)
	}
	if f := calls[1]; f.method != http.MethodGet || f.query != "lease_id="+leaseID {
		t.Errorf("the file: %+v", f)
	}
	if r := calls[2]; r.key != "" || r.body["lease_id"] != leaseID || r.body["lease_s"] != 60.0 {
		t.Errorf("the renewal, whose lease is at least a minute: %+v", r)
	}
	done := calls[3]
	if done.key != CompleteKey(versionID, leaseID) || done.body["status"] != "done" || done.body["pages"] != 1.0 ||
		done.body["model"] != "Flash-Lite" || done.body["reason"] != nil || done.body["idempotency_key"] != nil {
		t.Errorf("the completion: %+v", done)
	}
	if sk := calls[4]; sk.body["reason"] != "too_many_pages" || sk.body["body"] != nil || sk.body["pages"] != nil {
		t.Errorf("the skip: %+v", sk)
	}
}

// Core's refusals come back as ServiceErrors by their reasons, whether
// the call was attempted (a completion, recorded) or not (a renewal); a
// 401 is ErrUnauthenticated.
func TestServiceRefusals(t *testing.T) {
	var answer func(*http.Request) (int, string)
	s, _ := serviceCore(t, map[string]func(*http.Request) (int, string){
		"POST /v1/services/document_text/versions/" + versionID + "/renew":    func(r *http.Request) (int, string) { return answer(r) },
		"POST /v1/services/document_text/versions/" + versionID + "/complete": func(r *http.Request) (int, string) { return answer(r) },
		"POST /v1/services/document_text/queue":                               func(r *http.Request) (int, string) { return answer(r) },
	})
	ctx := t.Context()
	for _, c := range []struct {
		name   string
		status int
		body   string
		call   func() error
		reason string
		code   string
	}{
		{"a renewal of a claim lost", 409, `{"error":{"code":"conflict","message":"the claim no longer holds","details":{"reason":"lease_lost"}}}`,
			func() error { _, err := s.Renew(ctx, versionID, leaseID, time.Minute); return err }, ReasonLeaseLost, CodeConflict},
		{"a renewal of a text staff wrote", 409, `{"error":{"code":"conflict","message":"staff have written the text","details":{"reason":"edited_by_staff"}}}`,
			func() error { _, err := s.Renew(ctx, versionID, leaseID, time.Minute); return err }, ReasonEditedByStaff, CodeConflict},
		{"a completion of a text staff wrote", 409,
			`{"status":"failed","action_id":"01a0f2de-6692-7d3b-86c1-1e152a1250c5","review_state":"none","error":{"code":"conflict","message":"staff have written the text","details":{"reason":"edited_by_staff"}}}`,
			func() error {
				_, err := s.Complete(ctx, versionID, leaseID, Completion{Status: TextDone, Body: "x", Pages: 1, Model: "m"})
				return err
			}, ReasonEditedByStaff, CodeConflict},
		{"a version purged", 404, `{"error":{"code":"not_found","message":"no such text version"}}`,
			func() error { _, err := s.Renew(ctx, versionID, leaseID, time.Minute); return err }, "", CodeNotFound},
		{"a person's token", 403, `{"status":"denied","error":{"code":"forbidden","message":"not permitted","details":{"reason":"service_only"}}}`,
			func() error { _, err := s.Queue(ctx, 1, 0, 0); return err }, ReasonServiceOnly, CodeForbidden},
	} {
		answer = fixed(c.status, c.body)
		err := c.call()
		var se *ServiceError
		if !errors.As(err, &se) || se.Reason != c.reason || se.Code != c.code || !IsReason(err, c.reason) {
			t.Errorf("%s: %v", c.name, err)
		}
		if c.code == CodeNotFound && !IsNotFound(err) {
			t.Errorf("%s: not IsNotFound", c.name)
		}
		if strings.Contains(err.Error(), serviceToken) {
			t.Errorf("%s: the error holds the token", c.name)
		}
	}
	answer = fixed(401, `{"error":{"code":"unauthenticated","message":"the credential is missing or not valid"}}`)
	if _, err := s.Queue(ctx, 1, 0, 0); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a credential revoked: %v", err)
	}
}

// A catalogue offers the service when it has its four tools: a Core since
// #43.
func TestHasService(t *testing.T) {
	if !HasService(testCatalogue(t)) {
		t.Error("the pinned catalogue has no service")
	}
	raw := readCatalogue(t)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var kept []any
	for _, tl := range doc["tools"].([]any) {
		if tl.(map[string]any)["name"] != "document_text.queue" {
			kept = append(kept, tl)
		}
	}
	doc["tools"] = kept
	b, _ := json.Marshal(doc)
	old, err := ParseCatalogue(b)
	if err != nil {
		t.Fatal(err)
	}
	if HasService(old) || HasService(nil) {
		t.Error("a Core without the queue offers the service")
	}
}
