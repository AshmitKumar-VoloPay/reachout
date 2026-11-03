package campaigns

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/ashmit/reachout-server/internal/accounts"
	"github.com/ashmit/reachout-server/internal/schedule"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Accounts is what campaigns need from accounts.Service.
type Accounts interface {
	Get(ctx context.Context, userID, id int64) (accounts.Account, error)
	LoadSchedule(ctx context.Context, accountID int64) (schedule.Schedule, error)
}

type Service struct {
	pool *pgxpool.Pool
	acc  Accounts
	log  *slog.Logger
	now  func() time.Time
}

func NewService(pool *pgxpool.Pool, acc Accounts, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: pool, acc: acc, log: log, now: time.Now}
}

const campaignColumns = `id, account_id, name, steps, daily_limit, status, created_at, started_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanCampaign(r rowScanner) (Campaign, error) {
	var c Campaign
	var steps []byte
	if err := r.Scan(&c.ID, &c.AccountID, &c.Name, &steps, &c.DailyLimit, &c.Status, &c.CreatedAt, &c.StartedAt); err != nil {
		return Campaign{}, err
	}
	if err := json.Unmarshal(steps, &c.Steps); err != nil {
		return Campaign{}, err
	}
	return c, nil
}

// terminalStates is the SQL list of final lead states.
const terminalStates = `('replied','done','failed')`

// get loads one campaign owned by userID, with stats. Another user's id is ErrNotFound.
func (s *Service) get(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, userID, id int64) (Campaign, error) {
	c, err := scanCampaign(q.QueryRow(ctx,
		`SELECT `+campaignColumns+` FROM campaigns WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Campaign{}, ErrNotFound
	}
	if err != nil {
		return Campaign{}, err
	}
	err = q.QueryRow(ctx, statsSQL+` WHERE campaign_id = $1`, id).Scan(statsDest(&c.Stats)...)
	return c, err
}

const statsSQL = `SELECT count(*),
	count(*) FILTER (WHERE state = 'queued'),
	count(*) FILTER (WHERE current_step >= 1),
	count(*) FILTER (WHERE current_step >= 2),
	count(*) FILTER (WHERE current_step >= 3),
	count(*) FILTER (WHERE current_step >= 4),
	count(*) FILTER (WHERE state = 'replied'),
	count(*) FILTER (WHERE state = 'done'),
	count(*) FILTER (WHERE state = 'failed'),
	min(next_action_at) FILTER (WHERE state NOT IN ` + terminalStates + `)
	FROM campaign_leads`

func statsDest(st *Stats) []any {
	return []any{&st.Total, &st.Queued, &st.Sent, &st.Accepted, &st.Messaged, &st.FollowedUp,
		&st.Replied, &st.Done, &st.Failed, &st.NextActionAt}
}

func (s *Service) Get(ctx context.Context, userID, id int64) (Campaign, error) {
	return s.get(ctx, s.pool, userID, id)
}

// List returns the user's campaigns, newest first, each with stats.
func (s *Service) List(ctx context.Context, userID int64) ([]Campaign, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+campaignColumns+` FROM campaigns WHERE user_id = $1 ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	out := []Campaign{}
	var ids []int64
	for rows.Next() {
		c, err := scanCampaign(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
		ids = append(ids, c.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}

	srows, err := s.pool.Query(ctx,
		`SELECT campaign_id, `+statsSQL[len("SELECT "):]+` WHERE campaign_id = ANY($1) GROUP BY campaign_id`, ids)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	byID := map[int64]Stats{}
	for srows.Next() {
		var id int64
		var st Stats
		if err := srows.Scan(append([]any{&id}, statsDest(&st)...)...); err != nil {
			return nil, err
		}
		byID[id] = st
	}
	for i := range out {
		out[i].Stats = byID[out[i].ID]
	}
	return out, srows.Err()
}

// Create makes a draft campaign on one of the user's own LinkedIn accounts.
func (s *Service) Create(ctx context.Context, userID int64, in Input) (Campaign, error) {
	in, err := validateInput(in)
	if err != nil {
		return Campaign{}, err
	}
	if _, err := s.acc.Get(ctx, userID, in.AccountID); err != nil { // 404 for a foreign account
		return Campaign{}, err
	}
	steps, _ := json.Marshal(in.Steps)
	var id int64
	err = s.pool.QueryRow(ctx,
		`INSERT INTO campaigns (user_id, account_id, name, steps, daily_limit) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		userID, in.AccountID, in.Name, steps, in.DailyLimit).Scan(&id)
	if err != nil {
		return Campaign{}, err
	}
	return s.Get(ctx, userID, id)
}

