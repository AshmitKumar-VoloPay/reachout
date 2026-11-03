// Package worker sends the LinkedIn actions that campaigns schedule: connection requests,
// messages and follow-ups. It is the only code that causes activity on a LinkedIn account,
// so every rule that protects accounts lives here:
//
//   - nothing is sent outside the account's weekday working hours
//   - daily caps (campaign, account, and a warm-up ramp for new accounts) are never exceeded
//   - sends are spaced randomly, one LinkedIn task at a time per account
//   - every send is written down first, so a crash or a lost response can never cause a
//     blind duplicate
//   - when LinkedIn pushes back (limits), the lead is rescheduled, not failed
package worker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/ashmit/reachout-server/internal/accounts"
	"github.com/ashmit/reachout-server/internal/campaigns"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/schedule"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Attempt kinds (send_attempts.kind).
const (
	kindConnect  = "connect"
	kindMessage  = "message"
	kindFollowUp = "followup"
)

// Attempt statuses (send_attempts.status).
const (
	statusStarting  = "starting"
	statusSubmitted = "submitted"
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
	statusUnknown   = "unknown"
)

// Accounts is what the worker needs from accounts.Service.
type Accounts interface {
	IdentificationToken(ctx context.Context, accountID int64) (string, error)
	LoadSchedule(ctx context.Context, accountID int64) (schedule.Schedule, error)
	SendSettings(ctx context.Context, accountID int64) (accounts.SendSettings, error)
	SetNotBefore(ctx context.Context, accountID int64, t time.Time) error
	MarkNeedsAttention(ctx context.Context, accountID int64, status, detail string) error
}

// Recorder applies lead events. campaigns.Service implements it.
type Recorder interface {
	Record(ctx context.Context, campaignID, leadID int64, ev campaigns.Event, at time.Time, detail string) (campaigns.State, campaigns.Outcome, error)
}

type Config struct {
	Enabled  bool          // false turns every Tick into a no-op: the global kill switch
	Interval time.Duration // how often Run ticks

	// Random spacing between two sends from one account.
	MinGap, MaxGap time.Duration
	// Random wait after the window opens before the day's first send.
	FirstDelayMin, FirstDelayMax time.Duration

	StartingTTL     time.Duration // a 'starting' attempt older than this is treated as abandoned
	AttemptTTL      time.Duration // a submitted workflow older than this is given up on
	Concurrency     int           // accounts processed at once
	MaxLimitRetries int           // times LinkedIn may push one lead back before it is failed
}

func DefaultConfig() Config {
	return Config{
		Enabled: true, Interval: 15 * time.Second,
		MinGap: 2 * time.Minute, MaxGap: 6 * time.Minute,
		FirstDelayMin: time.Minute, FirstDelayMax: 10 * time.Minute,
		StartingTTL: 5 * time.Minute, AttemptTTL: 45 * time.Minute,
		Concurrency: 8, MaxLimitRetries: 3,
	}
}

type Worker struct {
	pool *pgxpool.Pool
	acc  Accounts
	rec  Recorder
	li   linkedin.Client
	cfg  Config
	log  *slog.Logger
	now  func() time.Time

	rngMu sync.Mutex
	rng   *rand.Rand
}

func New(pool *pgxpool.Pool, acc Accounts, rec Recorder, li linkedin.Client, cfg Config, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	return &Worker{pool: pool, acc: acc, rec: rec, li: li, cfg: cfg, log: log, now: time.Now,
		rng: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// Run ticks until ctx ends. Work in flight is safe to abandon: every send is recorded
// first and picked up again by the next process.
func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		w.log.Warn("worker disabled (WORKER_ENABLED=false): nothing will be sent")
		return
	}
	w.log.Info("worker started", "interval", w.cfg.Interval)
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		tc, cancel := context.WithTimeout(ctx, 2*time.Minute)
		w.Tick(tc)
		cancel()
		select {
		case <-ctx.Done():
			w.log.Info("worker stopped")
			return
		case <-t.C:
		}
	}
}

