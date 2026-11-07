package campaigns

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
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
	"github.com/ashmit/reachout-server/internal/leads"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/mdb"
	"github.com/ashmit/reachout-server/internal/testdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type env struct {
	t     *testing.T
	db    *mongo.Database
	svc   *Service
	accts *accounts.Service
	leads *leads.Service
	fake  *linkedin.Fake
	h     http.Handler
	auth  *auth.Service
	laSeq int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := testdb.New(t)
	key := make([]byte, 32)
	rand.Read(key)
	c, _ := accounts.NewCipher(key)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := linkedin.NewFake()
	accts := accounts.NewService(db, fake, c, log)
	svc := NewService(db, accts, log)

	mux, handler := httpapi.NewRouter(httpapi.Deps{DB: mdb.Pinger{DB: db}, Log: log, AllowedOrigins: []string{"http://localhost:5173"}})
	aSvc := auth.NewService(db)
	ah := auth.NewHandler(aSvc, auth.Options{Log: log})
	ah.Register(mux)
	NewHandler(svc, ah, log).Register(mux)
	return &env{t: t, db: db, svc: svc, accts: accts, leads: leads.NewService(db, fake, accts, log),
		fake: fake, h: handler, auth: aSvc}
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

// account connects a fake LinkedIn account for u. Its schedule is the default: Monday to
// Friday, 09:30 to 17:30 UTC.
func (e *env) account(u user) int64 {
	e.t.Helper()
	ctx := context.Background()
	acc, _, err := e.accts.StartConnect(ctx, u.id, "UTC")
	if err != nil {
		e.t.Fatal(err)
	}
	e.laSeq++
	e.fake.FinishSession(fmt.Sprintf("sess-%d", e.laSeq), linkedin.Account{
		ID: fmt.Sprintf("la-%d", e.laSeq), Name: "Acct", IdentificationToken: "T", ConnectedAt: time.Now(),
	})
	if got, err := e.accts.Refresh(ctx, u.id, acc.ID); err != nil || got.Status != accounts.StatusConnected {
		e.t.Fatalf("connect: %+v %v", got, err)
	}
	return acc.ID
}

// leadIDs inserts n leads for the user and returns their ids.
func (e *env) leadIDs(u user, prefix string, n int) []int64 {
	e.t.Helper()
	ids := make([]int64, n)
	for i := range ids {
		id, err := mdb.NextID(context.Background(), e.db, mdb.Leads)
		if err != nil {
			e.t.Fatal(err)
		}
		testdb.Insert(e.t, e.db, mdb.Leads, bson.D{{Key: "_id", Value: id}, {Key: "user_id", Value: u.id},
			{Key: "profile_url", Value: fmt.Sprintf("https://www.linkedin.com/in/%s-%d", prefix, i)},
			{Key: "name", Value: fmt.Sprintf("%s %d", prefix, i)}, {Key: "headline", Value: ""}, {Key: "company", Value: ""},
			{Key: "created_at", Value: time.Now()}})
		ids[i] = id
	}
	return ids
}

// cl reads one lead's place in a campaign straight from the database.
func (e *env) cl(campaignID, leadID int64) clDoc {
	return testdb.One[clDoc](e.t, e.db, mdb.CampaignLeads, bson.D{{Key: "campaign_id", Value: campaignID}, {Key: "lead_id", Value: leadID}})
}

func validSteps() []Step {
	return []Step{
		{Type: StepConnect, Note: "Hi, let's connect"},
		{Type: StepMessage, Body: "Thanks for connecting", DelayDays: 1},
		{Type: StepFollowUp, Body: "Just checking in", DelayDays: 2},
	}
}

