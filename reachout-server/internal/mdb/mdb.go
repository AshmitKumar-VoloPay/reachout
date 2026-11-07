// Package mdb is the MongoDB layer shared by every service: connection, the indexes that
// enforce our guarantees, integer ID allocation and transactions.
//
// Ids stay integers (not ObjectIDs) so the HTTP API and the client are unchanged. They come
// from the counters collection; allocate them before starting a transaction, never inside
// one, or every transaction would queue on the same counter document.
package mdb

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Collection names.
const (
	Users          = "users"
	Sessions       = "sessions"
	Accounts       = "linkedin_accounts" // the account's sending schedule is embedded as `schedule`
	Leads          = "leads"
	LeadImports    = "lead_imports"
	Campaigns      = "campaigns"
	CampaignLeads  = "campaign_leads"
	Messages       = "messages"
	SendAttempts   = "send_attempts"
	WebhookEvents  = "webhook_events"
	WebhookReplays = "webhook_replays"
	Counters       = "counters"
)

// Open connects and checks the server answers. Transactions need a replica set (MongoDB
// Atlas always is one; locally use `mongod --replSet rs0` and run rs.initiate() once).
func Open(ctx context.Context, uri, name string) (*mongo.Database, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(10 * time.Second))
	if err != nil {
		return nil, fmt.Errorf("connect to mongodb: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		client.Disconnect(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("ping mongodb: %w", err)
	}
	return client.Database(name), nil
}

// NextID returns the next integer id for a collection.
func NextID(ctx context.Context, db *mongo.Database, collection string) (int64, error) {
	var doc struct {
		Seq int64 `bson:"seq"`
	}
	err := db.Collection(Counters).FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: collection}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "seq", Value: 1}}}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&doc)
	return doc.Seq, err
}

// NextIDs reserves n consecutive ids and returns the first. Unused ones are simply skipped.
func NextIDs(ctx context.Context, db *mongo.Database, collection string, n int) (int64, error) {
	var doc struct {
		Seq int64 `bson:"seq"`
	}
	err := db.Collection(Counters).FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: collection}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "seq", Value: int64(n)}}}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&doc)
	return doc.Seq - int64(n) + 1, err
}

// IsDup reports a unique-index violation.
func IsDup(err error) bool { return mongo.IsDuplicateKeyError(err) }

// WithTx runs fn in a transaction, retrying it on transient conflicts. Everything fn does
// with the ctx it is given is committed together or not at all.
func WithTx(ctx context.Context, db *mongo.Database, fn func(ctx context.Context) error) error {
	s, err := db.Client().StartSession()
	if err != nil {
		return err
	}
	defer s.EndSession(ctx)
	_, err = s.WithTransaction(ctx, func(ctx context.Context) (any, error) { return nil, fn(ctx) })
	return err
}

// Lock makes concurrent transactions touching the same document take turns: the second
// one hits a write conflict and is retried by WithTx after the first commits. It is the
// counterpart of SELECT ... FOR UPDATE.
func Lock(ctx context.Context, coll *mongo.Collection, filter any) error {
	_, err := coll.UpdateOne(ctx, filter, bson.D{{Key: "$inc", Value: bson.D{{Key: "lock", Value: 1}}}}, options.UpdateOne().SetUpsert(true))
	return err
}

type index struct {
	coll    string
	keys    bson.D
	name    string
	unique  bool
	partial bson.D
	ttl     *int32
}

func k(pairs ...any) bson.D {
	d := make(bson.D, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		d = append(d, bson.E{Key: pairs[i].(string), Value: pairs[i+1]})
	}
	return d
}

var isString = k("$type", "string")

