package fakecore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/jsonstrict"
)

// apiError is Core's apperr.Error as the wire shows it. Classify by Code and
// Details; the message is for people.
type apiError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

// with returns a copy carrying one more detail.
func (e *apiError) with(key string, value any) *apiError {
	out := *e
	out.Details = make(map[string]any, len(e.Details)+1)
	for k, v := range e.Details {
		out.Details[k] = v
	}
	out.Details[key] = value
	return &out
}

// Error codes (§2.1).
const (
	codeInvalidArgument     = "invalid_argument"
	codeUnauthenticated     = "unauthenticated"
	codeForbidden           = "forbidden"
	codeNotFound            = "not_found"
	codeConflict            = "conflict"
	codeIdempotencyConflict = "idempotency_conflict"
	codeFailedPrecondition  = "failed_precondition"
	codeRateLimited         = "rate_limited"
	codeInternal            = "internal"
)

// maxMessage bounds a message in bytes, as Core bounds its own.
const maxMessage = 400

func clip(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	note := fmt.Sprintf("… (%d bytes)", len(s))
	cut := maxMessage - len(note)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + note
}

func newErr(code, format string, args ...any) *apiError {
	return &apiError{Code: code, Message: clip(fmt.Sprintf(format, args...))}
}

func invalid(format string, args ...any) *apiError {
	return newErr(codeInvalidArgument, format, args...)
}
func forbid(format string, args ...any) *apiError    { return newErr(codeForbidden, format, args...) }
func missing(format string, args ...any) *apiError   { return newErr(codeNotFound, format, args...) }
func conflicts(format string, args ...any) *apiError { return newErr(codeConflict, format, args...) }
func precondition(format string, args ...any) *apiError {
	return newErr(codeFailedPrecondition, format, args...)
}

