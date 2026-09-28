// Package storetest holds every implementation of store.Store to one
// contract (docs/design.md §8): memstore and pgstore run the same suite, so
// a worker behaves the same on either.
//
// Beyond what store.go says, the suite fixes what both implementations do
// where the interface leaves a choice:
//
//   - Times are kept to the microsecond, as Postgres keeps them, and a zero
//     time given to a write is the store's own now.
//   - A write without the ids it is keyed on is refused, as is an attempt in
//     a state store.go does not name, and a lease whose ttl is not positive.
//     An attempt needs its seat's member_id too: Unsettled finds it by that,
//     and PurgeMember removes it by that, bytes and all.
//   - PutAttempt writes an empty State as sending: a row is written ahead.
//   - FinishAttempt keeps the action id and posted message id already known
//     when the outcome carries none; its error code and reason replace the
//     old ones, since they say why the attempt stands where it now does.
//   - AttemptByAction of several attempts under one action is the oldest.
//   - Notes and RecentAnswerCosts with a limit of zero or less return
//     nothing.
//   - ForgetMessage with no message id removes nothing: notes about no
//     message are not about that one.
//   - PurgeMember removes everything the store holds in the seat: its notes,
//     its attempts (whose bytes hold the answers' bodies) and its cursors.
//     The seat's row goes with ForgetSeat, and the ledger's ids and numbers
//     stay (Core's docs/agent-runtime.md §6.3). PurgeAgent removes all of
//     an agent's but its ledger: every seat's notes, attempts and cursors,
//     its seats, its state and its leases (agent:{id}, conv:{id}:*), and
//     nothing of an agent whose id begins as its does.
//   - An agent's state keeps why (a reason) and the version of the row
//     the worker put in force; AgentState of an agent with none is
//     ErrNotFound.
//   - SeatGone of a seat never seen is ErrNotFound; ForgetSeat of one is
//     nothing.
//   - A ledger row is keyed on (agent, id), and recording one again is
//     nothing, so a retried write never counts twice.
//   - A secret's id is sec_ and up to 60 letters, digits, '_' and '-', and
//     one taken is ErrExists; the bytes come back as they were given. A
//     rewrap names the key it replaces, and is ErrConflict when that key no
//     longer wraps the secret. DeleteSecret of a secret not there is
//     nothing.
//   - A hosted agent is created at version 1 with the secrets it refers to,
//     each of its tenant, its token a core_token and its key a model_key;
//     a secret it does not refer to, or another agent's, is refused, and a
//     refused write keeps nothing. An update names the version it read,
//     and is ErrConflict at any other; its Core actor and tenant never
//     change; a secret it no longer refers to is destroyed with the write,
//     and deleting it takes its courses and its secrets. Its settings come
//     back as the same JSON, not the same bytes: Postgres keeps them as
//     jsonb. The registry's revision moves on with every write to an agent
//     or a course (a delete of nothing may move it too), and never with a
//     read, a person or a secret alone.
//   - A seat keeps what me_memberships last showed of it, its course's
//     status among it; perms of none come back as an empty map.
//   - Reports span [since, until), and group by the UTC day; a row is
//     there only when something was recorded in it. Answers count the
//     billable ones, outcomes every one; calls, tokens and cost are the
//     model calls', as Spend sums them.
//   - Lists come back in a fixed order: attempts by number, or oldest first;
//     seats by member id; seats gone by when they went; agent states by
//     agent id; secrets by id; hosted agents by id; their courses by agent,
//     then course; usage by day, then course; askers by member id; audit
//     events by time, then id. Ids sort bytewise, and rows written at one
//     instant keep the order they were written in.
//   - An audit event's id is the store's, increasing; its detail is a JSON
//     object, {} for none, and comes back as the same JSON.
package storetest

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// Opener gives a test a fresh, empty store, and closes it when the test
// ends.
type Opener func(t *testing.T) store.Store

// Run runs the whole contract against the stores open gives. Its subtests
// do not run in parallel: open may hand each one the same database, emptied.
func Run(t *testing.T, open func(t *testing.T) store.Store) {
	t.Helper()
	for _, g := range []struct {
		name string
		run  func(*testing.T, Opener)
	}{
		{"Leases", testLeases},
		{"Attempts", testAttempts},
		{"Cursors", testCursors},
		{"Memory", testMemory},
		{"Seats", testSeats},
		{"Ledger", testLedger},
		{"Status", testStatus},
		{"Secrets", testSecrets},
		{"Registry", testRegistry},
		{"Reports", testReports},
		{"Audit", testAudit},
	} {
		t.Run(g.name, func(t *testing.T) { g.run(t, open) })
	}
}

// base is a fixed time with nanoseconds, in the past so that a store's now
// is after it. Stores keep microseconds; us says what they keep.
var base = time.Date(2026, time.January, 2, 3, 4, 5, 123456789, time.UTC)

// at is base plus d.
func at(d time.Duration) time.Time { return base.Add(d) }

// us is t as a store keeps it.
func us(t time.Time) time.Time { return t.Truncate(time.Microsecond) }

// clockSlack is how far a store's now may be from the test's: Postgres may
// run elsewhere, on its own clock.
const clockSlack = time.Minute

// recent fails unless got is a store's now, taken between before and after.
func recent(t *testing.T, what string, got, before, after time.Time) {
	t.Helper()
	if got.Before(before.Add(-clockSlack)) || got.After(after.Add(clockSlack)) {
		t.Errorf("%s = %s, want the store's now (between %s and %s)", what, got, before, after)
	}
}

// sameTime fails unless got is want as a store keeps it.
func sameTime(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if !got.Equal(us(want)) {
		t.Errorf("%s = %s, want %s", what, got, us(want))
	}
}

// sameAttempt compares every field of two attempts, times to the microsecond
// and bytes by value.
func sameAttempt(t *testing.T, got, want store.Attempt) {
	t.Helper()
	if !bytes.Equal(got.Args, want.Args) {
		t.Errorf("attempt %s: args = %q, want %q", want.Key, got.Args, want.Args)
	}
	sameTime(t, "attempt "+want.Key+" created_at", got.CreatedAt, want.CreatedAt)
	sameTime(t, "attempt "+want.Key+" updated_at", got.UpdatedAt, want.UpdatedAt)
	got.Args, want.Args = nil, nil
	got.CreatedAt, want.CreatedAt = time.Time{}, time.Time{}
	got.UpdatedAt, want.UpdatedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("attempt %s:\n got %+v\nwant %+v", want.Key, got, want)
	}
}

// keys lists the attempts' keys, in order.
func keys(as []store.Attempt) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Key
	}
	return out
}
