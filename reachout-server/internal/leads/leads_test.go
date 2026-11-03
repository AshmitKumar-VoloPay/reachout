package leads

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ashmit/reachout-server/internal/accounts"
	"github.com/ashmit/reachout-server/internal/auth"
	"github.com/ashmit/reachout-server/internal/httpapi"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	svc   *Service
	accts *accounts.Service
	fake  *linkedin.Fake
	h     http.Handler
	auth  *auth.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testdb.New(t)
	key := make([]byte, 32)
	rand.Read(key)
	c, _ := accounts.NewCipher(key)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := linkedin.NewFake()
	accts := accounts.NewService(pool, fake, c, log)
	svc := NewService(pool, fake, accts, log)

	mux, handler := httpapi.NewRouter(httpapi.Deps{DB: pool, Log: log, AllowedOrigins: []string{"http://localhost:5173"}})
	aSvc := auth.NewService(pool)
	ah := auth.NewHandler(aSvc, auth.Options{Log: log})
	ah.Register(mux)
	accounts.NewHandler(accts, ah, log).Register(mux)
	NewHandler(svc, ah, log).Register(mux)
	return &env{t: t, pool: pool, svc: svc, accts: accts, fake: fake, h: handler, auth: aSvc}
}

type user struct {
	id     int64
	cookie string
}

