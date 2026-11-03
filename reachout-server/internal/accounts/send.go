package accounts

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// SendSettings is what the worker needs to decide whether an account may send right now.
// It is not exposed over HTTP.
type SendSettings struct {
	Status     string
	ConnectCap int        // configured daily connection requests
	MessageCap int        // configured daily messages and follow-ups
	RampStart  time.Time  // date the warm-up began (only the date matters)
	NotBefore  *time.Time // no send before this moment (spacing between sends)
}

func (s *Service) SendSettings(ctx context.Context, accountID int64) (SendSettings, error) {
	var st SendSettings
	err := s.pool.QueryRow(ctx,
		`SELECT a.status, sc.daily_connect_cap, sc.daily_message_cap, sc.ramp_started_at, sc.next_send_not_before
		   FROM linkedin_accounts a JOIN account_schedules sc ON sc.account_id = a.id
		  WHERE a.id = $1`, accountID).
		Scan(&st.Status, &st.ConnectCap, &st.MessageCap, &st.RampStart, &st.NotBefore)
	if errors.Is(err, pgx.ErrNoRows) {
		return SendSettings{}, ErrNotFound
	}
	return st, err
}

// SetNotBefore records the earliest time the account may send again.
func (s *Service) SetNotBefore(ctx context.Context, accountID int64, t time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE account_schedules SET next_send_not_before = $1 WHERE account_id = $2`, t, accountID)
	return err
}

// MarkNeedsAttention flags a connected account the worker found unusable (signed out of
// LinkedIn, unsupported language). Sending stops until a refresh or reconnect clears it.
func (s *Service) MarkNeedsAttention(ctx context.Context, accountID int64, status, detail string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE linkedin_accounts SET status = $1, status_detail = $2 WHERE id = $3 AND status = 'connected'`,
		status, detail, accountID)
	return err
}
