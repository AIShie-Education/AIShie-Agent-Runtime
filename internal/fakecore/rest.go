package fakecore

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/jsonstrict"
)

// REST, as Core's httpapi answers it: every tool at its catalogue route and
// at POST /v1/tools/{name}, writes with an Idempotency-Key header, and the
// outcome's HTTP status (Core's README, "The API in one paragraph").

const (
	headerIdempotencyKey = "Idempotency-Key"
	headerReplayed       = "Idempotency-Replayed"
	maxRESTBody          = 1 << 20
)

// schemaVersion is the database schema of the Core the catalogue was taken
// from, which Core's /healthz reports.
const schemaVersion = 8

// blobPath is where the files document_get points at are served, as Core's
// own file store serves them.
const blobPath = "/v1/blobs/"

func (c *Core) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": Version, "commit": Version,
			"schema_version": schemaVersion, "schema_latest": schemaVersion})
	})
	mux.HandleFunc("GET /v1/tools", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(c.cat.raw)
	})
	mux.Handle("POST /v1/tools/{tool_name}", c.restAuth(c.callByName))
	for _, t := range c.cat.tools {
		if t.Path != "" {
			mux.Handle(t.Method+" "+t.Path, c.restAuth(c.restTool(t, t.Method, t.Path)))
		}
	}
	mux.Handle("/mcp", c.mcpHandler())
	mux.HandleFunc("GET "+blobPath+"{token}", c.serveBlob)
	return routed(mux)
}

// routed answers, in JSON, for the routes the mux does not have.
func routed(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		probe := &recorder{header: http.Header{}}
		h.ServeHTTP(probe, r)
		if probe.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", probe.header.Get("Allow"))
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": newErr("method_not_allowed",
				"%s is not something %s takes; it takes %s", r.Method, r.URL.Path, probe.header.Get("Allow"))})
			return
		}
		writeError(w, missing("no such route; GET /v1/tools lists what there is"))
	})
}

type restCallerKey struct{}

// restAuth checks the bearer token and the rate limit, as Core's
// authenticated does: after authentication, so the limit is the actor's;
// before the call, so a refusal records nothing.
func (c *Core) restAuth(next http.HandlerFunc) http.Handler {
	return c.refusedLogged("rest", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		var a *actor
		if ok && strings.EqualFold(scheme, "Bearer") {
			a = c.authenticate(strings.TrimSpace(token))
		}
		if a == nil {
			writeError(w, newErr(codeUnauthenticated, "the credential is missing or not valid"))
			return
		}
		if ok, wait := c.limiter.allow(a.id); !ok {
			secs := max(int(math.Ceil(wait.Seconds())), 1)
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeError(w, newErr(codeRateLimited, "too many calls; try again in %d seconds", secs).with("retry_after_seconds", secs))
			c.logRefused(&peek{transport: "rest", actorID: a.id, method: r.Method, tool: c.routeTool(r)}, http.StatusTooManyRequests)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), restCallerKey{}, a.id)))
	}))
}

func (c *Core) callByName(w http.ResponseWriter, r *http.Request) {
	t := c.cat.byName[r.PathValue("tool_name")]
	if t == nil {
		writeError(w, missing("there is no tool named %q", r.PathValue("tool_name")))
		return
	}
	c.restTool(t, http.MethodPost, "")(w, r)
}

// restTool answers one tool's route: its arguments gathered from the body,
// the query and the path, then the same pipeline as MCP.
func (c *Core) restTool(t *toolDef, method, pattern string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actorID, _ := r.Context().Value(restCallerKey{}).(string)
		if len(r.Header.Values(headerIdempotencyKey)) > 1 {
			writeError(w, invalid("%s is given more than once", headerIdempotencyKey))
			return
		}
		args, err := buildArgs(t, method, pattern, r)
		if err != nil {
			if e, ok := asAPI(err); ok {
				writeError(w, e)
			} else {
				writeError(w, invalid("the body is larger than %d bytes", maxRESTBody))
			}
			return
		}
		key := r.Header.Get(headerIdempotencyKey)
		p := &peek{transport: "rest", actorID: actorID, method: r.Method, tool: t.mcpName, args: args, key: key}
		delayAfter, ok := c.admit(w, r, p)
		if !ok {
			return
		}
		out := c.serve(r.Context(), actorID, "rest", t, args, args, key, c.baseURL(r))
		// Carried out; the answer is held as a slow network would hold it.
		sleep(context.WithoutCancel(r.Context()), delayAfter)
		if out.Status == "error" {
			// A tool that bounds its own rate (conversation.draft) says
			// when to try again, as the limit does.
			if secs, ok := out.Error.Details["retry_after_seconds"].(int); ok && out.Error.Code == codeRateLimited {
				w.Header().Set("Retry-After", strconv.Itoa(secs))
			}
			writeError(w, out.Error)
			return
		}
		if out.Replayed {
			w.Header().Set(headerReplayed, "true")
		}
		writeJSON(w, outcomeStatus(out), out)
	}
}

