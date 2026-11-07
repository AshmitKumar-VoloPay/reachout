package accounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/mdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Account statuses stored in the account document's status field.
const (
	StatusPending              = "pending"
	StatusConnected            = "connected"
	StatusFrozen               = "frozen"
	StatusReconnectionRequired = "reconnection_required"
	StatusDisconnected         = "disconnected"
	StatusError                = "error"
)

// pendingTTL is how long a connection attempt may stay open. Linked API's own session
// lifetime is not documented, and only one attempt may be in flight at a time, so a
// stalled attempt must not block everyone else for long.
const pendingTTL = 10 * time.Minute

var (
	ErrNotFound         = errors.New("account not found")
	ErrConnectionBusy   = errors.New("another connection is in progress, try again in a few minutes")
	ErrAmbiguous        = errors.New("could not tell which LinkedIn account was just connected")
	ErrNotReconnectable = errors.New("account does not need to be reconnected")
	ErrInvalidSchedule  = errors.New("invalid schedule")
)

// Account is the browser-safe view of a connected account. It deliberately has no
// token field.
type Account struct {
	ID               int64      `json:"id"`
	Name             string     `json:"name"`
	ProfileURL       string     `json:"profile_url"`
	Headline         string     `json:"headline"`
	AvatarURL        string     `json:"avatar_url"`
	Status           string     `json:"status"`
	StatusDetail     string     `json:"status_detail,omitempty"`
	ConnectedAt      *time.Time `json:"connected_at,omitempty"`
	ConnectionLink   string     `json:"connection_link,omitempty"`   // while pending: where the user logs in
	ReconnectionLink string     `json:"reconnection_link,omitempty"` // when reconnection_required
	CreatedAt        time.Time  `json:"created_at"`
}

// accountDoc is the stored account. The sending schedule lives inside it.
type accountDoc struct {
	ID                  int64       `bson:"_id"`
	UserID              int64       `bson:"user_id"`
	Status              string      `bson:"status"`
	StatusDetail        string      `bson:"status_detail"`
	Name                string      `bson:"name"`
	ProfileURL          string      `bson:"profile_url"`
	Headline            string      `bson:"headline"`
	AvatarURL           string      `bson:"avatar_url"`
	IdentificationToken *string     `bson:"identification_token,omitempty"` // AES-GCM ciphertext, never the token
	LinkedInAccountID   *string     `bson:"linkedin_account_id,omitempty"`
	ConnectionSessionID *string     `bson:"connection_session_id,omitempty"`
	KnownAccountIDs     []string    `bson:"known_account_ids"` // Linked API accounts that existed before this session
	ReconnectionLink    *string     `bson:"reconnection_link,omitempty"`
	ConnectedAt         *time.Time  `bson:"connected_at,omitempty"`
	CreatedAt           time.Time   `bson:"created_at"`
	StatusEventAt       *time.Time  `bson:"status_event_at,omitempty"`
	NetworkSyncedAt     *time.Time  `bson:"network_synced_at,omitempty"`
	InboxSyncedAt       *time.Time  `bson:"inbox_synced_at,omitempty"`
	Schedule            scheduleDoc `bson:"schedule"`
}

type scheduleDoc struct {
	Timezone          string     `bson:"timezone"`
	WorkDays          []int      `bson:"work_days"` // 0 = Sunday .. 6 = Saturday
	StartMinute       int        `bson:"start_minute"`
	EndMinute         int        `bson:"end_minute"`
	Holidays          []string   `bson:"holidays"` // local dates, "2006-01-02"
	DailyConnectCap   int        `bson:"daily_connect_cap"`
	DailyMessageCap   int        `bson:"daily_message_cap"`
	RampStartedAt     time.Time  `bson:"ramp_started_at"` // only the date matters
	NextSendNotBefore *time.Time `bson:"next_send_not_before,omitempty"`
}

func (d accountDoc) account() Account {
	a := Account{
		ID: d.ID, Name: d.Name, ProfileURL: d.ProfileURL, Headline: d.Headline, AvatarURL: d.AvatarURL,
		Status: d.Status, StatusDetail: d.StatusDetail, ConnectedAt: d.ConnectedAt, CreatedAt: d.CreatedAt,
	}
	if d.ReconnectionLink != nil {
		a.ReconnectionLink = *d.ReconnectionLink
	}
	return a
}

// defaultSchedule is Monday to Friday, 09:30 to 17:30, with the warm-up starting today.
func defaultSchedule(timezone string, now time.Time) scheduleDoc {
	y, m, d := now.UTC().Date()
	return scheduleDoc{
		Timezone: timezone, WorkDays: []int{1, 2, 3, 4, 5}, StartMinute: 9*60 + 30, EndMinute: 17*60 + 30,
		Holidays: []string{}, DailyConnectCap: 5, DailyMessageCap: 10,
		RampStartedAt: time.Date(y, m, d, 0, 0, 0, 0, time.UTC),
	}
}