// indexes are idempotent to create. Several of them are guarantees the code relies on:
//
//	one_pending_connection        only one LinkedIn connection in flight, across all users
//	one_running_import_per_acct   one search import at a time per account
//	one_open_attempt_per_account  one LinkedIn task at a time per account (no double sends)
//	leads_user_url                a person is one lead per user
//	messages_external             one stored copy of each LinkedIn message
var indexes = []index{
	{coll: Users, keys: k("email", 1), name: "users_email", unique: true},
	{coll: Sessions, keys: k("expires_at", 1), name: "sessions_ttl", ttl: new(int32)},

	{coll: Accounts, keys: k("user_id", 1), name: "accounts_user"},
	{coll: Accounts, keys: k("linkedin_account_id", 1), name: "accounts_linkedin_id", unique: true, partial: k("linkedin_account_id", isString)},
	{coll: Accounts, keys: k("status", 1), name: "one_pending_connection", unique: true, partial: k("status", "pending")},

	{coll: Leads, keys: k("user_id", 1, "profile_url", 1), name: "leads_user_url", unique: true},
	{coll: Leads, keys: k("user_id", 1, "_id", -1), name: "leads_user_id"},
	{coll: Leads, keys: k("user_id", 1, "linkedin_urn", 1), name: "leads_urn", partial: k("linkedin_urn", isString)},
	{coll: Leads, keys: k("user_id", 1, "hashed_url", 1), name: "leads_hashed", partial: k("hashed_url", isString)},

	{coll: LeadImports, keys: k("account_id", 1), name: "one_running_import_per_acct", unique: true, partial: k("status", "running")},
	{coll: LeadImports, keys: k("user_id", 1, "_id", -1), name: "lead_imports_user"},

	{coll: Campaigns, keys: k("user_id", 1, "_id", -1), name: "campaigns_user"},
	{coll: Campaigns, keys: k("account_id", 1), name: "campaigns_account"},

	{coll: CampaignLeads, keys: k("campaign_id", 1, "lead_id", 1), name: "campaign_leads_pair", unique: true},
	{coll: CampaignLeads, keys: k("lead_id", 1), name: "campaign_leads_lead"},
	{coll: CampaignLeads, keys: k("campaign_id", 1, "state", 1, "next_action_at", 1), name: "campaign_leads_due"},

	{coll: Messages, keys: k("lead_id", 1, "external_id", 1), name: "messages_external", unique: true, partial: k("external_id", isString)},
	{coll: Messages, keys: k("lead_id", 1, "at", 1), name: "messages_lead_at"},
	{coll: Messages, keys: k("user_id", 1, "lead_id", 1), name: "messages_user_lead"},

	{coll: SendAttempts, keys: k("account_id", 1), name: "one_open_attempt_per_account", unique: true, partial: k("open", true)},
	{coll: SendAttempts, keys: k("account_id", 1, "created_at", 1), name: "send_attempts_account_day"},
	{coll: SendAttempts, keys: k("campaign_id", 1, "kind", 1, "created_at", 1), name: "send_attempts_campaign_day"},

	{coll: WebhookEvents, keys: k("received_at", 1), name: "webhook_events_received"},
}

// EnsureIndexes creates every collection and index. It runs at startup (it replaces the SQL
// migrations) and is safe to repeat. Collections are created here because a transaction
// cannot create one implicitly.
func EnsureIndexes(ctx context.Context, db *mongo.Database) error {
	for _, c := range []string{Users, Sessions, Accounts, Leads, LeadImports, Campaigns, CampaignLeads, Messages,
		SendAttempts, WebhookEvents, WebhookReplays, Counters} {
		if err := db.CreateCollection(ctx, c); err != nil && !isExists(err) {
			return fmt.Errorf("create collection %s: %w", c, err)
		}
	}
	for _, ix := range indexes {
		opts := options.Index().SetName(ix.name)
		if ix.unique {
			opts.SetUnique(true)
		}
		if ix.partial != nil {
			opts.SetPartialFilterExpression(ix.partial)
		}
		if ix.ttl != nil {
			opts.SetExpireAfterSeconds(*ix.ttl)
		}
		if _, err := db.Collection(ix.coll).Indexes().CreateOne(ctx, mongo.IndexModel{Keys: ix.keys, Options: opts}); err != nil {
			return fmt.Errorf("create index %s.%s: %w", ix.coll, ix.name, err)
		}
	}
	return nil
}

func isExists(err error) bool {
	var ce mongo.CommandError
	if asCommandError(err, &ce) {
		return ce.Code == 48 // NamespaceExists
	}
	return false
}

func asCommandError(err error, target *mongo.CommandError) bool {
	for err != nil {
		if ce, ok := err.(mongo.CommandError); ok {
			*target = ce
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Pinger adapts a database to the health check in httpapi.
type Pinger struct{ DB *mongo.Database }

func (p Pinger) Ping(ctx context.Context) error { return p.DB.Client().Ping(ctx, nil) }
