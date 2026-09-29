package api

import (
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
)

// Error codes (the API contract, §2.2): Core's, and version_mismatch,
// version_required and unavailable. Each is one HTTP status.
const (
	CodeInvalidArgument    = "invalid_argument"
	CodeUnauthenticated    = "unauthenticated"
	CodeForbidden          = "forbidden"
	CodeNotFound           = "not_found"
	CodeMethodNotAllowed   = "method_not_allowed"
	CodeConflict           = "conflict"
	CodeVersionMismatch    = "version_mismatch"
	CodeFailedPrecondition = "failed_precondition"
	CodeVersionRequired    = "version_required"
	CodeRateLimited        = "rate_limited"
	CodeInternal           = "internal"
	CodeUnavailable        = "unavailable"
)

// codeStatus is each code's HTTP status.
var codeStatus = map[string]int{
	CodeInvalidArgument:    http.StatusBadRequest,
	CodeUnauthenticated:    http.StatusUnauthorized,
	CodeForbidden:          http.StatusForbidden,
	CodeNotFound:           http.StatusNotFound,
	CodeMethodNotAllowed:   http.StatusMethodNotAllowed,
	CodeConflict:           http.StatusConflict,
	CodeVersionMismatch:    http.StatusPreconditionFailed,
	CodeFailedPrecondition: http.StatusUnprocessableEntity,
	CodeVersionRequired:    http.StatusPreconditionRequired,
	CodeRateLimited:        http.StatusTooManyRequests,
	CodeInternal:           http.StatusInternalServerError,
	CodeUnavailable:        http.StatusServiceUnavailable,
}

// Reasons any request may be refused for (the API contract's closed list,
// §2.3). The sign-in reasons are webauth's, and keys_unavailable.
const (
	ReasonNoRoute          = "no_route"
	ReasonMethodNotAllowed = "method_not_allowed"
	ReasonCrossOrigin      = "cross_origin"
	ReasonNotJSON          = "not_json"
	ReasonBodyTooLarge     = "body_too_large"
	ReasonMalformedJSON    = "malformed_json"
	ReasonUnknownField     = "unknown_field"
	ReasonMissingField     = "missing_field"
	ReasonInvalidField     = "invalid_field"
	ReasonUnknownParameter = "unknown_parameter"
	ReasonBadIfMatch       = "bad_if_match"
	ReasonRateLimited      = "rate_limited"
	ReasonStoreUnavailable = "store_unavailable"
	ReasonInternal         = "internal"
	ReasonKeysUnavailable  = "keys_unavailable"
)

// Reasons of a hosted agent's routes (§2.3).
const (
	ReasonAgentNotFound   = "agent_not_found"
	ReasonVersionRequired = "version_required"
	ReasonVersionMismatch = "version_mismatch"
	// The tokens'; the others are probe's (token_refused, …).
	ReasonTokenMalformed  = "token_malformed"
	ReasonAlreadyHosted   = "already_hosted"
	ReasonOperatorAgent   = "operator_agent"
	ReasonCoreUnavailable = "core_unavailable"
	// The models' and keys'.
	ReasonUnknownProvider        = "unknown_provider"
	ReasonAdapterNotOffered      = "adapter_not_offered"
	ReasonUnknownEndpoint        = "unknown_endpoint"
	ReasonKeyMalformed           = "key_malformed"
	ReasonSchoolKeyNotOffered    = "school_key_not_offered"
	ReasonUnknownOffer           = "unknown_offer"
	ReasonOwnKeyRequired         = "own_key_required"
	ReasonOwnKeyProviderMismatch = "own_key_provider_mismatch"
	ReasonModelDenied            = "model_denied"
	ReasonSettingsRejected       = "settings_rejected"
)

// maxMessage bounds a message, in bytes, as Core's apperr.Clip does.
const maxMessage = 400

// Error is a refusal: its code, its reason, and words for developers and
// logs, which never hold a token, a key, an assertion, a name, an email or
// a provider's text.
type Error struct {
	Code    string
	Reason  string
	Message string
	// Details are the members of details beside reason (§2.1): field,
	// agent_id, current_version, retry_after_seconds, problems.
	Details map[string]any
}

// errorBody is Core's envelope, with details.reason always there.
type errorBody struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// WriteError answers e: its code's status, Core's envelope, and the
// headers its code calls for (a 401's WWW-Authenticate is the caller's to
// set). The answer's reason is kept for the request's log line.
func WriteError(w http.ResponseWriter, e Error) {
	var body errorBody
	body.Error.Code, body.Error.Message = e.Code, e.Message
	if len(body.Error.Message) > maxMessage {
		body.Error.Message = body.Error.Message[:maxMessage]
	}
	body.Error.Details = map[string]any{"reason": e.Reason}
	for k, v := range e.Details {
		body.Error.Details[k] = v
	}
	status, ok := codeStatus[e.Code]
	if !ok {
		status = http.StatusInternalServerError
	}
	switch status {
	case http.StatusServiceUnavailable:
		w.Header().Set("Retry-After", "5")
	case http.StatusTooManyRequests:
		if s, ok := e.Details["retry_after_seconds"].(int); ok {
			w.Header().Set("Retry-After", strconv.Itoa(s))
		}
	}
	if rec, ok := w.(*recorder); ok {
		rec.reason = e.Reason
	}
	writeJSON(w, status, body)
}

// writeJSON answers v as JSON, with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// isJSON reports whether a Content-Type is JSON's, with or without a
// charset.
func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/json"
}
