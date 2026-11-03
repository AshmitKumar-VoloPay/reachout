package accounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Account statuses stored in linkedin_accounts.status.
const (
	StatusPending              = "pending"
	StatusConnected            = "connected"
	StatusFrozen               = "frozen"
	StatusReconnectionRequired = "reconnection_required"
	StatusDisconnected         = "disconnected"
	StatusError                = "error"
)

// pendingTTL is how long a connection attempt may stay open. Linked API's own session
// lifetime is not documented, and only one attempt may be in flight at a time, so a
// stalled attempt must not block everyone else for long.
const pendingTTL = 10 * time.Minute

var (
	ErrNotFound         = errors.New("account not found")
	ErrConnectionBusy   = errors.New("another connection is in progress, try again in a few minutes")
	ErrAmbiguous        = errors.New("could not tell which LinkedIn account was just connected")
	ErrNotReconnectable = errors.New("account does not need to be reconnected")
	ErrInvalidSchedule  = errors.New("invalid schedule")
)

// Account is the browser-safe view of a connected account. It deliberately has no
// token field.
type Account struct {
	ID               int64      `json:"id"`
	Name             string     `json:"name"`
	ProfileURL       string     `json:"profile_url"`
	Headline         string     `json:"headline"`
	AvatarURL        string     `json:"avatar_url"`
	Status           string     `json:"status"`
	StatusDetail     string     `json:"status_detail,omitempty"`
	ConnectedAt      *time.Time `json:"connected_at,omitempty"`
	ConnectionLink   string     `json:"connection_link,omitempty"`   // while pending: where the user logs in
	ReconnectionLink string     `json:"reconnection_link,omitempty"` // when reconnection_required
	CreatedAt        time.Time  `json:"created_at"`
}

type Service struct {
	pool   *pgxpool.Pool
	li     linkedin.Client
	cipher *Cipher
	log    *slog.Logger
	now    func() time.Time
}

func NewService(pool *pgxpool.Pool, li linkedin.Client, c *Cipher, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, li: li, cipher: c, log: log, now: time.Now}
}

// Every read below filters on user_id, so one user can never see or touch another's
// account. A foreign or unknown id is reported as ErrNotFound either way.
const accountColumns = `id, name, profile_url, headline, avatar_url, status, status_detail,
	connected_at, coalesce(reconnection_link, ''), created_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanAccount(r rowScanner) (Account, error) {
	var a Account
	err := r.Scan(&a.ID, &a.Name, &a.ProfileURL, &a.Headline, &a.AvatarURL, &a.Status, &a.StatusDetail,
		&a.ConnectedAt, &a.ReconnectionLink, &a.CreatedAt)
	return a, err
}

func (s *Service) get(ctx context.Context, userID, id int64) (Account, error) {
	a, err := scanAccount(s.pool.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM linkedin_accounts WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

func (s *Service) Get(ctx context.Context, userID, id int64) (Account, error) {
	return s.get(ctx, userID, id)
}

func (s *Service) List(ctx context.Context, userID int64) ([]Account, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+accountColumns+` FROM linkedin_accounts WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// StartConnect begins a LinkedIn connection for the user and returns the account row
// (status pending) plus the link the user opens to log into LinkedIn.
//
// Only one attempt can be pending across all users (see migration 002). Stale attempts
// are expired first so a user who closed the tab does not block others.
func (s *Service) StartConnect(ctx context.Context, userID int64, timezone string) (Account, string, error) {
	if err := validateTimezone(timezone); err != nil {
		return Account{}, "", err
	}
	s.expireStalePending(ctx)

	existing, err := s.li.ListAccounts(ctx)
	if err != nil {
		return Account{}, "", err
	}
	known := make([]string, 0, len(existing))
	for _, a := range existing {
		known = append(known, a.ID)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Account{}, "", err
	}
	defer tx.Rollback(ctx)

	var id int64
	err = tx.QueryRow(ctx,
		`INSERT INTO linkedin_accounts (user_id, status, known_account_ids)
		 VALUES ($1, 'pending', $2) RETURNING id`, userID, known).Scan(&id)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return Account{}, "", ErrConnectionBusy
		}
		return Account{}, "", err
	}
	sess, err := s.li.CreateConnectionSession(ctx)
	if err != nil {
		return Account{}, "", err // tx rolls back: no row left behind
	}
	if _, err := tx.Exec(ctx, `UPDATE linkedin_accounts SET connection_session_id = $1 WHERE id = $2`, sess.ID, id); err != nil {
		s.cancelQuietly(sess.ID)
		return Account{}, "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO account_schedules (account_id, timezone) VALUES ($1, $2)`, id, timezone); err != nil {
		s.cancelQuietly(sess.ID)
		return Account{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		s.cancelQuietly(sess.ID)
		return Account{}, "", err
	}
	a, err := s.get(ctx, userID, id)
	return a, sess.Link, err
}

