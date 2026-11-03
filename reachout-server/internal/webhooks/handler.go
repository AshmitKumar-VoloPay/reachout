package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ashmit/reachout-server/internal/campaigns"
	"github.com/ashmit/reachout-server/internal/httpapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxBody       = 1 << 20 // 1 MiB
	maxMessageLen = 10000   // characters of a stored message
)

// Recorder applies lead events. campaigns.Service implements it.
type Recorder interface {
	Record(ctx context.Context, campaignID, leadID int64, ev campaigns.Event, at time.Time, detail string) (campaigns.State, campaigns.Outcome, error)
}

// Handler is the public endpoint Linked API calls. It has no user session: the signature is
// the only credential, and every event is tied to one of our accounts through its Linked
// API account id before anything is read or changed.
type Handler struct {
	pool   *pgxpool.Pool
	rec    Recorder
	secret string
	log    *slog.Logger
	now    func() time.Time
}

// New returns a handler. With an empty secret every request is refused: an endpoint that
// cannot verify signatures must not accept events.
func New(pool *pgxpool.Pool, rec Recorder, secret string, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{pool: pool, rec: rec, secret: secret, log: log, now: time.Now}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhooks/linkedapi", h.serve)
}

type envelope struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt string          `json:"createdAt"`
	Data      json.RawMessage `json:"data"`
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	if h.secret == "" {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "webhook_disabled", "webhook signing secret is not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		httpapi.WriteError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
		return
	}
	// Verified over the raw bytes, before anything is parsed or stored.
	if !Verify(h.secret, body, r.Header.Get(TimestampHeader), r.Header.Get(SignatureHeader), h.now()) {
		h.log.Warn("webhook rejected: bad signature", "request_id", httpapi.RequestID(r.Context()))
		httpapi.WriteError(w, http.StatusUnauthorized, "invalid_signature", "invalid signature")
		return
	}
	var ev envelope
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" || ev.Type == "" || len(ev.ID) > 200 || len(ev.Type) > 100 {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_event", "malformed event")
		return
	}

	status, err := h.process(r.Context(), ev)
	if err != nil {
		// A 5xx makes Linked API retry the delivery; the event stays unprocessed until it succeeds.
		h.log.Error("webhook processing failed", "event", ev.ID, "type", ev.Type, "err", err, "request_id", httpapi.RequestID(r.Context()))
		httpapi.WriteError(w, http.StatusInternalServerError, "internal", "internal server error")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]string{"status": status})
}

// process handles one event at most once to completion. Delivery is at-least-once, so the
// same event id can arrive again: if the first delivery was fully handled the repeat is
// acknowledged and ignored, and if it was not (it failed part way) it is handled again.
func (h *Handler) process(ctx context.Context, ev envelope) (string, error) {
	now := h.now()
	tag, err := h.pool.Exec(ctx,
		`INSERT INTO webhook_events (event_id, type, received_at) VALUES ($1, $2, $3) ON CONFLICT (event_id) DO NOTHING`,
		ev.ID, ev.Type, now)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		var processed *time.Time
		if err := h.pool.QueryRow(ctx, `SELECT processed_at FROM webhook_events WHERE event_id = $1`, ev.ID).Scan(&processed); err != nil {
			return "", err
		}
		if processed != nil {
			return "duplicate", nil
		}
	}

	detail, err := h.dispatch(ctx, ev, now)
	if err != nil {
		return "", err
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE webhook_events SET processed_at = $1, detail = $2 WHERE event_id = $3`, now, detail, ev.ID); err != nil {
		return "", err
	}
	return "ok", nil
}

func (h *Handler) dispatch(ctx context.Context, ev envelope, now time.Time) (string, error) {
	switch ev.Type {
	case "network.connectionAccepted":
		return h.onAccepted(ctx, ev, now)
	case "inbox.messageReceived":
		return h.onMessageReceived(ctx, ev, now)
	case "account.reconnectionRequired", "account.active", "account.frozen", "account.deleted":
		return h.onAccountStatus(ctx, ev, now)
	}
	// Everything else (workflow results, sent messages, test pings, new event types) is
	// acknowledged so Linked API stops retrying it. We follow workflows by polling.
	return "ignored", nil
}

// account is one of our connected LinkedIn accounts, found from the Linked API account id.
type account struct{ id, userID int64 }

func (h *Handler) accountFor(ctx context.Context, linkedAPIID string) (account, bool, error) {
	if linkedAPIID == "" {
		return account{}, false, nil
	}
	var a account
	err := h.pool.QueryRow(ctx,
		`SELECT id, user_id FROM linkedin_accounts WHERE linkedin_account_id = $1`, linkedAPIID).Scan(&a.id, &a.userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return account{}, false, nil
	}
	return a, err == nil, err
}

// orderingTime reads an event's creation time to decide which of two events is newer. It keeps
// the event's own time, so two events a little ahead of our clock still order correctly,
// but treats a missing, unreadable or implausibly future time (more than an hour ahead) as
// "now", so one bad timestamp cannot make every later event look older.
func orderingTime(s string, now time.Time) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.After(now.Add(time.Hour)) {
		return now
	}
	return t
}

// parseTime reads an RFC 3339 time for scheduling. Missing, unreadable or future times become now, so a
// bad timestamp never schedules anything in the future or in a year 0001.
func parseTime(s string, now time.Time) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.After(now) {
		return now
	}
	return t
}
