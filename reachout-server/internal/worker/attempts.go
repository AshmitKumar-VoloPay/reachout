package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ashmit/reachout-server/internal/campaigns"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/mdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type outKind int

const (
	outSuccess        outKind = iota // the action happened: apply event
	outLimit                         // LinkedIn or a limit pushed back: reschedule the lead
	outRetryLater                    // not done, safe to try again soon
	outLeadFailed                    // permanent problem with this lead
	outUnknown                       // may or may not have been sent: never retry automatically
	outAccountProblem                // the account cannot send until someone fixes it
)

// outcome is what a finished (or failed) attempt means for the lead.
type outcome struct {
	kind        outKind
	event       campaigns.Event // outSuccess
	detail      string
	accountSt   string // outAccountProblem: status to put the account in
	accountWide bool   // outLimit: also hold back every other due send of this kind
	countRetry  bool   // outLimit: count against the lead's retry allowance
}

func stepsOf(b []byte) ([3]struct{ Note, Body string }, error) {
	var raw []struct {
		Note string `json:"note"`
		Body string `json:"body"`
	}
	var out [3]struct{ Note, Body string }
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) != 3 {
		return out, fmt.Errorf("stored campaign steps are not the 3-step sequence")
	}
	for i := range out {
		out[i].Note, out[i].Body = raw[i].Note, raw[i].Body
	}
	return out, nil
}

// resolveOpenAttempts settles every send still in flight: it asks Linked API for results of
// submitted workflows and recovers attempts that were recorded but never submitted.
func (w *Worker) resolveOpenAttempts(ctx context.Context) {
	cur, err := w.attempts().Find(ctx, bson.D{eq("open", true)}, options.Find().SetSort(bson.D{eq("_id", 1)}))
	if err != nil {
		w.log.Error("list open attempts", "err", err)
		return
	}
	var docs []attemptDoc
	if err := cur.All(ctx, &docs); err != nil {
		w.log.Error("list open attempts", "err", err)
		return
	}
	open := make([]attempt, len(docs))
	for i, d := range docs {
		open[i] = d.attempt()
	}

	for _, a := range open {
		switch a.status {
		case statusStarting:
			if w.now().Sub(a.createdAt) < w.cfg.StartingTTL {
				continue // being submitted right now, by this or another process
			}
			w.recoverStarting(ctx, a)
		case statusSubmitted:
			w.pollSubmitted(ctx, a)
		}
	}
}

// recoverStarting handles an attempt that was written down but never confirmed as submitted
// (a crash, or a lost response). A connection request is safe to try again: if it did go
// out, LinkedIn answers "already pending", which we treat as success. A message has no such
// check, so it is never resent: the lead is failed with a note to look at LinkedIn.
func (w *Worker) recoverStarting(ctx context.Context, a attempt) {
	if a.kind == kindConnect {
		w.settle(ctx, a, outcome{kind: outRetryLater, detail: "abandoned before it was submitted"})
		return
	}
	w.settle(ctx, a, outcome{kind: outUnknown, detail: "the " + a.kind + " may or may not have been sent before a restart"})
}

func (w *Worker) pollSubmitted(ctx context.Context, a attempt) {
	token, err := w.acc.IdentificationToken(ctx, a.accountID)
	if err != nil {
		w.log.Error("read token", "attempt", a.id, "err", err)
		return
	}
	res, err := w.li.GetWorkflow(ctx, token, a.workflowID)
	if err != nil {
		w.log.Warn("poll workflow", "attempt", a.id, "err", err) // transient: try again next tick
		w.expireIfStale(ctx, a)
		return
	}
	switch res.Status {
	case "completed":
		w.settle(ctx, a, w.classifyCompletion(a.kind, res.Completion))
	case "failed":
		w.settle(ctx, a, w.classifyFailure(a.kind, res.Failure))
	default: // pending (queued, or parked outside Linked API's own working hours) or running
		w.expireIfStale(ctx, a)
	}
}

// expireIfStale gives up on a workflow that has not produced a result in too long.
func (w *Worker) expireIfStale(ctx context.Context, a attempt) {
	if w.now().Sub(a.createdAt) < w.cfg.AttemptTTL {
		return
	}
	if a.kind == kindConnect {
		w.settle(ctx, a, outcome{kind: outRetryLater, detail: "no result from LinkedIn in time"})
		return
	}
	w.settle(ctx, a, outcome{kind: outUnknown, detail: "no result from LinkedIn in time for the " + a.kind})
}