func (e *env) campaign(u user, acct int64) Campaign {
	e.t.Helper()
	c, err := e.svc.Create(context.Background(), u.id, Input{AccountID: acct, Name: "Q4 CTOs", Steps: validSteps(), DailyLimit: 10})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// running creates a campaign with n leads and starts it at the given time.
func (e *env) running(u user, acct int64, n int, at time.Time) (Campaign, []int64) {
	e.t.Helper()
	c := e.campaign(u, acct)
	ids := e.leadIDs(u, fmt.Sprintf("r%d", c.ID), n)
	if _, err := e.svc.AddLeads(context.Background(), u.id, c.ID, ids); err != nil {
		e.t.Fatal(err)
	}
	e.svc.now = func() time.Time { return at }
	c, err := e.svc.Start(context.Background(), u.id, c.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return c, ids
}

func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

// Reference week: Fri 2026-11-06, Sat 07, Sun 08, Mon 09, Tue 10, Wed 11.
var (
	fri10   = utc(2026, 11, 6, 10, 0)
	fri11   = utc(2026, 11, 6, 11, 0)
	sat12   = utc(2026, 11, 7, 12, 0)
	mon0930 = utc(2026, 11, 9, 9, 30)
	mon11   = utc(2026, 11, 9, 11, 0)
	tue10   = utc(2026, 11, 10, 10, 0)
	tue11   = utc(2026, 11, 10, 11, 0)
	wed11   = utc(2026, 11, 11, 11, 0)
)

func TestStepValidation(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	mut := func(f func(s []Step)) []Step { s := validSteps(); f(s); return s }
	good, err := ValidateSteps(mut(func(s []Step) {
		s[0].Note = "  padded note \r\n"
		s[1].Body = "  hello  "
	}))
	if err != nil || good[0].Note != "padded note" || good[1].Body != "hello" {
		t.Fatalf("good steps: %+v %v", good, err)
	}
	// An empty connection note is allowed (connect without a note).
	if _, err := ValidateSteps(mut(func(s []Step) { s[0].Note = "" })); err != nil {
		t.Fatalf("empty note rejected: %v", err)
	}
	// Multi-line message bodies are fine.
	if _, err := ValidateSteps(mut(func(s []Step) { s[1].Body = "Line one\n\nLine two" })); err != nil {
		t.Fatalf("multiline body rejected: %v", err)
	}
	bad := map[string][]Step{
		"nil":                nil,
		"two steps":          validSteps()[:2],
		"four steps":         append(validSteps(), Step{Type: StepFollowUp, Body: "x", DelayDays: 1}),
		"wrong order":        {validSteps()[1], validSteps()[0], validSteps()[2]},
		"unknown type":       mut(func(s []Step) { s[1].Type = "call" }),
		"note too long":      mut(func(s []Step) { s[0].Note = long(MaxNoteLen + 1) }),
		"body on connect":    mut(func(s []Step) { s[0].Body = "x" }),
		"delay on connect":   mut(func(s []Step) { s[0].DelayDays = 1 }),
		"empty message":      mut(func(s []Step) { s[1].Body = "   " }),
		"message too long":   mut(func(s []Step) { s[1].Body = long(MaxBodyLen + 1) }),
		"note on message":    mut(func(s []Step) { s[1].Note = "x" }),
		"message delay neg":  mut(func(s []Step) { s[1].DelayDays = -1 }),
		"message delay big":  mut(func(s []Step) { s[1].DelayDays = MaxMessageDelay + 1 }),
		"followup delay 0":   mut(func(s []Step) { s[2].DelayDays = 0 }),
		"followup delay big": mut(func(s []Step) { s[2].DelayDays = MaxFollowUpDelay + 1 }),
		"control chars":      mut(func(s []Step) { s[1].Body = "bad\x00text" }),
	}
	for name, steps := range bad {
		if _, err := ValidateSteps(steps); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	// A message delay of 0 means "next open slot after acceptance" and is allowed.
	if _, err := ValidateSteps(mut(func(s []Step) { s[1].DelayDays = 0 })); err != nil {
		t.Errorf("message delay 0 rejected: %v", err)
	}
	// Rune count, not bytes: 300 multi-byte characters fit.
	if _, err := ValidateSteps(mut(func(s []Step) { s[0].Note = strings.Repeat("é", MaxNoteLen) })); err != nil {
		t.Errorf("300 accented chars rejected: %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	body := func(f func(m map[string]any)) map[string]any {
		m := map[string]any{"account_id": acct, "name": "Camp", "steps": validSteps(), "daily_limit": 10}
		f(m)
		return m
	}
	cases := map[string]map[string]any{
		"no name":        body(func(m map[string]any) { m["name"] = " " }),
		"multiline name": body(func(m map[string]any) { m["name"] = "a\nb" }),
		"long name":      body(func(m map[string]any) { m["name"] = strings.Repeat("n", MaxNameLen+1) }),
		"limit too high": body(func(m map[string]any) { m["daily_limit"] = MaxDailyLimit + 1 }),
		"limit negative": body(func(m map[string]any) { m["daily_limit"] = -3 }),
		"bad steps":      body(func(m map[string]any) { m["steps"] = validSteps()[:1] }),
		"unknown field":  body(func(m map[string]any) { m["status"] = "running" }),
	}
	for name, b := range cases {
		if rec := e.do(u.cookie, "POST", "/api/campaigns", b); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	rec := e.do(u.cookie, "POST", "/api/campaigns", body(func(m map[string]any) { delete(m, "daily_limit") }))
	c := decode[campaignResponse](t, rec).Campaign
	if rec.Code != 201 || c.Status != StatusDraft || c.DailyLimit != DefaultDaily || len(c.Steps) != 3 || c.Stats.Total != 0 {
		t.Fatalf("create: %d %+v", rec.Code, c)
	}
}

func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user("alice@example.com"), e.user("bob@example.com")
	acctA, acctB := e.account(alice), e.account(bob)
	c := e.campaign(alice, acctA)
	aliceLeads := e.leadIDs(alice, "al", 2)
	bobLeads := e.leadIDs(bob, "bo", 2)
	e.svc.AddLeads(context.Background(), alice.id, c.ID, aliceLeads)
	base := fmt.Sprintf("/api/campaigns/%d", c.ID)

	// Every route that takes a campaign id returns 404 for another user, and 401 when anonymous.
	routes := []struct {
		method, path string
		body         any
	}{
		{"GET", base, nil}, {"PUT", base, Input{AccountID: acctA, Name: "x", Steps: validSteps()}},
		{"DELETE", base, nil}, {"POST", base + "/start", nil}, {"POST", base + "/pause", nil},
		{"POST", base + "/resume", nil}, {"POST", base + "/stop", nil},
		{"GET", base + "/leads", nil}, {"POST", base + "/leads", map[string]any{"lead_ids": bobLeads}},
		{"DELETE", fmt.Sprintf("%s/leads/%d", base, aliceLeads[0]), nil},
	}
	for _, r := range routes {
		if rec := e.do(bob.cookie, r.method, r.path, r.body); rec.Code != 404 {
			t.Errorf("bob %s %s: %d %s, want 404", r.method, r.path, rec.Code, rec.Body)
		}
		if rec := e.do("", r.method, r.path, r.body); rec.Code != 401 {
			t.Errorf("anonymous %s %s: %d, want 401", r.method, r.path, rec.Code)
		}
	}
	// Bob cannot create a campaign on Alice's LinkedIn account.
	if rec := e.do(bob.cookie, "POST", "/api/campaigns", Input{AccountID: acctA, Name: "x", Steps: validSteps()}); rec.Code != 404 {
		t.Errorf("bob on alice's account: %d %s", rec.Code, rec.Body)
	}
	// Alice cannot put Bob's leads into her campaign: they count as not found.
	res, err := e.svc.AddLeads(context.Background(), alice.id, c.ID, bobLeads)
	if err != nil || res.NotFound != 2 || res.Added != 0 {
		t.Fatalf("alice adding bob's leads: %+v %v", res, err)
	}
	// Lists show only your own.
	if cs := decode[struct{ Campaigns []Campaign }](t, e.do(bob.cookie, "GET", "/api/campaigns", nil)).Campaigns; len(cs) != 0 {
		t.Fatalf("bob sees %d campaigns", len(cs))
	}
	_ = acctB
	// Nothing Bob tried changed Alice's campaign.
	got, _ := e.svc.Get(context.Background(), alice.id, c.ID)
	if got.Status != StatusDraft || got.Stats.Total != 2 || got.Name != "Q4 CTOs" {
		t.Fatalf("alice's campaign changed: %+v", got)
	}
}

func TestStartOnSaturdayQueuesUntilMonday(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)

	c, ids := e.running(u, acct, 3, sat12)
	if c.Status != StatusRunning || c.StartedAt == nil || c.Stats.Total != 3 || c.Stats.Queued != 3 {
		t.Fatalf("started: %+v", c)
	}
	if c.Stats.NextActionAt == nil || !c.Stats.NextActionAt.Equal(mon0930) {
		t.Fatalf("saturday start: next_action_at = %v, want Monday 09:30", c.Stats.NextActionAt)
	}
	page, _ := e.svc.ListLeads(context.Background(), u.id, c.ID, StateQueued, 0, 0)
	if len(page.Leads) != 3 {
		t.Fatalf("queued leads: %+v", page)
	}
	for _, l := range page.Leads {
		if l.State != StateQueued || l.CurrentStep != 0 || l.NextActionAt == nil || !l.NextActionAt.Equal(mon0930) {
			t.Fatalf("lead %d: %+v", l.LeadID, l)
		}
	}
	_ = ids

	// Started while the window is open: due immediately.
	e2 := newEnv(t)
	u2 := e2.user("b@example.com")
	c2, _ := e2.running(u2, e2.account(u2), 2, tue10)
	if c2.Stats.NextActionAt == nil || !c2.Stats.NextActionAt.Equal(tue10) {
		t.Fatalf("tuesday start: %v, want %v", c2.Stats.NextActionAt, tue10)
	}
	// After hours on a weekday: next morning.
	e3 := newEnv(t)
	u3 := e3.user("c@example.com")
	c3, _ := e3.running(u3, e3.account(u3), 1, utc(2026, 11, 10, 18, 0))
	if !c3.Stats.NextActionAt.Equal(utc(2026, 11, 11, 9, 30)) {
		t.Fatalf("after hours: %v", c3.Stats.NextActionAt)
	}
}

func TestStartRules(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	ctx := context.Background()

	c := e.campaign(u, acct)
	if _, err := e.svc.Start(ctx, u.id, c.ID); !errors.Is(err, ErrNoLeads) {
		t.Fatalf("no leads: %v", err)
	}
	e.svc.AddLeads(ctx, u.id, c.ID, e.leadIDs(u, "l", 2))

	testdb.Update(t, e.db, mdb.Accounts, bson.D{{Key: "_id", Value: acct}}, bson.D{{Key: "status", Value: "reconnection_required"}})
	if _, err := e.svc.Start(ctx, u.id, c.ID); !errors.Is(err, ErrAccountNotReady) {
		t.Fatalf("disconnected account: %v", err)
	}
	testdb.Update(t, e.db, mdb.Accounts, bson.D{{Key: "_id", Value: acct}}, bson.D{{Key: "status", Value: "connected"}})

	if _, err := e.svc.Start(ctx, u.id, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Start(ctx, u.id, c.ID); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("start twice: %v", err)
	}
	// Nothing was queued by the failed attempts.
	if n := testdb.Count(t, e.db, mdb.CampaignLeads, bson.D{{Key: "next_action_at", Value: nil}, {Key: "state", Value: "queued"}}); n != 0 {
		t.Fatalf("%d leads without a due time after start", n)
	}
}

func TestAddLeadsRules(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	ctx := context.Background()
	c := e.campaign(u, acct)
	ids := e.leadIDs(u, "l", 4)

	res, err := e.svc.AddLeads(ctx, u.id, c.ID, append([]int64{ids[0], ids[0], ids[1]}, 999999))
	if err != nil || res.Added != 2 || res.NotFound != 1 || res.Duplicates != 0 {
		t.Fatalf("first add: %+v %v", res, err)
	}
	res, _ = e.svc.AddLeads(ctx, u.id, c.ID, []int64{ids[0], ids[2]})
	if res.Added != 1 || res.Duplicates != 1 {
		t.Fatalf("second add: %+v", res)
	}
	// In a draft the leads wait for Start: no due time yet.
	if due := e.cl(c.ID, ids[0]).NextActionAt; due != nil {
		t.Fatalf("draft lead has a due time: %v", due)
	}
	for _, bad := range [][]int64{nil, make([]int64, MaxAddPerRequest+1)} {
		if _, err := e.svc.AddLeads(ctx, u.id, c.ID, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%d ids: %v", len(bad), err)
		}
	}

	// Added to a running campaign: scheduled on the spot, using the schedule.
	e.svc.now = func() time.Time { return sat12 }
	if _, err := e.svc.Start(ctx, u.id, c.ID); err != nil {
		t.Fatal(err)
	}
	res, _ = e.svc.AddLeads(ctx, u.id, c.ID, []int64{ids[3]})
	due := e.cl(c.ID, ids[3]).NextActionAt
	if res.Added != 1 || due == nil || !due.Equal(mon0930) {
		t.Fatalf("add to running: %+v due=%v", res, due)
	}
	// A stopped campaign takes no more leads.
	e.svc.Stop(ctx, u.id, c.ID)
	if _, err := e.svc.AddLeads(ctx, u.id, c.ID, e.leadIDs(u, "z", 1)); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("add to stopped: %v", err)
	}
}

func TestCampaignLeadCap(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	c := e.campaign(u, e.account(u))
	ctx := context.Background()
	// Fill the campaign to its limit directly, then try to add more through the service.
	for _, id := range e.leadIDs(u, "bulk", MaxCampaignLeads) {
		testdb.Insert(t, e.db, mdb.CampaignLeads, clDoc{CampaignID: c.ID, LeadID: id, UserID: u.id, State: StateQueued, UpdatedAt: time.Now()})
	}
	extra := e.leadIDs(u, "extra", 2)
	res, err := e.svc.AddLeads(ctx, u.id, c.ID, extra)
	if err != nil || res.OverCap != 2 || res.Added != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestOneActiveSequencePerLead(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	ctx := context.Background()
	shared := e.leadIDs(u, "shared", 1)

	a, _ := e.running(u, acct, 0+1, tue10) // campaign A running with its own lead
	_ = a
	cA := e.campaign(u, acct)
	e.svc.AddLeads(ctx, u.id, cA.ID, shared)
	e.svc.now = func() time.Time { return tue10 }
	if _, err := e.svc.Start(ctx, u.id, cA.ID); err != nil {
		t.Fatal(err)
	}

	// B cannot take a lead A is working, whether added now or already there when A started.
	cB := e.campaign(u, acct)
	res, _ := e.svc.AddLeads(ctx, u.id, cB.ID, shared)
	if res.Conflicts != 1 || res.Added != 0 {
		t.Fatalf("add busy lead: %+v", res)
	}
	// Race the other way: both drafts hold the lead, A starts first, then B must refuse to start.
	e2 := newEnv(t)
	u2 := e2.user("b@example.com")
	acct2 := e2.account(u2)
	lead := e2.leadIDs(u2, "s", 1)
	x, y := e2.campaign(u2, acct2), e2.campaign(u2, acct2)
	e2.svc.AddLeads(ctx, u2.id, x.ID, lead)
	e2.svc.AddLeads(ctx, u2.id, y.ID, lead)
	e2.svc.now = func() time.Time { return tue10 }
	if _, err := e2.svc.Start(ctx, u2.id, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.svc.Start(ctx, u2.id, y.ID); !errors.Is(err, ErrLeadConflict) {
		t.Fatalf("second start: %v", err)
	}
	// Once the first campaign is stopped the lead is free again.
	e2.svc.Stop(ctx, u2.id, x.ID)
	if _, err := e2.svc.Start(ctx, u2.id, y.ID); err != nil {
		t.Fatalf("start after stop: %v", err)
	}
}

func TestLifecycleTransitions(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	ctx := context.Background()
	c, ids := e.running(u, acct, 2, fri10)
	_ = ids

	// Invalid moves.
	if _, err := e.svc.Resume(ctx, u.id, c.ID); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("resume running: %v", err)
	}
	p, err := e.svc.Pause(ctx, u.id, c.ID)
	if err != nil || p.Status != StatusPaused {
		t.Fatalf("pause: %+v %v", p, err)
	}
	if _, err := e.svc.Pause(ctx, u.id, c.ID); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("pause twice: %v", err)
	}
	if _, err := e.svc.Start(ctx, u.id, c.ID); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("start a paused campaign: %v", err)
	}
	// Pausing keeps due times and states.
	if p.Stats.NextActionAt == nil || !p.Stats.NextActionAt.Equal(fri10) {
		t.Errorf("pause changed due times: %v", p.Stats.NextActionAt)
	}

	// Sends that came due while paused move to the next open slot on resume: Friday evening
	// -> Monday morning, not all at once in the past.
	e.svc.now = func() time.Time { return utc(2026, 11, 6, 18, 0) }
	r, err := e.svc.Resume(ctx, u.id, c.ID)
	if err != nil || r.Status != StatusRunning || r.Stats.NextActionAt == nil || !r.Stats.NextActionAt.Equal(mon0930) {
		t.Fatalf("resume: %+v %v", r.Stats, err)
	}

	// Stop: states kept, nothing due, no way back.
	s, err := e.svc.Stop(ctx, u.id, c.ID)
	if err != nil || s.Status != StatusStopped || s.Stats.NextActionAt != nil || s.Stats.Total != 2 || s.Stats.Queued != 2 {
		t.Fatalf("stop: %+v %v", s, err)
	}
	for name, fn := range map[string]func() (Campaign, error){
		"start":  func() (Campaign, error) { return e.svc.Start(ctx, u.id, c.ID) },
		"pause":  func() (Campaign, error) { return e.svc.Pause(ctx, u.id, c.ID) },
		"resume": func() (Campaign, error) { return e.svc.Resume(ctx, u.id, c.ID) },
		"stop":   func() (Campaign, error) { return e.svc.Stop(ctx, u.id, c.ID) },
	} {
		if _, err := fn(); !errors.Is(err, ErrWrongStatus) {
			t.Errorf("%s on a stopped campaign: %v", name, err)
		}
	}
	// A draft can be stopped (abandoned) too.
	d := e.campaign(u, acct)
	if st, err := e.svc.Stop(ctx, u.id, d.ID); err != nil || st.Status != StatusStopped {
		t.Fatalf("stop draft: %+v %v", st, err)
	}
}

func TestUpdateAndDeleteRules(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	ctx := context.Background()
	c := e.campaign(u, acct)

	newSteps := validSteps()
	newSteps[1].Body = "A different message"
	got, err := e.svc.Update(ctx, u.id, c.ID, Input{AccountID: acct, Name: "Renamed", Steps: newSteps, DailyLimit: 5})
	if err != nil || got.Name != "Renamed" || got.Steps[1].Body != "A different message" || got.DailyLimit != 5 {
		t.Fatalf("draft update: %+v %v", got, err)
	}
	if _, err := e.svc.Update(ctx, u.id, c.ID, Input{AccountID: acct, Name: "x", Steps: validSteps()[:1]}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad update: %v", err)
	}

	e.svc.AddLeads(ctx, u.id, c.ID, e.leadIDs(u, "l", 1))
	e.svc.now = func() time.Time { return tue10 }
	e.svc.Start(ctx, u.id, c.ID)

	// Running: name and limit only.
	if got, err := e.svc.Update(ctx, u.id, c.ID, Input{AccountID: acct, Name: "Live rename", Steps: newSteps, DailyLimit: 8}); err != nil || got.Name != "Live rename" || got.DailyLimit != 8 {
		t.Fatalf("running rename: %+v %v", got, err)
	}
	edited := validSteps()
	edited[2].Body = "Changed mid-flight"
	if _, err := e.svc.Update(ctx, u.id, c.ID, Input{AccountID: acct, Name: "x", Steps: edited, DailyLimit: 8}); !errors.Is(err, ErrNotEditable) {
		t.Fatalf("running steps edit: %v", err)
	}
	if err := e.svc.Delete(ctx, u.id, c.ID); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("delete running: %v", err)
	}
	e.svc.Pause(ctx, u.id, c.ID)
	if err := e.svc.Delete(ctx, u.id, c.ID); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("delete paused: %v", err)
	}
	e.svc.Stop(ctx, u.id, c.ID)
	if _, err := e.svc.Update(ctx, u.id, c.ID, Input{AccountID: acct, Name: "x", Steps: newSteps}); !errors.Is(err, ErrNotEditable) {
		t.Fatalf("update stopped: %v", err)
	}
	if err := e.svc.Delete(ctx, u.id, c.ID); err != nil {
		t.Fatalf("delete stopped: %v", err)
	}
	if err := e.svc.Delete(ctx, u.id, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	// Draft campaigns delete freely.
	if err := e.svc.Delete(ctx, u.id, e.campaign(u, acct).ID); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
}

