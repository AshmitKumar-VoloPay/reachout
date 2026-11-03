package schedule

import (
	"fmt"
	"time"
)

// New builds a validated Schedule from stored values: an IANA timezone, weekdays
// (0=Sunday .. 6=Saturday), window bounds in minutes after local midnight, and
// holidays as "2006-01-02" local dates.
func New(tz string, workDays []int, startMinute, endMinute int, holidays []string) (Schedule, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "" {
		return Schedule{}, fmt.Errorf("unknown timezone %q", tz)
	}
	s := Schedule{
		Location:    loc,
		WorkDays:    map[time.Weekday]bool{},
		StartMinute: startMinute,
		EndMinute:   endMinute,
		Holidays:    map[string]bool{},
	}
	for _, d := range workDays {
		if d < 0 || d > 6 {
			return Schedule{}, fmt.Errorf("invalid weekday %d", d)
		}
		s.WorkDays[time.Weekday(d)] = true
	}
	for _, h := range holidays {
		t, err := time.Parse("2006-01-02", h)
		if err != nil {
			return Schedule{}, fmt.Errorf("invalid holiday %q, want YYYY-MM-DD", h)
		}
		s.Holidays[t.Format("2006-01-02")] = true
	}
	if err := s.Validate(); err != nil {
		return Schedule{}, err
	}
	return s, nil
}