// classifyStartError decides what a failed StartWorkflow call means. A rejection that came
// back from Linked API means nothing was started. A network failure or 5xx is ambiguous:
// the workflow may exist.
func (w *Worker) classifyStartError(kind string, err error) outcome {
	var le *linkedin.Error
	if !errors.As(err, &le) {
		return w.ambiguous(kind, "unexpected error starting the workflow")
	}
	switch {
	case linkedin.IsSignedOut(err):
		return outcome{kind: outAccountProblem, accountSt: "reconnection_required", detail: "LinkedIn account is signed out"}
	case linkedin.IsNoSeats(err):
		return outcome{kind: outAccountProblem, accountSt: "frozen", detail: "no Linked API seat for this account"}
	case linkedin.IsLimitHit(err):
		return outcome{kind: outLimit, detail: le.Type, accountWide: true}
	case linkedin.IsOutsideWorkingHours(err):
		// Linked API's own window is narrower than ours. Try again at the next window.
		return outcome{kind: outLimit, detail: le.Type}
	case le.Status == 429:
		return outcome{kind: outRetryLater, detail: "Linked API rate limit"}
	case le.Status >= 400 && le.Status < 500:
		return outcome{kind: outLeadFailed, detail: "rejected by Linked API: " + le.Type}
	}
	return w.ambiguous(kind, "Linked API did not confirm the request")
}

func (w *Worker) ambiguous(kind, detail string) outcome {
	if kind == kindConnect {
		return outcome{kind: outRetryLater, detail: detail} // retry is idempotent for requests
	}
	return outcome{kind: outUnknown, detail: detail}
}

func (w *Worker) classifyFailure(kind string, f *linkedin.WorkflowFailure) outcome {
	if f != nil {
		switch f.Reason {
		case "linkedinAccountSignedOut":
			return outcome{kind: outAccountProblem, accountSt: "reconnection_required", detail: "LinkedIn account is signed out"}
		case "languageNotSupported":
			return outcome{kind: outAccountProblem, accountSt: "error", detail: "LinkedIn language is not supported"}
		}
	}
	if kind == kindConnect {
		return outcome{kind: outRetryLater, detail: "the workflow failed"}
	}
	return outcome{kind: outUnknown, detail: "the " + kind + " workflow failed without a clear result"}
}