func TestRemoveLead(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	ctx := context.Background()
	c, ids := e.running(u, e.account(u), 2, tue10)

	if err := e.svc.RemoveLead(ctx, u.id, c.ID, ids[0]); err != nil {
		t.Fatalf("remove queued lead: %v", err)
	}
	if err := e.svc.RemoveLead(ctx, u.id, c.ID, ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove twice: %v", err)
	}
	e.svc.Record(ctx, c.ID, ids[1], EvRequestSent, tue10, "")
	if err := e.svc.RemoveLead(ctx, u.id, c.ID, ids[1]); !errors.Is(err, ErrLeadNotRemovable) {
		t.Fatalf("remove contacted lead: %v", err)
	}
	// The lead itself still exists and is untouched.
	if n := testdb.Count(t, e.db, mdb.Leads, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}); n != 2 {
		t.Fatalf("leads deleted: %d", n)
	}
}

func TestLeadsInActiveCampaignCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	ctx := context.Background()
	c, ids := e.running(u, e.account(u), 2, tue10)
	free := e.leadIDs(u, "free", 1)[0]

	if err := e.leads.Delete(ctx, u.id, ids[0]); !errors.Is(err, leads.ErrInActiveCampaign) {
		t.Fatalf("delete lead in running campaign: %v", err)
	}
	deleted, blocked, err := e.leads.DeleteMany(ctx, u.id, []int64{ids[0], ids[1], free})
	if err != nil || deleted != 1 || blocked != 2 {
		t.Fatalf("bulk: deleted=%d blocked=%d %v", deleted, blocked, err)
	}
	e.svc.Pause(ctx, u.id, c.ID)
	if err := e.leads.Delete(ctx, u.id, ids[0]); !errors.Is(err, leads.ErrInActiveCampaign) {
		t.Fatalf("delete lead in paused campaign: %v", err)
	}
	// After the campaign is stopped the lead can go, and its campaign row goes with it.
	e.svc.Stop(ctx, u.id, c.ID)
	if err := e.leads.Delete(ctx, u.id, ids[0]); err != nil {
		t.Fatalf("delete after stop: %v", err)
	}
	got, _ := e.svc.Get(ctx, u.id, c.ID)
	if got.Stats.Total != 1 {
		t.Fatalf("campaign lead rows after delete: %d", got.Stats.Total)
	}
}

