package toolschema

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	gradeSubmit := catalogueTool(t, "grade_submit").InputSchema
	gradeList := catalogueTool(t, "grade_list").InputSchema
	tests := []struct {
		name   string
		schema json.RawMessage
		args   string
		// want is "" for arguments Core's schema takes, else what the
		// message must say.
		want []string
	}{
		{name: "valid", schema: gradeList, args: `{"course_id":"` + testUUID + `","limit":10,"assignment_id":null}`},
		{name: "a decimal as a string", schema: gradeSubmit, args: `{"course_id":"` + testUUID + `","score":"87.5"}`},
		{name: "a decimal as a number", schema: gradeSubmit, args: `{"course_id":"` + testUUID + `","score":87.5}`},
		{name: "an integral float is an integer", schema: gradeList, args: `{"course_id":"` + testUUID + `","limit":10.0}`},
		{
			name: "a missing property", schema: gradeList, args: `{}`,
			want: []string{"required", "course_id"},
		},
		{
			name: "a wrong type, and where", schema: gradeList, args: `{"course_id":"` + testUUID + `","limit":2.5}`,
			want: []string{"limit: ", `want "integer"`},
		},
		{
			name: "deep in an array of objects", schema: gradeSubmit,
			args: `{"course_id":"` + testUUID + `","score":"1","breakdown":[{"criterion":"c","points":"lots","max":"2"}]}`,
			want: []string{"breakdown[].points: ", "pattern"},
		},
		{
			name: "an unknown property", schema: gradeList, args: `{"course_id":"` + testUUID + `","grade":"A"}`,
			want: []string{"unexpected additional properties", "grade"},
		},
		{
			name: "not a UUID", schema: gradeList, args: `{"course_id":"` + testUUID + `","assignment_id":"HW3"}`,
			want: []string{`assignment_id: "HW3" is not a UUID`},
		},
		{
			name: "not a UUID, in an array", schema: catalogueTool(t, "grade_post").InputSchema,
			args: `{"course_id":"` + testUUID + `","grade_ids":["` + testUUID + `","nope"]}`,
			want: []string{`grade_ids[1]: "nope" is not a UUID`},
		},
		{
			name: "a long value is quoted short", schema: gradeList,
			args: `{"course_id":"` + strings.Repeat("é", 100) + `"}`,
			want: []string{`course_id: "` + strings.Repeat("é", 40) + `…" is not a UUID`},
		},
		{name: "a UUID in another form Core decodes", schema: gradeList, args: `{"course_id":"{` + testUUID + `}"}`},
		{
			name: "not an object", schema: gradeList, args: `["` + testUUID + `"]`,
			want: []string{"not a JSON object"},
		},
		{
			name: "not JSON", schema: gradeList, args: `{"course_id":`,
			want: []string{"not valid JSON"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.schema, json.RawMessage(tc.args))
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var ae *ArgumentError
			if !errors.As(err, &ae) {
				t.Fatalf("err %v, want an *ArgumentError", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(ae.Msg, w) {
					t.Errorf("message %q does not say %q", ae.Msg, w)
				}
			}
			if strings.Contains(ae.Msg, "validating ") || strings.Contains(ae.Msg, "/properties/") {
				t.Errorf("message %q is not cleaned up", ae.Msg)
			}
		})
	}
}

func TestValidateClipsLongMessages(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"s":{"type":"string","maxLength":1}}}`)
	err := Validate(schema, json.RawMessage(`{"s":"`+strings.Repeat("é", 2000)+`"}`))
	var ae *ArgumentError
	if !errors.As(err, &ae) {
		t.Fatalf("err %v", err)
	}
	if len(ae.Msg) > maxMessage || !strings.HasSuffix(ae.Msg, "…") {
		t.Errorf("message of %d bytes, not clipped: %q", len(ae.Msg), ae.Msg[:80])
	}
}

func TestValidateBrokenSchema(t *testing.T) {
	for _, schema := range []string{`{"type":`, `{"type":"object","properties":{"a":{"$ref":"#/nowhere"}}}`, `{"pattern":"("}`} {
		err := Validate(json.RawMessage(schema), json.RawMessage(`{}`))
		if err == nil {
			t.Errorf("%s: taken", schema)
			continue
		}
		if errors.As(err, new(*ArgumentError)) {
			t.Errorf("%s: blamed on the model: %v", schema, err)
		}
	}
}

func TestInstancePath(t *testing.T) {
	tests := map[string]string{
		"":                  "",
		"root":              "",
		"/properties/limit": "limit",
		"/properties/breakdown/items/properties/points":     "breakdown[].points",
		"/properties/perms/additionalProperties":            "perms.*",
		"/properties/v/anyOf/1/properties/a":                "v.a",
		"/properties/a~1b/prefixItems/2":                    "a/b[2]",
		"/properties/x/items/items/properties/y/properties": "x[][].y",
	}
	for in, want := range tests {
		if got := instancePath(in); got != want {
			t.Errorf("instancePath(%q) = %q, want %q", in, got, want)
		}
	}
}
