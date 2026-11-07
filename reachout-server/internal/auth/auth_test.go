package auth

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ashmit/reachout-server/internal/httpapi"
	"github.com/ashmit/reachout-server/internal/mdb"
	"github.com/ashmit/reachout-server/internal/testdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

const goodPW = "correct horse battery"

func TestMain(m *testing.M) {
	// Cheap hashing keeps the suite fast; production cost is set in argonParams.
	argonParams.Memory, argonParams.Time, argonParams.Threads = 8, 1, 1
	m.Run()
}

type env struct {
	db  *mongo.Database
	svc *Service
	h   http.Handler
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := testdb.New(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewService(db)
	mux, handler := httpapi.NewRouter(httpapi.Deps{
		DB: mdb.Pinger{DB: db}, Log: log, AllowedOrigins: []string{"http://localhost:5173"},
	})
	ah := NewHandler(svc, Options{Log: log})
	ah.Register(mux)
	mux.Handle("GET /api/private", ah.Require(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFrom(r.Context())
		httpapi.WriteJSON(w, 200, map[string]int64{"user_id": u.ID})
	}))
	return &env{db: db, svc: svc, h: handler}
}

func (e *env) do(method, path string, body any, cookie string, hdr map[string]string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	t.Fatalf("no session cookie in response (status %d, body %s)", rec.Code, rec.Body)
	return nil
}

func (e *env) signup(t *testing.T, email string) string {
	t.Helper()
	rec := e.do("POST", "/api/auth/signup", credentials{email, goodPW}, "", nil)
	if rec.Code != 201 {
		t.Fatalf("signup %s: %d %s", email, rec.Code, rec.Body)
	}
	return sessionCookie(t, rec).Value
}

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword(goodPW)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") || strings.Contains(h, goodPW) {
		t.Fatalf("unexpected hash format: %s", h)
	}
	h2, _ := HashPassword(goodPW)
	if h == h2 {
		t.Fatal("same password must hash differently (random salt)")
	}
	if ok, err := VerifyPassword(goodPW, h); !ok || err != nil {
		t.Fatalf("verify good: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword("wrong password!!", h); ok {
		t.Fatal("wrong password verified")
	}
	for _, bad := range []string{"", "plaintext", "$argon2id$v=19$m=8,t=1,p=1$!!$!!", "$bcrypt$x$y$z$w$v",
		"$argon2id$v=19$m=999999999,t=1,p=1$AAAA$AAAA"} {
		if ok, err := VerifyPassword(goodPW, bad); ok || err == nil {
			t.Errorf("malformed hash %q: ok=%v err=%v", bad, ok, err)
		}
	}
}

func TestSignupLoginMeLogout(t *testing.T) {
	e := newEnv(t)

	rec := e.do("POST", "/api/auth/signup", credentials{"  Alice@Example.COM ", goodPW}, "", nil)
	if rec.Code != 201 {
		t.Fatalf("signup: %d %s", rec.Code, rec.Body)
	}
	c := sessionCookie(t, rec)
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge <= 0 {
		t.Fatalf("cookie flags wrong: %+v", c)
	}
	body := rec.Body.String()
	if strings.Contains(body, "password") || strings.Contains(body, "argon2") {
		t.Fatalf("response leaks credentials: %s", body)
	}

	rec = e.do("GET", "/api/auth/me", nil, c.Value, nil)
	var me userResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &me) != nil || me.User.Email != "alice@example.com" {
		t.Fatalf("me: %d %s", rec.Code, rec.Body)
	}

	// Login with different capitalisation gives a fresh, different session.
	rec = e.do("POST", "/api/auth/login", credentials{"ALICE@example.com", goodPW}, "", nil)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	c2 := sessionCookie(t, rec)
	if c2.Value == c.Value {
		t.Fatal("login reused a session token")
	}

	// Logout kills only that session.
	if rec = e.do("POST", "/api/auth/logout", nil, c2.Value, nil); rec.Code != 204 {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec = e.do("GET", "/api/auth/me", nil, c2.Value, nil); rec.Code != 401 {
		t.Fatalf("logged-out session still valid: %d", rec.Code)
	}
	if rec = e.do("GET", "/api/auth/me", nil, c.Value, nil); rec.Code != 200 {
		t.Fatalf("other session was revoked: %d", rec.Code)
	}
}

