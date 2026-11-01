package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

// PublishPostHandler runs the publish flow for one claimed post. It is identical
// for every platform: only the adapter resolved from the registry differs.
type PublishPostHandler struct {
	posts       domain.ScheduledPostRepository
	registry    *Registry
	clock       Clock
	log         *slog.Logger
	lease       time.Duration
	maxAttempts int
}

func NewPublishPostHandler(posts domain.ScheduledPostRepository, registry *Registry, clock Clock, log *slog.Logger, lease time.Duration, maxAttempts int) *PublishPostHandler {
	if lease <= 0 {
		lease = domain.DefaultLease
	}
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	return &PublishPostHandler{posts: posts, registry: registry, clock: clock, log: log, lease: lease, maxAttempts: maxAttempts}
}

func (h *PublishPostHandler) Handle(ctx context.Context, id domain.PostID) error {
	post, err := h.posts.Get(ctx, id)
	if err != nil {
		var nf domain.NotFoundError
		if errors.As(err, &nf) {
			return nil
		}
		return err
	}
	if _, ok := post.State().(domain.ScheduledState); !ok {
		return nil // already taken by someone else
	}

	if err := post.StartPublishing(h.clock.Now(), h.lease); err != nil {
		return err
	}
	if err := h.posts.Save(ctx, post, post.PullEvents()); err != nil {
		var vc domain.VersionConflictError
		if errors.As(err, &vc) {
			// Another writer changed the row between load and claim; leave it for
			// the next tick rather than publishing something we no longer own.
			return nil
		}
		return err
	}

	attemptID := int64(0)
	if id, err := h.posts.StartAttempt(ctx, domain.AttemptRecord{
		ScheduleID: post.ID().String(),
		Platform:   post.Platform().String(),
		AttemptNo:  post.Attempts().Int(),
		StartedAt:  h.clock.Now(),
		PrepareRef: prepareRefString(post.PrepareRef()),
	}); err != nil {
		// The attempt log is diagnostic: never fail a publish because of it.
		h.log.Warn("recording publish attempt failed", "id", post.ID().String(), "error", err)
	} else {
		attemptID = id
	}

	publisher, ok := h.registry.Resolve(post.Platform())
	if !ok {
		return h.fail(ctx, post, attemptID, Permanent(
			fmt.Errorf("no adapter registered for platform %q", post.Platform().String()),
			fmt.Sprintf("platform %q is not configured", post.Platform().String()),
		))
	}

	pub := Publication{
		Platform:  post.Platform(),
		Content:   post.Content().String(),
		MediaKind: postMediaKind(post),
	}
	if pub.MediaKind != "" {
		pub.MediaURL = post.Media().URL()
	}
	ref := post.PrepareRef()

	if ref == nil {
		newRef, err := publisher.Prepare(ctx, pub)
		if err != nil {
			return h.fail(ctx, post, attemptID, err)
		}
		if newRef != nil {
			if err := post.AttachPrepareRef(*newRef, h.clock.Now()); err != nil {
				return err
			}
			// Persist the draft reference before the irreversible call, so a
			// retry reuses this draft instead of creating a second one.
			if err := h.posts.Save(ctx, post, post.PullEvents()); err != nil {
				h.log.Warn("persisting prepare reference failed", "id", post.ID().String(), "error", err)
			}
			ref = post.PrepareRef()
		}
	}

	// Defense against the lease-recovery race: a slow Prepare (Instagram's
	// bounded container poll is the longest flow) can outlive the lease, and
	// lease recovery would requeue the post while we are still mid-attempt.
	// Never make the irreversible call without holding the lease: the
	// RecoverExpiredLeases handler requeues the row and the next attempt runs
	// a fresh publish, so a publish here could duplicate the post.
	if ps, ok := post.State().(domain.PublishingState); ok && h.clock.Now().After(ps.LeaseUntil) {
		h.log.Warn("lease expired before the irreversible call; leaving the row for lease recovery",
			"id", post.ID().String(), "platform", post.Platform().String())
		h.finish(ctx, attemptID, domain.OutcomeRetryable, "lease_lost", "lease expired before publish; not published")
		return nil
	}

	externalID, err := publisher.Publish(ctx, pub, ref)
	if err != nil {
		return h.fail(ctx, post, attemptID, err)
	}
	if err := post.MarkPublished(externalID, h.clock.Now()); err != nil {
		return err
	}
	if err := h.posts.Save(ctx, post, post.PullEvents()); err != nil {
		return err
	}
	h.log.Info("published",
		"id", post.ID().String(),
		"platform", post.Platform().String(),
		"external_post_id", externalID.String(),
		"attempts", post.Attempts().Int(),
	)
	h.finish(ctx, attemptID, domain.OutcomeSucceeded, "", "")
	return nil
}

// postMediaKind adapts the optional domain media to the string-typed port; the
// empty string means text-only.
func postMediaKind(post *domain.ScheduledPost) string {
	if m := post.Media(); m != nil {
		return m.Kind()
	}
	return ""
}

// fail records a failed attempt: a retryable failure with attempts left goes
// back to scheduled with a backoff, anything else fails for good.
func (h *PublishPostHandler) fail(ctx context.Context, post *domain.ScheduledPost, attemptID int64, cause error) error {
	now := h.clock.Now()
	reason, err := domain.NewFailureReason(FailureReasonOf(cause))
	if err != nil {
		return err
	}
	outcome := domain.OutcomePermanent
	var retryAt *time.Time
	if IsRetryable(cause) && post.Attempts().Int() < h.maxAttempts {
		at := now.Add(Backoff(post.Attempts().Int()))
		retryAt = &at
		outcome = domain.OutcomeRetryable
	}
	if err := post.MarkFailure(reason, retryAt, now); err != nil {
		return err
	}
	if err := h.posts.Save(ctx, post, post.PullEvents()); err != nil {
		return err
	}
	fields := []any{"id", post.ID().String(), "platform", post.Platform().String(), "attempts", post.Attempts().Int(), "retryable", outcome == domain.OutcomeRetryable, "reason", reason.String()}
	if retryAt != nil {
		fields = append(fields, "next_attempt_at", retryAt.UTC().Format(time.RFC3339))
	}
	h.log.Warn("publish failed", fields...)
	h.finish(ctx, attemptID, outcome, "", reason.String())
	return nil
}

func (h *PublishPostHandler) finish(ctx context.Context, attemptID int64, outcome domain.AttemptOutcome, errCode, errMsg string) {
	if attemptID == 0 {
		return
	}
	if err := h.posts.FinishAttempt(ctx, attemptID, outcome, errCode, errMsg, h.clock.Now()); err != nil {
		h.log.Warn("updating publish attempt failed", "attempt_id", attemptID, "error", err)
	}
}

func prepareRefString(ref *domain.PrepareRef) *string {
	if ref == nil {
		return nil
	}
	s := ref.String()
	return &s
}
