package accounts

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ashmit/reachout-server/internal/auth"
	"github.com/ashmit/reachout-server/internal/httpapi"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	svc  *Service
	fake *linkedin.Fake
	h    http.Handler
	aSvc *auth.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testdb.New(t)
	key := make([]byte, 32)
	rand.Read(key)
	c, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := linkedin.NewFake()
	svc := NewService(pool, fake, c, log)

	mux, handler := httpapi.NewRouter(httpapi.Deps{DB: pool, Log: log, AllowedOrigins: []string{"http://localhost:5173"}})
	aSvc := auth.NewService(pool)
	ah := auth.NewHandler(aSvc, auth.Options{Log: log})
	ah.Register(mux)
	NewHandler(svc, ah, log).Register(mux)
	return &env{t: t, pool: pool, svc: svc, fake: fake, h: handler, aSvc: aSvc}
}

// user signs up and returns the session cookie value.
func (e *env) user(email string) string {
	e.t.Helper()
	_, tok, _, err := e.aSvc.Signup(context.Background(), email, "a long enough password")
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *env) do(cookie, method, path string, body any) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: cookie})
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
	return v
}

func laAccount(id, token string) linkedin.Account {
	n := strings.TrimPrefix(id, "la-") // keep the Linked API id itself out of visible fields
	return linkedin.Account{
		ID: id, Name: "Ada #" + n, URL: "https://www.linkedin.com/in/person-" + n, Headline: "CTO",
		IdentificationToken: token, Status: linkedin.AccountActive, ConnectedAt: time.Now(),
	}
}

