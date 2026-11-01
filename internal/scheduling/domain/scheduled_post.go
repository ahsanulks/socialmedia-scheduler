package domain

import "time"

// DefaultLease is how long a publish may hold a row before the worker treats it
// as crashed and requeues it.
const DefaultLease = 2 * time.Minute

// ScheduledPost is the aggregate root. Every field is unexported: the only ways
// in are NewScheduledPost (creation rules) and Rehydrate (structural rules), and
// the only ways out are the accessors, Snapshot and PullEvents.
type ScheduledPost struct {
	id          PostID
	content     PostContent
	media       *Media
	scheduledAt ScheduleTime
	platform    Platform
	prepareRef  *PrepareRef
	attempts    Attempt
	state       State
	version     Version
	baseVersion Version
	createdAt   time.Time
	updatedAt   time.Time
	events      []Event
}

// NewScheduledPost creates a scheduled post. Creation rules only: the publish
// time must be in the future. media is nil for text-only platforms.
func NewScheduledPost(id PostID, content PostContent, media *Media, at ScheduleTime, platform Platform, now time.Time) (*ScheduledPost, error) {
	now = now.UTC().Truncate(time.Millisecond)
	if !at.IsAfter(now) {
		return nil, invalid("scheduled_at", "must be in the future")
	}
	p := &ScheduledPost{
		id:          id,
		content:     content,
		media:       media,
		scheduledAt: at,
		platform:    platform,
		attempts:    NoAttempts(),
		state:       ScheduledState{},
		version:     InitialVersion,
		createdAt:   now,
		updatedAt:   now,
	}
	event := PostScheduled{
		postID:      p.id,
		version:     p.version,
		occurredAt:  now,
		Content:     content.String(),
		ScheduledAt: at.Time(),
		Platform:    platform.String(),
	}
	if media != nil {
		url := media.URL()
		event.MediaURL = &url
		event.MediaKind = media.Kind()
	}
	p.record(event)
	return p, nil
}

// Accessors.

func (p *ScheduledPost) ID() PostID                { return p.id }
func (p *ScheduledPost) Content() PostContent      { return p.content }
func (p *ScheduledPost) ScheduledAt() ScheduleTime { return p.scheduledAt }
func (p *ScheduledPost) Platform() Platform        { return p.platform }
func (p *ScheduledPost) Attempts() Attempt         { return p.attempts }
func (p *ScheduledPost) Version() Version          { return p.version }
func (p *ScheduledPost) CreatedAt() time.Time      { return p.createdAt }
func (p *ScheduledPost) UpdatedAt() time.Time      { return p.updatedAt }
func (p *ScheduledPost) Status() Status            { return p.state.Status() }
func (p *ScheduledPost) State() State              { return p.state }

// Media returns the attachment, nil for text-only posts. Media has no mutator:
// it is part of what was scheduled and cannot change afterward.
func (p *ScheduledPost) Media() *Media { return p.media }

// PrepareRef returns the platform-side draft reference, nil when nothing has
// been prepared yet.
func (p *ScheduledPost) PrepareRef() *PrepareRef { return p.prepareRef }

// BaseVersion is the version the aggregate was loaded with; the persistence
// adapter uses it for the optimistic lock. Zero means "not persisted yet".
func (p *ScheduledPost) BaseVersion() Version { return p.baseVersion }

// PullEvents drains the uncommitted events. Call it once, at the end of a
// handler, and hand the slice to the repository.
func (p *ScheduledPost) PullEvents() []Event {
	out := p.events
	p.events = nil
	return out
}

// MarkPersisted tells the aggregate that its current state is in storage. The
// repository calls it after a successful commit: a handler may save twice (for
// example persist the prepare reference, then publish the post) and the second
// save must still pass the optimistic lock.
func (p *ScheduledPost) MarkPersisted() {
	p.baseVersion = p.version
}

// mutate is the only place version and updatedAt change.
func (p *ScheduledPost) mutate(now time.Time) {
	p.version = p.version.Next()
	p.updatedAt = now.UTC().Truncate(time.Millisecond)
}

func (p *ScheduledPost) record(e Event) { p.events = append(p.events, e) }

// Behaviour.

// UpdateContent edits the copy of a scheduled post.
func (p *ScheduledPost) UpdateContent(c PostContent, now time.Time, expected Version) error {
	if _, ok := p.state.(ScheduledState); !ok {
		return InvalidTransitionError{Status: p.Status(), Action: "edit content of"}
	}
	if expected.Int() != p.version.Int() {
		return VersionConflictError{Expected: expected.Int(), Actual: p.version.Int()}
	}
	p.content = c
	p.mutate(now)
	p.record(PostContentUpdated{
		postID:     p.id,
		version:    p.version,
		occurredAt: p.updatedAt,
		Content:    c.String(),
	})
	return nil
}

// Reschedule moves the publish time. Allowed from scheduled and from failed;
// rescheduling a failed post resets its attempts and returns it to scheduled.
func (p *ScheduledPost) Reschedule(at ScheduleTime, now time.Time, expected Version) error {
	switch p.state.(type) {
	case ScheduledState, FailedState:
	default:
		return InvalidTransitionError{Status: p.Status(), Action: "reschedule"}
	}
	if expected.Int() != p.version.Int() {
		return VersionConflictError{Expected: expected.Int(), Actual: p.version.Int()}
	}
	now = now.UTC().Truncate(time.Millisecond)
	if !at.IsAfter(now) {
		return invalid("scheduled_at", "must be in the future")
	}
	p.scheduledAt = at
	p.attempts = NoAttempts()
	p.state = ScheduledState{}
	p.mutate(now)
	p.record(PostRescheduled{
		postID:      p.id,
		version:     p.version,
		occurredAt:  p.updatedAt,
		ScheduledAt: at.Time(),
		Attempts:    0,
	})
	return nil
}

