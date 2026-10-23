package domain

import "time"

// Event is a fact that happened to an aggregate. Events are appended to
// domain_events in the same transaction as the state change.
type Event interface {
	EventType() string
	AggregateID() PostID
	Version() Version
	OccurredAt() time.Time
}

type PostScheduled struct {
	postID      PostID
	version     Version
	occurredAt  time.Time
	Content     string
	ScheduledAt time.Time
	Platform    string
}

func (e PostScheduled) EventType() string     { return "PostScheduled" }
func (e PostScheduled) AggregateID() PostID   { return e.postID }
func (e PostScheduled) Version() Version      { return e.version }
func (e PostScheduled) OccurredAt() time.Time { return e.occurredAt }

type PostContentUpdated struct {
	postID     PostID
	version    Version
	occurredAt time.Time
	Content    string
}

func (e PostContentUpdated) EventType() string     { return "PostContentUpdated" }
func (e PostContentUpdated) AggregateID() PostID   { return e.postID }
func (e PostContentUpdated) Version() Version      { return e.version }
func (e PostContentUpdated) OccurredAt() time.Time { return e.occurredAt }

type PostRescheduled struct {
	postID      PostID
	version     Version
	occurredAt  time.Time
	ScheduledAt time.Time
	Attempts    int
}

func (e PostRescheduled) EventType() string     { return "PostRescheduled" }
func (e PostRescheduled) AggregateID() PostID   { return e.postID }
func (e PostRescheduled) Version() Version      { return e.version }
func (e PostRescheduled) OccurredAt() time.Time { return e.occurredAt }

type PostCanceled struct {
	postID     PostID
	version    Version
	occurredAt time.Time
	CanceledAt time.Time
}

func (e PostCanceled) EventType() string     { return "PostCanceled" }
func (e PostCanceled) AggregateID() PostID   { return e.postID }
func (e PostCanceled) Version() Version      { return e.version }
func (e PostCanceled) OccurredAt() time.Time { return e.occurredAt }

type PostPublishStarted struct {
	postID     PostID
	version    Version
	occurredAt time.Time
	Attempts   int
	LeaseUntil time.Time
}

func (e PostPublishStarted) EventType() string     { return "PostPublishStarted" }
func (e PostPublishStarted) AggregateID() PostID   { return e.postID }
func (e PostPublishStarted) Version() Version      { return e.version }
func (e PostPublishStarted) OccurredAt() time.Time { return e.occurredAt }

type PostPrepared struct {
	postID     PostID
	version    Version
	occurredAt time.Time
	PrepareRef string
}

func (e PostPrepared) EventType() string     { return "PostPrepared" }
func (e PostPrepared) AggregateID() PostID   { return e.postID }
func (e PostPrepared) Version() Version      { return e.version }
func (e PostPrepared) OccurredAt() time.Time { return e.occurredAt }

type PostPublished struct {
	postID         PostID
	version        Version
	occurredAt     time.Time
	ExternalPostID string
	PublishedAt    time.Time
}

func (e PostPublished) EventType() string     { return "PostPublished" }
func (e PostPublished) AggregateID() PostID   { return e.postID }
func (e PostPublished) Version() Version      { return e.version }
func (e PostPublished) OccurredAt() time.Time { return e.occurredAt }

type PostPublishFailed struct {
	postID     PostID
	version    Version
	occurredAt time.Time
	Reason     string
	RetryAt    *time.Time
	FailedAt   *time.Time
}

func (e PostPublishFailed) EventType() string     { return "PostPublishFailed" }
func (e PostPublishFailed) AggregateID() PostID   { return e.postID }
func (e PostPublishFailed) Version() Version      { return e.version }
func (e PostPublishFailed) OccurredAt() time.Time { return e.occurredAt }

type PostLeaseRecovered struct {
	postID     PostID
	version    Version
	occurredAt time.Time
	Reason     string
}

func (e PostLeaseRecovered) EventType() string     { return "PostLeaseRecovered" }
func (e PostLeaseRecovered) AggregateID() PostID   { return e.postID }
func (e PostLeaseRecovered) Version() Version      { return e.version }
func (e PostLeaseRecovered) OccurredAt() time.Time { return e.occurredAt }
