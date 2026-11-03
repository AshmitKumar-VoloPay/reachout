package webhooks

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ashmit/reachout-server/internal/linkedin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Reconciler asks Linked API to deliver again the events it gave up on (after its own 8
// attempts, for example during an outage on our side). It talks only to the admin API,
// never to LinkedIn, so it is safe to run at any hour. Replays reuse the original event id,
// so the endpoint's de-duplication makes them harmless.
type Reconciler struct {
	pool *pgxpool.Pool
	li   linkedin.Client
	log  *slog.Logger
	now  func() time.Time

	MaxReplays  int           // times one delivery is replayed before we stop asking
	MinInterval time.Duration // gap between replays of the same delivery
	Retention   time.Duration // how long webhook_events rows are kept for de-duplication
}

func NewReconciler(pool *pgxpool.Pool, li linkedin.Client, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{pool: pool, li: li, log: log, now: time.Now,
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
		var processed *time.Time
		err := r.pool.QueryRow(ctx, `SELECT processed_at FROM webhook_events WHERE event_id = $1`, d.EventID).Scan(&processed)
		if err == nil && processed != nil {
			continue
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return replayed, err
		}

		var count int
		var last time.Time
		err = r.pool.QueryRow(ctx, `SELECT replays, last_replayed_at FROM webhook_replays WHERE delivery_id = $1`, d.ID).Scan(&count, &last)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return replayed, err
		}
		if count >= r.MaxReplays || (count > 0 && now.Sub(last) < r.MinInterval) {
			continue
		}
		if err := r.li.ReplayWebhookDelivery(ctx, d.ID); err != nil {
			r.log.Warn("replay delivery", "delivery", d.ID, "err", err)
			continue
		}
		if _, err := r.pool.Exec(ctx,
			`INSERT INTO webhook_replays (delivery_id, replays, last_replayed_at) VALUES ($1, 1, $2)
			 ON CONFLICT (delivery_id) DO UPDATE SET replays = webhook_replays.replays + 1, last_replayed_at = $2`,
			d.ID, now); err != nil {
			return replayed, err
		}
		replayed++
	}
	return replayed, nil
}

// prune forgets old de-duplication rows. Linked API stops retrying within hours, so two
// weeks is far more than needed.
func (r *Reconciler) prune(ctx context.Context, now time.Time) {
	if _, err := r.pool.Exec(ctx, `DELETE FROM webhook_events WHERE received_at < $1`, now.Add(-r.Retention)); err != nil {
		r.log.Warn("prune webhook events", "err", err)
	}
	if _, err := r.pool.Exec(ctx, `DELETE FROM webhook_replays WHERE last_replayed_at < $1`, now.Add(-2*r.Retention)); err != nil {
		r.log.Warn("prune webhook replays", "err", err)
	}
}
