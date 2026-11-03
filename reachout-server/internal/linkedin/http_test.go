package linkedin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const secretToken = "admin-token-SECRET-123"

// server records the last request and replies with the given status and body.
func server(t *testing.T, status int, body string) (*HTTPClient, *http.Request, *string) {
	t.Helper()
	var last http.Request
	var lastBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = *r.Clone(context.Background())
		b, _ := io.ReadAll(r.Body)
		lastBody = string(b)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	return NewHTTPClient(ts.URL, secretToken), &last, &lastBody
}

func TestCreateConnectionSession(t *testing.T) {
	c, req, _ := server(t, 200, `{"success":true,"result":{"sessionId":"s1","connectionLink":"https://app.linkedapi.io/connection/s1"}}`)
	s, err := c.CreateConnectionSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "s1" || !strings.HasSuffix(s.Link, "/s1") {
		t.Fatalf("got %+v", s)
	}
	if req.Method != "POST" || req.URL.Path != "/admin/accounts.createConnectionSession" {
		t.Fatalf("wrong request: %s %s", req.Method, req.URL.Path)
	}
	if req.Header.Get("linked-api-token") != secretToken || req.Header.Get("identification-token") != "" {
		t.Fatalf("wrong auth headers: %v", req.Header)
	}
}

func TestReconnectAndSessionStatus(t *testing.T) {
	c, _, body := server(t, 200, `{"success":true,"result":{"reconnectionSessionId":"r1","reconnectionLink":"https://x/r1"}}`)
	s, err := c.CreateReconnectionSession(context.Background(), "acc-1")
	if err != nil || s.ID != "r1" || s.Link != "https://x/r1" {
		t.Fatalf("%+v %v", s, err)
	}
	if !strings.Contains(*body, `"accountId":"acc-1"`) {
		t.Fatalf("body %s", *body)
	}

	c, _, body = server(t, 200, `{"success":true,"result":{"session":{"sessionId":"s1","status":"streaming","type":"initial"}}}`)
	st, err := c.GetConnectionSession(context.Background(), "s1")
	if err != nil || st != SessionStreaming || !strings.Contains(*body, `"sessionId":"s1"`) {
		t.Fatalf("%q %v body=%s", st, err, *body)
	}
}

func TestListAccountsHandlesNulls(t *testing.T) {
	c, _, _ := server(t, 200, `{"success":true,"result":{"accounts":[
	 {"id":"a1","name":"Ada","url":"https://www.linkedin.com/in/ada","avatarUrl":null,"headline":null,
	  "countryCode":"IN","identificationToken":"tok-1","status":"active","connectedAt":"2026-11-03T10:00:00Z"},
	 {"id":"a2","name":"Bo","url":"u","avatarUrl":"https://img","headline":"CEO","countryCode":"US",
	  "identificationToken":"tok-2","status":"reconnection_required","connectedAt":"2026-11-02T10:00:00Z",
	  "reconnectionSessionId":"r","reconnectionLink":"https://x/r"}],
	 "pendingConnectionSessions":[]}}`)
	accs, err := c.ListAccounts(context.Background())
	if err != nil || len(accs) != 2 {
		t.Fatalf("%v %v", accs, err)
	}
	if accs[0].Headline != "" || accs[0].AvatarURL != "" || accs[0].IdentificationToken != "tok-1" {
		t.Fatalf("a1: %+v", accs[0])
	}
	if accs[1].Headline != "CEO" || accs[1].Status != AccountReconnectionRequired || accs[1].ReconnectionLink != "https://x/r" {
		t.Fatalf("a2: %+v", accs[1])
	}
	if !accs[0].ConnectedAt.Equal(time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("connectedAt: %v", accs[0].ConnectedAt)
	}
}

