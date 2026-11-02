package schedule

import (
	"math/rand"
	"testing"
	"time"
)

func mustDefault(t *testing.T, tz string) Schedule {
	t.Helper()
	s, err := Default(tz)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func in(t *testing.T, s Schedule, layout, v string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation(layout, v, s.Location)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

const L = "2006-01-02 15:04"

func TestNextSlot(t *testing.T) {
	s := mustDefault(t, "Asia/Kolkata")
	cases := []struct{ name, now, want string }{
		{"inside window unchanged", "2026-11-04 11:00", "2026-11-04 11:00"},
		{"window open edge", "2026-11-04 09:30", "2026-11-04 09:30"},
		{"before open same day", "2026-11-04 07:00", "2026-11-04 09:30"},
		{"close edge is closed", "2026-11-04 17:30", "2026-11-05 09:30"},
		{"after close", "2026-11-04 18:00", "2026-11-05 09:30"},
		{"friday evening -> monday", "2026-11-06 18:00", "2026-11-09 09:30"},
		{"saturday -> monday", "2026-11-07 12:00", "2026-11-09 09:30"},
		{"sunday -> monday", "2026-11-08 10:00", "2026-11-09 09:30"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := s.NextSlot(in(t, s, L, c.now))
			if want := in(t, s, L, c.want); !got.Equal(want) {
				t.Fatalf("got %v want %v", got, want)
			}
		})
	}
}

func TestNextSlotHolidays(t *testing.T) {
	s := mustDefault(t, "Asia/Kolkata")
	s.Holidays["2026-11-09"] = true // Monday
	got := s.NextSlot(in(t, s, L, "2026-11-06 18:00"))
	if want := in(t, s, L, "2026-11-10 09:30"); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNextSlotOtherTimezone(t *testing.T) {
	s := mustDefault(t, "America/New_York")
	// 2026-11-06 23:00 UTC is Fri 18:00 EST: closed, so Monday 09:30 EST.
	now := time.Date(2026, 11, 6, 23, 0, 0, 0, time.UTC)
	want := time.Date(2026, 11, 9, 9, 30, 0, 0, s.Location)
	if got := s.NextSlot(now); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestIsOpen(t *testing.T) {
	s := mustDefault(t, "Asia/Kolkata")
	if !s.IsOpen(in(t, s, L, "2026-11-04 10:00")) {
		t.Fatal("expected open")
	}
	if s.IsOpen(in(t, s, L, "2026-11-07 10:00")) {
		t.Fatal("saturday should be closed")
	}
	if s.IsOpen(in(t, s, L, "2026-11-04 17:30")) {
		t.Fatal("close edge should be closed")
	}
}

func TestAddBusinessDays(t *testing.T) {
	s := mustDefault(t, "Asia/Kolkata")
	cases := []struct {
		name, from string
		n          int
		want       string
	}{
		{"friday +2 -> tuesday", "2026-11-06 11:00", 2, "2026-11-10 11:00"},
		{"tuesday +2 -> thursday", "2026-11-03 11:00", 2, "2026-11-05 11:00"},
		{"zero days stays", "2026-11-03 11:00", 0, "2026-11-03 11:00"},
		{"zero days after hours snaps", "2026-11-03 20:00", 0, "2026-11-04 09:30"},
		{"time of day outside window snaps", "2026-11-03 16:00", 1, "2026-11-04 16:00"},
		{"saturday +1 -> monday open", "2026-11-07 12:00", 1, "2026-11-09 12:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := s.AddBusinessDays(in(t, s, L, c.from), c.n)
			if want := in(t, s, L, c.want); !got.Equal(want) {
				t.Fatalf("got %v want %v", got, want)
			}
		})
	}
}

func TestAddBusinessDaysSkipsHoliday(t *testing.T) {
	s := mustDefault(t, "Asia/Kolkata")
	s.Holidays["2026-11-10"] = true // Tuesday
	got := s.AddBusinessDays(in(t, s, L, "2026-11-06 11:00"), 2)
	if want := in(t, s, L, "2026-11-11 11:00"); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestDST(t *testing.T) {
	s := mustDefault(t, "America/New_York")
	// US spring-forward is Sunday 2026-03-08. Friday 18:00 EST -> Monday
	// 09:30 EDT (UTC-4), i.e. 13:30 UTC, not 14:30.
	fri := time.Date(2026, 3, 6, 18, 0, 0, 0, s.Location)
	got := s.NextSlot(fri)
	if want := time.Date(2026, 3, 9, 13, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("got %v want %v", got.UTC(), want)
	}
	// +1 business day across the change keeps wall-clock time.
	got = s.AddBusinessDays(time.Date(2026, 3, 6, 11, 0, 0, 0, s.Location), 1)
	if h, m, _ := got.In(s.Location).Clock(); h != 11 || m != 0 || got.Weekday() != time.Monday {
		t.Fatalf("got %v", got)
	}
}

func TestWithJitterRollsOver(t *testing.T) {
	s := mustDefault(t, "Asia/Kolkata")
	r := rand.New(rand.NewSource(1))
	got := s.WithJitter(in(t, s, L, "2026-11-04 17:29"), 3*time.Minute, 3*time.Minute, r)
	if want := in(t, s, L, "2026-11-05 09:30"); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := 0; i < 100; i++ {
		got := s.WithJitter(in(t, s, L, "2026-11-04 10:00"), 2*time.Minute, 6*time.Minute, r)
		d := got.Sub(in(t, s, L, "2026-11-04 10:00"))
		if d < 2*time.Minute || d > 6*time.Minute {
			t.Fatalf("jitter %v out of range", d)
		}
	}
}

func TestValidate(t *testing.T) {
	s := mustDefault(t, "UTC")
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := s
	bad.StartMinute, bad.EndMinute = 600, 600
	if bad.Validate() == nil {
		t.Fatal("expected invalid window")
	}
	bad = s
	bad.WorkDays = map[time.Weekday]bool{}
	if bad.Validate() == nil {
		t.Fatal("expected no working days error")
	}
}