type Service struct {
	db     *mongo.Database
	li     linkedin.Client
	cipher *Cipher
	log    *slog.Logger
	now    func() time.Time
}

func NewService(db *mongo.Database, li linkedin.Client, c *Cipher, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, li: li, cipher: c, log: log, now: time.Now}
}

func (s *Service) coll() *mongo.Collection { return s.db.Collection(mdb.Accounts) }

func eq(k string, v any) bson.E { return bson.E{Key: k, Value: v} }

// Every read below filters on user_id, so one user can never see or touch another's
// account. A foreign or unknown id is reported as ErrNotFound either way. This filter is the
// only tenant guard: MongoDB has no foreign keys to back it up.
func (s *Service) doc(ctx context.Context, userID, id int64) (accountDoc, error) {
	var d accountDoc
	err := s.coll().FindOne(ctx, bson.D{eq("_id", id), eq("user_id", userID)}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return accountDoc{}, ErrNotFound
	}
	return d, err
}

func (s *Service) get(ctx context.Context, userID, id int64) (Account, error) {
	d, err := s.doc(ctx, userID, id)
	return d.account(), err
}

func (s *Service) Get(ctx context.Context, userID, id int64) (Account, error) {
	return s.get(ctx, userID, id)
}

func (s *Service) List(ctx context.Context, userID int64) ([]Account, error) {
	cur, err := s.coll().Find(ctx, bson.D{eq("user_id", userID)}, options.Find().SetSort(bson.D{eq("_id", 1)}))
	if err != nil {
		return nil, err
	}
	var docs []accountDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.account())
	}
	return out, nil
}

// StartConnect begins a LinkedIn connection for the user and returns the account row
// (status pending) plus the link the user opens to log into LinkedIn.
//
// Only one attempt can be pending across all users (the one_pending_connection index).
// Stale attempts are expired first so a user who closed the tab does not block others.
func (s *Service) StartConnect(ctx context.Context, userID int64, timezone string) (Account, string, error) {
	if err := validateTimezone(timezone); err != nil {
		return Account{}, "", err
	}
	s.expireStalePending(ctx)

	existing, err := s.li.ListAccounts(ctx)
	if err != nil {
		return Account{}, "", err
	}
	known := make([]string, 0, len(existing))
	for _, a := range existing {
		known = append(known, a.ID)
	}

	id, err := mdb.NextID(ctx, s.db, mdb.Accounts)
	if err != nil {
		return Account{}, "", err
	}
	now := s.now()
	if _, err := s.coll().InsertOne(ctx, accountDoc{
		ID: id, UserID: userID, Status: StatusPending, KnownAccountIDs: known, CreatedAt: now,
		Schedule: defaultSchedule(timezone, now),
	}); err != nil {
		if mdb.IsDup(err) {
			return Account{}, "", ErrConnectionBusy
		}
		return Account{}, "", err
	}
	// The pending row now holds the global "one connection at a time" slot. If anything below
	// fails, remove it so the slot is not held for the full expiry time.
	undo := func() { s.coll().DeleteOne(context.WithoutCancel(ctx), bson.D{eq("_id", id)}) }

	sess, err := s.li.CreateConnectionSession(ctx)
	if err != nil {
		undo()
		return Account{}, "", err
	}
	if _, err := s.coll().UpdateOne(ctx, bson.D{eq("_id", id)},
		bson.D{{Key: "$set", Value: bson.D{eq("connection_session_id", sess.ID)}}}); err != nil {
		s.cancelQuietly(sess.ID)
		undo()
		return Account{}, "", err
	}
	a, err := s.get(ctx, userID, id)
	return a, sess.Link, err
}

func (s *Service) cancelQuietly(sessionID string) {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.li.CancelConnectionSession(c, sessionID); err != nil {
		s.log.Warn("cancel connection session", "err", err)
	}
}

// expireStalePending marks abandoned attempts as errors and cancels their sessions.
func (s *Service) expireStalePending(ctx context.Context) {
	cur, err := s.coll().Find(ctx, bson.D{eq("status", StatusPending), eq("created_at", bson.D{{Key: "$lt", Value: s.now().Add(-pendingTTL)}})})
	if err != nil {
		s.log.Warn("expire stale pending", "err", err)
		return
	}
	var stale []accountDoc
	if err := cur.All(ctx, &stale); err != nil {
		s.log.Warn("expire stale pending", "err", err)
		return
	}
	for _, d := range stale {
		// Only the caller that flips the status cancels the session, so two of them cannot both do it.
		r, err := s.coll().UpdateOne(ctx, bson.D{eq("_id", d.ID), eq("status", StatusPending)},
			bson.D{{Key: "$set", Value: bson.D{eq("status", StatusError), eq("status_detail", "connection timed out")}}})
		if err == nil && r.ModifiedCount == 1 && d.ConnectionSessionID != nil {
			s.cancelQuietly(*d.ConnectionSessionID)
		}
	}
}

