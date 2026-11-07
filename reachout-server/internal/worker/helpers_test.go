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
	"github.com/ashmit/reachout-server/internal/mdb"
	"github.com/ashmit/reachout-server/internal/testdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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
	db    *mongo.Database
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
	db := testdb.New(t)
	key := make([]byte, 32)
	rand.Read(key)
	c, _ := accounts.NewCipher(key)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := linkedin.NewFake()
	accts := accounts.NewService(db, fake, c, log)
	e := &env{t: t, db: db, fake: fake, accts: accts, camps: campaigns.NewService(db, accts, log),
		auth: auth.NewService(db), clock: &clock{}, byID: map[string]sentAction{}, results: func(sentAction) linkedin.WorkflowResult { return ok() }}

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
	w := New(e.db, e.accts, e.camps, e.fake, cfg, log)
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
	e.markSynced(acc.ID, true)
	return acc.ID
}

// markSynced says whether Linked API's network and inbox feeds are already on for the
// account. Most tests start with them on, so only the sync tests meet the gate.
func (e *env) markSynced(accountID int64, on bool) {
	e.t.Helper()
	update := bson.D{{Key: "$unset", Value: bson.D{{Key: "network_synced_at", Value: ""}, {Key: "inbox_synced_at", Value: ""}}}}
	if on {
		long := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		update = bson.D{{Key: "$set", Value: bson.D{{Key: "network_synced_at", Value: long}, {Key: "inbox_synced_at", Value: long}}}}
	}
	if _, err := e.db.Collection(mdb.Accounts).UpdateOne(context.Background(), bson.D{{Key: "_id", Value: accountID}}, update); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) rampStart(accountID int64, date string) {
	e.t.Helper()
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		e.t.Fatal(err)
	}
	testdb.Update(e.t, e.db, mdb.Accounts, bson.D{{Key: "_id", Value: accountID}}, bson.D{{Key: "schedule.ramp_started_at", Value: d}})
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
		id, err := mdb.NextID(ctx, e.db, mdb.Leads)
		if err != nil {
			e.t.Fatal(err)
		}
		testdb.Insert(e.t, e.db, mdb.Leads, bson.D{{Key: "_id", Value: id}, {Key: "user_id", Value: u.id},
			{Key: "profile_url", Value: leadURL(prefix, i)}, {Key: "name", Value: fmt.Sprintf("%s %d", prefix, i)},
			{Key: "headline", Value: ""}, {Key: "company", Value: ""}, {Key: "created_at", Value: time.Now()}})
		ids[i] = id
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
	testdb.Update(e.t, e.db, mdb.CampaignLeads, bson.D{{Key: "campaign_id", Value: campaignID},
		{Key: "state", Value: bson.D{{Key: "$in", Value: bson.A{"queued", "connected", "messaged", "followed_up"}}}}},
		bson.D{{Key: "next_action_at", Value: t}})
}

type leadInfo struct {
	state   campaigns.State
	due     *time.Time
	retries int
	lastErr string
	step    int
}

// clRow is a lead's place in a campaign, read straight from the database.
type clRow struct {
	CampaignID   int64      `bson:"campaign_id"`
	LeadID       int64      `bson:"lead_id"`
	State        string     `bson:"state"`
	NextActionAt *time.Time `bson:"next_action_at"`
	RetryCount   int        `bson:"retry_count"`
	LastError    string     `bson:"last_error"`
	CurrentStep  int        `bson:"current_step"`
}

func (e *env) lead(campaignID, leadID int64) leadInfo {
	e.t.Helper()
	r := testdb.One[clRow](e.t, e.db, mdb.CampaignLeads, bson.D{{Key: "campaign_id", Value: campaignID}, {Key: "lead_id", Value: leadID}})
	return leadInfo{state: campaigns.State(r.State), due: r.NextActionAt, retries: r.RetryCount, lastErr: r.LastError, step: r.CurrentStep}
}

func (e *env) attemptCounts(accountID int64) map[string]int {
	e.t.Helper()
	cur, err := e.db.Collection(mdb.SendAttempts).Find(context.Background(), bson.D{{Key: "account_id", Value: accountID}})
	if err != nil {
		e.t.Fatal(err)
	}
	var docs []attemptDoc
	if err := cur.All(context.Background(), &docs); err != nil {
		e.t.Fatal(err)
	}
	m := map[string]int{}
	for _, d := range docs {
		m[d.Status]++
	}
	return m
}

func (e *env) stateCounts(campaignID int64) map[campaigns.State]int {
	e.t.Helper()
	cur, err := e.db.Collection(mdb.CampaignLeads).Find(context.Background(), bson.D{{Key: "campaign_id", Value: campaignID}})
	if err != nil {
		e.t.Fatal(err)
	}
	var rows []clRow
	if err := cur.All(context.Background(), &rows); err != nil {
		e.t.Fatal(err)
	}
	m := map[campaigns.State]int{}
	for _, r := range rows {
		m[campaigns.State(r.State)]++
	}
	return m
}

// attemptDocs returns the send attempts matching filter, oldest first.
func (e *env) attemptDocs(filter bson.D) []attemptDoc {
	e.t.Helper()
	cur, err := e.db.Collection(mdb.SendAttempts).Find(context.Background(), filter, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		e.t.Fatal(err)
	}
	var docs []attemptDoc
	if err := cur.All(context.Background(), &docs); err != nil {
		e.t.Fatal(err)
	}
	return docs
}

type messageRow struct {
	Direction string `bson:"direction"`
	Body      string `bson:"body"`
}

// messages returns a lead's stored conversation, oldest first.
func (e *env) messages(leadID int64) []messageRow {
	e.t.Helper()
	cur, err := e.db.Collection(mdb.Messages).Find(context.Background(), bson.D{{Key: "lead_id", Value: leadID}}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		e.t.Fatal(err)
	}
	var rows []messageRow
	if err := cur.All(context.Background(), &rows); err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func (e *env) accountStatus(accountID int64) string {
	e.t.Helper()
	return testdb.One[struct {
		Status string `bson:"status"`
	}](e.t, e.db, mdb.Accounts, bson.D{{Key: "_id", Value: accountID}}).Status
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
