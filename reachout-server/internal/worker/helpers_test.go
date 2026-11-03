package worker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ashmit/reachout-server/internal/accounts"
	"github.com/ashmit/reachout-server/internal/auth"
	"github.com/ashmit/reachout-server/internal/campaigns"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }

type sentAction struct {
	token  string
	action map[string]any
	id     string
}

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	fake  *linkedin.Fake
	accts *accounts.Service
	camps *campaigns.Service
	auth  *auth.Service
	clock *clock

	mu      sync.Mutex
	sent    []sentAction
	byID    map[string]sentAction
	results func(a sentAction) linkedin.WorkflowResult // what GetWorkflow returns for a workflow
	seq     int
}

func ok() linkedin.WorkflowResult {
	return linkedin.WorkflowResult{Status: "completed", Completion: json.RawMessage(`{"success":true}`)}
}

func actionError(typ string) linkedin.WorkflowResult {
	return linkedin.WorkflowResult{Status: "completed",
		Completion: json.RawMessage(fmt.Sprintf(`{"success":false,"error":{"type":%q,"message":"upstream text"}}`, typ))}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testdb.New(t)
	key := make([]byte, 32)
	rand.Read(key)
	c, _ := accounts.NewCipher(key)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := linkedin.NewFake()
	accts := accounts.NewService(pool, fake, c, log)
	e := &env{t: t, pool: pool, fake: fake, accts: accts, camps: campaigns.NewService(pool, accts, log),
		auth: auth.NewService(pool), clock: &clock{}, byID: map[string]sentAction{}, results: func(sentAction) linkedin.WorkflowResult { return ok() }}

	fake.Workflow = func(token string, action map[string]any) (linkedin.WorkflowStart, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		id := fmt.Sprintf("wf-%d", len(e.sent)+1)
		s := sentAction{token: token, action: action, id: id}
		e.sent = append(e.sent, s)
		e.byID[id] = s
		return linkedin.WorkflowStart{ID: id, Status: "pending", PendingReason: "queued"}, nil
	}
	fake.GetWorkflowFn = func(token, id string) (linkedin.WorkflowResult, error) {
		e.mu.Lock()
		s := e.byID[id]
		fn := e.results
		e.mu.Unlock()
		return fn(s), nil
	}
	return e
}

func (e *env) sentCount() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.sent) }

func (e *env) setResults(f func(a sentAction) linkedin.WorkflowResult) {
	e.mu.Lock()
	e.results = f
	e.mu.Unlock()
}

// testConfig has fixed gaps so tests can predict times.
func testConfig() Config {
	return Config{
		Enabled: true, Interval: time.Second,
		MinGap: 3 * time.Minute, MaxGap: 3 * time.Minute,
		FirstDelayMin: 5 * time.Minute, FirstDelayMax: 5 * time.Minute,
		StartingTTL: 5 * time.Minute, AttemptTTL: 45 * time.Minute,
		Concurrency: 4, MaxLimitRetries: 3,
	}
}

func (e *env) worker(cfg Config) *Worker {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := New(e.pool, e.accts, e.camps, e.fake, cfg, log)
	w.now = e.clock.Now
	w.rng = mrand.New(mrand.NewSource(1))
	return w
}

func (e *env) tick(w *Worker, at time.Time) {
	e.t.Helper()
	e.clock.Set(at)
	w.Tick(context.Background())
}

type user struct{ id int64 }

func (e *env) user(email string) user {
	e.t.Helper()
	u, _, _, err := e.auth.Signup(context.Background(), email, "a long enough password")
	if err != nil {
		e.t.Fatal(err)
	}
	return user{id: u.ID}
}

// account connects a fake LinkedIn account (token "TOKEN-<n>") with the default schedule
// (Mon-Fri 09:30-17:30 UTC) and no warm-up limit, so tests only meet the limits they set.
func (e *env) account(u user) int64 {
	e.t.Helper()
	ctx := context.Background()
	acc, link, err := e.accts.StartConnect(ctx, u.id, "UTC")
	if err != nil {
		e.t.Fatal(err)
	}
	e.seq++
	e.fake.FinishSession(strings.TrimPrefix(link, "https://app.linkedapi.io/connection/"), linkedin.Account{
		ID: fmt.Sprintf("la-%d", e.seq), Name: "Acct", IdentificationToken: fmt.Sprintf("TOKEN-%d", e.seq), ConnectedAt: time.Now(),
	})
	if got, err := e.accts.Refresh(ctx, u.id, acc.ID); err != nil || got.Status != accounts.StatusConnected {
		e.t.Fatalf("connect: %+v %v", got, err)
	}
	e.rampStart(acc.ID, "2020-01-01")
	return acc.ID
}

