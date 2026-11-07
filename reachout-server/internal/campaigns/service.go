package campaigns

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"time"

	"github.com/ashmit/reachout-server/internal/accounts"
	"github.com/ashmit/reachout-server/internal/mdb"
	"github.com/ashmit/reachout-server/internal/schedule"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Accounts is what campaigns need from accounts.Service.
type Accounts interface {
	Get(ctx context.Context, userID, id int64) (accounts.Account, error)
	LoadSchedule(ctx context.Context, accountID int64) (schedule.Schedule, error)
}

type Service struct {
	db  *mongo.Database
	acc Accounts
	log *slog.Logger
	now func() time.Time
}

func NewService(db *mongo.Database, acc Accounts, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, acc: acc, log: log, now: time.Now}
}

// campaignDoc is the stored campaign. Its steps are embedded.
type campaignDoc struct {
	ID         int64      `bson:"_id"`
	UserID     int64      `bson:"user_id"`
	AccountID  int64      `bson:"account_id"`
	Name       string     `bson:"name"`
	Steps      []Step     `bson:"steps"`
	DailyLimit int        `bson:"daily_limit"`
	Status     Status     `bson:"status"`
	CreatedAt  time.Time  `bson:"created_at"`
	StartedAt  *time.Time `bson:"started_at,omitempty"`
}

func (d campaignDoc) campaign() Campaign {
	return Campaign{ID: d.ID, AccountID: d.AccountID, Name: d.Name, Steps: d.Steps, DailyLimit: d.DailyLimit,
		Status: d.Status, CreatedAt: d.CreatedAt, StartedAt: d.StartedAt}
}

func (s *Service) campaigns() *mongo.Collection { return s.db.Collection(mdb.Campaigns) }
func (s *Service) cls() *mongo.Collection       { return s.db.Collection(mdb.CampaignLeads) }

func eq(k string, v any) bson.E { return bson.E{Key: k, Value: v} }
func in(vals ...any) bson.D     { return bson.D{{Key: "$in", Value: bson.A(vals)}} }
func ids(vals []int64) bson.D {
	a := make(bson.A, len(vals))
	for i, v := range vals {
		a[i] = v
	}
	return bson.D{{Key: "$in", Value: a}}
}

// terminalStates are the final lead states; activeStates are the rest.
var (
	terminalStates = bson.D{{Key: "$nin", Value: bson.A{"replied", "done", "failed"}}} // matches active leads
	activeStates   = bson.A{"queued", "request_sent", "connected", "messaged", "followed_up"}
)

// doc loads a campaign owned by userID. Another user's id is ErrNotFound. This filter is the
// only tenant guard: MongoDB has no foreign keys to back it up.
func (s *Service) doc(ctx context.Context, userID, id int64) (campaignDoc, error) {
	var d campaignDoc
	err := s.campaigns().FindOne(ctx, bson.D{eq("_id", id), eq("user_id", userID)}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return campaignDoc{}, ErrNotFound
	}
	return d, err
}

// statsDoc is one $group result: the funnel counts of a campaign. Sent, accepted, messaged
// and followed up are "reached at least this step", so they never go down.
type statsDoc struct {
	ID         any        `bson:"_id"`
	Total      int        `bson:"total"`
	Queued     int        `bson:"queued"`
	Sent       int        `bson:"sent"`
	Accepted   int        `bson:"accepted"`
	Messaged   int        `bson:"messaged"`
	FollowedUp int        `bson:"followed_up"`
	Replied    int        `bson:"replied"`
	Done       int        `bson:"done"`
	Failed     int        `bson:"failed"`
	Next       *time.Time `bson:"next"`
}

func (d statsDoc) stats() Stats {
	return Stats{Total: d.Total, Queued: d.Queued, Sent: d.Sent, Accepted: d.Accepted, Messaged: d.Messaged,
		FollowedUp: d.FollowedUp, Replied: d.Replied, Done: d.Done, Failed: d.Failed, NextActionAt: d.Next}
}

func countIf(cond bson.D) bson.D {
	return bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{cond, 1, 0}}}}}
}

