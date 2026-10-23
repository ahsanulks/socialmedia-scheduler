package app

import (
	"context"

	"github.com/ahsanulks/social-scheduler/internal/scheduling/domain"
)

// CancelSchedule soft-deletes a schedule.
type CancelSchedule struct {
	ID              string
	ExpectedVersion *int
}

type CancelScheduleHandler struct {
	posts domain.ScheduledPostRepository
	clock Clock
}

func NewCancelScheduleHandler(posts domain.ScheduledPostRepository, clock Clock) *CancelScheduleHandler {
	return &CancelScheduleHandler{posts: posts, clock: clock}
}

func (h *CancelScheduleHandler) Handle(ctx context.Context, cmd CancelSchedule) (Result, error) {
	post, err := loadPost(ctx, h.posts, cmd.ID)
	if err != nil {
		return Result{}, err
	}
	var expected *domain.Version
	if cmd.ExpectedVersion != nil {
		v, err := domain.NewVersion(*cmd.ExpectedVersion)
		if err != nil {
			return Result{}, domain.ValidationError{Field: "expected_version", Message: "must be at least 1"}
		}
		expected = &v
	}
	if err := post.Cancel(h.clock.Now(), expected); err != nil {
		return Result{}, err
	}
	if err := h.posts.Save(ctx, post, post.PullEvents()); err != nil {
		return Result{}, err
	}
	return Result{ID: post.ID().String(), Version: post.Version().Int()}, nil
}