func TestWorkflowStartAndGet(t *testing.T) {
	c, req, body := server(t, 200, `{"success":true,"result":{"workflowId":"wf-1","workflowStatus":"pending","pendingReason":"queued","message":"m"}}`)
	ws, err := c.StartWorkflow(context.Background(), "ident-tok", map[string]any{"actionType": "st.checkConnectionStatus", "personUrl": "https://www.linkedin.com/in/x"})
	if err != nil || ws.ID != "wf-1" || ws.PendingReason != "queued" {
		t.Fatalf("%+v %v", ws, err)
	}
	if req.URL.Path != "/workflows" || req.Header.Get("identification-token") != "ident-tok" || req.Header.Get("linked-api-token") != secretToken {
		t.Fatalf("wrong request: %s %v", req.URL.Path, req.Header)
	}
	var sent map[string]any
	json.Unmarshal([]byte(*body), &sent)
	if sent["actionType"] != "st.checkConnectionStatus" {
		t.Fatalf("body %s", *body)
	}

	c, req, _ = server(t, 200, `{"success":true,"result":{"workflowStatus":"completed","pendingReason":null,
	  "completion":{"actionType":"st.checkConnectionStatus","success":true,"data":{"connectionStatus":"pending"}}}}`)
	wr, err := c.GetWorkflow(context.Background(), "ident-tok", "wf-1")
	if err != nil || wr.Status != "completed" || !strings.Contains(string(wr.Completion), `"connectionStatus":"pending"`) {
		t.Fatalf("%+v %v", wr, err)
	}
	if req.Method != "GET" || req.URL.Path != "/workflows/wf-1" {
		t.Fatalf("wrong request: %s %s", req.Method, req.URL.Path)
	}

	c, _, _ = server(t, 200, `{"success":true,"result":{"workflowStatus":"failed","failure":{"reason":"linkedinAccountSignedOut","message":"signed out"}}}`)
	wr, err = c.GetWorkflow(context.Background(), "t", "wf-2")
	if err != nil || wr.Failure == nil || wr.Failure.Reason != "linkedinAccountSignedOut" {
		t.Fatalf("%+v %v", wr, err)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		check  func(error) bool
	}{
		{"no seats", 400, `{"success":false,"error":{"type":"noAvailableSeats","message":"none"}}`, IsNoSeats},
		{"signed out", 400, `{"success":false,"error":{"type":"linkedinAccountSignedOut","message":"x"}}`, IsSignedOut},
		{"limit", 200, `{"success":false,"error":{"type":"limitExceeded","message":"x"}}`, IsLimitHit},
		{"not allowed", 200, `{"success":false,"error":{"type":"requestNotAllowed","message":"x"}}`, IsLimitHit},
		{"rate limited", 429, `{"success":false,"error":{"type":"tooManyRequests","message":"x"}}`, IsTransient},
		{"server error", 502, `<html>bad gateway</html>`, IsTransient},
		{"outside hours", 400, `{"success":false,"error":{"type":"outsideWorkingHours"}}`, IsOutsideWorkingHours},
	}
	for _, c := range cases {
		cl, _, _ := server(t, c.status, c.body)
		_, err := cl.ListAccounts(context.Background())
		if err == nil || !c.check(err) {
			t.Errorf("%s: err=%v did not match", c.name, err)
		}
	}
	// A 4xx auth failure is not transient and not a limit.
	cl, _, _ := server(t, 401, `{"success":false,"error":{"type":"invalidLinkedApiToken","message":"bad"}}`)
	_, err := cl.ListAccounts(context.Background())
	if err == nil || IsTransient(err) || IsLimitHit(err) {
		t.Fatalf("401 misclassified: %v", err)
	}
}

func TestErrorsNeverContainToken(t *testing.T) {
	// Unreachable server: the error must not echo the admin token.
	c := NewHTTPClient("http://127.0.0.1:1", secretToken)
	_, err := c.ListAccounts(context.Background())
	if err == nil || !IsTransient(err) {
		t.Fatalf("expected transient network error, got %v", err)
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("token leaked in error: %v", err)
	}
	// Server that echoes the token back in its body: still not in our error type/message path
	// beyond what Linked API returned in error.message, which we pass through unchanged.
	cl, _, _ := server(t, 500, `not json`)
	_, err = cl.ListAccounts(context.Background())
	if err == nil || strings.Contains(err.Error(), secretToken) {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestContextCancel(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // hold the response until the test ends
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := NewHTTPClient(ts.URL, "t").ListAccounts(ctx); err == nil || !IsTransient(err) {
		t.Fatalf("got %v", err)
	}
}

func TestFakeBehavesLikeClient(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	s, _ := f.CreateConnectionSession(ctx)
	if st, _ := f.GetConnectionSession(ctx, s.ID); st != SessionPending {
		t.Fatalf("status %q", st)
	}
	f.FinishSession(s.ID, Account{ID: "a1", IdentificationToken: "t"})
	if st, _ := f.GetConnectionSession(ctx, s.ID); st != SessionSuccess {
		t.Fatalf("status %q", st)
	}
	accs, _ := f.ListAccounts(ctx)
	if len(accs) != 1 || accs[0].Status != AccountActive {
		t.Fatalf("%+v", accs)
	}
	f.Err["ListAccounts"] = &Error{Type: "tooManyRequests", Status: 429}
	if _, err := f.ListAccounts(ctx); !IsTransient(err) {
		t.Fatal("injected error not returned")
	}
	if _, err := f.ListAccounts(ctx); err != nil {
		t.Fatal("injected error should fire once")
	}
}
