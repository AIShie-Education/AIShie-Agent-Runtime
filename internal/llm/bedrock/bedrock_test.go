package bedrock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

var signingTime = time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)

// seen is one request as the test server received it.
type seen struct {
	method, requestURI, host string
	header                   http.Header
	body                     []byte
}

// serve answers every request with status, header and body, and hands
// each request it saw to the test through the returned channel.
func serve(t *testing.T, status int, header map[string]string, body string) (*httptest.Server, chan seen) {
	t.Helper()
	ch := make(chan seen, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- seen{method: r.Method, requestURI: r.RequestURI, host: r.Host, header: r.Header.Clone(), body: b}
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

const okBody = `{"output":{"message":{"role":"assistant","content":[{"text":"Lexing splits text into tokens."}]}},
	"stopReason":"end_turn","usage":{"inputTokens":12,"outputTokens":7,"totalTokens":19},"metrics":{"latencyMs":300}}`

func TestCallSignsWithSigV4EndToEnd(t *testing.T) {
	srv, ch := serve(t, http.StatusOK, map[string]string{"X-Amzn-RequestId": "7f1c1f2e-0000-4000-8000-000000000001"}, okBody)
	model := "arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	creds := credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "FwoGZXIvYXdzEXAMPLETOKEN")
	a, err := NewWithCredentials(llm.Config{Model: model, Region: testRegion, BaseURL: srv.URL,
		Headers: map[string]string{"X-Tenant": "ten_instr_42"}}, creds)
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return signingTime }

	resp, err := a.Call(context.Background(), &llm.Request{System: "Be brief.", Messages: []llm.Message{llm.UserText("What is a lexer?")}})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Text() != "Lexing splits text into tokens." || resp.Stop != llm.StopEnd || resp.RawStop != "end_turn" {
		t.Errorf("response = %+v", resp)
	}
	if resp.RequestID != "7f1c1f2e-0000-4000-8000-000000000001" {
		t.Errorf("RequestID = %q", resp.RequestID)
	}
	if resp.Usage.Input != 12 || resp.Usage.Output != 7 || string(resp.Usage.Raw) != `{"inputTokens":12,"outputTokens":7,"totalTokens":19}` {
		t.Errorf("usage = %+v (raw %s)", resp.Usage, resp.Usage.Raw)
	}

	got := <-ch
	wantURI := "/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A123456789012%3Ainference-profile%2Fus.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse"
	if got.method != http.MethodPost || got.requestURI != wantURI {
		t.Errorf("request = %s %s, want POST %s", got.method, got.requestURI, wantURI)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if got.header.Get("X-Tenant") != "ten_instr_42" {
		t.Errorf("the configured header is missing: %v", got.header)
	}
	auth := got.header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260927/us-east-1/bedrock/aws4_request, SignedHeaders=") {
		t.Errorf("Authorization = %q", auth)
	}
	signed := signedHeaders(auth)
	for _, h := range []string{"host", "x-amz-date", "x-amz-security-token", "content-type", "x-tenant"} {
		if !strings.Contains(";"+signed+";", ";"+h+";") {
			t.Errorf("SignedHeaders %q lacks %s", signed, h)
		}
	}
	if got.header.Get("X-Amz-Date") != "20260927T080000Z" || got.header.Get("X-Amz-Security-Token") != "FwoGZXIvYXdzEXAMPLETOKEN" {
		t.Errorf("X-Amz-Date = %q, X-Amz-Security-Token = %q", got.header.Get("X-Amz-Date"), got.header.Get("X-Amz-Security-Token"))
	}
	// The signature covers the request as it arrived: the escaped path,
	// the headers and the body, recomputed here from what the server saw.
	v, _ := creds.Retrieve(context.Background())
	if want := resign(t, got, v); want != auth {
		t.Errorf("the signature does not match the request received:\n got  %s\n want %s", auth, want)
	}
	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if _, ok := body["toolConfig"]; ok {
		t.Errorf("toolConfig sent without tools: %s", got.body)
	}
}

func signedHeaders(auth string) string {
	_, after, _ := strings.Cut(auth, "SignedHeaders=")
	s, _, _ := strings.Cut(after, ",")
	return s
}

// resign signs, with the same credentials and time, a request rebuilt from
// what the server received, and returns its Authorization.
func resign(t *testing.T, got seen, creds aws.Credentials) string {
	t.Helper()
	req, err := http.NewRequest(got.method, "http://"+got.host+got.requestURI, bytes.NewReader(got.body))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range strings.Split(signedHeaders(got.header.Get("Authorization")), ";") {
		if h != "host" && h != "content-length" {
			req.Header.Set(h, got.header.Get(h))
		}
	}
	sum := sha256.Sum256(got.body)
	if err := v4.NewSigner().SignHTTP(context.Background(), creds, req, hex.EncodeToString(sum[:]), "bedrock", testRegion, signingTime); err != nil {
		t.Fatal(err)
	}
	return req.Header.Get("Authorization")
}

func TestSignFuncAddsSigV4Headers(t *testing.T) {
	cases := []struct {
		name, session string
		wantToken     bool
	}{
		{"long-term credentials", "", false},
		{"session credentials", "FwoGZXIvYXdzEXAMPLETOKEN", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", tc.session)
			a, err := NewWithCredentials(llm.Config{Model: claude, Region: "eu-central-1"}, provider)
			if err != nil {
				t.Fatal(err)
			}
			a.now = func() time.Time { return signingTime }
			creds, err := provider.Retrieve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"messages":[]}`)
			req, _ := http.NewRequest(http.MethodPost, a.endpoint, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if err := a.signFunc(body, creds)(req); err != nil {
				t.Fatalf("sign: %v", err)
			}
			auth := req.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260927/eu-central-1/bedrock/aws4_request, SignedHeaders=") ||
				!strings.Contains(auth, ", Signature=") {
				t.Errorf("Authorization = %q", auth)
			}
			if d := req.Header.Get("X-Amz-Date"); d != "20260927T080000Z" {
				t.Errorf("X-Amz-Date = %q", d)
			}
			token := req.Header.Get("X-Amz-Security-Token")
			if (token != "") != tc.wantToken || (tc.wantToken && token != tc.session) {
				t.Errorf("X-Amz-Security-Token = %q, want it %v", token, tc.wantToken)
			}
			if strings.Contains(auth, "wJalrXUtnFEMI") {
				t.Error("the secret key is in the Authorization header")
			}
		})
	}
}

func TestSignFuncReportsMissingCredentials(t *testing.T) {
	a, err := NewWithCredentials(llm.Config{Model: claude, Region: testRegion},
		aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{}, errors.New("no EC2 IMDS role found")
		}))
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := serve(t, http.StatusOK, nil, okBody)
	a.endpoint = srv.URL + "/model/x/converse"
	_, err = a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hi")}})
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrAuth || !strings.Contains(e.Message, "no EC2 IMDS role found") {
		t.Errorf("err = %v", err)
	}
}

func TestCredentialFailuresAreClassified(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, nil, okBody)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	// AWS's cache fetches under a context that never ends, so the
	// provider is released when the test ends, and no goroutine outlives it.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	blocking := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		<-release
		return aws.Credentials{}, errors.New("released")
	})
	unreachable := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, fmt.Errorf("operation error STS: AssumeRoleWithWebIdentity: %w",
			&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})
	})
	cases := []struct {
		name  string
		ctx   context.Context
		creds aws.CredentialsProvider
		want  llm.ErrorKind
	}{
		{"the call's context ends first", cancelled, blocking, llm.ErrTimeout},
		{"the chain cannot reach its source", context.Background(), unreachable, llm.ErrNetwork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := NewWithCredentials(llm.Config{Model: claude, Region: testRegion, BaseURL: srv.URL}, tc.creds)
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.Call(tc.ctx, &llm.Request{Messages: []llm.Message{llm.UserText("Hi")}})
			var e *llm.Error
			if !errors.As(err, &e) || e.Kind != tc.want {
				t.Errorf("err = %v, want %s", err, tc.want)
			}
		})
	}
}

// AWS's signature errors repeat the canonical request, signed headers and
// all; the session token must not reach the error, even cut short.
func TestRefusalsDoNotRepeatCredentials(t *testing.T) {
	const token = "IQoJb3JpZ2luX2VjEXAMPLESESSIONTOKENxyz0123456789"
	const key = "ABSKQmVkcm9ja0FQSUtleS1zZWNyZXQ="
	echo := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amzn-ErrorType", "InvalidSignatureException")
		w.WriteHeader(http.StatusForbidden)
		msg := "The request signature we calculated does not match the signature you provided. " +
			"Check your AWS Secret Access Key and signing method. Consult the service documentation for details.\n\n" +
			"The Canonical String for this request should have been\n'POST\n" + r.URL.EscapedPath() + "\n\n" +
			"content-type:application/json\nhost:" + r.Host + "\nx-amz-date:" + r.Header.Get("X-Amz-Date") +
			"\nx-amz-security-token:" + r.Header.Get("X-Amz-Security-Token") + "\n'"
		_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
	}
	bearer := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amzn-ErrorType", "UnrecognizedClientException")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "The bearer " + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + " is not valid."})
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		apiKey  string
		secret  string
	}{
		{"a signature error under session credentials", echo, "", token},
		{"a key repeated back", bearer, key, key},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			cfg := llm.Config{Model: "m", Region: testRegion, BaseURL: srv.URL}
			var a *Adapter
			var err error
			if tc.apiKey != "" {
				cfg.APIKey = tc.apiKey
				a, err = New(cfg)
			} else {
				a, err = NewWithCredentials(cfg, credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", token))
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hi")}})
			var e *llm.Error
			if !errors.As(err, &e) || e.Kind != llm.ErrAuth {
				t.Fatalf("err = %v, want auth", err)
			}
			if strings.Contains(err.Error(), tc.secret[:12]) {
				t.Errorf("the error repeats a credential: %v", err)
			}
			if !strings.Contains(e.Message, "does not match") && !strings.Contains(e.Message, "is not valid") {
				t.Errorf("the provider's message is gone: %q", e.Message)
			}
		})
	}
}

func TestScrub(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Rate exceeded", "Rate exceeded"},
		{"Signature mismatch.\n\nThe Canonical String for this request should have been\n'POST\n/x\nx-amz-security-token:IQo…",
			"Signature mismatch."},
		{"bad header x-amz-security-token: IQoJb3JpZ2lu and more\nnext line", "bad header x-amz-security-token: [redacted]\nnext line"},
		{"Authorization=AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260927, Signature=abc'", "Authorization=[redacted]'"},
		{"the key sk-12345 was refused", "the key [redacted] was refused"},
	}
	for _, tc := range cases {
		if got := scrub(tc.in, "sk-12345", ""); got != tc.want {
			t.Errorf("scrub(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCallWithAPIKeySendsABearer(t *testing.T) {
	srv, ch := serve(t, http.StatusOK, nil, okBody)
	a := newTestAdapter(t, func(c *llm.Config) {
		c.APIKey = "ABSKQmVkcm9ja0FQSUtleS1leGFtcGxl"
		c.BaseURL = srv.URL + "/"
		c.Headers = map[string]string{"authorization": "Bearer not-this-one", "AUTHORIZATION": "Bearer nor-this"}
	})
	if _, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hi")}}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	got := <-ch
	if auth := got.header.Get("Authorization"); auth != "Bearer ABSKQmVkcm9ja0FQSUtleS1leGFtcGxl" {
		t.Errorf("Authorization = %q", auth)
	}
	if got.header.Get("X-Amz-Date") != "" {
		t.Error("a request under an API key was signed as well")
	}
	if got.requestURI != "/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse" {
		t.Errorf("path = %q", got.requestURI)
	}
}

func TestCallClassifiesRefusals(t *testing.T) {
	const key = "ABSKQmVkcm9ja0FQSUtleS1zZWNyZXQ="
	cases := []struct {
		name     string
		status   int
		errType  string // x-amzn-ErrorType
		body     string
		wantKind llm.ErrorKind
		wantCode string
	}{
		{"throttled", 429, "ThrottlingException:http://internal.amazon.com/coral/com.amazon.bedrock/", `{"message":"Too many requests, please wait before trying again."}`, llm.ErrRateLimited, "ThrottlingException"},
		{"quota", 400, "ServiceQuotaExceededException", `{"message":"Too many tokens per day, please wait before trying again."}`, llm.ErrRateLimited, "ServiceQuotaExceededException"},
		{"unavailable", 503, "ServiceUnavailableException", `{"message":"Service unavailable."}`, llm.ErrOverloaded, "ServiceUnavailableException"},
		{"model not ready", 429, "ModelNotReadyException", `{"message":"Model is not ready for inference."}`, llm.ErrOverloaded, "ModelNotReadyException"},
		{"access denied", 403, "AccessDeniedException", `{"message":"You don't have access to the model with the specified model ID."}`, llm.ErrAuth, "AccessDeniedException"},
		{"bad key", 403, "UnrecognizedClientException", `{"message":"The security token included in the request is invalid."}`, llm.ErrAuth, "UnrecognizedClientException"},
		{"expired", 400, "ExpiredTokenException", `{"message":"The security token included in the request is expired"}`, llm.ErrAuth, "ExpiredTokenException"},
		{"too long", 400, "ValidationException", `{"message":"Input is too long for requested model."}`, llm.ErrContextOverflow, "ValidationException"},
		{"prompt too long", 400, "ValidationException", `{"message":"The model returned the following errors: prompt is too long: 215000 tokens > 200000 maximum"}`, llm.ErrContextOverflow, "ValidationException"},
		{"context limit", 400, "ValidationException", `{"message":"The model returned the following errors: input length and max_tokens exceed context limit: 197626 + 8192 > 200000"}`, llm.ErrContextOverflow, "ValidationException"},
		{"validation", 400, "ValidationException", `{"message":"A conversation must alternate between user and assistant roles."}`, llm.ErrBadRequest, "ValidationException"},
		{"model timeout", 408, "ModelTimeoutException", `{"message":"Model has timed out in processing the request."}`, llm.ErrTimeout, "ModelTimeoutException"},
		{"model error", 424, "ModelErrorException", `{"message":"The model encountered an error."}`, llm.ErrServer, "ModelErrorException"},
		{"internal", 500, "InternalServerException", `{"Message":"Internal server error"}`, llm.ErrServer, "InternalServerException"},
		{"type in body only", 429, "", `{"__type":"com.amazonaws.bedrock#ThrottlingException","message":"Rate exceeded"}`, llm.ErrRateLimited, "ThrottlingException"},
		{"unknown type", 404, "ResourceNotFoundException", `{"message":"Could not resolve the foundation model."}`, llm.ErrBadRequest, "ResourceNotFoundException"},
		{"no type", 502, "", `<html>Bad Gateway</html>`, llm.ErrServer, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := map[string]string{"X-Amzn-RequestId": "req-1"}
			if tc.errType != "" {
				h["X-Amzn-ErrorType"] = tc.errType
			}
			srv, _ := serve(t, tc.status, h, tc.body)
			a := newTestAdapter(t, func(c *llm.Config) { c.APIKey = key; c.BaseURL = srv.URL })
			resp, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hi")}})
			var e *llm.Error
			if resp != nil || !errors.As(err, &e) {
				t.Fatalf("Call = %v, %v; want an *llm.Error", resp, err)
			}
			if e.Kind != tc.wantKind || e.Code != tc.wantCode || e.Status != tc.status {
				t.Errorf("error = kind %s, code %q, status %d; want %s, %q, %d", e.Kind, e.Code, e.Status, tc.wantKind, tc.wantCode, tc.status)
			}
			if strings.Contains(err.Error(), key) {
				t.Errorf("the key is in the error: %v", err)
			}
		})
	}
}

func TestCallRejectsAnUnreadableAnswer(t *testing.T) {
	for _, body := range []string{
		`{"output":{"message":{"content":[{"text":7}]}}}`,
		`{"output":{"message":{"content":[{"toolUse":["not","an","object"]}]}},"stopReason":"tool_use"}`,
		`{"output":{"message":{"content":[{"reasoningContent":"text"}]}},"stopReason":"end_turn"}`,
		`{"output":{"message":{"content":[{"text":"Hi"}]}},"stopReason":"end_turn","usage":{"inputTokens":"12"}}`,
		`{"output":`,
	} {
		srv, _ := serve(t, http.StatusOK, nil, body)
		a := newTestAdapter(t, func(c *llm.Config) { c.BaseURL = srv.URL })
		_, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hi")}})
		var e *llm.Error
		if !errors.As(err, &e) || e.Kind != llm.ErrServer {
			t.Errorf("%s: err = %v, want a server error", body, err)
		}
	}
}

func TestCallRefusesAnEmptyRequest(t *testing.T) {
	a := newTestAdapter(t)
	for _, req := range []*llm.Request{nil, {}, {Messages: []llm.Message{llm.UserText("  "), {Role: llm.RoleAssistant}}}} {
		_, err := a.Call(context.Background(), req)
		var e *llm.Error
		if !errors.As(err, &e) || e.Kind != llm.ErrBadRequest {
			t.Errorf("Call(%v) err = %v, want bad_request", req, err)
		}
	}
	_, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hi")},
		Tools: []llm.Tool{{Name: "course_get", Schema: json.RawMessage(`{"type":`)}}, ToolMode: llm.ToolAuto})
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrBadRequest {
		t.Errorf("a tool with a broken schema: err = %v, want bad_request", err)
	}
}

// Each call keeps its own response header, so calls at once on one
// adapter classify their own refusals.
func TestConcurrentCallsKeepTheirOwnErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if bytes.Contains(b, []byte("throttle")) {
			w.Header().Set("X-Amzn-ErrorType", "ThrottlingException")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"message":"slow down"}`)
			return
		}
		w.Header().Set("X-Amzn-ErrorType", "AccessDeniedException")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"no"}`)
	}))
	defer srv.Close()
	a := newTestAdapter(t, func(c *llm.Config) { c.BaseURL = srv.URL })
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			word, want := "deny", llm.ErrAuth
			if i%2 == 0 {
				word, want = "throttle", llm.ErrRateLimited
			}
			_, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText(fmt.Sprintf("%s %d", word, i))}})
			var e *llm.Error
			if !errors.As(err, &e) || e.Kind != want {
				t.Errorf("call %d: err = %v, want %s", i, err, want)
			}
		}()
	}
	wg.Wait()
}

func TestNewTakesTheDefaultCredentialChain(t *testing.T) {
	isolateAWS(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDFROMENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secretfromenvEXAMPLE")
	t.Setenv("AWS_SESSION_TOKEN", "tokenfromenv")
	t.Setenv("AWS_REGION", "eu-west-3")
	srv, ch := serve(t, http.StatusOK, nil, okBody)
	a, err := New(llm.Config{Model: claude, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hi")}}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	got := <-ch
	if auth := got.header.Get("Authorization"); !strings.Contains(auth, "Credential=AKIDFROMENV/") || !strings.Contains(auth, "/eu-west-3/bedrock/aws4_request") {
		t.Errorf("Authorization = %q", auth)
	}
	if got.header.Get("X-Amz-Security-Token") != "tokenfromenv" {
		t.Errorf("X-Amz-Security-Token = %q", got.header.Get("X-Amz-Security-Token"))
	}
}

// isolateAWS keeps AWS's configuration chain from reading this machine's
// files, profile or instance role.
func isolateAWS(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for k, v := range map[string]string{
		"AWS_CONFIG_FILE": dir + "/config", "AWS_SHARED_CREDENTIALS_FILE": dir + "/credentials",
		"AWS_EC2_METADATA_DISABLED": "true", "AWS_PROFILE": "", "AWS_DEFAULT_PROFILE": "",
		"AWS_REGION": "", "AWS_DEFAULT_REGION": "", "AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "",
		"AWS_SESSION_TOKEN": "", "AWS_BEARER_TOKEN_BEDROCK": "", "AWS_WEB_IDENTITY_TOKEN_FILE": "",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "", "AWS_CONTAINER_CREDENTIALS_FULL_URI": "",
	} {
		t.Setenv(k, v)
	}
}

// Under an API key, AWS's configuration is read only for a region; one
// that does not read (a profile that is not there) is no reason to refuse.
func TestNewWithAPIKeyToleratesABrokenAWSConfiguration(t *testing.T) {
	isolateAWS(t)
	t.Setenv("AWS_PROFILE", "no-such-profile")
	if _, err := New(llm.Config{Model: claude, APIKey: "k", BaseURL: "https://proxy.example/bedrock"}); err != nil {
		t.Errorf("New under an API key: %v", err)
	}
	if _, err := New(llm.Config{Model: claude, APIKey: "k"}); err == nil || !strings.Contains(err.Error(), "no region") {
		t.Errorf("New under an API key for the default host with no region: err = %v", err)
	}
	if _, err := New(llm.Config{Model: claude, Region: testRegion}); err == nil || !strings.Contains(err.Error(), "AWS configuration") {
		t.Errorf("New under SigV4 with a broken profile: err = %v, want the configuration's error", err)
	}
}

func TestNewValidates(t *testing.T) {
	isolateAWS(t)
	creds := credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "")
	cases := []struct {
		name    string
		cfg     llm.Config
		creds   aws.CredentialsProvider
		wantErr string
	}{
		{"no model", llm.Config{Region: testRegion}, creds, "no model id"},
		{"no region under SigV4", llm.Config{Model: claude, BaseURL: "https://vpce-1.bedrock-runtime.us-east-1.vpce.amazonaws.com"}, creds, "no region"},
		{"no region for the default host", llm.Config{Model: claude, APIKey: "k"}, nil, "no region"},
		{"a region that is not one", llm.Config{Model: claude, Region: "us-east-1.evil.example/x"}, creds, "is not an AWS region"},
		{"a base URL that is not one", llm.Config{Model: claude, Region: testRegion, BaseURL: "bedrock.local"}, creds, "is not an http(s) URL"},
		{"a base URL with a query", llm.Config{Model: claude, Region: testRegion, BaseURL: "https://proxy.example?x=1"}, creds, "is not an http(s) URL"},
		{"an unknown dialect", llm.Config{Model: claude, Region: testRegion, Dialect: "yaml"}, creds, "unknown schema dialect"},
		{"a key and credentials", llm.Config{Model: claude, Region: testRegion, APIKey: "k"}, creds, "both an API key and SigV4 credentials"},
		{"a bearer to a base URL needs no region", llm.Config{Model: claude, APIKey: "k", BaseURL: "https://proxy.example/bedrock"}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.creds != nil {
				_, err = NewWithCredentials(tc.cfg, tc.creds)
			} else {
				_, err = New(tc.cfg)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("err = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
	if _, err := NewWithCredentials(llm.Config{Model: claude, Region: testRegion}, nil); err == nil {
		t.Error("NewWithCredentials took no credentials")
	}
}

func TestAdapterDescribesItself(t *testing.T) {
	cases := []struct {
		name         string
		cfg          func(*llm.Config)
		wantEndpoint string
		wantMaker    string
		wantDialect  toolschema.Dialect
		wantFiles    bool
	}{
		{"defaults", nil,
			"https://bedrock-runtime.us-east-1.amazonaws.com/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse",
			"bedrock_converse|https://bedrock-runtime.us-east-1.amazonaws.com|" + claude, toolschema.Bedrock, false},
		{"china", func(c *llm.Config) { c.Region = "cn-north-1"; c.Model = nova },
			"https://bedrock-runtime.cn-north-1.amazonaws.com.cn/model/amazon.nova-pro-v1%3A0/converse",
			"bedrock_converse|https://bedrock-runtime.cn-north-1.amazonaws.com.cn|" + nova, toolschema.Bedrock, false},
		{"a VPC endpoint, overrides", func(c *llm.Config) {
			c.BaseURL = "https://vpce-0a1b.bedrock-runtime.us-east-1.vpce.amazonaws.com/"
			c.Dialect = toolschema.FullCommon
			c.Capabilities = llm.CapabilityOverrides{FileInput: ptr(true), ToolChoiceNone: ptr(true), ParallelToolCalls: ptr(false)}
		},
			"https://vpce-0a1b.bedrock-runtime.us-east-1.vpce.amazonaws.com/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse",
			"bedrock_converse|https://vpce-0a1b.bedrock-runtime.us-east-1.vpce.amazonaws.com|" + claude, toolschema.FullCommon, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var opts []func(*llm.Config)
			if tc.cfg != nil {
				opts = append(opts, tc.cfg)
			}
			a := newTestAdapter(t, opts...)
			if a.endpoint != tc.wantEndpoint {
				t.Errorf("endpoint = %s, want %s", a.endpoint, tc.wantEndpoint)
			}
			if a.Maker() != tc.wantMaker || a.Dialect() != tc.wantDialect {
				t.Errorf("Maker = %s, Dialect = %s", a.Maker(), a.Dialect())
			}
			if a.Name() != llm.AdapterBedrockConverse || a.Provider() != llm.ProviderBedrock || !strings.HasSuffix(tc.wantMaker, "|"+a.Model()) {
				t.Errorf("Name = %s, Provider = %s, Model = %s", a.Name(), a.Provider(), a.Model())
			}
			c := a.Capabilities()
			if !c.ToolsWithHistory || c.ToolChoiceNone || c.FileInput != tc.wantFiles {
				t.Errorf("capabilities = %+v", c)
			}
		})
	}
	var _ llm.Adapter = (*Adapter)(nil)
}
