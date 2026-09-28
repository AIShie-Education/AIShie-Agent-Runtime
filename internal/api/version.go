package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

// A hosted agent's version is its ETag, the strong tag "<version>" (the
// API contract, §1), and If-Match names the version a write is made over.

// etag is a version's strong entity tag.
func etag(version int) string { return `"` + strconv.Itoa(version) + `"` }

// ifMatch reads If-Match: the version it names, and whether it names one.
// None, or *, names none; anything but one strong quoted integer (W/"7",
// "7", "a", two tags) is refused, bad_if_match.
func ifMatch(r *http.Request) (version int, named bool, bad *Error) {
	vs := r.Header.Values("If-Match")
	if len(vs) == 0 {
		return 0, false, nil
	}
	v := strings.TrimSpace(strings.Join(vs, ","))
	if v == "*" {
		return 0, false, nil
	}
	refuse := &Error{Code: CodeInvalidArgument, Reason: ReasonBadIfMatch,
		Message: `If-Match must be one strong entity tag of the agent's version, such as "7"`}
	inner, ok := strings.CutPrefix(v, `"`)
	if !ok {
		return 0, false, refuse
	}
	inner, ok = strings.CutSuffix(inner, `"`)
	if !ok || inner == "" || len(inner) > 9 || strings.TrimLeft(inner, "0123456789") != "" {
		return 0, false, refuse
	}
	n, err := strconv.Atoi(inner)
	if err != nil {
		return 0, false, refuse
	}
	return n, true, nil
}

// versionMismatch is a write named at a version the agent is no longer at.
func versionMismatch(current int) Error {
	return Error{Code: CodeVersionMismatch, Reason: ReasonVersionMismatch,
		Message: "the agent has changed since that version was read: read it again", Details: map[string]any{"current_version": current}}
}

// errVersionRequired is a write that must name the version it is made
// over, and did not.
var errVersionRequired = Error{Code: CodeVersionRequired, Reason: ReasonVersionRequired,
	Message: `this write needs If-Match: the agent's version as an entity tag, such as "7"`}

// heldTo is the version a write is held to in the store: the one If-Match
// names, or 0, any, when it names none.
func heldTo(version int, named bool) int {
	if named {
		return version
	}
	return 0
}

// writeMismatch answers 412 version_mismatch for a write of the agent id
// that the store refused at the version it was held to, naming the
// version the agent is at now; last, the version read before, when it
// cannot be read again.
func (s *Server) writeMismatch(ctx context.Context, w http.ResponseWriter, id string, last int) {
	current := last
	if again, err := s.o.Store.HostedAgent(ctx, id); err == nil {
		current = again.Version
	}
	WriteError(w, versionMismatch(current))
}