func (s *Service) cancelQuietly(sessionID string) {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.li.CancelConnectionSession(c, sessionID); err != nil {
		s.log.Warn("cancel connection session", "err", err)
	}
}

// expireStalePending marks abandoned attempts as errors and cancels their sessions.
func (s *Service) expireStalePending(ctx context.Context) {
	rows, err := s.pool.Query(ctx,
		`UPDATE linkedin_accounts
		    SET status = 'error', status_detail = 'connection timed out'
		  WHERE status = 'pending' AND created_at < $1
		RETURNING coalesce(connection_session_id, '')`, s.now().Add(-pendingTTL))
	if err != nil {
		s.log.Warn("expire stale pending", "err", err)
		return
	}
	var sessions []string
	for rows.Next() {
		var sid string
		if rows.Scan(&sid) == nil && sid != "" {
			sessions = append(sessions, sid)
		}
	}
	rows.Close()
	for _, sid := range sessions {
		s.cancelQuietly(sid)
	}
}

// Refresh brings one account up to date with Linked API. For a pending account it
// checks the connection session and, once login succeeded, claims the new account.
func (s *Service) Refresh(ctx context.Context, userID, id int64) (Account, error) {
	a, err := s.get(ctx, userID, id)
	if err != nil {
		return Account{}, err
	}
	if a.Status == StatusPending {
		return s.refreshPending(ctx, userID, id)
	}
	if a.Status == StatusError {
		return a, nil // failed attempts are final; the user starts a new one
	}
	return s.refreshConnected(ctx, userID, id)
}

func (s *Service) refreshPending(ctx context.Context, userID, id int64) (Account, error) {
	var sessionID string
	var known []string
	var createdAt time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT coalesce(connection_session_id, ''), known_account_ids, created_at
		   FROM linkedin_accounts WHERE id = $1 AND user_id = $2 AND status = 'pending'`,
		id, userID).Scan(&sessionID, &known, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.get(ctx, userID, id) // someone else already finished it
	}
	if err != nil {
		return Account{}, err
	}
	if s.now().Sub(createdAt) > pendingTTL {
		s.expireStalePending(ctx)
		return s.get(ctx, userID, id)
	}

	status, err := s.li.GetConnectionSession(ctx, sessionID)
	if err != nil {
		return Account{}, err
	}
	switch status {
	case linkedin.SessionExpired, linkedin.SessionError, linkedin.SessionCancelled:
		return s.fail(ctx, userID, id, "LinkedIn login "+status)
	case linkedin.SessionSuccess:
		return s.claimNewAccount(ctx, userID, id, known)
	default:
		return s.get(ctx, userID, id) // still waiting on the user
	}
}

func (s *Service) fail(ctx context.Context, userID, id int64, detail string) (Account, error) {
	if _, err := s.pool.Exec(ctx,
		`UPDATE linkedin_accounts SET status = 'error', status_detail = $1
		  WHERE id = $2 AND user_id = $3 AND status = 'pending'`, detail, id, userID); err != nil {
		return Account{}, err
	}
	return s.get(ctx, userID, id)
}

// claimNewAccount finds the one Linked API account that did not exist when the session
// started and was not already claimed, and attaches it to this row. Zero candidates means
// Linked API is still creating it. More than one means we cannot tell which is ours, so
// we refuse rather than risk giving a user someone else's LinkedIn account.
func (s *Service) claimNewAccount(ctx context.Context, userID, id int64, known []string) (Account, error) {
	accs, err := s.li.ListAccounts(ctx)
	if err != nil {
		return Account{}, err
	}
	skip := map[string]bool{}
	for _, k := range known {
		skip[k] = true
	}
	rows, err := s.pool.Query(ctx, `SELECT linkedin_account_id FROM linkedin_accounts WHERE linkedin_account_id IS NOT NULL`)
	if err != nil {
		return Account{}, err
	}
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil {
			skip[k] = true
		}
	}
	rows.Close()

	var candidates []linkedin.Account
	for _, a := range accs {
		if !skip[a.ID] {
			candidates = append(candidates, a)
		}
	}
	switch len(candidates) {
	case 0:
		return s.get(ctx, userID, id) // account still being created
	case 1:
	default:
		s.log.Error("ambiguous new linkedin account", "account_row", id, "candidates", len(candidates))
		if _, err := s.fail(ctx, userID, id, ErrAmbiguous.Error()); err != nil {
			return Account{}, err
		}
		return Account{}, ErrAmbiguous
	}

	c := candidates[0]
	enc, err := s.cipher.Encrypt(c.IdentificationToken, bindFor(id))
	if err != nil {
		return Account{}, err
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE linkedin_accounts
		    SET linkedin_account_id = $1, identification_token = $2, name = $3, profile_url = $4,
		        headline = $5, avatar_url = $6, status = $7, status_detail = '',
		        connected_at = $8, connection_session_id = NULL
		  WHERE id = $9 AND user_id = $10 AND status = 'pending'`,
		c.ID, enc, c.Name, c.URL, c.Headline, c.AvatarURL, mapStatus(c.Status), connectedAt(c, s.now()), id, userID)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return s.get(ctx, userID, id) // lost a race with a concurrent refresh
		}
		return Account{}, err
	}
	return s.get(ctx, userID, id)
}

