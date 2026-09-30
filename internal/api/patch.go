package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// PATCH /agents/{id}: the owner's model and own key (the API contract,
// §5.9), the offer of the school's plan it is on (D8), and whether its
// model may act for them (tools.writes, design §4), by merge-patch: a
// member left out is kept as it is, and null clears it.
// It is written only over the version If-Match names, and only once the
// new row passes what the registry holds a row to.

// patchRequest is PATCH's body; its members are read in turn, so that a
// member left out is told from one given as null.
type patchRequest struct {
	Model  json.RawMessage `json:"model"`
	OwnKey json.RawMessage `json:"own_key"`
	Tools  json.RawMessage `json:"tools"`
}

// toolsPatch is PATCH's tools: writes true or false, or null for the
// default, which is true (registry.WritesOf).
type toolsPatch struct {
	Writes json.RawMessage `json:"writes"`
}

type modelPatch struct {
	Own    json.RawMessage `json:"own"`
	School json.RawMessage `json:"school"`
}

type ownKeyPatch struct {
	Value *string `json:"value"`
}

// schoolPatch is PATCH's model.school: the offer of the school's plan, by
// the id GET /models lists it with.
type schoolPatch struct {
	Offer *string `json:"offer"`
}

// maxProblems and maxProblem bound settings_rejected's problems.
const (
	maxProblems = 20
	maxProblem  = 300
)

// isNull reports whether a member was given as null.
func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// decodeMember reads a member's object into v, as the body is read: no
// key given twice in two cases (malformed_json), no member v does not have
// by exactly its name (unknown_field, at the pointer at plus its name),
// nothing but one object (invalid_field at at).
func decodeMember(raw json.RawMessage, v any, at string) *Error {
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		return fieldError(CodeInvalidArgument, ReasonInvalidField, at, "must be an object")
	}
	switch twice, unknown := exactNames(raw, v); {
	case twice:
		return &Error{Code: CodeInvalidArgument, Reason: ReasonMalformedJSON, Message: "the body names a key twice in one object, in two cases"}
	case unknown != "":
		return fieldError(CodeInvalidArgument, ReasonUnknownField, at+"/"+pointerEscape(unknown), "a member this route does not take")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if name, ok := unknownField(err); ok {
			return fieldError(CodeInvalidArgument, ReasonUnknownField, at+"/"+pointerEscape(name), "a member this route does not take")
		}
		return fieldError(CodeInvalidArgument, ReasonInvalidField, at, "a member is not of its type")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fieldError(CodeInvalidArgument, ReasonInvalidField, at, "must be one object")
	}
	return nil
}

// patchOf is what a PATCH asks: whether it gives the own model (own, nil
// for null) and the key (key, nil for null), each in the order of §5.9's
// steps 1 to 4; a refusal is the first of them.
type patchOf struct {
	setOwn bool
	own    *modelSection
	setKey bool
	key    *string
	// setWrites: the patch gives tools.writes, true or false, or nil for
	// the default.
	setWrites bool
	writes    *bool
	// setSchool: the patch gives model.school, the offer of the school's
	// plan by its id, or "" for null.
	setSchool bool
	offer     string
}

