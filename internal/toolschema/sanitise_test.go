package toolschema

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestSanitise(t *testing.T) {
	tests := []struct {
		name    string
		dialect Dialect
		bound   []string
		in      string
		want    string
	}{
		{
			name: "null union becomes optional", dialect: OpenAI,
			in:   `{"type":"object","properties":{"a":{"type":["null","string"],"description":"the a"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string","description":"the a (optional; omit or null)"}}}`,
		},
		{
			name: "a required nullable property is offered as optional", dialect: OpenAI,
			in:   `{"type":"object","properties":{"a":{"type":["null","string"]},"b":{"type":"string"}},"required":["a","b"]}`,
			want: `{"type":"object","properties":{"a":{"type":"string","description":"(optional; omit or null)"},"b":{"type":"string"}},"required":["b"]}`,
		},
		{
			name: "decimal becomes a string, keeping the pattern", dialect: Anthropic,
			in:   `{"type":"object","properties":{"x":{"type":["number","string"],"pattern":"^[0-9.]+$","description":"a decimal"}},"required":["x"]}`,
			want: `{"type":"object","properties":{"x":{"type":"string","pattern":"^[0-9.]+$","description":"a decimal (a decimal number as a string, e.g. \"87.5\")"}},"required":["x"]}`,
		},
		{
			name: "nullable decimal", dialect: GeminiJSONSchema,
			in:   `{"type":"object","properties":{"x":{"type":["null","number","string"],"pattern":"^p$"}}}`,
			want: `{"type":"object","properties":{"x":{"type":"string","pattern":"^p$","description":"(a decimal number as a string, e.g. \"87.5\") (optional; omit or null)"}}}`,
		},
		{
			name: "uuid kept where format is taken", dialect: OpenAI,
			in:   `{"type":"object","properties":{"id":{"type":"string","format":"uuid"}},"required":["id"]}`,
			want: `{"type":"object","properties":{"id":{"type":"string","format":"uuid"}},"required":["id"]}`,
		},
		{
			name: "uuid said in words where format is refused", dialect: FullCommon,
			in:   `{"type":"object","properties":{"id":{"type":["null","string"],"format":"uuid","description":"the thing"}}}`,
			want: `{"type":"object","properties":{"id":{"type":"string","description":"the thing (UUID) (optional; omit or null)"}}}`,
		},
		{
			name: "bedrock refuses format too", dialect: Bedrock,
			in:   `{"type":"object","properties":{"at":{"type":"string","format":"date-time"}}}`,
			want: `{"type":"object","properties":{"at":{"type":"string","description":"(a date and time, RFC 3339)"}}}`,
		},
		{
			name: "bound properties are taken out", dialect: OpenAI, bound: []string{"course_id", "idempotency_key"},
			in:   `{"type":"object","properties":{"course_id":{"type":"string"},"idempotency_key":{"type":"string","minLength":1},"x":{"type":"string"}},"required":["course_id","x","idempotency_key"],"additionalProperties":false}`,
			want: `{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`,
		},
		{
			name: "only the root's bound properties are taken out", dialect: OpenAI, bound: []string{"course_id"},
			in:   `{"type":"object","properties":{"course_id":{"type":"string"},"o":{"type":"object","properties":{"course_id":{"type":"string"}},"required":["course_id"]}},"required":["course_id","o"]}`,
			want: `{"type":"object","properties":{"o":{"type":"object","properties":{"course_id":{"type":"string"}},"required":["course_id"]}},"required":["o"]}`,
		},
		{
			name: "$defs are inlined", dialect: OpenAI,
			in:   `{"type":"object","$defs":{"U":{"type":"string","format":"uuid"}},"properties":{"a":{"$ref":"#/$defs/U"},"b":{"type":"array","items":{"$ref":"#/$defs/U"}}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string","format":"uuid"},"b":{"type":"array","items":{"type":"string","format":"uuid"}}}}`,
		},
		{
			name: "draft 7 definitions, and a ref's siblings", dialect: OpenAI,
			in:   `{"type":"object","definitions":{"U":{"type":"string","description":"an id"}},"properties":{"a":{"$ref":"#/definitions/U","description":"of the thing"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string","description":"an id; of the thing"}}}`,
		},
		{
			name: "a reference used twice is inlined twice", dialect: FullCommon,
			in:   `{"type":"object","$defs":{"D":{"type":["number","string"]}},"properties":{"a":{"$ref":"#/$defs/D"},"b":{"$ref":"#/$defs/D"}}}`,
			want: `{"type":"object","properties":{"a":{"type":"string","description":"(a decimal number as a string, e.g. \"87.5\")"},"b":{"type":"string","description":"(a decimal number as a string, e.g. \"87.5\")"}}}`,
		},
		{
			name: "allOf is merged", dialect: OpenAI,
			in:   `{"type":"object","allOf":[{"properties":{"a":{"type":"string"}},"required":["a"]},{"properties":{"b":{"type":"integer","minimum":0}},"required":["b"]}]}`,
			want: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer","minimum":0}},"required":["a","b"]}`,
		},
		{
			name: "allOf takes the stricter bounds, both descriptions, the common type", dialect: OpenAI,
			in:   `{"type":"object","properties":{"n":{"allOf":[{"type":"number","minimum":1,"maximum":10,"description":"a count"},{"type":["integer","null"],"minimum":3,"maximum":20,"description":"whole"}]}}}`,
			want: `{"type":"object","properties":{"n":{"type":"integer","minimum":3,"maximum":10,"description":"a count; whole"}}}`,
		},
		{
			name: "anyOf is kept where unions are", dialect: OpenAI,
			in:   `{"type":"object","properties":{"v":{"anyOf":[{"type":"string","format":"uuid"},{"type":"integer"}]}}}`,
			want: `{"type":"object","properties":{"v":{"anyOf":[{"type":"string","format":"uuid"},{"type":"integer"}]}}}`,
		},
		{
			name: "anyOf of scalars is collapsed where unions are refused", dialect: FullCommon,
			in:   `{"type":"object","properties":{"v":{"anyOf":[{"type":"integer"},{"type":"string","format":"uuid"}],"description":"v"}}}`,
			want: `{"type":"object","properties":{"v":{"type":"string","description":"v (UUID) (also accepts: integer)"}}}`,
		},
		{
			name: "a type list is collapsed where unions are refused", dialect: GeminiOpenAPI,
			in:   `{"type":"object","properties":{"v":{"type":["boolean","integer","null"],"minimum":0}}}`,
			want: `{"type":"object","properties":{"v":{"type":"integer","minimum":0,"nullable":true,"description":"(also accepts: boolean) (optional; omit or null)"}}}`,
		},
		{
			name: "oneOf is kept as oneOf where it is taken", dialect: Anthropic,
			in:   `{"type":"object","properties":{"v":{"oneOf":[{"type":"string"},{"type":"boolean"}]}}}`,
			want: `{"type":"object","properties":{"v":{"oneOf":[{"type":"string"},{"type":"boolean"}]}}}`,
		},
		{
			name: "oneOf becomes anyOf under strict", dialect: OpenAIStrict,
			in:   `{"type":"object","properties":{"v":{"oneOf":[{"type":"string"},{"type":"boolean"}]}},"required":["v"]}`,
			want: `{"type":"object","properties":{"v":{"anyOf":[{"type":"string"},{"type":"boolean"}]}},"required":["v"],"additionalProperties":false}`,
		},
		{
			name: "anyOf of objects is merged into one object", dialect: FullCommon,
			in: `{"type":"object","properties":{"target":{"description":"what to grade","anyOf":[
				{"type":"object","properties":{"submission_id":{"type":"string"}},"required":["submission_id"],"additionalProperties":false},
				{"type":"object","properties":{"component_id":{"type":"string"},"student_member_id":{"type":"string"}},"required":["component_id","student_member_id"],"additionalProperties":false}]}},"required":["target"]}`,
			want: `{"type":"object","properties":{"target":{"type":"object","description":"what to grade (one of several forms)","additionalProperties":false,
				"properties":{"component_id":{"type":"string"},"student_member_id":{"type":"string"},"submission_id":{"type":"string"}}}},"required":["target"]}`,
		},
		{
			name: "a null branch is nullability", dialect: GeminiOpenAPI,
			in:   `{"type":"object","properties":{"v":{"anyOf":[{"type":"null"},{"type":"string","description":"a name"}]}}}`,
			want: `{"type":"object","properties":{"v":{"type":"string","nullable":true,"description":"a name (optional; omit or null)"}}}`,
		},
		{
			name: "enum is kept; a null in it is nullability", dialect: OpenAI,
			in:   `{"type":"object","properties":{"s":{"type":["string","null"],"enum":["all","listed",null]}}}`,
			want: `{"type":"object","properties":{"s":{"type":"string","enum":["all","listed"],"description":"(optional; omit or null)"}}}`,
		},
		{
			name: "strict puts null back in a nullable enum", dialect: OpenAIStrict,
			in:   `{"type":"object","properties":{"s":{"type":"string","enum":["all","listed"]}}}`,
			want: `{"type":"object","properties":{"s":{"type":["string","null"],"enum":["all","listed",null],"description":"(optional; omit or null)"}},"required":["s"],"additionalProperties":false}`,
		},
		{
			name: "OpenAPI says a number enum in words", dialect: GeminiOpenAPI,
			in:   `{"type":"object","properties":{"n":{"type":"integer","enum":[1,2,3]}},"required":["n"]}`,
			want: `{"type":"object","properties":{"n":{"type":"integer","description":"(one of: 1, 2, 3)"}},"required":["n"]}`,
		},
		{
			name: "const becomes an enum where const is not taken", dialect: FullCommon,
			in:   `{"type":"object","properties":{"k":{"type":"string","const":"rubric"}}}`,
			want: `{"type":"object","properties":{"k":{"type":"string","enum":["rubric"]}}}`,
		},
		{
			name: "const is kept where it is taken", dialect: OpenAI,
			in:   `{"type":"object","properties":{"k":{"const":"rubric"}}}`,
			want: `{"type":"object","properties":{"k":{"const":"rubric"}}}`,
		},
		{
			name: "a map is kept where maps are", dialect: OpenAI,
			in:   `{"type":"object","properties":{"perms":{"type":"object","additionalProperties":{"type":"string"},"description":"levels"}},"additionalProperties":false}`,
			want: `{"type":"object","properties":{"perms":{"type":"object","additionalProperties":{"type":"string"},"description":"levels"}},"additionalProperties":false}`,
		},
		{
			name: "an optional map is left out under strict", dialect: OpenAIStrict,
			in:   `{"type":"object","properties":{"perms":{"type":"object","additionalProperties":{"type":"string"}},"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`,
			want: `{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`,
		},
		{
			name: "an optional map is left out under OpenAPI", dialect: GeminiOpenAPI,
			in:   `{"type":"object","properties":{"perms":{"type":"object","additionalProperties":{"type":"string"}},"x":{"type":"string"}},"additionalProperties":false}`,
			want: `{"type":"object","properties":{"x":{"type":"string"}}}`,
		},
		{
			name: "a required map is an empty object under strict", dialect: Kimi,
			in:   `{"type":"object","properties":{"perms":{"type":"object","additionalProperties":{"type":"string"},"description":"levels"}},"required":["perms"]}`,
			want: `{"type":"object","properties":{"perms":{"type":"object","properties":{},"required":[],"additionalProperties":false,"description":"levels (give {} here)"}},"required":["perms"],"additionalProperties":false}`,
		},
		{
			name: "strict: nested objects, every property required, optional ones nullable", dialect: OpenAIStrict,
			in: `{"type":"object","properties":{"o":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["null","integer"]}},"required":["a"],"additionalProperties":false},
				"list":{"type":["null","array"],"items":{"type":"object","properties":{"c":{"type":"boolean"}}}}},"required":["o"],"additionalProperties":false}`,
			want: `{"type":"object","required":["list","o"],"additionalProperties":false,"properties":{
				"list":{"type":["array","null"],"description":"(optional; omit or null)","items":{"type":"object","properties":{"c":{"type":["boolean","null"],"description":"(optional; omit or null)"}},"required":["c"],"additionalProperties":false}},
				"o":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":["integer","null"],"description":"(optional; omit or null)"}},"required":["a","b"],"additionalProperties":false}}}`,
		},
		{
			name: "strict drops keywords it does not take", dialect: OpenAIStrict,
			in:   `{"type":"object","properties":{"s":{"type":"string","minLength":1,"maxLength":9,"title":"S","not":{"const":""}}},"required":["s"]}`,
			want: `{"type":"object","properties":{"s":{"type":"string"}},"required":["s"],"additionalProperties":false}`,
		},
		{
			name: "kimi drops every bound", dialect: Kimi,
			in:   `{"type":"object","properties":{"n":{"type":"integer","minimum":-2147483648,"maximum":2147483647},"l":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":3}},"required":["n","l"]}`,
			want: `{"type":"object","properties":{"n":{"type":"integer"},"l":{"type":"array","items":{"type":"string"}}},"required":["l","n"],"additionalProperties":false}`,
		},
		{
			name: "bounds pass through exactly", dialect: OpenAI,
			in:   `{"type":"object","properties":{"n":{"type":"integer","minimum":-2147483648,"maximum":12345678901234567890123}}}`,
			want: `{"type":"object","properties":{"n":{"type":"integer","minimum":-2147483648,"maximum":12345678901234567890123}}}`,
		},
		{
			name: "an untyped property is a string where a type is needed", dialect: FullCommon,
			in:   `{"type":"object","properties":{"v":{"description":"anything"},"w":true}}`,
			want: `{"type":"object","properties":{"v":{"type":"string","description":"anything (any JSON value, given here as a string)"},"w":{"type":"string","description":"(any JSON value, given here as a string)"}}}`,
		},
		{
			name: "an untyped property is kept where it is taken", dialect: Anthropic,
			in:   `{"type":"object","properties":{"v":{"description":"anything"},"w":true}}`,
			want: `{"type":"object","properties":{"v":{"description":"anything"},"w":{}}}`,
		},
		{
			name: "a false property is not offered", dialect: OpenAI,
			in:   `{"type":"object","properties":{"gone":false,"x":{"type":"string"}},"required":["gone","x"]}`,
			want: `{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`,
		},
		{
			name: "an array without items gets them where a type is needed", dialect: GeminiOpenAPI,
			in:   `{"type":"object","properties":{"l":{"type":"array"}},"required":["l"]}`,
			want: `{"type":"object","properties":{"l":{"type":"array","items":{"type":"string","description":"(any JSON value, given here as a string)"}}},"required":["l"]}`,
		},
		{
			name: "nullable items", dialect: OpenAI,
			in:   `{"type":"object","properties":{"l":{"type":"array","items":{"type":["null","string"]}}}}`,
			want: `{"type":"object","properties":{"l":{"type":"array","items":{"type":"string","description":"(or null)"}}}}`,
		},
		{
			name: "keywords of another type are dropped", dialect: OpenAI,
			in:   `{"type":"object","properties":{"v":{"type":["number","string"],"minimum":0,"pattern":"^p$"}}}`,
			want: `{"type":"object","properties":{"v":{"type":"string","pattern":"^p$","description":"(a decimal number as a string, e.g. \"87.5\")"}}}`,
		},
		{
			name: "alternatives at the root are merged, for every dialect", dialect: OpenAI,
			in:   `{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"object","properties":{"b":{"type":"string"}},"required":["b"]}]}`,
			want: `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}`,
		},
		{
			name: "no arguments at all", dialect: OpenAIStrict,
			in:   `{"type":"object","additionalProperties":false}`,
			want: `{"type":"object","properties":{},"required":[],"additionalProperties":false}`,
		},
		{
			name: "no schema at all", dialect: OpenAI,
			in:   `true`,
			want: `{"type":"object","properties":{}}`,
		},
		{
			name: "text is not HTML-escaped", dialect: OpenAI,
			in:   `{"type":"object","properties":{"q":{"type":"string","description":"a < b & c > d"}}}`,
			want: `{"type":"object","properties":{"q":{"type":"string","description":"a < b & c > d"}}}`,
		},
		{
			name: "allOf naming one property twice merges it", dialect: OpenAI,
			in:   `{"type":"object","allOf":[{"properties":{"a":{"type":"string","description":"the a"}}},{"properties":{"a":{"maxLength":5}},"required":["a"]}]}`,
			want: `{"type":"object","properties":{"a":{"type":"string","maxLength":5,"description":"the a"}},"required":["a"]}`,
		},
		{
			name: "scalar alternatives of one type unite their enums", dialect: FullCommon,
			in:   `{"type":"object","properties":{"s":{"anyOf":[{"type":"string","enum":["a"],"maxLength":3},{"type":"string","enum":["b"],"maxLength":3,"pattern":"^b$"}]}}}`,
			want: `{"type":"object","properties":{"s":{"type":"string","enum":["a","b"],"maxLength":3}}}`,
		},
		{
			name: "array alternatives are one array of either items", dialect: FullCommon,
			in:   `{"type":"object","properties":{"l":{"anyOf":[{"type":"array","items":{"type":"string"}},{"type":"array","items":{"type":"integer"}}]}}}`,
			want: `{"type":"object","properties":{"l":{"type":"array","description":"(one of several forms)","items":{"type":"string","description":"(also accepts: integer)"}}}}`,
		},
		{
			name: "merged objects whose properties differ", dialect: GeminiOpenAPI,
			in: `{"type":"object","properties":{"o":{"oneOf":[{"type":"object","properties":{"v":{"type":"string"}},"required":["v"]},
				{"type":"object","properties":{"v":{"type":"integer"},"w":{"type":"boolean"}},"required":["v","w"]}]}},"required":["o"]}`,
			want: `{"type":"object","required":["o"],"properties":{"o":{"type":"object","description":"(one of several forms)","required":["v"],
				"properties":{"v":{"type":"string","description":"(also accepts: integer)"},"w":{"type":"boolean"}}}}}`,
		},
		{
			name: "an alternative that takes anything makes the whole take anything", dialect: FullCommon,
			in:   `{"type":"object","properties":{"v":{"anyOf":[{"type":"string"},{"description":"anything"}]}}}`,
			want: `{"type":"object","properties":{"v":{"type":"string","description":"(any JSON value, given here as a string)"}}}`,
		},
		{
			name: "types are inferred where one is needed", dialect: Bedrock,
			in: `{"type":"object","properties":{"e":{"enum":[1,2]},"p":{"pattern":"^a"},"n":{"minimum":0},"o":{"properties":{"a":{"type":"string"}}},
				"l":{"items":{"type":"string"}},"m":{"enum":["a",1]},"b":{"const":true}}}`,
			want: `{"type":"object","properties":{"e":{"type":"number","enum":[1,2]},"p":{"type":"string","pattern":"^a"},"n":{"type":"number","minimum":0},
				"o":{"type":"object","properties":{"a":{"type":"string"}}},"l":{"type":"array","items":{"type":"string"}},
				"m":{"type":"string","enum":["a",1],"description":"(any JSON value, given here as a string)"},"b":{"type":"boolean","enum":[true]}}}`,
		},
		{
			name: "strict: nullable items", dialect: OpenAIStrict,
			in:   `{"type":"object","properties":{"l":{"type":"array","items":{"type":["null","string"]}}},"required":["l"]}`,
			want: `{"type":"object","properties":{"l":{"type":"array","items":{"type":["string","null"]}}},"required":["l"],"additionalProperties":false}`,
		},
		{
			name: "OpenAPI: nullable items", dialect: GeminiOpenAPI,
			in:   `{"type":"object","properties":{"l":{"type":"array","items":{"type":["null","string"]}}},"required":["l"]}`,
			want: `{"type":"object","properties":{"l":{"type":"array","items":{"type":"string","nullable":true}}},"required":["l"]}`,
		},
		{
			name: "strict: an optional alternative gets a null branch", dialect: OpenAIStrict,
			in:   `{"type":"object","properties":{"v":{"anyOf":[{"type":"string"},{"type":"integer"}]}}}`,
			want: `{"type":"object","properties":{"v":{"anyOf":[{"type":"string"},{"type":"integer"},{"type":"null"}],"description":"(optional; omit or null)"}},"required":["v"],"additionalProperties":false}`,
		},
		{
			name: "a map's nullable values", dialect: OpenAI,
			in:   `{"type":"object","properties":{"m":{"type":"object","additionalProperties":{"type":["null","string"]}}}}`,
			want: `{"type":"object","properties":{"m":{"type":"object","additionalProperties":{"type":"string","description":"(or null)"}}}}`,
		},
		{
			name: "a map that takes nothing", dialect: OpenAI,
			in:   `{"type":"object","properties":{"m":{"type":"object","additionalProperties":{"not":{}},"properties":{"a":{"type":"string"}}}}}`,
			want: `{"type":"object","properties":{"m":{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"not":{}}}}}`,
		},
		{
			name: "formats in words", dialect: Kimi,
			in: `{"type":"object","properties":{"a":{"type":"string","format":"date"},"b":{"type":"string","format":"email"},
				"c":{"type":"string","format":"uri"},"d":{"type":"string","format":"ipv4"}},"required":["a","b","c","d"]}`,
			want: `{"type":"object","additionalProperties":false,"required":["a","b","c","d"],"properties":{"a":{"type":"string","description":"(a date, YYYY-MM-DD)"},
				"b":{"type":"string","description":"(an email address)"},"c":{"type":"string","description":"(a URI)"},"d":{"type":"string","description":"(format: ipv4)"}}}`,
		},
		{
			name: "unknown keywords pass through where plain JSON Schema is taken", dialect: GeminiJSONSchema,
			in:   `{"type":"object","properties":{"s":{"type":"string","title":"S","examples":["x"]}}}`,
			want: `{"type":"object","properties":{"s":{"type":"string","title":"S","examples":["x"]}}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Sanitise(json.RawMessage(tc.in), tc.dialect, tc.bound)
			if err != nil {
				t.Fatal(err)
			}
			want := mustJSON(t, mustDecode(t, []byte(tc.want)))
			if string(got) != string(want) {
				t.Errorf("got  %s\nwant %s", got, want)
			}
			if strings.Contains(string(got), `\u00`) {
				t.Errorf("HTML-escaped: %s", got)
			}
		})
	}
}

func TestSanitiseRefuses(t *testing.T) {
	tests := []struct {
		name    string
		dialect Dialect
		in      string
		want    string
	}{
		{"an unknown dialect", "yaml", `{"type":"object"}`, "unknown dialect"},
		{"not JSON", OpenAI, `{"type":`, "not JSON"},
		{"two values", OpenAI, `{} {}`, "not JSON"},
		{"not an object", OpenAI, `{"type":"string"}`, "not an object"},
		{"false", OpenAI, `false`, "no arguments at all"},
		{"a recursive schema", OpenAI, `{"type":"object","$defs":{"N":{"type":"object","properties":{"next":{"$ref":"#/$defs/N"}}}},"properties":{"n":{"$ref":"#/$defs/N"}}}`, "recursive"},
		{"a schema that refers to its root", OpenAIStrict, `{"type":"object","properties":{"self":{"$ref":"#"}}}`, "recursive"},
		{"a reference elsewhere", OpenAI, `{"type":"object","properties":{"a":{"$ref":"https://example.com/s.json"}}}`, "not within the schema"},
		{"a reference to nothing", OpenAI, `{"type":"object","properties":{"a":{"$ref":"#/$defs/missing"}}}`, "nothing at"},
		{"allOf with no type in common", OpenAI, `{"type":"object","properties":{"a":{"allOf":[{"type":"string"},{"type":"integer"}]}}}`, "no type in common"},
		{"allOf with no enum value in common", OpenAI, `{"type":"object","properties":{"a":{"allOf":[{"enum":["x"]},{"enum":["y"]}]}}}`, "no enum value in common"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Sanitise(json.RawMessage(tc.in), tc.dialect, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want one saying %q", err, tc.want)
			}
		})
	}
}

// TestSanitiseExponentialRefs checks that references that multiply one
// another are refused rather than expanded without bound.
func TestSanitiseExponentialRefs(t *testing.T) {
	defs := map[string]any{"D0": map[string]any{"type": "string"}}
	for i := 1; i <= 20; i++ {
		prev := "#/$defs/D" + strconv.Itoa(i-1)
		defs["D"+strconv.Itoa(i)] = map[string]any{"type": "object", "properties": map[string]any{
			"a": map[string]any{"$ref": prev}, "b": map[string]any{"$ref": prev},
		}}
	}
	schema := map[string]any{"type": "object", "$defs": defs, "properties": map[string]any{"x": map[string]any{"$ref": "#/$defs/D20"}}}
	_, err := Sanitise(mustJSON(t, schema), OpenAI, nil)
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("err %v, want the size bound", err)
	}
}

// TestSanitiseLeavesInputAlone checks that the schema given is not changed:
// the same catalogue schema is sanitised once per dialect.
func TestSanitiseLeavesInputAlone(t *testing.T) {
	in := json.RawMessage(`{"type":"object","$defs":{"U":{"type":["null","string"],"format":"uuid"}},"properties":{"a":{"$ref":"#/$defs/U"},"course_id":{"type":"string"}},"required":["course_id"]}`)
	before := string(in)
	for _, d := range allDialects {
		if _, err := Sanitise(in, d, testBound); err != nil {
			t.Fatal(err)
		}
	}
	if string(in) != before {
		t.Fatalf("the input changed: %s", in)
	}
}

func TestDialectsAreValid(t *testing.T) {
	for _, d := range Dialects {
		if !d.Valid() {
			t.Errorf("%s is listed but not valid", d)
		}
		if _, err := rulesFor(d); err != nil {
			t.Errorf("%s has no rules: %v", d, err)
		}
	}
	if len(Dialects) != 8 {
		t.Errorf("%d dialects listed, want the 8 of dialect.go", len(Dialects))
	}
}

func TestSanitiseNothingValid(t *testing.T) {
	tests := []struct {
		name    string
		dialect Dialect
		in      string
		want    string
	}{
		{"alternatives that are all false, merged", FullCommon,
			`{"type":"object","properties":{"a":{"anyOf":[false,false]},"b":{"type":"string"}}}`,
			`{"type":"object","properties":{"b":{"type":"string"}}}`},
		{"alternatives that are all false, kept", OpenAI,
			`{"type":"object","properties":{"a":{"anyOf":[false,false]},"b":{"type":"string"}}}`,
			`{"type":"object","properties":{"b":{"type":"string"}}}`},
		{"a tuple of false", FullCommon,
			`{"type":"object","properties":{"l":{"type":"array","items":[false]}}}`,
			`{"type":"object","properties":{"l":{"type":"array","items":{"type":"string"},"maxItems":0}}}`},
		{"an empty tuple takes anything", Anthropic,
			`{"type":"object","properties":{"l":{"type":"array","items":[]}}}`,
			`{"type":"object","properties":{"l":{"type":"array","items":{}}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Sanitise(json.RawMessage(tc.in), tc.dialect, nil)
			if err != nil {
				t.Fatal(err)
			}
			if want := mustJSON(t, mustDecode(t, []byte(tc.want))); string(got) != string(want) {
				t.Errorf("got  %s\nwant %s", got, want)
			}
		})
	}
	if _, err := Sanitise(json.RawMessage(`{"anyOf":[false,{"type":"null"}]}`), OpenAI, nil); err == nil {
		t.Error("a root that takes nothing was taken")
	}
}
