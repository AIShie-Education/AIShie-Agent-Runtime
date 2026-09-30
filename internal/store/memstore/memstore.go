// Package memstore keeps the runtime's state in memory, for one worker and
// for tests (docs/design.md §8). It loses everything on restart: a restarted
// worker may find its idempotency keys taken in Core (idempotency_conflict)
// and moves on to the next attempt number. Use pgstore for anything that
// matters.
//
// Everything is copied in and out, so that no caller shares a slice with the
// store, and times are kept to the microsecond, as pgstore keeps them, so
// that both behave alike (storetest).
package memstore

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// Store is store.Store in maps under one mutex.
type Store struct {
	mu  sync.Mutex
	now func() time.Time
	// seq orders rows written at one instant in the order they were
	// written.
	seq int64

	leases map[string]lease
	// swept is when expired leases were last dropped.
	swept    time.Time
	attempts map[agentKey]*attemptRow
	cursors  map[cursorKey]string
	notes    map[conversationKey][]noteRow
	seats    map[seatKey]store.SeatRef
	calls    map[agentKey]store.LLMCall
	answers  map[agentKey]answerRow
	states   map[string]store.AgentState
	secrets  map[string]store.Secret
	people   map[string]store.Person
	hosted   map[string]store.HostedAgent
	courses  map[seatKey]store.HostedCourse
	rev      int64
	audit    []store.AuditEvent
	auditID  int64
	ocr      map[string]store.OCRText
	settings map[string]store.SiteSetting
	offers   map[string]store.SchoolOffer
	prices   map[string]store.SitePrice
	// pricesAt is when the site's prices last changed.
	pricesAt time.Time
	tenants  map[string]store.TenantQuota
	// tx is the transcriber's.
	tx transcription
}

type lease struct {
	holder  string
	expires time.Time
}

// agentKey is a row keyed on its agent and an id of its own: an attempt's
// idempotency key, or a ledger row's id.
type agentKey struct{ agent, id string }

type cursorKey struct{ agent, member, kind string }

type conversationKey struct{ agent, member, conversation string }

type seatKey struct{ agent, member string }

type attemptRow struct {
	store.Attempt
	seq int64
}

type noteRow struct {
	store.Note
	seq int64
}

type answerRow struct {
	store.AnswerRecord
	seq int64
}

var _ store.Store = (*Store)(nil)

// New is an empty store on the process's clock.
func New() *Store {
	return &Store{
		now:      time.Now,
		leases:   map[string]lease{},
		attempts: map[agentKey]*attemptRow{},
		cursors:  map[cursorKey]string{},
		notes:    map[conversationKey][]noteRow{},
		seats:    map[seatKey]store.SeatRef{},
		calls:    map[agentKey]store.LLMCall{},
		answers:  map[agentKey]answerRow{},
		states:   map[string]store.AgentState{},
		secrets:  map[string]store.Secret{},
		people:   map[string]store.Person{},
		hosted:   map[string]store.HostedAgent{},
		courses:  map[seatKey]store.HostedCourse{},
		ocr:      map[string]store.OCRText{},
		settings: map[string]store.SiteSetting{},
		offers:   map[string]store.SchoolOffer{},
		prices:   map[string]store.SitePrice{},
		tenants:  map[string]store.TenantQuota{},
		tx:       transcription{jobs: map[string]store.TranscriptionJob{}},
	}
}

// SetClock replaces the clock leases expire on and zero times are filled
// from, for tests.
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Close does nothing: there is nothing to release.
func (s *Store) Close() error { return nil }

// clock is the store's now, as it keeps times. Called with the lock held.
func (s *Store) clock() time.Time { return keep(s.now()) }

// orNow is t as the store keeps it, or the store's now when t is zero.
// Called with the lock held.
func (s *Store) orNow(t time.Time) time.Time {
	if t.IsZero() {
		return s.clock()
	}
	return keep(t)
}

// next is the next write's place in order. Called with the lock held.
func (s *Store) next() int64 {
	s.seq++
	return s.seq
}

// keep is t as Postgres would keep it: to the microsecond, in UTC, with no
// monotonic reading.
func keep(t time.Time) time.Time { return t.Truncate(time.Microsecond).UTC() }

// required refuses a write missing the ids it is keyed on. pairs holds a
// field's name, then its value.
func required(pairs ...string) error {
	var missing []string
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] == "" {
			missing = append(missing, pairs[i])
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("store: %s required", strings.Join(missing, ", "))
	}
	return nil
}

