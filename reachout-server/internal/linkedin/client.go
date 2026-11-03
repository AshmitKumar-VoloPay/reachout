// Package linkedin is a thin client for the Linked API (https://linkedapi.io).
// Contract notes live in docs/linkedapi.md. Callers depend on the Client interface so
// tests and local development can use Fake instead of the network.
package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Account is a LinkedIn account connected to our Linked API subscription.
type Account struct {
	ID                  string // Linked API account id (UUID)
	Name                string
	URL                 string // public profile URL
	AvatarURL           string
	Headline            string
	CountryCode         string
	IdentificationToken string // secret: never log or return to browsers
	Status              string // active | frozen | reconnection_required
	ConnectedAt         time.Time
	ReconnectionLink    string
}

// Linked API account statuses.
const (
	AccountActive               = "active"
	AccountFrozen               = "frozen"
	AccountReconnectionRequired = "reconnection_required"
)

type Session struct {
	ID   string
	Link string // page the user opens to log into LinkedIn
}

// Connection session statuses.
const (
	SessionPending   = "pending"
	SessionPreparing = "preparing"
	SessionServing   = "serving"
	SessionStreaming = "streaming"
	SessionSuccess   = "success"
	SessionExpired   = "expired"
	SessionError     = "error"
	SessionCancelled = "cancelled"
)

type WorkflowStart struct {
	ID            string
	Status        string // pending | running
	PendingReason string // queued | outsideWorkingHours | ""
	Message       string
}

type WorkflowFailure struct {
	Reason  string
	Message string
}

type WorkflowResult struct {
	Status        string // pending | running | completed | failed
	PendingReason string
	Completion    json.RawMessage // action result when completed
	Failure       *WorkflowFailure
}

// Client is everything the app needs from Linked API.
type Client interface {
	// Admin API (linked-api-token only).
	CreateConnectionSession(ctx context.Context) (Session, error)
	CreateReconnectionSession(ctx context.Context, accountID string) (Session, error)
	GetConnectionSession(ctx context.Context, sessionID string) (status string, err error)
	CancelConnectionSession(ctx context.Context, sessionID string) error
	ListAccounts(ctx context.Context) ([]Account, error)

	// Workflow API (also needs the account's identification token).
	StartWorkflow(ctx context.Context, identificationToken string, action map[string]any) (WorkflowStart, error)
	GetWorkflow(ctx context.Context, identificationToken, workflowID string) (WorkflowResult, error)
}

// Error is an error reported by Linked API (Type and Message come from its error body).
type Error struct {
	Type    string
	Message string
	Status  int // HTTP status; 0 when the request never got a response
}

func (e *Error) Error() string {
	if e.Message == "" {
		return "linkedapi: " + e.Type
	}
	return "linkedapi: " + e.Type + ": " + e.Message
}

// Transient reports whether retrying later may succeed.
func (e *Error) Transient() bool {
	return e.Status == 0 || e.Status == 429 || e.Status >= 500
}

func asError(err error) (*Error, bool) {
	var e *Error
	return e, errors.As(err, &e)
}

func hasType(err error, types ...string) bool {
	e, ok := asError(err)
	if !ok {
		return false
	}
	for _, t := range types {
		if e.Type == t {
			return true
		}
	}
	return false
}

// IsTransient reports whether err is a network failure, 429 or 5xx.
func IsTransient(err error) bool {
	e, ok := asError(err)
	return ok && e.Transient()
}

// IsLimitHit reports that a LinkedIn or configured limit stopped an action. Connection
// request limits surface as requestNotAllowed, note limits as noteLimitExceeded.
// Callers should reschedule, not fail the lead.
func IsLimitHit(err error) bool {
	return hasType(err, "limitExceeded", "requestNotAllowed", "noteLimitExceeded")
}

// IsSignedOut reports that the LinkedIn session ended and the account must be reconnected.
func IsSignedOut(err error) bool { return hasType(err, "linkedinAccountSignedOut") }

// IsNoSeats reports that the subscription has no usable seat.
func IsNoSeats(err error) bool {
	return hasType(err, "noAvailableSeats", "subscriptionRequired")
}

// IsOutsideWorkingHours reports that Linked API's own working-hours policy rejected the request.
func IsOutsideWorkingHours(err error) bool { return hasType(err, "outsideWorkingHours") }
