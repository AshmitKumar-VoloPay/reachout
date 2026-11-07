package webhooks

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/ashmit/reachout-server/internal/mdb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Reconciler asks Linked API to deliver again the events it gave up on (after its own 8
// attempts, for example during an outage on our side). It talks only to the admin API,
// never to LinkedIn, so it is safe to run at any hour. Replays reuse the original event id,
// so the endpoint's de-duplication makes them harmless.
type Reconciler struct {
	db  *mongo.Database
	li  linkedin.Client
	log *slog.Logger
	now func() time.Time

	MaxReplays  int           // times one delivery is replayed before we stop asking
	MinInterval time.Duration // gap between replays of the same delivery
	Retention   time.Duration // how long webhook_events rows are kept for de-duplication
}

func NewReconciler(db *mongo.Database, li linkedin.Client, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{db: db, li: li, log: log, now: time.Now,
		MaxReplays: 3, MinInterval: 10 * time.Minute, Retention: 14 * 24 * time.Hour}
}

// Run replays on a fixed interval until ctx ends.
func (r *Reconciler) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := r.Once(ctx); err != nil {
			r.log.Warn("webhook reconcile", "err", err)
		} else if n > 0 {
			r.log.Info("webhook deliveries replayed", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once replays failed deliveries we have not handled and returns how many it asked for.
func (r *Reconciler) Once(ctx context.Context) (int, error) {
	now := r.now()
	defer r.prune(ctx, now)

	ds, err := r.li.ListWebhookDeliveries(ctx)
	if err != nil {
		return 0, err
	}
	replayed := 0
	for _, d := range ds {
		if d.Status != "failed" || d.ID == "" {
			continue
		}
		// Already handled here (a later delivery got through): nothing to ask for.
		var seen struct {
			ProcessedAt *time.Time `bson:"processed_at"`
		}
		err := r.db.Collection(mdb.WebhookEvents).FindOne(ctx, bson.D{eq("_id", d.EventID)}).Decode(&seen)
		if err == nil && seen.ProcessedAt != nil {
			continue
		}
		if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
			return replayed, err
		}

		var count int
		var last time.Time
		var prior struct {
			Replays        int       `bson:"replays"`
			LastReplayedAt time.Time `bson:"last_replayed_at"`
		}
		err = r.db.Collection(mdb.WebhookReplays).FindOne(ctx, bson.D{eq("_id", d.ID)}).Decode(&prior)
		if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
			return replayed, err
		}
		count, last = prior.Replays, prior.LastReplayedAt
		if count >= r.MaxReplays || (count > 0 && now.Sub(last) < r.MinInterval) {
			continue
		}
		if err := r.li.ReplayWebhookDelivery(ctx, d.ID); err != nil {
			r.log.Warn("replay delivery", "delivery", d.ID, "err", err)
			continue
		}
		if _, err := r.db.Collection(mdb.WebhookReplays).UpdateOne(ctx, bson.D{eq("_id", d.ID)}, bson.D{
			{Key: "$inc", Value: bson.D{eq("replays", 1)}},
			{Key: "$set", Value: bson.D{eq("last_replayed_at", now)}},
		}, options.UpdateOne().SetUpsert(true)); err != nil {
			return replayed, err
		}
		replayed++
	}
	return replayed, nil
}

// prune forgets old de-duplication rows. Linked API stops retrying within hours, so two
// weeks is far more than needed.
func (r *Reconciler) prune(ctx context.Context, now time.Time) {
	if _, err := r.db.Collection(mdb.WebhookEvents).DeleteMany(ctx, bson.D{eq("received_at", bson.D{{Key: "$lt", Value: now.Add(-r.Retention)}})}); err != nil {
		r.log.Warn("prune webhook events", "err", err)
	}
	if _, err := r.db.Collection(mdb.WebhookReplays).DeleteMany(ctx, bson.D{eq("last_replayed_at", bson.D{{Key: "$lt", Value: now.Add(-2 * r.Retention)}})}); err != nil {
		r.log.Warn("prune webhook replays", "err", err)
	}
}
