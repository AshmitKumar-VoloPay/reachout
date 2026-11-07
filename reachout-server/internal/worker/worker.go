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
	"sort"
	"sync"
	"time"

	"github.com/ashmit/reachout-server/internal/accounts"
	"github.com/ashmit/reachout-server/internal/campaigns"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/mdb"
	"github.com/ashmit/reachout-server/internal/schedule"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Attempt kinds (send_attempts.kind).
const (
	kindConnect  = "connect"
	kindMessage  = "message"
	kindFollowUp = "followup"

	// Not sends: one-time tasks that switch on Linked API's event feeds for an account.
	kindSyncNetwork = "sync_network"
	kindSyncInbox   = "sync_inbox"
)

// syncRetry is how long to wait before trying a failed sync task again.
const syncRetry = 30 * time.Minute

func isSync(kind string) bool { return kind == kindSyncNetwork || kind == kindSyncInbox }

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
	db  *mongo.Database
	acc Accounts
	rec Recorder
	li  linkedin.Client
	cfg Config
	log *slog.Logger
	now func() time.Time

	rngMu sync.Mutex
	rng   *rand.Rand
}

func New(db *mongo.Database, acc Accounts, rec Recorder, li linkedin.Client, cfg Config, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	return &Worker{db: db, acc: acc, rec: rec, li: li, cfg: cfg, log: log, now: time.Now,
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
	now := w.now()
	set := map[int64]bool{}

	// Accounts with a send due: running campaigns that have a due lead, on a connected account.
	cc, err := w.campaignsColl().Find(ctx, bson.D{eq("status", "running")}, options.Find().SetProjection(bson.D{eq("account_id", 1)}))
	if err != nil {
		return nil, err
	}
	var running []struct {
		ID        int64 `bson:"_id"`
		AccountID int64 `bson:"account_id"`
	}
	if err := cc.All(ctx, &running); err != nil {
		return nil, err
	}
	if len(running) > 0 {
		accountOf := map[int64]int64{}
		campaignIDs := make([]int64, len(running))
		for i, c := range running {
			accountOf[c.ID] = c.AccountID
			campaignIDs[i] = c.ID
		}
		var due []int64
		if err := w.db.Collection(mdb.CampaignLeads).Distinct(ctx, "campaign_id", bson.D{
			eq("campaign_id", idsIn(campaignIDs)), eq("state", bson.D{{Key: "$in", Value: bson.A{"queued", "connected", "messaged"}}}),
			eq("next_action_at", bson.D{{Key: "$lte", Value: now}}),
		}).Decode(&due); err != nil {
			return nil, err
		}
		var candidates []int64
		for _, c := range due {
			candidates = append(candidates, accountOf[c])
		}
		if len(candidates) > 0 {
			ac, err := w.accountsColl().Find(ctx, bson.D{eq("_id", idsIn(candidates)), eq("status", "connected")},
				options.Find().SetProjection(bson.D{eq("_id", 1)}))
			if err != nil {
				return nil, err
			}
			var ok []struct {
				ID int64 `bson:"_id"`
			}
			if err := ac.All(ctx, &ok); err != nil {
				return nil, err
			}
			for _, a := range ok {
				set[a.ID] = true
			}
		}
	}

	// Connected accounts whose event feeds are not on yet.
	sc, err := w.accountsColl().Find(ctx, bson.D{eq("status", "connected"), {Key: "$or", Value: bson.A{
		bson.D{eq("network_synced_at", nil)}, bson.D{eq("inbox_synced_at", nil)},
	}}}, options.Find().SetProjection(bson.D{eq("_id", 1)}))
	if err != nil {
		return nil, err
	}
	var unsynced []struct {
		ID int64 `bson:"_id"`
	}
	if err := sc.All(ctx, &unsynced); err != nil {
		return nil, err
	}
	for _, a := range unsynced {
		set[a.ID] = true
	}

	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
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

	// Linked API only reports accepted requests and replies from the moment its network and
	// inbox sync are switched on. Sending before that would leave us blind to the answers,
	// so nothing is sent until both are on.
	if w.syncPending(ctx, accountID, now) {
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
		case claimStale:
			return // the lead changed while we were claiming it: look again next tick
		}
		// Nothing eligible. If leads are still due they belong to campaigns that reached
		// their own daily limit, so they wait for tomorrow. (Skipped if a send is in flight:
		// then rows are merely locked, not capped.)
		if open, _ := w.hasOpenAttempt(ctx, accountID); !open {
			w.deferDue(ctx, accountID, []string{stateFor(kind)}, nextDay, now)
		}
	}
}