func (e *env) rampStart(accountID int64, date string) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE account_schedules SET ramp_started_at = $1::date WHERE account_id = $2`, date, accountID); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) caps(u user, accountID int64, connect, message int, holidays ...string) {
	e.t.Helper()
	_, err := e.accts.SetSchedule(context.Background(), u.id, accountID, accounts.ScheduleSettings{
		Timezone: "UTC", WorkDays: []int{1, 2, 3, 4, 5}, Start: "09:30", End: "17:30",
		Holidays: holidays, DailyConnectCap: connect, DailyMessageCap: message,
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func steps() []campaigns.Step {
	return []campaigns.Step{
		{Type: campaigns.StepConnect, Note: "Hi, let's connect"},
		{Type: campaigns.StepMessage, Body: "Thanks for connecting", DelayDays: 1},
		{Type: campaigns.StepFollowUp, Body: "Just checking in", DelayDays: 2},
	}
}

type camp struct {
	id    int64
	leads []int64
}

// campaign creates and starts a campaign with n new leads, then makes every lead due at `due`.
func (e *env) campaign(u user, accountID int64, prefix string, n, dailyLimit int, due time.Time) camp {
	e.t.Helper()
	ctx := context.Background()
	c, err := e.camps.Create(ctx, u.id, campaigns.Input{AccountID: accountID, Name: prefix, Steps: steps(), DailyLimit: dailyLimit})
	if err != nil {
		e.t.Fatal(err)
	}
	ids := make([]int64, n)
	for i := range ids {
		err := e.pool.QueryRow(ctx, `INSERT INTO leads (user_id, profile_url, name) VALUES ($1, $2, $3) RETURNING id`,
			u.id, leadURL(prefix, i), fmt.Sprintf("%s %d", prefix, i)).Scan(&ids[i])
		if err != nil {
			e.t.Fatal(err)
		}
	}
	if _, err := e.camps.AddLeads(ctx, u.id, c.ID, ids); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.camps.Start(ctx, u.id, c.ID); err != nil {
		e.t.Fatal(err)
	}
	e.setDue(c.ID, due)
	return camp{id: c.ID, leads: ids}
}

func leadURL(prefix string, i int) string {
	return fmt.Sprintf("https://www.linkedin.com/in/%s-%d", prefix, i)
}

// setDue makes every unfinished lead of the campaign due at t.
func (e *env) setDue(campaignID int64, t time.Time) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE campaign_leads SET next_action_at = $1 WHERE campaign_id = $2 AND state IN ('queued','connected','messaged','followed_up')`,
		t, campaignID); err != nil {
		e.t.Fatal(err)
	}
}

type leadInfo struct {
	state   campaigns.State
	due     *time.Time
	retries int
	lastErr string
	step    int
}

func (e *env) lead(campaignID, leadID int64) leadInfo {
	e.t.Helper()
	var l leadInfo
	err := e.pool.QueryRow(context.Background(),
		`SELECT state, next_action_at, retry_count, coalesce(last_error, ''), current_step FROM campaign_leads WHERE campaign_id = $1 AND lead_id = $2`,
		campaignID, leadID).Scan(&l.state, &l.due, &l.retries, &l.lastErr, &l.step)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

func (e *env) attemptCounts(accountID int64) map[string]int {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT status, count(*) FROM send_attempts WHERE account_id = $1 GROUP BY status`, accountID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	m := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		rows.Scan(&s, &n)
		m[s] = n
	}
	return m
}

func (e *env) stateCounts(campaignID int64) map[campaigns.State]int {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT state, count(*) FROM campaign_leads WHERE campaign_id = $1 GROUP BY state`, campaignID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	m := map[campaigns.State]int{}
	for rows.Next() {
		var s campaigns.State
		var n int
		rows.Scan(&s, &n)
		m[s] = n
	}
	return m
}

func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

// Reference week (2026): Fri 6 Nov, Sat 7, Sun 8, Mon 9, Tue 10, Wed 11, Thu 12, Fri 13, Mon 16, Tue 17.
func day(d int) time.Time { return utc(2026, 11, d, 0, 0) }

// runDay ticks every `step` from 09:00 to 18:00 on the given November day and returns how
// many sends started that day.
func (e *env) runDay(w *Worker, d int, step time.Duration) int {
	e.t.Helper()
	before := e.sentCount()
	for t := day(d).Add(9 * time.Hour); !t.After(day(d).Add(18 * time.Hour)); t = t.Add(step) {
		e.tick(w, t)
	}
	return e.sentCount() - before
}