// Update edits a campaign. Steps and account can only change while it is a draft; name and
// daily limit stay editable until the campaign is stopped or done.
func (s *Service) Update(ctx context.Context, userID, id int64, in Input) (Campaign, error) {
	in, err := validateInput(in)
	if err != nil {
		return Campaign{}, err
	}
	// Checked before the transaction opens (see Start): the account must be the user's.
	if _, err := s.acc.Get(ctx, userID, in.AccountID); err != nil {
		return Campaign{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Campaign{}, err
	}
	defer tx.Rollback(ctx)

	cur, err := scanCampaign(tx.QueryRow(ctx,
		`SELECT `+campaignColumns+` FROM campaigns WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Campaign{}, ErrNotFound
	}
	if err != nil {
		return Campaign{}, err
	}
	switch cur.Status {
	case StatusStopped, StatusDone:
		return Campaign{}, ErrNotEditable
	case StatusDraft:
		// any account of the user's is fine
	default: // running or paused: only name and daily limit
		stepsNow, _ := json.Marshal(cur.Steps)
		stepsNew, _ := json.Marshal(in.Steps)
		if in.AccountID != cur.AccountID || string(stepsNow) != string(stepsNew) {
			return Campaign{}, ErrNotEditable
		}
	}
	steps, _ := json.Marshal(in.Steps)
	if _, err := tx.Exec(ctx,
		`UPDATE campaigns SET account_id = $1, name = $2, steps = $3, daily_limit = $4 WHERE id = $5 AND user_id = $6`,
		in.AccountID, in.Name, steps, in.DailyLimit, id, userID); err != nil {
		return Campaign{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Campaign{}, err
	}
	return s.Get(ctx, userID, id)
}

// Delete removes a campaign that is not active. Running or paused campaigns must be
// stopped first so nobody deletes one that is mid-conversation with prospects.
func (s *Service) Delete(ctx context.Context, userID, id int64) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM campaigns WHERE id = $1 AND user_id = $2 AND status IN ('draft','stopped','done')`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := s.Get(ctx, userID, id); err != nil {
		return err
	}
	return ErrWrongStatus
}

// accountOf returns the LinkedIn account a campaign belongs to, or ErrNotFound if the
// campaign is not the user's. It uses its own connection and takes no lock.
func (s *Service) accountOf(ctx context.Context, userID, id int64) (int64, error) {
	var accountID int64
	err := s.pool.QueryRow(ctx, `SELECT account_id FROM campaigns WHERE id = $1 AND user_id = $2`, id, userID).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return accountID, err
}

// lock loads and row-locks a campaign for a lifecycle change.
func (s *Service) lock(ctx context.Context, tx pgx.Tx, userID, id int64) (status Status, accountID int64, err error) {
	err = tx.QueryRow(ctx, `SELECT status, account_id FROM campaigns WHERE id = $1 AND user_id = $2 FOR UPDATE`,
		id, userID).Scan(&status, &accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// Start launches a draft campaign. Every queued lead gets its first send time from the
// account's schedule: now if the window is open, otherwise the next weekday window.
func (s *Service) Start(ctx context.Context, userID, id int64) (Campaign, error) {
	// Everything that needs another database connection (account lookups, the schedule) is
	// loaded before the transaction opens. A transaction that waits for a second pool
	// connection while holding its own can deadlock the whole pool under load.
	accountID, err := s.accountOf(ctx, userID, id)
	if err != nil {
		return Campaign{}, err
	}
	acct, err := s.acc.Get(ctx, userID, accountID)
	if err != nil {
		return Campaign{}, err
	}
	if acct.Status != accounts.StatusConnected {
		return Campaign{}, ErrAccountNotReady
	}
	sched, err := s.acc.LoadSchedule(ctx, accountID)
	if err != nil {
		return Campaign{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Campaign{}, err
	}
	defer tx.Rollback(ctx)

	status, lockedAccount, err := s.lock(ctx, tx, userID, id)
	if err != nil {
		return Campaign{}, err
	}
	if status != StatusDraft || lockedAccount != accountID {
		return Campaign{}, ErrWrongStatus
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM campaign_leads WHERE campaign_id = $1`, id).Scan(&n); err != nil {
		return Campaign{}, err
	}
	if n == 0 {
		return Campaign{}, ErrNoLeads
	}
	var conflicts int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM campaign_leads cl WHERE cl.campaign_id = $1 AND `+activeElsewhere, id).Scan(&conflicts); err != nil {
		return Campaign{}, err
	}
	if conflicts > 0 {
		return Campaign{}, ErrLeadConflict
	}

	now := s.now()
	if _, err := tx.Exec(ctx,
		`UPDATE campaign_leads SET next_action_at = $1, updated_at = $2 WHERE campaign_id = $3 AND state = 'queued'`,
		sched.NextSlot(now), now, id); err != nil {
		return Campaign{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE campaigns SET status = 'running', started_at = $1 WHERE id = $2`, now, id); err != nil {
		return Campaign{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Campaign{}, err
	}
	return s.Get(ctx, userID, id)
}

// activeElsewhere is true (for a campaign_leads row cl) when the same lead is still being
// worked in a different running or paused campaign. One prospect must never get two
// competing sequences.
const activeElsewhere = `EXISTS (
	SELECT 1 FROM campaign_leads o JOIN campaigns oc ON oc.id = o.campaign_id
	 WHERE o.lead_id = cl.lead_id AND o.campaign_id <> cl.campaign_id
	   AND oc.status IN ('running','paused') AND o.state NOT IN ` + terminalStates + `)`

// Pause stops sending for a running campaign. Lead states and due times are kept.
func (s *Service) Pause(ctx context.Context, userID, id int64) (Campaign, error) {
	return s.transition(ctx, userID, id, []Status{StatusRunning}, StatusPaused, nil)
}

// Resume restarts a paused campaign. Sends that came due while it was paused are moved to
// the next open slot instead of all firing at once.
func (s *Service) Resume(ctx context.Context, userID, id int64) (Campaign, error) {
	accountID, err := s.accountOf(ctx, userID, id)
	if err != nil {
		return Campaign{}, err
	}
	sched, err := s.acc.LoadSchedule(ctx, accountID) // before the transaction, see Start
	if err != nil {
		return Campaign{}, err
	}
	return s.transition(ctx, userID, id, []Status{StatusPaused}, StatusRunning,
		func(ctx context.Context, tx pgx.Tx, campaignID, lockedAccount int64) error {
			if lockedAccount != accountID {
				return ErrWrongStatus
			}
			now := s.now()
			_, err := tx.Exec(ctx,
				`UPDATE campaign_leads SET next_action_at = $1, updated_at = $2
				  WHERE campaign_id = $3 AND state NOT IN `+terminalStates+`
				    AND ((state = 'queued' AND next_action_at IS NULL) OR next_action_at < $2)`,
				sched.NextSlot(now), now, campaignID)
			return err
		})
}

// Stop ends a campaign for good. Leads keep the state they reached (so the funnel stays
// accurate) but nothing more is sent. A stopped campaign cannot be restarted.
func (s *Service) Stop(ctx context.Context, userID, id int64) (Campaign, error) {
	return s.transition(ctx, userID, id, []Status{StatusDraft, StatusRunning, StatusPaused}, StatusStopped,
		func(ctx context.Context, tx pgx.Tx, campaignID, _ int64) error {
			_, err := tx.Exec(ctx,
				`UPDATE campaign_leads SET next_action_at = NULL, updated_at = now()
				  WHERE campaign_id = $1 AND state NOT IN `+terminalStates, campaignID)
			return err
		})
}

// transition moves a campaign between statuses under a row lock. side runs in the same
// transaction with the campaign and account ids.
func (s *Service) transition(ctx context.Context, userID, id int64, from []Status, to Status,
	side func(ctx context.Context, tx pgx.Tx, campaignID, accountID int64) error) (Campaign, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Campaign{}, err
	}
	defer tx.Rollback(ctx)

	status, accountID, err := s.lock(ctx, tx, userID, id)
	if err != nil {
		return Campaign{}, err
	}
	ok := false
	for _, f := range from {
		ok = ok || status == f
	}
	if !ok {
		return Campaign{}, ErrWrongStatus
	}
	if _, err := tx.Exec(ctx, `UPDATE campaigns SET status = $1 WHERE id = $2`, to, id); err != nil {
		return Campaign{}, err
	}
	if side != nil {
		if err := side(ctx, tx, id, accountID); err != nil {
			return Campaign{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Campaign{}, err
	}
	return s.Get(ctx, userID, id)
}
