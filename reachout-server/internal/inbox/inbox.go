// Package inbox is the reply inbox: the people who answered, newest first, and the
// conversation with each. Everything is scoped to the signed-in user through the lead's owner.
package inbox

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/ashmit/reachout-server/internal/auth"
	"github.com/ashmit/reachout-server/internal/httpapi"
	"github.com/ashmit/reachout-server/internal/mdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var ErrNotFound = errors.New("conversation not found")

const (
	DefaultPage = 50
	MaxPage     = 200
	snippetLen  = 160
)

type Service struct{ db *mongo.Database }

func NewService(db *mongo.Database) *Service { return &Service{db: db} }

func eq(k string, v any) bson.E { return bson.E{Key: k, Value: v} }

// Conversation is one row of the inbox.
type Conversation struct {
	LeadID     int64     `json:"lead_id"`
	Name       string    `json:"name"`
	Headline   string    `json:"headline"`
	ProfileURL string    `json:"profile_url"`
	Snippet    string    `json:"snippet"`       // start of the latest message
	LastFrom   string    `json:"last_from"`     // "them" or "us"
	LastAt     time.Time `json:"last_at"`       // time of the latest message
	LastReply  time.Time `json:"last_reply_at"` // time of their latest reply
	Unread     int       `json:"unread"`
}

type List struct {
	Conversations []Conversation `json:"conversations"`
	Total         int            `json:"total"`
	UnreadTotal   int            `json:"unread_total"` // unread messages across the whole inbox
	Limit         int            `json:"limit"`
	Offset        int            `json:"offset"`
}

// conversations groups a user's messages into one row per lead. Messages carry their owner's
// id, so this never reaches another user's data. The latest message is picked with $top, which
// needs no sort of the whole collection.
func conversations(userID int64) mongo.Pipeline {
	isIn := bson.D{{Key: "$eq", Value: bson.A{"$direction", "in"}}}
	// Unread: a reply (direction "in") whose read_at is missing or null.
	notRead := bson.D{{Key: "$eq", Value: bson.A{bson.D{{Key: "$ifNull", Value: bson.A{"$read_at", nil}}}, nil}}}
	unread := bson.D{{Key: "$and", Value: bson.A{isIn, notRead}}}
	return mongo.Pipeline{
		{{Key: "$match", Value: bson.D{eq("user_id", userID)}}},
		{{Key: "$group", Value: bson.D{
			eq("_id", "$lead_id"),
			eq("last_reply", bson.D{{Key: "$max", Value: bson.D{{Key: "$cond", Value: bson.A{isIn, "$at", nil}}}}}),
			eq("last_at", bson.D{{Key: "$max", Value: "$at"}}),
			eq("unread", bson.D{{Key: "$sum", Value: bson.D{{Key: "$cond", Value: bson.A{unread, 1, 0}}}}}),
			eq("latest", bson.D{{Key: "$top", Value: bson.D{
				eq("sortBy", bson.D{eq("at", -1), eq("_id", -1)}),
				eq("output", bson.D{eq("body", "$body"), eq("direction", "$direction")}),
			}}}),
		}}},
		// Only people who have replied at least once belong in the inbox.
		{{Key: "$match", Value: bson.D{eq("last_reply", bson.D{{Key: "$ne", Value: nil}})}}},
	}
}

