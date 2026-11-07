package campaigns

import (
	"context"
	"errors"
	"time"

	"github.com/ashmit/reachout-server/internal/schedule"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// CampaignLead is one lead's progress inside a campaign.
type CampaignLead struct {
	LeadID       int64      `json:"lead_id"`
	ProfileURL   string     `json:"profile_url"`
	Name         string     `json:"name"`
	Headline     string     `json:"headline"`
	State        State      `json:"state"`
	CurrentStep  int        `json:"current_step"`
	NextActionAt *time.Time `json:"next_action_at,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type LeadsPage struct {
	Leads  []CampaignLead `json:"leads"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

// AddResult says what happened to each requested lead id.
type AddResult struct {
	Added      int `json:"added"`
	Duplicates int `json:"duplicates"` // already in this campaign
	NotFound   int `json:"not_found"`  // unknown id or not the user's lead
	Conflicts  int `json:"conflicts"`  // already being worked in another active campaign
	OverCap    int `json:"over_cap"`   // campaign is full
}

// clDoc is one lead's place in one campaign (the campaign_leads collection).
type clDoc struct {
	ID           bson.ObjectID `bson:"_id,omitempty"`
	CampaignID   int64         `bson:"campaign_id"`
	LeadID       int64         `bson:"lead_id"`
	UserID       int64         `bson:"user_id"`
	State        State         `bson:"state"`
	CurrentStep  int           `bson:"current_step"` // furthest stage reached, never decreases
	NextActionAt *time.Time    `bson:"next_action_at,omitempty"`
	LastError    *string       `bson:"last_error,omitempty"`
	UpdatedAt    time.Time     `bson:"updated_at"`
	RetryCount   int           `bson:"retry_count"`
}

// AddLeads puts the user's leads into a draft, running or paused campaign. Leads added to
// a running campaign are scheduled right away; in a draft they wait for Start.
func (s *Service) AddLeads(ctx context.Context, userID, campaignID int64, leadIDs []int64) (AddResult, error) {
	var res AddResult
	if len(leadIDs) == 0 || len(leadIDs) > MaxAddPerRequest {
		return res, bad("send between 1 and %d lead ids", MaxAddPerRequest)
	}
	seen := map[int64]bool{}
	var idList []int64
	for _, id := range leadIDs {
		if !seen[id] {
			seen[id] = true
			idList = append(idList, id)
		}
	}

	d, err := s.doc(ctx, userID, campaignID)
	if err != nil {
		return res, err
	}
	sched, err := s.acc.LoadSchedule(ctx, d.AccountID) // loaded before the transaction opens
	if err != nil {
		return res, err
	}

	// A transaction that first touches the campaign document makes concurrent adds to the same
	// campaign take turns, so the size limit and the duplicate checks hold. The body may run
	// again after a conflict, so it starts from a clean result each time.
	err = mdbTx(ctx, s, campaignID, func(ctx context.Context) error {
		res = AddResult{}
		var cur campaignDoc
		if err := s.campaigns().FindOne(ctx, bson.D{eq("_id", campaignID), eq("user_id", userID)}).Decode(&cur); err != nil {
			return err
		}
		if cur.Status != StatusDraft && cur.Status != StatusRunning && cur.Status != StatusPaused {
			return ErrWrongStatus
		}

		// Only leads that belong to this user.
		lc, err := s.db.Collection("leads").Find(ctx, bson.D{eq("_id", ids(idList)), eq("user_id", userID)},
			options.Find().SetProjection(bson.D{eq("_id", 1)}))
		if err != nil {
			return err
		}
		var ownedDocs []struct {
			ID int64 `bson:"_id"`
		}
		if err := lc.All(ctx, &ownedDocs); err != nil {
			return err
		}
		owned := map[int64]bool{}
		for _, o := range ownedDocs {
			owned[o.ID] = true
		}

		mc, err := s.cls().Find(ctx, bson.D{eq("campaign_id", campaignID), eq("lead_id", ids(idList))})
		if err != nil {
			return err
		}
		var members []clDoc
		if err := mc.All(ctx, &members); err != nil {
			return err
		}
		inCampaign := map[int64]bool{}
		for _, m := range members {
			inCampaign[m.LeadID] = true
		}

		busy, err := s.busyElsewhere(ctx, userID, campaignID, idList)
		if err != nil {
			return err
		}
		total, err := s.cls().CountDocuments(ctx, bson.D{eq("campaign_id", campaignID)})
		if err != nil {
			return err
		}

		var next *time.Time
		if cur.Status == StatusRunning {
			t := sched.NextSlot(s.now())
			next = &t
		}
		now := s.now()
		var docs []any
		for _, id := range idList {
			switch {
			case !owned[id]:
				res.NotFound++
			case inCampaign[id]:
				res.Duplicates++
			case busy[id]:
				res.Conflicts++
			case int(total) >= MaxCampaignLeads:
				res.OverCap++
			default:
				docs = append(docs, clDoc{CampaignID: campaignID, LeadID: id, UserID: userID, State: StateQueued,
					NextActionAt: next, UpdatedAt: now})
				res.Added++
				total++
			}
		}
		if len(docs) > 0 {
			if _, err := s.cls().InsertMany(ctx, docs); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}

// RemoveLead takes a lead out of a campaign. Only leads that have not been contacted yet
// (state queued) can be removed; for anyone else, stop the campaign instead.
func (s *Service) RemoveLead(ctx context.Context, userID, campaignID, leadID int64) error {
	r, err := s.cls().DeleteOne(ctx, bson.D{eq("campaign_id", campaignID), eq("lead_id", leadID), eq("user_id", userID), eq("state", "queued")})
	if err != nil {
		return err
	}
	if r.DeletedCount == 1 {
		return nil
	}
	n, err := s.cls().CountDocuments(ctx, bson.D{eq("campaign_id", campaignID), eq("lead_id", leadID), eq("user_id", userID)})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return ErrLeadNotRemovable
}

// ListLeads pages through a campaign's leads, optionally filtered by state.
func (s *Service) ListLeads(ctx context.Context, userID, campaignID int64, state State, limit, offset int) (LeadsPage, error) {
	if _, err := s.doc(ctx, userID, campaignID); err != nil { // ownership check, 404 otherwise
		return LeadsPage{}, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	filter := bson.D{eq("campaign_id", campaignID), eq("user_id", userID)}
	if state != "" {
		filter = append(filter, eq("state", string(state)))
	}
	page := LeadsPage{Leads: []CampaignLead{}, Limit: limit, Offset: offset}
	total, err := s.cls().CountDocuments(ctx, filter)
	if err != nil {
		return LeadsPage{}, err
	}
	page.Total = int(total)

	cur, err := s.cls().Find(ctx, filter, options.Find().SetSort(bson.D{eq("lead_id", 1)}).SetSkip(int64(offset)).SetLimit(int64(limit)))
	if err != nil {
		return LeadsPage{}, err
	}
	var rows []clDoc
	if err := cur.All(ctx, &rows); err != nil {
		return LeadsPage{}, err
	}
	leadIDs := make([]int64, len(rows))
	for i, r := range rows {
		leadIDs[i] = r.LeadID
	}
	lc, err := s.db.Collection("leads").Find(ctx, bson.D{eq("_id", ids(leadIDs)), eq("user_id", userID)})
	if err != nil {
		return LeadsPage{}, err
	}
	var leadDocs []struct {
		ID         int64  `bson:"_id"`
		ProfileURL string `bson:"profile_url"`
		Name       string `bson:"name"`
		Headline   string `bson:"headline"`
	}
	if err := lc.All(ctx, &leadDocs); err != nil {
		return LeadsPage{}, err
	}
	info := map[int64]int{}
	for i, l := range leadDocs {
		info[l.ID] = i
	}
	for _, r := range rows {
		cl := CampaignLead{LeadID: r.LeadID, State: r.State, CurrentStep: r.CurrentStep, NextActionAt: r.NextActionAt, UpdatedAt: r.UpdatedAt}
		if r.LastError != nil {
			cl.LastError = *r.LastError
		}
		if i, ok := info[r.LeadID]; ok {
			cl.ProfileURL, cl.Name, cl.Headline = leadDocs[i].ProfileURL, leadDocs[i].Name, leadDocs[i].Headline
		}
		page.Leads = append(page.Leads, cl)
	}
	return page, nil
}

// Record applies an event to one lead. It is the single entry point the worker (sends) and
// the webhook handler (acceptances, replies) use, so every state change goes through Apply
// and the next send time always comes from the account's schedule.
//
// It is not scoped by user: callers act for an account and must pass ids they resolved
// themselves. Concurrent events for one lead are safe: each update only applies if the lead
// is still in the state it was read in, otherwise the decision is made again from the new
// state. Duplicate or late events return Noop; an impossible event returns Invalid and
// changes nothing.
func (s *Service) Record(ctx context.Context, campaignID, leadID int64, ev Event, at time.Time, detail string) (State, Outcome, error) {
	var camp campaignDoc
	err := s.campaigns().FindOne(ctx, bson.D{eq("_id", campaignID)}).Decode(&camp)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", Invalid, ErrNotFound
	}
	if err != nil {
		return "", Invalid, err
	}
	days, needsSchedule, err := dueDays(camp.Steps, ev)
	if err != nil {
		return "", Invalid, err
	}
	var sched schedule.Schedule
	if needsSchedule {
		if sched, err = s.acc.LoadSchedule(ctx, camp.AccountID); err != nil {
			return "", Invalid, err
		}
	}

	for attempt := 0; attempt < 8; attempt++ {
		var cur clDoc
		err := s.cls().FindOne(ctx, bson.D{eq("campaign_id", campaignID), eq("lead_id", leadID)}).Decode(&cur)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return "", Invalid, ErrNotFound
		}
		if err != nil {
			return "", Invalid, err
		}

		next, out := Apply(cur.State, ev)
		if out != Applied {
			return cur.State, out, nil
		}

		set := bson.D{eq("state", next), eq("current_step", nextStep(cur.CurrentStep, next)), eq("updated_at", s.now())}
		if next == StateFailed {
			set = append(set, eq("last_error", detail))
		}
		update := bson.D{}
		// When the worker should act next: counted in business days on the account's schedule,
		// always inside a working window. None means waiting on something external, or finished.
		if needsSchedule && camp.Status != StatusStopped && !IsTerminal(next) {
			due := sched.AddBusinessDays(at, days)
			if due.IsZero() {
				return cur.State, Invalid, errors.New("schedule has no open day")
			}
			set = append(set, eq("next_action_at", due))
		} else {
			update = append(update, bson.E{Key: "$unset", Value: bson.D{eq("next_action_at", "")}})
		}
		update = append(update, bson.E{Key: "$set", Value: set})

		r, err := s.cls().UpdateOne(ctx, bson.D{eq("_id", cur.ID), eq("state", cur.State), eq("current_step", cur.CurrentStep)}, update)
		if err != nil {
			return cur.State, Invalid, err
		}
		if r.MatchedCount == 0 {
			continue // another event got there first: decide again from the new state
		}
		// The campaign is finished once nobody is left in progress.
		if IsTerminal(next) {
			s.finishIfDone(ctx, campaignID)
		}
		return next, Applied, nil
	}
	return "", Invalid, errors.New("could not apply the event: the lead kept changing")
}

func (s *Service) finishIfDone(ctx context.Context, campaignID int64) {
	n, err := s.cls().CountDocuments(ctx, bson.D{eq("campaign_id", campaignID), eq("state", terminalStates)})
	if err != nil || n > 0 {
		return
	}
	if _, err := s.campaigns().UpdateOne(ctx, bson.D{eq("_id", campaignID), eq("status", StatusRunning)},
		bson.D{{Key: "$set", Value: bson.D{eq("status", StatusDone)}}}); err != nil {
		s.log.Warn("finish campaign", "campaign", campaignID, "err", err)
	}
}

// dueDays is how many business days to wait after ev before the next action, and whether
// ev schedules a next action at all. A sent request waits for the acceptance event instead.
func dueDays(steps []Step, ev Event) (days int, scheduled bool, err error) {
	if len(steps) != 3 {
		return 0, false, errors.New("stored campaign is not the 3-step sequence")
	}
	switch ev {
	case EvAccepted:
		return steps[1].DelayDays, true, nil // wait before the first message
	case EvMessageSent, EvFollowUpSent:
		return steps[2].DelayDays, true, nil // silence before the follow-up, then before marking done
	}
	return 0, false, nil
}