// statsFor computes the funnel for the given campaigns in one pass over their leads.
func (s *Service) statsFor(ctx context.Context, campaignIDs []int64) (map[int64]Stats, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{eq("campaign_id", ids(campaignIDs))}}},
		{{Key: "$group", Value: bson.D{
			eq("_id", "$campaign_id"),
			eq("total", bson.D{{Key: "$sum", Value: 1}}),
			eq("queued", countIf(bson.D{{Key: "$eq", Value: bson.A{"$state", "queued"}}})),
			eq("sent", countIf(bson.D{{Key: "$gte", Value: bson.A{"$current_step", 1}}})),
			eq("accepted", countIf(bson.D{{Key: "$gte", Value: bson.A{"$current_step", 2}}})),
			eq("messaged", countIf(bson.D{{Key: "$gte", Value: bson.A{"$current_step", 3}}})),
			eq("followed_up", countIf(bson.D{{Key: "$gte", Value: bson.A{"$current_step", 4}}})),
			eq("replied", countIf(bson.D{{Key: "$eq", Value: bson.A{"$state", "replied"}}})),
			eq("done", countIf(bson.D{{Key: "$eq", Value: bson.A{"$state", "done"}}})),
			eq("failed", countIf(bson.D{{Key: "$eq", Value: bson.A{"$state", "failed"}}})),
			// The earliest scheduled send among leads still in progress; finished ones do not count.
			eq("next", bson.D{{Key: "$min", Value: bson.D{{Key: "$cond", Value: bson.A{
				bson.D{{Key: "$in", Value: bson.A{"$state", activeStates}}}, "$next_action_at", nil}}}}}),
		}}},
	}
	cur, err := s.cls().Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	var rows []statsDoc
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := map[int64]Stats{}
	for _, r := range rows {
		if id, ok := r.ID.(int64); ok {
			out[id] = r.stats()
		}
	}
	return out, nil
}

func (s *Service) withStats(ctx context.Context, d campaignDoc) (Campaign, error) {
	st, err := s.statsFor(ctx, []int64{d.ID})
	c := d.campaign()
	c.Stats = st[d.ID]
	return c, err
}

func (s *Service) Get(ctx context.Context, userID, id int64) (Campaign, error) {
	d, err := s.doc(ctx, userID, id)
	if err != nil {
		return Campaign{}, err
	}
	return s.withStats(ctx, d)
}

// List returns the user's campaigns, newest first, each with stats.
func (s *Service) List(ctx context.Context, userID int64) ([]Campaign, error) {
	cur, err := s.campaigns().Find(ctx, bson.D{eq("user_id", userID)}, options.Find().SetSort(bson.D{eq("_id", -1)}))
	if err != nil {
		return nil, err
	}
	var docs []campaignDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]Campaign, 0, len(docs))
	if len(docs) == 0 {
		return out, nil
	}
	idList := make([]int64, len(docs))
	for i, d := range docs {
		idList[i] = d.ID
	}
	stats, err := s.statsFor(ctx, idList)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		c := d.campaign()
		c.Stats = stats[d.ID]
		out = append(out, c)
	}
	return out, nil
}

// Create makes a draft campaign on one of the user's own LinkedIn accounts.
func (s *Service) Create(ctx context.Context, userID int64, input Input) (Campaign, error) {
	input, err := validateInput(input)
	if err != nil {
		return Campaign{}, err
	}
	if _, err := s.acc.Get(ctx, userID, input.AccountID); err != nil { // 404 for a foreign account
		return Campaign{}, err
	}
	id, err := mdb.NextID(ctx, s.db, mdb.Campaigns)
	if err != nil {
		return Campaign{}, err
	}
	d := campaignDoc{ID: id, UserID: userID, AccountID: input.AccountID, Name: input.Name, Steps: input.Steps,
		DailyLimit: input.DailyLimit, Status: StatusDraft, CreatedAt: s.now()}
	if _, err := s.campaigns().InsertOne(ctx, d); err != nil {
		return Campaign{}, err
	}
	return s.withStats(ctx, d)
}

// Update edits a campaign. Steps and account can only change while it is a draft; name and
// daily limit stay editable until the campaign is stopped or done.
func (s *Service) Update(ctx context.Context, userID, id int64, input Input) (Campaign, error) {
	input, err := validateInput(input)
	if err != nil {
		return Campaign{}, err
	}
	if _, err := s.acc.Get(ctx, userID, input.AccountID); err != nil { // the account must be the user's
		return Campaign{}, err
	}
	cur, err := s.doc(ctx, userID, id)
	if err != nil {
		return Campaign{}, err
	}
	switch cur.Status {
	case StatusStopped, StatusDone:
		return Campaign{}, ErrNotEditable
	case StatusDraft:
		// any account of the user's is fine
	default: // running or paused: only name and daily limit
		if input.AccountID != cur.AccountID || !reflect.DeepEqual(cur.Steps, input.Steps) {
			return Campaign{}, ErrNotEditable
		}
	}
	// The status is part of the filter, so an edit that raced with a status change fails
	// instead of applying rules that no longer hold.
	r, err := s.campaigns().UpdateOne(ctx, bson.D{eq("_id", id), eq("user_id", userID), eq("status", cur.Status)},
		bson.D{{Key: "$set", Value: bson.D{eq("account_id", input.AccountID), eq("name", input.Name),
			eq("steps", input.Steps), eq("daily_limit", input.DailyLimit)}}})
	if err != nil {
		return Campaign{}, err
	}
	if r.MatchedCount == 0 {
		return Campaign{}, ErrNotEditable
	}
	return s.Get(ctx, userID, id)
}

