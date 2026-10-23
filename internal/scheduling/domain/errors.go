package domain

import "fmt"

// NotFoundError reports an unknown aggregate.
type NotFoundError struct {
	ID string
}

func (e NotFoundError) Error() string { return "schedule not found: " + e.ID }

// VersionConflictError reports a stale expected_version.
type VersionConflictError struct {
	Expected int
	Actual   int
}

func (e VersionConflictError) Error() string {
	return fmt.Sprintf("version conflict: expected %d, actual %d", e.Expected, e.Actual)
}

// InvalidTransitionError reports an action that the current state forbids.
type InvalidTransitionError struct {
	Status Status
	Action string
	Detail string
}

func (e InvalidTransitionError) Error() string {
	msg := fmt.Sprintf("cannot %s a %s schedule", e.Action, e.Status)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}