// AcquireLease takes name for holder until ttl from now on the process's
// clock, when it is free, expired, or holder's already.
func (s *Store) AcquireLease(_ context.Context, name, holder string, ttl time.Duration) (bool, error) {
	if err := required("name", name, "holder", holder); err != nil {
		return false, err
	}
	if ttl <= 0 {
		return false, fmt.Errorf("store: lease %s for %s: ttl must be positive", name, ttl)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLeases(now)
	if l, ok := s.leases[name]; ok && l.holder != holder && !l.expired(now) {
		return false, nil
	}
	s.leases[name] = lease{holder: holder, expires: now.Add(ttl)}
	return true, nil
}

// expired reports whether the lease is past its time at now, as pgstore's
// expires_at < now() has it.
func (l lease) expired(now time.Time) bool { return l.expires.Before(now) }

// sweepLeases drops leases long expired, so that a holder that never
// releases (a crash, a bug) does not grow the map for ever. Called with the
// lock held.
func (s *Store) sweepLeases(now time.Time) {
	if now.Sub(s.swept) < time.Minute {
		return
	}
	s.swept = now
	for name, l := range s.leases {
		if l.expired(now) {
			delete(s.leases, name)
		}
	}
}

// ReleaseLease frees name if holder has it.
func (s *Store) ReleaseLease(_ context.Context, name, holder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.leases[name]; ok && l.holder == holder {
		delete(s.leases, name)
	}
	return nil
}

// copyAttempt is a with bytes of its own.
func copyAttempt(a store.Attempt) *store.Attempt {
	a.Args = append([]byte{}, a.Args...)
	return &a
}

// PutAttempt writes a's row, or returns the row already under its key with
// store.ErrExists. An empty State is written as sending. A row without its
// seat's member_id is refused: Unsettled and PurgeMember find it by that.
func (s *Store) PutAttempt(_ context.Context, a store.Attempt) (*store.Attempt, error) {
	if err := required("agent_id", a.AgentID, "key", a.Key, "member_id", a.MemberID); err != nil {
		return nil, err
	}
	if a.State == "" {
		a.State = store.AttemptSending
	}
	if !a.State.Known() {
		return nil, fmt.Errorf("store: attempt %s: unknown state %q", a.Key, a.State)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := agentKey{a.AgentID, a.Key}
	if row, ok := s.attempts[k]; ok {
		return copyAttempt(row.Attempt), store.ErrExists
	}
	a.CreatedAt = s.orNow(a.CreatedAt)
	a.UpdatedAt = a.CreatedAt
	row := &attemptRow{Attempt: *copyAttempt(a), seq: s.next()}
	s.attempts[k] = row
	return copyAttempt(row.Attempt), nil
}

// FinishAttempt records o for the attempt under key. An action id or posted
// message id already known is kept when o has none; o's error code and
// reason replace the old ones.
func (s *Store) FinishAttempt(_ context.Context, agentID, key string, o store.Outcome) error {
	if !o.State.Known() {
		return fmt.Errorf("store: attempt %s: unknown state %q", key, o.State)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.attempts[agentKey{agentID, key}]
	if !ok {
		return fmt.Errorf("attempt %s: %w", key, store.ErrNotFound)
	}
	row.State = o.State
	if o.ActionID != "" {
		row.ActionID = o.ActionID
	}
	if o.PostedMessageID != "" {
		row.PostedMessageID = o.PostedMessageID
	}
	row.ErrorCode, row.Reason = o.ErrorCode, o.Reason
	row.UpdatedAt = s.clock()
	return nil
}

// Attempt is the attempt under key, or store.ErrNotFound.
func (s *Store) Attempt(_ context.Context, agentID, key string) (*store.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.attempts[agentKey{agentID, key}]
	if !ok {
		return nil, fmt.Errorf("attempt %s: %w", key, store.ErrNotFound)
	}
	return copyAttempt(row.Attempt), nil
}

// attemptsWhere lists the agent's attempts for which keep holds, sorted by
// less, then as they were written. Called with the lock held.
func (s *Store) attemptsWhere(agentID string, keep func(*attemptRow) bool, less func(a, b *attemptRow) int) []store.Attempt {
	var rows []*attemptRow
	for k, row := range s.attempts {
		if k.agent == agentID && keep(row) {
			rows = append(rows, row)
		}
	}
	slices.SortFunc(rows, func(a, b *attemptRow) int {
		return cmp.Or(less(a, b), cmp.Compare(a.seq, b.seq))
	})
	var out []store.Attempt
	for _, row := range rows {
		out = append(out, *copyAttempt(row.Attempt))
	}
	return out
}

// byAge orders attempts oldest first.
func byAge(a, b *attemptRow) int { return a.CreatedAt.Compare(b.CreatedAt) }

// AttemptsFor lists the attempts at answering one message, by number.
func (s *Store) AttemptsFor(_ context.Context, agentID, conversationID, messageID string) ([]store.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attemptsWhere(agentID,
		func(r *attemptRow) bool { return r.ConversationID == conversationID && r.MessageID == messageID },
		func(a, b *attemptRow) int { return cmp.Or(cmp.Compare(a.No, b.No), byAge(a, b)) }), nil
}

// AttemptByAction is the oldest attempt Core recorded as actionID, or
// store.ErrNotFound. An empty actionID names no action.
func (s *Store) AttemptByAction(_ context.Context, agentID, actionID string) (*store.Attempt, error) {
	if actionID == "" {
		return nil, fmt.Errorf("attempt of no action: %w", store.ErrNotFound)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := s.attemptsWhere(agentID, func(r *attemptRow) bool { return r.ActionID == actionID }, byAge)
	if len(found) == 0 {
		return nil, fmt.Errorf("attempt of action %s: %w", actionID, store.ErrNotFound)
	}
	return &found[0], nil
}

// Unsettled lists one seat's attempts still sending or proposed, oldest
// first.
func (s *Store) Unsettled(_ context.Context, agentID, memberID string) ([]store.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attemptsWhere(agentID, func(r *attemptRow) bool {
		return r.MemberID == memberID && (r.State == store.AttemptSending || r.State == store.AttemptProposed)
	}, byAge), nil
}

// Cursor is the cursor, or "" when there is none yet.
func (s *Store) Cursor(_ context.Context, agentID, memberID, kind string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursors[cursorKey{agentID, memberID, kind}], nil
}

// SetCursor records value as the seat's cursor of kind.
func (s *Store) SetCursor(_ context.Context, agentID, memberID, kind, value string) error {
	if err := required("agent_id", agentID, "member_id", memberID, "kind", kind); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursors[cursorKey{agentID, memberID, kind}] = value
	return nil
}

// AddNote remembers n in its conversation.
func (s *Store) AddNote(_ context.Context, n store.Note) error {
	if err := required("agent_id", n.AgentID, "member_id", n.MemberID, "conversation_id", n.ConversationID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n.CreatedAt = s.orNow(n.CreatedAt)
	k := conversationKey{n.AgentID, n.MemberID, n.ConversationID}
	s.notes[k] = append(s.notes[k], noteRow{Note: n, seq: s.next()})
	return nil
}

// Notes are the conversation's newest limit notes, oldest first; none when
// limit is not positive.
func (s *Store) Notes(_ context.Context, agentID, memberID, conversationID string, limit int) ([]store.Note, error) {
	if limit <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := slices.Clone(s.notes[conversationKey{agentID, memberID, conversationID}])
	slices.SortFunc(rows, func(a, b noteRow) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.seq, b.seq))
	})
	rows = rows[max(0, len(rows)-limit):]
	var out []store.Note
	for _, row := range rows {
		out = append(out, row.Note)
	}
	return out, nil
}

// ForgetMessage removes the notes about messageID. An empty messageID names
// no message, and removes nothing.
func (s *Store) ForgetMessage(_ context.Context, agentID, memberID, conversationID, messageID string) error {
	if messageID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := conversationKey{agentID, memberID, conversationID}
	rows := slices.DeleteFunc(s.notes[k], func(r noteRow) bool { return r.MessageID == messageID })
	if len(rows) == 0 {
		delete(s.notes, k)
	} else {
		s.notes[k] = rows
	}
	return nil
}

// PurgeMember removes everything the store holds in the seat: its notes,
// its attempts, whose bytes hold its answers, and its cursors. The seat's
// row is ForgetSeat's to remove, and the ledger keeps its ids and numbers.
func (s *Store) PurgeMember(_ context.Context, agentID, memberID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.notes {
		if k.agent == agentID && k.member == memberID {
			delete(s.notes, k)
		}
	}
	for k, row := range s.attempts {
		if k.agent == agentID && row.MemberID == memberID {
			delete(s.attempts, k)
		}
	}
	for k := range s.cursors {
		if k.agent == agentID && k.member == memberID {
			delete(s.cursors, k)
		}
	}
	return nil
}

// PurgeAgent removes everything the store holds of an agent but its
// ledger: every seat's notes, attempts and cursors, its seats, its state
// and its leases.
func (s *Store) PurgeAgent(_ context.Context, agentID string) error {
	if err := required("agent_id", agentID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.notes {
		if k.agent == agentID {
			delete(s.notes, k)
		}
	}
	for k := range s.attempts {
		if k.agent == agentID {
			delete(s.attempts, k)
		}
	}
	for k := range s.cursors {
		if k.agent == agentID {
			delete(s.cursors, k)
		}
	}
	for k := range s.seats {
		if k.agent == agentID {
			delete(s.seats, k)
		}
	}
	delete(s.states, agentID)
	for name := range s.leases {
		if name == "agent:"+agentID || strings.HasPrefix(name, "conv:"+agentID+":") {
			delete(s.leases, name)
		}
	}
	return nil
}

// copySeat is r with a GoneAt and perms of its own.
func copySeat(r store.SeatRef) store.SeatRef {
	if r.GoneAt != nil {
		gone := *r.GoneAt
		r.GoneAt = &gone
	}
	r.Perms = maps.Clone(r.Perms)
	if r.Perms == nil {
		r.Perms = map[string]string{}
	}
	return r
}

// SeatSeen records the seat as current, as r says it is, clearing any
// GoneAt.
func (s *Store) SeatSeen(_ context.Context, r store.SeatRef) error {
	if err := required("agent_id", r.AgentID, "member_id", r.MemberID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.SeenAt, r.GoneAt = s.orNow(r.SeenAt), nil
	s.seats[seatKey{r.AgentID, r.MemberID}] = copySeat(r)
	return nil
}

// SeatGone records when the seat was first missed; a later call keeps the
// first time. A seat never seen is store.ErrNotFound.
func (s *Store) SeatGone(_ context.Context, agentID, memberID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := seatKey{agentID, memberID}
	r, ok := s.seats[k]
	if !ok {
		return fmt.Errorf("seat %s: %w", memberID, store.ErrNotFound)
	}
	if r.GoneAt == nil {
		gone := s.orNow(at)
		r.GoneAt = &gone
		s.seats[k] = r
	}
	return nil
}

// seatsWhere lists the seats for which keep holds, sorted by less. Called
// with the lock held.
func (s *Store) seatsWhere(keep func(store.SeatRef) bool, less func(a, b store.SeatRef) int) []store.SeatRef {
	var out []store.SeatRef
	for _, r := range s.seats {
		if keep(r) {
			out = append(out, copySeat(r))
		}
	}
	slices.SortFunc(out, less)
	return out
}

// KnownSeats lists the agent's seats, current and gone, by member id.
func (s *Store) KnownSeats(_ context.Context, agentID string) ([]store.SeatRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seatsWhere(
		func(r store.SeatRef) bool { return r.AgentID == agentID },
		func(a, b store.SeatRef) int { return strings.Compare(a.MemberID, b.MemberID) }), nil
}

// SeatsGoneBefore lists seats of any agent gone before t, in the order they
// went.
func (s *Store) SeatsGoneBefore(_ context.Context, t time.Time) ([]store.SeatRef, error) {
	t = keep(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seatsWhere(
		func(r store.SeatRef) bool { return r.GoneAt != nil && r.GoneAt.Before(t) },
		func(a, b store.SeatRef) int {
			return cmp.Or(a.GoneAt.Compare(*b.GoneAt), strings.Compare(a.AgentID, b.AgentID), strings.Compare(a.MemberID, b.MemberID))
		}), nil
}

// ForgetSeat removes the seat's row; a seat not known is nothing.
func (s *Store) ForgetSeat(_ context.Context, agentID, memberID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seats, seatKey{agentID, memberID})
	return nil
}

// RecordLLMCall records c, once: a call already recorded under its agent
// and id is left as it is.
func (s *Store) RecordLLMCall(_ context.Context, c store.LLMCall) error {
	if err := store.CheckCall(c); err != nil {
		return err
	}
	if len(c.RawUsage) > 0 && !json.Valid(c.RawUsage) {
		return fmt.Errorf("store: llm call %s: raw usage is not JSON", c.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := agentKey{c.AgentID, c.ID}
	if _, ok := s.calls[k]; ok {
		return nil
	}
	c.At, c.Kind = s.orNow(c.At), c.KindOf()
	c.RawUsage = append(json.RawMessage(nil), c.RawUsage...)
	s.calls[k] = c
	return nil
}

// RecordAnswer records a, once: an answer already recorded under its agent
// and id is left as it is.
func (s *Store) RecordAnswer(_ context.Context, a store.AnswerRecord) error {
	if err := required("id", a.ID, "agent_id", a.AgentID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := agentKey{a.AgentID, a.ID}
	if _, ok := s.answers[k]; ok {
		return nil
	}
	a.At = s.orNow(a.At)
	s.answers[k] = answerRow{AnswerRecord: a, seq: s.next()}
	return nil
}

// errNoScope is Spend asked for everything, which no quota is.
var errNoScope = errors.New("store: spend needs at least one scope field")

// inScope reports whether a row with these fields falls in sc; empty fields
// of sc do not filter.
func inScope(sc store.SpendScope, agent, tenant, course, opener, keySource string) bool {
	match := func(want, got string) bool { return want == "" || want == got }
	return match(sc.AgentID, agent) && match(sc.TenantID, tenant) && match(sc.CourseID, course) &&
		match(sc.OpenerMemberID, opener) && match(sc.KeySource, keySource)
}

// Spend is what the scope has used since a time: its billable answers, and
// the cost of its model calls.
func (s *Store) Spend(_ context.Context, sc store.SpendScope, since time.Time) (store.Spend, error) {
	if sc == (store.SpendScope{}) {
		return store.Spend{}, errNoScope
	}
	since = keep(since)
	s.mu.Lock()
	defer s.mu.Unlock()
	var out store.Spend
	for _, a := range s.answers {
		if a.Billable && !a.At.Before(since) && inScope(sc, a.AgentID, a.TenantID, a.CourseID, a.OpenerMemberID, a.KeySource) {
			out.Answers++
		}
	}
	for _, c := range s.calls {
		if !c.At.Before(since) && inScope(sc, c.AgentID, c.TenantID, c.CourseID, c.OpenerMemberID, c.KeySource) {
			out.CostPUSD += c.CostPUSD
		}
	}
	return out, nil
}

// RecentAnswerCosts are the costs of the agent's newest n billable answers,
// newest first; none when n is not positive.
func (s *Store) RecentAnswerCosts(_ context.Context, agentID string, n int) ([]int64, error) {
	if n <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []answerRow
	for k, a := range s.answers {
		if k.agent == agentID && a.Billable {
			rows = append(rows, a)
		}
	}
	slices.SortFunc(rows, func(a, b answerRow) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(b.seq, a.seq))
	})
	var out []int64
	for _, a := range rows[:min(n, len(rows))] {
		out = append(out, a.CostPUSD)
	}
	return out, nil
}

// SetAgentState records the agent's state, replacing the one before
// unless that one names a later config version.
func (s *Store) SetAgentState(_ context.Context, st store.AgentState) error {
	if err := required("agent_id", st.AgentID, "state", st.State); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.states[st.AgentID]; ok && old.ConfigVersion > st.ConfigVersion {
		return nil
	}
	st.UpdatedAt = s.orNow(st.UpdatedAt)
	s.states[st.AgentID] = st
	return nil
}

// AgentState is one agent's state, or store.ErrNotFound.
func (s *Store) AgentState(_ context.Context, agentID string) (*store.AgentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[agentID]
	if !ok {
		return nil, fmt.Errorf("the state of agent %s: %w", agentID, store.ErrNotFound)
	}
	return &st, nil
}

// AgentStates lists every agent's state, by agent id.
func (s *Store) AgentStates(_ context.Context) ([]store.AgentState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.AgentState
	for _, st := range s.states {
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b store.AgentState) int { return strings.Compare(a.AgentID, b.AgentID) })
	return out, nil
}

// copySecret is sec with bytes of its own.
func copySecret(sec store.Secret) store.Secret {
	sec.WrappedDEK = slices.Clone(sec.WrappedDEK)
	sec.Nonce = slices.Clone(sec.Nonce)
	sec.Ciphertext = slices.Clone(sec.Ciphertext)
	return sec
}

// PutSecret stores sec; an id already taken is store.ErrExists.
func (s *Store) PutSecret(_ context.Context, sec store.Secret) error {
	if err := store.CheckSecret(sec); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.secrets[sec.ID]; ok {
		return fmt.Errorf("secret %s: %w", sec.ID, store.ErrExists)
	}
	sec.CreatedAt = s.orNow(sec.CreatedAt)
	s.secrets[sec.ID] = copySecret(sec)
	return nil
}

// Secret is the secret id, or store.ErrNotFound.
func (s *Store) Secret(_ context.Context, id string) (*store.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, ok := s.secrets[id]
	if !ok {
		return nil, fmt.Errorf("secret %s: %w", id, store.ErrNotFound)
	}
	sec = copySecret(sec)
	return &sec, nil
}

// ListSecrets lists up to limit secrets whose ids sort after afterID, by id.
func (s *Store) ListSecrets(_ context.Context, afterID string, limit int) ([]store.Secret, error) {
	if limit <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Secret
	for id, sec := range s.secrets {
		if id > afterID {
			out = append(out, copySecret(sec))
		}
	}
	slices.SortFunc(out, func(a, b store.Secret) int { return strings.Compare(a.ID, b.ID) })
	return out[:min(limit, len(out))], nil
}

// RewrapSecret replaces the secret's wrapped data key, if fromKEKID still
// wraps it.
func (s *Store) RewrapSecret(_ context.Context, id, fromKEKID, kekID string, wrapped []byte) error {
	if kekID == "" || len(wrapped) == 0 {
		return fmt.Errorf("store: secret %s: kek_id and the wrapped key required", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, ok := s.secrets[id]
	switch {
	case !ok:
		return fmt.Errorf("secret %s: %w", id, store.ErrNotFound)
	case sec.KEKID != fromKEKID:
		return fmt.Errorf("secret %s: %w", id, store.ErrConflict)
	}
	sec.KEKID, sec.WrappedDEK = kekID, slices.Clone(wrapped)
	s.secrets[id] = sec
	return nil
}

// DeleteSecret destroys the secret; one that is not there is nothing.
func (s *Store) DeleteSecret(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.secretInUse(id); err != nil {
		return err
	}
	delete(s.secrets, id)
	return nil
}

// secretInUse refuses to delete a secret a hosted agent, or an offer of
// the school's plan, refers to. Called with the lock held.
func (s *Store) secretInUse(id string) error {
	for _, a := range s.hosted {
		if a.TokenSecretID == id || a.KeySecretID == id {
			return fmt.Errorf("secret %s: %w", id, store.ErrInUse)
		}
	}
	for _, o := range s.offers {
		if o.KeySecretID == id {
			return fmt.Errorf("secret %s: %w", id, store.ErrInUse)
		}
	}
	if c := s.tx.cred; c != nil && c.SecretID == id {
		return fmt.Errorf("secret %s: %w", id, store.ErrInUse)
	}
	return nil
}

// PutPerson records p, replacing what was known of them.
func (s *Store) PutPerson(_ context.Context, p store.Person) error {
	if err := required("core_actor_id", p.CoreActorID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p.LastSeenAt = s.orNow(p.LastSeenAt)
	s.people[p.CoreActorID] = p
	return nil
}

// Person is the person, or store.ErrNotFound.
func (s *Store) Person(_ context.Context, coreActorID string) (*store.Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.people[coreActorID]
	if !ok {
		return nil, fmt.Errorf("person %s: %w", coreActorID, store.ErrNotFound)
	}
	return &p, nil
}

// copyHosted is a with settings of its own.
func copyHosted(a store.HostedAgent) store.HostedAgent {
	a.Settings = slices.Clone(a.Settings)
	return a
}

// storedSecret finds a secret for store.CheckAgentSecrets. Called with the
// lock held.
func (s *Store) storedSecret(id string) (*store.Secret, error) {
	sec, ok := s.secrets[id]
	if !ok {
		return nil, fmt.Errorf("secret %s: %w", id, store.ErrNotFound)
	}
	return &sec, nil
}

// checkSecretsFree refuses secrets given for an agent whose ids are taken,
// and secrets the agent refers to that another agent does. Called with the
// lock held.
func (s *Store) checkSecretsFree(a store.HostedAgent, secrets []store.Secret) error {
	for _, sec := range secrets {
		if err := store.CheckSecret(sec); err != nil {
			return err
		}
		if _, ok := s.secrets[sec.ID]; ok {
			return fmt.Errorf("secret %s: %w", sec.ID, store.ErrExists)
		}
	}
	for _, other := range s.hosted {
		if other.ID == a.ID {
			continue
		}
		for _, id := range []string{a.TokenSecretID, a.KeySecretID} {
			if id != "" && (other.TokenSecretID == id || other.KeySecretID == id) {
				return fmt.Errorf("store: hosted agent %s: secret %s is agent %s's", a.ID, id, other.ID)
			}
		}
	}
	return nil
}

// CreateHostedAgent stores a at version 1 with its secrets.
func (s *Store) CreateHostedAgent(_ context.Context, a store.HostedAgent, secrets ...store.Secret) (*store.HostedAgent, error) {
	a, err := store.CheckHostedAgent(a)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.hosted[a.ID]; ok {
		return nil, fmt.Errorf("hosted agent %s: %w", a.ID, store.ErrExists)
	}
	for _, other := range s.hosted {
		if other.CoreActorID == a.CoreActorID {
			return nil, fmt.Errorf("hosted agent of actor %s: %w", a.CoreActorID, store.ErrExists)
		}
	}
	if err := s.checkSecretsFree(a, secrets); err != nil {
		return nil, err
	}
	if err := store.CheckAgentSecrets(a, secrets, s.storedSecret); err != nil {
		return nil, err
	}
	for _, sec := range secrets {
		sec.CreatedAt = s.orNow(sec.CreatedAt)
		s.secrets[sec.ID] = copySecret(sec)
	}
	a.Version, a.CreatedAt = 1, s.orNow(a.CreatedAt)
	a.UpdatedAt = a.CreatedAt
	s.hosted[a.ID] = copyHosted(a)
	s.rev++
	out := copyHosted(a)
	return &out, nil
}

// HostedAgent is the agent id, or store.ErrNotFound.
func (s *Store) HostedAgent(_ context.Context, id string) (*store.HostedAgent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.hosted[id]
	if !ok {
		return nil, fmt.Errorf("hosted agent %s: %w", id, store.ErrNotFound)
	}
	out := copyHosted(a)
	return &out, nil
}

// hostedWhere lists the hosted agents keep holds for, by id. Called with the
// lock held.
func (s *Store) hostedWhere(keep func(store.HostedAgent) bool) []store.HostedAgent {
	var out []store.HostedAgent
	for _, a := range s.hosted {
		if keep(a) {
			out = append(out, copyHosted(a))
		}
	}
	slices.SortFunc(out, func(x, y store.HostedAgent) int { return strings.Compare(x.ID, y.ID) })
	return out
}

// HostedAgentByActor is the agent of a Core actor, or store.ErrNotFound.
func (s *Store) HostedAgentByActor(_ context.Context, coreActorID string) (*store.HostedAgent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := s.hostedWhere(func(a store.HostedAgent) bool { return a.CoreActorID == coreActorID })
	if len(found) == 0 {
		return nil, fmt.Errorf("hosted agent of actor %s: %w", coreActorID, store.ErrNotFound)
	}
	return &found[0], nil
}

// HostedAgentsOwnedBy lists an owner's agents, by id.
func (s *Store) HostedAgentsOwnedBy(_ context.Context, ownerActorID string) ([]store.HostedAgent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hostedWhere(func(a store.HostedAgent) bool { return a.OwnerActorID == ownerActorID }), nil
}

// HostedAgents lists every hosted agent, by id.
func (s *Store) HostedAgents(_ context.Context) ([]store.HostedAgent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hostedWhere(func(store.HostedAgent) bool { return true }), nil
}

// UpdateHostedAgent writes a over the agent of its id, if a.Version is
// still its version.
func (s *Store) UpdateHostedAgent(_ context.Context, a store.HostedAgent, secrets ...store.Secret) (*store.HostedAgent, error) {
	a, err := store.CheckHostedAgent(a)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.hosted[a.ID]
	switch {
	case !ok:
		return nil, fmt.Errorf("hosted agent %s: %w", a.ID, store.ErrNotFound)
	case old.Version != a.Version:
		return nil, fmt.Errorf("hosted agent %s at version %d: %w", a.ID, a.Version, store.ErrConflict)
	case old.CoreActorID != a.CoreActorID || old.TenantID != a.TenantID:
		return nil, fmt.Errorf("store: hosted agent %s: its Core actor and tenant do not change", a.ID)
	}
	if err := s.checkSecretsFree(a, secrets); err != nil {
		return nil, err
	}
	if err := store.CheckAgentSecrets(a, secrets, s.storedSecret); err != nil {
		return nil, err
	}
	for _, sec := range secrets {
		sec.CreatedAt = s.orNow(sec.CreatedAt)
		s.secrets[sec.ID] = copySecret(sec)
	}
	for _, id := range []string{old.TokenSecretID, old.KeySecretID} {
		if id != "" && id != a.TokenSecretID && id != a.KeySecretID {
			delete(s.secrets, id)
		}
	}
	a.Version, a.CreatedAt, a.UpdatedAt = old.Version+1, old.CreatedAt, s.clock()
	s.hosted[a.ID] = copyHosted(a)
	s.rev++
	out := copyHosted(a)
	return &out, nil
}

// SetHostedAgentPaused pauses or resumes the agent, at version when it is
// not 0, whatever its version otherwise.
func (s *Store) SetHostedAgentPaused(_ context.Context, id string, paused bool, version int) (*store.HostedAgent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.hosted[id]
	switch {
	case !ok:
		return nil, fmt.Errorf("hosted agent %s: %w", id, store.ErrNotFound)
	case version != 0 && a.Version != version:
		return nil, fmt.Errorf("hosted agent %s at version %d: %w", id, version, store.ErrConflict)
	}
	a.Paused, a.Version, a.UpdatedAt = paused, a.Version+1, s.clock()
	s.hosted[id] = a
	s.rev++
	out := copyHosted(a)
	return &out, nil
}

// DeleteHostedAgent destroys the agent, its courses and its secrets, if it
// is still as cond says.
func (s *Store) DeleteHostedAgent(_ context.Context, id string, cond store.DeleteIf) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.hosted[id]
	switch {
	case !ok:
		return fmt.Errorf("hosted agent %s: %w", id, store.ErrNotFound)
	case cond.TokenSecretID != "" && a.TokenSecretID != cond.TokenSecretID, cond.Version != 0 && a.Version != cond.Version:
		return fmt.Errorf("hosted agent %s: %w", id, store.ErrConflict)
	}
	for k := range s.courses {
		if k.agent == id {
			delete(s.courses, k)
		}
	}
	delete(s.hosted, id)
	delete(s.secrets, a.TokenSecretID)
	if a.KeySecretID != "" {
		delete(s.secrets, a.KeySecretID)
	}
	s.rev++
	return nil
}

// PutHostedCourse writes an agent's settings for a course.
func (s *Store) PutHostedCourse(_ context.Context, c store.HostedCourse) error {
	c, err := store.CheckHostedCourse(c)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.hosted[c.AgentID]; !ok {
		return fmt.Errorf("hosted agent %s: %w", c.AgentID, store.ErrNotFound)
	}
	c.UpdatedAt = s.orNow(c.UpdatedAt)
	c.Settings = slices.Clone(c.Settings)
	s.courses[seatKey{c.AgentID, c.CourseID}] = c
	s.rev++
	return nil
}

// coursesWhere lists the courses keep holds for, by agent, then course.
// Called with the lock held.
func (s *Store) coursesWhere(keep func(store.HostedCourse) bool) []store.HostedCourse {
	var out []store.HostedCourse
	for _, c := range s.courses {
		if keep(c) {
			c.Settings = slices.Clone(c.Settings)
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(x, y store.HostedCourse) int {
		return cmp.Or(strings.Compare(x.AgentID, y.AgentID), strings.Compare(x.CourseID, y.CourseID))
	})
	return out
}

// HostedCourses lists one agent's courses, by course id.
func (s *Store) HostedCourses(_ context.Context, agentID string) ([]store.HostedCourse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.coursesWhere(func(c store.HostedCourse) bool { return c.AgentID == agentID }), nil
}

// ListHostedCourses lists every hosted agent's courses.
func (s *Store) ListHostedCourses(_ context.Context) ([]store.HostedCourse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.coursesWhere(func(store.HostedCourse) bool { return true }), nil
}

// DeleteHostedCourse removes an agent's settings for a course.
func (s *Store) DeleteHostedCourse(_ context.Context, agentID, courseID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := seatKey{agentID, courseID}
	if _, ok := s.courses[k]; ok {
		delete(s.courses, k)
		s.rev++
	}
	return nil
}

// RegistryRev is the registry's revision.
func (s *Store) RegistryRev(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rev, nil
}

// inSpan reports whether t is in [since, until).
func inSpan(t, since, until time.Time) bool { return !t.Before(since) && t.Before(until) }

// Usage is agentID's use in [since, until), a row per UTC day and course.
func (s *Store) Usage(_ context.Context, agentID string, since, until time.Time) ([]store.UsageRow, error) {
	if err := store.CheckSpan(agentID, since, until); err != nil {
		return nil, err
	}
	since, until = keep(since), keep(until)
	type key struct {
		day    time.Time
		course string
	}
	rows := map[key]*store.UsageRow{}
	row := func(at time.Time, course string) *store.UsageRow {
		k := key{store.UTCDay(at), course}
		if rows[k] == nil {
			rows[k] = &store.UsageRow{Day: k.day, CourseID: course, Outcomes: map[string]int{}}
		}
		return rows[k]
	}
	s.mu.Lock()
	for k, a := range s.answers {
		if k.agent == agentID && inSpan(a.At, since, until) {
			r := row(a.At, a.CourseID)
			r.Outcomes[a.Outcome]++
			if a.Billable {
				r.Answers++
			}
			r.Writes.Add(a.Writes)
		}
	}
	for k, c := range s.calls {
		if k.agent == agentID && inSpan(c.At, since, until) {
			r := row(c.At, c.CourseID)
			r.ModelCalls++
			r.InputTokens += c.Input
			r.CacheReadTokens += c.CacheRead
			r.CacheWriteTokens += c.CacheWrite
			r.OutputTokens += c.Output
			r.ReasoningTokens += c.Reasoning
			r.CostPUSD += c.CostPUSD
		}
	}
	s.mu.Unlock()
	out := make([]store.UsageRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b store.UsageRow) int {
		return cmp.Or(a.Day.Compare(b.Day), strings.Compare(a.CourseID, b.CourseID))
	})
	return out, nil
}

// TenantUsage is the use on keySource in [since, until), a row per
// tenant.
func (s *Store) TenantUsage(_ context.Context, keySource string, since, until time.Time) ([]store.TenantUsage, error) {
	if err := store.CheckKeySpan(keySource, since, until); err != nil {
		return nil, err
	}
	since, until = keep(since), keep(until)
	rows := map[string]*store.TenantUsage{}
	row := func(tenant string) *store.TenantUsage {
		if rows[tenant] == nil {
			rows[tenant] = &store.TenantUsage{TenantID: tenant}
		}
		return rows[tenant]
	}
	s.mu.Lock()
	for _, a := range s.answers {
		if a.KeySource == keySource && inSpan(a.At, since, until) {
			r := row(a.TenantID)
			if a.Billable {
				r.Answers++
			}
		}
	}
	for _, c := range s.calls {
		if c.KeySource == keySource && inSpan(c.At, since, until) && c.KindOf() == store.CallAnswer {
			r := row(c.TenantID)
			r.ModelCalls++
			r.CostPUSD += c.CostPUSD
		}
	}
	s.mu.Unlock()
	out := make([]store.TenantUsage, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b store.TenantUsage) int { return strings.Compare(a.TenantID, b.TenantID) })
	return out, nil
}

// AskerUsage is agentID's use in one course in [since, until), a row per
// asker.
func (s *Store) AskerUsage(_ context.Context, agentID, courseID string, since, until time.Time) ([]store.AskerUsage, error) {
	if err := store.CheckSpan(agentID, since, until); err != nil {
		return nil, err
	}
	since, until = keep(since), keep(until)
	rows := map[string]*store.AskerUsage{}
	row := func(opener string) *store.AskerUsage {
		if rows[opener] == nil {
			rows[opener] = &store.AskerUsage{OpenerMemberID: opener, Outcomes: map[string]int{}}
		}
		return rows[opener]
	}
	s.mu.Lock()
	for k, a := range s.answers {
		if k.agent == agentID && a.CourseID == courseID && inSpan(a.At, since, until) {
			r := row(a.OpenerMemberID)
			r.Outcomes[a.Outcome]++
			if a.Billable {
				r.Answers++
			}
		}
	}
	for k, c := range s.calls {
		if k.agent == agentID && c.CourseID == courseID && inSpan(c.At, since, until) {
			r := row(c.OpenerMemberID)
			r.ModelCalls++
			r.InputTokens += c.Input
			r.OutputTokens += c.Output
			r.CostPUSD += c.CostPUSD
		}
	}
	s.mu.Unlock()
	out := make([]store.AskerUsage, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b store.AskerUsage) int { return strings.Compare(a.OpenerMemberID, b.OpenerMemberID) })
	return out, nil
}
