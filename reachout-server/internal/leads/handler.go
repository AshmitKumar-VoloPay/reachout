package leads

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/ashmit/reachout-server/internal/accounts"
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
	mux.Handle("GET /api/leads", h.auth.Require(h.list))
	mux.Handle("POST /api/leads/import/urls", h.auth.Require(h.importURLs))
	mux.Handle("POST /api/leads/import/search", h.auth.Require(h.importSearch))
	mux.Handle("GET /api/leads/imports/{id}", h.auth.Require(h.getImport))
	mux.Handle("POST /api/leads/bulk-delete", h.auth.Require(h.bulkDelete))
	mux.Handle("GET /api/leads/{id}", h.auth.Require(h.get))
	mux.Handle("DELETE /api/leads/{id}", h.auth.Require(h.delete))
}

func userID(r *http.Request) int64 {
	u, _ := auth.UserFrom(r.Context())
	return u.ID
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpapi.WriteError(w, http.StatusNotFound, "not_found", "not found")
		return 0, false
	}
	return id, true
}

func intParam(r *http.Request, name string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(name))
	return n
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	page, err := h.svc.List(r.Context(), userID(r), ListParams{
		Query: r.URL.Query().Get("q"), Limit: intParam(r, "limit"), Offset: intParam(r, "offset"),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, page)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	l, err := h.svc.Get(r.Context(), userID(r), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]Lead{"lead": l})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), userID(r), id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) bulkDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if !httpapi.DecodeJSON(w, r, &in) {
		return
	}
	n, err := h.svc.DeleteMany(r.Context(), userID(r), in.IDs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]int64{"deleted": n})
}

func (h *Handler) importURLs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string   `json:"text"` // pasted blob: one URL per line, or comma separated
		URLs []string `json:"urls"`
	}
	if !httpapi.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.svc.ImportURLs(r.Context(), userID(r), in.Text, in.URLs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) importSearch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountID int64  `json:"account_id"`
		SearchURL string `json:"search_url"`
		Limit     int    `json:"limit"`
	}
	if !httpapi.DecodeJSON(w, r, &in) {
		return
	}
	imp, err := h.svc.StartSearch(r.Context(), userID(r), in.AccountID, in.SearchURL, in.Limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusAccepted, map[string]Import{"import": imp})
}

func (h *Handler) getImport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	imp, err := h.svc.GetImport(r.Context(), userID(r), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]Import{"import": imp})
}

// fail maps service errors to responses. Upstream (Linked API) text is logged, never returned.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var oh *OutsideWorkingHoursError
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, accounts.ErrNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, ErrInvalidInput):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, ErrAccountNotReady):
		httpapi.WriteError(w, http.StatusConflict, "account_not_connected", err.Error())
	case errors.Is(err, ErrImportInProgress):
		httpapi.WriteError(w, http.StatusConflict, "import_in_progress", err.Error())
	case errors.As(err, &oh):
		httpapi.WriteError(w, http.StatusConflict, "outside_working_hours", oh.Error())
	case linkedin.IsSignedOut(err):
		httpapi.WriteError(w, http.StatusConflict, "account_signed_out", "the LinkedIn account is signed out, reconnect it")
	case linkedin.IsLimitHit(err), linkedin.IsOutsideWorkingHours(err):
		httpapi.WriteError(w, http.StatusConflict, "linkedin_limit", "LinkedIn is not accepting searches for this account right now")
	case linkedin.IsTransient(err):
		h.log.Warn("linked api unavailable", "err", err, "request_id", httpapi.RequestID(r.Context()))
		httpapi.WriteError(w, http.StatusBadGateway, "linkedin_unavailable", "LinkedIn service is unavailable, try again shortly")
	default:
		h.log.Error("leads", "err", err, "request_id", httpapi.RequestID(r.Context()))
		httpapi.WriteError(w, http.StatusInternalServerError, "internal", "internal server error")
	}
}
