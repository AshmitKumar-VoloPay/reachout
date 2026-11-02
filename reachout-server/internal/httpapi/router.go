// Package httpapi holds the router, shared middleware and JSON helpers.
// Feature handlers (auth, leads, campaigns...) register their routes on the mux
// returned by NewRouter's caller in later phases.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Pinger is satisfied by *pgxpool.Pool.
type Pinger interface {
	Ping(ctx context.Context) error
}

type Deps struct {
	DB  Pinger
	Log *slog.Logger
}

const requestTimeout = 30 * time.Second

// NewRouter returns the mux and the fully wrapped handler. Register new routes on
// the mux; serve the handler.
func NewRouter(d Deps) (*http.ServeMux, http.Handler) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := d.DB.Ping(ctx); err != nil {
			WriteError(w, http.StatusServiceUnavailable, "db_down", "database unavailable")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, http.StatusNotFound, "not_found", "no such route")
	})

	// Outermost first: ID, access log, panic recovery, timeout.
	var h http.Handler = mux
	h = timeout(requestTimeout)(h)
	h = recoverer(d.Log)(h)
	h = accessLog(d.Log)(h)
	h = withRequestID(h)
	return mux, h
}
