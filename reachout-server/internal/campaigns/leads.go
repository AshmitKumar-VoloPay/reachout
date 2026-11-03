package campaigns

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ashmit/reachout-server/internal/schedule"
	"github.com/jackc/pgx/v5"
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

// AddLeads puts the user's leads into a draft, running or paused campaign. Leads added to
// a running campaign are scheduled right away; in a draft they wait for Start.
func (s *Service) AddLeads(ctx context.Context, userID, campaignID int64, leadIDs []int64) (AddResult, error) {
	var res AddResult
	if len(leadIDs) == 0 || len(leadIDs) > MaxAddPerRequest {
		return res, bad("send between 1 and %d lead ids", MaxAddPerRequest)
	}
	seen := map[int64]bool{}
	var ids []int64
	for _, id := range leadIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	// The schedule comes from another connection, so load it before the transaction opens.
	accountID, err := s.accountOf(ctx, userID, campaignID)
	if err != nil {
		return res, err
	}
	sched, err := s.acc.LoadSchedule(ctx, accountID)
	if err != nil {
		return res, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)

	status, lockedAccount, err := s.lock(ctx, tx, userID, campaignID)
	if err != nil {
		return res, err
	}
	if lockedAccount != accountID || (status != StatusDraft && status != StatusRunning && status != StatusPaused) {
		return res, ErrWrongStatus
	}

	// Only leads that belong to this user, in the order requested.
	owned := map[int64]bool{}
	rows, err := tx.Query(ctx, `SELECT id FROM leads WHERE user_id = $1 AND id = ANY($2)`, userID, ids)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return res, err
		}
		owned[id] = true
	}
	rows.Close()

	inCampaign := map[int64]bool{}
	if rows, err = tx.Query(ctx, `SELECT lead_id FROM campaign_leads WHERE campaign_id = $1 AND lead_id = ANY($2)`, campaignID, ids); err != nil {
		return res, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return res, err
		}
		inCampaign[id] = true
	}
	rows.Close()

	busy := map[int64]bool{}
	if rows, err = tx.Query(ctx,
		`SELECT DISTINCT o.lead_id FROM campaign_leads o JOIN campaigns oc ON oc.id = o.campaign_id
		  WHERE o.lead_id = ANY($1) AND o.campaign_id <> $2 AND oc.user_id = $3
		    AND oc.status IN ('running','paused') AND o.state NOT IN `+terminalStates, ids, campaignID, userID); err != nil {
		return res, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return res, err
		}
		busy[id] = true
	}
	rows.Close()

	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM campaign_leads WHERE campaign_id = $1`, campaignID).Scan(&total); err != nil {
		return res, err
	}

	var next *time.Time
	if status == StatusRunning {
		t := sched.NextSlot(s.now())
		next = &t
	}

	for _, id := range ids {
		switch {
		case !owned[id]:
			res.NotFound++
		case inCampaign[id]:
			res.Duplicates++
		case busy[id]:
			res.Conflicts++
		case total >= MaxCampaignLeads:
			res.OverCap++
		default:
			if _, err := tx.Exec(ctx,
				`INSERT INTO campaign_leads (campaign_id, lead_id, user_id, next_action_at) VALUES ($1, $2, $3, $4)`,
				campaignID, id, userID, next); err != nil {
				return res, err
			}
			res.Added++
			total++
		}
	}
	return res, tx.Commit(ctx)
}

// RemoveLead takes a lead out of a campaign. Only leads that have not been contacted yet
// (state queued) can be removed; for anyone else, stop the campaign instead.
func (s *Service) RemoveLead(ctx context.Context, userID, campaignID, leadID int64) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM campaign_leads WHERE campaign_id = $1 AND lead_id = $2 AND user_id = $3 AND state = 'queued'`,
		campaignID, leadID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM campaign_leads WHERE campaign_id = $1 AND lead_id = $2 AND user_id = $3)`,
		campaignID, leadID, userID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return ErrLeadNotRemovable
}

// ListLeads pages through a campaign's leads, optionally filtered by state.
func (s *Service) ListLeads(ctx context.Context, userID, campaignID int64, state State, limit, offset int) (LeadsPage, error) {
	if _, err := s.Get(ctx, userID, campaignID); err != nil { // ownership check, 404 otherwise
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
	page := LeadsPage{Leads: []CampaignLead{}, Limit: limit, Offset: offset}
	st := string(state) // empty means all states
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM campaign_leads WHERE campaign_id = $1 AND user_id = $2 AND ($3 = '' OR state = $3)`,
		campaignID, userID, st).Scan(&page.Total); err != nil {
		return LeadsPage{}, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT l.id, l.profile_url, l.name, l.headline, cl.state, cl.current_step, cl.next_action_at,
		        coalesce(cl.last_error, ''), cl.updated_at
		   FROM campaign_leads cl JOIN leads l ON l.id = cl.lead_id
		  WHERE cl.campaign_id = $1 AND cl.user_id = $2 AND ($3 = '' OR cl.state = $3)
		  ORDER BY l.id LIMIT $4 OFFSET $5`, campaignID, userID, st, limit, offset)
	if err != nil {
		return LeadsPage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var l CampaignLead
		if err := rows.Scan(&l.LeadID, &l.ProfileURL, &l.Name, &l.Headline, &l.State, &l.CurrentStep,
			&l.NextActionAt, &l.LastError, &l.UpdatedAt); err != nil {
			return LeadsPage{}, err
		}
		page.Leads = append(page.Leads, l)
	}
	return page, rows.Err()
}

// Record applies an event to one lead. It is the single entry point the worker (sends) and
// the webhook handler (acceptances, replies) use, so every state change goes through Apply
// and the next send time always comes from the account's schedule.
//
// It is not scoped by user: callers act for an account and must pass ids they resolved
// themselves. The row lock makes concurrent events for one lead apply one at a time.
// Duplicate or late events return Noop; an impossible event returns Invalid and changes nothing.
func (s *Service) Record(ctx context.Context, campaignID, leadID int64, ev Event, at time.Time, detail string) (State, Outcome, error) {
	// Steps and the account schedule are read before the transaction opens: loading the
	// schedule needs another connection, and a transaction holding a row lock while it
	// waits for one can deadlock the pool when many events arrive together. Steps never
	// change once a campaign has started, so reading them early is safe.
	var accountID int64
	var stepsJSON []byte
	err := s.pool.QueryRow(ctx, `SELECT account_id, steps FROM campaigns WHERE id = $1`, campaignID).Scan(&accountID, &stepsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", Invalid, ErrNotFound
	}
	if err != nil {
		return "", Invalid, err
	}
	days, needsSchedule, err := dueDays(stepsJSON, ev)
	if err != nil {
		return "", Invalid, err
	}
	var sched schedule.Schedule
	if needsSchedule {
		if sched, err = s.acc.LoadSchedule(ctx, accountID); err != nil {
			return "", Invalid, err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", Invalid, err
	}
	defer tx.Rollback(ctx)

	var cur State
	var step int
	var status Status
	err = tx.QueryRow(ctx,
		`SELECT cl.state, cl.current_step, c.status
		   FROM campaign_leads cl JOIN campaigns c ON c.id = cl.campaign_id
		  WHERE cl.campaign_id = $1 AND cl.lead_id = $2 FOR UPDATE OF cl`, campaignID, leadID).
		Scan(&cur, &step, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", Invalid, ErrNotFound
	}
	if err != nil {
		return "", Invalid, err
	}

	next, out := Apply(cur, ev)
	if out != Applied {
		return cur, out, nil
	}

	// When the worker should act next: counted in business days on the account's schedule,
	// always inside a working window. nil means waiting on something external or finished.
	var due *time.Time
	if needsSchedule && status != StatusStopped && !IsTerminal(next) {
		t := sched.AddBusinessDays(at, days)
		if t.IsZero() {
			return cur, Invalid, fmt.Errorf("schedule has no open day")
		}
		due = &t
	}
	var lastErr any
	if next == StateFailed {
		lastErr = detail
	}
	if _, err := tx.Exec(ctx,
		`UPDATE campaign_leads
		    SET state = $1, current_step = $2, next_action_at = $3, updated_at = now(),
		        last_error = coalesce($4, last_error)
		  WHERE campaign_id = $5 AND lead_id = $6`,
		next, nextStep(step, next), due, lastErr, campaignID, leadID); err != nil {
		return cur, Invalid, err
	}
	// The campaign is finished once nobody is left in progress.
	if IsTerminal(next) {
		if _, err := tx.Exec(ctx,
			`UPDATE campaigns SET status = 'done'
			  WHERE id = $1 AND status = 'running'
			    AND NOT EXISTS (SELECT 1 FROM campaign_leads WHERE campaign_id = $1 AND state NOT IN `+terminalStates+`)`,
			campaignID); err != nil {
			return cur, Invalid, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return cur, Invalid, err
	}
	return next, Applied, nil
}

// dueDays is how many business days to wait after ev before the next action, and whether
// ev schedules a next action at all. A sent request waits for the acceptance event instead.
func dueDays(stepsJSON []byte, ev Event) (days int, scheduled bool, err error) {
	var steps []Step
	if err := unmarshalSteps(stepsJSON, &steps); err != nil {
		return 0, false, err
	}
	switch ev {
	case EvAccepted:
		return steps[1].DelayDays, true, nil // wait before the first message
	case EvMessageSent, EvFollowUpSent:
		return steps[2].DelayDays, true, nil // silence before the follow-up, then before marking done
	}
	return 0, false, nil
}
