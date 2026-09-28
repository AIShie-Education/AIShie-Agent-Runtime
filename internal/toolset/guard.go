package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
)

// SeatGuard is what keeps a model's member writes off the seats it must
// never change, whoever asks and whatever Core would allow (design §4): the
// agent's own seat, which Core refuses too, and the seats of the people it
// acts for: its principal's, its owner's own seat in the course, and the
// conversation's opener's, which in a conversation offered writes is the
// principal's for a delegate, and for an agent nobody owns whoever opened
// it. Run checks every member_* write against it before anything reaches
// Core. A write that names a seat (member_id, or any of member_ids) names
// none of these; a change to every seat of a roster role
// (member_update_perms_bulk) is made only when none of the people's seats
// has that role, which the runtime reads with member_get (Core leaves the
// caller's own seat out of it). Anything else is refused as an is_error
// result that reaches nobody, and counted (Writes.Guarded). A member write
// run without the agent's seat is refused too.
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
	for _, id := range append([]string{t.MemberID}, t.MemberIDs...) {
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
	if tool != roleWide || t.Role == nil {
		return "", nil
	}
	for _, id := range g.people() {
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
	return "", nil
}

// errNoRole is a seat Core did not show the runtime.
var errNoRole = errors.New("toolset: Core did not show the seat's role")

// seatRole is the roster role of the seat id, as member_get shows it to
// the agent.
func seatRole(ctx context.Context, c *core.Client, courseID, id string) (string, error) {
	args, err := json.Marshal(map[string]string{"course_id": courseID, "member_id": id})
	if err != nil {
		return "", err
	}
	env, err := c.Call(core.WithPriority(ctx, core.PriorityAnswer), "member_get", args)
	if err != nil {
		return "", err
	}
	if env == nil || env.Status != core.StatusExecuted {
		return "", errNoRole
	}
	var seat struct {
		Role string `json:"role"`
	}
	if json.Unmarshal(env.Result, &seat) != nil || seat.Role == "" {
		return "", errNoRole
	}
	return seat.Role, nil
}
