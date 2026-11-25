// Package schoolday maps an instant to the school's calendar day. A nil
// location means UTC, as it does for a Core that names no school time zone.
package schoolday

import "time"

// Location returns loc, or UTC when Core did not name one.
func Location(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}

// Day is the instant of midnight at the start of t's day in loc. AddDate,
// rather than 24 hours, reaches the next one across a daylight-saving change.
func Day(t time.Time, loc *time.Location) time.Time {
	loc = Location(loc)
	local := t.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

// CivilDate is t's calendar date in loc, represented at midnight UTC. Price
// rows and report span arithmetic use civil dates, not elapsed 24-hour spans:
// their storage and text form is YYYY-MM-DD, while Day is the actual instant
// at which such a date begins.
func CivilDate(t time.Time, loc *time.Location) time.Time {
	local := t.In(Location(loc))
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

// Days is the number of calendar dates from since until until (exclusive),
// positive when until is the later date. It holds across 23- and 25-hour days.
func Days(since, until time.Time, loc *time.Location) int {
	return int(CivilDate(until, loc).Sub(CivilDate(since, loc)).Hours() / 24)
}