// Refresh brings one account up to date with Linked API. For a pending account it
// checks the connection session and, once login succeeded, claims the new account.
func (s *Service) Refresh(ctx context.Context, userID, id int64) (Account, error) {
	a, err := s.get(ctx, userID, id)
	if err != nil {
		return Account{}, err
	}
	if a.Status == StatusPending {
		return s.refreshPending(ctx, userID, id)
	}
	if a.Status == StatusError {
		return a, nil // failed attempts are final; the user starts a new one
	}
	return s.refreshConnected(ctx, userID, id)
}

func (s *Service) refreshPending(ctx context.Context, userID, id int64) (Account, error) {
	var d accountDoc
	err := s.coll().FindOne(ctx, bson.D{eq("_id", id), eq("user_id", userID), eq("status", StatusPending)}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return s.get(ctx, userID, id) // someone else already finished it
	}
	if err != nil {
		return Account{}, err
	}
	if s.now().Sub(d.CreatedAt) > pendingTTL {
		s.expireStalePending(ctx)
		return s.get(ctx, userID, id)
	}
	sessionID := ""
	if d.ConnectionSessionID != nil {
		sessionID = *d.ConnectionSessionID
	}

	status, err := s.li.GetConnectionSession(ctx, sessionID)
	if err != nil {
		return Account{}, err
	}
	switch status {
	case linkedin.SessionExpired, linkedin.SessionError, linkedin.SessionCancelled:
		return s.fail(ctx, userID, id, "LinkedIn login "+status)
	case linkedin.SessionSuccess:
		return s.claimNewAccount(ctx, userID, id, d.KnownAccountIDs)
	default:
		return s.get(ctx, userID, id) // still waiting on the user
	}
}

func (s *Service) fail(ctx context.Context, userID, id int64, detail string) (Account, error) {
	if _, err := s.coll().UpdateOne(ctx, bson.D{eq("_id", id), eq("user_id", userID), eq("status", StatusPending)},
		bson.D{{Key: "$set", Value: bson.D{eq("status", StatusError), eq("status_detail", detail)}}}); err != nil {
		return Account{}, err
	}
	return s.get(ctx, userID, id)
}

// claimNewAccount finds the one Linked API account that did not exist when the session
// started and was not already claimed, and attaches it to this row. Zero candidates means
// Linked API is still creating it. More than one means we cannot tell which is ours, so
// we refuse rather than risk giving a user someone else's LinkedIn account.
func (s *Service) claimNewAccount(ctx context.Context, userID, id int64, known []string) (Account, error) {
	accs, err := s.li.ListAccounts(ctx)
	if err != nil {
		return Account{}, err
	}
	skip := map[string]bool{}
	for _, k := range known {
		skip[k] = true
	}
	cur, err := s.coll().Find(ctx, bson.D{eq("linkedin_account_id", bson.D{{Key: "$type", Value: "string"}})},
		options.Find().SetProjection(bson.D{eq("linkedin_account_id", 1)}))
	if err != nil {
		return Account{}, err
	}
	var claimed []accountDoc
	if err := cur.All(ctx, &claimed); err != nil {
		return Account{}, err
	}
	for _, c := range claimed {
		if c.LinkedInAccountID != nil {
			skip[*c.LinkedInAccountID] = true
		}
	}

	var candidates []linkedin.Account
	for _, a := range accs {
		if !skip[a.ID] {
			candidates = append(candidates, a)
		}
	}
	switch len(candidates) {
	case 0:
		return s.get(ctx, userID, id) // account still being created
	case 1:
	default:
		s.log.Error("ambiguous new linkedin account", "account_row", id, "candidates", len(candidates))
		if _, err := s.fail(ctx, userID, id, ErrAmbiguous.Error()); err != nil {
			return Account{}, err
		}
		return Account{}, ErrAmbiguous
	}

	c := candidates[0]
	enc, err := s.cipher.Encrypt(c.IdentificationToken, bindFor(id))
	if err != nil {
		return Account{}, err
	}
	_, err = s.coll().UpdateOne(ctx, bson.D{eq("_id", id), eq("user_id", userID), eq("status", StatusPending)}, bson.D{
		{Key: "$set", Value: bson.D{
			eq("linkedin_account_id", c.ID), eq("identification_token", enc), eq("name", c.Name), eq("profile_url", c.URL),
			eq("headline", c.Headline), eq("avatar_url", c.AvatarURL), eq("status", mapStatus(c.Status)),
			eq("status_detail", ""), eq("connected_at", connectedAt(c, s.now())),
		}},
		{Key: "$unset", Value: bson.D{eq("connection_session_id", "")}},
	})
	if err != nil {
		if mdb.IsDup(err) {
			return s.get(ctx, userID, id) // lost a race with a concurrent refresh
		}
		return Account{}, err
	}
	return s.get(ctx, userID, id)
}

