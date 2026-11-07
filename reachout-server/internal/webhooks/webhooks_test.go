package webhooks

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
	"testing"
	"time"

	"github.com/ashmit/reachout-server/internal/accounts"
	"github.com/ashmit/reachout-server/internal/auth"
	"github.com/ashmit/reachout-server/internal/campaigns"
	"github.com/ashmit/reachout-server/internal/httpapi"
	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/mdb"
	"github.com/ashmit/reachout-server/internal/testdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The secret and vectors are the ones in Linked API's signature documentation.
const (
	docSecret = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	vector1   = `{"id":"event-1","type":"webhook.test","data":{"message":"Hello"}}`
	vector1OK = "8f93c19766fe42f27da56fcb5ae11a909c406683ccc0766bd29edc52c71fd209"
	// What you get if you hex-decode the secret before signing: the documented mistake.
	vector1HexDecodedMistake = "cf8a7cbd4da0130448492ced3d1aaffd608738b7fff2623e27eaecd4dd54a236"
	vector2                  = `{"id":"event-2","type":"webhook.test","data":{"message":"Zoë ♥ 日本"}}`
	vector2OK                = "87f5bc51d542ce4d8b61350b35807420dce7b3473bd7ecec87211b544c299c69"
)

func TestSignatureMatchesTheDocumentedVectors(t *testing.T) {
	at := time.Unix(1700000000, 0)
	if !Verify(docSecret, []byte(vector1), "1700000000", vector1OK, at) {
		t.Fatal("vector 1 rejected")
	}
	if !Verify(docSecret, []byte(vector2), "1700000000", vector2OK, at) {
		t.Fatal("vector 2 (multi-byte text) rejected: the body must be hashed as bytes")
	}
	if Verify(docSecret, []byte(vector1), "1700000000", vector1HexDecodedMistake, at) {
		t.Fatal("accepted the signature made with a hex-decoded secret")
	}
	// Our own signer agrees with the documented result.
	if ts, sig := Sign(docSecret, []byte(vector1), at); ts != "1700000000" || sig != vector1OK {
		t.Fatalf("Sign gives %s %s", ts, sig)
	}
}

func TestVerifyRejectsAnythingElse(t *testing.T) {
	at := time.Unix(1700000000, 0)
	body := []byte(vector1)
	cases := map[string]bool{
		"tampered body":          Verify(docSecret, []byte(vector1+" "), "1700000000", vector1OK, at),
		"other secret":           Verify(docSecret+"x", body, "1700000000", vector1OK, at),
		"empty secret":           Verify("", body, "1700000000", vector1OK, at),
		"other timestamp":        Verify(docSecret, body, "1700000001", vector1OK, at),
		"uppercase hex":          Verify(docSecret, body, "1700000000", strings.ToUpper(vector1OK), at),
		"short signature":        Verify(docSecret, body, "1700000000", vector1OK[:60], at),
		"empty signature":        Verify(docSecret, body, "1700000000", "", at),
		"non-hex signature":      Verify(docSecret, body, "1700000000", strings.Repeat("z", 64), at),
		"empty timestamp":        Verify(docSecret, body, "", vector1OK, at),
		"non-numeric timestamp":  Verify(docSecret, body, "yesterday", vector1OK, at),
		"signature with padding": Verify(docSecret, body, "1700000000", vector1OK+"\n", at),
	}
	for name, got := range cases {
		if got {
			t.Errorf("%s: accepted", name)
		}
	}
	// The timestamp window is 300 seconds either way.
	sign := func(delta int64) bool {
		ts, sig := Sign(docSecret, body, time.Unix(1700000000+delta, 0))
		return Verify(docSecret, body, ts, sig, at)
	}
	for delta, want := range map[int64]bool{0: true, 299: true, -299: true, 300: true, -300: true, 301: false, -301: false, 86400: false} {
		if got := sign(delta); got != want {
			t.Errorf("timestamp %+ds from now: accepted=%v, want %v", delta, got, want)
		}
	}
}

// ---- endpoint tests ----

type flakyRecorder struct {
	inner Recorder
	fail  bool
}

func (f *flakyRecorder) Record(ctx context.Context, c, l int64, ev campaigns.Event, at time.Time, d string) (campaigns.State, campaigns.Outcome, error) {
	if f.fail {
		return "", campaigns.Invalid, errors.New("database unavailable")
	}
	return f.inner.Record(ctx, c, l, ev, at, d)
}