// readPatch reads a PATCH's members, with the school's plan in force sc.
func (s *Server) readPatch(req patchRequest, sc config.School) (patchOf, *Error) {
	var p patchOf
	var mp modelPatch
	var choice *OwnModelChoice
	if req.Model != nil {
		if e := decodeMember(req.Model, &mp, "/model"); e != nil {
			return p, e
		}
		if mp.Own != nil {
			p.setOwn = true
			if !isNull(mp.Own) {
				choice = &OwnModelChoice{}
				if e := decodeMember(mp.Own, choice, "/model/own"); e != nil {
					return p, e
				}
				if e := choice.shape("/model/own"); e != nil {
					return p, e
				}
			}
		}
	}
	if req.OwnKey != nil {
		p.setKey = true
		if !isNull(req.OwnKey) {
			var kp ownKeyPatch
			if e := decodeMember(req.OwnKey, &kp, "/own_key"); e != nil {
				return p, e
			}
			if kp.Value == nil {
				return p, fieldError(CodeInvalidArgument, ReasonMissingField, "/own_key/value", "own_key needs its value")
			}
			if e := keyMalformed(*kp.Value, "/own_key/value"); e != nil {
				return p, e
			}
			p.key = kp.Value
		}
	}
	if req.Tools != nil {
		p.setWrites = true
		if !isNull(req.Tools) {
			var tp toolsPatch
			if e := decodeMember(req.Tools, &tp, "/tools"); e != nil {
				return p, e
			}
			switch w := string(bytes.TrimSpace(tp.Writes)); w {
			case "", "null":
				p.setWrites = tp.Writes != nil
			case "true", "false":
				on := w == "true"
				p.writes = &on
			default:
				return p, fieldError(CodeInvalidArgument, ReasonInvalidField, "/tools/writes", "must be true, false or null")
			}
		}
	}
	if mp.School != nil {
		p.setSchool = true
		if !isNull(mp.School) {
			var sp schoolPatch
			if e := decodeMember(mp.School, &sp, "/model/school"); e != nil {
				return p, e
			}
			if sp.Offer == nil || *sp.Offer == "" {
				return p, fieldError(CodeInvalidArgument, ReasonMissingField, "/model/school/offer", "the school's plan needs the offer's id")
			}
			if !sc.Offered() {
				return p, &Error{Code: CodeFailedPrecondition, Reason: ReasonSchoolKeyNotOffered, Message: "the school offers no model on its plan",
					Details: map[string]any{"field": "/model/school"}}
			}
			if _, ok := sc.OfferOf(*sp.Offer); !ok {
				return p, fieldError(CodeInvalidArgument, ReasonUnknownOffer, "/model/school/offer", "not an offer GET /models lists")
			}
			p.offer = *sp.Offer
		}
	}
	if choice != nil {
		sec, e := choice.section("/model/own")
		if e != nil {
			return p, e
		}
		if e := s.denied(sec, "/model/own"); e != nil {
			return p, e
		}
		p.own = sec
	}
	return p, nil
}

// update is PATCH /agents/{id}.
func (s *Server) update(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	version, named, bad := ifMatch(r)
	switch {
	case bad != nil:
		WriteError(w, *bad)
		return
	case !named:
		WriteError(w, errVersionRequired)
		return
	}
	var req patchRequest
	if !readBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	eff, err := s.effective(ctx)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	p, e := s.readPatch(req, eff.Runtime.School)
	if e != nil {
		WriteError(w, *e)
		return
	}
	row := s.ownRow(ctx, w, r.PathValue("id"), c)
	if row == nil || !checkVersion(w, row, version, true) {
		return
	}
	au.detail["agent_id"] = row.ID

	// The row as it would be: the own model, the school's offer, and the
	// key, merged.
	next := *row
	own, school := modelSlots(row.Settings)
	wasOwn, offer := own, ""
	if school != nil {
		offer = school.Offer
	}
	wasOffer := offer
	if p.setOwn {
		own = p.own
	}
	if p.setSchool {
		offer = p.offer
	}
	var changed []string
	settings, err := putModels(row.Settings, own, offer)
	if err != nil {
		s.o.Log.Error("a hosted agent's settings could not be read", "agent", row.ID, "err", err)
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the agent's settings could not be read"})
		return
	}
	if !sameJSON(settings, row.Settings) {
		next.Settings = settings
		if offer != wasOffer {
			changed = append(changed, "model.school")
		}
		if offer == wasOffer || !sameSection(wasOwn, own) {
			changed = append(changed, "model.own")
		}
	}
	if p.setWrites {
		ws, err := putWrites(next.Settings, p.writes)
		if err != nil {
			s.o.Log.Error("a hosted agent's settings could not be read", "agent", row.ID, "err", err)
			WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the agent's settings could not be read"})
			return
		}
		if !sameJSON(ws, next.Settings) {
			next.Settings = ws
			changed = append(changed, "tools.writes")
			au.detail["writes"] = registry.WritesOf(ws)
		}
	}
	var sealed *store.Secret
	switch {
	case p.setKey && p.key == nil && row.KeySecretID != "":
		next.KeySecretID, next.KeyHint, next.KeyProvider = "", "", ""
		changed = append(changed, "own_key")
	case p.key != nil:
		// Sealed only once the row passes; its id is known now, for the
		// dry run.
		sealed = &store.Secret{ID: vault.NewSecretID(), TenantID: row.TenantID, Kind: store.SecretModelKey, CreatedBy: c.ActorID}
		next.KeySecretID, next.KeyHint = sealed.ID, vault.Hint(store.SecretModelKey, *p.key)
		next.KeyProvider = ""
		if own != nil {
			next.KeyProvider = own.Provider
		}
		changed = append(changed, "own_key")
	}
	if len(changed) == 0 {
		// Nothing to write: the agent as it is, at its version.
		au.skip = true
		s.writeAgent(ctx, w, http.StatusOK, row)
		return
	}
	if offer != "" {
		au.detail["offer"] = offer
	}
	if own != nil {
		au.detail["provider"], au.detail["adapter"], au.detail["model"] = own.Provider, own.Adapter, own.Model
		switch {
		case next.KeySecretID == "":
			WriteError(w, Error{Code: CodeFailedPrecondition, Reason: ReasonOwnKeyRequired,
				Message: "the model is on the owner's own key, and none is stored or given", Details: map[string]any{"field": "/own_key"}})
			return
		case sealed == nil && next.KeyProvider != own.Provider:
			WriteError(w, Error{Code: CodeFailedPrecondition, Reason: ReasonOwnKeyProviderMismatch,
				Message: "the stored key is not for this provider: give a key for it", Details: map[string]any{"field": "/own_key"}})
			return
		}
	}
	if own != nil || offer != "" {
		courses, err := s.o.Store.HostedCourses(ctx, row.ID)
		if err != nil {
			s.storeUnavailable(w, "a hosted agent's courses", err)
			return
		}
		o := registry.Options{CoreBaseURL: s.o.CoreBaseURL, Allowlist: s.o.Allowlist}
		if err := registry.CheckPriced(ctx, eff, next, courses, o, s.pricesOf(eff), s.o.Now()); err != nil {
			WriteError(w, Error{Code: CodeFailedPrecondition, Reason: ReasonSettingsRejected,
				Message: "the runtime cannot run these settings", Details: map[string]any{"problems": problems(err)}})
			return
		}
	}
	var secrets []store.Secret
	if sealed != nil {
		if s.o.Vault == nil {
			WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the runtime cannot keep keys now"})
			return
		}
		sec, err := s.o.Vault.Seal(ctx, *sealed, *p.key)
		if err != nil {
			s.o.Log.Error("a key could not be sealed", "agent", row.ID, "err", err)
			WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the runtime could not keep the key"})
			return
		}
		next.KeyHint = sec.Hint
		secrets = append(secrets, sec)
		au.detail["key_hint"] = sec.Hint
	}
	next.Version = version
	updated, err := s.o.Store.UpdateHostedAgent(ctx, next, secrets...)
	switch {
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, errAgentNotFound)
		return
	case errors.Is(err, store.ErrConflict):
		s.writeMismatch(ctx, w, row.ID, version)
		return
	case err != nil:
		s.storeUnavailable(w, "a hosted agent updated", err)
		return
	}
	au.detail["version"], au.detail["changed"] = updated.Version, changed
	s.writeAgent(ctx, w, http.StatusOK, updated)
}