func (s *Service) refreshConnected(ctx context.Context, userID, id int64) (Account, error) {
	d, err := s.doc(ctx, userID, id)
	if err != nil {
		return Account{}, err
	}
	laID := ""
	if d.LinkedInAccountID != nil {
		laID = *d.LinkedInAccountID
	}
	accs, err := s.li.ListAccounts(ctx)
	if err != nil {
		return Account{}, err
	}
	for _, a := range accs {
		if a.ID != laID {
			continue
		}
		enc, err := s.cipher.Encrypt(a.IdentificationToken, bindFor(id)) // token may have been regenerated
		if err != nil {
			return Account{}, err
		}
		set := bson.D{
			eq("name", a.Name), eq("profile_url", a.URL), eq("headline", a.Headline), eq("avatar_url", a.AvatarURL),
			eq("status", mapStatus(a.Status)), eq("status_detail", ""), eq("identification_token", enc),
		}
		update := bson.D{{Key: "$set", Value: set}}
		if a.ReconnectionLink != "" {
			update[0].Value = append(set, eq("reconnection_link", a.ReconnectionLink))
		} else {
			update = append(update, bson.E{Key: "$unset", Value: bson.D{eq("reconnection_link", "")}})
		}
		if _, err := s.coll().UpdateOne(ctx, bson.D{eq("_id", id), eq("user_id", userID)}, update); err != nil {
			return Account{}, err
		}
		return s.get(ctx, userID, id)
	}
	// Gone from Linked API: disconnected there.
	if _, err := s.coll().UpdateOne(ctx, bson.D{eq("_id", id), eq("user_id", userID)},
		bson.D{{Key: "$set", Value: bson.D{eq("status", StatusDisconnected), eq("status_detail", "removed in Linked API")}}}); err != nil {
		return Account{}, err
	}
	return s.get(ctx, userID, id)
}

// Reconnect starts a reconnection session for an account that needs one and returns its link.
func (s *Service) Reconnect(ctx context.Context, userID, id int64) (string, error) {
	d, err := s.doc(ctx, userID, id)
	if err != nil {
		return "", err
	}
	if d.Status != StatusReconnectionRequired || d.LinkedInAccountID == nil {
		return "", ErrNotReconnectable
	}
	sess, err := s.li.CreateReconnectionSession(ctx, *d.LinkedInAccountID)
	if err != nil {
		return "", err
	}
	if _, err := s.coll().UpdateOne(ctx, bson.D{eq("_id", id), eq("user_id", userID)},
		bson.D{{Key: "$set", Value: bson.D{eq("reconnection_link", sess.Link)}}}); err != nil {
		return "", err
	}
	return sess.Link, nil
}

// IdentificationToken returns the decrypted token for the worker. It is intentionally not
// scoped by user (the worker acts for accounts, not browsers) and must never be exposed
// through an HTTP handler.
func (s *Service) IdentificationToken(ctx context.Context, accountID int64) (string, error) {
	var d accountDoc
	err := s.coll().FindOne(ctx, bson.D{eq("_id", accountID)}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) || (err == nil && d.IdentificationToken == nil) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return s.cipher.Decrypt(*d.IdentificationToken, bindFor(accountID))
}

func bindFor(rowID int64) string { return fmt.Sprintf("linkedin_accounts:%d", rowID) }

func mapStatus(linkedAPIStatus string) string {
	switch linkedAPIStatus {
	case linkedin.AccountActive:
		return StatusConnected
	case linkedin.AccountFrozen:
		return StatusFrozen
	case linkedin.AccountReconnectionRequired:
		return StatusReconnectionRequired
	}
	return StatusError
}

func connectedAt(a linkedin.Account, fallback time.Time) time.Time {
	if a.ConnectedAt.IsZero() {
		return fallback
	}
	return a.ConnectedAt
}

func validateTimezone(tz string) error {
	if _, err := time.LoadLocation(tz); err != nil || tz == "" {
		return fmt.Errorf("%w: unknown timezone %q", ErrInvalidSchedule, tz)
	}
	return nil
}