// Cancel soft-deletes a schedule. It is idempotent: canceling a canceled post
// is a no-op that changes nothing.
func (p *ScheduledPost) Cancel(now time.Time, expected *Version) error {
	if _, ok := p.state.(CanceledState); ok {
		return nil
	}
	switch p.state.(type) {
	case ScheduledState, FailedState:
	default:
		return InvalidTransitionError{Status: p.Status(), Action: "cancel"}
	}
	if expected != nil && expected.Int() != p.version.Int() {
		return VersionConflictError{Expected: expected.Int(), Actual: p.version.Int()}
	}
	now = now.UTC().Truncate(time.Millisecond)
	p.state = CanceledState{CanceledAt: now}
	p.mutate(now)
	p.record(PostCanceled{
		postID:     p.id,
		version:    p.version,
		occurredAt: now,
		CanceledAt: now,
	})
	return nil
}

// StartPublishing claims the post for publishing and takes the lease.
func (p *ScheduledPost) StartPublishing(now time.Time, lease time.Duration) error {
	if _, ok := p.state.(ScheduledState); !ok {
		return InvalidTransitionError{Status: p.Status(), Action: "start publishing"}
	}
	now = now.UTC().Truncate(time.Millisecond)
	leaseUntil := now.Add(lease)
	p.attempts = p.attempts.Next()
	p.state = PublishingState{LeaseUntil: leaseUntil}
	p.mutate(now)
	p.record(PostPublishStarted{
		postID:     p.id,
		version:    p.version,
		occurredAt: now,
		Attempts:   p.attempts.Int(),
		LeaseUntil: leaseUntil,
	})
	return nil
}

// AttachPrepareRef stores the platform-side draft reference so a retry reuses
// the same draft instead of creating a second one.
func (p *ScheduledPost) AttachPrepareRef(ref PrepareRef, now time.Time) error {
	if _, ok := p.state.(PublishingState); !ok {
		return InvalidTransitionError{Status: p.Status(), Action: "attach prepare reference to"}
	}
	if p.prepareRef != nil {
		return nil
	}
	p.prepareRef = &ref
	p.mutate(now)
	p.record(PostPrepared{
		postID:     p.id,
		version:    p.version,
		occurredAt: p.updatedAt,
		PrepareRef: ref.String(),
	})
	return nil
}

// MarkPublished records the platform's post id.
func (p *ScheduledPost) MarkPublished(id ExternalPostID, now time.Time) error {
	if _, ok := p.state.(PublishingState); !ok {
		return InvalidTransitionError{Status: p.Status(), Action: "mark published"}
	}
	now = now.UTC().Truncate(time.Millisecond)
	p.state = PublishedState{ExternalID: id, PublishedAt: now}
	p.mutate(now)
	p.record(PostPublished{
		postID:         p.id,
		version:        p.version,
		occurredAt:     now,
		ExternalPostID: id.String(),
		PublishedAt:    now,
	})
	return nil
}

// MarkFailure records a failed publish. A retryAt in the future returns the
// post to scheduled with a retry; nil marks it failed for good.
func (p *ScheduledPost) MarkFailure(reason FailureReason, retryAt *time.Time, now time.Time) error {
	if _, ok := p.state.(PublishingState); !ok {
		return InvalidTransitionError{Status: p.Status(), Action: "mark failed"}
	}
	now = now.UTC().Truncate(time.Millisecond)
	var retryCopy, failedCopy *time.Time
	if retryAt != nil {
		at := retryAt.UTC().Truncate(time.Millisecond)
		p.state = ScheduledState{Retry: &Retry{At: at, Reason: reason}}
		retryCopy = &at
	} else {
		p.state = FailedState{Reason: reason, FailedAt: now}
		failedCopy = &now
	}
	// mutate first: the event must carry the post-change version, or the
	// (aggregate_id, aggregate_version) unique index rejects the append.
	p.mutate(now)
	p.record(PostPublishFailed{
		postID:     p.id,
		version:    p.version,
		occurredAt: now,
		Reason:     reason.String(),
		RetryAt:    retryCopy,
		FailedAt:   failedCopy,
	})
	return nil
}

// RecoverExpiredLease requeues a publish whose worker died mid-flight.
func (p *ScheduledPost) RecoverExpiredLease(now time.Time) error {
	if _, ok := p.state.(PublishingState); !ok {
		return InvalidTransitionError{Status: p.Status(), Action: "recover lease of"}
	}
	now = now.UTC().Truncate(time.Millisecond)
	reason, err := NewFailureReason("lease expired")
	if err != nil {
		return err
	}
	p.state = ScheduledState{Retry: &Retry{At: now, Reason: reason}}
	p.mutate(now)
	p.record(PostLeaseRecovered{
		postID:     p.id,
		version:    p.version,
		occurredAt: now,
		Reason:     reason.String(),
	})
	return nil
}
