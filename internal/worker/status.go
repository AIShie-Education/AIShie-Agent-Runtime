package worker

import (
	"slices"
	"strings"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// AgentStatus is what the supervisor knows of one configured agent, for
// /status: ids, states and numbers, never what anyone wrote.
type AgentStatus struct {
	AgentID string `json:"agent_id"`
	// State is the agent's state as this worker last set it (running,
	// paused, unauthorized, …); "" when another worker runs it.
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
	Paused bool   `json:"paused,omitempty"`
	// Hosted is set for an agent of the registry, rather than of YAML.
	Hosted bool `json:"hosted,omitempty"`
	// Leased is whether this worker holds the agent's lease; Running,
	// whether it runs the agent now.
	Leased  bool `json:"leased"`
	Running bool `json:"running"`
	// CatalogueHash is the hash of the Core catalogue the agent runs with.
	CatalogueHash string `json:"catalogue_hash,omitempty"`
	// SlowUntil is when polling stops being halved after a 429.
	SlowUntil *time.Time `json:"slow_until,omitempty"`
	// Answering is how many answers are in progress.
	Answering int          `json:"answering"`
	Seats     []SeatStatus `json:"seats,omitempty"`
}

// SeatStatus is one seat an agent answers in.
type SeatStatus struct {
	MemberID   string `json:"member_id"`
	CourseID   string `json:"course_id"`
	CourseCode string `json:"course_code"`
	// AnswersCourse is true for a course tutor.
	AnswersCourse bool `json:"answers_course"`
	// Answering is whether the seat answers now: it runs, and is not held.
	Answering bool   `json:"answering"`
	Level     string `json:"level"`
	// Held is whether its inbox is not polled, and HeldWhy why.
	Held    bool   `json:"held"`
	HeldWhy string `json:"held_why,omitempty"`
	// HeldBack is how many conversations are held back now.
	HeldBack   int        `json:"held_back"`
	Hot        bool       `json:"hot"`
	Tools      []string   `json:"tools,omitempty"`
	LastPoll   *time.Time `json:"last_poll,omitempty"`
	LastEvents *time.Time `json:"last_events,omitempty"`
}

// Status is every configured agent as this worker knows it, by id.
func (s *Supervisor) Status() []AgentStatus {
	s.mu.Lock()
	out := make([]AgentStatus, 0, len(s.runners)+len(s.paused))
	agents := map[int]*Agent{}
	hosted := map[string]bool{}
	if s.cfg != nil {
		for _, a := range s.cfg.Agents {
			hosted[a.ID] = a.Hosted != nil
		}
	}
	for _, r := range s.runners {
		st := AgentStatus{AgentID: r.id, Leased: r.holds, Running: r.agent != nil, Hosted: r.cfg.Hosted != nil}
		if r.holds {
			st.State, st.Detail = r.state, r.detail
		}
		if r.agent != nil {
			agents[len(out)] = r.agent
		}
		out = append(out, st)
	}
	for id := range s.paused {
		out = append(out, AgentStatus{AgentID: id, State: store.AgentPaused, Paused: true, Hosted: hosted[id]})
	}
	for id, detail := range s.rejected {
		out = append(out, AgentStatus{AgentID: id, State: store.AgentError, Detail: "not run: " + detail, Hosted: true})
	}
	s.mu.Unlock()
	for i, a := range agents {
		a.status(&out[i])
	}
	slices.SortFunc(out, func(x, y AgentStatus) int { return strings.Compare(x.AgentID, y.AgentID) })
	return out
}

// status fills in what the agent knows of itself.
func (a *Agent) status(st *AgentStatus) {
	a.mu.Lock()
	sched, cat := a.sched, a.cat
	if sched == nil {
		a.mu.Unlock()
		return
	}
	st.CatalogueHash = cat.Hash()
	if a.now().Before(a.slowUntil) {
		t := a.slowUntil
		st.SlowUntil = &t
	}
	seats := make([]*Seat, 0, len(a.seats))
	for _, s := range a.seats {
		seats = append(seats, s)
	}
	a.mu.Unlock()
	st.Answering = sched.busy()
	for _, s := range seats {
		st.Seats = append(st.Seats, s.status())
	}
	slices.SortFunc(st.Seats, func(x, y SeatStatus) int { return strings.Compare(x.MemberID, y.MemberID) })
}

// status is the seat as it stands.
func (s *Seat) status() SeatStatus {
	tools := s.toolNames()
	now := s.a.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SeatStatus{
		MemberID: s.id, CourseID: s.course, CourseCode: s.m.Code, AnswersCourse: s.m.AnswersCourse,
		Answering: s.hold == nil, Level: s.m.Level("conversation_answer"), Held: s.hold != nil, Hot: now.Before(s.hotUntil),
		Tools: tools,
	}
	if s.hold != nil {
		st.HeldWhy = s.hold.why
	}
	for _, h := range s.heldBack {
		if now.Before(h.until) {
			st.HeldBack++
		}
	}
	if !s.lastPoll.IsZero() {
		t := s.lastPoll
		st.LastPoll = &t
	}
	if !s.lastEvents.IsZero() {
		t := s.lastEvents
		st.LastEvents = &t
	}
	return st
}
