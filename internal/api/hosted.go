package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// HostedAgent is a hosted agent as its owner reads it (the API contract,
// §4): what its row says, its status as worked out from the worker's
// state, its seats as the worker last read them, and today's use. Of its
// key, a hint alone; of its token, nothing (it is the runtime's, issued by
// the agent's id).
type HostedAgent struct {
	ID               string     `json:"id"`
	Version          int        `json:"version"`
	CoreActorID      string     `json:"core_actor_id"`
	OwnerActorID     string     `json:"owner_actor_id"`
	DisplayName      string     `json:"display_name"`
	Status           string     `json:"status"`
	Problem          *Problem   `json:"problem"`
	Paused           bool       `json:"paused"`
	Model            ModelSlots `json:"model"`
	OwnKey           *OwnKey    `json:"own_key"`
	Tools            ToolsView  `json:"tools"`
	Seats            []Seat     `json:"seats"`
	SeatsAsOf        *time.Time `json:"seats_as_of"`
	ProposalsWaiting int        `json:"proposals_waiting"`
	Today            Today      `json:"today"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// ModelSlots are the agent's models: its own key's, and the offer of the
// school's plan it is on (D8), each null for none. On the plan, the own
// model is the fallback.
type ModelSlots struct {
	Own    *OwnModel    `json:"own"`
	School *SchoolModel `json:"school"`
}

// ToolsView is what the agent's model may do beside reading (design §4):
// Writes, whether it is offered the writes its seats' perms allow in the
// conversations its owner opens, as its owner set it (true when they have
// not). Core still decides each write by the seat's perms.
type ToolsView struct {
	Writes bool `json:"writes"`
}

// OwnKey is what may be shown of the owner's stored key: its hint, and the
// provider it was given for (null for a key stored by hand).
type OwnKey struct {
	Hint     string  `json:"hint"`
	Provider *string `json:"provider"`
}

// Seat is one of the agent's seats, as facts the front end words for its
// owner (§4).
type Seat struct {
	CourseID    string `json:"course_id"`
	CourseCode  string `json:"course_code"`
	CourseTitle string `json:"course_title"`
	Section     string `json:"section"`
	SeatStatus  string `json:"seat_status"`
	// CourseStatus is null for a seat seen before the store kept it.
	CourseStatus *string `json:"course_status"`
	// Kind is course_tutor, delegate or member.
	Kind             string    `json:"kind"`
	Answers          bool      `json:"answers"`
	AnswerLevel      string    `json:"answer_level"`
	ReadsWork        bool      `json:"reads_work"`
	ReadsMaterial    bool      `json:"reads_material"`
	ProposalsWaiting int       `json:"proposals_waiting"`
	SeenAt           time.Time `json:"seen_at"`
}

// Today is what the agent used since the start of the UTC day: its
// billable answers and the cost of its model calls, in dollars with six
// places; and, on the school's plan, its owner's use of the plan.
type Today struct {
	Since   time.Time  `json:"since"`
	Answers int        `json:"answers"`
	CostUSD string     `json:"cost_usd"`
	School  *SchoolUse `json:"school"`
}

// SchoolUse is what an owner used of the school's plan since the start of
// the UTC day, across all of their agents (scope "owner"): the answers on
// the school's key, against per_owner_day, and the cost of its model
// calls, against its dollars when it has any; and the most answers one
// asker has of one agent in a course, per_asker_day. The worker checks
// them before each answer: once one is spent, the owner's own key answers
// when it stands behind the plan, and the plan's notice is posted when
// not.
type SchoolUse struct {
	Scope         string  `json:"scope"`
	Used          int     `json:"used"`
	Limit         int     `json:"limit"`
	UsedUSD       string  `json:"used_usd"`
	LimitUSD      *string `json:"limit_usd"`
	PerAskerLimit int     `json:"per_asker_limit"`
}

// seatOf is a seat as the store keeps it, worded as facts.
func seatOf(r store.SeatRef) Seat {
	m := core.Membership{Status: r.Status, CourseStatus: r.CourseStatus, Perms: r.Perms}
	s := Seat{
		CourseID: r.CourseID, CourseCode: r.CourseCode, CourseTitle: r.CourseTitle, Section: r.Section, SeatStatus: r.Status,
		Kind: "member", Answers: m.Answers(), AnswerLevel: m.Level("conversation_answer"),
		ReadsWork:     m.Level("submission_read") != core.LevelDenied || m.Level("grade_read") != core.LevelDenied,
		ReadsMaterial: m.Level("document_read") != core.LevelDenied, SeenAt: r.SeenAt.UTC(),
	}
	if r.CourseStatus != "" {
		cs := r.CourseStatus
		s.CourseStatus = &cs
	}
	switch {
	case r.AnswersCourse:
		s.Kind = "course_tutor"
	case r.PrincipalMemberID != "":
		s.Kind = "delegate"
	}
	return s
}

// sortSeats sorts seats by course code, then section.
func sortSeats(seats []Seat) {
	slices.SortStableFunc(seats, func(x, y Seat) int {
		return cmp.Or(strings.Compare(x.CourseCode, y.CourseCode), strings.Compare(x.Section, y.Section))
	})
}

// costUSD is pusd in dollars, to six places, rounded to the nearest.
func costUSD(pusd int64) string {
	neg := pusd < 0
	if neg {
		pusd = -pusd
	}
	micro := (pusd + 500_000) / 1_000_000
	s := fmt.Sprintf("%d.%06d", micro/1_000_000, micro%1_000_000)
	if neg && micro != 0 {
		s = "-" + s
	}
	return s
}

// errStore is the store not answering a read or write a view needs.
var errStore = errors.New("api: the store cannot be reached")

// view is row as its owner reads it: its status from the worker's state,
// its current seats with the proposals waiting in each, and today's use.
func (s *Server) view(ctx context.Context, row *store.HostedAgent) (*HostedAgent, error) {
	now := s.o.Now()
	st, err := s.o.Store.AgentState(ctx, row.ID)
	if errors.Is(err, store.ErrNotFound) {
		st, err = nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errStore, err)
	}
	known, err := s.o.Store.KnownSeats(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errStore, err)
	}
	own, school := modelSlots(row.Settings)
	eff, err := s.effective(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errStore, err)
	}
	sc := eff.Runtime.School
	status, problem := statusOf(row, own != nil || school != nil, st)
	v := &HostedAgent{
		ID: row.ID, Version: row.Version, CoreActorID: row.CoreActorID, OwnerActorID: row.OwnerActorID,
		DisplayName: row.DisplayName, Status: status, Problem: problem, Paused: row.Paused,
		Model: ModelSlots{Own: ownModelView(own, s.pricesOf(eff), now), School: schoolModelView(school, sc, own != nil && row.KeySecretID != "")},
		Tools: ToolsView{Writes: registry.WritesOf(row.Settings)},
		Seats: []Seat{}, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
	if row.KeySecretID != "" {
		v.OwnKey = &OwnKey{Hint: row.KeyHint}
		if row.KeyProvider != "" {
			p := row.KeyProvider
			v.OwnKey.Provider = &p
		}
	}
	for _, r := range known {
		if v.SeatsAsOf == nil || r.SeenAt.After(*v.SeatsAsOf) {
			t := r.SeenAt.UTC()
			v.SeatsAsOf = &t
		}
		if r.GoneAt != nil {
			continue
		}
		seat := seatOf(r)
		n, err := store.ProposalsWaiting(ctx, s.o.Store, row.ID, r.MemberID)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errStore, err)
		}
		seat.ProposalsWaiting = n
		v.ProposalsWaiting += n
		v.Seats = append(v.Seats, seat)
	}
	sortSeats(v.Seats)
	since := store.UTCDay(now)
	spend, err := s.o.Store.Spend(ctx, store.SpendScope{AgentID: row.ID}, since)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errStore, err)
	}
	v.Today = Today{Since: since, Answers: spend.Answers, CostUSD: costUSD(spend.CostPUSD)}
	if school != nil {
		use, err := s.schoolUse(ctx, sc, row.TenantID, since)
		if err != nil {
			return nil, err
		}
		v.Today.School = use
	}
	return v, nil
}

// schoolUse is the owner's use of the school's plan sc today, the owner
// being the agent's tenant: every answer on the school's key of any of
// their agents.
func (s *Server) schoolUse(ctx context.Context, sc config.School, tenant string, since time.Time) (*SchoolUse, error) {
	spend, err := s.o.Store.Spend(ctx, store.SpendScope{TenantID: tenant, KeySource: config.KeySchool}, since)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errStore, err)
	}
	owner, asker := sc.OwnerQuota(), sc.AskerQuota()
	use := &SchoolUse{Scope: "owner", Used: spend.Answers, Limit: *owner.Answers, UsedUSD: costUSD(spend.CostPUSD), PerAskerLimit: *asker.Answers}
	if owner.USD != nil {
		limit := costUSD(pricing.PUSD(*owner.USD))
		use.LimitUSD = &limit
	}
	return use, nil
}
