package linkedin

import (
	"context"
	"fmt"
	"sync"
)

// Fake is an in-memory Client for tests and local development without a Linked API account.
// Drive it from a test: FinishSession completes a connection session, SetAccountStatus
// changes an account, and Err makes the next call to a named method fail.
type Fake struct {
	mu         sync.Mutex
	n          int
	Accounts   []Account
	Deliveries []Delivery
	Replayed   []string
	sessions   map[string]string // session id -> status
	Err        map[string]error  // method name -> error returned once
	Calls      []string          // method names, in call order
	Workflow   func(token string, action map[string]any) (WorkflowStart, error)
	// GetWorkflowFn overrides GetWorkflow's default "completed, success" answer.
	GetWorkflowFn func(token, workflowID string) (WorkflowResult, error)
}

var _ Client = (*Fake)(nil)

func NewFake() *Fake {
	return &Fake{sessions: map[string]string{}, Err: map[string]error{}}
}

func (f *Fake) enter(method string) error {
	f.Calls = append(f.Calls, method)
	if err := f.Err[method]; err != nil {
		delete(f.Err, method)
		return err
	}
	return nil
}

func (f *Fake) CreateConnectionSession(ctx context.Context) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("CreateConnectionSession"); err != nil {
		return Session{}, err
	}
	f.n++
	id := fmt.Sprintf("sess-%d", f.n)
	f.sessions[id] = SessionPending
	return Session{ID: id, Link: "https://app.linkedapi.io/connection/" + id}, nil
}

func (f *Fake) CreateReconnectionSession(ctx context.Context, accountID string) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("CreateReconnectionSession"); err != nil {
		return Session{}, err
	}
	f.n++
	id := fmt.Sprintf("resess-%d", f.n)
	f.sessions[id] = SessionPending
	return Session{ID: id, Link: "https://app.linkedapi.io/connection/" + id}, nil
}

func (f *Fake) GetConnectionSession(ctx context.Context, sessionID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("GetConnectionSession"); err != nil {
		return "", err
	}
	st, ok := f.sessions[sessionID]
	if !ok {
		return "", &Error{Type: "sessionNotFound", Status: 404}
	}
	return st, nil
}

func (f *Fake) CancelConnectionSession(ctx context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("CancelConnectionSession"); err != nil {
		return err
	}
	f.sessions[sessionID] = SessionCancelled
	return nil
}

func (f *Fake) ListAccounts(ctx context.Context) ([]Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("ListAccounts"); err != nil {
		return nil, err
	}
	return append([]Account(nil), f.Accounts...), nil
}

// Deliveries is what ListWebhookDeliveries returns; Replayed lists replayed delivery ids.
func (f *Fake) ListWebhookDeliveries(ctx context.Context) ([]Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("ListWebhookDeliveries"); err != nil {
		return nil, err
	}
	return append([]Delivery(nil), f.Deliveries...), nil
}

func (f *Fake) ReplayWebhookDelivery(ctx context.Context, deliveryID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("ReplayWebhookDelivery"); err != nil {
		return err
	}
	f.Replayed = append(f.Replayed, deliveryID)
	return nil
}

func (f *Fake) StartWorkflow(ctx context.Context, token string, action map[string]any) (WorkflowStart, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("StartWorkflow"); err != nil {
		return WorkflowStart{}, err
	}
	if f.Workflow != nil {
		return f.Workflow(token, action)
	}
	f.n++
	return WorkflowStart{ID: fmt.Sprintf("wf-%d", f.n), Status: "pending", PendingReason: "queued"}, nil
}

func (f *Fake) GetWorkflow(ctx context.Context, token, workflowID string) (WorkflowResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter("GetWorkflow"); err != nil {
		return WorkflowResult{}, err
	}
	if f.GetWorkflowFn != nil {
		return f.GetWorkflowFn(token, workflowID)
	}
	return WorkflowResult{Status: "completed", Completion: []byte(`{"success":true}`)}, nil
}

// SetSessionStatus forces a session into a status such as SessionStreaming or SessionExpired.
func (f *Fake) SetSessionStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[id] = status
}

// FinishSession marks a session successful and adds the account it created.
func (f *Fake) FinishSession(id string, a Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[id] = SessionSuccess
	if a.Status == "" {
		a.Status = AccountActive
	}
	f.Accounts = append(f.Accounts, a)
}

// AddAccount adds an account without a session (e.g. one made in the Linked API dashboard).
func (f *Fake) AddAccount(a Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a.Status == "" {
		a.Status = AccountActive
	}
	f.Accounts = append(f.Accounts, a)
}

// SetAccountStatus changes a stored account's status; it reports whether the account exists.
func (f *Fake) SetAccountStatus(id, status string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.Accounts {
		if f.Accounts[i].ID == id {
			f.Accounts[i].Status = status
			return true
		}
	}
	return false
}

// RemoveAccount deletes an account, as if it was disconnected in Linked API.
func (f *Fake) RemoveAccount(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.Accounts {
		if f.Accounts[i].ID == id {
			f.Accounts = append(f.Accounts[:i], f.Accounts[i+1:]...)
			return
		}
	}
}
