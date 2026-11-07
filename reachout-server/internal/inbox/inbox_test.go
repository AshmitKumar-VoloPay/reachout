package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ashmit/reachout-server/internal/auth"
	"github.com/ashmit/reachout-server/internal/httpapi"
	"github.com/ashmit/reachout-server/internal/mdb"
	"github.com/ashmit/reachout-server/internal/testdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type env struct {
	t    *testing.T
	db   *mongo.Database
	h    http.Handler
	auth *auth.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := testdb.New(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux, handler := httpapi.NewRouter(httpapi.Deps{DB: mdb.Pinger{DB: db}, Log: log})
	aSvc := auth.NewService(db)
	ah := auth.NewHandler(aSvc, auth.Options{Log: log})
	ah.Register(mux)
	NewHandler(NewService(db), ah, log).Register(mux)
	return &env{t: t, db: db, h: handler, auth: aSvc}
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

func (e *env) get(u user, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if u.cookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: u.cookie})
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

func (e *env) lead(u user, slug string) int64 {
	e.t.Helper()
	id, err := mdb.NextID(context.Background(), e.db, mdb.Leads)
	if err != nil {
		e.t.Fatal(err)
	}
	testdb.Insert(e.t, e.db, mdb.Leads, bson.D{{Key: "_id", Value: id}, {Key: "user_id", Value: u.id},
		{Key: "profile_url", Value: "https://www.linkedin.com/in/" + slug}, {Key: "name", Value: strings.ToUpper(slug[:1]) + slug[1:]},
		{Key: "headline", Value: "CTO"}, {Key: "company", Value: ""}, {Key: "created_at", Value: time.Now()}})
	return id
}

var t0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

// msg adds a message `mins` minutes after t0.
func (e *env) msg(lead int64, dir, body string, mins int, read bool) {
	e.t.Helper()
	owner := testdb.One[struct {
		UserID int64 `bson:"user_id"`
	}](e.t, e.db, mdb.Leads, bson.D{{Key: "_id", Value: lead}})
	id, err := mdb.NextID(context.Background(), e.db, mdb.Messages)
	if err != nil {
		e.t.Fatal(err)
	}
	doc := bson.D{{Key: "_id", Value: id}, {Key: "user_id", Value: owner.UserID}, {Key: "lead_id", Value: lead},
		{Key: "direction", Value: dir}, {Key: "body", Value: body}, {Key: "at", Value: t0.Add(time.Duration(mins) * time.Minute)}}
	if read {
		doc = append(doc, bson.E{Key: "read_at", Value: t0})
	}
	testdb.Insert(e.t, e.db, mdb.Messages, doc)
}

func TestListShowsConversationsNewestReplyFirst(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	ann, bob, cy, noReply := e.lead(u, "ann"), e.lead(u, "bob"), e.lead(u, "cy"), e.lead(u, "dee")

	e.msg(ann, "out", "Hi Ann", 0, true)
	e.msg(ann, "in", "Hello! Tell me more", 10, false)
	e.msg(ann, "in", "Also, pricing?", 11, false)
	e.msg(bob, "out", "Hi Bob", 1, true)
	e.msg(bob, "in", "Not interested", 30, true) // newest reply, already read
	e.msg(cy, "out", "Hi Cy", 2, true)
	e.msg(cy, "in", "Maybe later", 20, false)
	e.msg(cy, "out", "Great, I'll follow up", 25, true) // we spoke last
	e.msg(noReply, "out", "Hi Dee", 3, true)            // never answered: not in the inbox

	l := decode[List](t, e.get(u, "GET", "/api/inbox"))
	if l.Total != 3 || l.UnreadTotal != 3 || len(l.Conversations) != 3 {
		t.Fatalf("list: total %d unread %d rows %d", l.Total, l.UnreadTotal, len(l.Conversations))
	}
	got := []string{}
	for _, c := range l.Conversations {
		got = append(got, c.Name)
	}
	if fmt.Sprint(got) != "[Bob Cy Ann]" { // by time of the latest reply, newest first
		t.Fatalf("order %v", got)
	}
	byName := map[string]Conversation{}
	for _, c := range l.Conversations {
		byName[c.Name] = c
	}
	if a := byName["Ann"]; a.Unread != 2 || a.Snippet != "Also, pricing?" || a.LastFrom != "them" {
		t.Fatalf("ann: %+v", a)
	}
	if b := byName["Bob"]; b.Unread != 0 || b.LastFrom != "them" {
		t.Fatalf("bob: %+v", b)
	}
	if c := byName["Cy"]; c.Unread != 1 || c.LastFrom != "us" || c.Snippet != "Great, I'll follow up" {
		t.Fatalf("cy: %+v", c)
	}

	// Unread only.
	un := decode[List](t, e.get(u, "GET", "/api/inbox?unread=1"))
	if un.Total != 2 || len(un.Conversations) != 2 || un.UnreadTotal != 3 {
		t.Fatalf("unread list: %+v", un)
	}
	// Paging.
	p := decode[List](t, e.get(u, "GET", "/api/inbox?limit=1&offset=1"))
	if len(p.Conversations) != 1 || p.Conversations[0].Name != "Cy" || p.Total != 3 || p.Limit != 1 {
		t.Fatalf("page: %+v", p)
	}
	if p := decode[List](t, e.get(u, "GET", "/api/inbox?limit=99999&offset=-4")); p.Limit != MaxPage || p.Offset != 0 {
		t.Fatalf("bounds: %+v", p)
	}
	// An empty inbox is an empty list, not null.
	empty := e.user("empty@example.com")
	rec := e.get(empty, "GET", "/api/inbox")
	if !strings.Contains(rec.Body.String(), `"conversations":[]`) {
		t.Fatalf("empty inbox: %s", rec.Body)
	}
}

