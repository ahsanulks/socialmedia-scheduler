package app

import (
	"context"
	"errors"
	"time"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

// UpdateSchedule edits content and/or moves the publish time.
type UpdateSchedule struct {
	ID              string
	Content         *string
	ScheduledAt     *time.Time
	ExpectedVersion int
}

type UpdateScheduleHandler struct {
	posts    domain.ScheduledPostRepository
	registry *Registry
	clock    Clock
}

func NewUpdateScheduleHandler(posts domain.ScheduledPostRepository, registry *Registry, clock Clock) *UpdateScheduleHandler {
	return &UpdateScheduleHandler{posts: posts, registry: registry, clock: clock}
}

func (h *UpdateScheduleHandler) Handle(ctx context.Context, cmd UpdateSchedule) (Result, error) {
	post, err := loadPost(ctx, h.posts, cmd.ID)
	if err != nil {
		return Result{}, err
	}
	if cmd.Content == nil && cmd.ScheduledAt == nil {
		return Result{}, domain.ValidationError{Field: "body", Message: "at least one of content or scheduled_at is required"}
	}
	expected, err := domain.NewVersion(cmd.ExpectedVersion)
	if err != nil {
		return Result{}, domain.ValidationError{Field: "expected_version", Message: "must be at least 1"}
	}
	if expected.Int() != post.Version().Int() {
		return Result{}, domain.VersionConflictError{Expected: expected.Int(), Actual: post.Version().Int()}
	}
	if _, isFailed := post.State().(domain.FailedState); isFailed && cmd.ScheduledAt == nil {
		return Result{}, domain.InvalidTransitionError{
			Status: post.Status(),
			Action: "edit",
			Detail: "a failed schedule must be given a new scheduled_at",
		}
	}

	now := h.clock.Now()
	if cmd.Content != nil {
		content, err := domain.NewPostContent(*cmd.Content)
		if err != nil {
			return Result{}, err
		}
		if caps, ok := h.registry.Capabilities(post.Platform()); ok {
			if err := checkCapabilities(post.Platform(), caps, content, post.Media()); err != nil {
				return Result{}, err
			}
		}
		// Re-read the version after each mutation: two changes in one request
		// advance it twice.
		if err := post.UpdateContent(content, now, post.Version()); err != nil {
			return Result{}, err
		}
	}
	if cmd.ScheduledAt != nil {
		if err := post.Reschedule(domain.NewScheduleTime(*cmd.ScheduledAt), now, post.Version()); err != nil {
			return Result{}, err
		}
	}
	if err := h.posts.Save(ctx, post, post.PullEvents()); err != nil {
		return Result{}, err
	}
	return Result{ID: post.ID().String(), Version: post.Version().Int()}, nil
}

// loadPost parses an id and loads the aggregate. A malformed id is a 404, not a
// 422: the resource simply does not exist.
func loadPost(ctx context.Context, posts domain.ScheduledPostRepository, rawID string) (*domain.ScheduledPost, error) {
	id, err := domain.NewPostID(rawID)
	if err != nil {
		return nil, domain.NotFoundError{ID: rawID}
	}
	post, err := posts.Get(ctx, id)
	if err != nil {
		var nf domain.NotFoundError
		if errors.As(err, &nf) {
			return nil, nf
		}
		return nil, err
	}
	return post, nil
}