func TestRecordFollowsBusinessDays(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	ctx := context.Background()
	c, ids := e.running(u, e.account(u), 1, fri10)
	lead := ids[0]

	rec := func(ev Event, at time.Time) (State, Outcome) {
		t.Helper()
		st, out, err := e.svc.Record(ctx, c.ID, lead, ev, at, "")
		if err != nil {
			t.Fatalf("record %s: %v", ev, err)
		}
		return st, out
	}
	due := func() *time.Time {
		return e.cl(c.ID, lead).NextActionAt
	}

	// Request sent: now waiting for the acceptance, nothing scheduled.
	if st, out := rec(EvRequestSent, fri10); st != StateRequestSent || out != Applied || due() != nil {
		t.Fatalf("request sent: %s %v due=%v", st, out, due())
	}
	// Accepted on Friday 11:00, message delay 1 business day -> Monday 11:00 (not Saturday).
	if st, out := rec(EvAccepted, fri11); st != StateConnected || out != Applied || due() == nil || !due().Equal(mon11) {
		t.Fatalf("accepted: %s %v due=%v want %v", st, out, due(), mon11)
	}
	// Message sent Monday 11:00, follow-up delay 2 -> Wednesday 11:00.
	if st, out := rec(EvMessageSent, mon11); st != StateMessaged || out != Applied || !due().Equal(wed11) {
		t.Fatalf("message sent: %s %v due=%v want %v", st, out, due(), wed11)
	}
	// A duplicate acceptance webhook changes nothing, including the due time.
	before := *due()
	if st, out := rec(EvAccepted, tue10); st != StateMessaged || out != Noop || !due().Equal(before) {
		t.Fatalf("duplicate accepted: %s %v due=%v", st, out, due())
	}
	// Follow-up Wednesday 11:00 -> final wait of 2 business days -> Friday 11:00, then done.
	if st, out := rec(EvFollowUpSent, wed11); st != StateFollowedUp || out != Applied || !due().Equal(utc(2026, 11, 13, 11, 0)) {
		t.Fatalf("follow-up: %s %v due=%v", st, out, due())
	}
	if st, out := rec(EvDone, utc(2026, 11, 13, 11, 0)); st != StateDone || out != Applied || due() != nil {
		t.Fatalf("done: %s %v due=%v", st, out, due())
	}
	got, _ := e.svc.Get(ctx, u.id, c.ID)
	if got.Status != StatusDone || got.Stats.Done != 1 || got.Stats.Sent != 1 || got.Stats.Accepted != 1 || got.Stats.Messaged != 1 || got.Stats.FollowedUp != 1 {
		t.Fatalf("campaign after the last lead finished: %+v", got)
	}
}