// routeTool is the MCP name of the tool a REST request is for, for the log.
func (c *Core) routeTool(r *http.Request) string {
	if name := r.PathValue("tool_name"); name != "" {
		return mcpName(name)
	}
	for _, t := range c.cat.tools {
		if t.Method+" "+t.Path == r.Pattern {
			return t.mcpName
		}
	}
	return ""
}

// admit runs a REST call past the test's injection, which over REST comes
// after the rate limit: the arguments are known only then. It reports
// whether the call is to go on, and how long its answer is then to be held.
func (c *Core) admit(w http.ResponseWriter, r *http.Request, p *peek) (time.Duration, bool) {
	c.hooks.RLock()
	f := c.inject
	c.hooks.RUnlock()
	if f == nil {
		return 0, true
	}
	in := f(InjectedCall{ActorID: p.actorID, Transport: "rest", Method: p.method, Tool: p.tool, Args: p.args, IdempotencyKey: p.key})
	if in == nil {
		return 0, true
	}
	if !sleep(r.Context(), in.Delay) {
		return 0, false
	}
	if in.Status != 0 {
		writeInjected(w, "rest", in)
		c.logRefused(p, in.Status)
		return 0, false
	}
	return in.DelayAfter, true
}

// outcomeStatus is an attempted call's HTTP status.
func outcomeStatus(out outcome) int {
	switch out.Status {
	case actExecuted:
		return http.StatusOK
	case actProposed:
		return http.StatusAccepted
	case actDenied:
		return http.StatusForbidden
	}
	if out.Error != nil {
		return codeStatus(out.Error.Code)
	}
	return http.StatusConflict
}

func codeStatus(code string) int {
	switch code {
	case codeRateLimited:
		return http.StatusTooManyRequests
	case codeInvalidArgument:
		return http.StatusBadRequest
	case codeUnauthenticated:
		return http.StatusUnauthorized
	case codeForbidden:
		return http.StatusForbidden
	case codeNotFound:
		return http.StatusNotFound
	case codeConflict, codeIdempotencyConflict:
		return http.StatusConflict
	case codeFailedPrecondition:
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers a call that was never attempted.
func writeError(w http.ResponseWriter, e *apiError) {
	if e.Code == codeUnauthenticated {
		w.Header().Set("WWW-Authenticate", `Bearer realm="aishiteru"`)
	}
	writeJSON(w, codeStatus(e.Code), map[string]any{"error": e})
}

var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// buildArgs assembles a tool's arguments from the JSON body (POST), the
// query string (GET) and the path, into the object an MCP client would have
// sent. The path wins, and a body that disagrees with it is refused.
func buildArgs(t *toolDef, method, pattern string, r *http.Request) ([]byte, error) {
	args := map[string]any{}
	if method == http.MethodGet {
		for name, values := range r.URL.Query() {
			args[name] = coerce(t.props[name], values)
		}
	} else {
		body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxRESTBody))
		if err != nil {
			return nil, err
		}
		if len(body) > 0 {
			if err := jsonstrict.Check(body); err != nil {
				return nil, invalid("the body: %v", err)
			}
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.UseNumber()
			if err := dec.Decode(&args); err != nil {
				return nil, invalid("the body must be a JSON object: %v", err)
			}
			if args == nil {
				return nil, invalid("the body must be a JSON object, not null")
			}
			if _, err := dec.Token(); err != io.EOF {
				return nil, invalid("the body must be one JSON object, with nothing after it")
			}
		}
	}
	for _, m := range pathParam.FindAllStringSubmatch(pattern, -1) {
		name, value := m[1], r.PathValue(m[1])
		if prior, ok := args[name]; ok && prior != value {
			return nil, invalid("%s in the request is not the one in the path; they must agree", name)
		}
		args[name] = value
	}
	return json.Marshal(args)
}

// coerce turns query-string text into the JSON type the schema asks for;
// what cannot be converted goes through as text, for the schema to refuse.
func coerce(s *jsonschema.Schema, values []string) any {
	if len(values) == 0 {
		return ""
	}
	if s == nil {
		return values[0]
	}
	types := s.Types
	if s.Type != "" {
		types = []string{s.Type}
	}
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
		var n json.Number
		if !slices.Contains(types, "string") && json.Unmarshal([]byte(v), &n) == nil && n.String() == v {
			return n
		}
	}
	return v
}

// serveBlob serves a file document_get pointed at, as a download.
func (c *Core) serveBlob(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	doc := c.blobs[r.PathValue("token")]
	c.mu.Unlock()
	if doc == nil {
		writeError(w, forbid("the download URL is not valid, or has expired"))
		return
	}
	h := w.Header()
	if doc.contentType != nil {
		h.Set("Content-Type", *doc.contentType)
	}
	h.Set("Content-Length", strconv.Itoa(len(doc.file)))
	h.Set("Content-Disposition", "attachment")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, no-store")
	_, _ = w.Write(doc.file)
}
