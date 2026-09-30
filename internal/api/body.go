package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/jsonstrict"
)

// noQuery refuses a query parameter, answering the refusal
// (unknown_parameter, naming the first): no route takes one but DELETE,
// which reads its own (the API contract, §1). It reports whether the
// request may go on.
func noQuery(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	if len(q) == 0 && !strings.Contains(r.URL.RawQuery, ";") {
		return true
	}
	field := ""
	if names := slices.Sorted(maps.Keys(q)); len(names) > 0 {
		field = names[0]
	}
	WriteError(w, Error{Code: CodeInvalidArgument, Reason: ReasonUnknownParameter, Message: "this route takes no query parameter",
		Details: map[string]any{"field": field}})
	return false
}

// queryOf reads the query of a GET that takes one: the parameters named
// alone, each given once (unknown_parameter, invalid_field, naming it),
// and no body but an empty one or {}. It answers a refusal, and reports
// whether the request may go on.
func queryOf(w http.ResponseWriter, r *http.Request, names ...string) (map[string]string, bool) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || strings.Contains(r.URL.RawQuery, ";") {
		WriteError(w, Error{Code: CodeInvalidArgument, Reason: ReasonUnknownParameter, Message: "the query cannot be read",
			Details: map[string]any{"field": ""}})
		return nil, false
	}
	out := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(q)) {
		switch {
		case !slices.Contains(names, k):
			WriteError(w, Error{Code: CodeInvalidArgument, Reason: ReasonUnknownParameter,
				Message: "this route takes " + strings.Join(names, ", ") + " alone", Details: map[string]any{"field": k}})
			return nil, false
		case len(q[k]) != 1:
			WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, k, "a parameter is given once"))
			return nil, false
		}
		out[k] = q[k][0]
	}
	var none struct{}
	return out, decodeBody(w, r, &none, true)
}

// noBody refuses a query parameter and a body that is not empty or {} (the
// API contract, §1), answering the refusal; it reports whether the request
// may go on.
func noBody(w http.ResponseWriter, r *http.Request) bool {
	var none struct{}
	return noQuery(w, r) && decodeBody(w, r, &none, true)
}

// readBody reads the body of a route that takes one, and no query, into v,
// as decodeBody does, answering a refusal; it reports whether the request
// may go on.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	return noQuery(w, r) && decodeBody(w, r, v, false)
}

// decodeBody reads r's body into v, as the API contract's §3.4 reads one:
// at most MaxBodyBytes (body_too_large), JSON by its Content-Type
// (not_json), no key given twice, in one case or in two (malformed_json,
// jsonstrict and exactNames), no member v does not have by exactly its
// name (unknown_field, with its JSON Pointer), and nothing after the
// object (malformed_json). An empty body is v as it is when empty is
// allowed, and missing_field otherwise. It answers a refusal, and reports
// whether the request may go on. The query is the route's to refuse
// (readBody, noBody).
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
	switch twice, unknown := exactNames(raw, v); {
	case twice:
		return refuse(ReasonMalformedJSON, "the body names a key twice in one object, in two cases", nil)
	case unknown != "":
		return refuse(ReasonUnknownField, "the body has a member this route does not take", map[string]any{"field": "/" + pointerEscape(unknown)})
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

// exactNames holds the members of the object raw to the fields of the
// struct v points to by exactly their names, as encoding/json does not:
// it reads a member into a field whose name it matches in any case, and
// of two such the last, so that {"token": a, "TOKEN": b} would be read as
// b. It reports whether two members' names differ in case alone (twice:
// a member named twice, as the reader would take them), and else the
// first member that is not exactly a field's name (unknown). An object
// that cannot be read, or a v that is not a struct's pointer, is left to
// the decoding that follows.
func exactNames(raw []byte, v any) (twice bool, unknown string) {
	t := reflect.TypeOf(v)
	if t == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return false, ""
	}
	names, ok := memberNames(raw)
	if !ok {
		return false, ""
	}
	folded := make(map[string]bool, len(names))
	for _, n := range names {
		f := strings.ToLower(strings.ToUpper(n))
		if folded[f] {
			return true, ""
		}
		folded[f] = true
	}
	fields := map[string]bool{}
	fieldNames(t.Elem(), fields)
	for _, n := range names {
		if !fields[n] {
			return false, n
		}
	}
	return false, ""
}

// memberNames are the names of the members of the object raw, in order;
// false when raw is not one object that can be read.
func memberNames(raw []byte) ([]string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	var names []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		name, _ := t.(string)
		names = append(names, name)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
	}
	return names, true
}

// fieldNames adds to into the names of the members encoding/json reads
// into a struct of type t: each exported field's, as its tag names it or
// by its own name, and those of the structs it embeds.
func fieldNames(t reflect.Type, into map[string]bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if ft := f.Type; f.Anonymous && name == "" {
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				fieldNames(ft, into)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		into[name] = true
	}
}