// Tick runs one round: settle sends in flight, finish leads whose follow-up wait is over,
// then give every account with due work the chance to start at most one new send.
func (w *Worker) Tick(ctx context.Context) {
	if !w.cfg.Enabled {
		return
	}
	w.resolveOpenAttempts(ctx)
	w.finishDueLeads(ctx)

	ids, err := w.accountsWithDueWork(ctx)
	if err != nil {
		w.log.Error("list accounts with due work", "err", err)
		return
	}
	sem := make(chan struct{}, w.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id int64) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					w.log.Error("worker panic", "account", id, "panic", r)
				}
			}()
			w.processAccount(ctx, id)
		}(id)
	}
	wg.Wait()
}

func (w *Worker) accountsWithDueWork(ctx context.Context) ([]int64, error) {
	rows, err := w.pool.Query(ctx,
		`SELECT DISTINCT c.account_id
		   FROM campaign_leads cl
		   JOIN campaigns c ON c.id = cl.campaign_id
		   JOIN linkedin_accounts a ON a.id = c.account_id
		  WHERE c.status = 'running' AND a.status = 'connected'
		    AND cl.state IN ('queued','connected','messaged') AND cl.next_action_at <= $1
		  ORDER BY c.account_id`, w.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// state each kind of send starts from.
func stateFor(kind string) string {
	switch kind {
	case kindMessage:
		return string(campaigns.StateConnected)
	case kindFollowUp:
		return string(campaigns.StateMessaged)
	}
	return string(campaigns.StateQueued)
}

func schedKind(kind string) schedule.Kind {
	if kind == kindConnect {
		return schedule.KindConnect
	}
	return schedule.KindMessage
}

func eventFor(kind string) campaigns.Event {
	switch kind {
	case kindMessage:
		return campaigns.EvMessageSent
	case kindFollowUp:
		return campaigns.EvFollowUpSent
	}
	return campaigns.EvRequestSent
}

// processAccount starts at most one send for the account, if every rule allows it.
func (w *Worker) processAccount(ctx context.Context, accountID int64) {
	if open, err := w.hasOpenAttempt(ctx, accountID); err != nil || open {
		return // one LinkedIn task at a time per account
	}
	st, err := w.acc.SendSettings(ctx, accountID)
	if err != nil || st.Status != accounts.StatusConnected {
		return
	}
	sched, err := w.acc.LoadSchedule(ctx, accountID)
	if err != nil {
		w.log.Error("load schedule", "account", accountID, "err", err)
		return
	}
	now := w.now()

	// Outside the weekday working window: do nothing now and move the due work to the next
	// window, so the dashboard shows when sending resumes.
	if !sched.IsOpen(now) {
		w.deferDue(ctx, accountID, []string{stateFor(kindConnect), stateFor(kindMessage), stateFor(kindFollowUp)}, sched.NextSlot(now), now)
		return
	}

	// Spacing. The first send of a day waits a random while after the window opens; later
	// sends wait for the gap chosen when the previous one finished.
	opens, _ := sched.OpensAt(now)
	notBefore := st.NotBefore
	if notBefore == nil || notBefore.Before(opens) {
		t := opens.Add(w.randDur(w.cfg.FirstDelayMin, w.cfg.FirstDelayMax))
		if err := w.acc.SetNotBefore(ctx, accountID, t); err != nil {
			w.log.Error("set not-before", "account", accountID, "err", err)
			return
		}
		notBefore = &t
	}
	if now.Before(*notBefore) {
		return
	}

	dayStart, dayEnd := sched.DayBounds(now)
	used, err := w.usedToday(ctx, accountID, dayStart, dayEnd)
	if err != nil {
		w.log.Error("count sends", "account", accountID, "err", err)
		return
	}
	caps := map[schedule.Kind]int{
		schedule.KindConnect: schedule.EffectiveCap(schedule.KindConnect, st.ConnectCap, st.RampStart, now, sched.Location),
		schedule.KindMessage: schedule.EffectiveCap(schedule.KindMessage, st.MessageCap, st.RampStart, now, sched.Location),
	}
	nextDay := sched.NextSlot(dayEnd)

	// Replies to people come before new outreach.
	for _, kind := range []string{kindMessage, kindFollowUp, kindConnect} {
		sk := schedKind(kind)
		if used[sk] >= caps[sk] {
			w.deferDue(ctx, accountID, []string{stateFor(kind)}, nextDay, now) // today's cap is spent
			continue
		}
		c, res, err := w.claim(ctx, accountID, kind, dayStart, dayEnd, now)
		if err != nil {
			w.log.Error("claim", "account", accountID, "kind", kind, "err", err)
			return
		}
		switch res {
		case claimed:
			w.send(ctx, c, now)
			return
		case claimBusy:
			return // another worker took this account
		}
		// Nothing eligible. If leads are still due they belong to campaigns that reached
		// their own daily limit, so they wait for tomorrow. (Skipped if a send is in flight:
		// then rows are merely locked, not capped.)
		if open, _ := w.hasOpenAttempt(ctx, accountID); !open {
			w.deferDue(ctx, accountID, []string{stateFor(kind)}, nextDay, now)
		}
	}
}

func (w *Worker) hasOpenAttempt(ctx context.Context, accountID int64) (bool, error) {
	var open bool
	err := w.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM send_attempts WHERE account_id = $1 AND status IN ('starting','submitted'))`,
		accountID).Scan(&open)
	return open, err
}

// usedToday counts today's attempts per kind of cap. Failed attempts do not count: LinkedIn
// never performed them. 'unknown' ones do, because they may have been performed.
func (w *Worker) usedToday(ctx context.Context, accountID int64, from, to time.Time) (map[schedule.Kind]int, error) {
	rows, err := w.pool.Query(ctx,
		`SELECT kind, count(*) FROM send_attempts
		  WHERE account_id = $1 AND created_at >= $2 AND created_at < $3
		    AND status IN ('starting','submitted','succeeded','unknown')
		  GROUP BY kind`, accountID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := map[schedule.Kind]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		used[schedKind(kind)] += n
	}
	return used, rows.Err()
}

type claimResult int

const (
	claimNone claimResult = iota
	claimed
	claimBusy
)

// attempt is one row of send_attempts.
type attempt struct {
	id, userID, accountID, campaignID, leadID int64
	kind, status, workflowID                  string
	createdAt                                 time.Time
}

// toSend is a claimed lead plus what to send.
type toSend struct {
	attempt
	profileURL string
	text       string // note for a connection request, body for messages
}

// claim picks the next due lead of the given kind and records the attempt, in one
// transaction. The row lock keeps two workers off the same lead, and the unique index on
// open attempts keeps them off the same account. Nothing else touches the connection pool
// while the transaction is open.
func (w *Worker) claim(ctx context.Context, accountID int64, kind string, dayStart, dayEnd, now time.Time) (*toSend, claimResult, error) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return nil, claimNone, err
	}
	defer tx.Rollback(ctx)

	q := `SELECT cl.campaign_id, cl.lead_id, cl.user_id, c.steps, l.profile_url
	        FROM campaign_leads cl
	        JOIN campaigns c ON c.id = cl.campaign_id
	        JOIN leads l ON l.id = cl.lead_id
	       WHERE c.account_id = $1 AND c.status = 'running' AND cl.state = $2 AND cl.next_action_at <= $3`
	args := []any{accountID, stateFor(kind), now}
	if kind == kindConnect {
		// A campaign's own daily limit applies to its connection requests.
		q += ` AND (SELECT count(*) FROM send_attempts a
		             WHERE a.campaign_id = c.id AND a.kind = 'connect'
		               AND a.created_at >= $4 AND a.created_at < $5
		               AND a.status IN ('starting','submitted','succeeded','unknown')) < c.daily_limit`
		args = append(args, dayStart, dayEnd)
	}
	q += ` ORDER BY cl.next_action_at, cl.lead_id LIMIT 1 FOR UPDATE OF cl SKIP LOCKED`

	var t toSend
	var stepsJSON []byte
	err = tx.QueryRow(ctx, q, args...).Scan(&t.campaignID, &t.leadID, &t.userID, &stepsJSON, &t.profileURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, claimNone, nil
	}
	if err != nil {
		return nil, claimNone, err
	}
	steps, err := stepsOf(stepsJSON)
	if err != nil {
		return nil, claimNone, err
	}
	switch kind {
	case kindConnect:
		t.text = steps[0].Note
	case kindMessage:
		t.text = steps[1].Body
	default:
		t.text = steps[2].Body
	}

	t.accountID, t.kind, t.status, t.createdAt = accountID, kind, statusStarting, now
	err = tx.QueryRow(ctx,
		`INSERT INTO send_attempts (user_id, account_id, campaign_id, lead_id, kind, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, 'starting', $6, $6) RETURNING id`,
		t.userID, accountID, t.campaignID, t.leadID, kind, now).Scan(&t.id)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return nil, claimBusy, nil // another worker already has a task open for this account
		}
		return nil, claimNone, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, claimNone, err
	}
	return &t, claimed, nil
}

// send submits the claimed action to Linked API. The attempt row already exists, so whatever
// happens here, the outcome is recoverable.
func (w *Worker) send(ctx context.Context, t *toSend, now time.Time) {
	token, err := w.acc.IdentificationToken(ctx, t.accountID)
	if err != nil {
		w.log.Error("read token", "attempt", t.id, "err", err)
		w.settle(ctx, t.attempt, outcome{kind: outRetryLater, detail: "could not read the account token"})
		return
	}
	var action map[string]any
	if t.kind == kindConnect {
		action = map[string]any{"actionType": "st.sendConnectionRequest", "personUrl": t.profileURL}
		if t.text != "" {
			action["note"] = t.text
		}
	} else {
		action = map[string]any{"actionType": "st.sendMessage", "personUrl": t.profileURL, "text": t.text}
	}

	ws, err := w.li.StartWorkflow(ctx, token, action)
	if err != nil {
		w.settle(ctx, t.attempt, w.classifyStartError(t.kind, err))
		return
	}
	if _, err := w.pool.Exec(ctx,
		`UPDATE send_attempts SET status = 'submitted', workflow_id = $1, updated_at = $2 WHERE id = $3 AND status = 'starting'`,
		ws.ID, now, t.id); err != nil {
		// The workflow exists but we could not note it. The attempt stays 'starting' and is
		// recovered by the stale-attempt rules, which never blindly resend a message.
		w.log.Error("mark submitted", "attempt", t.id, "err", err)
		return
	}
	w.log.Info("send submitted", "attempt", t.id, "account", t.accountID, "kind", t.kind)
}

// deferDue moves due leads in the given states, for running campaigns of one account, to
// `until`. Leads that are not due yet are left alone.
func (w *Worker) deferDue(ctx context.Context, accountID int64, states []string, until, now time.Time) {
	if _, err := w.pool.Exec(ctx,
		`UPDATE campaign_leads cl SET next_action_at = $1, updated_at = $2
		   FROM campaigns c
		  WHERE c.id = cl.campaign_id AND c.account_id = $3 AND c.status = 'running'
		    AND cl.state = ANY($4) AND cl.next_action_at <= $2`,
		until, now, accountID, states); err != nil {
		w.log.Error("defer due leads", "account", accountID, "err", err)
	}
}

// finishDueLeads marks leads done once their wait after the follow-up has passed with no
// reply. It sends nothing to LinkedIn, so it is not limited to working hours.
func (w *Worker) finishDueLeads(ctx context.Context) {
	now := w.now()
	rows, err := w.pool.Query(ctx,
		`SELECT cl.campaign_id, cl.lead_id FROM campaign_leads cl JOIN campaigns c ON c.id = cl.campaign_id
		  WHERE c.status = 'running' AND cl.state = 'followed_up' AND cl.next_action_at <= $1
		  ORDER BY cl.next_action_at LIMIT 200`, now)
	if err != nil {
		w.log.Error("list finished leads", "err", err)
		return
	}
	type pair struct{ campaign, lead int64 }
	var ps []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.campaign, &p.lead); err == nil {
			ps = append(ps, p)
		}
	}
	rows.Close()
	for _, p := range ps {
		if _, _, err := w.rec.Record(ctx, p.campaign, p.lead, campaigns.EvDone, now, ""); err != nil {
			w.log.Error("finish lead", "campaign", p.campaign, "lead", p.lead, "err", err)
		}
	}
}

func (w *Worker) randDur(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	w.rngMu.Lock()
	defer w.rngMu.Unlock()
	return min + time.Duration(w.rng.Int63n(int64(max-min)))
}

// jitter returns t plus the random gap between sends, moved into a working window.
func (w *Worker) jitter(s schedule.Schedule, t time.Time, min, max time.Duration) time.Time {
	w.rngMu.Lock()
	defer w.rngMu.Unlock()
	return s.WithJitter(t, min, max, w.rng)
}