func TestRecordAfterHoursSnapsToNextWindow(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	ctx := context.Background()
	c, ids := e.running(u, e.account(u), 1, fri10)
	e.svc.Record(ctx, c.ID, ids[0], EvRequestSent, fri10, "")
	// Accepted Friday 19:00 (outside hours) with message delay 1: Monday 19:00 is also outside,
	// so the message waits for Tuesday 09:30. It never lands outside the window.
	e.svc.Record(ctx, c.ID, ids[0], EvAccepted, utc(2026, 11, 6, 19, 0), "")
	due := e.cl(c.ID, ids[0]).NextActionAt
	if due == nil || !due.Equal(utc(2026, 11, 10, 9, 30)) {
		t.Fatalf("due %v, want Tuesday 09:30", due)
	}
}

func TestRecordReplyStopsEverything(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	ctx := context.Background()
	c, ids := e.running(u, e.account(u), 2, tue10)
	a, b := ids[0], ids[1]

	e.svc.Record(ctx, c.ID, a, EvRequestSent, tue10, "")
	e.svc.Record(ctx, c.ID, a, EvAccepted, tue10, "")
	e.svc.Record(ctx, c.ID, a, EvMessageSent, tue10, "")
	st, out, err := e.svc.Record(ctx, c.ID, a, EvReplied, tue11, "")
	if err != nil || st != StateReplied || out != Applied {
		t.Fatalf("reply: %s %v %v", st, out, err)
	}
	after := e.cl(c.ID, a)
	due, step := after.NextActionAt, after.CurrentStep
	if due != nil || step != 3 {
		t.Fatalf("after reply: due=%v step=%d (want no due time and the message step kept)", due, step)
	}
	// Later worker events for the same lead are dropped.
	for _, ev := range []Event{EvFollowUpSent, EvDone, EvFailed, EvReplied} {
		if st, out, _ := e.svc.Record(ctx, c.ID, a, ev, wed11, "boom"); st != StateReplied || out != Noop {
			t.Errorf("after reply, %s: %s %v", ev, st, out)
		}
	}
	// A reply from someone who never got to "connected" in our books still counts, and
	// implies the connection for the funnel.
	e.svc.Record(ctx, c.ID, b, EvRequestSent, tue10, "")
	e.svc.Record(ctx, c.ID, b, EvReplied, tue11, "")
	got, _ := e.svc.Get(ctx, u.id, c.ID)
	if got.Stats.Replied != 2 || got.Stats.Accepted != 2 || got.Stats.Sent != 2 || got.Stats.Messaged != 1 {
		t.Fatalf("funnel: %+v", got.Stats)
	}
	if got.Status != StatusDone {
		t.Fatalf("every lead replied, campaign should be done: %s", got.Status)
	}
}

