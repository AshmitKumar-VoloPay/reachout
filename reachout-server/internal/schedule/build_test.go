package schedule

import (
	"testing"
	"time"
)

func TestNewBuildsAndValidates(t *testing.T) {
	s, err := New("Asia/Kolkata", []int{1, 2, 3, 4, 5}, 570, 1050, []string{"2026-12-25"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.WorkDays[time.Monday] || s.WorkDays[time.Sunday] || !s.Holidays["2026-12-25"] {
		t.Fatalf("unexpected schedule: %+v", s)
	}
	// Built schedules behave like Default ones.
	d, _ := Default("Asia/Kolkata")
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, s.Location)
	if !s.NextSlot(now).Equal(d.NextSlot(now)) {
		t.Fatal("New and Default disagree")
	}
}

func TestNewRejectsBadInput(t *testing.T) {
	ok := func() error { _, err := New("UTC", []int{1}, 540, 1020, nil); return err }
	if err := ok(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() error{
		"empty tz":      func() error { _, err := New("", []int{1}, 540, 1020, nil); return err },
		"unknown tz":    func() error { _, err := New("Mars/Base", []int{1}, 540, 1020, nil); return err },
		"bad weekday":   func() error { _, err := New("UTC", []int{7}, 540, 1020, nil); return err },
		"no days":       func() error { _, err := New("UTC", nil, 540, 1020, nil); return err },
		"empty window":  func() error { _, err := New("UTC", []int{1}, 600, 600, nil); return err },
		"inverted":      func() error { _, err := New("UTC", []int{1}, 1020, 540, nil); return err },
		"past midnight": func() error { _, err := New("UTC", []int{1}, 540, 1441, nil); return err },
		"bad holiday":   func() error { _, err := New("UTC", []int{1}, 540, 1020, []string{"25/12/2026"}); return err },
	}
	for name, f := range cases {
		if f() == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
