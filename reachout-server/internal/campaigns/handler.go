package campaigns

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/ashmit/reachout-server/internal/accounts"
	"github.com/ashmit/reachout-server/internal/auth"
	"github.com/ashmit/reachout-server/internal/httpapi"
)

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
	r := h.auth.Require
	mux.Handle("GET /api/campaigns", r(h.list))
	mux.Handle("POST /api/campaigns", r(h.create))
	mux.Handle("GET /api/campaigns/{id}", r(h.get))
	mux.Handle("PUT /api/campaigns/{id}", r(h.update))
	mux.Handle("DELETE /api/campaigns/{id}", r(h.delete))
	mux.Handle("POST /api/campaigns/{id}/start", r(h.life(h.svc.Start)))
	mux.Handle("POST /api/campaigns/{id}/pause", r(h.life(h.svc.Pause)))
	mux.Handle("POST /api/campaigns/{id}/resume", r(h.life(h.svc.Resume)))
	mux.Handle("POST /api/campaigns/{id}/stop", r(h.life(h.svc.Stop)))
	mux.Handle("GET /api/campaigns/{id}/leads", r(h.listLeads))
	mux.Handle("POST /api/campaigns/{id}/leads", r(h.addLeads))
	mux.Handle("DELETE /api/campaigns/{id}/leads/{leadId}", r(h.removeLead))
}

func userID(r *http.Request) int64 {
	u, _ := auth.UserFrom(r.Context())
	return u.ID
}

func idParam(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		httpapi.WriteError(w, http.StatusNotFound, "not_found", "not found")
		return 0, false
	}
	return id, true
}

func intQuery(r *http.Request, name string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(name))
	return n
}

type campaignResponse struct {
	Campaign Campaign `json:"campaign"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	cs, err := h.svc.List(r.Context(), userID(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"campaigns": cs})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var in Input
	if !httpapi.DecodeJSON(w, r, &in) {
		return
	}
	c, err := h.svc.Create(r.Context(), userID(r), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusCreated, campaignResponse{c})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "id")
	if !ok {
		return
	}
	c, err := h.svc.Get(r.Context(), userID(r), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, campaignResponse{c})
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "id")
	if !ok {
		return
	}
	var in Input
	if !httpapi.DecodeJSON(w, r, &in) {
		return
	}
	c, err := h.svc.Update(r.Context(), userID(r), id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, campaignResponse{c})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "id")
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), userID(r), id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// life adapts start, pause, resume and stop, which all take a user and campaign id.
func (h *Handler) life(fn func(ctx context.Context, userID, id int64) (Campaign, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(w, r, "id")
		if !ok {
			return
		}
		c, err := fn(r.Context(), userID(r), id)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		httpapi.WriteJSON(w, http.StatusOK, campaignResponse{c})
	}
}

func (h *Handler) listLeads(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "id")
	if !ok {
		return
	}
	state := State(r.URL.Query().Get("state"))
	if state != "" && !validState(state) {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_input", "unknown state filter")
		return
	}
	page, err := h.svc.ListLeads(r.Context(), userID(r), id, state, intQuery(r, "limit"), intQuery(r, "offset"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, page)
}

func validState(s State) bool {
	switch s {
	case StateQueued, StateRequestSent, StateConnected, StateMessaged, StateFollowedUp, StateReplied, StateDone, StateFailed:
		return true
	}
	return false
}

func (h *Handler) addLeads(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		LeadIDs []int64 `json:"lead_ids"`
	}
	if !httpapi.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.svc.AddLeads(r.Context(), userID(r), id, in.LeadIDs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) removeLead(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "id")
	if !ok {
		return
	}
	leadID, ok := idParam(w, r, "leadId")
	if !ok {
		return
	}
	if err := h.svc.RemoveLead(r.Context(), userID(r), id, leadID); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, accounts.ErrNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, ErrInvalid):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_campaign", err.Error())
	case errors.Is(err, ErrNotEditable):
		httpapi.WriteError(w, http.StatusConflict, "not_editable", err.Error())
	case errors.Is(err, ErrWrongStatus):
		httpapi.WriteError(w, http.StatusConflict, "wrong_status", err.Error())
	case errors.Is(err, ErrNoLeads):
		httpapi.WriteError(w, http.StatusConflict, "no_leads", err.Error())
	case errors.Is(err, ErrAccountNotReady):
		httpapi.WriteError(w, http.StatusConflict, "account_not_connected", err.Error())
	case errors.Is(err, ErrLeadConflict):
		httpapi.WriteError(w, http.StatusConflict, "lead_conflict", err.Error())
	case errors.Is(err, ErrLeadNotRemovable):
		httpapi.WriteError(w, http.StatusConflict, "lead_not_removable", err.Error())
	default:
		h.log.Error("campaigns", "err", err, "request_id", httpapi.RequestID(r.Context()))
		httpapi.WriteError(w, http.StatusInternalServerError, "internal", "internal server error")
	}
}