func TestRecordInvalidAndMissing(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	ctx := context.Background()
	c, ids := e.running(u, e.account(u), 1, tue10)

	// A message cannot be sent before the request is accepted.
	st, out, err := e.svc.Record(ctx, c.ID, ids[0], EvMessageSent, tue10, "")
	if err != nil || st != StateQueued || out != Invalid {
		t.Fatalf("message before accept: %s %v %v", st, out, err)
	}
	if _, _, err := e.svc.Record(ctx, c.ID, 999999, EvRequestSent, tue10, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown lead: %v", err)
	}
	if _, _, err := e.svc.Record(ctx, 999999, ids[0], EvRequestSent, tue10, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown campaign: %v", err)
	}
	// Failure keeps the error text and does not lower progress.
	e.svc.Record(ctx, c.ID, ids[0], EvRequestSent, tue10, "")
	e.svc.Record(ctx, c.ID, ids[0], EvFailed, tue10, "personNotFound")
	page, _ := e.svc.ListLeads(ctx, u.id, c.ID, StateFailed, 0, 0)
	if len(page.Leads) != 1 || page.Leads[0].LastError != "personNotFound" || page.Leads[0].CurrentStep != 1 || page.Leads[0].NextActionAt != nil {
		t.Fatalf("failed lead: %+v", page.Leads)
	}
}