func (s *Service) refreshConnected(ctx context.Context, userID, id int64) (Account, error) {
	var laID string
	err := s.pool.QueryRow(ctx,
		`SELECT coalesce(linkedin_account_id, '') FROM linkedin_accounts WHERE id = $1 AND user_id = $2`,
		id, userID).Scan(&laID)
	if err != nil {
		return Account{}, err
	}
	accs, err := s.li.ListAccounts(ctx)
	if err != nil {
		return Account{}, err
	}
	for _, a := range accs {
		if a.ID != laID {
			continue
		}
		enc, err := s.cipher.Encrypt(a.IdentificationToken, bindFor(id)) // token may have been regenerated
		if err != nil {
			return Account{}, err
		}
		_, err = s.pool.Exec(ctx,
			`UPDATE linkedin_accounts
			    SET name = $1, profile_url = $2, headline = $3, avatar_url = $4, status = $5,
			        status_detail = '', identification_token = $6, reconnection_link = nullif($7, '')
			  WHERE id = $8 AND user_id = $9`,
			a.Name, a.URL, a.Headline, a.AvatarURL, mapStatus(a.Status), enc, a.ReconnectionLink, id, userID)
		if err != nil {
			return Account{}, err
		}
		return s.get(ctx, userID, id)
	}
	// Gone from Linked API: disconnected there.
	if _, err := s.pool.Exec(ctx,
		`UPDATE linkedin_accounts SET status = 'disconnected', status_detail = 'removed in Linked API'
		  WHERE id = $1 AND user_id = $2`, id, userID); err != nil {
		return Account{}, err
	}
	return s.get(ctx, userID, id)
}

// Reconnect starts a reconnection session for an account that needs one and returns its link.
func (s *Service) Reconnect(ctx context.Context, userID, id int64) (string, error) {
	var laID, status string
	err := s.pool.QueryRow(ctx,
		`SELECT coalesce(linkedin_account_id, ''), status FROM linkedin_accounts WHERE id = $1 AND user_id = $2`,
		id, userID).Scan(&laID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if status != StatusReconnectionRequired || laID == "" {
		return "", ErrNotReconnectable
	}
	sess, err := s.li.CreateReconnectionSession(ctx, laID)
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE linkedin_accounts SET reconnection_link = $1 WHERE id = $2 AND user_id = $3`, sess.Link, id, userID); err != nil {
		return "", err
	}
	return sess.Link, nil
}

// IdentificationToken returns the decrypted token for the worker. It is intentionally not
// scoped by user (the worker acts for accounts, not browsers) and must never be exposed
// through an HTTP handler.
func (s *Service) IdentificationToken(ctx context.Context, accountID int64) (string, error) {
	var enc *string
	err := s.pool.QueryRow(ctx, `SELECT identification_token FROM linkedin_accounts WHERE id = $1`, accountID).Scan(&enc)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && enc == nil) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return s.cipher.Decrypt(*enc, bindFor(accountID))
}

func bindFor(rowID int64) string { return fmt.Sprintf("linkedin_accounts:%d", rowID) }

func mapStatus(linkedAPIStatus string) string {
	switch linkedAPIStatus {
	case linkedin.AccountActive:
		return StatusConnected
	case linkedin.AccountFrozen:
		return StatusFrozen
	case linkedin.AccountReconnectionRequired:
		return StatusReconnectionRequired
	}
	return StatusError
}

func connectedAt(a linkedin.Account, fallback time.Time) time.Time {
	if a.ConnectedAt.IsZero() {
		return fallback
	}
	return a.ConnectedAt
}

func validateTimezone(tz string) error {
	if _, err := time.LoadLocation(tz); err != nil || tz == "" {
		return fmt.Errorf("%w: unknown timezone %q", ErrInvalidSchedule, tz)
	}
	return nil
}
