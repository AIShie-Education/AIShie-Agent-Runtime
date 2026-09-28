package probe

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/providers"
)

// TrialTimeout bounds TryModel's one call.
const TrialTimeout = 25 * time.Second

// What TryModel found (the API contract's keys/test result).
const (
	// ResultOK: the provider answered the call.
	ResultOK = "ok"
	// ResultKeyRefused: the provider refused the key (401, 403).
	ResultKeyRefused = "key_refused"
	// ResultModelNotFound: the key was taken, and the provider has no
	// such model.
	ResultModelNotFound = "model_not_found"
	// ResultKeyAccepted: the key was taken, and the call failed for
	// another reason (rate limited, overloaded, a request it refused).
	ResultKeyAccepted = "key_accepted"
	// ResultUnreachable: the provider was not reached: the network, a
	// timeout, TLS, an address the runtime does not call (netguard), or a
	// redirect.
	ResultUnreachable = "unreachable"
)

// Trial is what TryModel found. It holds nothing the provider wrote but a
// code of a safe shape: some providers echo a masked key in their
// messages.
type Trial struct {
	Result string
	// HTTPStatus is the provider's HTTP status, 0 when none came.
	HTTPStatus int
	// ProviderCode is the provider's error code or type, when it has the
	// shape of one (letters, digits, _ . : -, at most 64); "" otherwise.
	ProviderCode string
	// Kind is the llm error's kind, "" for none.
	Kind llm.ErrorKind
	// Latency is how long the call took.
	Latency time.Duration
	// Err is an error of the call's that is not the provider's answer, for
	// check --live's own report: never shown by the API.
	Err error
}

// providerCodeRe is the shape of a provider code the API passes on.
var providerCodeRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// modelNotFoundCodes are what providers call a model they do not have.
var modelNotFoundCodes = []string{"model_not_found", "model_not_exist", "NotFound"}

// TryModel tries key with m, in one call of one output token, within
// TrialTimeout: the key works if the provider answers anything but a
// refusal of it. client carries the call (for a hosted model, the netguard
// client, which follows no redirect); newAdapter builds the adapter
// (providers.New when nil). The error is an adapter that could not be
// built; whatever the call came to is in the Trial.
func TryModel(ctx context.Context, m config.Model, key string, client *http.Client, newAdapter func(llm.Config) (llm.Adapter, error)) (Trial, error) {
	if newAdapter == nil {
		newAdapter = providers.New
	}
	ad, err := newAdapter(providers.Config(m, key, client))
	if err != nil {
		return Trial{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, TrialTimeout)
	defer cancel()
	start := time.Now()
	_, err = ad.Call(ctx, &llm.Request{Messages: []llm.Message{llm.UserText("Reply with the word OK.")}, ToolMode: llm.ToolAuto,
		Limits: llm.Limits{MaxOutputTokens: 1}})
	t := Trial{Latency: time.Since(start), Result: ResultOK}
	if err == nil {
		return t, nil
	}
	var le *llm.Error
	if !errors.As(err, &le) {
		t.Result, t.Err = ResultUnreachable, err
		return t, nil
	}
	t.Kind, t.HTTPStatus = le.Kind, le.Status
	if providerCodeRe.MatchString(le.Code) {
		t.ProviderCode = le.Code
	}
	switch {
	case le.Kind == llm.ErrAuth:
		t.Result = ResultKeyRefused
	case le.Kind == llm.ErrNetwork || le.Kind == llm.ErrTimeout:
		t.Result = ResultUnreachable
	case le.Kind == llm.ErrBadRequest && (le.Status == http.StatusNotFound || notFoundCode(le.Code)):
		t.Result = ResultModelNotFound
	default:
		t.Result = ResultKeyAccepted
	}
	return t, nil
}

// notFoundCode reports whether a provider's code says it has no such
// model.
func notFoundCode(code string) bool {
	for _, c := range modelNotFoundCodes {
		if strings.Contains(code, c) {
			return true
		}
	}
	return false
}
