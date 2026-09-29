package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
)

// SeatGuard is what keeps a model's member writes off the seats it must
// never change, whoever asks and whatever Core would allow (design §4): the
// agent's own seat, which Core refuses too, and the seats of the people it
// acts for: its principal's, its owner's own seat in the course, and the
// conversation's opener's, which in a conversation offered writes is the
// principal's for a delegate, and for an agent nobody owns whoever opened
// it; and the seats of those people's other agents, their delegates, which
// only their owner brings in and answers for. Run checks every member_*
// write against it before anything reaches Core. A write that names a seat
// (member_id, or any of member_ids) names none of the first three, and
// none the runtime reads with member_get to be another agent of the people
// it acts for, or cannot read; a change to every seat of a roster role
// (member_update_perms_bulk) is made only when none of the people's seats
// has that role, which the runtime reads with member_get (Core leaves the
// caller's own seat out of it), and no seat of their other agents has it,
// which it reads with member_list. Anything else is refused as an is_error
// result that reaches nobody, and counted (Writes.Guarded). A member write
// run without the agent's seat is refused too. Core holds a delegate to
// the same since 169cf50 (not_your_principal); the runtime holds every
// agent to it again, before Core.
type SeatGuard struct {
	// Self is the agent's seat; Principal its principal's, "" for a seat
	// that is nobody's delegate; Opener the conversation's opener's.
	Self, Principal, Opener string
}

// guarded reports whether a write of tool is one SeatGuard checks.
func guarded(tool string) bool { return strings.HasPrefix(tool, "member_") }

// roleWide is the write that changes every seat of a roster role.
const roleWide = "member_update_perms_bulk"

// memberTargets are what of a member write's arguments names seats, and the
// role a role-wide change selects them by.
type memberTargets struct {
	MemberID  string   `json:"member_id"`
	MemberIDs []string `json:"member_ids"`
	Role      *string  `json:"role"`
}

