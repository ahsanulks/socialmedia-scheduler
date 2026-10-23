package domain

import (
	"context"
	"time"
)

// AttemptOutcome describes how one publish try ended.
type AttemptOutcome string

const (
	OutcomeStarted   AttemptOutcome = "started"
	OutcomeSucceeded AttemptOutcome = "succeeded"
	OutcomeRetryable AttemptOutcome = "retryable"
	OutcomePermanent AttemptOutcome = "permanent"
)

// AttemptRecord is one row of the publish attempt log.
type AttemptRecord struct {
	ScheduleID string
	Platform   string
	AttemptNo  int
	StartedAt  time.Time
	PrepareRef *string
}

// ScheduledPostRepository is the write-side port. It is implemented by the
// SQLite adapter, which persists state + events + projection in one transaction.
type ScheduledPostRepository interface {
	Get(ctx context.Context, id PostID) (*ScheduledPost, error)
	Save(ctx context.Context, post *ScheduledPost, events []Event) error

	// FindDueIDs lists schedules that are due (or whose retry time has passed).
	FindDueIDs(ctx context.Context, now time.Time, limit int) ([]PostID, error)

	// FindExpiredLeaseIDs lists publishes whose lease ran out (worker crash).
	FindExpiredLeaseIDs(ctx context.Context, now time.Time, limit int) ([]PostID, error)

	// CountPublishedSince supports the per-platform rate budget.
	CountPublishedSince(ctx context.Context, platform Platform, since time.Time) (int, error)

	// StartAttempt appends a publish_attempts row and returns its id.
	StartAttempt(ctx context.Context, rec AttemptRecord) (int64, error)

	// FinishAttempt records the outcome of a previously started attempt.
	FinishAttempt(ctx context.Context, attemptID int64, outcome AttemptOutcome, errCode, errMsg string, finishedAt time.Time) error
}