// Delete removes a campaign that is not active, with its lead entries. Running or paused
// campaigns must be stopped first so nobody deletes one that is mid-conversation with prospects.
func (s *Service) Delete(ctx context.Context, userID, id int64) error {
	r, err := s.campaigns().DeleteOne(ctx, bson.D{eq("_id", id), eq("user_id", userID),
		eq("status", in("draft", "stopped", "done"))})
	if err != nil {
		return err
	}
	if r.DeletedCount == 1 {
		_, err := s.cls().DeleteMany(ctx, bson.D{eq("campaign_id", id), eq("user_id", userID)})
		return err
	}
	if _, err := s.doc(ctx, userID, id); err != nil {
		return err
	}
	return ErrWrongStatus
}

// cas moves a campaign to a new status only if it is currently in one of the allowed ones.
// It is a single atomic update, so two requests cannot both succeed.
func (s *Service) cas(ctx context.Context, userID, id int64, from []Status, set bson.D) error {
	fromVals := make([]any, len(from))
	for i, f := range from {
		fromVals[i] = f
	}
	r, err := s.campaigns().UpdateOne(ctx, bson.D{eq("_id", id), eq("user_id", userID), eq("status", in(fromVals...))},
		bson.D{{Key: "$set", Value: set}})
	if err != nil {
		return err
	}
	if r.MatchedCount == 1 {
		return nil
	}
	if _, err := s.doc(ctx, userID, id); err != nil {
		return err // not found
	}
	return ErrWrongStatus
}

// busyElsewhere returns which of the leads are still being worked in another running or
// paused campaign of this user. One prospect must never get two competing sequences.
func (s *Service) busyElsewhere(ctx context.Context, userID, exceptCampaign int64, leadIDs []int64) (map[int64]bool, error) {
	busy := map[int64]bool{}
	if len(leadIDs) == 0 {
		return busy, nil
	}
	cur, err := s.cls().Find(ctx, bson.D{eq("lead_id", ids(leadIDs)), eq("campaign_id", bson.D{{Key: "$ne", Value: exceptCampaign}}),
		eq("user_id", userID), eq("state", terminalStates)})
	if err != nil {
		return nil, err
	}
	var rows []clDoc
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	var campaignIDs []int64
	for _, r := range rows {
		campaignIDs = append(campaignIDs, r.CampaignID)
	}
	if len(campaignIDs) == 0 {
		return busy, nil
	}
	cc, err := s.campaigns().Find(ctx, bson.D{eq("_id", ids(campaignIDs)), eq("status", in("running", "paused"))},
		options.Find().SetProjection(bson.D{eq("_id", 1)}))
	if err != nil {
		return nil, err
	}
	var live []campaignDoc
	if err := cc.All(ctx, &live); err != nil {
		return nil, err
	}
	liveSet := map[int64]bool{}
	for _, c := range live {
		liveSet[c.ID] = true
	}
	for _, r := range rows {
		if liveSet[r.CampaignID] {
			busy[r.LeadID] = true
		}
	}
	return busy, nil
}