// sameSection reports whether two model sections say the same.
func sameSection(x, y *modelSection) bool {
	if x == nil || y == nil {
		return x == y
	}
	a, errA := json.Marshal(x)
	b, errB := json.Marshal(y)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

// putWrites is settings with tools.writes set to writes, or taken out for
// nil (the default); the rest of the settings, tools' other members among
// them, are kept as they are, and a tools left empty goes.
func putWrites(settings json.RawMessage, writes *bool) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(settings) > 0 {
		if err := json.Unmarshal(settings, &m); err != nil || m == nil {
			return nil, errors.New("api: the agent's settings are not a JSON object")
		}
	}
	tools := map[string]json.RawMessage{}
	if raw, ok := m["tools"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &tools); err != nil || tools == nil {
			return nil, errors.New("api: the agent's tools settings are not a JSON object")
		}
	}
	if writes == nil {
		delete(tools, "writes")
	} else {
		tools["writes"] = json.RawMessage(strconv.FormatBool(*writes))
	}
	if len(tools) == 0 {
		delete(m, "tools")
	} else {
		raw, err := json.Marshal(tools)
		if err != nil {
			return nil, err
		}
		m["tools"] = raw
	}
	return json.Marshal(m)
}

// problems are why a row does not pass, one by one, redacted and bounded,
// as settings_rejected lists them.
func problems(err error) []string {
	var out []string
	for _, p := range strings.Split(config.Rejection{Err: err}.Detail(), "; ") {
		if p = strings.TrimSpace(redact.String(p)); p == "" {
			continue
		}
		out = append(out, clipRunes(p, maxProblem))
		if len(out) == maxProblems {
			break
		}
	}
	return slices.Compact(out)
}

// sameJSON reports whether two JSON documents say the same.
func sameJSON(x, y json.RawMessage) bool {
	var a, b any
	if json.Unmarshal(x, &a) != nil || json.Unmarshal(y, &b) != nil {
		return false
	}
	ax, _ := json.Marshal(a)
	bx, _ := json.Marshal(b)
	return bytes.Equal(ax, bx)
}