// syncPending starts whichever sync task is still missing and reports whether any is. It
// returns true while sync is incomplete, even when it is waiting to retry after a failure.
func (w *Worker) syncPending(ctx context.Context, accountID int64, now time.Time) bool {
	var acct struct {
		Network *time.Time `bson:"network_synced_at"`
		Inbox   *time.Time `bson:"inbox_synced_at"`
	}
	if err := w.accountsColl().FindOne(ctx, bson.D{eq("_id", accountID)}).Decode(&acct); err != nil {
		w.log.Error("read sync state", "account", accountID, "err", err)
		return true // when in doubt, do not send
	}
	pending := false
	for _, task := range []struct {
		kind string
		done *time.Time
	}{{kindSyncNetwork, acct.Network}, {kindSyncInbox, acct.Inbox}} {
		if task.done != nil {
			continue
		}
		pending = true
		var last attemptDoc
		err := w.attempts().FindOne(ctx, bson.D{eq("account_id", accountID), eq("kind", task.kind)},
			options.FindOne().SetSort(bson.D{eq("created_at", -1)})).Decode(&last)
		if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
			w.log.Error("read sync history", "account", accountID, "err", err)
			return true
		}
		if err == nil && now.Sub(last.CreatedAt) < syncRetry {
			continue // tried recently and it did not work: wait before trying again
		}
		t, res, err := w.claimSync(ctx, accountID, task.kind, now)
		if err != nil {
			w.log.Error("claim sync", "account", accountID, "kind", task.kind, "err", err)
			return true
		}
		if res == claimed {
			w.send(ctx, t, now)
		}
		return true // one task at a time: a sync task in flight or just started
	}
	return pending
}

