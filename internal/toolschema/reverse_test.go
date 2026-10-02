package toolschema

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestReverse(t *testing.T) {
	const schema = `{"type":"object","additionalProperties":false,"required":["course_id","who"],"properties":{
		"course_id":{"type":"string","format":"uuid"},
		"who":{"type":["null","string"]},
		"limit":{"type":"integer"},
		"after":{"type":["null","string"]},
		"score":{"type":["number","string"]},
		"o":{"type":["null","object"],"additionalProperties":false,"properties":{"a":{"type":"string"},"b":{"type":["null","string"]}}},
		"items":{"type":["null","array"],"items":{"type":"object","additionalProperties":false,"properties":{"c":{"type":"boolean"},"d":{"type":["null","string"]}}}},
		"perms":{"type":"object","additionalProperties":{"type":"string"}},
		"any":{}}}`
	bound := map[string]any{"course_id": testUUID}
	tests := []struct {
		name string
		args string
		want string
	}{
		{
			name: "course_id is put back over what the model wrote",
			args: `{"course_id":"another-course","who":"me"}`,
			want: `{"course_id":"` + testUUID + `","who":"me"}`,
		},
		{
			name: "nulls Core does not take are dropped, those it takes kept",
			args: `{"who":"me","limit":null,"after":null,"score":null}`,
			want: `{"course_id":"` + testUUID + `","who":"me","after":null}`,
		},
		{
			name: "nulls are dropped at any depth",
			args: `{"who":null,"o":{"a":null,"b":null},"items":[{"c":null,"d":null},{"c":true}]}`,
			want: `{"course_id":"` + testUUID + `","who":null,"o":{"b":null},"items":[{"d":null},{"c":true}]}`,
		},
		{
			name: "a map's null values are dropped",
			args: `{"who":"me","perms":{"grade_read":null,"document_read":"autonomous"}}`,
			want: `{"course_id":"` + testUUID + `","who":"me","perms":{"document_read":"autonomous"}}`,
		},
		{
			name: "an untyped property keeps its null",
			args: `{"who":"me","any":null}`,
			want: `{"course_id":"` + testUUID + `","who":"me","any":null}`,
		},
		{
			name: "a required nullable property left out is given as null",
			args: `{}`,
			want: `{"course_id":"` + testUUID + `","who":null}`,
		},
		{
			name: "no arguments at all",
			args: ``,
			want: `{"course_id":"` + testUUID + `","who":null}`,
		},
		{
			name: "numbers are kept exactly as written",
			args: `{"who":"me","limit":12345678901234567890123,"score":87.50000000000000000001,"o":{"a":"1e400"}}`,
			want: `{"course_id":"` + testUUID + `","who":"me","limit":12345678901234567890123,"score":87.50000000000000000001,"o":{"a":"1e400"}}`,
		},
		{
			name: "a property Core does not know is left for Validate",
			args: `{"who":"me","extra":1,"gone":null}`,
			want: `{"course_id":"` + testUUID + `","who":"me","extra":1}`,
		},
		{
			name: "text is not HTML-escaped",
			args: `{"who":"<b>&amp;</b>"}`,
			want: `{"course_id":"` + testUUID + `","who":"<b>&amp;</b>"}`,
		},
		{
			name: "a null inside an array is left for Validate",
			args: `{"who":"me","items":[null]}`,
			want: `{"course_id":"` + testUUID + `","who":"me","items":[null]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Reverse(json.RawMessage(schema), json.RawMessage(tc.args), bound)
			if err != nil {
				t.Fatal(err)
			}
			if want := mustJSON(t, mustDecode(t, []byte(tc.want))); string(got) != string(want) {
				t.Errorf("got  %s\nwant %s", got, want)
			}
		})
	}
}

func TestReverseBound(t *testing.T) {
	tests := []struct {
		name, schema, args string
		bound              map[string]any
		want               string
	}{
		{
			name:   "a bound value the schema has no place for is left out",
			schema: `{"type":"object","properties":{"actor_id":{"type":"string"}},"additionalProperties":false}`,
			args:   `{"actor_id":"a","course_id":"model-wrote-this"}`,
			bound:  map[string]any{"course_id": testUUID},
			want:   `{"actor_id":"a"}`,
		},
		{
			name:   "a schema open to other properties takes it",
			schema: `{"type":"object"}`,
			args:   `{}`,
			bound:  map[string]any{"course_id": testUUID},
			want:   `{"course_id":"` + testUUID + `"}`,
		},
		{
			name:   "several bound values",
			schema: `{"type":"object","properties":{"course_id":{"type":"string"},"idempotency_key":{"type":"string","minLength":1},"body":{"type":"string"}},"additionalProperties":false}`,
			args:   `{"body":"hi","idempotency_key":"chosen-by-the-model"}`,
			bound:  map[string]any{"course_id": testUUID, "idempotency_key": "tool:m1:abc"},
			want:   `{"body":"hi","course_id":"` + testUUID + `","idempotency_key":"tool:m1:abc"}`,
		},
		{
			name:   "a bound value of nil takes the model's out, though the schema has a place for it",
			schema: `{"type":"object","properties":{"body":{"type":"string"},"revises":{"type":"string","format":"uuid"}},"additionalProperties":false}`,
			args:   `{"body":"hi","revises":"` + testUUID + `"}`,
			bound:  map[string]any{"revises": nil},
			want:   `{"body":"hi"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Reverse(json.RawMessage(tc.schema), json.RawMessage(tc.args), tc.bound)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestReverseAlternatives(t *testing.T) {
	// A null is kept when any alternative takes it there.
	tests := []struct {
		name, schema, want string
	}{
		{
			name: "closed branches",
			schema: `{"type":"object","properties":{"t":{"anyOf":[
				{"type":"object","properties":{"a":{"type":"string"},"n":{"type":["null","string"]}},"additionalProperties":false},
				{"type":"object","properties":{"b":{"type":"string"},"n":{"type":"string"}},"additionalProperties":false}]}}}`,
			// a and b are strings where they may be at all; only n takes null.
			want: `{"t":{"n":null}}`,
		},
		{
			name: "an open branch",
			schema: `{"type":"object","properties":{"t":{"anyOf":[
				{"type":"object","properties":{"a":{"type":"string"}}},
				{"type":"object","properties":{"b":{"type":"string"}},"additionalProperties":false}]}}}`,
			// The first branch says nothing of b or z, so null is valid
			// there under it.
			want: `{"t":{"b":null,"n":null,"z":null}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Reverse(json.RawMessage(tc.schema), json.RawMessage(`{"t":{"a":null,"b":null,"n":null,"z":null}}`), nil)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestReverseRefuses(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`)
	for _, args := range []string{`[1,2]`, `"text"`, `null`, `42`, `{"a":`, `{} {}`} {
		_, err := Reverse(schema, json.RawMessage(args), nil)
		var ae *ArgumentError
		if !errors.As(err, &ae) {
			t.Errorf("%s: err %v, want an *ArgumentError", args, err)
		}
	}
	if _, err := Reverse(json.RawMessage(`{"properties":{"a":{"$ref":"#/nowhere"}}}`), json.RawMessage(`{}`), nil); err == nil {
		t.Error("a broken schema was taken")
	} else if errors.As(err, new(*ArgumentError)) {
		t.Errorf("a broken schema is blamed on the model: %v", err)
	}
}
