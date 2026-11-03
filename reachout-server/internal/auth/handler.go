package auth

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/ashmit/reachout-server/internal/httpapi"
)

const cookieName = "reachout_session"

// CookieName is the session cookie's name, exported for tests and clients that need to set it.
const CookieName = cookieName

type ctxKey struct{}

// UserFrom returns the authenticated user set by Require. Handlers behind Require can rely on ok.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

type Options struct {
	CookieSecure bool
	Log          *slog.Logger
}

type Handler struct {
	svc    *Service
	opts   Options
	acct   *Limiter // failed logins per email+IP
	ip     *Limiter // failed logins per IP
	signup *Limiter // signup attempts per IP
}

func NewHandler(svc *Service, opts Options) *Handler {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Handler{
		svc:    svc,
		opts:   opts,
		acct:   NewLimiter(5, 15*time.Minute),
		ip:     NewLimiter(20, 15*time.Minute),
		signup: NewLimiter(10, time.Hour),
	}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/signup", h.signupHandler)
	mux.HandleFunc("POST /api/auth/login", h.login)
	mux.HandleFunc("POST /api/auth/logout", h.logout)
	mux.Handle("GET /api/auth/me", h.Require(h.me))
}

// Require wraps a handler so it only runs for a signed-in user, who is then available via UserFrom.
func (h *Handler) Require(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || c.Value == "" || len(c.Value) > 128 {
			unauthorized(w)
			return
		}
		u, err := h.svc.Authenticate(r.Context(), c.Value)
		if errors.Is(err, ErrNoSession) {
			h.clearCookie(w)
			unauthorized(w)
			return
		}
		if err != nil {
			h.opts.Log.Error("authenticate", "err", err, "request_id", httpapi.RequestID(r.Context()))
			httpapi.WriteError(w, http.StatusInternalServerError, "internal", "internal server error")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

func unauthorized(w http.ResponseWriter) {
	httpapi.WriteError(w, http.StatusUnauthorized, "unauthorized", "sign in required")
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type userResponse struct {
	User User `json:"user"`
}

func (h *Handler) signupHandler(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if h.signup.Blocked(ip) {
		tooMany(w, time.Hour)
		return
	}
	var in credentials
	if !httpapi.DecodeJSON(w, r, &in) {
		return
	}
	h.signup.Add(ip) // every attempt counts: hashing is expensive
	u, token, expires, err := h.svc.Signup(r.Context(), in.Email, in.Password)
	switch {
	case errors.Is(err, ErrInvalidEmail), errors.Is(err, ErrWeakPassword):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_input", err.Error())
	case errors.Is(err, ErrEmailTaken):
		httpapi.WriteError(w, http.StatusConflict, "email_taken", err.Error())
	case err != nil:
		h.serverError(w, r, "signup", err)
	default:
		h.setCookie(w, token, expires)
		httpapi.WriteJSON(w, http.StatusCreated, userResponse{u})
	}
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !httpapi.DecodeJSON(w, r, &in) {
		return
	}
	ip := clientIP(r)
	acctKey := normalizeKey(in.Email) + "|" + ip
	if h.ip.Blocked(ip) || h.acct.Blocked(acctKey) {
		tooMany(w, 15*time.Minute)
		return
	}
	u, token, expires, err := h.svc.Login(r.Context(), in.Email, in.Password)
	switch {
	case errors.Is(err, ErrInvalidCredentials):
		h.ip.Add(ip)
		h.acct.Add(acctKey)
		httpapi.WriteError(w, http.StatusUnauthorized, "invalid_credentials", err.Error())
	case err != nil:
		h.serverError(w, r, "login", err)
	default:
		h.acct.Reset(acctKey)
		h.setCookie(w, token, expires)
		httpapi.WriteJSON(w, http.StatusOK, userResponse{u})
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		if err := h.svc.Logout(r.Context(), c.Value); err != nil {
			h.serverError(w, r, "logout", err)
			return
		}
	}
	h.clearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	httpapi.WriteJSON(w, http.StatusOK, userResponse{u})
}

func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, op string, err error) {
	h.opts.Log.Error(op, "err", err, "request_id", httpapi.RequestID(r.Context()))
	httpapi.WriteError(w, http.StatusInternalServerError, "internal", "internal server error")
}

func tooMany(w http.ResponseWriter, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
	httpapi.WriteError(w, http.StatusTooManyRequests, "too_many_attempts", "too many attempts, try again later")
}

func (h *Handler) setCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", Expires: expires,
		MaxAge: int(SessionTTL.Seconds()), HttpOnly: true, Secure: h.opts.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: h.opts.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

// clientIP uses the TCP peer address. It does not trust X-Forwarded-For, which a
// client can forge. Behind a reverse proxy, put the proxy's real-IP handling in front
// (rewrite RemoteAddr) before relying on the per-IP limits.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func normalizeKey(email string) string {
	if e, err := NormalizeEmail(email); err == nil {
		return e
	}
	if len(email) > 254 {
		email = email[:254]
	}
	return email
}