func TestSignupValidation(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name string
		in   credentials
		want int
	}{
		{"weak password", credentials{"a@b.co", "short"}, 400},
		{"too long password", credentials{"a@b.co", strings.Repeat("x", 129)}, 400},
		{"empty email", credentials{"", goodPW}, 400},
		{"not an email", credentials{"nope", goodPW}, 400},
		{"display name form", credentials{"Bob <bob@x.com>", goodPW}, 400},
		{"no tld", credentials{"bob@localhost", goodPW}, 400},
	}
	for _, c := range cases {
		if rec := e.do("POST", "/api/auth/signup", c.in, "", nil); rec.Code != c.want {
			t.Errorf("%s: got %d %s, want %d", c.name, rec.Code, rec.Body, c.want)
		}
	}
	if rec := e.do("POST", "/api/auth/signup", map[string]string{"email": "a@b.co", "password": goodPW, "admin": "true"}, "", nil); rec.Code != 400 {
		t.Errorf("unknown field accepted: %d", rec.Code)
	}
}

func TestDuplicateEmail(t *testing.T) {
	e := newEnv(t)
	e.signup(t, "dup@example.com")
	rec := e.do("POST", "/api/auth/signup", credentials{"DUP@example.com", goodPW}, "", nil)
	if rec.Code != 409 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestLoginFailuresLookIdentical(t *testing.T) {
	e := newEnv(t)
	e.signup(t, "real@example.com")
	a := e.do("POST", "/api/auth/login", credentials{"real@example.com", "wrong password!!"}, "", nil)
	b := e.do("POST", "/api/auth/login", credentials{"ghost@example.com", "wrong password!!"}, "", nil)
	if a.Code != 401 || b.Code != 401 || a.Body.String() != b.Body.String() {
		t.Fatalf("wrong-password vs unknown-email differ: %d %q / %d %q", a.Code, a.Body, b.Code, b.Body)
	}
}

func TestProtectedRouteNeedsSession(t *testing.T) {
	e := newEnv(t)
	for _, cookie := range []string{"", "garbage", strings.Repeat("a", 200)} {
		if rec := e.do("GET", "/api/private", nil, cookie, nil); rec.Code != 401 {
			t.Errorf("cookie %.10q: got %d, want 401", cookie, rec.Code)
		}
	}
	tok := e.signup(t, "p@example.com")
	rec := e.do("GET", "/api/private", nil, tok, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "user_id") {
		t.Fatalf("with session: %d %s", rec.Code, rec.Body)
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	e := newEnv(t)
	tok := e.signup(t, "exp@example.com")
	testdb.Update(t, e.db, mdb.Sessions, bson.D{}, bson.D{{Key: "expires_at", Value: time.Now().Add(-time.Minute)}})
	rec := e.do("GET", "/api/auth/me", nil, tok, nil)
	if rec.Code != 401 {
		t.Fatalf("expired session accepted: %d", rec.Code)
	}
	// The stale cookie is cleared so the browser stops sending it.
	if c := sessionCookie(t, rec); c.MaxAge >= 0 {
		t.Fatalf("cookie not cleared: %+v", c)
	}
}

func TestSessionExpiryUsesClock(t *testing.T) {
	e := newEnv(t)
	tok := e.signup(t, "clock@example.com")
	e.svc.now = func() time.Time { return time.Now().Add(SessionTTL + time.Minute) }
	if rec := e.do("GET", "/api/auth/me", nil, tok, nil); rec.Code != 401 {
		t.Fatalf("session valid past TTL: %d", rec.Code)
	}
}

func TestTokenStoredHashedOnly(t *testing.T) {
	e := newEnv(t)
	tok := e.signup(t, "hash@example.com")
	// The raw token must not be a session's id in either form, only its hash.
	if n := testdb.Count(t, e.db, mdb.Sessions, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: bson.A{[]byte(tok), tok}}}}}); n != 0 {
		t.Fatal("raw session token found in database")
	}
	if n := testdb.Count(t, e.db, mdb.Sessions, bson.D{{Key: "_id", Value: hashToken(tok)}}); n != 1 {
		t.Fatalf("hashed token row count = %d, want 1", n)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	e.signup(t, "rl@example.com")
	for i := 0; i < 5; i++ {
		if rec := e.do("POST", "/api/auth/login", credentials{"rl@example.com", "wrong password!!"}, "", nil); rec.Code != 401 {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	// Locked out even with the right password.
	rec := e.do("POST", "/api/auth/login", credentials{"rl@example.com", goodPW}, "", nil)
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d, retry-after %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	// Another account from the same IP is still allowed (per-account bucket).
	e.signup(t, "other@example.com")
	if rec := e.do("POST", "/api/auth/login", credentials{"other@example.com", goodPW}, "", nil); rec.Code != 200 {
		t.Fatalf("unrelated account blocked: %d", rec.Code)
	}
}

func TestLimiterWindowAndReset(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter(2, time.Minute)
	l.now = func() time.Time { return now }
	l.Add("k")
	l.Add("k")
	if !l.Blocked("k") {
		t.Fatal("should be blocked at max")
	}
	now = now.Add(61 * time.Second)
	if l.Blocked("k") {
		t.Fatal("should unblock after window")
	}
	l.Add("k")
	l.Add("k")
	l.Reset("k")
	if l.Blocked("k") {
		t.Fatal("reset did not clear")
	}
}

func TestSignupRateLimit(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 10; i++ {
		e.do("POST", "/api/auth/signup", credentials{"bad", goodPW}, "", nil) // each attempt counts
	}
	if rec := e.do("POST", "/api/auth/signup", credentials{"ok@example.com", goodPW}, "", nil); rec.Code != 429 {
		t.Fatalf("got %d, want 429", rec.Code)
	}
}

func TestCSRF(t *testing.T) {
	e := newEnv(t)
	good := credentials{"csrf@example.com", goodPW}
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"foreign origin", map[string]string{"Origin": "https://evil.example"}, 403},
		{"null origin", map[string]string{"Origin": "null"}, 403},
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"allowed origin", map[string]string{"Origin": "http://localhost:5173"}, 201},
	}
	for _, c := range cases {
		rec := e.do("POST", "/api/auth/signup", good, "", c.hdr)
		if rec.Code != c.want {
			t.Errorf("%s: got %d %s, want %d", c.name, rec.Code, rec.Body, c.want)
		}
	}
	// A blocked request must not have created the user or a session.
	if n := testdb.Count(t, e.db, mdb.Users, bson.D{}); n != 1 {
		t.Fatalf("users = %d, want 1 (only the allowed request)", n)
	}
	// Logout is a POST too: a cross-site page cannot sign the user out.
	tok := sessionCookieValue(t, e)
	if rec := e.do("POST", "/api/auth/logout", nil, tok, map[string]string{"Origin": "https://evil.example"}); rec.Code != 403 {
		t.Fatalf("cross-site logout: %d", rec.Code)
	}
	if rec := e.do("GET", "/api/auth/me", nil, tok, nil); rec.Code != 200 {
		t.Fatal("session was revoked by a blocked request")
	}
}

func sessionCookieValue(t *testing.T, e *env) string {
	t.Helper()
	rec := e.do("POST", "/api/auth/login", credentials{"csrf@example.com", goodPW}, "", nil)
	return sessionCookie(t, rec).Value
}