func (s *Service) List(ctx context.Context, userID int64, unreadOnly bool, limit, offset int) (List, error) {
	if limit <= 0 {
		limit = DefaultPage
	}
	if limit > MaxPage {
		limit = MaxPage
	}
	if offset < 0 {
		offset = 0
	}
	out := List{Conversations: []Conversation{}, Limit: limit, Offset: offset}

	shown := bson.A{}
	if unreadOnly {
		shown = append(shown, bson.D{{Key: "$match", Value: bson.D{eq("unread", bson.D{{Key: "$gt", Value: 0}})}}})
	}
	rowsStage := append(append(bson.A{}, shown...),
		bson.D{{Key: "$sort", Value: bson.D{eq("last_reply", -1), eq("_id", -1)}}},
		bson.D{{Key: "$skip", Value: int64(offset)}}, bson.D{{Key: "$limit", Value: int64(limit)}})
	totalStage := append(append(bson.A{}, shown...), bson.D{{Key: "$count", Value: "n"}})
	pipeline := append(conversations(userID), bson.D{{Key: "$facet", Value: bson.D{
		eq("rows", rowsStage),
		eq("total", totalStage),
		eq("unread_total", bson.A{bson.D{{Key: "$group", Value: bson.D{eq("_id", nil), eq("n", bson.D{{Key: "$sum", Value: "$unread"}})}}}}),
	}}})

	cur, err := s.db.Collection(mdb.Messages).Aggregate(ctx, pipeline)
	if err != nil {
		return List{}, err
	}
	var res []struct {
		Rows []struct {
			LeadID    int64     `bson:"_id"`
			LastReply time.Time `bson:"last_reply"`
			LastAt    time.Time `bson:"last_at"`
			Unread    int       `bson:"unread"`
			Latest    struct {
				Body      string `bson:"body"`
				Direction string `bson:"direction"`
			} `bson:"latest"`
		} `bson:"rows"`
		Total []struct {
			N int `bson:"n"`
		} `bson:"total"`
		UnreadTotal []struct {
			N int `bson:"n"`
		} `bson:"unread_total"`
	}
	if err := cur.All(ctx, &res); err != nil {
		return List{}, err
	}
	if len(res) == 0 {
		return out, nil
	}
	if len(res[0].Total) > 0 {
		out.Total = res[0].Total[0].N
	}
	if len(res[0].UnreadTotal) > 0 {
		out.UnreadTotal = res[0].UnreadTotal[0].N
	}

	leadIDs := make(bson.A, len(res[0].Rows))
	for i, r := range res[0].Rows {
		leadIDs[i] = r.LeadID
	}
	lc, err := s.db.Collection(mdb.Leads).Find(ctx, bson.D{eq("_id", bson.D{{Key: "$in", Value: leadIDs}}), eq("user_id", userID)})
	if err != nil {
		return List{}, err
	}
	var leadDocs []struct {
		ID         int64  `bson:"_id"`
		Name       string `bson:"name"`
		Headline   string `bson:"headline"`
		ProfileURL string `bson:"profile_url"`
	}
	if err := lc.All(ctx, &leadDocs); err != nil {
		return List{}, err
	}
	byID := map[int64]int{}
	for i, l := range leadDocs {
		byID[l.ID] = i
	}
	for _, r := range res[0].Rows {
		i, ok := byID[r.LeadID]
		if !ok {
			continue // the lead is gone; its messages go with it
		}
		c := Conversation{LeadID: r.LeadID, Name: leadDocs[i].Name, Headline: leadDocs[i].Headline, ProfileURL: leadDocs[i].ProfileURL,
			Snippet: snippet(r.Latest.Body), LastAt: r.LastAt, LastReply: r.LastReply, Unread: r.Unread, LastFrom: "them"}
		if r.Latest.Direction == "out" {
			c.LastFrom = "us"
		}
		out.Conversations = append(out.Conversations, c)
	}
	return out, nil
}

func snippet(s string) string {
	if utf8.RuneCountInString(s) <= snippetLen {
		return s
	}
	return string([]rune(s)[:snippetLen]) + "…"
}

type Message struct {
	ID        int64     `json:"id"`
	Direction string    `json:"direction"` // "in" (they wrote) or "out" (we sent)
	Body      string    `json:"body"`
	At        time.Time `json:"at"`
	Read      bool      `json:"read"`
}

type CampaignRef struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type Detail struct {
	LeadID     int64         `json:"lead_id"`
	Name       string        `json:"name"`
	Headline   string        `json:"headline"`
	ProfileURL string        `json:"profile_url"`
	Campaigns  []CampaignRef `json:"campaigns"`
	Messages   []Message     `json:"messages"`
}

