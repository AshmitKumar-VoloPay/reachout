package linkedin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	DefaultBaseURL = "https://api.linkedapi.io"
	maxResponse    = 4 << 20
)

// HTTPClient talks to the real Linked API. The admin token is sent in a header and is
// never included in errors or logs.
type HTTPClient struct {
	base  string
	token string
	hc    *http.Client
}

var _ Client = (*HTTPClient)(nil)

func NewHTTPClient(baseURL, adminToken string) *HTTPClient {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &HTTPClient{
		base:  baseURL,
		token: adminToken,
		hc:    &http.Client{Timeout: 30 * time.Second},
	}
}

type envelope struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// call sends one request and decodes envelope.result into out (out may be nil).
func (c *HTTPClient) call(ctx context.Context, method, path, identToken string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("linked-api-token", c.token)
	if identToken != "" {
		req.Header.Set("identification-token", identToken)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		// url.Error text contains only the URL (no headers), but keep it out of the type.
		return &Error{Type: "network", Message: sanitizeNetErr(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return &Error{Type: "network", Message: "reading response failed", Status: resp.StatusCode}
	}

	var env envelope
	if jerr := json.Unmarshal(raw, &env); jerr != nil {
		return &Error{Type: "bad_response", Message: fmt.Sprintf("HTTP %d, non-JSON body", resp.StatusCode), Status: resp.StatusCode}
	}
	if !env.Success || resp.StatusCode >= 400 {
		e := &Error{Type: "unknown", Status: resp.StatusCode}
		if env.Error != nil {
			e.Type, e.Message = env.Error.Type, env.Error.Message
		}
		return e
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return &Error{Type: "bad_response", Message: "unexpected result shape", Status: resp.StatusCode}
		}
	}
	return nil
}

// sanitizeNetErr gives a fixed message so transport error text (URLs, addresses) never
// reaches logs or API responses.
func sanitizeNetErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return "request timed out"
	}
	return "request failed"
}

func (c *HTTPClient) admin(ctx context.Context, name string, body, out any) error {
	if body == nil {
		body = struct{}{}
	}
	return c.call(ctx, http.MethodPost, "/admin/"+name, "", body, out)
}

func (c *HTTPClient) CreateConnectionSession(ctx context.Context) (Session, error) {
	var r struct {
		SessionID      string `json:"sessionId"`
		ConnectionLink string `json:"connectionLink"`
	}
	if err := c.admin(ctx, "accounts.createConnectionSession", nil, &r); err != nil {
		return Session{}, err
	}
	return Session{ID: r.SessionID, Link: r.ConnectionLink}, nil
}

func (c *HTTPClient) CreateReconnectionSession(ctx context.Context, accountID string) (Session, error) {
	var r struct {
		SessionID string `json:"reconnectionSessionId"`
		Link      string `json:"reconnectionLink"`
	}
	if err := c.admin(ctx, "accounts.createReconnectionSession", map[string]string{"accountId": accountID}, &r); err != nil {
		return Session{}, err
	}
	return Session{ID: r.SessionID, Link: r.Link}, nil
}

func (c *HTTPClient) GetConnectionSession(ctx context.Context, sessionID string) (string, error) {
	var r struct {
		Session struct {
			Status string `json:"status"`
		} `json:"session"`
	}
	if err := c.admin(ctx, "accounts.getConnectionSession", map[string]string{"sessionId": sessionID}, &r); err != nil {
		return "", err
	}
	return r.Session.Status, nil
}

func (c *HTTPClient) CancelConnectionSession(ctx context.Context, sessionID string) error {
	return c.admin(ctx, "accounts.cancelConnectionSession", map[string]string{"sessionId": sessionID}, nil)
}