type env struct {
	t     *testing.T
	db    *mongo.Database
	camps *campaigns.Service
	accts *accounts.Service
	auth  *auth.Service
	fake  *linkedin.Fake
	rec   *flakyRecorder
	h     *Handler
	srv   http.Handler
	now   time.Time
	seq   int
	evSeq int
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
	camps := campaigns.NewService(db, accts, log)
	e := &env{t: t, db: db, camps: camps, accts: accts, auth: auth.NewService(db), fake: fake,
		rec: &flakyRecorder{inner: camps}, now: time.Date(2026, 11, 10, 10, 5, 0, 0, time.UTC)}
	e.h = New(db, e.rec, docSecret, log)
	e.h.now = func() time.Time { return e.now }
	mux, handler := httpapi.NewRouter(httpapi.Deps{DB: mdb.Pinger{DB: db}, Log: log, AllowedOrigins: []string{"http://localhost:5173"}})
	e.h.Register(mux)
	e.srv = handler
	return e
}

type owner struct {
	userID    int64
	accountID int64  // our row id
	linkedID  string // Linked API's account id, as used in events
}

func (e *env) owner(email string) owner {
	e.t.Helper()
	ctx := context.Background()
	u, _, _, err := e.auth.Signup(ctx, email, "a long enough password")
	if err != nil {
		e.t.Fatal(err)
	}
	acc, link, err := e.accts.StartConnect(ctx, u.ID, "UTC")
	if err != nil {
		e.t.Fatal(err)
	}
	e.seq++
	laID := fmt.Sprintf("la-%d", e.seq)
	e.fake.FinishSession(strings.TrimPrefix(link, "https://app.linkedapi.io/connection/"),
		linkedin.Account{ID: laID, Name: "Acct", IdentificationToken: "T" + laID, ConnectedAt: time.Now()})
	if got, err := e.accts.Refresh(ctx, u.ID, acc.ID); err != nil || got.Status != accounts.StatusConnected {
		e.t.Fatalf("connect: %+v %v", got, err)
	}
	return owner{userID: u.ID, accountID: acc.ID, linkedID: laID}
}

type lead struct {
	id, campaign int64
	url          string
}