func (e *env) user(email string) user {
	e.t.Helper()
	u, tok, _, err := e.auth.Signup(context.Background(), email, "a long enough password")
	if err != nil {
		e.t.Fatal(err)
	}
	return user{id: u.ID, cookie: tok}
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

// connectedAccount links a fake LinkedIn account to u and returns its row id. The
// schedule is set to "always open" so tests do not depend on the clock.
func (e *env) connectedAccount(u user, laID string) int64 {
	e.t.Helper()
	ctx := context.Background()
	acc, _, err := e.accts.StartConnect(ctx, u.id, "UTC")
	if err != nil {
		e.t.Fatal(err)
	}
	e.fake.FinishSession(fmt.Sprintf("sess-%d", len(e.fake.Accounts)+1), linkedin.Account{
		ID: laID, Name: "Account " + laID, IdentificationToken: "TOKEN-" + laID, ConnectedAt: time.Now(),
	})
	if got, err := e.accts.Refresh(ctx, u.id, acc.ID); err != nil || got.Status != accounts.StatusConnected {
		e.t.Fatalf("connect: %+v %v", got, err)
	}
	e.setSchedule(u, acc.ID, nil)
	return acc.ID
}

// setSchedule makes the account open all day, every day, except on the given holidays.
func (e *env) setSchedule(u user, accountID int64, holidays []string) {
	e.t.Helper()
	_, err := e.accts.SetSchedule(context.Background(), u.id, accountID, accounts.ScheduleSettings{
		Timezone: "UTC", WorkDays: []int{0, 1, 2, 3, 4, 5, 6}, Start: "00:00", End: "24:00",
		Holidays: holidays, DailyConnectCap: 5, DailyMessageCap: 10,
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func profile(slug string) string { return "https://www.linkedin.com/in/" + slug }

func (e *env) count(userID int64) int {
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM leads WHERE user_id = $1`, userID).Scan(&n)
	return n
}

func TestImportURLsDedupesAndReports(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")

	text := strings.Join([]string{
		"https://www.linkedin.com/in/jane-doe",
		"linkedin.com/in/Jane-Doe/", // same person, different spelling
		"https://uk.linkedin.com/in/john-roe?trk=x, https://www.linkedin.com/in/ann-lee",
		"https://www.linkedin.com/company/acme", // not a profile
		"https://evil.example/in/mallory",       // not LinkedIn
		"not-a-url",
	}, "\n")
	rec := e.do(u.cookie, "POST", "/api/leads/import/urls", map[string]string{"text": text})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	res := decode[ImportResult](t, rec)
	if res.Imported != 3 || res.Duplicates != 1 || len(res.Invalid) != 3 || res.OverCap != 0 {
		t.Fatalf("first import: %+v", res)
	}
	if e.count(u.id) != 3 {
		t.Fatalf("stored %d leads, want 3", e.count(u.id))
	}

	// Importing the same list again creates nothing.
	rec = e.do(u.cookie, "POST", "/api/leads/import/urls", map[string]string{"text": text})
	res = decode[ImportResult](t, rec)
	if res.Imported != 0 || res.Duplicates != 4 || e.count(u.id) != 3 {
		t.Fatalf("second import: %+v, stored %d", res, e.count(u.id))
	}

	// The urls array works too, and both inputs combine.
	rec = e.do(u.cookie, "POST", "/api/leads/import/urls", map[string]any{
		"urls": []string{"linkedin.com/in/new-person"}, "text": "linkedin.com/in/ann-lee",
	})
	res = decode[ImportResult](t, rec)
	if res.Imported != 1 || res.Duplicates != 1 {
		t.Fatalf("combined input: %+v", res)
	}

	// Stored URLs are the canonical form.
	var u1 string
	e.pool.QueryRow(context.Background(), `SELECT profile_url FROM leads WHERE user_id = $1 AND profile_url LIKE '%john%'`, u.id).Scan(&u1)
	if u1 != profile("john-roe") {
		t.Fatalf("stored %q", u1)
	}
}

func TestImportURLsInputLimits(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	for name, body := range map[string]any{
		"empty":       map[string]string{"text": "  \n "},
		"nothing":     map[string]string{},
		"unknown key": map[string]any{"text": "linkedin.com/in/x", "account_id": 1},
	} {
		if rec := e.do(u.cookie, "POST", "/api/leads/import/urls", body); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	many := make([]string, MaxPasteURLs+1)
	for i := range many {
		many[i] = profile(fmt.Sprintf("p%d", i))
	}
	rec := e.do(u.cookie, "POST", "/api/leads/import/urls", map[string]any{"urls": many})
	if rec.Code != 400 || e.count(u.id) != 0 {
		t.Fatalf("%d paste URLs: %d, stored %d", len(many), rec.Code, e.count(u.id))
	}
	// Exactly the limit is accepted.
	rec = e.do(u.cookie, "POST", "/api/leads/import/urls", map[string]any{"urls": many[:MaxPasteURLs]})
	if rec.Code != 200 || e.count(u.id) != MaxPasteURLs {
		t.Fatalf("at the limit: %d, stored %d", rec.Code, e.count(u.id))
	}
}

func TestPerUserLeadLimit(t *testing.T) {
	e := newEnv(t)
	e.svc.MaxLeads = 3
	a, b := e.user("a@example.com"), e.user("b@example.com")

	res, err := e.svc.ImportURLs(context.Background(), a.id, "", []string{profile("a1"), profile("a2")})
	if err != nil || res.Imported != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	// One slot left: the first new person fits, the rest are skipped, an existing one is a duplicate.
	res, err = e.svc.ImportURLs(context.Background(), a.id, "", []string{profile("a1"), profile("a3"), profile("a4"), profile("a5")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 1 || res.Duplicates != 1 || res.OverCap != 2 || e.count(a.id) != 3 {
		t.Fatalf("%+v, stored %d", res, e.count(a.id))
	}
	// At the limit a re-import of existing leads still works and adds nothing.
	res, _ = e.svc.ImportURLs(context.Background(), a.id, "", []string{profile("a1"), profile("zz")})
	if res.Duplicates != 1 || res.OverCap != 1 || e.count(a.id) != 3 {
		t.Fatalf("at limit: %+v", res)
	}
	// The limit is per user.
	res, _ = e.svc.ImportURLs(context.Background(), b.id, "", []string{profile("a1"), profile("b2")})
	if res.Imported != 2 || e.count(b.id) != 2 {
		t.Fatalf("other user: %+v", res)
	}
}

func TestConcurrentImportsRespectLimit(t *testing.T) {
	e := newEnv(t)
	e.svc.MaxLeads = 10
	u := e.user("a@example.com")
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			urls := make([]string, 5)
			for i := range urls {
				urls[i] = profile(fmt.Sprintf("g%d-p%d", g, i))
			}
			e.svc.ImportURLs(context.Background(), u.id, "", urls)
		}(g)
	}
	wg.Wait()
	if n := e.count(u.id); n != 10 {
		t.Fatalf("stored %d leads under a limit of 10", n)
	}
}

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user("alice@example.com"), e.user("bob@example.com")
	e.do(alice.cookie, "POST", "/api/leads/import/urls", map[string]string{"text": profile("shared") + " " + profile("only-alice")})
	e.do(bob.cookie, "POST", "/api/leads/import/urls", map[string]string{"text": profile("shared")})

	// The same person is a separate lead for each user.
	if e.count(alice.id) != 2 || e.count(bob.id) != 1 {
		t.Fatalf("alice %d, bob %d", e.count(alice.id), e.count(bob.id))
	}
	var aliceLead int64
	e.pool.QueryRow(context.Background(), `SELECT id FROM leads WHERE user_id = $1 AND profile_url LIKE '%only-alice'`, alice.id).Scan(&aliceLead)
	path := fmt.Sprintf("/api/leads/%d", aliceLead)

	if rec := e.do(bob.cookie, "GET", path, nil); rec.Code != 404 {
		t.Errorf("bob GET alice's lead: %d", rec.Code)
	}
	if rec := e.do(bob.cookie, "DELETE", path, nil); rec.Code != 404 {
		t.Errorf("bob DELETE alice's lead: %d", rec.Code)
	}
	rec := e.do(bob.cookie, "POST", "/api/leads/bulk-delete", map[string]any{"ids": []int64{aliceLead}})
	if rec.Code != 200 || decode[map[string]int64](t, rec)["deleted"] != 0 {
		t.Errorf("bob bulk-delete: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("", "GET", "/api/leads", nil); rec.Code != 401 {
		t.Errorf("anonymous list: %d", rec.Code)
	}
	// Alice's lead survived all of that, and Bob never sees it in a list or search.
	if e.count(alice.id) != 2 {
		t.Fatal("alice's lead was deleted by bob")
	}
	page := decode[Page](t, e.do(bob.cookie, "GET", "/api/leads?q=only-alice", nil))
	if page.Total != 0 || len(page.Leads) != 0 {
		t.Fatalf("bob found alice's lead: %+v", page)
	}
	if page := decode[Page](t, e.do(bob.cookie, "GET", "/api/leads", nil)); page.Total != 1 {
		t.Fatalf("bob total %d, want 1", page.Total)
	}
}

func TestListSearchAndPaging(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	ctx := context.Background()
	for i := 1; i <= 7; i++ {
		e.pool.Exec(ctx, `INSERT INTO leads (user_id, profile_url, name, headline, company) VALUES ($1, $2, $3, $4, $5)`,
			u.id, profile(fmt.Sprintf("person-%d", i)), fmt.Sprintf("Person %d", i), "Engineer", "Acme")
	}
	e.pool.Exec(ctx, `INSERT INTO leads (user_id, profile_url, name, headline, company) VALUES ($1, $2, 'Zed 100%', 'CTO', 'Under_Score Inc')`,
		u.id, profile("zed"))

	page := decode[Page](t, e.do(u.cookie, "GET", "/api/leads?limit=3", nil))
	if page.Total != 8 || len(page.Leads) != 3 || page.Limit != 3 || page.Leads[0].Name != "Zed 100%" {
		t.Fatalf("page 1: %+v", page)
	}
	page2 := decode[Page](t, e.do(u.cookie, "GET", "/api/leads?limit=3&offset=3", nil))
	if len(page2.Leads) != 3 || page2.Leads[0].ID >= page.Leads[2].ID {
		t.Fatalf("page 2 overlaps or is out of order: %+v", page2)
	}
	if last := decode[Page](t, e.do(u.cookie, "GET", "/api/leads?limit=3&offset=6", nil)); len(last.Leads) != 2 {
		t.Fatalf("last page: %+v", last)
	}
	// Search is case-insensitive and covers name, headline, company and URL.
	for q, want := range map[string]int{"person 3": 1, "ENGINEER": 7, "acme": 7, "person-5": 1, "cto": 1, "nobody": 0} {
		if got := decode[Page](t, e.do(u.cookie, "GET", "/api/leads?q="+strings.ReplaceAll(q, " ", "%20"), nil)).Total; got != want {
			t.Errorf("q=%q: total %d, want %d", q, got, want)
		}
	}
	// LIKE wildcards in the query are literal text, not patterns.
	if got := decode[Page](t, e.do(u.cookie, "GET", "/api/leads?q=%25", nil)).Total; got != 1 {
		t.Errorf("q=%%: total %d, want 1 (only the lead containing a percent sign)", got)
	}
	if got := decode[Page](t, e.do(u.cookie, "GET", "/api/leads?q=_", nil)).Total; got != 1 {
		t.Errorf("q=_: total %d, want 1", got)
	}
	// Page size is capped and bad numbers fall back to defaults.
	if p := decode[Page](t, e.do(u.cookie, "GET", "/api/leads?limit=100000&offset=-5", nil)); p.Limit != MaxPageSize || p.Offset != 0 {
		t.Fatalf("limits: %+v", p)
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	e.svc.ImportURLs(context.Background(), u.id, "", []string{profile("a"), profile("b"), profile("c")})
	page := decode[Page](t, e.do(u.cookie, "GET", "/api/leads", nil))

	if rec := e.do(u.cookie, "DELETE", fmt.Sprintf("/api/leads/%d", page.Leads[0].ID), nil); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := e.do(u.cookie, "DELETE", fmt.Sprintf("/api/leads/%d", page.Leads[0].ID), nil); rec.Code != 404 {
		t.Fatalf("delete twice: %d", rec.Code)
	}
	rec := e.do(u.cookie, "POST", "/api/leads/bulk-delete", map[string]any{"ids": []int64{page.Leads[1].ID, page.Leads[2].ID, 999999}})
	if rec.Code != 200 || decode[map[string]int64](t, rec)["deleted"] != 2 || e.count(u.id) != 0 {
		t.Fatalf("bulk: %d %s", rec.Code, rec.Body)
	}
	for _, ids := range [][]int64{nil, make([]int64, MaxBulkDelete+1)} {
		if rec := e.do(u.cookie, "POST", "/api/leads/bulk-delete", map[string]any{"ids": ids}); rec.Code != 400 {
			t.Errorf("bulk with %d ids: %d", len(ids), rec.Code)
		}
	}
	// Re-importing a deleted person works.
	if res, _ := e.svc.ImportURLs(context.Background(), u.id, "", []string{profile("a")}); res.Imported != 1 {
		t.Fatalf("re-import: %+v", res)
	}
	if rec := e.do(u.cookie, "DELETE", "/api/leads/notanumber", nil); rec.Code != 404 {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

const searchURL = "https://www.linkedin.com/search/results/people/?keywords=cto&origin=FACETED_SEARCH"

func searchResult(people ...map[string]any) linkedin.WorkflowResult {
	data, _ := json.Marshal(people)
	comp := fmt.Sprintf(`{"actionType":"st.searchPeople","success":true,"data":%s}`, data)
	return linkedin.WorkflowResult{Status: "completed", Completion: json.RawMessage(comp)}
}

func person(name, url string) map[string]any {
	return map[string]any{"name": name, "publicUrl": url, "headline": "CTO at Acme", "location": "London"}
}

func TestSearchImportLifecycle(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.connectedAccount(u, "la-1")

	var started map[string]any
	var startToken string
	e.fake.Workflow = func(token string, action map[string]any) (linkedin.WorkflowStart, error) {
		started, startToken = action, token
		return linkedin.WorkflowStart{ID: "wf-search-1", Status: "pending", PendingReason: "queued"}, nil
	}
	rec := e.do(u.cookie, "POST", "/api/leads/import/search", map[string]any{"account_id": acct, "search_url": searchURL, "limit": 5})
	if rec.Code != 202 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	imp := decode[map[string]Import](t, rec)["import"]
	if imp.Status != "running" || imp.Requested != 5 || imp.PendingReason != "queued" {
		t.Fatalf("started import: %+v", imp)
	}
	if started["actionType"] != "st.searchPeople" || started["customSearchUrl"] != searchURL || started["limit"] != 5 {
		t.Fatalf("workflow action: %v", started)
	}
	if startToken != "TOKEN-la-1" {
		t.Fatalf("workflow used token %q, want the account's decrypted token", startToken)
	}
	path := fmt.Sprintf("/api/leads/imports/%d", imp.ID)

	// Still queued on LinkedIn's side.
	e.fake.GetWorkflowFn = func(string, string) (linkedin.WorkflowResult, error) {
		return linkedin.WorkflowResult{Status: "pending", PendingReason: "queued"}, nil
	}
	got := decode[map[string]Import](t, e.do(u.cookie, "GET", path, nil))["import"]
	if got.Status != "running" || got.PendingReason != "queued" || e.count(u.id) != 0 {
		t.Fatalf("while queued: %+v", got)
	}

	// Done: three people, one repeated spelling, one without a URL, one already a lead.
	e.svc.ImportURLs(context.Background(), u.id, "", []string{profile("existing")})
	e.fake.GetWorkflowFn = func(string, string) (linkedin.WorkflowResult, error) {
		return searchResult(
			person("Jane Doe", "https://www.linkedin.com/in/jane-doe/"),
			person("Jane Doe again", "linkedin.com/in/JANE-DOE"),
			person("John Roe", "https://www.linkedin.com/in/john-roe"),
			person("Existing", "https://www.linkedin.com/in/existing"),
			map[string]any{"name": "Hidden Profile", "publicUrl": nil},
		), nil
	}
	got = decode[map[string]Import](t, e.do(u.cookie, "GET", path, nil))["import"]
	if got.Status != "completed" || got.Found != 5 || got.Imported != 2 || got.Duplicates != 2 || got.Invalid != 1 || got.FinishedAt == nil {
		t.Fatalf("completed import: %+v", got)
	}
	var name, headline string
	e.pool.QueryRow(context.Background(), `SELECT name, headline FROM leads WHERE user_id = $1 AND profile_url = $2`,
		u.id, profile("jane-doe")).Scan(&name, &headline)
	if name != "Jane Doe" || headline != "CTO at Acme" {
		t.Fatalf("lead data: %q %q", name, headline)
	}
	// The pre-existing blank-name lead was enriched from the search result.
	e.pool.QueryRow(context.Background(), `SELECT name FROM leads WHERE user_id = $1 AND profile_url = $2`, u.id, profile("existing")).Scan(&name)
	if name != "Existing" {
		t.Fatalf("existing lead not enriched: %q", name)
	}

	// Polling again changes nothing, and a new search is allowed once the first has finished.
	again := decode[map[string]Import](t, e.do(u.cookie, "GET", path, nil))["import"]
	if again.Imported != 2 || e.count(u.id) != 3 {
		t.Fatalf("second poll: %+v, %d leads", again, e.count(u.id))
	}
	if rec := e.do(u.cookie, "POST", "/api/leads/import/search", map[string]any{"account_id": acct, "search_url": searchURL}); rec.Code != 202 {
		t.Fatalf("second search: %d %s", rec.Code, rec.Body)
	}
}

func TestConcurrentPollsImportOnce(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.connectedAccount(u, "la-1")
	imp, err := e.svc.StartSearch(context.Background(), u.id, acct, searchURL, 10)
	if err != nil {
		t.Fatal(err)
	}
	e.fake.GetWorkflowFn = func(string, string) (linkedin.WorkflowResult, error) {
		return searchResult(person("A", profile("a")), person("B", profile("b")), person("C", profile("c"))), nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.svc.GetImport(context.Background(), u.id, imp.ID)
		}()
	}
	wg.Wait()
	final, _ := e.svc.GetImport(context.Background(), u.id, imp.ID)
	if final.Status != "completed" || final.Imported != 3 || e.count(u.id) != 3 {
		t.Fatalf("%+v, %d leads", final, e.count(u.id))
	}
}

func TestSearchRespectsWorkingHours(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.connectedAccount(u, "la-1")

	now := time.Now().UTC()
	var hols []string
	for d := -1; d <= 1; d++ {
		hols = append(hols, now.AddDate(0, 0, d).Format("2006-01-02"))
	}
	e.setSchedule(u, acct, hols) // closed: today and its neighbours are holidays

	rec := e.do(u.cookie, "POST", "/api/leads/import/search", map[string]any{"account_id": acct, "search_url": searchURL})
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "outside_working_hours") {
		t.Fatalf("closed: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), now.AddDate(0, 0, 2).Format("2006-01-02")) {
		t.Fatalf("message should name the next window (after the holidays): %s", rec.Body)
	}
	for _, c := range e.fake.Calls {
		if c == "StartWorkflow" {
			t.Fatal("a LinkedIn workflow was started outside working hours")
		}
	}
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM lead_imports`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d import rows left behind", n)
	}

	e.setSchedule(u, acct, nil) // open again
	if rec := e.do(u.cookie, "POST", "/api/leads/import/search", map[string]any{"account_id": acct, "search_url": searchURL}); rec.Code != 202 {
		t.Fatalf("open: %d %s", rec.Code, rec.Body)
	}
}

func TestSearchValidationAndOwnership(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user("alice@example.com"), e.user("bob@example.com")
	acct := e.connectedAccount(alice, "la-1")
	start := func(c string, body map[string]any) *httptest.ResponseRecorder {
		return e.do(c, "POST", "/api/leads/import/search", body)
	}

	cases := map[string]map[string]any{
		"not linkedin":   {"account_id": acct, "search_url": "https://example.com/search/results/people/?keywords=x"},
		"company search": {"account_id": acct, "search_url": "https://www.linkedin.com/search/results/companies/?keywords=x"},
		"profile url":    {"account_id": acct, "search_url": profile("jane")},
		"empty url":      {"account_id": acct, "search_url": ""},
		"limit too high": {"account_id": acct, "search_url": searchURL, "limit": MaxSearchLimit + 1},
		"negative limit": {"account_id": acct, "search_url": searchURL, "limit": -1},
		"unknown field":  {"account_id": acct, "search_url": searchURL, "term": "x"},
	}
	for name, body := range cases {
		if rec := start(alice.cookie, body); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// Bob cannot search with Alice's LinkedIn account, and cannot read her imports.
	if rec := start(bob.cookie, map[string]any{"account_id": acct, "search_url": searchURL}); rec.Code != 404 {
		t.Errorf("bob using alice's account: %d %s", rec.Code, rec.Body)
	}
	if rec := start(alice.cookie, map[string]any{"account_id": 999999, "search_url": searchURL}); rec.Code != 404 {
		t.Errorf("unknown account: %d", rec.Code)
	}
	rec := start(alice.cookie, map[string]any{"account_id": acct, "search_url": searchURL})
	if rec.Code != 202 {
		t.Fatalf("alice: %d %s", rec.Code, rec.Body)
	}
	impPath := fmt.Sprintf("/api/leads/imports/%d", decode[map[string]Import](t, rec)["import"].ID)
	if rec := e.do(bob.cookie, "GET", impPath, nil); rec.Code != 404 {
		t.Errorf("bob reading alice's import: %d", rec.Code)
	}
	// One running import per account.
	if rec := start(alice.cookie, map[string]any{"account_id": acct, "search_url": searchURL}); rec.Code != 409 || !strings.Contains(rec.Body.String(), "import_in_progress") {
		t.Errorf("second concurrent import: %d %s", rec.Code, rec.Body)
	}
	// Account that is not connected.
	pending, _, _ := e.accts.StartConnect(context.Background(), bob.id, "UTC")
	if rec := start(bob.cookie, map[string]any{"account_id": pending.ID, "search_url": searchURL}); rec.Code != 409 || !strings.Contains(rec.Body.String(), "account_not_connected") {
		t.Errorf("pending account: %d %s", rec.Code, rec.Body)
	}
}

func TestSearchFailureModes(t *testing.T) {
	type tc struct {
		name   string
		result linkedin.WorkflowResult
		want   string
	}
	cases := []tc{
		{"signed out", linkedin.WorkflowResult{Status: "failed", Failure: &linkedin.WorkflowFailure{Reason: "linkedinAccountSignedOut"}}, "signed out"},
		{"language", linkedin.WorkflowResult{Status: "failed", Failure: &linkedin.WorkflowFailure{Reason: "languageNotSupported"}}, "language"},
		{"search not allowed", linkedin.WorkflowResult{Status: "completed", Completion: json.RawMessage(`{"success":false,"error":{"type":"searchingNotAllowed","message":"raw upstream text"}}`)}, "did not allow this search"},
		{"limit exceeded", linkedin.WorkflowResult{Status: "completed", Completion: json.RawMessage(`{"success":false,"error":{"type":"limitExceeded"}}`)}, "limit"},
		{"unexpected shape", linkedin.WorkflowResult{Status: "completed", Completion: json.RawMessage(`{"success":true,"data":{"people":[]}}`)}, "unexpected result"},
		{"not json", linkedin.WorkflowResult{Status: "completed", Completion: json.RawMessage(`"oops"`)}, "unexpected result"},
	}
	for _, c := range cases {
		e := newEnv(t)
		u := e.user("a@example.com")
		acct := e.connectedAccount(u, "la-1")
		imp, err := e.svc.StartSearch(context.Background(), u.id, acct, searchURL, 10)
		if err != nil {
			t.Fatal(err)
		}
		e.fake.GetWorkflowFn = func(string, string) (linkedin.WorkflowResult, error) { return c.result, nil }
		rec := e.do(u.cookie, "GET", fmt.Sprintf("/api/leads/imports/%d", imp.ID), nil)
		got := decode[map[string]Import](t, rec)["import"]
		if got.Status != "failed" || !strings.Contains(got.Message, c.want) || strings.Contains(rec.Body.String(), "raw upstream") || e.count(u.id) != 0 {
			t.Errorf("%s: %+v (%s)", c.name, got, rec.Body)
		}
		// A failed import frees the account for another search.
		if _, err := e.svc.StartSearch(context.Background(), u.id, acct, searchURL, 10); err != nil {
			t.Errorf("%s: account stayed blocked: %v", c.name, err)
		}
	}
}

func TestSearchStartErrorsLeaveNothingBehind(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.connectedAccount(u, "la-1")

	e.fake.Err["StartWorkflow"] = &linkedin.Error{Type: "linkedinAccountSignedOut", Status: 400, Message: "secret upstream text"}
	rec := e.do(u.cookie, "POST", "/api/leads/import/search", map[string]any{"account_id": acct, "search_url": searchURL})
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "account_signed_out") || strings.Contains(rec.Body.String(), "secret upstream") {
		t.Fatalf("signed out: %d %s", rec.Code, rec.Body)
	}
	e.fake.Err["StartWorkflow"] = &linkedin.Error{Type: "tooManyRequests", Status: 429}
	if rec := e.do(u.cookie, "POST", "/api/leads/import/search", map[string]any{"account_id": acct, "search_url": searchURL}); rec.Code != 502 {
		t.Fatalf("transient: %d %s", rec.Code, rec.Body)
	}
	// Each failed start is recorded as failed, not left running, so the next try works.
	if rec := e.do(u.cookie, "POST", "/api/leads/import/search", map[string]any{"account_id": acct, "search_url": searchURL}); rec.Code != 202 {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
}

func TestStaleImportIsAbandoned(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.connectedAccount(u, "la-1")
	imp, _ := e.svc.StartSearch(context.Background(), u.id, acct, searchURL, 10)
	e.pool.Exec(context.Background(), `UPDATE lead_imports SET created_at = now() - interval '21 minutes'`)

	got, err := e.svc.GetImport(context.Background(), u.id, imp.ID)
	if err != nil || got.Status != "failed" || !strings.Contains(got.Message, "too long") {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := e.svc.StartSearch(context.Background(), u.id, acct, searchURL, 10); err != nil {
		t.Fatalf("account stayed blocked: %v", err)
	}
}

func TestSearchRespectsLeadLimit(t *testing.T) {
	e := newEnv(t)
	e.svc.MaxLeads = 2
	u := e.user("a@example.com")
	acct := e.connectedAccount(u, "la-1")
	imp, _ := e.svc.StartSearch(context.Background(), u.id, acct, searchURL, 10)
	e.fake.GetWorkflowFn = func(string, string) (linkedin.WorkflowResult, error) {
		return searchResult(person("A", profile("a")), person("B", profile("b")), person("C", profile("c"))), nil
	}
	got, _ := e.svc.GetImport(context.Background(), u.id, imp.ID)
	if got.Status != "completed" || got.Imported != 2 || got.OverCap != 1 || e.count(u.id) != 2 {
		t.Fatalf("%+v, %d leads", got, e.count(u.id))
	}
}
