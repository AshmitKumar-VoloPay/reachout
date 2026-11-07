package accounts

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ashmit/reachout-server/internal/schedule"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Upper bounds protect the LinkedIn account: even if a user types a bigger number the
// server will not accept it. Linked API's own limits sit on top of these.
const (
	MaxDailyConnectCap = 30
	MaxDailyMessageCap = 100
	maxHolidays        = 100
)

// ScheduleSettings is the editable per-account sending schedule.
type ScheduleSettings struct {
	Timezone        string   `json:"timezone"`
	WorkDays        []int    `json:"work_days"` // 0=Sunday .. 6=Saturday
	Start           string   `json:"start"`     // "HH:MM" local, window opens
	End             string   `json:"end"`       // "HH:MM" local, window closes (exclusive)
	Holidays        []string `json:"holidays"`  // "YYYY-MM-DD" local dates
	DailyConnectCap int      `json:"daily_connect_cap"`
	DailyMessageCap int      `json:"daily_message_cap"`
}

// ScheduleView adds what the UI shows next to the settings.
type ScheduleView struct {
	ScheduleSettings
	OpenNow        bool      `json:"open_now"`
	NextSendWindow time.Time `json:"next_send_window"` // earliest moment sending is allowed
}

func (s *Service) GetSchedule(ctx context.Context, userID, accountID int64) (ScheduleView, error) {
	if _, err := s.get(ctx, userID, accountID); err != nil {
		return ScheduleView{}, err
	}
	st, err := s.loadSettings(ctx, accountID)
	if err != nil {
		return ScheduleView{}, err
	}
	return s.view(st)
}

// LoadSchedule returns the account's sending schedule for the worker and campaign code. It
// is not scoped by user: the caller must already have established who owns the account.
func (s *Service) LoadSchedule(ctx context.Context, accountID int64) (schedule.Schedule, error) {
	st, err := s.loadSettings(ctx, accountID)
	if err != nil {
		return schedule.Schedule{}, err
	}
	return buildSchedule(st)
}

func (s *Service) loadSettings(ctx context.Context, accountID int64) (ScheduleSettings, error) {
	var d accountDoc
	err := s.coll().FindOne(ctx, bson.D{eq("_id", accountID)}, options.FindOne().SetProjection(bson.D{eq("schedule", 1)})).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return ScheduleSettings{}, ErrNotFound
	}
	if err != nil {
		return ScheduleSettings{}, err
	}
	sc := d.Schedule
	return ScheduleSettings{
		Timezone: sc.Timezone, WorkDays: append([]int{}, sc.WorkDays...), Start: formatMinute(sc.StartMinute),
		End: formatMinute(sc.EndMinute), Holidays: append([]string{}, sc.Holidays...),
		DailyConnectCap: sc.DailyConnectCap, DailyMessageCap: sc.DailyMessageCap,
	}, nil
}

func (s *Service) view(st ScheduleSettings) (ScheduleView, error) {
	sc, err := buildSchedule(st)
	if err != nil {
		return ScheduleView{}, err
	}
	now := s.now()
	return ScheduleView{ScheduleSettings: st, OpenNow: sc.IsOpen(now), NextSendWindow: sc.NextSlot(now)}, nil
}

// SetSchedule validates and stores new settings. Invalid input returns an error wrapping
// ErrInvalidSchedule and changes nothing.
func (s *Service) SetSchedule(ctx context.Context, userID, accountID int64, in ScheduleSettings) (ScheduleView, error) {
	if _, err := s.get(ctx, userID, accountID); err != nil {
		return ScheduleView{}, err
	}
	st, err := normalizeSchedule(in)
	if err != nil {
		return ScheduleView{}, err
	}
	start, _ := parseMinute(st.Start)
	end, _ := parseMinute(st.End)
	_, err = s.coll().UpdateOne(ctx, bson.D{eq("_id", accountID), eq("user_id", userID)}, bson.D{{Key: "$set", Value: bson.D{
		eq("schedule.timezone", st.Timezone), eq("schedule.work_days", st.WorkDays), eq("schedule.start_minute", start),
		eq("schedule.end_minute", end), eq("schedule.holidays", st.Holidays),
		eq("schedule.daily_connect_cap", st.DailyConnectCap), eq("schedule.daily_message_cap", st.DailyMessageCap),
	}}})
	if err != nil {
		return ScheduleView{}, err
	}
	return s.view(st)
}

// normalizeSchedule validates in and returns it with days and holidays sorted and unique.
func normalizeSchedule(in ScheduleSettings) (ScheduleSettings, error) {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidSchedule, fmt.Sprintf(format, a...))
	}
	st := in
	st.Timezone = strings.TrimSpace(st.Timezone)

	if st.DailyConnectCap < 0 || st.DailyConnectCap > MaxDailyConnectCap {
		return st, bad("daily_connect_cap must be 0 to %d", MaxDailyConnectCap)
	}
	if st.DailyMessageCap < 0 || st.DailyMessageCap > MaxDailyMessageCap {
		return st, bad("daily_message_cap must be 0 to %d", MaxDailyMessageCap)
	}
	if len(st.Holidays) > maxHolidays {
		return st, bad("at most %d holidays", maxHolidays)
	}
	start, err := parseMinute(st.Start)
	if err != nil {
		return st, bad("start: %v", err)
	}
	end, err := parseMinute(st.End)
	if err != nil {
		return st, bad("end: %v", err)
	}

	st.WorkDays = uniqueSortedInts(st.WorkDays)
	st.Holidays = uniqueSortedStrings(st.Holidays)
	// schedule.New is the single validator for tz, weekdays, window and holiday format.
	if _, err := schedule.New(st.Timezone, st.WorkDays, start, end, st.Holidays); err != nil {
		return st, bad("%v", err)
	}
	st.Start, st.End = formatMinute(start), formatMinute(end)
	return st, nil
}

func buildSchedule(st ScheduleSettings) (schedule.Schedule, error) {
	start, err := parseMinute(st.Start)
	if err != nil {
		return schedule.Schedule{}, err
	}
	end, err := parseMinute(st.End)
	if err != nil {
		return schedule.Schedule{}, err
	}
	return schedule.New(st.Timezone, st.WorkDays, start, end, st.Holidays)
}

// parseMinute turns "HH:MM" into minutes after midnight. "24:00" is allowed for a window
// that runs to the end of the day.
func parseMinute(v string) (int, error) {
	if v == "24:00" {
		return 24 * 60, nil
	}
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, fmt.Errorf("%q is not HH:MM", v)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func formatMinute(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

func uniqueSortedInts(in []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

func uniqueSortedStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
