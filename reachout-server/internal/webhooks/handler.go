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
	"github.com/ashmit/reachout-server/internal/mdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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
	db     *mongo.Database
	rec    Recorder
	secret string
	log    *slog.Logger
	now    func() time.Time
}

// New returns a handler. With an empty secret every request is refused: an endpoint that
// cannot verify signatures must not accept events.
func New(db *mongo.Database, rec Recorder, secret string, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{db: db, rec: rec, secret: secret, log: log, now: time.Now}
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
	events := h.db.Collection(mdb.WebhookEvents)
	// The event id is the document id, so two deliveries of one event cannot both insert it.
	_, err := events.InsertOne(ctx, bson.D{eq("_id", ev.ID), eq("type", ev.Type), eq("received_at", now), eq("detail", "")})
	if err != nil {
		if !mdb.IsDup(err) {
			return "", err
		}
		var seen struct {
			ProcessedAt *time.Time `bson:"processed_at"`
		}
		if err := events.FindOne(ctx, bson.D{eq("_id", ev.ID)}).Decode(&seen); err != nil {
			return "", err
		}
		if seen.ProcessedAt != nil {
			return "duplicate", nil
		}
	}

	detail, err := h.dispatch(ctx, ev, now)
	if err != nil {
		return "", err
	}
	if _, err := events.UpdateOne(ctx, bson.D{eq("_id", ev.ID)},
		bson.D{{Key: "$set", Value: bson.D{eq("processed_at", now), eq("detail", detail)}}}); err != nil {
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
	var d struct {
		ID     int64 `bson:"_id"`
		UserID int64 `bson:"user_id"`
	}
	err := h.db.Collection(mdb.Accounts).FindOne(ctx, bson.D{eq("linkedin_account_id", linkedAPIID)},
		options.FindOne().SetProjection(bson.D{eq("_id", 1), eq("user_id", 1)})).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return account{}, false, nil
	}
	return account{id: d.ID, userID: d.UserID}, err == nil, err
}

func eq(k string, v any) bson.E { return bson.E{Key: k, Value: v} }

func idsIn(vals []int64) bson.D {
	a := make(bson.A, len(vals))
	for i, v := range vals {
		a[i] = v
	}
	return bson.D{{Key: "$in", Value: a}}
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
