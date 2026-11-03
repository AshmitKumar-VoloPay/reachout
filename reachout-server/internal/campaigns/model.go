package campaigns

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The fixed three-step sequence.
const (
	StepConnect  = "connect"
	StepMessage  = "message"
	StepFollowUp = "followup"
)

const (
	MaxNoteLen       = 300  // LinkedIn's connection note limit (200 on free accounts)
	MaxBodyLen       = 3000 // LinkedIn message length
	MaxMessageDelay  = 14   // business days between acceptance and the message
	MaxFollowUpDelay = 30   // business days of silence before the follow-up
	MaxNameLen       = 100
	MaxCampaignLeads = 1000
	MaxAddPerRequest = 500
	MaxDailyLimit    = 30
	DefaultDaily     = 10
)

var (
	ErrNotFound         = errors.New("campaign not found")
	ErrInvalid          = errors.New("invalid campaign")
	ErrNotEditable      = errors.New("campaign can no longer be edited in this status")
	ErrWrongStatus      = errors.New("action not allowed in the campaign's current status")
	ErrNoLeads          = errors.New("add at least one lead before starting")
	ErrAccountNotReady  = errors.New("the LinkedIn account is not connected")
	ErrLeadConflict     = errors.New("some leads are already in another active campaign")
	ErrLeadNotRemovable = errors.New("only leads that have not been contacted yet can be removed")
)

// Step is one stage of the sequence.
//
//	connect  : Note is the optional connection note
//	message  : Body is sent DelayDays business days after the request is accepted
//	followup : Body is sent DelayDays business days after the message if there was no reply
type Step struct {
	Type      string `json:"type"`
	Note      string `json:"note,omitempty"`
	Body      string `json:"body,omitempty"`
	DelayDays int    `json:"delay_days,omitempty"`
}

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// cleanText trims the text and rejects control characters other than newlines and tabs.
func cleanText(s string) (string, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return "", errors.New("contains control characters")
		}
	}
	return s, nil
}

// ValidateSteps checks that steps are exactly connect, message, followup, in that order,
// and returns a cleaned copy.
func ValidateSteps(in []Step) ([]Step, error) {
	if len(in) != 3 {
		return nil, bad("a campaign has exactly 3 steps (connect, message, followup), got %d", len(in))
	}
	want := []string{StepConnect, StepMessage, StepFollowUp}
	out := make([]Step, 3)
	for i, st := range in {
		if st.Type != want[i] {
			return nil, bad("step %d must be %q, got %q", i+1, want[i], st.Type)
		}
		out[i].Type = st.Type
		switch st.Type {
		case StepConnect:
			if st.Body != "" || st.DelayDays != 0 {
				return nil, bad("connect step only takes a note")
			}
			note, err := cleanText(st.Note)
			if err != nil {
				return nil, bad("connect note %v", err)
			}
			if utf8.RuneCountInString(note) > MaxNoteLen {
				return nil, bad("connect note is longer than %d characters", MaxNoteLen)
			}
			out[i].Note = note
		case StepMessage, StepFollowUp:
			if st.Note != "" {
				return nil, bad("%s step takes a body, not a note", st.Type)
			}
			body, err := cleanText(st.Body)
			if err != nil {
				return nil, bad("%s body %v", st.Type, err)
			}
			if body == "" {
				return nil, bad("%s body is required", st.Type)
			}
			if utf8.RuneCountInString(body) > MaxBodyLen {
				return nil, bad("%s body is longer than %d characters", st.Type, MaxBodyLen)
			}
			lo, hi := 0, MaxMessageDelay
			if st.Type == StepFollowUp {
				lo, hi = 1, MaxFollowUpDelay
			}
			if st.DelayDays < lo || st.DelayDays > hi {
				return nil, bad("%s delay_days must be %d to %d business days", st.Type, lo, hi)
			}
			out[i].Body, out[i].DelayDays = body, st.DelayDays
		}
	}
	return out, nil
}

// Status is a campaign's lifecycle state.
type Status string

const (
	StatusDraft   Status = "draft"
	StatusRunning Status = "running"
	StatusPaused  Status = "paused"
	StatusStopped Status = "stopped"
	StatusDone    Status = "done"
)

// Input is what a user supplies to create or edit a campaign.
type Input struct {
	AccountID  int64  `json:"account_id"`
	Name       string `json:"name"`
	Steps      []Step `json:"steps"`
	DailyLimit int    `json:"daily_limit"`
}

// Stats are the funnel numbers for one campaign. Sent, Accepted, Messaged and FollowedUp
// are cumulative ("reached at least this far") and never go down.
type Stats struct {
	Total        int        `json:"total"`
	Queued       int        `json:"queued"`
	Sent         int        `json:"sent"`     // connection requests sent
	Accepted     int        `json:"accepted"` // connected, including people who replied first
	Messaged     int        `json:"messaged"` // first message sent
	FollowedUp   int        `json:"followed_up"`
	Replied      int        `json:"replied"`
	Done         int        `json:"done"` // finished without a reply
	Failed       int        `json:"failed"`
	NextActionAt *time.Time `json:"next_action_at,omitempty"` // earliest scheduled send; "queued until" in the UI
}

type Campaign struct {
	ID         int64      `json:"id"`
	AccountID  int64      `json:"account_id"`
	Name       string     `json:"name"`
	Steps      []Step     `json:"steps"`
	DailyLimit int        `json:"daily_limit"`
	Status     Status     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	Stats      Stats      `json:"stats"`
}

func unmarshalSteps(b []byte, out *[]Step) error {
	if err := json.Unmarshal(b, out); err != nil {
		return err
	}
	if len(*out) != 3 {
		return fmt.Errorf("stored campaign has %d steps, want 3", len(*out))
	}
	return nil
}

func validateInput(in Input) (Input, error) {
	name, err := cleanText(in.Name)
	if err != nil || name == "" || strings.ContainsRune(name, '\n') {
		return in, bad("name is required and must be a single line")
	}
	if utf8.RuneCountInString(name) > MaxNameLen {
		return in, bad("name is longer than %d characters", MaxNameLen)
	}
	in.Name = name
	if in.DailyLimit == 0 {
		in.DailyLimit = DefaultDaily
	}
	if in.DailyLimit < 1 || in.DailyLimit > MaxDailyLimit {
		return in, bad("daily_limit must be 1 to %d", MaxDailyLimit)
	}
	steps, err := ValidateSteps(in.Steps)
	if err != nil {
		return in, err
	}
	in.Steps = steps
	return in, nil
}
