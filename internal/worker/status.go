package worker

import "time"

// AgentStatus is one configured agent in the management status endpoint.
type AgentStatus struct {
	AgentID       string
	Running       bool
	Paused        bool
	Hosted        bool
	CatalogueHash string
	SlowUntil     *time.Time
	Answering     int
	Seats         []SeatStatus
}

// SeatStatus is retained as a compact management DTO for the chat wiring.
// The legacy supervisor returns no seats.
type SeatStatus struct {
	MemberID   string
	CourseID   string
	CourseCode string
	Level      string
	HeldWhy    string
	Tools      []string
}
