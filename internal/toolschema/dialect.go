// Package toolschema turns Core's tool input schemas into what each model API
// takes, and the model's arguments back into what Core takes (Core's
// docs/agent-runtime.md §3.8).
package toolschema

// Dialect is the JSON Schema a model API takes for tool parameters.
type Dialect string

const (
	// OpenAI is OpenAI Chat and Responses, non-strict: the common transform
	// and nothing more. DeepSeek, non-strict, takes the same.
	OpenAI Dialect = "openai"
	// OpenAIStrict is OpenAI's strict mode: every property required, the
	// optional ones nullable, no allOf/not/if/then/else, additionalProperties
	// false everywhere. A per-model choice, once its contract tests pass.
	OpenAIStrict Dialect = "openai_strict"
	// Anthropic, non-strict: the common transform.
	Anthropic Dialect = "anthropic"
	// GeminiJSONSchema is generateContent's parametersJsonSchema, and the
	// Interactions API's parameters.
	GeminiJSONSchema Dialect = "gemini_json_schema"
	// GeminiOpenAPI is generateContent's parameters, an OpenAPI 3.0 subset:
	// unions become nullable, no additionalProperties.
	GeminiOpenAPI Dialect = "gemini_openapi"
	// Bedrock is Converse's inputSchema.json. Unverified per model family,
	// so it takes the full common transform.
	Bedrock Dialect = "bedrock"
	// FullCommon is the full common transform, with format moved into the
	// description: Qwen, GLM, OpenRouter, vLLM, Ollama, LM Studio, and any
	// OpenAI-compatible server the runtime does not know, whose chat
	// templates break easily on unions.
	FullCommon Dialect = "full_common"
	// Kimi is the full common transform and the strict one: Moonshot's
	// strict defaults to true.
	Kimi Dialect = "kimi"
)

// Valid reports whether d is one of the dialects above.
func (d Dialect) Valid() bool {
	switch d {
	case OpenAI, OpenAIStrict, Anthropic, GeminiJSONSchema, GeminiOpenAPI, Bedrock, FullCommon, Kimi:
		return true
	}
	return false
}
