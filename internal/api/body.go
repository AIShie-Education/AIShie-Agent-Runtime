package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/jsonstrict"
)

// noBody refuses a query parameter (no route of R4 takes one), and a body
// that is not empty or {} (the API contract, §1), answering the refusal; it
// reports whether the request may go on.
func noBody(w http.ResponseWriter, r *http.Request) bool {
	if len(r.URL.Query()) > 0 || strings.Contains(r.URL.RawQuery, ";") {
		names := make([]string, 0, len(r.URL.Query()))
		for k := range r.URL.Query() {
			names = append(names, k)
		}
		slices.Sort(names)
		field := ""
		if len(names) > 0 {
			field = names[0]
		}
		WriteError(w, Error{Code: CodeInvalidArgument, Reason: ReasonUnknownParameter, Message: "this route takes no query parameter",
			Details: map[string]any{"field": field}})
		return false
	}
	var none struct{}
	return decodeBody(w, r, &none, true)
}

// decodeBody reads r's body into v, as the API contract's §3.4 reads one:
// at most MaxBodyBytes (body_too_large), JSON by its Content-Type
// (not_json), no key given twice (malformed_json, jsonstrict), no member v
// does not have (unknown_field, with its JSON Pointer), and nothing after
// the object (malformed_json). An empty body is v as it is when empty is
// allowed, and missing_field otherwise. It answers a refusal, and reports
// whether the request may go on.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, empty bool) bool {
	refuse := func(reason, msg string, details map[string]any) bool {
		WriteError(w, Error{Code: CodeInvalidArgument, Reason: reason, Message: msg, Details: details})
		return false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return refuse(ReasonBodyTooLarge, "the request body is larger than 65536 bytes", nil)
	case err != nil:
		return refuse(ReasonMalformedJSON, "the request body could not be read", nil)
	case len(bytes.TrimSpace(raw)) == 0 && empty:
		return true
	case len(bytes.TrimSpace(raw)) == 0:
		return refuse(ReasonMissingField, "the request needs a body: one JSON object", map[string]any{"field": ""})
	case !isJSON(r.Header.Get("Content-Type")):
		return refuse(ReasonNotJSON, "a request's body must be JSON (Content-Type: application/json)", nil)
	}
	if err := jsonstrict.Check(raw); err != nil {
		return refuse(ReasonMalformedJSON, "the body names a key twice in one object, or holds a number out of bounds", nil)
	}
	if t := bytes.TrimSpace(raw); t[0] != '{' {
		return refuse(ReasonMalformedJSON, "the body must be one JSON object", nil)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if name, ok := unknownField(err); ok {
			return refuse(ReasonUnknownField, "the body has a member this route does not take", map[string]any{"field": "/" + pointerEscape(name)})
		}
		return refuse(ReasonMalformedJSON, "the body is not JSON of this route's shape", nil)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return refuse(ReasonMalformedJSON, "something follows the body's object", nil)
	}
	return true
}

// unknownField is the member encoding/json's DisallowUnknownFields
// refused.
func unknownField(err error) (string, bool) {
	const prefix = `json: unknown field "`
	msg := err.Error()
	if !strings.HasPrefix(msg, prefix) || !strings.HasSuffix(msg, `"`) {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(msg, prefix), `"`), true
}

// pointerEscape escapes a member's name for a JSON Pointer (RFC 6901).
func pointerEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