// classifyCompletion turns a finished workflow's result into an outcome.
func (w *Worker) classifyCompletion(kind string, completion json.RawMessage) outcome {
	var comp struct {
		Success bool `json:"success"`
		Error   *struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(completion, &comp); err != nil {
		return w.ambiguous(kind, "unreadable result from LinkedIn")
	}
	if comp.Success {
		return outcome{kind: outSuccess, event: eventFor(kind)}
	}
	if comp.Error == nil {
		return w.ambiguous(kind, "LinkedIn reported a failure without a reason")
	}
	typ := comp.Error.Type

	if kind == kindConnect {
		switch typ {
		case "alreadyPending": // the request is out already (e.g. a retried send)
			return outcome{kind: outSuccess, event: campaigns.EvRequestSent}
		case "alreadyConnected":
			return outcome{kind: outSuccess, event: campaigns.EvAccepted}
		case "invitationAlreadyReceived":
			return outcome{kind: outLeadFailed, detail: "this person already invited you; accept it on LinkedIn"}
		case "limitExceeded", "requestNotAllowed", "noteLimitExceeded":
			// requestNotAllowed can be a sending limit or something about this person; either
			// way hold back the account's other requests too and count it against this lead.
			return outcome{kind: outLimit, detail: typ, accountWide: true, countRetry: true}
		}
		return outcome{kind: outLeadFailed, detail: "LinkedIn error: " + typ}
	}
	if typ == "limitExceeded" {
		return outcome{kind: outLimit, detail: typ, accountWide: true, countRetry: true}
	}
	return outcome{kind: outLeadFailed, detail: "LinkedIn error: " + typ}
}

// settle applies an outcome. Each branch first closes the attempt, so settling twice (two
// workers, or a retry after a crash) cannot apply an outcome twice.
func (w *Worker) settle(ctx context.Context, a attempt, o outcome) {
	now := w.now()
	if isSync(a.kind) {
		w.settleSync(ctx, a, o, now)
		return
	}
	switch o.kind {
	case outSuccess:
		w.settleSuccess(ctx, a, o, now)
	case outLimit:
		w.settleLimit(ctx, a, o, now)
	case outRetryLater:
		if !w.closeAttempt(ctx, a, statusFailed, o.detail, now) {
			return
		}
		if sched, err := w.acc.LoadSchedule(ctx, a.accountID); err == nil {
			at := w.jitter(sched, now, 5*time.Minute, 15*time.Minute)
			w.setLeadDue(ctx, a, at)
		}
	case outLeadFailed:
		if w.closeAttempt(ctx, a, statusFailed, o.detail, now) {
			w.failLead(ctx, a, o.detail, now)
		}
	case outUnknown:
		if w.closeAttempt(ctx, a, statusUnknown, o.detail, now) {
			w.failLead(ctx, a, "outcome unknown ("+o.detail+"); check LinkedIn before sending again", now)
		}
	case outAccountProblem:
		if w.closeAttempt(ctx, a, statusFailed, o.detail, now) {
			if err := w.acc.MarkNeedsAttention(ctx, a.accountID, o.accountSt, o.detail); err != nil {
				w.log.Error("mark account", "account", a.accountID, "err", err)
			}
		}
	}
}

// settleSync finishes a sync task. It touches no lead. A failure is simply recorded: the
// next try comes after syncRetry, and sending stays off until both syncs have succeeded.
func (w *Worker) settleSync(ctx context.Context, a attempt, o outcome, now time.Time) {
	switch o.kind {
	case outSuccess:
		if !w.closeAttempt(ctx, a, statusSucceeded, "", now) {
			return
		}
		col := "network_synced_at"
		if a.kind == kindSyncInbox {
			col = "inbox_synced_at"
		}
		if _, err := w.accountsColl().UpdateOne(ctx, bson.D{eq("_id", a.accountID)}, bson.D{{Key: "$set", Value: bson.D{eq(col, now)}}}); err != nil {
			w.log.Error("mark synced", "account", a.accountID, "kind", a.kind, "err", err)
			return
		}
		if sched, err := w.acc.LoadSchedule(ctx, a.accountID); err == nil {
			if err := w.acc.SetNotBefore(ctx, a.accountID, w.jitter(sched, now, w.cfg.MinGap, w.cfg.MaxGap)); err != nil {
				w.log.Error("set spacing", "account", a.accountID, "err", err)
			}
		}
		w.log.Info("event feed enabled", "account", a.accountID, "kind", a.kind)
	case outAccountProblem:
		if w.closeAttempt(ctx, a, statusFailed, o.detail, now) {
			if err := w.acc.MarkNeedsAttention(ctx, a.accountID, o.accountSt, o.detail); err != nil {
				w.log.Error("mark account", "account", a.accountID, "err", err)
			}
		}
	default:
		detail := o.detail
		if detail == "" {
			detail = "sync did not complete"
		}
		w.closeAttempt(ctx, a, statusFailed, detail, now)
	}
}

func (w *Worker) settleSuccess(ctx context.Context, a attempt, o outcome, now time.Time) {
	// Record is idempotent, so doing it before closing the attempt is safe if we crash in between.
	_, res, err := w.rec.Record(ctx, a.campaignID, a.leadID, o.event, now, "")
	switch {
	case errors.Is(err, campaigns.ErrNotFound):
		w.closeAttempt(ctx, a, statusFailed, "the lead was removed", now)
		return
	case err != nil:
		w.log.Error("record event", "attempt", a.id, "err", err) // attempt stays open: retried next tick
		return
	case res == campaigns.Invalid:
		w.log.Error("event rejected by state machine", "attempt", a.id, "event", o.event)
	}
	if !w.closeAttempt(ctx, a, statusSucceeded, "", now) {
		return // another process already finished this attempt
	}
	if a.kind != kindConnect {
		idx := 1
		if a.kind == kindFollowUp {
			idx = 2
		}
		w.storeSentMessage(ctx, a, idx, now)
	}
	w.db.Collection(mdb.CampaignLeads).UpdateOne(ctx, bson.D{eq("campaign_id", a.campaignID), eq("lead_id", a.leadID)},
		bson.D{{Key: "$set", Value: bson.D{eq("retry_count", 0)}}})

	// Random gap before the account's next send.
	if sched, err := w.acc.LoadSchedule(ctx, a.accountID); err == nil {
		if err := w.acc.SetNotBefore(ctx, a.accountID, w.jitter(sched, now, w.cfg.MinGap, w.cfg.MaxGap)); err != nil {
			w.log.Error("set spacing", "account", a.accountID, "err", err)
		}
	}
}

// settleLimit reschedules instead of failing: a limit says "not today", not "never".
func (w *Worker) settleLimit(ctx context.Context, a attempt, o outcome, now time.Time) {
	if !w.closeAttempt(ctx, a, statusFailed, "limit: "+o.detail, now) {
		return
	}
	sched, err := w.acc.LoadSchedule(ctx, a.accountID)
	if err != nil {
		w.log.Error("load schedule", "account", a.accountID, "err", err)
		return
	}
	_, dayEnd := sched.DayBounds(now)
	nextDay := sched.NextSlot(dayEnd)

	if o.countRetry {
		var after struct {
			RetryCount int `bson:"retry_count"`
		}
		err := w.db.Collection(mdb.CampaignLeads).FindOneAndUpdate(ctx,
			bson.D{eq("campaign_id", a.campaignID), eq("lead_id", a.leadID)},
			bson.D{{Key: "$inc", Value: bson.D{eq("retry_count", 1)}}},
			options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&after)
		if err == nil && after.RetryCount >= w.cfg.MaxLimitRetries {
			w.failLead(ctx, a, "LinkedIn repeatedly refused this request ("+o.detail+")", now)
		} else {
			w.setLeadDue(ctx, a, nextDay)
		}
	} else {
		w.setLeadDue(ctx, a, nextDay)
	}
	if o.accountWide {
		w.deferDue(ctx, a.accountID, []string{stateFor(a.kind)}, nextDay, now)
	}
	w.log.Warn("send pushed back", "attempt", a.id, "account", a.accountID, "reason", o.detail, "until", nextDay)
}

// closeAttempt moves an open attempt to a final status. It reports whether this call did it.
func (w *Worker) closeAttempt(ctx context.Context, a attempt, status, detail string, now time.Time) bool {
	// Only an open attempt can be closed, and unsetting `open` frees the account's single slot.
	r, err := w.attempts().UpdateOne(ctx, bson.D{eq("_id", a.id), eq("open", true)}, bson.D{
		{Key: "$set", Value: bson.D{eq("status", status), eq("detail", detail), eq("updated_at", now), eq("finished_at", now)}},
		{Key: "$unset", Value: bson.D{eq("open", "")}},
	})
	if err != nil {
		w.log.Error("close attempt", "attempt", a.id, "err", err)
		return false
	}
	return r.ModifiedCount == 1
}

func (w *Worker) failLead(ctx context.Context, a attempt, detail string, now time.Time) {
	if _, _, err := w.rec.Record(ctx, a.campaignID, a.leadID, campaigns.EvFailed, now, detail); err != nil && !errors.Is(err, campaigns.ErrNotFound) {
		w.log.Error("fail lead", "attempt", a.id, "err", err)
	}
}

// setLeadDue changes when the lead is next tried, without touching its state.
func (w *Worker) setLeadDue(ctx context.Context, a attempt, at time.Time) {
	if _, err := w.db.Collection(mdb.CampaignLeads).UpdateOne(ctx,
		bson.D{eq("campaign_id", a.campaignID), eq("lead_id", a.leadID), eq("state", stateFor(a.kind))},
		bson.D{{Key: "$set", Value: bson.D{eq("next_action_at", at), eq("updated_at", w.now())}}}); err != nil {
		w.log.Error("reschedule lead", "attempt", a.id, "err", err)
	}
}

// storeSentMessage keeps the text that was sent in the lead's conversation. The body comes from
// the campaign's step, as it was when it ran. Called once per attempt, by whoever closed it.
func (w *Worker) storeSentMessage(ctx context.Context, a attempt, step int, now time.Time) {
	var camp struct {
		Steps []campaigns.Step `bson:"steps"`
	}
	if err := w.campaignsColl().FindOne(ctx, bson.D{eq("_id", a.campaignID)}).Decode(&camp); err != nil || len(camp.Steps) <= step {
		w.log.Error("store sent message", "attempt", a.id, "err", err)
		return
	}
	id, err := mdb.NextID(ctx, w.db, mdb.Messages)
	if err != nil {
		w.log.Error("store sent message", "attempt", a.id, "err", err)
		return
	}
	if _, err := w.db.Collection(mdb.Messages).InsertOne(ctx, bson.D{eq("_id", id), eq("user_id", a.userID), eq("lead_id", a.leadID),
		eq("direction", "out"), eq("body", camp.Steps[step].Body), eq("at", now)}); err != nil {
		w.log.Error("store sent message", "attempt", a.id, "err", err)
	}
}