func asAPI(err error) (*apiError, bool) {
	var e *apiError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// denial is the error a denied call carries.
func denial(reason string) *apiError { return forbid("not permitted").with("reason", reason) }

// outcome is what became of a call that got as far as being attempted, or,
// with status error, one that did not: Core's pipeline.Outcome.
type outcome struct {
	Status      string          `json:"status"`
	ActionID    string          `json:"action_id,omitempty"`
	ReviewState string          `json:"review_state,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *apiError       `json:"error,omitempty"`
	Replayed    bool            `json:"replayed,omitempty"`
}

// errorOutcome is a call never attempted.
func errorOutcome(e *apiError) outcome { return outcome{Status: "error", Error: e} }

// Review states.
const (
	reviewNone      = "none"
	reviewPending   = "pending"
	reviewReviewed  = "reviewed"
	reviewEscalated = "escalated"
)

// gate is what a tool's authorization asks for.
type gate struct {
	perms []string
	// any: holding one of perms is enough; the target names the one that
	// governs.
	any bool
	// self: the tool is about the caller alone (me_get): an active actor
	// may call it.
	self bool
	// ownAgents, with perms, is what an agent's owner may do about a
	// target of their own agent's however little perms give them (Core's
	// Gate.OwnAgents): the level it returns, when higher, is theirs.
	ownAgents func(c *Core, caller *actor, seat *member, tgt target) level
	// refusal, when it gives one, is what a caller whose seat perms deny
	// is told instead of permission_denied (Core's Gate.Refusal): its
	// details.reason is the denial's.
	refusal func(caller *actor, seat *member) *apiError
	// ownerJudgedBy, for a write gated by a permission no person holds, is
	// what an agent's owner is measured by instead, deciding a proposal of
	// their agent's (Core's Spec.OwnerJudgedBy; ownerJudges).
	ownerJudgedBy []string
}

// target is what a call acts on, as its tool resolves it.
type target struct {
	typ   string
	id    *string
	scope scope
	// perms is the permission that governs this target, for a tool whose
	// gate is any.
	perms []string
}

// readCtx is what a read's query is given.
type readCtx struct {
	now    time.Time
	actor  *actor
	member *member
	course *course
	base   string
}

// execCtx is what a write's execution is given.
type execCtx struct {
	now       time.Time
	actor     *actor
	member    *member
	course    *course
	actionID  string
	createdAt time.Time
	events    []*event
}

// emit files an event under the executing action unless it names one.
func (ec *execCtx) emit(e *event) {
	if e.actionID == nil {
		id := ec.actionID
		e.actionID = &id
	}
	ec.events = append(ec.events, e)
}

// impl is how the fake carries out one tool, in the shape of Core's
// tool.Spec: a gate, a resolution of the target, then a query (a read) or a
// pin and an execution (a write).
type impl struct {
	gate     gate
	decode   func(raw []byte) (any, error)
	courseOf func(in any) string
	resolve  func(c *Core, co *course, in any) (target, error)
	query    func(c *Core, rc *readCtx, in any) (any, error)
	pin      func(c *Core, m *member, in any) error
	execute  func(c *Core, ec *execCtx, in any) (any, error)
}

// spec is impl, typed by the tool's input.
type spec[In any] struct {
	gate    gate
	resolve func(c *Core, co *course, in In) (target, error)
	query   func(c *Core, rc *readCtx, in In) (any, error)
	pin     func(c *Core, m *member, in In) error
	execute func(c *Core, ec *execCtx, in In) (any, error)
}

// courseScoped is an input that names its course.
type courseScoped interface{ courseID() uuid.UUID }

// inCourse is embedded by every course-scoped tool's input.
type inCourse struct {
	CourseID uuid.UUID `json:"course_id"`
}

func (i inCourse) courseID() uuid.UUID { return i.CourseID }

func define[In any](s spec[In]) *impl {
	im := &impl{gate: s.gate}
	im.decode = func(raw []byte) (any, error) {
		var in In
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, invalid("arguments: %v", err)
		}
		return in, nil
	}
	im.courseOf = func(in any) string {
		if cs, ok := in.(courseScoped); ok {
			if id := cs.courseID(); id != uuid.Nil {
				return id.String()
			}
		}
		return ""
	}
	if s.resolve != nil {
		im.resolve = func(c *Core, co *course, in any) (target, error) { return s.resolve(c, co, in.(In)) }
	}
	if s.query != nil {
		im.query = func(c *Core, rc *readCtx, in any) (any, error) { return s.query(c, rc, in.(In)) }
	}
	if s.pin != nil {
		im.pin = func(c *Core, m *member, in any) error { return s.pin(c, m, in.(In)) }
	}
	if s.execute != nil {
		im.execute = func(c *Core, ec *execCtx, in any) (any, error) { return s.execute(c, ec, in.(In)) }
	}
	return im
}

// decodeArgs judges raw arguments as Core's tool.Decode does: one JSON
// object, no key named twice, matching the tool's schema, no U+0000; then
// into the tool's own input.
func (t *toolDef) decodeArgs(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	if err := jsonstrict.Check(raw); err != nil {
		return nil, invalid("arguments: %v", err)
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return nil, invalid("arguments are not valid JSON: %v", err)
	}
	if err := t.schema.Validate(instance); err != nil {
		return nil, invalid("arguments do not match the schema of %s: %v", t.Name, err)
	}
	if hasNUL(instance) {
		return nil, invalid("arguments cannot contain U+0000")
	}
	if t.impl == nil {
		return instance, nil
	}
	return t.impl.decode(raw)
}

func hasNUL(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.ContainsRune(x, 0)
	case []any:
		for _, e := range x {
			if hasNUL(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if strings.ContainsRune(k, 0) || hasNUL(e) {
				return true
			}
		}
	}
	return false
}

// invoke runs one call as caller, whatever door it came in by. raw is the
// tool's own arguments (the key taken out); key is the idempotency key.
// The lock is held.
func (c *Core) invoke(caller *actor, t *toolDef, raw []byte, key, base string) outcome {
	in, err := t.decodeArgs(raw)
	if err != nil {
		return c.failure(err)
	}
	if t.impl == nil {
		if t.write {
			if err := checkKey(t, key); err != nil {
				return c.failure(err)
			}
		}
		return c.refuseUnimplemented(t, in)
	}
	var out outcome
	if t.write {
		out, err = c.invokeWrite(caller, t, in, raw, key)
	} else {
		out, err = c.invokeRead(caller, t, in, base)
	}
	if err != nil {
		return c.failure(err)
	}
	return out
}

// failure is the envelope of a call never attempted. Anything but an
// apiError is a fault of the fake's own, and says only that.
func (c *Core) failure(err error) outcome {
	if e, ok := asAPI(err); ok {
		return errorOutcome(e)
	}
	return errorOutcome(&apiError{Code: codeInternal, Message: "something went wrong on our side; retry with the same idempotency_key"})
}

// refuseUnimplemented answers a tool the fake does not carry out as a call
// the seat may not make: never attempted, nothing recorded. A course that
// does not exist is not_found, as it is in Core; otherwise forbidden, with
// details.reason saying why, so that nobody mistakes it for Core's answer.
func (c *Core) refuseUnimplemented(t *toolDef, in any) outcome {
	if args, ok := in.(map[string]any); ok {
		if s, ok := args["course_id"].(string); ok {
			id, err := uuid.Parse(s)
			if err != nil {
				return errorOutcome(invalid("arguments: %v", err))
			}
			if c.courses[id.String()] == nil {
				return errorOutcome(missing("course %s does not exist", id))
			}
		}
	}
	return errorOutcome(forbid("%s is not something this fake Core carries out", t.Name).with("reason", "not_implemented"))
}

// authorized is the gate's verdict on one call, and what was learnt on the
// way.
type authorized struct {
	decision decision
	target   target
	course   *course
	// refused is what a denied call is told where its gate knows more
	// than the permission that denied it (gate.refusal); nil for the plain
	// denial of decision.reason.
	refused *apiError
}

// refusal is what a denied call is told.
func (a authorized) refusal() *apiError {
	if a.refused != nil {
		return a.refused
	}
	return denial(a.decision.reason)
}

// explain asks a gate that knows why a seat its perms deny is refused
// (gate.refusal), and records what it says: the refusal, and its reason in
// place of permission_denied (Core's authorized.explain).
func (a *authorized) explain(g gate, caller *actor) {
	if g.refusal == nil || a.decision.reason != reasonPermDenied || a.decision.member == nil {
		return
	}
	if e := g.refusal(caller, a.decision.member); e != nil {
		a.refused = e
		a.decision.reason, _ = e.Details["reason"].(string)
	}
}

// noun is the part of a tool name before the dot: the target type of a call
// denied before its target was looked up.
func noun(name string) string {
	n, _, _ := strings.Cut(name, ".")
	return n
}

// authorize runs a tool's gate as Core's pipeline does: the seat and the
// permission first, the target only for a caller who passed, then the
// target's own permission and scope. asMember, when set, checks that seat
// instead of the caller's: how a proposal is authorized again on approval.
func (c *Core) authorize(t *toolDef, in any, act *actor, asMember *member) (authorized, error) {
	im := t.impl
	res := authorized{target: target{typ: noun(t.Name)}}
	now := c.now()
	if im.gate.self {
		if !act.active() {
			res.decision = deny(reasonActorNotActive, nil)
			return res, nil
		}
		res.decision = decision{level: autonomous}
		tgt, err := im.resolve(c, nil, in)
		if err != nil {
			return res, err
		}
		res.target = tgt
		return res, nil
	}
	cid := im.courseOf(in)
	if cid == "" {
		return res, invalid("course_id is required")
	}
	co := c.courses[cid]
	if co == nil {
		return res, missing("course %s does not exist", cid)
	}
	res.course = co
	check := func(perms []string) decision {
		if asMember != nil {
			return evaluate(asMember.actor, co, asMember, perms, t.write, now)
		}
		return evaluate(act, co, c.seatOf(act, co), perms, t.write, now)
	}
	if im.gate.any {
		for _, p := range im.gate.perms {
			if res.decision = check([]string{p}); res.decision.level.allowed() {
				break
			}
		}
	} else {
		res.decision = check(im.gate.perms)
	}
	// An agent's owner, whose seat counts, goes on to see whether the
	// target is their own agent's, however little the perms give them;
	// anyone else denied stops here.
	owner := im.gate.ownAgents != nil && res.decision.member != nil && res.decision.level < autonomous &&
		(res.decision.level.allowed() || res.decision.reason == reasonPermDenied)
	if !res.decision.level.allowed() && !owner {
		res.explain(im.gate, act)
		return res, nil
	}
	tgt, err := im.resolve(c, co, in)
	if err != nil {
		if _, ok := asAPI(err); ok && !res.decision.level.allowed() {
			// Let as far as the target only as an owner: whether it
			// exists is none of their business.
			return res, nil
		}
		return res, err
	}
	if tgt.typ == "" {
		tgt.typ = res.target.typ
	}
	res.target = tgt
	if owner {
		if l := im.gate.ownAgents(c, act, res.decision.member, tgt); l > res.decision.level {
			res.decision = decision{level: l, member: res.decision.member}
		}
		if !res.decision.level.allowed() {
			return res, nil
		}
	}
	if len(tgt.perms) > 0 {
		if res.decision = check(tgt.perms); !res.decision.level.allowed() {
			return res, nil
		}
	}
	if r := checkScope(res.decision.member, tgt.scope); r != "" {
		res.decision = deny(r, res.decision.member)
	}
	return res, nil
}

// seatOf is the actor's seat in the course that is not removed; nil for
// none.
func (c *Core) seatOf(act *actor, co *course) *member {
	for _, m := range c.memberList {
		if m.actor == act && m.course == co && m.status != statusRemoved {
			return m
		}
	}
	return nil
}

func (c *Core) invokeRead(caller *actor, t *toolDef, in any, base string) (outcome, error) {
	a, err := c.authorize(t, in, caller, nil)
	if err != nil {
		return outcome{}, err
	}
	if !a.decision.level.allowed() {
		return outcome{Status: actDenied, Error: a.refusal()}, nil
	}
	rc := &readCtx{now: c.now(), actor: caller, member: a.decision.member, course: a.course, base: base}
	res, err := t.impl.query(c, rc, in)
	if err != nil {
		return outcome{}, err
	}
	body, err := json.Marshal(res)
	if err != nil {
		return outcome{}, fmt.Errorf("%s: result: %w", t.Name, err)
	}
	return outcome{Status: actExecuted, Result: body}, nil
}

// initialStatus is the status an action row is first written with.
func initialStatus(l level) string {
	switch l {
	case denied:
		return actDenied
	case confirmRequired:
		return actProposed
	}
	return actApproved
}

func errorResult(e *apiError) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"error": e})
	return b
}

func storedError(result json.RawMessage) *apiError {
	var v struct {
		Error *apiError `json:"error"`
	}
	if json.Unmarshal(result, &v) != nil {
		return nil
	}
	return v.Error
}

// invokeWrite is Core's pipeline for a write: the key checked, a replay
// answered from the stored outcome, then authorization, the action row
// written before anything happens (denials included), a proposal pinned,
// or the tool executed.
func (c *Core) invokeWrite(caller *actor, t *toolDef, in any, raw []byte, key string) (outcome, error) {
	if err := checkKey(t, key); err != nil {
		return outcome{}, err
	}
	canonical, err := canonicalize(raw)
	if err != nil {
		return outcome{}, invalid("%v", err)
	}
	hash := payloadHash(t.Name, canonical)
	if existing := c.keys[actorKey{caller.id, key}]; existing != nil {
		return replay(existing, hash)
	}

	a, err := c.authorize(t, in, caller, nil)
	if err != nil {
		return outcome{}, err
	}
	lvl := a.decision.level
	status := initialStatus(lvl)
	var failure *apiError
	if !lvl.allowed() {
		failure = a.refusal()
	}
	// A proposal is kept as its tool pins it: the arguments as the tool
	// read them, not as they were written (an id in upper case comes back
	// in lower), which is what answer_pending and the views compare with.
	// The hash stays the call's as made, which is what a retry presents.
	if status == actProposed && t.impl.pin != nil {
		if err := t.impl.pin(c, a.decision.member, in); err != nil {
			e, ok := asAPI(err)
			if !ok {
				return outcome{}, err
			}
			status, failure = actFailed, e
		} else if canonical, err = pinned(in); err != nil {
			return outcome{}, fmt.Errorf("%s: pinned arguments: %w", t.Name, err)
		}
	}
	now := c.now()
	act := &action{
		id: newID(), actor: caller, member: a.decision.member, course: a.course, actionType: t.Name,
		targetType: a.target.typ, targetID: a.target.id, payload: canonical, hash: hash, key: key,
		authz: lvl, status: status, reviewState: reviewNone, createdAt: now,
	}
	if failure != nil {
		act.result = errorResult(failure)
	}
	c.recordAction(act)

	out := outcome{Status: status, ActionID: act.id, ReviewState: reviewNone, Error: failure}
	switch status {
	case actDenied, actFailed:
		return out, nil
	case actProposed:
		id := act.id
		payload := map[string]any{"action_type": t.Name, "target_type": act.targetType, "target_id": act.targetID}
		c.flush([]*event{{typ: "action.proposed", course: a.course, actionID: &id, subjectType: "action", subjectID: &id,
			payload: mustJSON(payload)}})
		return out, nil
	}

	ec := &execCtx{now: now, actor: caller, member: a.decision.member, course: a.course, actionID: act.id, createdAt: now}
	res, err := t.impl.execute(c, ec, in)
	if err != nil {
		e, ok := asAPI(err)
		if !ok {
			c.forgetAction(act)
			return outcome{}, fmt.Errorf("%s: %w", t.Name, err)
		}
		act.status, act.result = actFailed, errorResult(e)
		out.Status, out.Error = actFailed, e
		return out, nil
	}
	full, err := json.Marshal(res)
	if err != nil {
		c.forgetAction(act)
		return outcome{}, fmt.Errorf("%s: result: %w", t.Name, err)
	}
	c.flush(ec.events)
	if lvl == pendingReview {
		out.ReviewState = reviewPending
	}
	act.status, act.executedAt, act.reviewState, act.result = actExecuted, &now, out.ReviewState, full
	out.Status, out.Result = actExecuted, full
	return out, nil
}

// pinned is a proposal's arguments as its tool read them, canonical.
func pinned(in any) ([]byte, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	return canonicalize(raw)
}

// checkKey holds a write's idempotency key to 1 to 200 characters of text.
func checkKey(t *toolDef, key string) error {
	switch {
	case key == "":
		return invalid("%s changes state, so it needs an idempotency key", t.Name)
	case !utf8.ValidString(key) || strings.ContainsRune(key, 0):
		return invalid("the idempotency key must be UTF-8 text without U+0000")
	case utf8.RuneCountInString(key) > maxKeyChars:
		return invalid("the idempotency key is longer than %d characters", maxKeyChars)
	}
	return nil
}

// replay answers a call whose key was used before: the stored outcome as it
// stands now, or a conflict if the arguments differ.
func replay(a *action, hash string) (outcome, error) {
	if a.hash != hash {
		return outcome{}, newErr(codeIdempotencyConflict,
			"this idempotency key was already used for a different %s call; use a new key for a new request", a.actionType).
			with("action_id", a.id)
	}
	out := outcome{Status: a.status, ActionID: a.id, ReviewState: a.reviewState, Replayed: true}
	switch a.status {
	case actDenied, actFailed, actCancelled:
		out.Error = storedError(a.result)
	default:
		if len(a.result) > 0 {
			out.Result = a.result
		}
	}
	return out, nil
}

// actorKey is where an idempotency key is unique: per actor.
type actorKey struct{ actor, key string }

func (c *Core) recordAction(a *action) {
	c.actions[a.id] = a
	c.actionList = append(c.actionList, a)
	c.keys[actorKey{a.actor.id, a.key}] = a
	if a.actionType == toolConversationAnswer && a.targetID != nil {
		if cv := c.conversations[*a.targetID]; cv != nil {
			cv.answers = append(cv.answers, a)
		}
	}
}

// forgetAction takes back an action row after a fault of the fake's own:
// Core rolls such a call back whole.
func (c *Core) forgetAction(a *action) {
	delete(c.actions, a.id)
	delete(c.keys, actorKey{a.actor.id, a.key})
	c.actionList = slices.DeleteFunc(c.actionList, func(x *action) bool { return x == a })
	if a.actionType == toolConversationAnswer && a.targetID != nil {
		if cv := c.conversations[*a.targetID]; cv != nil {
			cv.answers = slices.DeleteFunc(cv.answers, func(x *action) bool { return x == a })
		}
	}
}

// flush gives events their place in the feed, in the order emitted, and
// wakes the calls waiting for news (wait.go).
func (c *Core) flush(evs []*event) {
	defer c.newsFlushed(evs)
	now := c.now()
	for _, e := range evs {
		c.seq++
		e.seq, e.occurredAt = c.seq, now
		if len(e.payload) == 0 || string(e.payload) == "null" {
			e.payload = json.RawMessage("{}")
		}
		if e.course != nil { // an event of no course is in no course's feed
			e.course.events = append(e.course.events, e)
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// newID is a fresh UUID v7, as Core makes its keys: time-ordered, so that
// ordering by id is ordering by creation.
func newID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
