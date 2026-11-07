package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ashmit/reachout-server/internal/campaigns"
	"github.com/ashmit/reachout-server/internal/leads"
	"github.com/ashmit/reachout-server/internal/mdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// identity is how an event names a person. Any of the three may be missing.
type identity struct{ publicURL, hashedURL, urn string }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// findLead returns the user's lead that this account has campaign dealings with and that the
// event is about. Matching is limited to the account's own user and campaigns: an event for
// one user's account can never touch another user's leads.
func (h *Handler) findLead(ctx context.Context, a account, id identity) (int64, bool, error) {
	var urls []string
	var hashed string
	for _, raw := range []string{id.publicURL, id.hashedURL} {
		if raw == "" {
			continue
		}
		if n, err := leads.NormalizeProfileURL(raw); err == nil {
			urls = append(urls, n)
		}
	}
	if id.hashedURL != "" {
		if n, err := leads.NormalizeProfileURL(id.hashedURL); err == nil {
			hashed = n
		}
	}
	if len(urls) == 0 && id.urn == "" {
		return 0, false, nil
	}

	// Only campaigns of this account count, and only this user's leads can match.
	campaignIDs, err := h.accountCampaigns(ctx, a)
	if err != nil || len(campaignIDs) == 0 {
		return 0, false, err
	}
	match := bson.A{}
	if len(urls) > 0 {
		urlVals := make(bson.A, len(urls))
		for i, u := range urls {
			urlVals[i] = u
		}
		match = append(match, bson.D{eq("profile_url", bson.D{{Key: "$in", Value: urlVals}})},
			bson.D{eq("hashed_url", bson.D{{Key: "$in", Value: urlVals}})})
	}
	if id.urn != "" {
		match = append(match, bson.D{eq("linkedin_urn", id.urn)})
	}
	cur, err := h.db.Collection(mdb.Leads).Find(ctx, bson.D{eq("user_id", a.userID), {Key: "$or", Value: match}},
		options.Find().SetSort(bson.D{eq("_id", 1)}).SetProjection(bson.D{eq("_id", 1)}))
	if err != nil {
		return 0, false, err
	}
	var cands []struct {
		ID int64 `bson:"_id"`
	}
	if err := cur.All(ctx, &cands); err != nil {
		return 0, false, err
	}
	leadID, found := int64(0), false
	for _, c := range cands { // the first (oldest) one that this account has dealt with
		n, err := h.db.Collection(mdb.CampaignLeads).CountDocuments(ctx, bson.D{eq("lead_id", c.ID), eq("campaign_id", idsIn(campaignIDs))})
		if err != nil {
			return 0, false, err
		}
		if n > 0 {
			leadID, found = c.ID, true
			break
		}
	}
	if !found {
		return 0, false, nil
	}
	// Remember the other forms of the identity, so later events that carry only one of them
	// still match. Only blanks are filled; a stored value is never overwritten.
	leadsColl := h.db.Collection(mdb.Leads)
	for field, v := range map[string]string{"linkedin_urn": id.urn, "hashed_url": hashed} {
		if v == "" {
			continue
		}
		if _, err := leadsColl.UpdateOne(ctx, bson.D{eq("_id", leadID), eq("user_id", a.userID), eq(field, bson.D{{Key: "$exists", Value: false}})},
			bson.D{{Key: "$set", Value: bson.D{eq(field, v)}}}); err != nil {
			return 0, false, err
		}
	}
	return leadID, true, nil
}

// accountCampaigns lists the ids of this account's campaigns.
func (h *Handler) accountCampaigns(ctx context.Context, a account) ([]int64, error) {
	cur, err := h.db.Collection(mdb.Campaigns).Find(ctx, bson.D{eq("account_id", a.id), eq("user_id", a.userID)},
		options.Find().SetProjection(bson.D{eq("_id", 1)}))
	if err != nil {
		return nil, err
	}
	var docs []struct {
		ID int64 `bson:"_id"`
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]int64, len(docs))
	for i, d := range docs {
		out[i] = d.ID
	}
	return out, nil
}

// record applies ev to the lead in every campaign of this account that includes it.
func (h *Handler) record(ctx context.Context, a account, leadID int64, ev campaigns.Event, at time.Time) (int, error) {
	campaignIDs, err := h.accountCampaigns(ctx, a)
	if err != nil || len(campaignIDs) == 0 {
		return 0, err
	}
	cur, err := h.db.Collection(mdb.CampaignLeads).Find(ctx,
		bson.D{eq("lead_id", leadID), eq("user_id", a.userID), eq("campaign_id", idsIn(campaignIDs))},
		options.Find().SetSort(bson.D{eq("campaign_id", 1)}).SetProjection(bson.D{eq("campaign_id", 1)}))
	if err != nil {
		return 0, err
	}
	var rows []struct {
		CampaignID int64 `bson:"campaign_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return 0, err
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.CampaignID
	}
	applied := 0
	for _, cid := range ids {
		_, out, err := h.rec.Record(ctx, cid, leadID, ev, at, "")
		if errors.Is(err, campaigns.ErrNotFound) {
			continue
		}
		if err != nil {
			return applied, err
		}
		if out == campaigns.Applied {
			applied++
		}
	}
	return applied, nil
}

func (h *Handler) onAccepted(ctx context.Context, ev envelope, now time.Time) (string, error) {
	var d struct {
		AccountID  string  `json:"accountId"`
		PersonURN  *string `json:"personUrn"`
		PersonURL  *string `json:"personUrl"`
		DetectedAt string  `json:"detectedAt"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return "malformed", nil // retrying cannot fix it
	}
	a, ok, err := h.accountFor(ctx, d.AccountID)
	if err != nil || !ok {
		return "unknown account", err
	}
	// personUrl may be a public or a hashed profile URL: try it as both.
	person := deref(d.PersonURL)
	leadID, found, err := h.findLead(ctx, a, identity{publicURL: person, hashedURL: person, urn: deref(d.PersonURN)})
	if err != nil || !found {
		return "no matching lead", err
	}
	n, err := h.record(ctx, a, leadID, campaigns.EvAccepted, parseTime(d.DetectedAt, now))
	if err != nil {
		return "", err
	}
	return recorded(n), nil
}

