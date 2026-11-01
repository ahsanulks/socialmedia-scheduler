// Package scheduler runs the publish loop inside the API process.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/app"
	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

// Worker claims due schedules and publishes them. The flow is identical for
// every platform; the adapter is resolved from the registry.
type Worker struct {
	posts     domain.ScheduledPostRepository
	registry  *app.Registry
	recover   *app.RecoverExpiredLeasesHandler
	publish   *app.PublishPostHandler
	clock     app.Clock
	prune     app.IdempotencyPruner
	retention time.Duration
	batch     int
	tick      time.Duration
	log       *slog.Logger
	lastPrune time.Time
}

func New(
	posts domain.ScheduledPostRepository,
	registry *app.Registry,
	recover *app.RecoverExpiredLeasesHandler,
	publish *app.PublishPostHandler,
	clock app.Clock,
	prune app.IdempotencyPruner,
	retention time.Duration,
	tick time.Duration,
	batch int,
	log *slog.Logger,
) *Worker {
	if tick <= 0 {
		tick = 5 * time.Second
	}
	if batch <= 0 {
		batch = 10
	}
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		posts:     posts,
		registry:  registry,
		recover:   recover,
		publish:   publish,
		clock:     clock,
		prune:     prune,
		retention: retention,
		batch:     batch,
		tick:      tick,
		log:       log,
	}
}

// Run loops until the context is cancelled.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()
	w.log.Info("worker started", "tick", w.tick.String(), "batch", w.batch)
	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker stopped")
			return
		case <-ticker.C:
			if err := w.Tick(ctx); err != nil {
				w.log.Error("worker tick failed", "error", err)
			}
		}
	}
}

// Tick runs one pass: recover expired leases, prune stale idempotency
// rows at most hourly, then publish what is due.
func (w *Worker) Tick(ctx context.Context) error {
	if _, err := w.recover.Handle(ctx); err != nil {
		w.log.Error("lease recovery failed", "error", err)
	}
	if w.prune != nil && w.retention > 0 && time.Since(w.lastPrune) >= time.Hour {
		n, err := w.prune.Prune(ctx, w.clock.Now().Add(-w.retention))
		if err != nil {
			w.log.Error("idempotency prune failed", "error", err)
		} else if n > 0 {
			w.log.Info("pruned idempotency rows", "count", n, "retention", w.retention.String())
		}
		w.lastPrune = w.clock.Now()
	}

	ids, err := w.posts.FindDueIDs(ctx, w.clock.Now(), w.batch)
	if err != nil {
		return err
	}

	remaining := map[string]int{}
	known := map[string]bool{}

	for _, id := range ids {
		post, err := w.posts.Get(ctx, id)
		if err != nil {
			w.log.Warn("loading due schedule failed", "id", id.String(), "error", err)
			continue
		}
		platform := post.Platform().String()
		if !known[platform] {
			known[platform] = true
			remaining[platform] = -1 // -1 means "no limit configured"
			if caps, ok := w.registry.Capabilities(post.Platform()); ok && caps.DailyLimit > 0 {
				published, err := w.posts.CountPublishedSince(ctx, post.Platform(), w.clock.Now().Add(-24*time.Hour))
				if err != nil {
					w.log.Error("counting published posts failed", "platform", platform, "error", err)
				} else {
					remaining[platform] = caps.DailyLimit - published
					if remaining[platform] < 0 {
						remaining[platform] = 0
					}
				}
			}
		}
		if remaining[platform] == 0 {
			w.log.Warn("daily limit reached, skipping platform", "platform", platform)
			continue
		}
		if err := w.publish.Handle(ctx, id); err != nil {
			w.log.Error("publish failed", "id", id.String(), "error", err)
			continue
		}
		if remaining[platform] > 0 {
			remaining[platform]--
		}
	}
	return nil
}