// lead creates a lead in a running campaign of the owner's account, in the given state.
func (e *env) lead(o owner, slug string, state campaigns.State) lead {
	e.t.Helper()
	ctx := context.Background()
	c, err := e.camps.Create(ctx, o.userID, campaigns.Input{AccountID: o.accountID, Name: slug, DailyLimit: 10, Steps: []campaigns.Step{
		{Type: campaigns.StepConnect, Note: "hi"},
		{Type: campaigns.StepMessage, Body: "thanks", DelayDays: 1},
		{Type: campaigns.StepFollowUp, Body: "ping", DelayDays: 2},
	}})
	if err != nil {
		e.t.Fatal(err)
	}
	url := "https://www.linkedin.com/in/" + slug
	id, err := mdb.NextID(ctx, e.db, mdb.Leads)
	if err != nil {
		e.t.Fatal(err)
	}
	testdb.Insert(e.t, e.db, mdb.Leads, bson.D{{Key: "_id", Value: id}, {Key: "user_id", Value: o.userID}, {Key: "profile_url", Value: url},
		{Key: "name", Value: slug}, {Key: "headline", Value: ""}, {Key: "company", Value: ""}, {Key: "created_at", Value: time.Now()}})
	if _, err := e.camps.AddLeads(ctx, o.userID, c.ID, []int64{id}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.camps.Start(ctx, o.userID, c.ID); err != nil {
		e.t.Fatal(err)
	}
	steps := map[campaigns.State][]campaigns.Event{
		campaigns.StateQueued:      nil,
		campaigns.StateRequestSent: {campaigns.EvRequestSent},
		campaigns.StateConnected:   {campaigns.EvRequestSent, campaigns.EvAccepted},
		campaigns.StateMessaged:    {campaigns.EvRequestSent, campaigns.EvAccepted, campaigns.EvMessageSent},
	}
	for _, ev := range steps[state] {
		if _, _, err := e.camps.Record(ctx, c.ID, id, ev, e.now.Add(-time.Hour), ""); err != nil {
			e.t.Fatal(err)
		}
	}
	return lead{id: id, campaign: c.ID, url: url}
}

func (e *env) state(l lead) (campaigns.State, *time.Time, int) {
	e.t.Helper()
	d := testdb.One[struct {
		State        campaigns.State `bson:"state"`
		NextActionAt *time.Time      `bson:"next_action_at"`
		CurrentStep  int             `bson:"current_step"`
	}](e.t, e.db, mdb.CampaignLeads, bson.D{{Key: "campaign_id", Value: l.campaign}, {Key: "lead_id", Value: l.id}})
	return d.State, d.NextActionAt, d.CurrentStep
}

func (e *env) count(coll string, filter bson.D) int {
	e.t.Helper()
	return testdb.Count(e.t, e.db, coll, filter)
}

func leadFilter(id int64) bson.D { return bson.D{{Key: "lead_id", Value: id}} }

var processed = bson.D{{Key: "processed_at", Value: bson.D{{Key: "$exists", Value: true}}}}

// messages returns a lead's stored conversation, oldest first.
func (e *env) messages(leadID int64) []struct {
	Direction string `bson:"direction"`
	Body      string `bson:"body"`
} {
	e.t.Helper()
	cur, err := e.db.Collection(mdb.Messages).Find(context.Background(), leadFilter(leadID), options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		e.t.Fatal(err)
	}
	var rows []struct {
		Direction string `bson:"direction"`
		Body      string `bson:"body"`
	}
	if err := cur.All(context.Background(), &rows); err != nil {
		e.t.Fatal(err)
	}
	return rows
}

// event builds an event body.
func (e *env) event(id, typ string, createdAt time.Time, data map[string]any) []byte {
	e.t.Helper()
	b, err := json.Marshal(map[string]any{"id": id, "type": typ, "createdAt": createdAt.Format(time.RFC3339Nano), "data": data})
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// deliver posts a correctly signed body.
func (e *env) deliver(body []byte) *httptest.ResponseRecorder {
	ts, sig := Sign(docSecret, body, e.now)
	return e.post(body, map[string]string{TimestampHeader: ts, SignatureHeader: sig})
}

func (e *env) post(body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/webhooks/linkedapi", bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return rec
}

func status(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var r struct{ Status string }
	json.Unmarshal(rec.Body.Bytes(), &r)
	return r.Status
}

func (e *env) nextID() string { e.evSeq++; return fmt.Sprintf("evt-%d", e.evSeq) }

func (e *env) accepted(o owner, personURL string, at time.Time) []byte {
	return e.event(e.nextID(), "network.connectionAccepted", at, map[string]any{
		"accountId": o.linkedID, "personUrn": nil, "personUrl": personURL, "detectedAt": at.Format(time.RFC3339)})
}

func (e *env) reply(o owner, messageID, text string, data map[string]any) []byte {
	d := map[string]any{"accountId": o.linkedID, "type": "st", "threadId": "th-1", "messageId": messageID,
		"sender": "them", "text": text, "time": e.now.Add(-time.Minute).Format(time.RFC3339)}
	for k, v := range data {
		d[k] = v
	}
	return e.event(e.nextID(), "inbox.messageReceived", e.now, d)
}

func TestBadRequestsAreRejectedAndStoreNothing(t *testing.T) {
	e := newEnv(t)
	o := e.owner("a@example.com")
	l := e.lead(o, "jane", campaigns.StateRequestSent)
	body := e.accepted(o, l.url, e.now.Add(-time.Minute))
	ts, sig := Sign(docSecret, body, e.now)
	oldTS, oldSig := Sign(docSecret, body, e.now.Add(-10*time.Minute))
	otherTS, otherSig := Sign("another-secret", body, e.now)

	cases := map[string]map[string]string{
		"no headers":                    {},
		"no signature":                  {TimestampHeader: ts},
		"no timestamp":                  {SignatureHeader: sig},
		"wrong signature":               {TimestampHeader: ts, SignatureHeader: strings.Repeat("a", 64)},
		"garbage signature":             {TimestampHeader: ts, SignatureHeader: "not-hex"},
		"signed too long ago":           {TimestampHeader: oldTS, SignatureHeader: oldSig},
		"wrong secret":                  {TimestampHeader: otherTS, SignatureHeader: otherSig},
		"signature of a different body": {TimestampHeader: ts, SignatureHeader: func() string { _, s := Sign(docSecret, []byte("{}"), e.now); return s }()},
	}
	for name, hdr := range cases {
		rec := e.post(body, hdr)
		if rec.Code != 401 || !strings.Contains(rec.Body.String(), "invalid_signature") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// A body altered after signing.
	if rec := e.post(append(body[:len(body)-1:len(body)-1], []byte(` ,"x":1}`)...), map[string]string{TimestampHeader: ts, SignatureHeader: sig}); rec.Code != 401 {
		t.Errorf("tampered body: %d", rec.Code)
	}
	// None of that stored an event or changed a lead.
	if n := e.count(mdb.WebhookEvents, bson.D{}); n != 0 {
		t.Fatalf("%d events stored from rejected requests", n)
	}
	if s, _, _ := e.state(l); s != campaigns.StateRequestSent {
		t.Fatalf("state changed by a rejected request: %s", s)
	}
	// The same body, properly signed, works.
	if rec := e.deliver(body); rec.Code != 200 {
		t.Fatalf("valid delivery: %d %s", rec.Code, rec.Body)
	}
}

func TestEndpointNeedsASecretAndSaneInput(t *testing.T) {
	e := newEnv(t)
	body := []byte(vector1)
	noSecret := New(e.db, e.rec, "", nil)
	noSecret.now = func() time.Time { return time.Unix(1700000000, 0) }
	mux := http.NewServeMux()
	noSecret.Register(mux)
	req := httptest.NewRequest("POST", "/webhooks/linkedapi", bytes.NewReader(body))
	req.Header.Set(TimestampHeader, "1700000000")
	req.Header.Set(SignatureHeader, vector1OK)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("no secret configured: %d (an endpoint that cannot verify must refuse)", rec.Code)
	}

	// Signed but malformed: 400, and no event row so a fixed resend is not mistaken for a duplicate.
	for name, b := range map[string]string{
		"not json": "nope", "no id": `{"type":"x"}`, "no type": `{"id":"1"}`, "empty": ``,
	} {
		ts, sig := Sign(docSecret, []byte(b), e.now)
		if rec := e.post([]byte(b), map[string]string{TimestampHeader: ts, SignatureHeader: sig}); rec.Code != 400 {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	if n := e.count(mdb.WebhookEvents, bson.D{}); n != 0 {
		t.Fatalf("%d events stored for malformed input", n)
	}
	// Oversized bodies are refused before any signature work.
	if rec := e.post(bytes.Repeat([]byte("a"), 2<<20), nil); rec.Code != 413 {
		t.Fatalf("2 MiB body: %d", rec.Code)
	}
	if rec := httptest.NewRecorder(); true {
		e.srv.ServeHTTP(rec, httptest.NewRequest("GET", "/webhooks/linkedapi", nil))
		if rec.Code == 200 {
			t.Fatal("GET accepted")
		}
	}
	// A browser page cannot post here: the CSRF check applies to the endpoint too.
	req = httptest.NewRequest("POST", "/webhooks/linkedapi", bytes.NewReader(body))
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("cross-site post: %d", rec.Code)
	}
}

func TestAcceptedRequestAdvancesTheLead(t *testing.T) {
	e := newEnv(t)
	o := e.owner("a@example.com")
	l := e.lead(o, "jane-doe", campaigns.StateRequestSent)
	at := utc(2026, 11, 10, 10, 0) // Tuesday

	// A differently written URL still finds the lead.
	rec := e.deliver(e.accepted(o, "https://UK.linkedin.com/in/Jane-Doe/?trk=x", at))
	if rec.Code != 200 || status(t, rec) != "ok" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	s, due, step := e.state(l)
	// The message is due one business day after the acceptance: Wednesday 10:00.
	if s != campaigns.StateConnected || step != 2 || due == nil || !due.Equal(utc(2026, 11, 11, 10, 0)) {
		t.Fatalf("state %s step %d due %v", s, step, due)
	}
}

func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

func TestReplyStopsTheSequenceAndIsStored(t *testing.T) {
	e := newEnv(t)
	o := e.owner("a@example.com")
	l := e.lead(o, "jane-doe", campaigns.StateMessaged)

	rec := e.deliver(e.reply(o, "m-1", "Thanks, interested! Call me.", map[string]any{"personPublicUrl": l.url}))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	s, due, step := e.state(l)
	if s != campaigns.StateReplied || due != nil || step != 3 {
		t.Fatalf("state %s due %v step %d", s, due, step)
	}
	if msgs := e.messages(l.id); len(msgs) != 1 || msgs[0].Direction != "in" || msgs[0].Body != "Thanks, interested! Call me." {
		t.Fatalf("stored messages: %+v", msgs)
	}

	// The same LinkedIn message in a different event is stored once.
	e.deliver(e.reply(o, "m-1", "Thanks, interested! Call me.", map[string]any{"personPublicUrl": l.url}))
	if n := e.count(mdb.Messages, leadFilter(l.id)); n != 1 {
		t.Fatalf("%d copies of one message", n)
	}
	// A second, different message is added; the state stays replied.
	e.deliver(e.reply(o, "m-2", "Also, my number is...", map[string]any{"personPublicUrl": l.url}))
	if n := e.count(mdb.Messages, leadFilter(l.id)); n != 2 {
		t.Fatalf("%d messages", n)
	}
	if s, _, _ := e.state(l); s != campaigns.StateReplied {
		t.Fatalf("state %s", s)
	}
}

func TestReplyFromSomeoneWhoNeverAcceptedStillCounts(t *testing.T) {
	e := newEnv(t)
	o := e.owner("a@example.com")
	l := e.lead(o, "quick", campaigns.StateQueued) // we have not even recorded the request
	e.deliver(e.reply(o, "m-1", "hello", map[string]any{"personPublicUrl": l.url}))
	s, due, step := e.state(l)
	if s != campaigns.StateReplied || due != nil || step < 2 {
		t.Fatalf("state %s due %v step %d", s, due, step)
	}
}

func TestOnlyInboundMessagesFromOurLeadsAreStored(t *testing.T) {
	e := newEnv(t)
	o := e.owner("a@example.com")
	l := e.lead(o, "jane", campaigns.StateMessaged)

	// Our own message (also reported for API-sent ones), and a stranger's message.
	e.deliver(e.reply(o, "m-1", "from us", map[string]any{"personPublicUrl": l.url, "sender": "us"}))
	e.deliver(e.reply(o, "m-2", "from a stranger", map[string]any{"personPublicUrl": "https://www.linkedin.com/in/stranger"}))
	e.deliver(e.reply(o, "m-3", "no identity at all", map[string]any{}))
	if n := e.count(mdb.Messages, bson.D{}); n != 0 {
		t.Fatalf("%d messages stored, want none", n)
	}
	if s, _, _ := e.state(l); s != campaigns.StateMessaged {
		t.Fatalf("state changed: %s", s)
	}
}

func TestDuplicateDeliveryChangesNothing(t *testing.T) {
	e := newEnv(t)
	o := e.owner("a@example.com")
	l := e.lead(o, "jane", campaigns.StateRequestSent)
	body := e.accepted(o, l.url, utc(2026, 11, 10, 10, 0))

	if rec := e.deliver(body); status(t, rec) != "ok" {
		t.Fatalf("first: %s", rec.Body)
	}
	_, due1, _ := e.state(l)
	e.now = e.now.Add(2 * time.Minute) // a retry, a little later
	rec := e.deliver(body)
	if rec.Code != 200 || status(t, rec) != "duplicate" {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body)
	}
	if _, due2, _ := e.state(l); due2 == nil || !due2.Equal(*due1) {
		t.Fatalf("a replay changed the due time: %v -> %v", due1, due2)
	}
	if n := e.count(mdb.WebhookEvents, bson.D{}); n != 1 {
		t.Fatalf("%d event rows", n)
	}

	// A reply replayed many times: one message, still replied.
	l2 := e.lead(o, "john", campaigns.StateMessaged)
	reply := e.reply(o, "m-9", "hi", map[string]any{"personPublicUrl": l2.url})
	for i := 0; i < 5; i++ {
		e.deliver(reply)
	}
	if n := e.count(mdb.Messages, leadFilter(l2.id)); n != 1 {
		t.Fatalf("%d messages after replays", n)
	}
}

func TestAFailedAttemptIsHandledAgainOnRetry(t *testing.T) {
	e := newEnv(t)
	o := e.owner("a@example.com")
	l := e.lead(o, "jane", campaigns.StateRequestSent)
	body := e.accepted(o, l.url, utc(2026, 11, 10, 10, 0))

	e.rec.fail = true // the database hiccups while handling the event
	if rec := e.deliver(body); rec.Code != 500 {
		t.Fatalf("failing delivery: %d %s", rec.Code, rec.Body)
	}
	if s, _, _ := e.state(l); s != campaigns.StateRequestSent {
		t.Fatalf("state %s", s)
	}
	if n := e.count(mdb.WebhookEvents, processed); n != 0 {
		t.Fatal("an event whose handling failed was marked processed")
	}

	e.rec.fail = false // Linked API retries the same event
	rec := e.deliver(body)
	if rec.Code != 200 || status(t, rec) != "ok" {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	if s, _, _ := e.state(l); s != campaigns.StateConnected {
		t.Fatalf("retry did not apply the event: %s", s)
	}
	if n := e.count(mdb.WebhookEvents, processed); n != 1 {
		t.Fatal("event not marked processed after the retry")
	}
}

func TestEventsInTheWrongOrderEndConsistent(t *testing.T) {
	e := newEnv(t)
	o := e.owner("a@example.com")

	// A reply arrives before the acceptance event: the lead ends replied, and the late
	// acceptance changes nothing.
	l := e.lead(o, "a", campaigns.StateRequestSent)
	e.deliver(e.reply(o, "m-1", "hi", map[string]any{"personPublicUrl": l.url}))
	e.deliver(e.accepted(o, l.url, utc(2026, 11, 10, 9, 0)))
	if s, due, _ := e.state(l); s != campaigns.StateReplied || due != nil {
		t.Fatalf("reply then late acceptance: %s %v", s, due)
	}

	// The acceptance arrives while we still think the request is only queued (we sent it,
	// then crashed before recording it). Our own late bookkeeping must not undo it.
	l2 := e.lead(o, "b", campaigns.StateQueued)
	e.deliver(e.accepted(o, l2.url, utc(2026, 11, 10, 9, 0)))
	if s, _, _ := e.state(l2); s != campaigns.StateConnected {
		t.Fatalf("acceptance from queued: %s", s)
	}
	_, out, err := e.camps.Record(context.Background(), l2.campaign, l2.id, campaigns.EvRequestSent, e.now, "")
	if err != nil || out != campaigns.Noop {
		t.Fatalf("late request-sent bookkeeping: %v %v", out, err)
	}
	if s, _, _ := e.state(l2); s != campaigns.StateConnected {
		t.Fatalf("state after late bookkeeping: %s", s)
	}

	// A reply, then the same acceptance delivered twice and a message event for the same
	// person in between: always one consistent final state.
	l3 := e.lead(o, "c", campaigns.StateRequestSent)
	acc := e.accepted(o, l3.url, utc(2026, 11, 10, 9, 0))
	for _, body := range [][]byte{acc, e.reply(o, "m-3", "yo", map[string]any{"personPublicUrl": l3.url}), acc, acc} {
		e.deliver(body)
	}
	if s, due, _ := e.state(l3); s != campaigns.StateReplied || due != nil {
		t.Fatalf("mixed order: %s %v", s, due)
	}
}

func TestAccountStatusEventsApplyNewestFirstWins(t *testing.T) {
	e := newEnv(t)
	o := e.owner("a@example.com")
	statusOf := func() string {
		return testdb.One[struct {
			Status string `bson:"status"`
		}](t, e.db, mdb.Accounts, bson.D{{Key: "_id", Value: o.accountID}}).Status
	}
	send := func(typ string, at time.Time) {
		e.deliver(e.event(e.nextID(), typ, at, map[string]any{"accountId": o.linkedID, "status": "x"}))
	}
	t1, t2, t3 := utc(2026, 11, 10, 9, 0), utc(2026, 11, 10, 9, 30), utc(2026, 11, 10, 10, 0)

	send("account.reconnectionRequired", t1)
	if statusOf() != "reconnection_required" {
		t.Fatalf("status %s", statusOf())
	}
	send("account.active", t3) // newest
	send("account.reconnectionRequired", t2)
	if statusOf() != "connected" {
		t.Fatalf("an older event overwrote a newer one: %s", statusOf())
	}
	send("account.frozen", utc(2026, 11, 10, 10, 5))
	if statusOf() != "frozen" {
		t.Fatalf("status %s", statusOf())
	}
	send("account.deleted", utc(2026, 11, 10, 10, 6))
	if statusOf() != "disconnected" {
		t.Fatalf("status %s", statusOf())
	}

	// An absurd future timestamp cannot lock the status: it is treated as "now".
	send("account.active", utc(2099, 1, 1, 0, 0))
	send("account.frozen", utc(2026, 11, 10, 10, 30))
	if statusOf() != "frozen" {
		t.Fatalf("a far-future timestamp blocked later events: %s", statusOf())
	}

	// Accounts that are still connecting or failed to connect are not touched by events.
	e.db.Collection(mdb.Accounts).UpdateOne(context.Background(), bson.D{{Key: "_id", Value: o.accountID}}, bson.D{
		{Key: "$set", Value: bson.D{{Key: "status", Value: "error"}}}, {Key: "$unset", Value: bson.D{{Key: "status_event_at", Value: ""}}}})
	send("account.active", utc(2026, 11, 10, 11, 0))
	if statusOf() != "error" {
		t.Fatalf("an event overrode a failed connection: %s", statusOf())
	}
}

func TestEventsOnlyAffectTheAccountOwnersLeads(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.owner("alice@example.com"), e.owner("bob@example.com")
	aLead := e.lead(alice, "shared-person", campaigns.StateRequestSent)
	bLead := e.lead(bob, "shared-person", campaigns.StateRequestSent) // the same person, Bob's own lead

	// An event for Alice's account moves Alice's lead only.
	e.deliver(e.accepted(alice, aLead.url, utc(2026, 11, 10, 10, 0)))
	if s, _, _ := e.state(aLead); s != campaigns.StateConnected {
		t.Fatalf("alice's lead: %s", s)
	}
	if s, _, _ := e.state(bLead); s != campaigns.StateRequestSent {
		t.Fatalf("bob's lead moved by an event for alice's account: %s", s)
	}

	// Bob's lead that Alice never contacted cannot be reached through Alice's account.
	only := e.lead(bob, "bobs-only", campaigns.StateMessaged)
	e.deliver(e.reply(alice, "m-1", "hello", map[string]any{"personPublicUrl": only.url}))
	if s, _, _ := e.state(only); s != campaigns.StateMessaged {
		t.Fatalf("bob's lead replied via alice's account: %s", s)
	}
	if n := e.count(mdb.Messages, leadFilter(only.id)); n != 0 {
		t.Fatal("a message was stored on another user's lead")
	}

	// Unknown and empty account ids are acknowledged and ignored.
	for _, id := range []string{"la-999", "", "' OR 1=1 --"} {
		rec := e.deliver(e.event(e.nextID(), "network.connectionAccepted", e.now,
			map[string]any{"accountId": id, "personUrl": bLead.url, "detectedAt": e.now.Format(time.RFC3339)}))
		if rec.Code != 200 {
			t.Errorf("account %q: %d", id, rec.Code)
		}
	}
	if s, _, _ := e.state(bLead); s != campaigns.StateRequestSent {
		t.Fatalf("bob's lead moved by an event for an unknown account: %s", s)
	}
}

func TestWeLearnOtherNamesForAPerson(t *testing.T) {
	e := newEnv(t)
	o := e.owner("a@example.com")
	l := e.lead(o, "jane-doe", campaigns.StateMessaged)

	// First reply names the person every way; a later one only by the hashed URL.
	e.deliver(e.reply(o, "m-1", "one", map[string]any{
		"personPublicUrl": l.url, "personHashedUrl": "https://www.linkedin.com/in/ACoAAB123", "personUrn": "urn:li:member:123"}))
	ids := testdb.One[struct {
		URN    *string `bson:"linkedin_urn"`
		Hashed *string `bson:"hashed_url"`
	}](t, e.db, mdb.Leads, bson.D{{Key: "_id", Value: l.id}})
	urn, hashed := ids.URN, ids.Hashed
	if urn == nil || *urn != "urn:li:member:123" || hashed == nil || *hashed != "https://www.linkedin.com/in/acoaab123" {
		t.Fatalf("remembered %v %v", urn, hashed)
	}
	for i, data := range []map[string]any{
		{"personPublicUrl": nil, "personHashedUrl": "https://www.linkedin.com/in/ACoAAB123"},
		{"personPublicUrl": nil, "personHashedUrl": nil, "personUrn": "urn:li:member:123"},
	} {
		e.deliver(e.reply(o, fmt.Sprintf("later-%d", i), "again", data))
	}
	if n := e.count(mdb.Messages, leadFilter(l.id)); n != 3 {
		t.Fatalf("%d messages stored, want 3 (each found by a different identity)", n)
	}
	// Acceptance events name people by personUrl or URN too.
	l2 := e.lead(o, "john-roe", campaigns.StateRequestSent)
	testdb.Update(t, e.db, mdb.Leads, bson.D{{Key: "_id", Value: l2.id}}, bson.D{{Key: "linkedin_urn", Value: "urn:li:member:9"}})
	e.deliver(e.event(e.nextID(), "network.connectionAccepted", e.now, map[string]any{
		"accountId": o.linkedID, "personUrn": "urn:li:member:9", "personUrl": "https://www.linkedin.com/in/ACoAAZZZ", "detectedAt": e.now.Format(time.RFC3339)}))
	if s, _, _ := e.state(l2); s != campaigns.StateConnected {
		t.Fatalf("matched by URN: %s", s)
	}
}

func TestOtherEventTypesAreAcknowledgedAndIgnored(t *testing.T) {
	e := newEnv(t)
	o := e.owner("a@example.com")
	l := e.lead(o, "jane", campaigns.StateRequestSent)
	for _, typ := range []string{"workflow.completed", "workflow.started", "inbox.messageSent", "network.connectionAdded",
		"network.connectionRequestReceived", "webhook.test", "something.brand.new"} {
		rec := e.deliver(e.event(e.nextID(), typ, e.now, map[string]any{"accountId": o.linkedID, "personUrl": l.url, "sender": "them", "text": "x"}))
		if rec.Code != 200 {
			t.Errorf("%s: %d", typ, rec.Code)
		}
	}
	if s, _, _ := e.state(l); s != campaigns.StateRequestSent {
		t.Fatalf("an ignored event changed the lead: %s", s)
	}
	if n := e.count(mdb.WebhookEvents, processed); n != 7 {
		t.Fatalf("%d events recorded as handled, want 7", n)
	}
}

func TestMessageTextIsBounded(t *testing.T) {
	e := newEnv(t)
	o := e.owner("a@example.com")
	l := e.lead(o, "jane", campaigns.StateMessaged)
	e.deliver(e.reply(o, "m-1", strings.Repeat("é", 25000), map[string]any{"personPublicUrl": l.url}))
	e.deliver(e.reply(o, "m-2", "   ", map[string]any{"personPublicUrl": l.url}))
	e.deliver(e.reply(o, "m-3", "<script>alert(1)</script>", map[string]any{"personPublicUrl": l.url}))
	var got []string
	for _, m := range e.messages(l.id) {
		got = append(got, m.Body)
	}
	if len(got) != 3 || len([]rune(got[0])) != maxMessageLen || got[1] != "(no text)" || got[2] != "<script>alert(1)</script>" {
		t.Fatalf("stored bodies: %d %q", len(got), []any{len([]rune(got[0])), got[1], got[2]})
	}
}

func TestReconcilerReplaysOnlyWhatWasMissed(t *testing.T) {
	e := newEnv(t)
	o := e.owner("a@example.com")
	l := e.lead(o, "jane", campaigns.StateRequestSent)
	// Event e-done was handled here already; e-missed was not.
	e.deliver(e.event("e-done", "network.connectionAccepted", e.now, map[string]any{"accountId": o.linkedID, "personUrl": l.url, "detectedAt": e.now.Format(time.RFC3339)}))
	e.fake.Deliveries = []linkedin.Delivery{
		{ID: "d-missed", EventID: "e-missed", EventType: "inbox.messageReceived", Status: "failed", Attempts: 8},
		{ID: "d-handled", EventID: "e-done", EventType: "network.connectionAccepted", Status: "failed", Attempts: 8},
		{ID: "d-ok", EventID: "e-ok", Status: "success", Attempts: 1},
		{ID: "d-pending", EventID: "e-p", Status: "pending"},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewReconciler(e.db, e.fake, log)
	clock := e.now
	r.now = func() time.Time { return clock }

	n, err := r.Once(context.Background())
	if err != nil || n != 1 || fmt.Sprint(e.fake.Replayed) != "[d-missed]" {
		t.Fatalf("first run: %d %v replayed %v", n, err, e.fake.Replayed)
	}
	// Not again straight away.
	if n, _ := r.Once(context.Background()); n != 0 {
		t.Fatalf("replayed again immediately: %d", n)
	}
	// Later, up to the limit, then never again.
	for i := 2; i <= 5; i++ {
		clock = clock.Add(11 * time.Minute)
		n, _ := r.Once(context.Background())
		if want := map[int]int{2: 1, 3: 1, 4: 0, 5: 0}[i]; n != want {
			t.Fatalf("run %d replayed %d, want %d", i, n, want)
		}
	}
	// Linked API errors are reported, not swallowed, and do not crash the loop.
	e.fake.Err["ListWebhookDeliveries"] = &linkedin.Error{Type: "network", Status: 0}
	if _, err := r.Once(context.Background()); err == nil {
		t.Fatal("listing failure not reported")
	}
}

func TestOldDedupeRowsArePruned(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.db.Collection(mdb.WebhookEvents).InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: "old"}, {Key: "received_at", Value: e.now.Add(-30 * 24 * time.Hour)}},
		bson.D{{Key: "_id", Value: "fresh"}, {Key: "received_at", Value: e.now.Add(-time.Hour)}},
	})
	r := NewReconciler(e.db, e.fake, nil)
	r.now = func() time.Time { return e.now }
	r.Once(ctx)
	if e.count(mdb.WebhookEvents, bson.D{{Key: "_id", Value: "old"}}) != 0 || e.count(mdb.WebhookEvents, bson.D{{Key: "_id", Value: "fresh"}}) != 1 {
		t.Fatal("pruning removed the wrong rows")
	}
}