func (h *Handler) onMessageReceived(ctx context.Context, ev envelope, now time.Time) (string, error) {
	var d struct {
		AccountID       string  `json:"accountId"`
		ThreadID        string  `json:"threadId"`
		PersonURN       *string `json:"personUrn"`
		PersonHashedURL *string `json:"personHashedUrl"`
		PersonPublicURL *string `json:"personPublicUrl"`
		MessageID       string  `json:"messageId"`
		Sender          string  `json:"sender"`
		Text            string  `json:"text"`
		Time            string  `json:"time"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return "malformed", nil
	}
	if d.Sender != "them" { // our own messages are recorded when the worker sends them
		return "not an inbound message", nil
	}
	a, ok, err := h.accountFor(ctx, d.AccountID)
	if err != nil || !ok {
		return "unknown account", err
	}
	leadID, found, err := h.findLead(ctx, a, identity{
		publicURL: deref(d.PersonPublicURL), hashedURL: deref(d.PersonHashedURL), urn: deref(d.PersonURN)})
	if err != nil || !found {
		return "no matching lead", err // someone who is not one of our leads: not stored
	}
	at := parseTime(d.Time, now)

	body := strings.TrimSpace(d.Text)
	if utf8.RuneCountInString(body) > maxMessageLen {
		body = string([]rune(body)[:maxMessageLen])
	}
	if body == "" {
		body = "(no text)"
	}
	// Stored once per LinkedIn message id even if it arrives in several events.
	extID := d.MessageID
	if extID == "" {
		extID = "event:" + ev.ID
	}
	msgID, err := mdb.NextID(ctx, h.db, mdb.Messages)
	if err != nil {
		return "", err
	}
	msg := bson.D{eq("_id", msgID), eq("user_id", a.userID), eq("lead_id", leadID), eq("direction", "in"), eq("body", body), eq("at", at), eq("external_id", extID)}
	if d.ThreadID != "" {
		msg = append(msg, eq("thread_id", d.ThreadID))
	}
	// The unique index on (lead, external id) makes a repeat of the same message a no-op.
	if _, err := h.db.Collection(mdb.Messages).InsertOne(ctx, msg); err != nil && !mdb.IsDup(err) {
		return "", err
	}
	// A reply ends the sequence for this person, whatever step they were on.
	n, err := h.record(ctx, a, leadID, campaigns.EvReplied, at)
	if err != nil {
		return "", err
	}
	return "reply stored; " + recorded(n), nil
}

func recorded(n int) string {
	if n == 0 {
		return "no state change"
	}
	return "state updated"
}

// accountStatus maps a Linked API account event to our status.
var accountStatus = map[string]string{
	"account.reconnectionRequired": "reconnection_required",
	"account.active":               "connected",
	"account.frozen":               "frozen",
	"account.deleted":              "disconnected",
}

func (h *Handler) onAccountStatus(ctx context.Context, ev envelope, now time.Time) (string, error) {
	var d struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return "malformed", nil
	}
	a, ok, err := h.accountFor(ctx, d.AccountID)
	if err != nil || !ok {
		return "unknown account", err
	}
	// Events can arrive out of order, so only a newer one than the last applied may change
	// the status. Accounts that are still connecting, or failed to connect, are left alone.
	at := orderingTime(ev.CreatedAt, now)
	res, err := h.db.Collection(mdb.Accounts).UpdateOne(ctx, bson.D{
		eq("_id", a.id), eq("status", bson.D{{Key: "$nin", Value: bson.A{"pending", "error"}}}),
		{Key: "$or", Value: bson.A{bson.D{eq("status_event_at", nil)}, bson.D{eq("status_event_at", bson.D{{Key: "$lt", Value: at}})}}},
	}, bson.D{{Key: "$set", Value: bson.D{eq("status", accountStatus[ev.Type]), eq("status_event_at", at),
		eq("status_detail", "reported by Linked API ("+ev.Type+")")}}})
	if err != nil {
		return "", err
	}
	if res.MatchedCount == 0 {
		return "stale or unchanged", nil
	}
	return "account " + accountStatus[ev.Type], nil
}