// claimSync records a sync task for the account. It uses the same one-open-task-per-account
// index as sends, so a sync never runs alongside a send.
func (w *Worker) claimSync(ctx context.Context, accountID int64, kind string, now time.Time) (*toSend, claimResult, error) {
	var acct struct {
		UserID int64 `bson:"user_id"`
	}
	if err := w.accountsColl().FindOne(ctx, bson.D{eq("_id", accountID)}).Decode(&acct); err != nil {
		return nil, claimNone, err
	}
	id, err := mdb.NextID(ctx, w.db, mdb.SendAttempts)
	if err != nil {
		return nil, claimNone, err
	}
	if _, err := w.attempts().InsertOne(ctx, attemptDoc{ID: id, UserID: acct.UserID, AccountID: accountID, Kind: kind,
		Status: statusStarting, Open: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		if mdb.IsDup(err) {
			return nil, claimBusy, nil
		}
		return nil, claimNone, err
	}
	return &toSend{attempt: attempt{id: id, userID: acct.UserID, accountID: accountID, kind: kind, status: statusStarting, createdAt: now}}, claimed, nil
}

func (w *Worker) hasOpenAttempt(ctx context.Context, accountID int64) (bool, error) {
	n, err := w.attempts().CountDocuments(ctx, bson.D{eq("account_id", accountID), eq("open", true)})
	return n > 0, err
}

// usedToday counts today's attempts per kind of cap. Failed attempts do not count: LinkedIn
// never performed them. 'unknown' ones do, because they may have been performed.
func (w *Worker) usedToday(ctx context.Context, accountID int64, from, to time.Time) (map[schedule.Kind]int, error) {
	cur, err := w.attempts().Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			eq("account_id", accountID), eq("created_at", bson.D{{Key: "$gte", Value: from}, {Key: "$lt", Value: to}}),
			eq("kind", bson.D{{Key: "$in", Value: bson.A{kindConnect, kindMessage, kindFollowUp}}}),
			eq("status", bson.D{{Key: "$in", Value: countedStatuses}}),
		}}},
		{{Key: "$group", Value: bson.D{eq("_id", "$kind"), eq("n", bson.D{{Key: "$sum", Value: 1}})}}},
	})
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Kind string `bson:"_id"`
		N    int    `bson:"n"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	used := map[schedule.Kind]int{}
	for _, r := range rows {
		used[schedKind(r.Kind)] += r.N
	}
	return used, nil
}

type claimResult int

const (
	claimNone claimResult = iota
	claimed
	claimBusy
	claimStale // the lead was no longer due when we came to claim it
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
	// The account's running campaigns.
	cc, err := w.campaignsColl().Find(ctx, bson.D{eq("account_id", accountID), eq("status", "running")})
	if err != nil {
		return nil, claimNone, err
	}
	var camps []struct {
		ID         int64            `bson:"_id"`
		UserID     int64            `bson:"user_id"`
		Steps      []campaigns.Step `bson:"steps"`
		DailyLimit int              `bson:"daily_limit"`
	}
	if err := cc.All(ctx, &camps); err != nil {
		return nil, claimNone, err
	}
	byID := map[int64]int{}
	eligible := []int64{}
	for i, c := range camps {
		byID[c.ID] = i
		eligible = append(eligible, c.ID)
	}
	if len(eligible) == 0 {
		return nil, claimNone, nil
	}
	if kind == kindConnect {
		// A campaign's own daily limit applies to its connection requests.
		sent, err := w.attempts().Aggregate(ctx, mongo.Pipeline{
			{{Key: "$match", Value: bson.D{
				eq("campaign_id", idsIn(eligible)), eq("kind", kindConnect),
				eq("created_at", bson.D{{Key: "$gte", Value: dayStart}, {Key: "$lt", Value: dayEnd}}),
				eq("status", bson.D{{Key: "$in", Value: countedStatuses}}),
			}}},
			{{Key: "$group", Value: bson.D{eq("_id", "$campaign_id"), eq("n", bson.D{{Key: "$sum", Value: 1}})}}},
		})
		if err != nil {
			return nil, claimNone, err
		}
		var rows []struct {
			ID int64 `bson:"_id"`
			N  int   `bson:"n"`
		}
		if err := sent.All(ctx, &rows); err != nil {
			return nil, claimNone, err
		}
		today := map[int64]int{}
		for _, r := range rows {
			today[r.ID] = r.N
		}
		eligible = eligible[:0]
		for _, c := range camps {
			if today[c.ID] < c.DailyLimit {
				eligible = append(eligible, c.ID)
			}
		}
		if len(eligible) == 0 {
			return nil, claimNone, nil
		}
	}

	// The next due lead of this kind, oldest first.
	var cand struct {
		ID         bson.ObjectID `bson:"_id"`
		CampaignID int64         `bson:"campaign_id"`
		LeadID     int64         `bson:"lead_id"`
	}
	err = w.db.Collection(mdb.CampaignLeads).FindOne(ctx, bson.D{
		eq("campaign_id", idsIn(eligible)), eq("state", stateFor(kind)), eq("next_action_at", bson.D{{Key: "$lte", Value: now}}),
	}, options.FindOne().SetSort(bson.D{eq("next_action_at", 1), eq("lead_id", 1)})).Decode(&cand)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, claimNone, nil
	}
	if err != nil {
		return nil, claimNone, err
	}
	camp := camps[byID[cand.CampaignID]]
	if len(camp.Steps) != 3 {
		return nil, claimNone, errors.New("stored campaign steps are not the 3-step sequence")
	}
	var lead struct {
		ProfileURL string `bson:"profile_url"`
	}
	if err := w.db.Collection(mdb.Leads).FindOne(ctx, bson.D{eq("_id", cand.LeadID)}).Decode(&lead); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, claimStale, nil // the lead was deleted
		}
		return nil, claimNone, err
	}

	t := toSend{profileURL: lead.ProfileURL}
	switch kind {
	case kindConnect:
		t.text = camp.Steps[0].Note
	case kindMessage:
		t.text = camp.Steps[1].Body
	default:
		t.text = camp.Steps[2].Body
	}
	id, err := mdb.NextID(ctx, w.db, mdb.SendAttempts)
	if err != nil {
		return nil, claimNone, err
	}
	// Record the attempt before anything is sent. The unique index on open attempts lets only
	// one worker hold an account at a time, which is what keeps two of them off the same lead.
	if _, err := w.attempts().InsertOne(ctx, attemptDoc{ID: id, UserID: camp.UserID, AccountID: accountID, CampaignID: cand.CampaignID,
		LeadID: cand.LeadID, Kind: kind, Status: statusStarting, Open: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		if mdb.IsDup(err) {
			return nil, claimBusy, nil // another worker already has a task open for this account
		}
		return nil, claimNone, err
	}
	t.attempt = attempt{id: id, userID: camp.UserID, accountID: accountID, campaignID: cand.CampaignID, leadID: cand.LeadID,
		kind: kind, status: statusStarting, createdAt: now}

	// Now that we hold the account, check the lead is still waiting for this send. An attempt is
	// only ever closed after its lead's state has moved on, so whoever held the account before us
	// has finished with this lead already if it was theirs.
	n, err := w.db.Collection(mdb.CampaignLeads).CountDocuments(ctx, bson.D{
		eq("_id", cand.ID), eq("state", stateFor(kind)), eq("next_action_at", bson.D{{Key: "$lte", Value: now}}),
	})
	if err != nil || n == 0 {
		w.closeAttempt(ctx, t.attempt, statusFailed, "the lead changed before it was claimed", now)
		if err != nil {
			return nil, claimNone, err
		}
		return nil, claimStale, nil
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
	switch t.kind {
	case kindConnect:
		action = map[string]any{"actionType": "st.sendConnectionRequest", "personUrl": t.profileURL}
		if t.text != "" {
			action["note"] = t.text
		}
	case kindSyncNetwork:
		action = map[string]any{"actionType": "st.syncNetwork"}
	case kindSyncInbox:
		action = map[string]any{"actionType": "st.syncInbox"}
	default:
		action = map[string]any{"actionType": "st.sendMessage", "personUrl": t.profileURL, "text": t.text}
	}

	ws, err := w.li.StartWorkflow(ctx, token, action)
	if err != nil {
		w.settle(ctx, t.attempt, w.classifyStartError(t.kind, err))
		return
	}
	if _, err := w.attempts().UpdateOne(ctx, bson.D{eq("_id", t.id), eq("status", statusStarting)},
		bson.D{{Key: "$set", Value: bson.D{eq("status", statusSubmitted), eq("workflow_id", ws.ID), eq("updated_at", now)}}}); err != nil {
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
	cc, err := w.campaignsColl().Find(ctx, bson.D{eq("account_id", accountID), eq("status", "running")},
		options.Find().SetProjection(bson.D{eq("_id", 1)}))
	if err != nil {
		w.log.Error("defer due leads", "account", accountID, "err", err)
		return
	}
	var camps []struct {
		ID int64 `bson:"_id"`
	}
	if err := cc.All(ctx, &camps); err != nil || len(camps) == 0 {
		return
	}
	campaignIDs := make([]int64, len(camps))
	for i, c := range camps {
		campaignIDs[i] = c.ID
	}
	stateVals := make(bson.A, len(states))
	for i, st := range states {
		stateVals[i] = st
	}
	if _, err := w.db.Collection(mdb.CampaignLeads).UpdateMany(ctx, bson.D{
		eq("campaign_id", idsIn(campaignIDs)), eq("state", bson.D{{Key: "$in", Value: stateVals}}),
		eq("next_action_at", bson.D{{Key: "$lte", Value: now}}),
	}, bson.D{{Key: "$set", Value: bson.D{eq("next_action_at", until), eq("updated_at", now)}}}); err != nil {
		w.log.Error("defer due leads", "account", accountID, "err", err)
	}
}

// finishDueLeads marks leads done once their wait after the follow-up has passed with no
// reply. It sends nothing to LinkedIn, so it is not limited to working hours.
func (w *Worker) finishDueLeads(ctx context.Context) {
	now := w.now()
	cc, err := w.campaignsColl().Find(ctx, bson.D{eq("status", "running")}, options.Find().SetProjection(bson.D{eq("_id", 1)}))
	if err != nil {
		w.log.Error("list finished leads", "err", err)
		return
	}
	var camps []struct {
		ID int64 `bson:"_id"`
	}
	if err := cc.All(ctx, &camps); err != nil || len(camps) == 0 {
		return
	}
	campaignIDs := make([]int64, len(camps))
	for i, c := range camps {
		campaignIDs[i] = c.ID
	}
	cur, err := w.db.Collection(mdb.CampaignLeads).Find(ctx, bson.D{
		eq("campaign_id", idsIn(campaignIDs)), eq("state", "followed_up"), eq("next_action_at", bson.D{{Key: "$lte", Value: now}}),
	}, options.Find().SetSort(bson.D{eq("next_action_at", 1)}).SetLimit(200))
	if err != nil {
		w.log.Error("list finished leads", "err", err)
		return
	}
	var ps []struct {
		CampaignID int64 `bson:"campaign_id"`
		LeadID     int64 `bson:"lead_id"`
	}
	if err := cur.All(ctx, &ps); err != nil {
		w.log.Error("list finished leads", "err", err)
		return
	}
	for _, p := range ps {
		if _, _, err := w.rec.Record(ctx, p.CampaignID, p.LeadID, campaigns.EvDone, now, ""); err != nil {
			w.log.Error("finish lead", "campaign", p.CampaignID, "lead", p.LeadID, "err", err)
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

// attemptDoc is one row of the send_attempts collection. Open is true while the attempt is in
// flight (starting or submitted) and absent afterwards: a unique partial index on it is what
// allows one LinkedIn task at a time per account.
type attemptDoc struct {
	ID         int64      `bson:"_id"`
	UserID     int64      `bson:"user_id"`
	AccountID  int64      `bson:"account_id"`
	CampaignID int64      `bson:"campaign_id"`
	LeadID     int64      `bson:"lead_id"`
	Kind       string     `bson:"kind"`
	Status     string     `bson:"status"`
	Open       bool       `bson:"open,omitempty"`
	WorkflowID *string    `bson:"workflow_id,omitempty"`
	Detail     string     `bson:"detail"`
	CreatedAt  time.Time  `bson:"created_at"`
	UpdatedAt  time.Time  `bson:"updated_at"`
	FinishedAt *time.Time `bson:"finished_at,omitempty"`
}

func (d attemptDoc) attempt() attempt {
	a := attempt{id: d.ID, userID: d.UserID, accountID: d.AccountID, campaignID: d.CampaignID, leadID: d.LeadID,
		kind: d.Kind, status: d.Status, createdAt: d.CreatedAt}
	if d.WorkflowID != nil {
		a.workflowID = *d.WorkflowID
	}
	return a
}

// countedStatuses are the attempts that use up a daily cap: failed ones never reached LinkedIn,
// but "unknown" ones may have.
var countedStatuses = bson.A{statusStarting, statusSubmitted, statusSucceeded, statusUnknown}

func (w *Worker) attempts() *mongo.Collection      { return w.db.Collection(mdb.SendAttempts) }
func (w *Worker) campaignsColl() *mongo.Collection { return w.db.Collection(mdb.Campaigns) }
func (w *Worker) accountsColl() *mongo.Collection  { return w.db.Collection(mdb.Accounts) }

func eq(k string, v any) bson.E { return bson.E{Key: k, Value: v} }

func idsIn(vals []int64) bson.D {
	a := make(bson.A, len(vals))
	for i, v := range vals {
		a[i] = v
	}
	return bson.D{{Key: "$in", Value: a}}
}
