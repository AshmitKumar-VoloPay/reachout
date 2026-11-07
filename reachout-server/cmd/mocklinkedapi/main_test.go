package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ashmit/reachout-server/internal/linkedin"
)

// The mock must speak what the real client expects, so test it with the real client.
func TestMockWorksWithTheRealClient(t *testing.T) {
	srv := httptest.NewServer(newMock().handler("http://mock"))
	defer srv.Close()
	c := linkedin.NewHTTPClient(srv.URL, "any-token")
	ctx := t.Context()

	sess, err := c.CreateConnectionSession(ctx)
	if err != nil || sess.ID == "" || sess.Link == "" {
		t.Fatalf("session: %+v %v", sess, err)
	}
	var status string
	for i := 0; i < 3; i++ { // pending, streaming, success
		if status, err = c.GetConnectionSession(ctx, sess.ID); err != nil {
			t.Fatal(err)
		}
	}
	if status != linkedin.SessionSuccess {
		t.Fatalf("status after three checks: %q", status)
	}
	accs, err := c.ListAccounts(ctx)
	if err != nil || len(accs) != 1 || accs[0].IdentificationToken == "" || accs[0].Status != linkedin.AccountActive {
		t.Fatalf("accounts: %+v %v", accs, err)
	}
	if again, _ := c.GetConnectionSession(ctx, sess.ID); again != linkedin.SessionSuccess {
		t.Fatal("status changed after success")
	}
	if accs2, _ := c.ListAccounts(ctx); len(accs2) != 1 {
		t.Fatalf("a repeated check created another account: %d", len(accs2))
	}

	ws, err := c.StartWorkflow(ctx, accs[0].IdentificationToken, map[string]any{"actionType": "st.searchPeople", "limit": 3})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.GetWorkflow(ctx, accs[0].IdentificationToken, ws.ID)
	if err != nil || res.Status != "completed" {
		t.Fatalf("workflow: %+v %v", res, err)
	}
	var comp struct {
		Success bool             `json:"success"`
		Data    []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(res.Completion, &comp); err != nil || !comp.Success || len(comp.Data) != 3 {
		t.Fatalf("search result: %s %v", res.Completion, err)
	}
	if ds, err := c.ListWebhookDeliveries(ctx); err != nil || len(ds) != 0 {
		t.Fatalf("deliveries: %v %v", ds, err)
	}
}

func TestMockRequiresTheTokens(t *testing.T) {
	srv := httptest.NewServer(newMock().handler("http://mock"))
	defer srv.Close()
	for name, path := range map[string]string{"admin": "/admin/accounts.getAll", "workflow": "/workflows"} {
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader([]byte("{}")))
		if err != nil || resp.StatusCode != 401 {
			t.Errorf("%s without a token: %v %v", name, resp, err)
		}
	}
	req, _ := http.NewRequest("POST", srv.URL+"/workflows", bytes.NewReader([]byte("{}")))
	req.Header.Set("linked-api-token", "x") // admin token but no identification token
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Errorf("workflow without an identification token: %v %v", resp, err)
	}
}
