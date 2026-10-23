package domain

import "time"

// Snapshot is the primitives-only view of an aggregate handed to the
// persistence adapter. It contains no value objects, so adapters never build
// domain types.
type Snapshot struct {
	ID             string
	Content        string
	ScheduledAt    time.Time
	Platform       string
	Status         string
	Version        int
	Attempts       int
	PrepareRef     *string
	NextAttemptAt  *time.Time
	LastError      *string
	LeaseUntil     *time.Time
	ExternalPostID *string
	PublishedAt    *time.Time
	FailedAt       *time.Time
	CanceledAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Snapshot returns the current state of the aggregate.
func (p *ScheduledPost) Snapshot() Snapshot {
	s := Snapshot{
		ID:          p.id.String(),
		Content:     p.content.String(),
		ScheduledAt: p.scheduledAt.Time(),
		Platform:    p.platform.String(),
		Status:      p.state.Status().String(),
		Version:     p.version.Int(),
		Attempts:    p.attempts.Int(),
		CreatedAt:   p.createdAt,
		UpdatedAt:   p.updatedAt,
	}
	if p.prepareRef != nil {
		ref := p.prepareRef.String()
		s.PrepareRef = &ref
	}
	switch st := p.state.(type) {
	case ScheduledState:
		if st.Retry != nil {
			at := st.Retry.At
			reason := st.Retry.Reason.String()
			s.NextAttemptAt = &at
			s.LastError = &reason
		}
	case PublishingState:
		lease := st.LeaseUntil
		s.LeaseUntil = &lease
	case PublishedState:
		ext := st.ExternalID.String()
		published := st.PublishedAt
		s.ExternalPostID = &ext
		s.PublishedAt = &published
	case FailedState:
		reason := st.Reason.String()
		failed := st.FailedAt
		s.LastError = &reason
		s.FailedAt = &failed
	case CanceledState:
		canceled := st.CanceledAt
		s.CanceledAt = &canceled
	}
	return s
}

// Rehydrate rebuilds an aggregate from storage. It applies structural rules
// only — an overdue schedule is legitimate after downtime, so the "must be in
// the future" rule of NewScheduledPost is deliberately not applied here.
func Rehydrate(s Snapshot) (*ScheduledPost, error) {
	id, err := NewPostID(s.ID)
	if err != nil {
		return nil, err
	}
	content, err := NewPostContent(s.Content)
	if err != nil {
		return nil, err
	}
	platform, err := NewPlatform(s.Platform)
	if err != nil {
		return nil, err
	}
	version, err := NewVersion(s.Version)
	if err != nil {
		return nil, err
	}
	attempts, err := NewAttempt(s.Attempts)
	if err != nil {
		return nil, err
	}
	status, err := ParseStatus(s.Status)
	if err != nil {
		return nil, err
	}

	var state State
	switch status {
	case StatusScheduled():
		if s.LeaseUntil != nil {
			return nil, ValidationError{Field: "status", Message: "scheduled row must not carry a lease"}
		}
		st := ScheduledState{}
		if s.NextAttemptAt != nil {
			if s.LastError == nil {
				return nil, ValidationError{Field: "next_attempt_at", Message: "retry requires a reason"}
			}
			reason, err := NewFailureReason(*s.LastError)
			if err != nil {
				return nil, err
			}
			st.Retry = &Retry{At: s.NextAttemptAt.UTC(), Reason: reason}
		}
		state = st
	case StatusPublishing():
		if s.LeaseUntil == nil {
			return nil, ValidationError{Field: "lease_until", Message: "publishing row requires a lease"}
		}
		if s.NextAttemptAt != nil {
			return nil, ValidationError{Field: "next_attempt_at", Message: "publishing row must not carry a retry"}
		}
		state = PublishingState{LeaseUntil: s.LeaseUntil.UTC()}
	case StatusPublished():
		if s.ExternalPostID == nil || s.PublishedAt == nil {
			return nil, ValidationError{Field: "published", Message: "published row requires an external post id"}
		}
		ext, err := NewExternalPostID(*s.ExternalPostID)
		if err != nil {
			return nil, err
		}
		state = PublishedState{ExternalID: ext, PublishedAt: s.PublishedAt.UTC()}
	case StatusFailed():
		if s.LastError == nil || s.FailedAt == nil {
			return nil, ValidationError{Field: "failed", Message: "failed row requires a reason and a timestamp"}
		}
		reason, err := NewFailureReason(*s.LastError)
		if err != nil {
			return nil, err
		}
		state = FailedState{Reason: reason, FailedAt: s.FailedAt.UTC()}
	case StatusCanceled():
		if s.CanceledAt == nil {
			return nil, ValidationError{Field: "canceled_at", Message: "canceled row requires a timestamp"}
		}
		state = CanceledState{CanceledAt: s.CanceledAt.UTC()}
	}

	post := &ScheduledPost{
		id:          id,
		content:     content,
		scheduledAt: NewScheduleTime(s.ScheduledAt),
		platform:    platform,
		attempts:    attempts,
		state:       state,
		version:     version,
		baseVersion: version,
		createdAt:   s.CreatedAt.UTC(),
		updatedAt:   s.UpdatedAt.UTC(),
	}
	if s.PrepareRef != nil {
		ref, err := NewPrepareRef(*s.PrepareRef)
		if err != nil {
			return nil, err
		}
		post.prepareRef = &ref
	}
	return post, nil
}