func TestRecordOnStoppedCampaignStillRecordsReplies(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	ctx := context.Background()
	c, ids := e.running(u, e.account(u), 1, tue10)
	e.svc.Record(ctx, c.ID, ids[0], EvRequestSent, tue10, "")
	e.svc.Stop(ctx, u.id, c.ID)

	// Acceptance arrives after the stop: the state moves, but nothing is scheduled.
	e.svc.Record(ctx, c.ID, ids[0], EvAccepted, tue11, "")
	stopped := e.cl(c.ID, ids[0])
	due, state := stopped.NextActionAt, stopped.State
	if state != StateConnected || due != nil {
		t.Fatalf("after stop: state=%s due=%v", state, due)
	}
	if st, _, _ := e.svc.Record(ctx, c.ID, ids[0], EvReplied, tue11, ""); st != StateReplied {
		t.Fatalf("reply on stopped campaign: %s", st)
	}
	got, _ := e.svc.Get(ctx, u.id, c.ID)
	if got.Status != StatusStopped {
		t.Fatalf("stopped campaign changed status to %s", got.Status)
	}
}

func TestConcurrentEventsEndConsistent(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	ctx := context.Background()
	c, ids := e.running(u, e.account(u), 1, tue10)
	events := []Event{EvRequestSent, EvAccepted, EvAccepted, EvMessageSent, EvRequestSent, EvReplied, EvAccepted, EvReplied, EvFollowUpSent}
	var wg sync.WaitGroup
	for _, ev := range events {
		wg.Add(1)
		go func(ev Event) {
			defer wg.Done()
			e.svc.Record(ctx, c.ID, ids[0], ev, tue10, "")
		}(ev)
	}
	wg.Wait()
	final := e.cl(c.ID, ids[0])
	state, due, step := final.State, final.NextActionAt, final.CurrentStep
	// Whatever the interleaving, a reply is in the set, so the lead must end replied with
	// nothing scheduled, and the step can only be one a legal path could have reached.
	if state != StateReplied || due != nil || step < 1 || step > 3 {
		t.Fatalf("state=%s due=%v step=%d", state, due, step)
	}
}