// connect runs the full happy path for cookie's user and returns the account.
func (e *env) connect(cookie, laID, token string) Account {
	e.t.Helper()
	rec := e.do(cookie, "POST", "/api/accounts/connect", map[string]string{"timezone": "Asia/Kolkata"})
	if rec.Code != 201 {
		e.t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	acc := decode[accountResponse](e.t, rec).Account
	sess := "sess-" + strings.TrimPrefix(acc.ConnectionLink, "https://app.linkedapi.io/connection/sess-")
	e.fake.FinishSession(sess, laAccount(laID, token))
	rec = e.do(cookie, "POST", "/api/accounts/"+itoa(acc.ID)+"/refresh", nil)
	if rec.Code != 200 {
		e.t.Fatalf("refresh: %d %s", rec.Code, rec.Body)
	}
	return decode[accountResponse](e.t, rec).Account
}

func itoa(n int64) string    { return strings.TrimSpace(strings.Trim(jsonNum(n), "\"")) }
func jsonNum(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestConnectFlow(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")

	rec := e.do(u, "POST", "/api/accounts/connect", map[string]string{"timezone": "Asia/Kolkata"})
	if rec.Code != 201 {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	pending := decode[accountResponse](t, rec).Account
	if pending.Status != StatusPending || !strings.HasPrefix(pending.ConnectionLink, "https://app.linkedapi.io/connection/") {
		t.Fatalf("pending: %+v", pending)
	}

	// Still waiting on the user: refresh keeps it pending.
	rec = e.do(u, "POST", "/api/accounts/"+itoa(pending.ID)+"/refresh", nil)
	if got := decode[accountResponse](t, rec).Account; got.Status != StatusPending {
		t.Fatalf("expected still pending, got %+v", got)
	}

	// Login finished in LinkedIn, but the account is not created yet.
	sess := "sess-1"
	e.fake.SetSessionStatus(sess, linkedin.SessionSuccess)
	rec = e.do(u, "POST", "/api/accounts/"+itoa(pending.ID)+"/refresh", nil)
	if got := decode[accountResponse](t, rec).Account; got.Status != StatusPending {
		t.Fatalf("expected pending while account is being created, got %+v", got)
	}

	// Account appears.
	e.fake.AddAccount(laAccount("la-1", "IDENT-TOKEN-1"))
	rec = e.do(u, "POST", "/api/accounts/"+itoa(pending.ID)+"/refresh", nil)
	acc := decode[accountResponse](t, rec).Account
	if acc.Status != StatusConnected || acc.Name != "Ada #1" || acc.ConnectedAt == nil {
		t.Fatalf("connected: %+v", acc)
	}

	// The schedule row was created with the requested timezone.
	rec = e.do(u, "GET", "/api/accounts/"+itoa(acc.ID)+"/schedule", nil)
	sv := decode[ScheduleView](t, rec)
	if rec.Code != 200 || sv.Timezone != "Asia/Kolkata" || sv.Start != "09:30" || sv.End != "17:30" ||
		len(sv.WorkDays) != 5 || sv.DailyConnectCap != 5 {
		t.Fatalf("default schedule: %d %+v", rec.Code, sv)
	}
}

func TestTokenEncryptedAtRestAndNeverReturned(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acc := e.connect(u, "la-1", "IDENT-TOKEN-SECRET")

	var stored string
	e.pool.QueryRow(context.Background(), `SELECT identification_token FROM linkedin_accounts WHERE id = $1`, acc.ID).Scan(&stored)
	if strings.Contains(stored, "IDENT-TOKEN-SECRET") || !strings.HasPrefix(stored, "v1:") {
		t.Fatalf("token stored in the clear or unversioned: %q", stored)
	}
	got, err := e.svc.IdentificationToken(context.Background(), acc.ID)
	if err != nil || got != "IDENT-TOKEN-SECRET" {
		t.Fatalf("decrypt: %q %v", got, err)
	}

	// No endpoint output may contain the token or the Linked API account id.
	for _, path := range []string{"/api/accounts", "/api/accounts/" + itoa(acc.ID), "/api/accounts/" + itoa(acc.ID) + "/schedule"} {
		body := e.do(u, "GET", path, nil).Body.String()
		if strings.Contains(body, "IDENT-TOKEN-SECRET") || strings.Contains(body, "identification") || strings.Contains(body, "la-1") {
			t.Fatalf("%s leaks secrets: %s", path, body)
		}
	}
}

func TestTokenBoundToItsRow(t *testing.T) {
	e := newEnv(t)
	a := e.connect(e.user("a@example.com"), "la-1", "TOKEN-A")
	b := e.connect(e.user("b@example.com"), "la-2", "TOKEN-B")

	// Copy A's ciphertext into B's row: it must not decrypt as B.
	_, err := e.pool.Exec(context.Background(),
		`UPDATE linkedin_accounts SET identification_token = (SELECT identification_token FROM linkedin_accounts WHERE id = $1) WHERE id = $2`,
		a.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := e.svc.IdentificationToken(context.Background(), b.ID); err == nil {
		t.Fatalf("swapped ciphertext decrypted as %q", tok)
	}
}

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user("alice@example.com"), e.user("bob@example.com")
	a := e.connect(alice, "la-1", "TOKEN-A")

	own := "/api/accounts/" + itoa(a.ID)
	for _, c := range []struct{ method, path string }{
		{"GET", own},
		{"POST", own + "/refresh"},
		{"POST", own + "/reconnect"},
		{"GET", own + "/schedule"},
		{"PUT", own + "/schedule"},
	} {
		var body any
		if c.method == "PUT" {
			body = validSchedule()
		}
		if rec := e.do(bob, c.method, c.path, body); rec.Code != 404 {
			t.Errorf("bob %s %s: got %d %s, want 404", c.method, c.path, rec.Code, rec.Body)
		}
		if rec := e.do("", c.method, c.path, body); rec.Code != 401 {
			t.Errorf("anonymous %s %s: got %d, want 401", c.method, c.path, rec.Code)
		}
	}

	// Bob's failed write must not have changed Alice's schedule.
	if sv := decode[ScheduleView](t, e.do(alice, "GET", own+"/schedule", nil)); sv.Timezone != "Asia/Kolkata" {
		t.Fatalf("alice's schedule changed: %+v", sv)
	}
	// Lists are per user.
	if accs := decode[struct{ Accounts []Account }](t, e.do(bob, "GET", "/api/accounts", nil)).Accounts; len(accs) != 0 {
		t.Fatalf("bob sees %d accounts", len(accs))
	}
	if accs := decode[struct{ Accounts []Account }](t, e.do(alice, "GET", "/api/accounts", nil)).Accounts; len(accs) != 1 {
		t.Fatalf("alice sees %d accounts", len(accs))
	}
	// Non-numeric ids are just not found.
	if rec := e.do(alice, "GET", "/api/accounts/abc", nil); rec.Code != 404 {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

func TestOnlyOneConnectionAtATime(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user("alice@example.com"), e.user("bob@example.com")

	first := e.do(alice, "POST", "/api/accounts/connect", nil)
	if first.Code != 201 {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	rec := e.do(bob, "POST", "/api/accounts/connect", nil)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "connection_busy") {
		t.Fatalf("second connect: %d %s", rec.Code, rec.Body)
	}
	// The refused attempt left no row and no Linked API session behind.
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM linkedin_accounts WHERE user_id = (SELECT id FROM users WHERE email = 'bob@example.com')`).Scan(&n)
	if n != 0 {
		t.Fatalf("bob has %d rows", n)
	}

	// Once Alice's attempt finishes, Bob can go.
	acc := decode[accountResponse](t, first).Account
	e.fake.FinishSession("sess-1", laAccount("la-1", "T"))
	e.do(alice, "POST", "/api/accounts/"+itoa(acc.ID)+"/refresh", nil)
	if rec := e.do(bob, "POST", "/api/accounts/connect", nil); rec.Code != 201 {
		t.Fatalf("bob after alice: %d %s", rec.Code, rec.Body)
	}
}

func TestStalePendingDoesNotBlockForever(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user("alice@example.com"), e.user("bob@example.com")
	if rec := e.do(alice, "POST", "/api/accounts/connect", nil); rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	e.pool.Exec(context.Background(), `UPDATE linkedin_accounts SET created_at = now() - interval '11 minutes'`)

	if rec := e.do(bob, "POST", "/api/accounts/connect", nil); rec.Code != 201 {
		t.Fatalf("bob blocked by stale attempt: %d %s", rec.Code, rec.Body)
	}
	accs := decode[struct{ Accounts []Account }](t, e.do(alice, "GET", "/api/accounts", nil)).Accounts
	if len(accs) != 1 || accs[0].Status != StatusError {
		t.Fatalf("alice's stale attempt: %+v", accs)
	}
	if st, _ := e.fake.GetConnectionSession(context.Background(), "sess-1"); st != linkedin.SessionCancelled {
		t.Fatalf("stale session not cancelled: %q", st)
	}
}

func TestAmbiguousNewAccountIsNotAssigned(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acc := decode[accountResponse](t, e.do(u, "POST", "/api/accounts/connect", nil)).Account

	// Two new Linked API accounts appear (e.g. one made in their dashboard meanwhile).
	e.fake.FinishSession("sess-1", laAccount("la-1", "T1"))
	e.fake.AddAccount(laAccount("la-2", "T2"))

	rec := e.do(u, "POST", "/api/accounts/"+itoa(acc.ID)+"/refresh", nil)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "connect_ambiguous") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	got := decode[accountResponse](t, e.do(u, "GET", "/api/accounts/"+itoa(acc.ID), nil)).Account
	if got.Status != StatusError {
		t.Fatalf("status %q", got.Status)
	}
	var tok *string
	e.pool.QueryRow(context.Background(), `SELECT identification_token FROM linkedin_accounts WHERE id = $1`, acc.ID).Scan(&tok)
	if tok != nil {
		t.Fatal("an account was attached despite ambiguity")
	}
}

func TestAlreadyClaimedAccountsAreIgnored(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user("alice@example.com"), e.user("bob@example.com")
	e.connect(alice, "la-1", "TA") // la-1 is now Alice's

	// Bob connects. Even if la-1 were missing from `known` (e.g. a stale snapshot), it is claimed.
	acc := decode[accountResponse](t, e.do(bob, "POST", "/api/accounts/connect", nil)).Account
	e.pool.Exec(context.Background(), `UPDATE linkedin_accounts SET known_account_ids = '{}' WHERE id = $1`, acc.ID)
	e.fake.FinishSession("sess-2", laAccount("la-2", "TB"))
	got := decode[accountResponse](t, e.do(bob, "POST", "/api/accounts/"+itoa(acc.ID)+"/refresh", nil)).Account
	if got.Status != StatusConnected || got.Name != "Ada #2" {
		t.Fatalf("bob got %+v, want la-2", got)
	}
}

func TestSessionFailureStates(t *testing.T) {
	for _, st := range []string{linkedin.SessionExpired, linkedin.SessionError, linkedin.SessionCancelled} {
		e := newEnv(t)
		u := e.user("a@example.com")
		acc := decode[accountResponse](t, e.do(u, "POST", "/api/accounts/connect", nil)).Account
		e.fake.SetSessionStatus("sess-1", st)
		got := decode[accountResponse](t, e.do(u, "POST", "/api/accounts/"+itoa(acc.ID)+"/refresh", nil)).Account
		if got.Status != StatusError || !strings.Contains(got.StatusDetail, st) {
			t.Errorf("%s: %+v", st, got)
		}
		// A new attempt is allowed afterwards.
		if rec := e.do(u, "POST", "/api/accounts/connect", nil); rec.Code != 201 {
			t.Errorf("%s: retry blocked: %d %s", st, rec.Code, rec.Body)
		}
	}
}

func TestLinkedAPIErrorsAreMapped(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")

	e.fake.Err["ListAccounts"] = &linkedin.Error{Type: "noAvailableSeats", Status: 400, Message: "internal detail"}
	rec := e.do(u, "POST", "/api/accounts/connect", nil)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "no_seats") || strings.Contains(rec.Body.String(), "internal detail") {
		t.Fatalf("no seats: %d %s", rec.Code, rec.Body)
	}

	e.fake.Err["CreateConnectionSession"] = &linkedin.Error{Type: "tooManyRequests", Status: 429, Message: "upstream detail"}
	rec = e.do(u, "POST", "/api/accounts/connect", nil)
	if rec.Code != 502 || strings.Contains(rec.Body.String(), "upstream detail") {
		t.Fatalf("transient: %d %s", rec.Code, rec.Body)
	}
	// The failed attempt rolled back, so a retry is not blocked.
	if rec := e.do(u, "POST", "/api/accounts/connect", nil); rec.Code != 201 {
		t.Fatalf("retry after failure: %d %s", rec.Code, rec.Body)
	}
}

func TestRefreshSyncsStatusAndDisconnect(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acc := e.connect(u, "la-1", "T1")
	path := "/api/accounts/" + itoa(acc.ID)

	e.fake.SetAccountStatus("la-1", linkedin.AccountReconnectionRequired)
	got := decode[accountResponse](t, e.do(u, "POST", path+"/refresh", nil)).Account
	if got.Status != StatusReconnectionRequired {
		t.Fatalf("status %q", got.Status)
	}

	// Reconnect link is issued only for accounts that need it.
	rec := e.do(u, "POST", path+"/reconnect", nil)
	link := decode[map[string]string](t, rec)["reconnection_link"]
	if rec.Code != 200 || !strings.HasPrefix(link, "https://app.linkedapi.io/connection/") {
		t.Fatalf("reconnect: %d %s", rec.Code, rec.Body)
	}
	e.fake.SetAccountStatus("la-1", linkedin.AccountActive)
	if got := decode[accountResponse](t, e.do(u, "POST", path+"/refresh", nil)).Account; got.Status != StatusConnected {
		t.Fatalf("after reconnect: %q", got.Status)
	}
	if rec := e.do(u, "POST", path+"/reconnect", nil); rec.Code != 409 {
		t.Fatalf("reconnect on healthy account: %d", rec.Code)
	}

	e.fake.RemoveAccount("la-1")
	if got := decode[accountResponse](t, e.do(u, "POST", path+"/refresh", nil)).Account; got.Status != StatusDisconnected {
		t.Fatalf("after removal: %q", got.Status)
	}
}

func TestCipher(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	c, _ := NewCipher(key)
	a, _ := c.Encrypt("secret", "row:1")
	b, _ := c.Encrypt("secret", "row:1")
	if a == b {
		t.Fatal("nonce reused: identical ciphertexts")
	}
	if got, err := c.Decrypt(a, "row:1"); err != nil || got != "secret" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := c.Decrypt(a, "row:2"); err == nil {
		t.Fatal("decrypted under a different binding")
	}
	tampered := a[:len(a)-2] + "AA"
	if _, err := c.Decrypt(tampered, "row:1"); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	other := make([]byte, 32)
	rand.Read(other)
	c2, _ := NewCipher(other)
	if _, err := c2.Decrypt(a, "row:1"); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
	for _, bad := range []string{"", "plain", "v1:", "v1:!!!", "v2:AAAA"} {
		if _, err := c.Decrypt(bad, "row:1"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := NewCipher(make([]byte, 16)); err == nil {
		t.Fatal("short key accepted")
	}
}

func validSchedule() ScheduleSettings {
	return ScheduleSettings{
		Timezone: "Europe/London", WorkDays: []int{1, 2, 3, 4}, Start: "10:00", End: "16:30",
		Holidays: []string{"2026-12-25", "2026-12-24"}, DailyConnectCap: 8, DailyMessageCap: 20,
	}
}

func TestScheduleUpdate(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acc := e.connect(u, "la-1", "T")
	path := "/api/accounts/" + itoa(acc.ID) + "/schedule"

	in := validSchedule()
	in.WorkDays = []int{4, 1, 3, 2, 2} // unordered with a duplicate
	rec := e.do(u, "PUT", path, in)
	if rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	got := decode[ScheduleView](t, e.do(u, "GET", path, nil))
	want := validSchedule()
	if got.Timezone != want.Timezone || got.Start != "10:00" || got.End != "16:30" ||
		len(got.WorkDays) != 4 || got.WorkDays[0] != 1 || got.WorkDays[3] != 4 ||
		len(got.Holidays) != 2 || got.Holidays[0] != "2026-12-24" ||
		got.DailyConnectCap != 8 || got.DailyMessageCap != 20 {
		t.Fatalf("stored schedule: %+v", got)
	}
	// next_send_window is a working moment: weekday Mon-Thu, inside the window, in London time.
	loc, _ := time.LoadLocation("Europe/London")
	nw := got.NextSendWindow.In(loc)
	mins := nw.Hour()*60 + nw.Minute()
	if wd := nw.Weekday(); wd < time.Monday || wd > time.Thursday || mins < 600 || mins >= 990 {
		t.Fatalf("next_send_window %v is outside the configured schedule", nw)
	}
}

func TestScheduleValidation(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acc := e.connect(u, "la-1", "T")
	path := "/api/accounts/" + itoa(acc.ID) + "/schedule"
	before := decode[ScheduleView](t, e.do(u, "GET", path, nil))

	mut := func(f func(*ScheduleSettings)) ScheduleSettings { s := validSchedule(); f(&s); return s }
	cases := map[string]ScheduleSettings{
		"unknown timezone":  mut(func(s *ScheduleSettings) { s.Timezone = "Mars/Base" }),
		"empty timezone":    mut(func(s *ScheduleSettings) { s.Timezone = "" }),
		"no work days":      mut(func(s *ScheduleSettings) { s.WorkDays = nil }),
		"weekday 7":         mut(func(s *ScheduleSettings) { s.WorkDays = []int{7} }),
		"negative weekday":  mut(func(s *ScheduleSettings) { s.WorkDays = []int{-1} }),
		"start after end":   mut(func(s *ScheduleSettings) { s.Start, s.End = "17:00", "09:00" }),
		"empty window":      mut(func(s *ScheduleSettings) { s.Start, s.End = "09:00", "09:00" }),
		"bad time format":   mut(func(s *ScheduleSettings) { s.Start = "9am" }),
		"bad end":           mut(func(s *ScheduleSettings) { s.End = "25:00" }),
		"bad holiday":       mut(func(s *ScheduleSettings) { s.Holidays = []string{"25-12-2026"} }),
		"connect cap high":  mut(func(s *ScheduleSettings) { s.DailyConnectCap = MaxDailyConnectCap + 1 }),
		"connect cap neg":   mut(func(s *ScheduleSettings) { s.DailyConnectCap = -1 }),
		"message cap high":  mut(func(s *ScheduleSettings) { s.DailyMessageCap = MaxDailyMessageCap + 1 }),
		"too many holidays": mut(func(s *ScheduleSettings) { s.Holidays = manyDates(101) }),
	}
	for name, in := range cases {
		rec := e.do(u, "PUT", path, in)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_schedule") {
			t.Errorf("%s: got %d %s", name, rec.Code, rec.Body)
		}
	}
	// Unknown JSON fields are refused too.
	if rec := e.do(u, "PUT", path, map[string]any{"timezone": "UTC", "admin": true}); rec.Code != 400 {
		t.Errorf("unknown field: %d", rec.Code)
	}
	// None of the rejected updates changed anything.
	after := decode[ScheduleView](t, e.do(u, "GET", path, nil))
	if after.Timezone != before.Timezone || after.Start != before.Start || after.DailyConnectCap != before.DailyConnectCap {
		t.Fatalf("schedule changed by rejected input: %+v -> %+v", before, after)
	}
	// 24:00 end is allowed.
	ok := validSchedule()
	ok.End = "24:00"
	if rec := e.do(u, "PUT", path, ok); rec.Code != 200 {
		t.Fatalf("24:00: %d %s", rec.Code, rec.Body)
	}
}

func manyDates(n int) []string {
	out := make([]string, n)
	d := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range out {
		out[i] = d.AddDate(0, 0, i).Format("2006-01-02")
	}
	return out
}

func TestConnectValidatesTimezone(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	rec := e.do(u, "POST", "/api/accounts/connect", map[string]string{"timezone": "Nowhere/Land"})
	if rec.Code != 400 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	for _, c := range e.fake.Calls {
		if c == "CreateConnectionSession" {
			t.Fatal("Linked API session created for an invalid request")
		}
	}
}
