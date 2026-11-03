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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("conversation not found")

const (
	DefaultPage = 50
	MaxPage     = 200
	snippetLen  = 160
)

type Service struct{ pool *pgxpool.Pool }

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

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

// base selects one row per lead that has at least one reply.
const base = `
	SELECT m.lead_id,
	       max(m.at) FILTER (WHERE m.direction = 'in') AS last_reply,
	       max(m.at)                                     AS last_at,
	       count(*) FILTER (WHERE m.direction = 'in' AND m.read_at IS NULL) AS unread
	  FROM messages m JOIN leads l ON l.id = m.lead_id
	 WHERE l.user_id = $1
	 GROUP BY m.lead_id
	HAVING bool_or(m.direction = 'in')`

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

	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE ($2 = false OR unread > 0)), coalesce(sum(unread), 0) FROM (`+base+`) c`,
		userID, unreadOnly).Scan(&out.Total, &out.UnreadTotal); err != nil {
		return List{}, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT l.id, l.name, l.headline, l.profile_url, c.last_reply, c.last_at, c.unread,
		        lm.body, lm.direction
		   FROM (`+base+`) c
		   JOIN leads l ON l.id = c.lead_id
		   JOIN LATERAL (SELECT body, direction FROM messages WHERE lead_id = l.id ORDER BY at DESC, id DESC LIMIT 1) lm ON true
		  WHERE ($2 = false OR c.unread > 0)
		  ORDER BY c.last_reply DESC, l.id DESC LIMIT $3 OFFSET $4`, userID, unreadOnly, limit, offset)
	if err != nil {
		return List{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Conversation
		var body, dir string
		if err := rows.Scan(&c.LeadID, &c.Name, &c.Headline, &c.ProfileURL, &c.LastReply, &c.LastAt, &c.Unread, &body, &dir); err != nil {
			return List{}, err
		}
		c.Snippet = snippet(body)
		c.LastFrom = "them"
		if dir == "out" {
			c.LastFrom = "us"
		}
		out.Conversations = append(out.Conversations, c)
	}
	return out, rows.Err()
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
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, headline, profile_url FROM leads WHERE id = $1 AND user_id = $2`, leadID, userID).
		Scan(&d.LeadID, &d.Name, &d.Headline, &d.ProfileURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, err
	}

	rows, err := s.pool.Query(ctx,
		`SELECT id, direction, body, at, read_at IS NOT NULL OR direction = 'out' FROM messages WHERE lead_id = $1 ORDER BY at, id`, leadID)
	if err != nil {
		return Detail{}, err
	}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Direction, &m.Body, &m.At, &m.Read); err != nil {
			rows.Close()
			return Detail{}, err
		}
		d.Messages = append(d.Messages, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Detail{}, err
	}

	crow, err := s.pool.Query(ctx,
		`SELECT c.id, c.name, cl.state FROM campaign_leads cl JOIN campaigns c ON c.id = cl.campaign_id
		  WHERE cl.lead_id = $1 AND cl.user_id = $2 ORDER BY c.id DESC`, leadID, userID)
	if err != nil {
		return Detail{}, err
	}
	defer crow.Close()
	for crow.Next() {
		var c CampaignRef
		if err := crow.Scan(&c.ID, &c.Name, &c.State); err != nil {
			return Detail{}, err
		}
		d.Campaigns = append(d.Campaigns, c)
	}
	return d, crow.Err()
}

// MarkRead marks every reply from the lead as read and returns how many changed.
func (s *Service) MarkRead(ctx context.Context, userID, leadID int64) (int64, error) {
	var owned bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM leads WHERE id = $1 AND user_id = $2)`, leadID, userID).Scan(&owned); err != nil {
		return 0, err
	}
	if !owned {
		return 0, ErrNotFound
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE messages SET read_at = now() WHERE lead_id = $1 AND direction = 'in' AND read_at IS NULL`, leadID)
	return tag.RowsAffected(), err
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