// people are the seats of the people the agent acts for, once each, but
// its own: its principal's, then the opener's.
func (g SeatGuard) people() []string {
	var out []string
	for _, id := range []string{g.Principal, g.Opener} {
		if id == "" || sameID(id, g.Self) || slices.ContainsFunc(out, func(o string) bool { return sameID(o, id) }) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// sameID compares two ids as Core reads them: a UUID in any case.
func sameID(a, b string) bool { return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) }

// check is the guard's word on a member write of tool with args, the
// arguments as they will reach Core, in courseID: "" to let it through,
// or why it is refused. Its error is fatal to the answer (a 401 or ctx
// done while the runtime read a seat's role), as Run's is.
func (g SeatGuard) check(ctx context.Context, c *core.Client, courseID, tool string, args json.RawMessage) (string, error) {
	if g.Self == "" {
		return tool + " is not offered to the model here: the runtime does not know which seats it must leave alone", nil
	}
	var t memberTargets
	if err := json.Unmarshal(args, &t); err != nil {
		return "the runtime could not read which seats " + tool + " changes; nothing was changed", nil
	}
	named := append([]string{t.MemberID}, t.MemberIDs...)
	for _, id := range named {
		switch {
		case id == "":
		case sameID(id, g.Self):
			return fmt.Sprintf("%s would change your own seat (%s): you never change your own seat, and nothing was changed. "+
				"Say so, and that someone who manages the course's members can", tool, cut(id, 64)), nil
		case sameID(id, g.Principal) || sameID(id, g.Opener):
			return fmt.Sprintf("%s would change the seat of the person you act for (%s): you never change that seat, whoever asks, "+
				"and nothing was changed. Say so, and that someone else who manages the course's members can", tool, cut(id, 64)), nil
		}
	}
	people := g.people()
	for _, id := range named {
		if id == "" || len(people) == 0 {
			continue
		}
		why, err := g.otherAgent(ctx, c, courseID, tool, id, people)
		if err != nil || why != "" {
			return why, err
		}
	}
	if tool != roleWide || t.Role == nil {
		return "", nil
	}
	for _, id := range people {
		role, err := seatRole(ctx, c, courseID, id)
		if err != nil {
			switch {
			case errors.Is(err, core.ErrUnauthenticated):
				return "", err
			case ctx.Err() != nil:
				return "", ctx.Err()
			}
			return fmt.Sprintf("%s changes every seat with the role %s, and the runtime could not read whether the seat of the person "+
				"you act for is one of them, so nothing was changed. Change the seats you mean one at a time, with member_update_perms",
				tool, cut(*t.Role, 64)), nil
		}
		if strings.EqualFold(role, *t.Role) {
			return fmt.Sprintf("%s changes every seat with the role %s, and the seat of the person you act for (%s) has that role: you "+
				"never change that seat, so nothing was changed. Change the other seats one at a time, with member_update_perms",
				tool, cut(*t.Role, 64), id), nil
		}
	}
	if len(people) == 0 {
		return "", nil
	}
	agentSeat, err := agentOfRole(ctx, c, courseID, *t.Role, g.Self, people)
	switch {
	case errors.Is(err, core.ErrUnauthenticated):
		return "", err
	case ctx.Err() != nil:
		return "", ctx.Err()
	case err != nil:
		return fmt.Sprintf("%s changes every seat with the role %s, and the runtime could not read whether a seat of another agent of "+
			"the person you act for is one of them, so nothing was changed. Change the seats you mean one at a time, with member_update_perms",
			tool, cut(*t.Role, 64)), nil
	case agentSeat != "":
		return fmt.Sprintf("%s changes every seat with the role %s, and the seat of another agent of the person you act for (%s) has "+
			"that role: you never change their agents' seats, so nothing was changed. Change the other seats one at a time, with "+
			"member_update_perms", tool, cut(*t.Role, 64), agentSeat), nil
	}
	return "", nil
}

// otherAgent is the guard's word on a member write of tool that names the
// seat id: "" when id is no agent of people's, or no seat at all (Core then
// says so), and why it is refused when it is one, or when the runtime
// cannot read whose it is. Its error is fatal to the answer, as check's.
func (g SeatGuard) otherAgent(ctx context.Context, c *core.Client, courseID, tool, id string, people []string) (string, error) {
	seat, err := readSeat(ctx, c, courseID, id)
	switch {
	case errors.Is(err, core.ErrUnauthenticated):
		return "", err
	case ctx.Err() != nil:
		return "", ctx.Err()
	case errors.Is(err, errNoSeat):
		return "", nil
	case err != nil:
		return fmt.Sprintf("%s would change the seat %s, and the runtime could not read whose seat it is, so nothing was changed: it "+
			"reads a seat with member_get before changing it, to leave alone the agents of the person you act for. Say so", tool, cut(id, 64)), nil
	}
	if seat.PrincipalMemberID != "" && slices.ContainsFunc(people, func(p string) bool { return sameID(p, seat.PrincipalMemberID) }) {
		return fmt.Sprintf("%s would change the seat of another agent of the person you act for (%s): you never change their "+
			"agents' seats, whoever asks, and nothing was changed. Say so, and that its owner can", tool, cut(id, 64)), nil
	}
	return "", nil
}

// seatView is what the guard reads of a seat: its role, and whose
// delegate it is.
type seatView struct {
	ID                string `json:"id"`
	Role              string `json:"role"`
	PrincipalMemberID string `json:"principal_member_id"`
}

// errNoSeat is a seat Core says is not in the course.
var errNoSeat = errors.New("toolset: no such seat in the course")

// readSeat is the seat id as member_get shows it to the agent.
func readSeat(ctx context.Context, c *core.Client, courseID, id string) (seatView, error) {
	args, err := json.Marshal(map[string]string{"course_id": courseID, "member_id": id})
	if err != nil {
		return seatView{}, err
	}
	env, err := c.Call(core.WithPriority(ctx, core.PriorityAnswer), "member_get", args)
	if err != nil {
		return seatView{}, err
	}
	switch {
	case env == nil:
		return seatView{}, errNoRole
	case env.Status == core.StatusError && env.Code() == core.CodeNotFound:
		return seatView{}, errNoSeat
	case env.Status != core.StatusExecuted:
		return seatView{}, errNoRole
	}
	var seat seatView
	if json.Unmarshal(env.Result, &seat) != nil || seat.Role == "" {
		return seatView{}, errNoRole
	}
	return seat, nil
}

// maxRosterPages bounds the pages of member_list the guard reads of one
// role: 2,000 seats, far past a course's agents.
const maxRosterPages = 10

// agentOfRole is the first seat with the roster role role that is another
// agent of one of people, "" when there is none, as member_list shows the
// course to the agent (self, its own seat, left out, as Core leaves the
// caller's seat out of a role-wide change). A roster it cannot read to
// the end is an error.
func agentOfRole(ctx context.Context, c *core.Client, courseID, role, self string, people []string) (string, error) {
	after := ""
	for range maxRosterPages {
		in := map[string]any{"course_id": courseID, "role": role, "limit": 200}
		if after != "" {
			in["after"] = after
		}
		args, err := json.Marshal(in)
		if err != nil {
			return "", err
		}
		env, err := c.Call(core.WithPriority(ctx, core.PriorityAnswer), "member_list", args)
		if err != nil {
			return "", err
		}
		if env == nil || env.Status != core.StatusExecuted {
			return "", errNoRole
		}
		var page struct {
			Members []seatView `json:"members"`
			Next    *string    `json:"next"`
		}
		if json.Unmarshal(env.Result, &page) != nil {
			return "", errNoRole
		}
		for _, m := range page.Members {
			if !sameID(m.ID, self) && m.PrincipalMemberID != "" &&
				slices.ContainsFunc(people, func(p string) bool { return sameID(p, m.PrincipalMemberID) }) {
				return m.ID, nil
			}
		}
		if page.Next == nil || *page.Next == "" {
			return "", nil
		}
		after = *page.Next
	}
	return "", errors.New("toolset: the course has more seats of the role than the runtime reads")
}

// errNoRole is a seat Core did not show the runtime.
var errNoRole = errors.New("toolset: Core did not show the seat's role")

// seatRole is the roster role of the seat id, as member_get shows it to
// the agent.
func seatRole(ctx context.Context, c *core.Client, courseID, id string) (string, error) {
	seat, err := readSeat(ctx, c, courseID, id)
	if errors.Is(err, errNoSeat) {
		return "", errNoRole
	}
	return seat.Role, err
}
