package schedule

import (
	"testing"
	"time"
)

func TestRampLimit(t *testing.T) {
	loc := time.UTC
	start := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC) // a Monday
	cases := []struct {
		name string
		now  time.Time
		k    Kind
		want int
	}{
		{"day 0", time.Date(2026, 11, 2, 9, 0, 0, 0, loc), KindConnect, 5},
		{"day 6", time.Date(2026, 11, 8, 23, 59, 0, 0, loc), KindConnect, 5},
		{"day 7", time.Date(2026, 11, 9, 0, 0, 0, 0, loc), KindConnect, 10},
		{"day 13", time.Date(2026, 11, 15, 12, 0, 0, 0, loc), KindConnect, 10},
		{"day 14", time.Date(2026, 11, 16, 0, 0, 0, 0, loc), KindConnect, 20},
		{"day 20", time.Date(2026, 11, 22, 12, 0, 0, 0, loc), KindConnect, 20},
		{"day 21", time.Date(2026, 11, 23, 0, 0, 0, 0, loc), KindConnect, NoLimit},
		{"a year later", time.Date(2027, 11, 2, 0, 0, 0, 0, loc), KindConnect, NoLimit},
		{"messages week 1", time.Date(2026, 11, 3, 0, 0, 0, 0, loc), KindMessage, 10},
		{"messages week 3", time.Date(2026, 11, 17, 0, 0, 0, 0, loc), KindMessage, 40},
		{"messages after ramp", time.Date(2026, 11, 30, 0, 0, 0, 0, loc), KindMessage, NoLimit},
		{"before the start date counts as day 0", time.Date(2026, 10, 1, 0, 0, 0, 0, loc), KindConnect, 5},
	}
	for _, c := range cases {
		if got := RampLimit(c.k, start, c.now, loc); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func TestRampDayIsCountedInTheAccountsTimezone(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	start := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	// 2026-11-08 20:00 UTC is already 2026-11-09 01:30 in Kolkata: day 7, the second week.
	now := time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC)
	if got := RampLimit(KindConnect, start, now, ist); got != 10 {
		t.Fatalf("got %d, want 10", got)
	}
	if got := RampLimit(KindConnect, start, now, time.UTC); got != 5 {
		t.Fatalf("UTC got %d, want 5", got)
	}
}

func TestEffectiveCap(t *testing.T) {
	start := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	day0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	old := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	if got := EffectiveCap(KindConnect, 30, start, day0, time.UTC); got != 5 {
		t.Errorf("new account with a high cap: %d, want 5", got)
	}
	if got := EffectiveCap(KindConnect, 3, start, day0, time.UTC); got != 3 {
		t.Errorf("configured below ramp: %d, want 3", got)
	}
	if got := EffectiveCap(KindConnect, 30, start, old, time.UTC); got != 30 {
		t.Errorf("old account: %d, want 30", got)
	}
	if got := EffectiveCap(KindConnect, 0, start, old, time.UTC); got != 0 {
		t.Errorf("cap 0 must stay 0: %d", got)
	}
}

func TestDayBoundsAndOpensAt(t *testing.T) {
	s, _ := Default("America/New_York")
	// Normal day.
	st, en := s.DayBounds(time.Date(2026, 11, 3, 15, 0, 0, 0, s.Location))
	if en.Sub(st) != 24*time.Hour || st.Hour() != 0 {
		t.Fatalf("normal day: %v - %v", st, en)
	}
	// Spring forward (2026-03-08) is 23 hours; fall back (2026-11-01) is 25.
	st, en = s.DayBounds(time.Date(2026, 3, 8, 12, 0, 0, 0, s.Location))
	if en.Sub(st) != 23*time.Hour {
		t.Fatalf("spring-forward day is %v long", en.Sub(st))
	}
	st, en = s.DayBounds(time.Date(2026, 11, 1, 12, 0, 0, 0, s.Location))
	if en.Sub(st) != 25*time.Hour {
		t.Fatalf("fall-back day is %v long", en.Sub(st))
	}

	d, _ := Default("Asia/Kolkata")
	opens, ok := d.OpensAt(time.Date(2026, 11, 4, 3, 0, 0, 0, d.Location)) // Wednesday, before opening
	if !ok || opens.Hour() != 9 || opens.Minute() != 30 || opens.Day() != 4 {
		t.Fatalf("weekday: %v %v", opens, ok)
	}
	if _, ok := d.OpensAt(time.Date(2026, 11, 7, 12, 0, 0, 0, d.Location)); ok {
		t.Fatal("Saturday has no window")
	}
	d.Holidays["2026-11-04"] = true
	if _, ok := d.OpensAt(time.Date(2026, 11, 4, 12, 0, 0, 0, d.Location)); ok {
		t.Fatal("a holiday has no window")
	}
}