// Get returns the whole conversation with one lead, oldest message first.
func (s *Service) Get(ctx context.Context, userID, leadID int64) (Detail, error) {
	d := Detail{Campaigns: []CampaignRef{}, Messages: []Message{}}
	var lead struct {
		ID         int64  `bson:"_id"`
		Name       string `bson:"name"`
		Headline   string `bson:"headline"`
		ProfileURL string `bson:"profile_url"`
	}
	err := s.db.Collection(mdb.Leads).FindOne(ctx, bson.D{eq("_id", leadID), eq("user_id", userID)}).Decode(&lead)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, err
	}
	d.LeadID, d.Name, d.Headline, d.ProfileURL = lead.ID, lead.Name, lead.Headline, lead.ProfileURL

	mc, err := s.db.Collection(mdb.Messages).Find(ctx, bson.D{eq("lead_id", leadID), eq("user_id", userID)},
		options.Find().SetSort(bson.D{eq("at", 1), eq("_id", 1)}))
	if err != nil {
		return Detail{}, err
	}
	var msgs []struct {
		ID        int64      `bson:"_id"`
		Direction string     `bson:"direction"`
		Body      string     `bson:"body"`
		At        time.Time  `bson:"at"`
		ReadAt    *time.Time `bson:"read_at"`
	}
	if err := mc.All(ctx, &msgs); err != nil {
		return Detail{}, err
	}
	for _, m := range msgs {
		d.Messages = append(d.Messages, Message{ID: m.ID, Direction: m.Direction, Body: m.Body, At: m.At, Read: m.ReadAt != nil || m.Direction == "out"})
	}

	cc, err := s.db.Collection(mdb.CampaignLeads).Find(ctx, bson.D{eq("lead_id", leadID), eq("user_id", userID)})
	if err != nil {
		return Detail{}, err
	}
	var members []struct {
		CampaignID int64  `bson:"campaign_id"`
		State      string `bson:"state"`
	}
	if err := cc.All(ctx, &members); err != nil {
		return Detail{}, err
	}
	if len(members) == 0 {
		return d, nil
	}
	campaignIDs := make(bson.A, len(members))
	for i, m := range members {
		campaignIDs[i] = m.CampaignID
	}
	nc, err := s.db.Collection(mdb.Campaigns).Find(ctx, bson.D{eq("_id", bson.D{{Key: "$in", Value: campaignIDs}}), eq("user_id", userID)},
		options.Find().SetSort(bson.D{eq("_id", -1)}))
	if err != nil {
		return Detail{}, err
	}
	var names []struct {
		ID   int64  `bson:"_id"`
		Name string `bson:"name"`
	}
	if err := nc.All(ctx, &names); err != nil {
		return Detail{}, err
	}
	state := map[int64]string{}
	for _, m := range members {
		state[m.CampaignID] = m.State
	}
	for _, c := range names {
		d.Campaigns = append(d.Campaigns, CampaignRef{ID: c.ID, Name: c.Name, State: state[c.ID]})
	}
	return d, nil
}

// MarkRead marks every reply from the lead as read and returns how many changed.
func (s *Service) MarkRead(ctx context.Context, userID, leadID int64) (int64, error) {
	n, err := s.db.Collection(mdb.Leads).CountDocuments(ctx, bson.D{eq("_id", leadID), eq("user_id", userID)})
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrNotFound
	}
	r, err := s.db.Collection(mdb.Messages).UpdateMany(ctx,
		bson.D{eq("lead_id", leadID), eq("user_id", userID), eq("direction", "in"), eq("read_at", nil)},
		bson.D{{Key: "$set", Value: bson.D{eq("read_at", time.Now())}}})
	if err != nil {
		return 0, err
	}
	return r.ModifiedCount, nil
}

// Handler serves the inbox to the browser.
type Handler struct {
	svc  *Service
	auth *auth.Handler
	log  *slog.Logger
}

func NewHandler(svc *Service, a *auth.Handler, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{svc: svc, auth: a, log: log}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/inbox", h.auth.Require(h.list))
	mux.Handle("GET /api/inbox/{leadId}", h.auth.Require(h.get))
	mux.Handle("POST /api/inbox/{leadId}/read", h.auth.Require(h.markRead))
}

func userID(r *http.Request) int64 {
	u, _ := auth.UserFrom(r.Context())
	return u.ID
}

func leadID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("leadId"), 10, 64)
	if err != nil || id <= 0 {
		httpapi.WriteError(w, http.StatusNotFound, "not_found", "conversation not found")
		return 0, false
	}
	return id, true
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	l, err := h.svc.List(r.Context(), userID(r), q.Get("unread") == "1" || q.Get("unread") == "true", limit, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, l)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := leadID(w, r)
	if !ok {
		return
	}
	d, err := h.svc.Get(r.Context(), userID(r), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]Detail{"conversation": d})
}

func (h *Handler) markRead(w http.ResponseWriter, r *http.Request) {
	id, ok := leadID(w, r)
	if !ok {
		return
	}
	n, err := h.svc.MarkRead(r.Context(), userID(r), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]int64{"marked": n})
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) {
		httpapi.WriteError(w, http.StatusNotFound, "not_found", "conversation not found")
		return
	}
	h.log.Error("inbox", "err", err, "request_id", httpapi.RequestID(r.Context()))
	httpapi.WriteError(w, http.StatusInternalServerError, "internal", "internal server error")
}
