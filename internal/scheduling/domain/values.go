package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxContentRunes is a service-wide sanity bound. It is NOT a platform rule:
// each publisher adapter declares its own limit through app.Capabilities and the
// API edge enforces it before a schedule is accepted.
const MaxContentRunes = 5000

var (
	postIDPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	platformPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
)

// ValidationError reports a rejected value or an illegal argument.
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

func invalid(field, msg string) error { return ValidationError{Field: field, Message: msg} }

// PostID is a canonical lowercase UUID.
type PostID struct{ v string }

func NewPostID(raw string) (PostID, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if !postIDPattern.MatchString(s) {
		return PostID{}, invalid("id", "must be a UUID")
	}
	return PostID{v: s}, nil
}

func (id PostID) String() string { return id.v }
func (id PostID) IsZero() bool   { return id.v == "" }

// PostContent is the trimmed text body.
type PostContent struct{ v string }

func NewPostContent(raw string) (PostContent, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return PostContent{}, invalid("content", "must not be empty")
	}
	if n := utf8.RuneCountInString(s); n > MaxContentRunes {
		return PostContent{}, invalid("content", fmt.Sprintf("must be at most %d characters", MaxContentRunes))
	}
	return PostContent{v: s}, nil
}

func (c PostContent) String() string { return c.v }
func (c PostContent) RuneCount() int { return utf8.RuneCountInString(c.v) }

// ScheduleTime is an instant truncated to milliseconds, always UTC.
type ScheduleTime struct{ t time.Time }

func NewScheduleTime(t time.Time) ScheduleTime {
	return ScheduleTime{t: t.UTC().Truncate(time.Millisecond)}
}

func (s ScheduleTime) Time() time.Time          { return s.t }
func (s ScheduleTime) IsZero() bool             { return s.t.IsZero() }
func (s ScheduleTime) IsAfter(t time.Time) bool { return s.t.After(t) }
func (s ScheduleTime) String() string           { return s.t.UTC().Format(time.RFC3339) }

// Platform is a validated platform slug, e.g. "threads". The domain knows no
// platform by name: membership is the registry's business.
type Platform struct{ v string }

func NewPlatform(raw string) (Platform, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return Platform{}, invalid("platform", "must not be empty")
	}
	if !platformPattern.MatchString(s) {
		return Platform{}, invalid("platform", "must match ^[a-z][a-z0-9_]{1,31}$")
	}
	return Platform{v: s}, nil
}

func (p Platform) String() string { return p.v }
func (p Platform) IsZero() bool   { return p.v == "" }

// Version counts domain changes to an aggregate. It only ever grows by one.
type Version struct{ n int }

// InitialVersion is the version of a freshly created aggregate.
var InitialVersion = Version{n: 1}

func NewVersion(n int) (Version, error) {
	if n < 1 {
		return Version{}, invalid("version", "must be at least 1")
	}
	return Version{n: n}, nil
}

func (v Version) Int() int       { return v.n }
func (v Version) IsZero() bool   { return v.n == 0 }
func (v Version) Next() Version  { return Version{n: v.n + 1} }
func (v Version) String() string { return fmt.Sprintf("%d", v.n) }

// Attempt counts publish tries.
type Attempt struct{ n int }

func NoAttempts() Attempt { return Attempt{} }

func NewAttempt(n int) (Attempt, error) {
	if n < 0 {
		return Attempt{}, invalid("attempts", "must not be negative")
	}
	return Attempt{n: n}, nil
}

func (a Attempt) Int() int      { return a.n }
func (a Attempt) Next() Attempt { return Attempt{n: a.n + 1} }

// PrepareRef is an opaque, platform-side draft reference.
type PrepareRef struct{ v string }

func NewPrepareRef(raw string) (PrepareRef, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return PrepareRef{}, invalid("prepare_ref", "must not be empty")
	}
	if len(s) > 128 {
		return PrepareRef{}, invalid("prepare_ref", "must be at most 128 characters")
	}
	return PrepareRef{v: s}, nil
}

func (r PrepareRef) String() string { return r.v }

// ExternalPostID is the identifier the platform assigned to the published post.
type ExternalPostID struct{ v string }

func NewExternalPostID(raw string) (ExternalPostID, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ExternalPostID{}, invalid("external_post_id", "must not be empty")
	}
	if len(s) > 128 {
		return ExternalPostID{}, invalid("external_post_id", "must be at most 128 characters")
	}
	return ExternalPostID{v: s}, nil
}

func (e ExternalPostID) String() string { return e.v }

// FailureReason explains why a publish did not succeed.
type FailureReason struct{ v string }

func NewFailureReason(raw string) (FailureReason, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return FailureReason{}, invalid("reason", "must not be empty")
	}
	if utf8.RuneCountInString(s) > 500 {
		s = string([]rune(s)[:500])
	}
	return FailureReason{v: s}, nil
}

func (f FailureReason) String() string { return f.v }

// Status is derived from the state variant, never stored on the aggregate.
type Status struct{ v string }

func StatusScheduled() Status  { return Status{v: "scheduled"} }
func StatusPublishing() Status { return Status{v: "publishing"} }
func StatusPublished() Status  { return Status{v: "published"} }
func StatusFailed() Status     { return Status{v: "failed"} }
func StatusCanceled() Status   { return Status{v: "canceled"} }

func ParseStatus(raw string) (Status, error) {
	switch raw {
	case "scheduled", "publishing", "published", "failed", "canceled":
		return Status{v: raw}, nil
	default:
		return Status{}, invalid("status", "unknown status "+raw)
	}
}

func (s Status) String() string { return s.v }
func (s Status) IsZero() bool   { return s.v == "" }
func (s Status) IsTerminal() bool {
	return s.v == "published" || s.v == "canceled"
}