func TestStatsAndLeadListing(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	ctx := context.Background()
	c, ids := e.running(u, e.account(u), 5, tue10)
	// 0 queued; 1 request sent; 2 accepted; 3 messaged; 4 replied after message.
	e.svc.Record(ctx, c.ID, ids[1], EvRequestSent, tue10, "")
	for _, ev := range []Event{EvRequestSent, EvAccepted} {
		e.svc.Record(ctx, c.ID, ids[2], ev, tue10, "")
	}
	for _, ev := range []Event{EvRequestSent, EvAccepted, EvMessageSent} {
		e.svc.Record(ctx, c.ID, ids[3], ev, tue10, "")
	}
	for _, ev := range []Event{EvRequestSent, EvAccepted, EvMessageSent, EvReplied} {
		e.svc.Record(ctx, c.ID, ids[4], ev, tue10, "")
	}
	got, _ := e.svc.Get(ctx, u.id, c.ID)
	want := Stats{Total: 5, Queued: 1, Sent: 4, Accepted: 3, Messaged: 2, FollowedUp: 0, Replied: 1}
	g := got.Stats
	g.NextActionAt = nil
	if g != want {
		t.Fatalf("stats %+v, want %+v", g, want)
	}
	if got.Stats.NextActionAt == nil {
		t.Fatal("campaign with work in progress should report its next action time")
	}

	list, err := e.svc.List(ctx, u.id)
	if err != nil || len(list) != 1 || list[0].Stats.Sent != 4 || list[0].Stats.Replied != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	page, _ := e.svc.ListLeads(ctx, u.id, c.ID, "", 2, 0)
	if page.Total != 5 || len(page.Leads) != 2 || page.Limit != 2 {
		t.Fatalf("page: %+v", page)
	}
	if p, _ := e.svc.ListLeads(ctx, u.id, c.ID, StateMessaged, 0, 0); p.Total != 1 || p.Leads[0].LeadID != ids[3] {
		t.Fatalf("messaged filter: %+v", p)
	}
	if rec := e.do(u.cookie, "GET", fmt.Sprintf("/api/campaigns/%d/leads?state=bogus", c.ID), nil); rec.Code != 400 {
		t.Fatalf("bad state filter: %d", rec.Code)
	}
}

func TestHTTPLifecycle(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	acct := e.account(u)
	ids := e.leadIDs(u, "h", 2)

	rec := e.do(u.cookie, "POST", "/api/campaigns", Input{AccountID: acct, Name: "HTTP", Steps: validSteps()})
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	base := fmt.Sprintf("/api/campaigns/%d", decode[campaignResponse](t, rec).Campaign.ID)

	if rec := e.do(u.cookie, "POST", base+"/start", nil); rec.Code != 409 || !strings.Contains(rec.Body.String(), "no_leads") {
		t.Fatalf("start empty: %d %s", rec.Code, rec.Body)
	}
	rec = e.do(u.cookie, "POST", base+"/leads", map[string]any{"lead_ids": ids})
	if rec.Code != 200 || decode[AddResult](t, rec).Added != 2 {
		t.Fatalf("add leads: %d %s", rec.Code, rec.Body)
	}
	for _, step := range []struct {
		action string
		status Status
	}{{"start", StatusRunning}, {"pause", StatusPaused}, {"resume", StatusRunning}, {"stop", StatusStopped}} {
		rec := e.do(u.cookie, "POST", base+"/"+step.action, nil)
		if got := decode[campaignResponse](t, rec).Campaign.Status; rec.Code != 200 || got != step.status {
			t.Fatalf("%s: %d status=%s %s", step.action, rec.Code, got, rec.Body)
		}
	}
	if rec := e.do(u.cookie, "POST", base+"/resume", nil); rec.Code != 409 || !strings.Contains(rec.Body.String(), "wrong_status") {
		t.Fatalf("resume stopped: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(u.cookie, "DELETE", base, nil); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := e.do(u.cookie, "GET", base, nil); rec.Code != 404 {
		t.Fatalf("get deleted: %d", rec.Code)
	}
	if rec := e.do(u.cookie, "GET", "/api/campaigns/abc", nil); rec.Code != 404 {
		t.Fatalf("bad id: %d", rec.Code)
	}
}
