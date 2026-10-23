package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

// RecoverExpiredLeasesHandler requeues publishes whose worker died mid-flight.
type RecoverExpiredLeasesHandler struct {
	posts domain.ScheduledPostRepository
	clock Clock
	log   *slog.Logger
	limit int
}

func NewRecoverExpiredLeasesHandler(posts domain.ScheduledPostRepository, clock Clock, log *slog.Logger, limit int) *RecoverExpiredLeasesHandler {
	if limit <= 0 {
		limit = 100
	}
	return &RecoverExpiredLeasesHandler{posts: posts, clock: clock, log: log, limit: limit}
}

func (h *RecoverExpiredLeasesHandler) Handle(ctx context.Context) (int, error) {
	ids, err := h.posts.FindExpiredLeaseIDs(ctx, h.clock.Now(), h.limit)
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, id := range ids {
		post, err := h.posts.Get(ctx, id)
		if err != nil {
			h.log.Warn("loading post for lease recovery failed", "id", id.String(), "error", err)
			continue
		}
		if err := post.RecoverExpiredLease(h.clock.Now()); err != nil {
			continue
		}
		if err := h.posts.Save(ctx, post, post.PullEvents()); err != nil {
			var vc domain.VersionConflictError
			if errors.As(err, &vc) {
				continue
			}
			return recovered, err
		}
		recovered++
		h.log.Warn("recovered expired publish lease", "id", id.String())
	}
	return recovered, nil
}
