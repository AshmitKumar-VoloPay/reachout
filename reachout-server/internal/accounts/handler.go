package accounts

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/ashmit/reachout-server/internal/auth"
	"github.com/ashmit/reachout-server/internal/httpapi"
	"github.com/ashmit/reachout-server/internal/linkedin"
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
	mux.Handle("GET /api/accounts", h.auth.Require(h.list))
	mux.Handle("POST /api/accounts/connect", h.auth.Require(h.connect))
	mux.Handle("GET /api/accounts/{id}", h.auth.Require(h.get))
	mux.Handle("POST /api/accounts/{id}/refresh", h.auth.Require(h.refresh))
	mux.Handle("POST /api/accounts/{id}/reconnect", h.auth.Require(h.reconnect))
	mux.Handle("GET /api/accounts/{id}/schedule", h.auth.Require(h.getSchedule))
	mux.Handle("PUT /api/accounts/{id}/schedule", h.auth.Require(h.putSchedule))
}

// ids returns the signed-in user and the {id} path value. On a bad id it writes the 404 itself.
func (h *Handler) ids(w http.ResponseWriter, r *http.Request) (userID, accountID int64, ok bool) {
	u, _ := auth.UserFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpapi.WriteError(w, http.StatusNotFound, "not_found", "account not found")
		return 0, 0, false
	}
	return u.ID, id, true
}

type accountResponse struct {
	Account Account `json:"account"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	accs, err := h.svc.List(r.Context(), u.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"accounts": accs})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	uid, id, ok := h.ids(w, r)
	if !ok {
		return
	}
	a, err := h.svc.Get(r.Context(), uid, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, accountResponse{a})
}

func (h *Handler) connect(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	var in struct {
		Timezone string `json:"timezone"`
	}
	if r.ContentLength != 0 && !httpapi.DecodeJSON(w, r, &in) {
		return
	}
	if in.Timezone == "" {
		in.Timezone = "UTC"
	}
	a, link, err := h.svc.StartConnect(r.Context(), u.ID, in.Timezone)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	a.ConnectionLink = link
	httpapi.WriteJSON(w, http.StatusCreated, accountResponse{a})
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	uid, id, ok := h.ids(w, r)
	if !ok {
		return
	}
	a, err := h.svc.Refresh(r.Context(), uid, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, accountResponse{a})
}

func (h *Handler) reconnect(w http.ResponseWriter, r *http.Request) {
	uid, id, ok := h.ids(w, r)
	if !ok {
		return
	}
	link, err := h.svc.Reconnect(r.Context(), uid, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]string{"reconnection_link": link})
}

func (h *Handler) getSchedule(w http.ResponseWriter, r *http.Request) {
	uid, id, ok := h.ids(w, r)
	if !ok {
		return
	}
	v, err := h.svc.GetSchedule(r.Context(), uid, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, v)
}

func (h *Handler) putSchedule(w http.ResponseWriter, r *http.Request) {
	uid, id, ok := h.ids(w, r)
	if !ok {
		return
	}
	var in ScheduleSettings
	if !httpapi.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.SetSchedule(r.Context(), uid, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, v)
}

// fail maps service errors to responses. Linked API error text is logged, not returned,
// so upstream details never reach the browser.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "not_found", "account not found")
	case errors.Is(err, ErrInvalidSchedule):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_schedule", err.Error())
	case errors.Is(err, ErrConnectionBusy):
		httpapi.WriteError(w, http.StatusConflict, "connection_busy", err.Error())
	case errors.Is(err, ErrAmbiguous):
		httpapi.WriteError(w, http.StatusConflict, "connect_ambiguous", err.Error())
	case errors.Is(err, ErrNotReconnectable):
		httpapi.WriteError(w, http.StatusConflict, "not_reconnectable", err.Error())
	case linkedin.IsNoSeats(err):
		httpapi.WriteError(w, http.StatusConflict, "no_seats", "no LinkedIn seat is available on the subscription")
	case linkedin.IsTransient(err):
		h.log.Warn("linked api unavailable", "err", err, "request_id", httpapi.RequestID(r.Context()))
		httpapi.WriteError(w, http.StatusBadGateway, "linkedin_unavailable", "LinkedIn service is unavailable, try again shortly")
	default:
		h.log.Error("accounts", "err", err, "request_id", httpapi.RequestID(r.Context()))
		httpapi.WriteError(w, http.StatusInternalServerError, "internal", "internal server error")
	}
}
