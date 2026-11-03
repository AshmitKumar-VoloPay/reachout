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
	"github.com/jackc/pgx/v5"
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

	var leadID int64
	err := h.pool.QueryRow(ctx,
		`SELECT l.id FROM leads l
		  WHERE l.user_id = $1
		    AND EXISTS (SELECT 1 FROM campaign_leads cl JOIN campaigns c ON c.id = cl.campaign_id
		                 WHERE cl.lead_id = l.id AND c.account_id = $2)
		    AND (l.profile_url = ANY($3) OR l.hashed_url = ANY($3) OR ($4 <> '' AND l.linkedin_urn = $4))
		  ORDER BY l.id LIMIT 1`,
		a.userID, a.id, urls, id.urn).Scan(&leadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	// Remember the other forms of the identity, so later events that carry only one of them still match.
	if id.urn != "" || hashed != "" {
		if _, err := h.pool.Exec(ctx,
			`UPDATE leads SET linkedin_urn = coalesce(linkedin_urn, nullif($1, '')), hashed_url = coalesce(hashed_url, nullif($2, ''))
			  WHERE id = $3 AND user_id = $4`, id.urn, hashed, leadID, a.userID); err != nil {
			return 0, false, err
		}
	}
	return leadID, true, nil
}

// record applies ev to the lead in every campaign of this account that includes it.
func (h *Handler) record(ctx context.Context, a account, leadID int64, ev campaigns.Event, at time.Time) (int, error) {
	rows, err := h.pool.Query(ctx,
		`SELECT cl.campaign_id FROM campaign_leads cl JOIN campaigns c ON c.id = cl.campaign_id
		  WHERE cl.lead_id = $1 AND c.account_id = $2 AND cl.user_id = $3 ORDER BY cl.campaign_id`, leadID, a.id, a.userID)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
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
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO messages (lead_id, direction, body, at, external_id, thread_id)
		 VALUES ($1, 'in', $2, $3, $4, nullif($5, '')) ON CONFLICT (lead_id, external_id) WHERE external_id IS NOT NULL DO NOTHING`,
		leadID, body, at, extID, d.ThreadID); err != nil {
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
	tag, err := h.pool.Exec(ctx,
		`UPDATE linkedin_accounts SET status = $1, status_event_at = $2, status_detail = $3
		  WHERE id = $4 AND status NOT IN ('pending','error')
		    AND (status_event_at IS NULL OR status_event_at < $2)`,
		accountStatus[ev.Type], at, "reported by Linked API ("+ev.Type+")", a.id)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "stale or unchanged", nil
	}
	return "account " + accountStatus[ev.Type], nil
}