func TestLongMessagesAreSummarised(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	l := e.lead(u, "ann")
	e.msg(l, "in", strings.Repeat("é", 500), 1, false)
	c := decode[List](t, e.get(u, "GET", "/api/inbox")).Conversations[0]
	if n := len([]rune(c.Snippet)); n != snippetLen+1 || !strings.HasSuffix(c.Snippet, "…") {
		t.Fatalf("snippet is %d characters: %q", n, c.Snippet)
	}
	// The full text is in the conversation.
	d := decode[map[string]Detail](t, e.get(u, "GET", fmt.Sprintf("/api/inbox/%d", l)))["conversation"]
	if len([]rune(d.Messages[0].Body)) != 500 {
		t.Fatal("conversation lost text")
	}
}

func TestConversationAndMarkRead(t *testing.T) {
	e := newEnv(t)
	u := e.user("a@example.com")
	ann := e.lead(u, "ann")
	e.msg(ann, "in", "second", 10, false)
	e.msg(ann, "out", "first", 0, true)
	e.msg(ann, "in", "third", 20, false)
	campaignID, err := mdb.NextID(context.Background(), e.db, mdb.Campaigns)
	if err != nil {
		t.Fatal(err)
	}
	testdb.Insert(t, e.db, mdb.Campaigns, bson.D{{Key: "_id", Value: campaignID}, {Key: "user_id", Value: u.id}, {Key: "account_id", Value: int64(1)},
		{Key: "name", Value: "Q4"}, {Key: "steps", Value: bson.A{}}, {Key: "status", Value: "running"}, {Key: "created_at", Value: time.Now()}})
	testdb.Insert(t, e.db, mdb.CampaignLeads, bson.D{{Key: "campaign_id", Value: campaignID}, {Key: "lead_id", Value: ann},
		{Key: "user_id", Value: u.id}, {Key: "state", Value: "replied"}, {Key: "updated_at", Value: time.Now()}})

	d := decode[map[string]Detail](t, e.get(u, "GET", fmt.Sprintf("/api/inbox/%d", ann)))["conversation"]
	if d.Name != "Ann" || d.Headline != "CTO" || len(d.Messages) != 3 {
		t.Fatalf("detail: %+v", d)
	}
	// Oldest first, our own messages count as read.
	order := []string{}
	for _, m := range d.Messages {
		order = append(order, m.Body)
	}
	if fmt.Sprint(order) != "[first second third]" || !d.Messages[0].Read || d.Messages[1].Read || d.Messages[2].Read {
		t.Fatalf("messages: %+v", d.Messages)
	}
	if len(d.Campaigns) != 1 || d.Campaigns[0].Name != "Q4" || d.Campaigns[0].State != "replied" {
		t.Fatalf("campaigns: %+v", d.Campaigns)
	}
	// Opening a conversation does not mark it read by itself.
	if l := decode[List](t, e.get(u, "GET", "/api/inbox")); l.UnreadTotal != 2 {
		t.Fatalf("unread after viewing: %d", l.UnreadTotal)
	}

	rec := e.get(u, "POST", fmt.Sprintf("/api/inbox/%d/read", ann))
	if n := decode[map[string]int64](t, rec)["marked"]; rec.Code != 200 || n != 2 {
		t.Fatalf("mark read: %d %s", rec.Code, rec.Body)
	}
	if l := decode[List](t, e.get(u, "GET", "/api/inbox")); l.UnreadTotal != 0 || l.Conversations[0].Unread != 0 {
		t.Fatalf("after marking read: %+v", l)
	}
	// Marking again changes nothing.
	if n := decode[map[string]int64](t, e.get(u, "POST", fmt.Sprintf("/api/inbox/%d/read", ann)))["marked"]; n != 0 {
		t.Fatalf("second mark: %d", n)
	}
}

func TestInboxIsPrivateToItsOwner(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user("alice@example.com"), e.user("bob@example.com")
	aLead := e.lead(alice, "ann")
	e.msg(aLead, "in", "secret reply", 5, false)
	bLead := e.lead(bob, "ben")
	e.msg(bLead, "in", "bobs reply", 5, false)

	// Bob sees only his own.
	l := decode[List](t, e.get(bob, "GET", "/api/inbox"))
	if l.Total != 1 || l.Conversations[0].Name != "Ben" || strings.Contains(fmt.Sprint(l), "secret") {
		t.Fatalf("bob's inbox: %+v", l)
	}
	// He cannot open or mark Alice's conversation, and the failed attempt changes nothing.
	for _, c := range []struct{ method, path string }{
		{"GET", fmt.Sprintf("/api/inbox/%d", aLead)},
		{"POST", fmt.Sprintf("/api/inbox/%d/read", aLead)},
	} {
		if rec := e.get(bob, c.method, c.path); rec.Code != 404 {
			t.Errorf("bob %s %s: %d", c.method, c.path, rec.Code)
		}
		if rec := e.get(user{}, c.method, c.path); rec.Code != 401 {
			t.Errorf("anonymous %s %s: %d", c.method, c.path, rec.Code)
		}
	}
	if l := decode[List](t, e.get(alice, "GET", "/api/inbox")); l.UnreadTotal != 1 {
		t.Fatalf("alice's message was marked read by bob: %+v", l)
	}
	for _, bad := range []string{"/api/inbox/abc", "/api/inbox/0", "/api/inbox/-3", "/api/inbox/999999"} {
		if rec := e.get(alice, "GET", bad); rec.Code != 404 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	if rec := e.get(user{}, "GET", "/api/inbox"); rec.Code != 401 {
		t.Errorf("anonymous list: %d", rec.Code)
	}
}