// Start launches a draft campaign. Every queued lead gets its first send time from the
// account's schedule: now if the window is open, otherwise the next weekday window.
//
// Order matters, because nothing here is one transaction: the due times are written while the
// campaign is still a draft (the worker ignores drafts), and only then is it switched to
// running. A crash in between leaves a harmless draft that can be started again.
func (s *Service) Start(ctx context.Context, userID, id int64) (Campaign, error) {
	d, err := s.doc(ctx, userID, id)
	if err != nil {
		return Campaign{}, err
	}
	if d.Status != StatusDraft {
		return Campaign{}, ErrWrongStatus
	}
	acct, err := s.acc.Get(ctx, userID, d.AccountID)
	if err != nil {
		return Campaign{}, err
	}
	if acct.Status != accounts.StatusConnected {
		return Campaign{}, ErrAccountNotReady
	}
	sched, err := s.acc.LoadSchedule(ctx, d.AccountID)
	if err != nil {
		return Campaign{}, err
	}

	cur, err := s.cls().Find(ctx, bson.D{eq("campaign_id", id)}, options.Find().SetProjection(bson.D{eq("lead_id", 1)}))
	if err != nil {
		return Campaign{}, err
	}
	var members []clDoc
	if err := cur.All(ctx, &members); err != nil {
		return Campaign{}, err
	}
	if len(members) == 0 {
		return Campaign{}, ErrNoLeads
	}
	leadIDs := make([]int64, len(members))
	for i, m := range members {
		leadIDs[i] = m.LeadID
	}
	busy, err := s.busyElsewhere(ctx, userID, id, leadIDs)
	if err != nil {
		return Campaign{}, err
	}
	if len(busy) > 0 {
		return Campaign{}, ErrLeadConflict
	}

	now := s.now()
	if _, err := s.cls().UpdateMany(ctx, bson.D{eq("campaign_id", id), eq("state", "queued")},
		bson.D{{Key: "$set", Value: bson.D{eq("next_action_at", sched.NextSlot(now)), eq("updated_at", now)}}}); err != nil {
		return Campaign{}, err
	}
	if err := s.cas(ctx, userID, id, []Status{StatusDraft}, bson.D{eq("status", StatusRunning), eq("started_at", now)}); err != nil {
		return Campaign{}, err
	}
	return s.Get(ctx, userID, id)
}

// Pause stops sending for a running campaign. Lead states and due times are kept.
func (s *Service) Pause(ctx context.Context, userID, id int64) (Campaign, error) {
	if err := s.cas(ctx, userID, id, []Status{StatusRunning}, bson.D{eq("status", StatusPaused)}); err != nil {
		return Campaign{}, err
	}
	return s.Get(ctx, userID, id)
}

// Resume restarts a paused campaign. Sends that came due while it was paused are moved to
// the next open slot instead of all firing at once. They are rescheduled while the campaign
// is still paused, then it is switched back to running.
func (s *Service) Resume(ctx context.Context, userID, id int64) (Campaign, error) {
	d, err := s.doc(ctx, userID, id)
	if err != nil {
		return Campaign{}, err
	}
	if d.Status != StatusPaused {
		return Campaign{}, ErrWrongStatus
	}
	sched, err := s.acc.LoadSchedule(ctx, d.AccountID)
	if err != nil {
		return Campaign{}, err
	}
	now := s.now()
	if _, err := s.cls().UpdateMany(ctx, bson.D{eq("campaign_id", id), eq("state", terminalStates), {Key: "$or", Value: bson.A{
		bson.D{eq("state", "queued"), eq("next_action_at", nil)},
		bson.D{eq("next_action_at", bson.D{{Key: "$lt", Value: now}})},
	}}}, bson.D{{Key: "$set", Value: bson.D{eq("next_action_at", sched.NextSlot(now)), eq("updated_at", now)}}}); err != nil {
		return Campaign{}, err
	}
	if err := s.cas(ctx, userID, id, []Status{StatusPaused}, bson.D{eq("status", StatusRunning)}); err != nil {
		return Campaign{}, err
	}
	return s.Get(ctx, userID, id)
}

// Stop ends a campaign for good. Leads keep the state they reached (so the funnel stays
// accurate) but nothing more is sent. A stopped campaign cannot be restarted. The status
// changes first, which is what actually stops the worker; clearing the due times is tidying.
func (s *Service) Stop(ctx context.Context, userID, id int64) (Campaign, error) {
	if err := s.cas(ctx, userID, id, []Status{StatusDraft, StatusRunning, StatusPaused}, bson.D{eq("status", StatusStopped)}); err != nil {
		return Campaign{}, err
	}
	if _, err := s.cls().UpdateMany(ctx, bson.D{eq("campaign_id", id), eq("state", terminalStates)},
		bson.D{{Key: "$unset", Value: bson.D{eq("next_action_at", "")}}, {Key: "$set", Value: bson.D{eq("updated_at", s.now())}}}); err != nil {
		return Campaign{}, err
	}
	return s.Get(ctx, userID, id)
}

// mdbTx runs fn in a transaction that first touches the campaign document, so concurrent
// transactions on the same campaign take turns.
func mdbTx(ctx context.Context, s *Service, campaignID int64, fn func(ctx context.Context) error) error {
	return mdb.WithTx(ctx, s.db, func(ctx context.Context) error {
		if err := mdb.Lock(ctx, s.campaigns(), bson.D{eq("_id", campaignID)}); err != nil {
			return err
		}
		return fn(ctx)
	})
}
