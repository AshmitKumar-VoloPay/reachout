// Package schedule is the single place that encodes weekday / working-hours
// rules. The worker, REST API and MCP server all go through it.
package schedule

import (
	"fmt"
	"math/rand"
	"time"
)

// Schedule describes when an account may send outbound actions.
type Schedule struct {
	Location    *time.Location
	WorkDays    map[time.Weekday]bool
	StartMinute int             // minutes after local midnight, window opens (inclusive)
	EndMinute   int             // minutes after local midnight, window closes (exclusive)
	Holidays    map[string]bool // local dates, "2006-01-02"
}

// Default returns Mon-Fri 09:30-17:30 in the given timezone.
func Default(tz string) (Schedule, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return Schedule{}, fmt.Errorf("load timezone %q: %w", tz, err)
	}
	return Schedule{
		Location: loc,
		WorkDays: map[time.Weekday]bool{
			time.Monday: true, time.Tuesday: true, time.Wednesday: true,
			time.Thursday: true, time.Friday: true,
		},
		StartMinute: 9*60 + 30,
		EndMinute:   17*60 + 30,
		Holidays:    map[string]bool{},
	}, nil
}

// Validate reports whether the schedule can ever produce a slot.
func (s Schedule) Validate() error {
	if s.Location == nil {
		return fmt.Errorf("schedule: missing location")
	}
	if s.StartMinute < 0 || s.EndMinute > 24*60 || s.StartMinute >= s.EndMinute {
		return fmt.Errorf("schedule: invalid window %d-%d", s.StartMinute, s.EndMinute)
	}
	for _, on := range s.WorkDays {
		if on {
			return nil
		}
	}
	return fmt.Errorf("schedule: no working days")
}

func dateKey(t time.Time) string { return t.Format("2006-01-02") }

func (s Schedule) isWorkDate(t time.Time) bool {
	return s.WorkDays[t.Weekday()] && !s.Holidays[dateKey(t)]
}

// at returns the local time on t's date at the given minute of day.
// time.Date resolves DST gaps/overlaps, so this stays correct across changes.
func (s Schedule) at(t time.Time, minute int) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, minute/60, minute%60, 0, 0, s.Location)
}

// IsOpen reports whether t falls inside a working window.
func (s Schedule) IsOpen(t time.Time) bool {
	l := t.In(s.Location)
	if !s.isWorkDate(l) {
		return false
	}
	return !l.Before(s.at(l, s.StartMinute)) && l.Before(s.at(l, s.EndMinute))
}

// NextSlot returns the earliest time >= now that is inside a working window.
// If now is already inside the window it is returned unchanged.
func (s Schedule) NextSlot(now time.Time) time.Time {
	l := now.In(s.Location)
	if s.isWorkDate(l) {
		start, end := s.at(l, s.StartMinute), s.at(l, s.EndMinute)
		switch {
		case l.Before(start):
			return start
		case l.Before(end):
			return l
		}
	}
	// Move to following days until a work date is found. Bounded so a
	// pathological schedule cannot loop forever.
	for i := 1; i <= 366*2; i++ {
		y, m, d := l.Date()
		next := time.Date(y, m, d+i, 0, 0, 0, 0, s.Location)
		if s.isWorkDate(next) {
			return s.at(next, s.StartMinute)
		}
	}
	return time.Time{} // unreachable once Validate passes (aside from all-year holidays)
}

// AddBusinessDays moves t forward by n working days, keeping the local time of
// day, then snaps to the next open slot. "2 days after Friday" is Tuesday.
func (s Schedule) AddBusinessDays(t time.Time, n int) time.Time {
	l := t.In(s.Location)
	h, mi, sec := l.Clock()
	for added, i := 0, 0; added < n && i < 366*2; i++ {
		y, m, d := l.Date()
		l = time.Date(y, m, d+1, h, mi, sec, 0, s.Location)
		if s.isWorkDate(l) {
			added++
		}
	}
	return s.NextSlot(l)
}

// WithJitter adds a random delay in [min, max] to t and re-snaps into the
// window, so jitter near closing time rolls over to the next working day.
func (s Schedule) WithJitter(t time.Time, min, max time.Duration, r *rand.Rand) time.Time {
	d := min
	if max > min {
		d += time.Duration(r.Int63n(int64(max - min)))
	}
	return s.NextSlot(t.Add(d))
}
