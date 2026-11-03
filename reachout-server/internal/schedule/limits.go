package schedule

import (
	"math"
	"time"
)

// Kind groups LinkedIn actions that share a daily cap.
type Kind int

const (
	KindConnect Kind = iota // connection requests
	KindMessage             // messages and follow-ups
)

// NoLimit is returned by RampLimit once an account is past its warm-up period.
const NoLimit = math.MaxInt32

// rampSteps are the per-day maximums for weeks 1, 2 and 3 after an account is first
// connected. From week 4 only the configured cap applies. New or newly automated accounts
// that suddenly send a lot are the ones LinkedIn restricts.
var rampSteps = map[Kind][3]int{
	KindConnect: {5, 10, 20},
	KindMessage: {10, 20, 40},
}

// RampLimit is the warm-up ceiling for k on the day containing now. rampStart is the date
// the account started (only its calendar date is used); the day is counted in loc.
func RampLimit(k Kind, rampStart, now time.Time, loc *time.Location) int {
	y, m, d := now.In(loc).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	ry, rm, rd := rampStart.Date()
	start := time.Date(ry, rm, rd, 0, 0, 0, 0, time.UTC)

	days := int(today.Sub(start).Hours() / 24)
	if days < 0 {
		days = 0
	}
	week := days / 7
	if week >= len(rampSteps[k]) {
		return NoLimit
	}
	return rampSteps[k][week]
}

// EffectiveCap is the number of actions allowed per day: the configured cap, lowered by
// the warm-up ceiling while the account is new.
func EffectiveCap(k Kind, configured int, rampStart, now time.Time, loc *time.Location) int {
	if r := RampLimit(k, rampStart, now, loc); r < configured {
		return r
	}
	return configured
}

// DayBounds returns the start of the local day containing t and the start of the next one.
// On a daylight-saving change the day is 23 or 25 hours long.
func (s Schedule) DayBounds(t time.Time) (start, end time.Time) {
	y, m, d := t.In(s.Location).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, s.Location), time.Date(y, m, d+1, 0, 0, 0, 0, s.Location)
}

// OpensAt returns when the working window opens on the day containing t. ok is false on a
// day that is not a working day (weekend or holiday).
func (s Schedule) OpensAt(t time.Time) (opens time.Time, ok bool) {
	l := t.In(s.Location)
	if !s.isWorkDate(l) {
		return time.Time{}, false
	}
	return s.at(l, s.StartMinute), true
}
