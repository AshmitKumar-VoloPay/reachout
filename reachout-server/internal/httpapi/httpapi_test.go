package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func newTestRouter(db Pinger) (*http.ServeMux, http.Handler) {
	return NewRouter(Deps{DB: db, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func do(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	_, h := newTestRouter(fakeDB{})
	rec := do(h, "GET", "/healthz", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	_, h = newTestRouter(fakeDB{err: errors.New("down")})
	if rec := do(h, "GET", "/healthz", "", nil); rec.Code != 503 {
		t.Fatalf("db down: got %d", rec.Code)
	}
}

func TestNotFoundIsJSON(t *testing.T) {
	_, h := newTestRouter(fakeDB{})
	rec := do(h, "GET", "/nope", "", nil)
	var b errorBody
	if rec.Code != 404 || json.Unmarshal(rec.Body.Bytes(), &b) != nil || b.Error.Code != "not_found" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestRequestID(t *testing.T) {
	_, h := newTestRouter(fakeDB{})
	if id := do(h, "GET", "/healthz", "", nil).Header().Get("X-Request-ID"); len(id) != 16 {
		t.Fatalf("generated id %q", id)
	}
	if id := do(h, "GET", "/healthz", "", map[string]string{"X-Request-ID": "abc-123"}).Header().Get("X-Request-ID"); id != "abc-123" {
		t.Fatalf("did not reuse id: %q", id)
	}
	// Injection attempts and oversized values are replaced, not echoed.
	for _, bad := range []string{"has space", "a\"b", strings.Repeat("x", 65)} {
		if id := do(h, "GET", "/healthz", "", map[string]string{"X-Request-ID": bad}).Header().Get("X-Request-ID"); id == bad {
			t.Fatalf("echoed unsafe id %q", bad)
		}
	}
}

func TestPanicBecomes500AndServerSurvives(t *testing.T) {
	mux, h := newTestRouter(fakeDB{})
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	rec := do(h, "GET", "/boom", "", nil)
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "kaboom") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "GET", "/healthz", "", nil); rec.Code != 200 {
		t.Fatal("server did not survive panic")
	}
}

func TestDecodeJSON(t *testing.T) {
	type in struct {
		Name string `json:"name"`
	}
	cases := []struct {
		name, body string
		ok         bool
		status     int
	}{
		{"valid", `{"name":"a"}`, true, 200},
		{"unknown field", `{"name":"a","x":1}`, false, 400},
		{"not json", `nope`, false, 400},
		{"trailing data", `{"name":"a"} {"name":"b"}`, false, 400},
		{"too large", `{"name":"` + strings.Repeat("a", 2<<20) + `"}`, false, 413},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/", strings.NewReader(c.body))
			var v in
			ok := DecodeJSON(rec, req, &v)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v", ok, c.ok)
			}
			if !ok && rec.Code != c.status {
				t.Fatalf("status %d want %d", rec.Code, c.status)
			}
		})
	}
}