func (c *HTTPClient) ListAccounts(ctx context.Context) ([]Account, error) {
	var r struct {
		Accounts []struct {
			ID                  string    `json:"id"`
			Name                string    `json:"name"`
			URL                 string    `json:"url"`
			AvatarURL           *string   `json:"avatarUrl"`
			Headline            *string   `json:"headline"`
			CountryCode         string    `json:"countryCode"`
			IdentificationToken string    `json:"identificationToken"`
			Status              string    `json:"status"`
			ConnectedAt         time.Time `json:"connectedAt"`
			ReconnectionLink    string    `json:"reconnectionLink"`
		} `json:"accounts"`
	}
	if err := c.admin(ctx, "accounts.getAll", nil, &r); err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(r.Accounts))
	for _, a := range r.Accounts {
		acc := Account{
			ID: a.ID, Name: a.Name, URL: a.URL, CountryCode: a.CountryCode,
			IdentificationToken: a.IdentificationToken, Status: a.Status,
			ConnectedAt: a.ConnectedAt, ReconnectionLink: a.ReconnectionLink,
		}
		if a.AvatarURL != nil {
			acc.AvatarURL = *a.AvatarURL
		}
		if a.Headline != nil {
			acc.Headline = *a.Headline
		}
		out = append(out, acc)
	}
	return out, nil
}

// ListWebhookDeliveries returns recent deliveries, newest first. The response key name is
// not shown in Linked API's docs ("deliveries" is assumed); an unexpected shape yields an
// empty list, which is safe: nothing gets replayed.
func (c *HTTPClient) ListWebhookDeliveries(ctx context.Context) ([]Delivery, error) {
	var r struct {
		Deliveries []struct {
			ID        string `json:"id"`
			EventType string `json:"eventType"`
			EventID   string `json:"eventId"`
			Status    string `json:"status"`
			Attempts  int    `json:"attempts"`
		} `json:"deliveries"`
	}
	if err := c.admin(ctx, "webhook.deliveries", nil, &r); err != nil {
		return nil, err
	}
	out := make([]Delivery, 0, len(r.Deliveries))
	for _, d := range r.Deliveries {
		out = append(out, Delivery{ID: d.ID, EventType: d.EventType, EventID: d.EventID, Status: d.Status, Attempts: d.Attempts})
	}
	return out, nil
}

func (c *HTTPClient) ReplayWebhookDelivery(ctx context.Context, deliveryID string) error {
	return c.admin(ctx, "webhook.replayDelivery", map[string]string{"deliveryId": deliveryID}, nil)
}

func (c *HTTPClient) StartWorkflow(ctx context.Context, identToken string, action map[string]any) (WorkflowStart, error) {
	var r struct {
		WorkflowID    string  `json:"workflowId"`
		Status        string  `json:"workflowStatus"`
		PendingReason *string `json:"pendingReason"`
		Message       string  `json:"message"`
	}
	if err := c.call(ctx, http.MethodPost, "/workflows", identToken, action, &r); err != nil {
		return WorkflowStart{}, err
	}
	ws := WorkflowStart{ID: r.WorkflowID, Status: r.Status, Message: r.Message}
	if r.PendingReason != nil {
		ws.PendingReason = *r.PendingReason
	}
	return ws, nil
}

func (c *HTTPClient) GetWorkflow(ctx context.Context, identToken, workflowID string) (WorkflowResult, error) {
	var r struct {
		Status        string          `json:"workflowStatus"`
		PendingReason *string         `json:"pendingReason"`
		Completion    json.RawMessage `json:"completion"`
		Failure       *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"failure"`
	}
	if err := c.call(ctx, http.MethodGet, "/workflows/"+url.PathEscape(workflowID), identToken, nil, &r); err != nil {
		return WorkflowResult{}, err
	}
	wr := WorkflowResult{Status: r.Status, Completion: r.Completion}
	if r.PendingReason != nil {
		wr.PendingReason = *r.PendingReason
	}
	if r.Failure != nil {
		wr.Failure = &WorkflowFailure{Reason: r.Failure.Reason, Message: r.Failure.Message}
	}
	return wr, nil
}
