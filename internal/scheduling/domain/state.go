package domain

import "time"

// State is the sealed lifecycle ADT. Each variant carries only what is valid in
// that stage, so a status field can never disagree with the state.
type State interface {
	Status() Status
	sealed()
}

// Retry is the optional pending retry of a scheduled post. A scheduled post with
// no Retry is simply due.
type Retry struct {
	At     time.Time
	Reason FailureReason
}

type ScheduledState struct {
	Retry *Retry
}

func (ScheduledState) sealed()        {}
func (ScheduledState) Status() Status { return StatusScheduled() }

type PublishingState struct {
	LeaseUntil time.Time
}

func (PublishingState) sealed()        {}
func (PublishingState) Status() Status { return StatusPublishing() }

type PublishedState struct {
	ExternalID  ExternalPostID
	PublishedAt time.Time
}

func (PublishedState) sealed()        {}
func (PublishedState) Status() Status { return StatusPublished() }

type FailedState struct {
	Reason   FailureReason
	FailedAt time.Time
}

func (FailedState) sealed()        {}
func (FailedState) Status() Status { return StatusFailed() }

type CanceledState struct {
	CanceledAt time.Time
}

func (CanceledState) sealed()        {}
func (CanceledState) Status() Status { return StatusCanceled() }
